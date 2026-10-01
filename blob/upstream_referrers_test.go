package blob

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestReferrersFollowsPages(t *testing.T) {
	subject := digest.FromString("subject")
	page := func(w http.ResponseWriter, ds ...string) {
		ix := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex}
		for _, d := range ds {
			ix.Manifests = append(ix.Manifests, v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromString(d)})
		}
		w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
		json.NewEncoder(w).Encode(ix)
	}
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		switch r.URL.Query().Get("last") {
		case "":
			// Relative to the page, the way a registry usually writes it.
			w.Header().Set("Link", `</base/v2/app/referrers/`+subject.String()+`?last=a>; rel="next"`)
			page(w, "a")
		case "a":
			w.Header().Set("Link", `<`+"http://"+r.Host+`/base/v2/app/referrers/`+subject.String()+`?last=b>; rel="next"`)
			page(w, "b")
		default:
			page(w, "c")
		}
	}))
	defer srv.Close()

	u, err := NewUpstream(srv.URL + "/base")
	require.NoError(t, err)
	ds, err := u.Referrers(context.Background(), "app", subject, 1<<20)
	require.NoError(t, err)
	require.Len(t, ds, 3)
	require.Equal(t, digest.FromString("c"), ds[2].Digest)
	require.Equal(t, []string{
		"/base/v2/app/referrers/" + subject.String(),
		"/base/v2/app/referrers/" + subject.String() + "?last=a",
		"/base/v2/app/referrers/" + subject.String() + "?last=b",
	}, asked)
}

func TestReferrersRefusesAnotherHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://elsewhere.example/v2/app/referrers/x?last=a>; rel="next"`)
		json.NewEncoder(w).Encode(v1.Index{})
	}))
	defer srv.Close()

	u, err := NewUpstream(srv.URL)
	require.NoError(t, err)
	_, err = u.Referrers(context.Background(), "app", digest.FromString("s"), 1<<20)
	require.ErrorContains(t, err, "another host")
}

func TestUpstreamErrors(t *testing.T) {
	status := http.StatusNotFound
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	u, err := NewUpstream(srv.URL)
	require.NoError(t, err)
	get := func() error {
		_, err := u.Referrers(context.Background(), "app", digest.FromString("s"), 1<<20)
		return err
	}

	require.ErrorIs(t, get(), flob.ErrNotExist, "no referrers API")

	status = http.StatusInternalServerError
	err = get()
	var se *UpstreamStatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusInternalServerError, se.Status)
	require.NotErrorIs(t, err, ErrUpstreamUnauthorized)

	status = http.StatusForbidden
	require.ErrorIs(t, get(), ErrUpstreamUnauthorized)

	srv.Close()
	require.ErrorIs(t, get(), ErrUpstreamUnreachable)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = u.Referrers(ctx, "app", digest.FromString("s"), 1<<20)
	require.ErrorIs(t, err, context.Canceled, "the client giving up is still that")
}
