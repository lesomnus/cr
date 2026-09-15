package registry_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxtest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

// TestCacheRequestsAreCounted: every way a cache answers is a count under
// its outcome, and what the upstream cost is measured beside it.
func TestCacheRequestsAreCounted(t *testing.T) {
	h := otxtest.New(t)
	ctx := h.Into(context.Background())
	meter := otx.From(ctx).Meter()

	up := newUpstream(t, nil)
	body, _ := up.image("library/app", "layer bytes")
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", "latest", body, v1.MediaTypeImageManifest).StatusCode)

	u, err := blob.NewUpstream(up.srv.URL, "", "", blob.WithMeter(meter))
	require.NoError(t, err)
	p := &registry.Proxy{Prefix: "docker.io", Upstream: u, TagTTL: time.Minute}
	clock := &fakeClock{now: time.Now()}
	ix := memindex.New()
	ix.Now = clock.Now
	c := &harness{t: t, h: registry.New(registry.Config{
		Stores:  blob.NewCache(flob.NewMemStores(), blob.CacheRoute{Prefix: p.Prefix, Origin: u.Stores(p.Name)}),
		Index:   ix,
		Proxies: []*registry.Proxy{p},
		Now:     clock.Now,
		Meter:   meter,
	})}
	const repo = "docker.io/library/app"
	get := func(ref string) int { return c.do("GET", "/v2/"+repo+"/manifests/"+ref, nil).StatusCode }

	require.Equal(t, http.StatusOK, get("latest"), "miss")
	require.Equal(t, http.StatusOK, get("latest"), "hit, within the tag's ttl")
	clock.Add(2 * time.Minute)
	require.Equal(t, http.StatusOK, get("latest"), "revalidated: the upstream still has the same digest")
	require.Equal(t, http.StatusNotFound, get("nope"), "unknown upstream")

	up.srv.Close()
	clock.Add(2 * time.Minute)
	require.Equal(t, http.StatusOK, get("latest"), "stale: the upstream is gone, the cache is not")
	require.Equal(t, http.StatusInternalServerError, get("other"), "error: not cached, and no upstream")

	outcomes := map[string]int64{}
	upstream := map[string]uint64{}
	var manifestBytes int64
	for _, sm := range h.Collect(ctx).ScopeMetrics {
		for _, mt := range sm.Metrics {
			switch mt.Name {
			case "cr.cache.requests":
				sum, ok := mt.Data.(metricdata.Sum[int64])
				require.True(t, ok)
				for _, dp := range sum.DataPoints {
					proxy, _ := dp.Attributes.Value("cr.cache.proxy")
					require.Equal(t, "docker.io", proxy.AsString())
					outcome, _ := dp.Attributes.Value("cr.cache.outcome")
					outcomes[outcome.AsString()] += dp.Value
				}
			case "cr.cache.upstream.duration":
				hist, ok := mt.Data.(metricdata.Histogram[float64])
				require.True(t, ok)
				for _, dp := range hist.DataPoints {
					op, _ := dp.Attributes.Value("cr.cache.operation")
					upstream[op.AsString()] += dp.Count
				}
			case "cr.cache.upstream.bytes":
				sum, ok := mt.Data.(metricdata.Sum[int64])
				require.True(t, ok)
				for _, dp := range sum.DataPoints {
					op, _ := dp.Attributes.Value("cr.cache.operation")
					if op.AsString() == "manifest get" {
						manifestBytes += dp.Value
					}
				}
			}
		}
	}
	require.Equal(t, map[string]int64{"miss": 1, "hit": 1, "revalidated": 1, "unknown": 1, "stale": 1, "error": 1}, outcomes)
	require.Equal(t, uint64(1), upstream["manifest get"], "the miss")
	require.GreaterOrEqual(t, upstream["manifest head"], uint64(3), "the miss, the revalidation, the unknown, and the ones that failed")
	require.Equal(t, int64(len(body)), manifestBytes)
}
