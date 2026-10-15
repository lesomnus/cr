package indextest

import (
	"context"
	"errors"
	"github.com/lesomnus/cr/index"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func testOrphans(t *testing.T, ix index.Index) {
	ctx := context.Background()
	const repo = "acme/app"
	_, err := ix.Repo().Ensure(ctx, repo)
	require.NoError(t, err)
	subject := manifest("subject")
	ref := manifest("ref")
	ref.Subject = subject.Digest
	require.NoError(t, ix.Manifest().Put(ctx, repo, subject, nil))
	require.NoError(t, ix.Manifest().Put(ctx, repo, ref, nil))
	_, err = ix.Orphan().Get(ctx, repo, subject.Digest)
	require.ErrorIs(t, err, index.ErrNotFound, "a live subject is not a candidate")

	fail := errors.New("rollback")
	err = ix.Tx(ctx, repo, func(tx index.Index) error {
		_, err := tx.Manifest().Erase(ctx, repo, subject.Digest)
		require.NoError(t, err)
		_, err = tx.Orphan().Get(ctx, repo, subject.Digest)
		require.NoError(t, err)
		return fail
	})
	require.ErrorIs(t, err, fail)
	_, err = ix.Orphan().Get(ctx, repo, subject.Digest)
	require.ErrorIs(t, err, index.ErrNotFound)
	_, err = ix.Manifest().Get(ctx, repo, subject.Digest)
	require.NoError(t, err)

	_, err = ix.Manifest().Erase(ctx, repo, subject.Digest)
	require.NoError(t, err)
	first, err := ix.Orphan().Get(ctx, repo, subject.Digest)
	require.NoError(t, err)
	require.False(t, first.ObservedAt.IsZero())
	require.NoError(t, ix.Manifest().Put(ctx, repo, ref, nil))
	same, err := ix.Orphan().Get(ctx, repo, subject.Digest)
	require.NoError(t, err)
	require.Equal(t, first.ObservedAt, same.ObservedAt, "idempotent re-push does not postpone collection")

	time.Sleep(time.Millisecond)
	another := manifest("another")
	another.Subject = subject.Digest
	require.NoError(t, ix.Manifest().Put(ctx, repo, another, nil))
	refreshed, err := ix.Orphan().Get(ctx, repo, subject.Digest)
	require.NoError(t, err)
	require.True(t, refreshed.ObservedAt.After(first.ObservedAt))
	candidates, err := ix.Orphan().List(ctx, repo, index.Page{})
	require.NoError(t, err)
	require.Len(t, candidates, 1, "referrers share a subject candidate")

	// A subject in another repository does not rescue this one.
	other := manifest("other-ref")
	other.Subject = subject.Digest
	require.NoError(t, ix.Manifest().Put(ctx, "other", subject, nil))
	require.NoError(t, ix.Manifest().Put(ctx, repo, other, nil))
	_, err = ix.Orphan().Get(ctx, repo, subject.Digest)
	require.NoError(t, err)
	err = ix.Tx(ctx, repo, func(tx index.Index) error {
		bad := manifest("aborted-ref")
		bad.Subject = d("absent")
		require.NoError(t, tx.Manifest().Put(ctx, repo, bad, nil))
		return fail
	})
	require.ErrorIs(t, err, fail)
	_, err = ix.Orphan().Get(ctx, repo, d("absent"))
	require.ErrorIs(t, err, index.ErrNotFound)

	// Simulate rows predating candidate tracking. Only the initial scan finds
	// them; completion and candidates are transactional together.
	require.NoError(t, ix.Orphan().Erase(ctx, repo, subject.Digest))
	err = ix.Tx(ctx, repo, func(tx index.Index) error {
		done, err := tx.Orphan().Seed(ctx, repo)
		require.NoError(t, err)
		require.True(t, done)
		return fail
	})
	require.ErrorIs(t, err, fail)
	_, err = ix.Orphan().Get(ctx, repo, subject.Digest)
	require.ErrorIs(t, err, index.ErrNotFound)
	done, err := ix.Orphan().Seed(ctx, repo)
	require.NoError(t, err)
	require.True(t, done)
	_, err = ix.Orphan().Get(ctx, repo, subject.Digest)
	require.NoError(t, err)
	require.NoError(t, ix.Orphan().Erase(ctx, repo, subject.Digest))
	done, err = ix.Orphan().Seed(ctx, repo)
	require.NoError(t, err)
	require.True(t, done)
	_, err = ix.Orphan().Get(ctx, repo, subject.Digest)
	require.ErrorIs(t, err, index.ErrNotFound, "completed scans do not rescan manifests")

	require.NoError(t, ix.Manifest().Put(ctx, repo, subject, nil))
	_, err = ix.Manifest().Erase(ctx, repo, subject.Digest)
	require.NoError(t, err)
	require.NoError(t, ix.Repo().Erase(ctx, repo))
	candidates, err = ix.Orphan().List(ctx, repo, index.Page{})
	require.NoError(t, err)
	require.Empty(t, candidates)
	// Repository deletion also forgets its scan checkpoint.
	require.NoError(t, ix.Manifest().Put(ctx, repo, ref, nil))
	require.NoError(t, ix.Orphan().Erase(ctx, repo, subject.Digest))
	done, err = ix.Orphan().Seed(ctx, repo)
	require.NoError(t, err)
	require.True(t, done)
	_, err = ix.Orphan().Get(ctx, repo, subject.Digest)
	require.NoError(t, err)
}
