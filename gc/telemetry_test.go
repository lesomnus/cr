package gc_test

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxtest"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lesomnus/cr/gc"
)

// TestCollectionIsMeasured: a run is counted by what it was and how it
// ended, what it removed is counted, and the index is asked how much it
// holds.
func TestCollectionIsMeasured(t *testing.T) {
	h := otxtest.New(t)
	ctx := h.Into(context.Background())
	e := newEnv(t)
	e.image("acme/app", "latest", "kept")
	stray := e.blob("acme/app", []byte("stray"))
	e.clock.Add(2 * time.Hour)

	col := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Runs: &gc.MemRuns{}, Now: e.clock.Now, Meter: otx.From(ctx).Meter()})
	run, err := col.Collect(ctx, gc.KindFull, gc.TriggerCli)
	require.NoError(t, err)
	require.Equal(t, 1, run.Blobs)
	require.False(t, e.has("acme/app", stray.Digest))

	sums := map[string]int64{}
	gauges := map[string]int64{}
	durations := 0
	for _, sm := range h.Collect(ctx).ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					key := m.Name
					if what, ok := dp.Attributes.Value("cr.gc.what"); ok {
						key += " " + what.AsString()
					}
					if state, ok := dp.Attributes.Value("cr.gc.state"); ok {
						key += " " + state.AsString()
					}
					sums[key] += dp.Value
				}
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					gauges[m.Name] = dp.Value
				}
			case metricdata.Histogram[float64]:
				if m.Name == "cr.gc.run.duration" {
					durations += len(data.DataPoints)
				}
			}
		}
	}
	require.Equal(t, int64(1), sums["cr.gc.runs done"])
	require.Equal(t, int64(1), sums["cr.gc.reclaimed blobs"])
	require.Equal(t, int64(len("stray")), sums["cr.gc.reclaimed.bytes"])
	require.Equal(t, 1, durations)
	require.Equal(t, int64(0), gauges["cr.gc.missing"])
	require.Equal(t, int64(1), gauges["cr.repositories"])
	require.Equal(t, int64(1), gauges["cr.manifests"])
	require.Equal(t, int64(1), gauges["cr.tags"])
}
