package registry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// upstream is another cr, served over HTTP: the registry a cache reads from.
type upstream struct {
	*harness
	srv *httptest.Server
}

func newUpstream(t *testing.T, guard *auth.Guard) *upstream {
	reg := registry.New(registry.Config{Stores: flob.NewMemStores(), Index: memindex.New(), Guard: guard})
	mux := http.NewServeMux()
	mux.Handle("/v2/", reg)
	if guard != nil {
		mux.HandleFunc("/token", guard.ServeToken)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &upstream{harness: &harness{t: t, h: mux}, srv: srv}
}

type cache struct {
	*harness
	base  *flob.MemStores
	ix    *memindex.Index
	clock *fakeClock
}

func newCache(t *testing.T, up *upstream, username, password string) *cache {
	u, err := blob.NewUpstream(up.srv.URL, username, password)
	require.NoError(t, err)
	p := &registry.Proxy{Prefix: "docker.io", Upstream: u, TagTTL: time.Minute}
	base := flob.NewMemStores()
	clock := &fakeClock{now: time.Now()}
	ix := memindex.New()
	ix.Now = clock.Now
	reg := registry.New(registry.Config{
		Stores:  blob.NewCache(base, blob.CacheRoute{Prefix: p.Prefix, Origin: u.Stores(p.Name)}),
		Index:   ix,
		Proxies: []*registry.Proxy{p},
		Now:     clock.Now,
	})
	return &cache{harness: &harness{t: t, h: reg}, base: base, ix: ix, clock: clock}
}

func (c *cache) cached(repo string, d digest.Digest) bool {
	_, err := c.base.Use(repo).Stat(context.Background(), flob.Digest(d))
	return err == nil
}

func TestProxyPull(t *testing.T) {
	up := newUpstream(t, nil)
	body, m := up.image("library/app", "layer bytes")
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", "latest", body, v1.MediaTypeImageManifest).StatusCode)

	c := newCache(t, up, "", "")
	const repo = "docker.io/library/app"

	res := c.do("GET", "/v2/"+repo+"/manifests/latest", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, v1.MediaTypeImageManifest, res.Header.Get("Content-Type"))
	require.Equal(t, body, read(t, res))

	res = c.do("GET", "/v2/"+repo+"/blobs/"+m.Layers[0].Digest.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, []byte("layer bytes"), read(t, res))
	require.Eventually(t, func() bool { return c.cached(repo, m.Layers[0].Digest) }, 5*time.Second, 10*time.Millisecond)

	// A blob no manifest has fetched yet is answered from upstream too.
	res = c.do("HEAD", "/v2/"+repo+"/blobs/"+m.Config.Digest.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)

	// Upstream gone: what is cached is still served, before the tag is due
	// for a check and after it.
	up.srv.Close()
	res = c.do("GET", "/v2/"+repo+"/manifests/latest", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	c.clock.Add(2 * time.Minute)
	res = c.do("GET", "/v2/"+repo+"/manifests/latest", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	res = c.do("GET", "/v2/"+repo+"/blobs/"+m.Layers[0].Digest.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, []byte("layer bytes"), read(t, res))

	// A cache takes no pushes; a repository beside it does.
	res = c.pushManifest(repo, "mine", body, v1.MediaTypeImageManifest)
	require.Equal(t, http.StatusMethodNotAllowed, res.StatusCode)
	require.Equal(t, "UNSUPPORTED", code(t, res))
	res = c.do("POST", "/v2/"+repo+"/blobs/uploads/", nil)
	require.Equal(t, http.StatusMethodNotAllowed, res.StatusCode)
	res = c.do("POST", "/v2/acme/app/blobs/uploads/", nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)
}

func TestProxyFetchesOnlyWhatIsAsked(t *testing.T) {
	up := newUpstream(t, nil)
	amd64, amd := up.image("library/multi", "amd64 layer")
	arm64, arm := up.image("library/multi", "arm64 layer")
	for _, b := range [][]byte{amd64, arm64} {
		require.Equal(t, http.StatusCreated, up.pushManifest("library/multi", digest.FromBytes(b).String(), b, v1.MediaTypeImageManifest).StatusCode)
	}
	idx, _ := json.Marshal(v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{
			{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(amd64), Size: int64(len(amd64)), Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
			{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(arm64), Size: int64(len(arm64)), Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}},
		},
	})
	require.Equal(t, http.StatusCreated, up.pushManifest("library/multi", "latest", idx, v1.MediaTypeImageIndex).StatusCode)

	ctx := context.Background()
	c := newCache(t, up, "", "")
	const repo = "docker.io/library/multi"

	res := c.do("GET", "/v2/"+repo+"/manifests/latest", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, v1.MediaTypeImageIndex, res.Header.Get("Content-Type"))
	ms, err := c.ix.Manifest().List(ctx, repo, index.Page{})
	require.NoError(t, err)
	require.Len(t, ms, 1, "the index alone")

	// The client picks its platform and asks for that one by digest.
	res = c.do("GET", "/v2/"+repo+"/manifests/"+digest.FromBytes(amd64).String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	res = c.do("GET", "/v2/"+repo+"/blobs/"+amd.Layers[0].Digest.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	read(t, res)

	ms, err = c.ix.Manifest().List(ctx, repo, index.Page{})
	require.NoError(t, err)
	require.Len(t, ms, 2)
	_, err = c.ix.Manifest().Get(ctx, repo, digest.FromBytes(arm64))
	require.ErrorIs(t, err, index.ErrNotFound)
	require.Eventually(t, func() bool { return c.cached(repo, amd.Layers[0].Digest) }, 5*time.Second, 10*time.Millisecond)
	require.False(t, c.cached(repo, arm.Layers[0].Digest))
}

func TestProxyTagMoves(t *testing.T) {
	up := newUpstream(t, nil)
	one, _ := up.image("library/app", "one")
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", "v", one, v1.MediaTypeImageManifest).StatusCode)

	c := newCache(t, up, "", "")
	const repo = "docker.io/library/app"
	res := c.do("GET", "/v2/"+repo+"/manifests/v", nil)
	require.Equal(t, digest.FromBytes(one).String(), res.Header.Get("Docker-Content-Digest"))

	two, _ := up.image("library/app", "two")
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", "v", two, v1.MediaTypeImageManifest).StatusCode)

	// Inside the TTL the cache answers without asking.
	res = c.do("GET", "/v2/"+repo+"/manifests/v", nil)
	require.Equal(t, digest.FromBytes(one).String(), res.Header.Get("Docker-Content-Digest"))

	c.clock.Add(2 * time.Minute)
	res = c.do("HEAD", "/v2/"+repo+"/manifests/v", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, digest.FromBytes(two).String(), res.Header.Get("Docker-Content-Digest"))

	// Gone upstream and never cached.
	res = c.do("GET", "/v2/"+repo+"/manifests/nope", nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	require.Equal(t, "MANIFEST_UNKNOWN", code(t, res))
}

func TestProxyUpstreamAuth(t *testing.T) {
	st := auth.NewPolicyStore(time.Hour, auth.Static{Bindings: []auth.Binding{
		{Subject: "ci", Repo: "*", Actions: []auth.Action{auth.ActionAll}},
	}})
	require.NoError(t, st.Refresh(context.Background()))
	tokens, err := auth.NewTokens([]auth.StaticToken{{Name: "ci", Token: "ci-secret"}})
	require.NoError(t, err)
	k, err := auth.GenerateKey()
	require.NoError(t, err)
	issuer, err := auth.NewIssuer("upstream", "upstream", time.Minute, k)
	require.NoError(t, err)
	up := newUpstream(t, &auth.Guard{Authenticator: auth.Chain{tokens}, Policy: st, Issuer: issuer})

	ci := basic("ci")
	body, m := up.imageAs(ci, "private/app", "secret layer")
	require.Equal(t, http.StatusCreated, up.do("PUT", "/v2/private/app/manifests/latest", body, "Content-Type", v1.MediaTypeImageManifest, "Authorization", ci).StatusCode)

	anonymous := newCache(t, up, "", "")
	res := anonymous.do("GET", "/v2/docker.io/private/app/manifests/latest", nil)
	require.Equal(t, http.StatusInternalServerError, res.StatusCode)

	c := newCache(t, up, "whoever", "ci-secret")
	res = c.do("GET", "/v2/docker.io/private/app/manifests/latest", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, body, read(t, res))
	res = c.do("GET", "/v2/docker.io/private/app/blobs/"+m.Layers[0].Digest.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, []byte("secret layer"), read(t, res))
}

// imageAs pushes a config and a layer with a credential and answers the
// manifest over them, not yet pushed.
func (x *harness) imageAs(authorization, repo, layer string) ([]byte, v1.Manifest) {
	x.t.Helper()
	config := []byte(`{"architecture":"amd64","layer":"` + layer + `"}`)
	for _, b := range [][]byte{config, []byte(layer)} {
		res := x.do("POST", "/v2/"+repo+"/blobs/uploads/?digest="+digest.FromBytes(b).String(), b, "Authorization", authorization)
		require.Equal(x.t, http.StatusCreated, res.StatusCode)
	}
	m := v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    descriptor(v1.MediaTypeImageConfig, config),
		Layers:    []v1.Descriptor{descriptor(v1.MediaTypeImageLayerGzip, []byte(layer))},
	}
	b, _ := json.Marshal(m)
	return b, m
}

// slowStores is a store that takes its time to finish a write, as a bucket
// does after the last byte has been read.
type slowStores struct{ flob.Stores }

func (s slowStores) Use(id string) flob.Store { return slowStore{s.Stores.Use(id)} }

type slowStore struct{ flob.Store }

func (s slowStore) Add(ctx context.Context, m flob.Meta, r io.Reader) (flob.Meta, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return flob.Meta{}, err
	}
	select {
	case <-time.After(50 * time.Millisecond):
	case <-ctx.Done():
		return flob.Meta{}, ctx.Err()
	}
	return s.Store.Add(ctx, m, bytes.NewReader(b))
}

// TestProxyFillOutlivesTheRequest is a cache fill that finishes after the
// request that read the blob has ended, which is every fill into a store
// slower than the client.
func TestProxyFillOutlivesTheRequest(t *testing.T) {
	up := newUpstream(t, nil)
	_, m := up.image("library/app", "a layer that outlives its request")

	u, err := blob.NewUpstream(up.srv.URL, "", "")
	require.NoError(t, err)
	p := &registry.Proxy{Prefix: "docker.io", Upstream: u}
	base := flob.NewMemStores()
	reg := registry.New(registry.Config{
		Stores:  blob.NewCache(slowStores{base}, blob.CacheRoute{Prefix: p.Prefix, Origin: u.Stores(p.Name)}),
		Index:   memindex.New(),
		Proxies: []*registry.Proxy{p},
	})

	const repo = "docker.io/library/app"
	d := m.Layers[0].Digest
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, "GET", "/v2/"+repo+"/blobs/"+d.String(), nil)
	w := httptest.NewRecorder()
	reg.ServeHTTP(w, req)
	// What net/http does the moment a handler returns.
	cancel()
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "a layer that outlives its request", w.Body.String())

	require.Eventually(t, func() bool {
		_, err := base.Use(repo).Stat(context.Background(), flob.Digest(d))
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
}
