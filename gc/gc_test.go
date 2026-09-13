package gc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/gc"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	t      *testing.T
	clock  *clock
	stores *flob.MemStores
	ix     *memindex.Index
	reg    http.Handler
}

func newEnv(t *testing.T) *env {
	c := &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	stores := flob.NewMemStores(flob.StageConfig{TTL: time.Millisecond, Retention: time.Millisecond})
	ix := memindex.New()
	ix.Now = c.Now
	return &env{t: t, clock: c, stores: stores, ix: ix, reg: registry.New(registry.Config{Stores: stores, Index: ix, Now: c.Now})}
}

func (e *env) do(method, path string, body []byte, header ...string) int {
	e.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	e.reg.ServeHTTP(w, req)
	return w.Code
}

func (e *env) blob(repo string, b []byte) v1.Descriptor {
	e.t.Helper()
	d := digest.FromBytes(b)
	require.Equal(e.t, http.StatusCreated, e.do("POST", "/v2/"+repo+"/blobs/uploads/?digest="+d.String(), b))
	return v1.Descriptor{MediaType: "application/octet-stream", Digest: d, Size: int64(len(b))}
}

func (e *env) manifest(repo, ref string, m any, mt string) v1.Descriptor {
	e.t.Helper()
	b, _ := json.Marshal(m)
	d := digest.FromBytes(b)
	if ref == "" {
		ref = d.String()
	}
	require.Equal(e.t, http.StatusCreated, e.do("PUT", "/v2/"+repo+"/manifests/"+ref, b, "Content-Type", mt))
	return v1.Descriptor{MediaType: mt, Digest: d, Size: int64(len(b))}
}

func (e *env) image(repo, ref, layer string) (v1.Descriptor, v1.Descriptor) {
	config := e.blob(repo, []byte(`{"layer":"`+layer+`"}`))
	config.MediaType = v1.MediaTypeImageConfig
	l := e.blob(repo, []byte(layer))
	m := e.manifest(repo, ref, v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    config,
		Layers:    []v1.Descriptor{l},
	}, v1.MediaTypeImageManifest)
	return m, l
}

func (e *env) has(repo string, d digest.Digest) bool {
	_, err := e.stores.Use(repo).Stat(context.Background(), flob.Digest(d))
	return err == nil
}

func (e *env) indexed(repo string, d digest.Digest) bool {
	_, err := e.ix.Manifest().Get(context.Background(), repo, d)
	return err == nil
}

func TestUntagged(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	const repo = "acme/app"

	tagged, _ := e.image(repo, "latest", "tagged")
	loose, looseLayer := e.image(repo, "", "loose")
	pinned, _ := e.image(repo, "", "pinned")
	child, _ := e.image(repo, "", "child")
	idx := e.manifest(repo, "", v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{child},
	}, v1.MediaTypeImageIndex)
	sig := e.manifest(repo, "", v1.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    v1.MediaTypeImageManifest,
		ArtifactType: "application/vnd.example.sig",
		Config:       v1.DescriptorEmptyJSON,
		Layers:       []v1.Descriptor{e.blob(repo, []byte("signature"))},
		Subject:      &tagged,
	}, v1.MediaTypeImageManifest)

	collector := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Untagged: 24 * time.Hour, Now: e.clock.Now})

	// Young: nothing goes.
	r, err := collector.Run(ctx)
	require.NoError(t, err)
	require.Zero(t, r.Manifests)

	e.clock.Add(48 * time.Hour)
	// A pull by digest inside the window keeps a manifest no tag names.
	e.ix.Pulled().Touch(repo, "", pinned.Digest, e.clock.Now().Add(-time.Hour))

	r, err = collector.Run(ctx)
	require.NoError(t, err)

	require.True(t, e.indexed(repo, tagged.Digest))
	require.True(t, e.indexed(repo, pinned.Digest))
	require.True(t, e.indexed(repo, sig.Digest), "a referrer of a manifest that is here")
	require.False(t, e.indexed(repo, loose.Digest))
	require.False(t, e.has(repo, loose.Digest))
	require.False(t, e.has(repo, looseLayer.Digest))
	require.False(t, e.indexed(repo, idx.Digest))

	// The child was held by the index when it was looked at, or it was not;
	// either way the next run has nothing holding it.
	_, err = collector.Run(ctx)
	require.NoError(t, err)
	require.False(t, e.indexed(repo, child.Digest))
	require.False(t, e.has(repo, child.Digest))
}

func TestRetention(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	const repo = "acme/app"

	for _, tag := range []string{"nightly-1", "nightly-2", "nightly-3", "nightly-4", "v1.0.0"} {
		e.image(repo, tag, tag)
		e.clock.Add(time.Minute)
	}

	p, err := auth.NewPolicy(nil, []auth.TagRule{
		{Repo: "*", Tag: "*", Kind: auth.TagRetention, Keep: 0},
		{Repo: "acme/*", Tag: "nightly-*", Kind: auth.TagRetention, Keep: 2},
		{Repo: "*", Tag: "v*", Kind: auth.TagImmutable},
	})
	require.NoError(t, err)

	collector := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Policy: func() *auth.Policy { return p }, Now: e.clock.Now})
	r, err := collector.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, r.Tags)

	names, err := e.ix.Tag().List(ctx, repo, index.Page{})
	require.NoError(t, err)
	require.Equal(t, []string{"nightly-3", "nightly-4", "v1.0.0"}, names)
}

func TestStages(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	require.Equal(t, http.StatusAccepted, e.do("POST", "/v2/acme/app/blobs/uploads/", nil))
	time.Sleep(10 * time.Millisecond)

	r, err := gc.New(gc.Config{Stores: e.stores, Index: e.ix}).Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, r.Stages)
}

func TestCacheEviction(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)

	// Two caches, one of them pulled recently.
	stale, staleLayer := e.image("mirror/stale", "latest", "stale")
	used, _ := e.image("mirror/used", "latest", "used")
	mine, _ := e.image("acme/app", "latest", "mine")

	e.clock.Add(48 * time.Hour)
	e.ix.Pulled().Touch("mirror/used", "", used.Digest, e.clock.Now().Add(-time.Hour))

	cache := func(repo string) time.Duration {
		if strings.HasPrefix(repo, "mirror/") {
			return 24 * time.Hour
		}
		return 0
	}
	r, err := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Cache: cache, Now: e.clock.Now}).Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, r.Tags)
	require.Equal(t, 1, r.Manifests)

	require.False(t, e.indexed("mirror/stale", stale.Digest))
	require.False(t, e.has("mirror/stale", staleLayer.Digest))
	require.True(t, e.indexed("mirror/used", used.Digest), "pulled by digest within the window")
	_, err = e.ix.Tag().Get(ctx, "mirror/used", "latest")
	require.NoError(t, err)
	require.True(t, e.indexed("acme/app", mine.Digest), "not a cache")
}
