package registry_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

func TestProxyFallbackFillsCacheAndSurvivesBothOrigins(t *testing.T) {
	up := newUpstream(t, nil)
	body, manifest := up.image("dist/prod/app", "a layer served by the fallback")
	require.Equal(t, http.StatusCreated, up.pushManifest("dist/prod/app", "release", body, "application/vnd.oci.image.manifest.v1+json").StatusCode)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer primary.Close()
	u, err := blob.NewUpstreams([]blob.UpstreamTarget{{URL: primary.URL}, {URL: up.srv.URL}})
	require.NoError(t, err)
	p := &registry.Proxy{Prefix: "dist", Remote: "dist", Upstream: u}
	base := flob.NewMemStores()
	c := &harness{t: t, h: registry.New(registry.Config{
		Stores: blob.NewCache(base, blob.CacheRoute{Prefix: p.Prefix, Origin: u.Stores(p.Name)}),
		Index:  memindex.New(), Proxies: []*registry.Proxy{p},
	})}
	manifestURL := "/v2/dist/prod/app/manifests/" + digest.FromBytes(body).String()
	r := c.do("GET", manifestURL, nil)
	require.Equal(t, http.StatusOK, r.StatusCode)
	require.Equal(t, body, read(t, r))
	layerURL := "/v2/dist/prod/app/blobs/" + manifest.Layers[0].Digest.String()
	r = c.do("GET", layerURL, nil)
	require.Equal(t, http.StatusOK, r.StatusCode)
	require.Equal(t, []byte("a layer served by the fallback"), read(t, r))
	require.Eventually(t, func() bool {
		_, err := base.Use("dist/prod/app").Stat(t.Context(), flob.Digest(manifest.Layers[0].Digest))
		return err == nil
	}, time.Second, time.Millisecond)
	primary.Close()
	up.srv.Close()
	for _, path := range []string{manifestURL, layerURL} {
		r := c.do("GET", path, nil)
		require.Equal(t, http.StatusOK, r.StatusCode)
		read(t, r)
	}
}
