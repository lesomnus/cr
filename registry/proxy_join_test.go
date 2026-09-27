package registry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxtest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

// gatedUpstream is a registry whose manifest requests are counted, and held
// until the gate opens.
type gatedUpstream struct {
	*harness
	srv       *httptest.Server
	gate      chan struct{}
	manifests atomic.Int64
	fail      atomic.Bool
}

func newGatedUpstream(t *testing.T) *gatedUpstream {
	reg := registry.New(registry.Config{Stores: flob.NewMemStores(), Index: memindex.New()})
	u := &gatedUpstream{harness: &harness{t: t, h: reg}, gate: make(chan struct{})}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			u.manifests.Add(1)
			<-u.gate
			if u.fail.Load() {
				http.Error(w, "down", http.StatusInternalServerError)
				return
			}
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(u.srv.Close)
	// Before the server closes, which waits for what the gate holds.
	t.Cleanup(func() {
		select {
		case <-u.gate:
		default:
			close(u.gate)
		}
	})
	return u
}

func (u *gatedUpstream) cache(t *testing.T, meter metric.Meter) *harness {
	b, err := blob.NewUpstream(u.srv.URL, "", "")
	require.NoError(t, err)
	p := &registry.Proxy{Prefix: "docker.io", Upstream: b, TagTTL: time.Minute}
	return &harness{t: t, h: registry.New(registry.Config{
		Stores:  blob.NewCache(flob.NewMemStores(), blob.CacheRoute{Prefix: p.Prefix, Origin: b.Stores(p.Name)}),
		Index:   memindex.New(),
		Proxies: []*registry.Proxy{p},
		Meter:   meter,
	})}
}

// herd sends n requests for ref at once and answers their statuses, once
// every one of them has had the time to reach the registry and the gate is
// opened.
func herd(c *harness, gate chan struct{}, n int, ref string) []int {
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = c.do("GET", "/v2/docker.io/library/app/manifests/"+ref, nil).StatusCode
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	return codes
}

// TestProxyJoinsAHerd: clients asking for one cold tag at once ask the
// upstream once between them, and say so in the count of cache requests.
func TestProxyJoinsAHerd(t *testing.T) {
	h := otxtest.New(t)
	ctx := h.Into(context.Background())

	up := newGatedUpstream(t)
	body, _ := up.image("library/app", "layer")
	close(up.gate)
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", "latest", body, v1.MediaTypeImageManifest).StatusCode)
	up.gate = make(chan struct{})

	c := up.cache(t, otx.From(ctx).Meter())
	for _, code := range herd(c, up.gate, 8, "latest") {
		require.Equal(t, http.StatusOK, code)
	}
	require.Equal(t, int64(2), up.manifests.Load(), "one HEAD and one GET, for eight clients")

	outcomes := map[string]int64{}
	for _, sm := range h.Collect(ctx).ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != "cr.cache.requests" {
				continue
			}
			for _, dp := range mt.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value("cr.cache.outcome")
				outcomes[v.AsString()] += dp.Value
			}
		}
	}
	require.Equal(t, int64(1), outcomes["miss"])
	require.Equal(t, int64(8), outcomes["miss"]+outcomes["joined"]+outcomes["hit"])
}

// TestProxyJoinsAFailure: a fetch that fails fails every request that
// joined it, and none of them asks again.
func TestProxyJoinsAFailure(t *testing.T) {
	up := newGatedUpstream(t)
	up.fail.Store(true)

	c := up.cache(t, nil)
	for _, code := range herd(c, up.gate, 8, "latest") {
		require.Equal(t, http.StatusBadGateway, code)
	}
	require.Equal(t, int64(1), up.manifests.Load(), "the HEAD that failed, for eight clients")
}

// TestProxyJoinOutlivesTheRequest: a request that stops waiting leaves the
// fetch to finish for the cache, and for whoever joined it.
func TestProxyJoinOutlivesTheRequest(t *testing.T) {
	up := newGatedUpstream(t)
	body, _ := up.image("library/app", "layer")
	close(up.gate)
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", "latest", body, v1.MediaTypeImageManifest).StatusCode)
	up.gate = make(chan struct{})
	c := up.cache(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequestWithContext(ctx, "GET", "/v2/docker.io/library/app/manifests/latest", nil)
		c.h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	require.Eventually(t, func() bool { return up.manifests.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the request waited for the fetch after it was cancelled")
	}

	close(up.gate)
	require.Eventually(t, func() bool {
		return c.do("GET", "/v2/docker.io/library/app/manifests/latest", nil).StatusCode == http.StatusOK
	}, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, int64(2), up.manifests.Load(), "the fetch the first request started, and nothing after it")
}
