package registry_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

// sizes is stores that remember the size every Add was told, by digest. It
// hides every capability of the stores beneath, so a mount copies through
// Add rather than linking.
type sizes struct {
	inner flob.Stores

	mu   sync.Mutex
	told map[flob.Digest]int64
}

func (s *sizes) Use(id string) flob.Store { return &sizesStore{Store: s.inner.Use(id), s: s} }

type sizesStore struct {
	flob.Store
	s *sizes
}

func (s *sizesStore) Add(ctx context.Context, m flob.Meta, r io.Reader) (flob.Meta, error) {
	s.s.mu.Lock()
	s.s.told[m.Digest] = m.Size
	s.s.mu.Unlock()
	return s.Store.Add(ctx, m, r)
}

// TestAddIsToldTheSize: wherever the registry knows how long a blob is before
// it reads it, it says so, since a store that streams an upload needs to; and
// where it does not know, it says nothing rather than guess.
func TestAddIsToldTheSize(t *testing.T) {
	s := &sizes{inner: flob.NewMemStores(), told: map[flob.Digest]int64{}}
	x := &harness{t: t, h: registry.New(registry.Config{Stores: s, Index: memindex.New()})}
	told := func(d digest.Digest) int64 {
		t.Helper()
		s.mu.Lock()
		defer s.mu.Unlock()
		n, ok := s.told[flob.Digest(d)]
		require.True(t, ok, "Add was not called for %s", d)
		return n
	}

	// A monolithic upload: its Content-Length.
	b := []byte("a monolithic upload")
	d := x.pushBlob("acme/a", b)
	require.EqualValues(t, len(b), told(d))

	// The same with no Content-Length, as a chunked transfer encoding sends
	// it: nothing is known, and nothing is said.
	b = []byte("sent chunked")
	d = digest.FromBytes(b)
	req := httptest.NewRequest("POST", "/v2/acme/a/blobs/uploads/?digest="+d.String(), io.NopCloser(bytes.NewReader(b)))
	req.ContentLength = -1
	w := httptest.NewRecorder()
	x.h.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	require.Zero(t, told(d))

	// A mount copied from another repository: the size the source has.
	b = []byte("mounted")
	d = x.pushBlob("acme/a", b)
	delete(s.told, flob.Digest(d))
	res := x.do("POST", "/v2/acme/b/blobs/uploads/?mount="+d.String()+"&from=acme/a", nil)
	require.Equal(t, http.StatusCreated, res.StatusCode)
	require.EqualValues(t, len(b), told(d))

	// A manifest: its bytes.
	body, _ := x.image("acme/a", "layer")
	require.Equal(t, http.StatusCreated, x.pushManifest("acme/a", "latest", body, v1.MediaTypeImageManifest).StatusCode)
	require.EqualValues(t, len(body), told(digest.FromBytes(body)))
}
