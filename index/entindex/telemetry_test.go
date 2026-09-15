package entindex_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxtest"
	"github.com/opencontainers/go-digest"
	entsql "github.com/protobuf-orm/ent/dialect/sql"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lesomnus/payday/config"

	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/index/entindex"
	"github.com/lesomnus/cr/internal/ent"
	entmigrate "github.com/lesomnus/cr/internal/ent/migrate"
)

// TestLockWaitIsMeasured: a wait for a repository's lock is a point under
// whatever said it was waiting, and one that gave up is counted.
func TestLockWaitIsMeasured(t *testing.T) {
	h := otxtest.New(t)
	ctx := h.Into(context.Background())
	c := config.DbConfig{
		Driver: "sqlite3",
		Dsn:    fmt.Sprintf("file:%s/wait.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)", t.TempDir()),
	}
	db, dialect, err := c.Open(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	drv := entsql.OpenDB(dialect, db)
	require.NoError(t, entmigrate.NewSchema(drv).Create(ctx))
	ix := entindex.New(ent.NewClient(ent.Driver(drv)), entindex.WithWait(50*time.Millisecond), entindex.WithMeter(otx.From(ctx).Meter()))

	unlock, err := ix.Lock(index.Waiting(ctx, "sweep"), "r")
	require.NoError(t, err)
	err = ix.Tx(index.Waiting(ctx, "manifest push"), "r", func(index.Index) error { return nil })
	require.ErrorIs(t, err, index.ErrBusy)
	unlock()
	require.NoError(t, ix.Tx(ctx, "r", func(index.Index) error { return nil }), "and now it does not wait")

	waits := map[string]uint64{}
	gaveUp := map[string]int64{}
	for _, sm := range h.Collect(ctx).ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "cr.repository.lock.wait":
				hist, ok := m.Data.(metricdata.Histogram[float64])
				require.True(t, ok)
				for _, dp := range hist.DataPoints {
					what, _ := dp.Attributes.Value("cr.lock.for")
					waits[what.AsString()] += dp.Count
				}
			case "cr.repository.lock.timeouts":
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok)
				for _, dp := range sum.DataPoints {
					what, _ := dp.Attributes.Value("cr.lock.for")
					gaveUp[what.AsString()] += dp.Value
				}
			}
		}
	}
	require.Equal(t, map[string]uint64{"sweep": 1, "manifest push": 1, "other": 1}, waits)
	require.Equal(t, map[string]int64{"manifest push": 1}, gaveUp)

	// The pull bookkeeping: what a flush found waiting, and how long it took.
	for _, name := range []string{"a", "b"} {
		d := digest.FromString(name)
		require.NoError(t, ix.Manifest().Put(ctx, "r", index.Manifest{Digest: d, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 1}, nil))
		require.NoError(t, ix.Tag().Set(ctx, "r", name, d, ""))
		ix.Pulled().Touch("r", name, d, time.Now())
	}
	require.NoError(t, ix.Flush(ctx))
	pending, flushes := int64(-1), uint64(0)
	for _, sm := range h.Collect(ctx).ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "cr.index.pulls.pending":
				for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
					pending = dp.Value
				}
			case "cr.index.pulls.flush.duration":
				for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
					flushes += dp.Count
				}
			}
		}
	}
	require.Equal(t, int64(4), pending, "two manifests and two tags")
	require.Equal(t, uint64(1), flushes)
}
