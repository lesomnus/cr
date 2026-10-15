package gc_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/lesomnus/cr/gc"
	"github.com/lesomnus/cr/index"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestOrphanedCollection(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	const repo = "acme/app"
	subject, _ := e.image(repo, "latest", "subject")
	loose, _ := e.image(repo, "", "ordinary untagged")
	removed := e.referrer(repo, subject, "removed")
	tagged := e.referrer(repo, subject, "tagged")
	held := e.referrer(repo, subject, "held")
	pulled := e.referrer(repo, subject, "pulled")
	require.NoError(t, e.ix.Tag().Set(ctx, repo, "signature", tagged.Digest, ""))
	parent := e.manifest(repo, "index", v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{held}}, v1.MediaTypeImageIndex)
	e.clock.Add(30 * 24 * time.Hour)
	require.Equal(t, http.StatusAccepted, e.do("DELETE", "/v2/"+repo+"/manifests/"+subject.Digest.String(), nil))
	// A copy in a different repository does not protect these referrers.
	e.image("elsewhere", "latest", "subject")
	disabled := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Now: e.clock.Now})
	r, err := disabled.Run(ctx)
	require.NoError(t, err)
	require.Zero(t, r.Manifests)
	collector := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Now: e.clock.Now, Orphaned: 24 * time.Hour})
	r, err = collector.Run(ctx)
	require.NoError(t, err)
	require.Zero(t, r.Manifests, "old signatures get a new grace after subject deletion")
	e.clock.Add(25 * time.Hour)
	e.ix.Pulled().Touch(repo, "", pulled.Digest, e.clock.Now())
	r, err = collector.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, r.Manifests)
	require.False(t, e.indexed(repo, removed.Digest))
	require.False(t, e.has(repo, removed.Digest))
	for _, d := range []digest.Digest{loose.Digest, tagged.Digest, held.Digest, pulled.Digest} {
		require.True(t, e.indexed(repo, d), d.String())
	}
	_, err = e.ix.Orphan().Get(ctx, repo, subject.Digest)
	require.NoError(t, err, "protected refs retain their candidate")
	require.NoError(t, e.ix.Tag().Erase(ctx, repo, "signature"))
	require.Equal(t, http.StatusAccepted, e.do("DELETE", "/v2/"+repo+"/manifests/"+parent.Digest.String(), nil))
	e.clock.Add(25 * time.Hour)
	r, err = collector.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, r.Manifests)
	_, err = e.ix.Orphan().Get(ctx, repo, subject.Digest)
	require.ErrorIs(t, err, index.ErrNotFound)
	require.True(t, e.indexed(repo, loose.Digest), "untagged remains disabled")
}

func TestOrphanedArrivalAndReturn(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	const repo = "acme/app"
	subject, _ := e.image("other", "latest", "subject")
	first := e.referrer(repo, subject, "first") // referrer before its subject
	collector := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Now: e.clock.Now, Orphaned: time.Hour})
	e.clock.Add(2 * time.Hour)
	second := e.referrer(repo, subject, "second")
	r, err := collector.Run(ctx)
	require.NoError(t, err)
	require.Zero(t, r.Manifests, "a new referrer refreshes the shared grace")
	e.image(repo, "latest", "subject")
	r, err = collector.Run(ctx)
	require.NoError(t, err)
	require.Zero(t, r.Manifests)
	_, err = e.ix.Orphan().Get(ctx, repo, subject.Digest)
	require.ErrorIs(t, err, index.ErrNotFound)
	require.Equal(t, http.StatusAccepted, e.do("DELETE", "/v2/"+repo+"/manifests/"+subject.Digest.String(), nil))
	e.clock.Add(2 * time.Hour)
	r, err = collector.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, r.Manifests)
	require.False(t, e.indexed(repo, first.Digest))
	require.False(t, e.indexed(repo, second.Digest))
}

func TestOrphanedInitialDiscoveryAndChains(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	const repo = "acme/app"
	absent := v1.Descriptor{Digest: digest.FromString("absent"), MediaType: v1.MediaTypeImageManifest, Size: 1}
	first := e.referrer(repo, absent, "first")
	second := e.referrer(repo, first, "second")
	// Simulate a legacy row whose insertion never registered a candidate.
	require.NoError(t, e.ix.Orphan().Erase(ctx, repo, absent.Digest))
	e.clock.Add(30 * 24 * time.Hour)
	collector := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Now: e.clock.Now, Orphaned: time.Hour})
	r, err := collector.Run(ctx)
	require.NoError(t, err)
	require.Zero(t, r.Manifests, "discovered legacy orphans start a fresh grace")
	e.clock.Add(2 * time.Hour)
	r, err = collector.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, r.Manifests)
	require.False(t, e.indexed(repo, first.Digest))
	require.True(t, e.indexed(repo, second.Digest))
	e.clock.Add(2 * time.Hour)
	r, err = collector.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, r.Manifests)
	require.False(t, e.indexed(repo, second.Digest))
}

func TestOrphanedRescan(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	const repo = "acme/app"
	absent := v1.Descriptor{Digest: digest.FromString("absent"), MediaType: v1.MediaTypeImageManifest, Size: 1}
	old := e.referrer(repo, absent, "old")
	missing := v1.Descriptor{Digest: digest.FromString("missing"), MediaType: v1.MediaTypeImageManifest, Size: 1}
	legacy := e.referrer(repo, missing, "legacy")
	done, err := e.ix.Orphan().Seed(ctx, repo)
	require.NoError(t, err)
	require.True(t, done)
	require.NoError(t, e.ix.Orphan().Erase(ctx, repo, missing.Digest))
	e.clock.Add(2 * time.Hour)
	collector := gc.New(gc.Config{Stores: e.stores, Index: e.ix, Now: e.clock.Now, Orphaned: time.Hour, RescanOrphans: true})
	r, err := collector.Collect(ctx, gc.KindOnline, gc.TriggerCli)
	require.NoError(t, err)
	require.Equal(t, 1, r.Manifests, "existing candidates keep their original grace")
	require.False(t, e.indexed(repo, old.Digest))
	require.True(t, e.indexed(repo, legacy.Digest), "new discoveries get a fresh grace")
	candidate, err := e.ix.Orphan().Get(ctx, repo, missing.Digest)
	require.NoError(t, err)
	require.Equal(t, e.clock.Now(), candidate.ObservedAt)
	cs, err := e.ix.Orphan().List(ctx, repo, index.Page{})
	require.NoError(t, err)
	require.Len(t, cs, 1, "collected candidates are removed")
	e.clock.Add(2 * time.Hour)
	r, err = gc.New(gc.Config{Stores: e.stores, Index: e.ix, Now: e.clock.Now, Orphaned: time.Hour}).Collect(ctx, gc.KindOnline, gc.TriggerCli)
	require.NoError(t, err)
	require.Equal(t, 1, r.Manifests)
	cs, err = e.ix.Orphan().List(ctx, repo, index.Page{})
	require.NoError(t, err)
	require.Empty(t, cs)
}

func TestOrphanedRescanSkipped(t *testing.T) {
	for _, reason := range []string{"disabled", "cache retention"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t)
			const repo = "acme/app"
			absent := v1.Descriptor{Digest: digest.FromString("absent"), MediaType: v1.MediaTypeImageManifest, Size: 1}
			ref := e.referrer(repo, absent, "ref")
			done, err := e.ix.Orphan().Seed(ctx, repo)
			require.NoError(t, err)
			require.True(t, done)
			require.NoError(t, e.ix.Orphan().Erase(ctx, repo, absent.Digest))
			cfg := gc.Config{Stores: e.stores, Index: e.ix, Now: e.clock.Now, RescanOrphans: true}
			if reason == "cache retention" {
				cfg.Orphaned = time.Hour
				cfg.Cache = func(string) time.Duration { return 24 * time.Hour }
			}
			r, err := gc.New(cfg).Run(ctx)
			require.NoError(t, err)
			require.Zero(t, r.Manifests)
			_, err = e.ix.Orphan().Get(ctx, repo, absent.Digest)
			require.ErrorIs(t, err, index.ErrNotFound, "no discovery in skipped repositories")
			require.True(t, e.indexed(repo, ref.Digest))
		})
	}
}

// Change the index after enumeration but before a deletion transaction. Each
// deletion must read the current candidate and subject, not the stale list.
type beforeDeleteIndex struct {
	index.Index
	calls  int
	change func()
}

func (i *beforeDeleteIndex) Tx(ctx context.Context, repo string, f func(index.Index) error) error {
	i.calls++
	if i.calls == 2 {
		i.change()
	}
	return i.Index.Tx(ctx, repo, f)
}
func TestOrphanedConcurrentChanges(t *testing.T) {
	for _, change := range []string{"subject returns", "new referrer", "tag", "holder", "pull"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t)
			const repo = "acme/app"
			subject, _ := e.image("other", "latest", "subject")
			ref := e.referrer(repo, subject, "ref")
			e.clock.Add(2 * time.Hour)
			wrapped := &beforeDeleteIndex{Index: e.ix}
			wrapped.change = func() {
				switch change {
				case "subject returns":
					e.image(repo, "latest", "subject")
				case "new referrer":
					e.referrer(repo, subject, "new")
				case "tag":
					require.NoError(t, e.ix.Tag().Set(ctx, repo, "keep", ref.Digest, ""))
				case "holder":
					e.manifest(repo, "keep", v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{ref}}, v1.MediaTypeImageIndex)
				case "pull":
					e.ix.Pulled().Touch(repo, "", ref.Digest, e.clock.Now())
				}
			}
			collector := gc.New(gc.Config{Stores: e.stores, Index: wrapped, Now: e.clock.Now, Orphaned: time.Hour})
			r, err := collector.Run(ctx)
			require.NoError(t, err)
			require.Zero(t, r.Manifests)
			require.GreaterOrEqual(t, wrapped.calls, 2)
			require.True(t, e.indexed(repo, ref.Digest))
		})
	}
}
