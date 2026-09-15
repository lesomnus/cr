package gc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/gc"
)

func TestSweep(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	const repo = "acme/app"

	m, layer := e.image(repo, "latest", "kept")
	stray := e.blob(repo, []byte("stray"))
	elsewhere := e.blob("acme/other", []byte("stray elsewhere"))

	// Past the sweep's delay for what was just added.
	e.clock.Add(2 * time.Hour)
	col := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Now: e.clock.Now})
	r, err := col.Sweep(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, 1, r.Blobs)
	require.Equal(t, int64(len("stray")), r.Bytes)
	require.Empty(t, r.Missing)

	require.False(t, e.has(repo, stray.Digest))
	require.True(t, e.has(repo, m.Digest))
	require.True(t, e.has(repo, layer.Digest))
	require.True(t, e.has("acme/other", elsewhere.Digest), "only the repository asked for is swept")

	// A manifest whose bytes went is reported, and not repaired.
	require.NoError(t, e.stores.Use(repo).Erase(ctx, flob.Digest(m.Digest)))
	r, err = col.Sweep(ctx, repo)
	require.NoError(t, err)
	require.Zero(t, r.Blobs)
	require.Equal(t, []digest.Digest{m.Digest}, r.Missing)
}

func TestFull(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.image("acme/app", "latest", "a")
	e.blob("acme/app", []byte("stray a"))
	// A namespace the index has no repository for is the store's, and swept.
	e.blob("acme/web", []byte("stray b"))
	e.clock.Add(2 * time.Hour)

	runs := &gc.MemRuns{}
	col := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Runs: runs, Now: e.clock.Now})
	run, err := col.Collect(ctx, gc.KindFull, gc.TriggerCli)
	require.NoError(t, err)
	require.Equal(t, gc.StateDone, run.State)
	require.Equal(t, 2, run.Repositories)
	require.Equal(t, 2, run.Blobs)
	require.NotNil(t, run.Finished)

	got, err := runs.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, gc.StateDone, got.State)
	require.Equal(t, 2, got.Blobs)
}

// TestPushDuringSweep is the wait bound: a manifest push to a repository whose
// lock a sweep holds waits, and past the bound is told to retry.
func TestPushDuringSweep(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.ix.Wait = 50 * time.Millisecond
	const repo = "acme/app"

	config := e.blob(repo, []byte(`{"os":"linux"}`))
	config.MediaType = v1.MediaTypeImageConfig
	layer := e.blob(repo, []byte("layer"))
	b, _ := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    config,
		Layers:    []v1.Descriptor{layer},
	})
	push := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/v2/"+repo+"/manifests/latest", bytes.NewReader(b))
		req.Header.Set("Content-Type", v1.MediaTypeImageManifest)
		w := httptest.NewRecorder()
		e.reg.ServeHTTP(w, req)
		return w
	}

	unlock, err := e.ix.Lock(ctx, repo)
	require.NoError(t, err)
	w := push()
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotEmpty(t, w.Header().Get("Retry-After"))

	unlock()
	require.Equal(t, http.StatusCreated, push().Code)
}

func TestTrigger(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	stray := e.blob("acme/app", []byte("stray"))
	e.clock.Add(2 * time.Hour)

	runs := &gc.MemRuns{}
	col := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Runs: runs, Now: e.clock.Now})

	// Held, so the run that starts cannot finish while it is asked for again.
	unlock, err := e.ix.Lock(ctx, "acme/app")
	require.NoError(t, err)

	run, err := col.Trigger(ctx, gc.TriggerAdmin)
	require.NoError(t, err)
	require.Equal(t, gc.StateRunning, run.State)

	again, err := col.Trigger(ctx, gc.TriggerAdmin)
	require.ErrorIs(t, err, gc.ErrRunning)
	require.Equal(t, run.ID, again.ID)

	unlock()
	require.Eventually(t, func() bool {
		r, err := runs.Get(ctx, run.ID)
		return err == nil && r.State == gc.StateDone
	}, 5*time.Second, 10*time.Millisecond)
	require.False(t, e.has("acme/app", stray.Digest))

	// And a new one may start.
	next, err := col.Trigger(ctx, gc.TriggerAdmin)
	require.NoError(t, err)
	require.NotEqual(t, run.ID, next.ID)
	require.Eventually(t, func() bool {
		r, err := runs.Get(ctx, next.ID)
		return err == nil && r.State == gc.StateDone
	}, 5*time.Second, 10*time.Millisecond)
}

// TestSweepLeavesWhatJustArrived is the delay: a blob younger than it may be
// a push whose manifest is on its way, and is left for a later sweep.
func TestSweepLeavesWhatJustArrived(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	const repo = "acme/app"
	stray := e.blob(repo, []byte("stray"))

	col := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Now: e.clock.Now})
	r, err := col.Sweep(ctx, repo)
	require.NoError(t, err)
	require.Zero(t, r.Blobs)
	require.True(t, e.has(repo, stray.Digest))

	e.clock.Add(2 * time.Hour)
	r, err = col.Sweep(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, 1, r.Blobs)
	require.False(t, e.has(repo, stray.Digest))

	// A negative delay asks nothing about age.
	again := e.blob(repo, []byte("stray again"))
	at := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Delay: -1, Now: e.clock.Now})
	r, err = at.Sweep(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, 1, r.Blobs)
	require.False(t, e.has(repo, again.Digest))
}

// TestSweepMissing: a manifest the store lost is reported, unless it was
// pushed as the walk began, when its blobs may have arrived after the walk
// passed them.
func TestSweepMissing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	const repo = "acme/app"

	m, _ := e.image(repo, "latest", "lost")
	require.NoError(t, e.stores.Use(repo).Erase(ctx, flob.Digest(m.Digest)))

	col := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Now: e.clock.Now})
	r, err := col.Sweep(ctx, repo)
	require.NoError(t, err)
	require.Empty(t, r.Missing)

	e.clock.Add(2 * time.Minute)
	r, err = col.Sweep(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, []digest.Digest{m.Digest}, r.Missing)
}
