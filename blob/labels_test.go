package blob_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/blob"
)

func TestTagsOf(t *testing.T) {
	require.Equal(t, []string{"a", "b"}, blob.TagsOf(flob.Labels{"Tag": {"a", "b"}}))
	// As S3 metadata gives them back.
	require.Equal(t, []string{"a", "b", "c"}, blob.TagsOf(flob.Labels{"Tag": {"a, b", "c,a"}}))
	require.Empty(t, blob.TagsOf(flob.Labels{}))
}

func TestLabelTag(t *testing.T) {
	ctx := context.Background()
	s := flob.NewMemStores().Use("acme/app")
	m, err := s.Add(ctx, flob.Meta{}, bytes.NewReader([]byte(`{"schemaVersion":2}`)))
	require.NoError(t, err)
	d := digest.Digest(m.Digest)

	tags := func() []string {
		info, err := s.Stat(ctx, m.Digest)
		require.NoError(t, err)
		ls, err := info.Labels(ctx)
		require.NoError(t, err)
		return blob.TagsOf(ls)
	}

	require.NoError(t, blob.LabelTag(ctx, s, d, "v1", true))
	require.NoError(t, blob.LabelTag(ctx, s, d, "latest", true))
	require.NoError(t, blob.LabelTag(ctx, s, d, "latest", true))
	require.Equal(t, []string{"v1", "latest"}, tags())

	// Labels a store joined are taken apart before one is removed.
	require.NoError(t, s.Label(ctx, m.Digest, flob.Labels{"Tag": {"v1, latest, nightly"}}))
	require.NoError(t, blob.LabelTag(ctx, s, d, "latest", false))
	require.Equal(t, []string{"v1", "nightly"}, tags())

	require.NoError(t, blob.LabelTag(ctx, s, d, "v1", false))
	require.NoError(t, blob.LabelTag(ctx, s, d, "nightly", false))
	require.Empty(t, tags())

	// A manifest that is not there is nothing to label.
	require.NoError(t, blob.LabelTag(ctx, s, digest.FromString("absent"), "v1", true))
}
