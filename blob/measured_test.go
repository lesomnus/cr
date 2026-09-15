package blob_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxtest"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lesomnus/cr/blob"
)

// TestMeasured: every call is a point under its operation and outcome, and
// the capabilities of the store beneath are still found through it.
func TestMeasured(t *testing.T) {
	h := otxtest.New(t)
	ctx := h.Into(context.Background())
	stores := blob.Measured(flob.NewMemStores(), "memory", otx.From(ctx).Meter())
	s := stores.Use("acme/app")

	_, err := s.Stat(ctx, flob.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000"))
	require.ErrorIs(t, err, flob.ErrNotExist)
	m, err := s.Add(ctx, flob.Meta{}, strings.NewReader("blob"))
	require.NoError(t, err)
	_, err = s.Add(ctx, flob.Meta{Digest: m.Digest}, strings.NewReader("blob"))
	require.ErrorIs(t, err, flob.ErrAlreadyExists)
	_, err = s.Stat(ctx, m.Digest)
	require.NoError(t, err)

	_, ok := flob.AsWalker(s)
	require.True(t, ok, "the memory store walks, through the measuring one")
	_, ok = flob.AsNamespacer(stores)
	require.True(t, ok)

	counts := map[string]uint64{}
	for _, sm := range h.Collect(ctx).ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != "cr.store.operation.duration" {
				continue
			}
			hist, ok := mt.Data.(metricdata.Histogram[float64])
			require.True(t, ok)
			for _, dp := range hist.DataPoints {
				driver, _ := dp.Attributes.Value("cr.store.driver")
				op, _ := dp.Attributes.Value("cr.store.operation")
				outcome, _ := dp.Attributes.Value("cr.store.outcome")
				counts[driver.AsString()+" "+op.AsString()+" "+outcome.AsString()] += dp.Count
			}
		}
	}
	require.Equal(t, map[string]uint64{
		"memory stat not_found": 1,
		"memory stat ok":        1,
		"memory add ok":         1,
		"memory add exists":     1,
	}, counts)
}
