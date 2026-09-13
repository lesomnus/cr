package blob_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/lesomnus/flob"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/blob"
)

func TestRouter(t *testing.T) {
	ctx := context.Background()
	fallback := flob.NewMemStores()
	library := flob.NewMemStores()
	team := flob.NewMemStores()

	_, err := blob.NewRouter(blob.Route{Prefix: "library", Stores: library})
	require.Error(t, err)

	r, err := blob.NewRouter(
		blob.Route{Prefix: "", Stores: fallback},
		blob.Route{Prefix: "library", Stores: library},
		blob.Route{Prefix: "acme/team", Stores: team},
	)
	require.NoError(t, err)

	require.Same(t, library, r.Stores("library/ubuntu"))
	require.Same(t, library, r.Stores("library"))
	require.Same(t, fallback, r.Stores("librarything"))
	require.Same(t, team, r.Stores("acme/team/app"))
	require.Same(t, fallback, r.Stores("acme/app"))
	require.Len(t, r.Pools(), 3)

	for _, name := range []string{"library/ubuntu", "acme/app", "acme/team/app"} {
		_, err := r.Use(name).Add(ctx, flob.Meta{}, bytes.NewReader([]byte(name)))
		require.NoError(t, err)
	}
	// Written where the route says, and only there.
	_, err = library.Use("library/ubuntu").Stat(ctx, flob.DigestFromBytes([]byte("library/ubuntu")))
	require.NoError(t, err)
	_, err = fallback.Use("library/ubuntu").Stat(ctx, flob.DigestFromBytes([]byte("library/ubuntu")))
	require.ErrorIs(t, err, flob.ErrNotExist)

	// A namespace a pool holds for a route that no longer sends it there is
	// not listed.
	_, err = fallback.Use("library/stray").Add(ctx, flob.Meta{}, bytes.NewReader([]byte("stray")))
	require.NoError(t, err)

	names := map[string]bool{}
	for ns, err := range r.Namespaces(ctx) {
		require.NoError(t, err)
		names[ns] = true
	}
	require.Equal(t, map[string]bool{"library/ubuntu": true, "acme/app": true, "acme/team/app": true}, names)

	// A mount across pools is refused by the link, which is the registry's
	// cue to copy.
	l, ok := flob.AsLinker(r.Use("acme/app"))
	require.True(t, ok)
	_, err = l.Link(ctx, flob.DigestFromBytes([]byte("library/ubuntu")), r.Use("library/ubuntu"))
	require.ErrorIs(t, err, flob.ErrIncompatibleStore)
}
