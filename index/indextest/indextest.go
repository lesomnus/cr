// Package indextest is the behaviour every [index.Index] owes, run against
// each implementation.
package indextest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/index"
)

// Open answers a fresh, empty index. wait is the lock bound it should apply.
type Open func(t *testing.T, wait time.Duration) index.Index

func d(s string) digest.Digest { return digest.FromString(s) }

func Run(t *testing.T, open Open) {
	t.Run("repos", func(t *testing.T) { testRepos(t, open(t, 0)) })
	t.Run("manifests", func(t *testing.T) { testManifests(t, open(t, 0)) })
	t.Run("erase releases", func(t *testing.T) { testErase(t, open(t, 0)) })
	t.Run("referrers", func(t *testing.T) { testReferrers(t, open(t, 0)) })
	t.Run("tags", func(t *testing.T) { testTags(t, open(t, 0)) })
	t.Run("tx rolls back", func(t *testing.T) { testRollback(t, open(t, 0)) })
	t.Run("tx busy", func(t *testing.T) { testBusy(t, open(t, 100*time.Millisecond)) })
	t.Run("lock", func(t *testing.T) { testLock(t, open(t, 100*time.Millisecond)) })
	t.Run("marks", func(t *testing.T) { testMarks(t, open(t, 0)) })
	t.Run("pulled", func(t *testing.T) { testPulled(t, open(t, 0)) })
}

func testRepos(t *testing.T, ix index.Index) {
	ctx := context.Background()
	r := ix.Repo()

	_, err := r.Get(ctx, "acme/app")
	require.ErrorIs(t, err, index.ErrNotFound)

	for _, name := range []string{"acme/app", "acme/web", "library/ubuntu"} {
		v, err := r.Ensure(ctx, name)
		require.NoError(t, err)
		require.Equal(t, name, v.Name)
	}
	_, err = r.Ensure(ctx, "acme/app")
	require.NoError(t, err)

	names, err := r.List(ctx, index.Page{})
	require.NoError(t, err)
	require.Equal(t, []string{"acme/app", "acme/web", "library/ubuntu"}, names)

	names, err = r.List(ctx, index.Page{Last: "acme/app", N: 1})
	require.NoError(t, err)
	require.Equal(t, []string{"acme/web"}, names)

	desc := "The Ubuntu base image"
	v, err := r.Update(ctx, "library/ubuntu", index.RepoPatch{Description: &desc})
	require.NoError(t, err)
	require.Equal(t, desc, v.Description)
	_, err = r.Update(ctx, "nope", index.RepoPatch{Description: &desc})
	require.ErrorIs(t, err, index.ErrNotFound)

	found, err := r.Search(ctx, "UBUNTU", index.Page{})
	require.NoError(t, err)
	require.Len(t, found, 1)
	found, err = r.Search(ctx, "base", index.Page{})
	require.NoError(t, err)
	require.Len(t, found, 1)
	found, err = r.Search(ctx, "acme", index.Page{N: 1})
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, "acme/app", found[0].Name)

	require.NoError(t, r.Erase(ctx, "acme/web"))
	_, err = r.Get(ctx, "acme/web")
	require.ErrorIs(t, err, index.ErrNotFound)
}

func manifest(name string) index.Manifest {
	return index.Manifest{
		Digest:       d(name),
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		ArtifactType: "application/vnd.oci.image.config.v1+json",
		Size:         int64(len(name)),
		Annotations:  map[string]string{"name": name},
	}
}

func testManifests(t *testing.T, ix index.Index) {
	ctx := context.Background()
	m := ix.Manifest()

	_, err := m.Get(ctx, "acme/app", d("one"))
	require.ErrorIs(t, err, index.ErrNotFound)

	one := manifest("one")
	require.NoError(t, m.Put(ctx, "acme/app", one, []digest.Digest{d("config"), d("layer"), d("layer")}))
	require.NoError(t, m.Put(ctx, "acme/app", one, []digest.Digest{d("config"), d("layer")}))

	got, err := m.Get(ctx, "acme/app", one.Digest)
	require.NoError(t, err)
	require.Equal(t, one.MediaType, got.MediaType)
	require.Equal(t, one.ArtifactType, got.ArtifactType)
	require.Equal(t, one.Size, got.Size)
	require.Equal(t, "one", got.Annotations["name"])
	require.False(t, got.CreatedAt.IsZero())

	held, err := m.Holds(ctx, "acme/app", d("layer"))
	require.NoError(t, err)
	require.True(t, held)
	held, err = m.Holds(ctx, "acme/other", d("layer"))
	require.NoError(t, err)
	require.False(t, held)

	require.NoError(t, m.Put(ctx, "acme/app", manifest("two"), nil))
	vs, err := m.List(ctx, "acme/app", index.Page{})
	require.NoError(t, err)
	require.Len(t, vs, 2)
	require.Less(t, vs[0].Digest.String(), vs[1].Digest.String())
}

func testErase(t *testing.T, ix index.Index) {
	ctx := context.Background()
	m := ix.Manifest()

	child := manifest("child")
	require.NoError(t, m.Put(ctx, "r", child, []digest.Digest{d("config"), d("shared")}))
	require.NoError(t, m.Put(ctx, "r", manifest("other"), []digest.Digest{d("shared")}))
	idx := manifest("index")
	require.NoError(t, m.Put(ctx, "r", idx, []digest.Digest{child.Digest}))

	// The index releases nothing: its child is still a manifest here.
	released, err := m.Erase(ctx, "r", idx.Digest)
	require.NoError(t, err)
	require.Empty(t, released)

	// The child releases its config and keeps what the other one holds.
	released, err = m.Erase(ctx, "r", child.Digest)
	require.NoError(t, err)
	require.Equal(t, []digest.Digest{d("config")}, released)

	_, err = m.Erase(ctx, "r", child.Digest)
	require.ErrorIs(t, err, index.ErrNotFound)
}

func testReferrers(t *testing.T, ix index.Index) {
	ctx := context.Background()
	m := ix.Manifest()

	subject := d("subject")
	for _, at := range []string{"sig", "sbom", "sig2"} {
		v := manifest(at)
		v.ArtifactType = "application/" + at
		v.Subject = subject
		require.NoError(t, m.Put(ctx, "r", v, nil))
	}
	require.NoError(t, m.Put(ctx, "r", manifest("unrelated"), nil))

	vs, err := m.Referrers(ctx, "r", subject, "")
	require.NoError(t, err)
	require.Len(t, vs, 3)
	vs, err = m.Referrers(ctx, "r", subject, "application/sbom")
	require.NoError(t, err)
	require.Len(t, vs, 1)
	require.Equal(t, d("sbom"), vs[0].Digest)
	vs, err = m.Referrers(ctx, "other", subject, "")
	require.NoError(t, err)
	require.Empty(t, vs)
}

func testTags(t *testing.T, ix index.Index) {
	ctx := context.Background()
	g := ix.Tag()

	require.NoError(t, g.Set(ctx, "r", "latest", d("one"), ""))
	require.ErrorIs(t, g.Set(ctx, "r", "latest", d("two"), ""), index.ErrTagMoved)
	require.ErrorIs(t, g.Set(ctx, "r", "latest", d("two"), d("elsewhere")), index.ErrTagMoved)
	require.ErrorIs(t, g.Set(ctx, "r", "new", d("two"), d("one")), index.ErrTagMoved)

	v, err := g.Get(ctx, "r", "latest")
	require.NoError(t, err)
	require.Equal(t, d("one"), v.Digest)
	moved := v.MovedAt

	time.Sleep(10 * time.Millisecond)
	require.NoError(t, g.Set(ctx, "r", "latest", d("two"), d("one")))
	v, err = g.Get(ctx, "r", "latest")
	require.NoError(t, err)
	require.Equal(t, d("two"), v.Digest)
	require.True(t, v.MovedAt.After(moved))

	require.NoError(t, g.Set(ctx, "r", "stable", d("two"), ""))
	require.NoError(t, g.Set(ctx, "r", "alpha", d("one"), ""))

	names, err := g.List(ctx, "r", index.Page{})
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "latest", "stable"}, names)
	names, err = g.List(ctx, "r", index.Page{Last: "alpha", N: 1})
	require.NoError(t, err)
	require.Equal(t, []string{"latest"}, names)

	of, err := g.Of(ctx, "r", d("two"))
	require.NoError(t, err)
	require.Len(t, of, 2)

	all, err := g.All(ctx, "r")
	require.NoError(t, err)
	require.Len(t, all, 3)

	newest, err := g.Newest(ctx, "r")
	require.NoError(t, err)
	require.Equal(t, "alpha", newest.Name, "the last one set")

	require.NoError(t, g.Erase(ctx, "r", "alpha"))
	require.ErrorIs(t, g.Erase(ctx, "r", "alpha"), index.ErrNotFound)
	_, err = g.Get(ctx, "r", "alpha")
	require.ErrorIs(t, err, index.ErrNotFound)

	newest, err = g.Newest(ctx, "r")
	require.NoError(t, err)
	require.Equal(t, "stable", newest.Name)
	_, err = g.Newest(ctx, "none")
	require.ErrorIs(t, err, index.ErrNotFound)
}

func testRollback(t *testing.T, ix index.Index) {
	ctx := context.Background()
	boom := errors.New("boom")
	err := ix.Tx(ctx, "r", func(tx index.Index) error {
		if _, err := tx.Repo().Ensure(ctx, "r"); err != nil {
			return err
		}
		if err := tx.Manifest().Put(ctx, "r", manifest("m"), []digest.Digest{d("l")}); err != nil {
			return err
		}
		if err := tx.Tag().Set(ctx, "r", "latest", d("m"), ""); err != nil {
			return err
		}
		// Seen from inside.
		if _, err := tx.Tag().Get(ctx, "r", "latest"); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)

	_, err = ix.Repo().Get(ctx, "r")
	require.ErrorIs(t, err, index.ErrNotFound)
	_, err = ix.Manifest().Get(ctx, "r", d("m"))
	require.ErrorIs(t, err, index.ErrNotFound)
	_, err = ix.Tag().Get(ctx, "r", "latest")
	require.ErrorIs(t, err, index.ErrNotFound)

	require.NoError(t, ix.Tx(ctx, "r", func(tx index.Index) error {
		return tx.Tag().Set(ctx, "r", "latest", d("m"), "")
	}))
	_, err = ix.Tag().Get(ctx, "r", "latest")
	require.NoError(t, err)
}

func testBusy(t *testing.T, ix index.Index) {
	ctx := context.Background()
	held := make(chan struct{})
	done := make(chan struct{})

	var wg sync.WaitGroup
	wg.Go(func() {
		ix.Tx(ctx, "r", func(index.Index) error {
			close(held)
			<-done
			return nil
		})
	})

	<-held
	err := ix.Tx(ctx, "r", func(index.Index) error { return nil })
	close(done)
	wg.Wait()
	require.ErrorIs(t, err, index.ErrBusy)

	require.NoError(t, ix.Tx(ctx, "r", func(index.Index) error { return nil }))
}

func testMarks(t *testing.T, ix index.Index) {
	ctx := context.Background()
	m := ix.Manifest()
	require.NoError(t, m.Put(ctx, "r", manifest("a"), []digest.Digest{d("x"), d("y")}))
	require.NoError(t, m.Put(ctx, "r", manifest("b"), []digest.Digest{d("y"), d("a")}))
	require.NoError(t, m.Put(ctx, "other", manifest("c"), []digest.Digest{d("z")}))

	seen := map[digest.Digest]struct{}{}
	for v, err := range m.Marks(ctx, "r") {
		require.NoError(t, err)
		seen[v] = struct{}{}
	}
	got := []string{}
	for v := range seen {
		got = append(got, v.String())
	}
	slices.Sort(got)
	want := []string{d("a").String(), d("b").String(), d("x").String(), d("y").String()}
	slices.Sort(want)
	require.Equal(t, want, got)
}

// Flusher is an index whose pull bookkeeping is written later.
type Flusher interface {
	Flush(context.Context) error
}

func testPulled(t *testing.T, ix index.Index) {
	ctx := context.Background()
	m := manifest("m")
	require.NoError(t, ix.Manifest().Put(ctx, "r", m, nil))
	require.NoError(t, ix.Tag().Set(ctx, "r", "latest", m.Digest, ""))

	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	ix.Pulled().Touch("r", "latest", m.Digest, at)
	ix.Pulled().Touch("r", "latest", m.Digest, at.Add(-time.Minute))
	if f, ok := ix.(Flusher); ok {
		require.NoError(t, f.Flush(ctx))
	}

	got, err := ix.Manifest().Get(ctx, "r", m.Digest)
	require.NoError(t, err)
	require.True(t, at.Equal(got.PulledAt), "%v != %v", at, got.PulledAt)
	tag, err := ix.Tag().Get(ctx, "r", "latest")
	require.NoError(t, err)
	require.True(t, at.Equal(tag.PulledAt))
}

// testLock is the lock held outside a transaction keeping out a transaction
// that wants it, and a second lock.
func testLock(t *testing.T, ix index.Index) {
	ctx := context.Background()
	unlock, err := ix.Lock(ctx, "r")
	require.NoError(t, err)

	err = ix.Tx(ctx, "r", func(index.Index) error { return nil })
	require.ErrorIs(t, err, index.ErrBusy)
	_, err = ix.Lock(ctx, "r")
	require.ErrorIs(t, err, index.ErrBusy)

	unlock()
	require.NoError(t, ix.Tx(ctx, "r", func(index.Index) error { return nil }))
	unlock, err = ix.Lock(ctx, "r")
	require.NoError(t, err)
	unlock()
}
