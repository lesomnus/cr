package blob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func unavailableOrigin(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := "http://" + l.Addr().String()
	require.NoError(t, l.Close())
	return address
}

func TestOrderedUpstreamManifestPriorityAndFailures(t *testing.T) {
	body := []byte(`{"schemaVersion":2}`)
	d := digest.FromBytes(body)
	for _, status := range []int{200, 401, 403, 404, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var secondary atomic.Int32
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondary.Add(1)
				require.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
				w.Write(body)
			}))
			defer fallback.Close()
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
				w.WriteHeader(status)
				if status == 200 {
					w.Write(body)
				}
			}))
			defer primary.Close()
			u, err := NewUpstreams([]string{primary.URL, fallback.URL}, WithBearer(Literal("fixture-token")))
			require.NoError(t, err)
			got, _, gotDigest, err := u.GetManifest(t.Context(), "dist/prod/demo", d.String(), 4096)
			if status == 200 || status >= 500 {
				require.NoError(t, err)
				require.Equal(t, body, got)
				require.Equal(t, d, gotDigest)
			} else {
				require.Error(t, err)
				if status == 404 {
					require.ErrorIs(t, err, flob.ErrNotExist)
				}
			}
			if status >= 500 {
				require.EqualValues(t, 1, secondary.Load())
			} else {
				require.Zero(t, secondary.Load())
			}
		})
	}
}

func TestOrderedUpstreamConnectionTimeoutAndCancellation(t *testing.T) {
	d := digest.FromString("manifest")
	var calls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Docker-Content-Digest", d.String())
	}))
	defer second.Close()
	u, err := NewUpstreams([]string{unavailableOrigin(t), second.URL})
	require.NoError(t, err)
	got, err := u.HeadManifest(t.Context(), "dist/demo", "release")
	require.NoError(t, err)
	require.Equal(t, d, got)
	require.EqualValues(t, 1, calls.Load())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = u.HeadManifest(ctx, "dist/demo", "release")
	require.ErrorIs(t, err, context.Canceled)
	require.EqualValues(t, 1, calls.Load())
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer stall.Close()
	u, err = NewUpstreams([]string{stall.URL, second.URL})
	require.NoError(t, err)
	u.client.Transport.(*http.Transport).ResponseHeaderTimeout = 20 * time.Millisecond
	got, err = u.HeadManifest(t.Context(), "dist/demo", "release")
	require.NoError(t, err)
	require.Equal(t, d, got)
	require.EqualValues(t, 2, calls.Load())
}

func TestOrderedUpstreamManifestKeepsResolvedDigest(t *testing.T) {
	body := []byte("the primary's image")
	d := digest.FromBytes(body)
	var fallbackPath string
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Docker-Content-Digest", d.String())
			return
		}
		w.WriteHeader(503)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackPath = r.URL.Path
		if strings.HasSuffix(r.URL.Path, "/release") {
			w.Write([]byte("a moved tag"))
			return
		}
		w.Write(body)
	}))
	defer secondary.Close()
	u, err := NewUpstreams([]string{primary.URL, secondary.URL})
	require.NoError(t, err)
	got, _, gotDigest, err := u.GetManifest(t.Context(), "dist/demo", "release", 4096)
	require.NoError(t, err)
	require.Equal(t, body, got)
	require.Equal(t, d, gotDigest)
	require.Equal(t, "/v2/dist/demo/manifests/"+d.String(), fallbackPath)
}

func TestOrderedUpstreamInvalidContentAndCredentialsStop(t *testing.T) {
	var calls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer fallback.Close()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("wrong bytes")) }))
	defer primary.Close()
	u, err := NewUpstreams([]string{primary.URL, fallback.URL})
	require.NoError(t, err)
	_, _, _, err = u.GetManifest(t.Context(), "dist/demo", digest.FromString("expected").String(), 4096)
	require.ErrorContains(t, err, "hashes to")
	require.Zero(t, calls.Load())
	u, err = NewUpstreams([]string{primary.URL, fallback.URL}, WithBearer(fileSecret(t, filepath.Join(t.TempDir(), "missing"))))
	require.NoError(t, err)
	_, err = u.HeadManifest(t.Context(), "dist/demo", "release")
	require.ErrorIs(t, err, ErrUpstreamUnauthorized)
	require.Zero(t, calls.Load())
}

func TestOrderedUpstreamReferrersRestartWholePagination(t *testing.T) {
	first, second := digest.FromString("first"), digest.FromString("second")
	index := func(d digest.Digest) []byte {
		b, _ := json.Marshal(v1.Index{Manifests: []v1.Descriptor{{Digest: d}}})
		return b
	}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Link", `<?page=2>; rel="next"`)
		w.Write(index(first))
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.URL.RawQuery, "a new origin starts from its first page")
		w.Write(index(second))
	}))
	defer fallback.Close()
	u, err := NewUpstreams([]string{primary.URL, fallback.URL})
	require.NoError(t, err)
	descriptors, err := u.Referrers(t.Context(), "dist/demo", first, 4096)
	require.NoError(t, err)
	require.Len(t, descriptors, 1)
	require.Equal(t, second, descriptors[0].Digest)
}

func TestOrderedUpstreamBlobResumesInterruptedBodyAndSeeks(t *testing.T) {
	body := []byte("complete content-addressed layer")
	d := flob.Digest(digest.FromBytes(body))
	var fallbackCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if r.Method != http.MethodHead {
			w.Write(body[:7])
		}
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		require.Equal(t, "/v2/dist/demo/blobs/"+string(d), r.URL.Path)
		var offset int
		_, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &offset)
		require.NoError(t, err)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[offset:])
	}))
	defer secondary.Close()
	u, err := NewUpstreams([]string{primary.URL, secondary.URL})
	require.NoError(t, err)
	s := u.Stores(func(r string) string { return r }).Use("dist/demo")
	_, err = s.Stat(t.Context(), d)
	require.NoError(t, err)
	r, _, err := s.Open(t.Context(), d)
	require.NoError(t, err)
	defer r.Close()
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, body, got)
	_, err = r.Seek(2, io.SeekStart)
	require.NoError(t, err)
	got, err = io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, body[2:], got)
	require.EqualValues(t, 2, fallbackCalls.Load())
}

func TestOrderedUpstreamBlobHeaderFailuresAndMissing(t *testing.T) {
	for _, status := range []int{404, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			body := []byte("layer")
			var calls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer primary.Close()
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				if r.Method != http.MethodHead {
					w.Write(body)
				}
			}))
			defer secondary.Close()
			u, err := NewUpstreams([]string{primary.URL, secondary.URL})
			require.NoError(t, err)
			s := u.Stores(func(r string) string { return r }).Use("dist/demo")
			d := flob.Digest(digest.FromBytes(body))
			_, err = s.Stat(t.Context(), d)
			if status == 404 {
				require.ErrorIs(t, err, flob.ErrNotExist)
			} else {
				require.NoError(t, err)
			}
			r, _, err := s.Open(t.Context(), d)
			if status == 404 {
				require.ErrorIs(t, err, flob.ErrNotExist)
				require.Zero(t, calls.Load())
			} else {
				require.NoError(t, err)
				defer r.Close()
				got, err := io.ReadAll(r)
				require.NoError(t, err)
				require.Equal(t, body, got)
				require.EqualValues(t, 2, calls.Load())
			}
		})
	}
}

func TestOrderedUpstreamConfigurationValidation(t *testing.T) {
	for _, urls := range [][]string{nil, {}, {""}, {"file:///tmp/blob"}, {"https://user:pass@host"}, {"https://host?token=x"}, {"https://host/#x"}} {
		_, err := NewUpstreams(urls)
		require.Error(t, err)
	}
	u, err := NewUpstreams([]string{unavailableOrigin(t), unavailableOrigin(t)})
	require.NoError(t, err)
	_, err = u.HeadManifest(t.Context(), "dist/demo", "release")
	require.ErrorIs(t, err, ErrUpstreamUnreachable)
	require.False(t, errors.Is(err, flob.ErrNotExist))
}

func TestOrderedUpstreamChallengeTokensStayWithTheirOrigin(t *testing.T) {
	d := digest.FromString("manifest")
	var primaryTokens, fallbackTokens atomic.Int32
	realm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		require.True(t, ok)
		require.Equal(t, "reader", user)
		require.Equal(t, "password", password)
		if r.URL.Path == "/primary" {
			primaryTokens.Add(1)
			io.WriteString(w, `{"token":"primary-token","expires_in":3600}`)
		} else {
			fallbackTokens.Add(1)
			io.WriteString(w, `{"token":"fallback-token","expires_in":3600}`)
		}
	}))
	defer realm.Close()
	registry := func(which, token string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			require.True(t, auth == "" || auth == "Bearer "+token, "a challenge token must never travel to another origin")
			if auth == "" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/%s",service="%s"`, realm.URL, which, which))
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Docker-Content-Digest", d.String())
			w.WriteHeader(status)
		}))
	}
	primary := registry("primary", "primary-token", 503)
	defer primary.Close()
	fallback := registry("fallback", "fallback-token", 200)
	defer fallback.Close()
	u, err := NewUpstreams([]string{primary.URL, fallback.URL}, WithPassword("reader", Literal("password")))
	require.NoError(t, err)
	for range 2 {
		got, err := u.HeadManifest(t.Context(), "dist/demo", "release")
		require.NoError(t, err)
		require.Equal(t, d, got)
	}
	require.EqualValues(t, 1, primaryTokens.Load())
	require.EqualValues(t, 1, fallbackTokens.Load())
}

func TestOrderedUpstreamInterruptedManifestAndTokenFailures(t *testing.T) {
	body := []byte(`{"schemaVersion":2}`)
	d := digest.FromBytes(body)
	var calls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write(body)
	}))
	defer fallback.Close()
	truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write(body[:5])
	}))
	defer truncated.Close()
	u, err := NewUpstreams([]string{truncated.URL, fallback.URL})
	require.NoError(t, err)
	got, _, _, err := u.GetManifest(t.Context(), "dist/demo", d.String(), 4096)
	require.NoError(t, err)
	require.Equal(t, body, got)
	require.EqualValues(t, 1, calls.Load())
	for _, status := range []int{401, 404, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			realm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer realm.Close()
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token"`, realm.URL))
				w.WriteHeader(401)
			}))
			defer primary.Close()
			before := calls.Load()
			u, err := NewUpstreams([]string{primary.URL, fallback.URL})
			require.NoError(t, err)
			_, _, _, err = u.GetManifest(t.Context(), "dist/demo", d.String(), 4096)
			if status == 503 {
				require.NoError(t, err)
				require.Equal(t, before+1, calls.Load())
			} else {
				require.ErrorIs(t, err, ErrUpstreamUnauthorized)
				require.Equal(t, before, calls.Load())
			}
		})
	}
}

func TestOrderedUpstreamMissingAfterFailureStopsRemainingOrigins(t *testing.T) {
	var calls atomic.Int32
	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer third.Close()
	u, err := NewUpstreams([]string{unavailableOrigin(t), missing.URL, third.URL})
	require.NoError(t, err)
	_, _, _, err = u.GetManifest(t.Context(), "dist/demo", digest.FromString("missing").String(), 4096)
	require.ErrorIs(t, err, flob.ErrNotExist)
	require.NotErrorIs(t, err, ErrUpstreamUnreachable, "the terminal error determines the registry's response")
	require.Zero(t, calls.Load())
}

func TestOrderedUpstreamTLSFailureCanAdvance(t *testing.T) {
	primary := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS must fail before HTTP") }))
	primary.Config.ErrorLog = log.New(io.Discard, "", 0)
	primary.StartTLS()
	defer primary.Close()
	d := digest.FromString("manifest")
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Docker-Content-Digest", d.String()) }))
	defer fallback.Close()
	u, err := NewUpstreams([]string{primary.URL, fallback.URL})
	require.NoError(t, err)
	got, err := u.HeadManifest(t.Context(), "dist/demo", "release")
	require.NoError(t, err)
	require.Equal(t, d, got)
}

func TestOrderedUpstreamBadReferrersAndRangesAreTerminal(t *testing.T) {
	var fallbackCalls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fallbackCalls.Add(1) }))
	defer fallback.Close()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "{invalid JSON") }))
	defer primary.Close()
	u, err := NewUpstreams([]string{primary.URL, fallback.URL})
	require.NoError(t, err)
	_, err = u.Referrers(t.Context(), "dist/demo", digest.FromString("subject"), 4096)
	require.Error(t, err)
	require.Zero(t, fallbackCalls.Load())
	layer := []byte("layer bytes")
	ranges := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", "bytes 0-9/10") // does not begin at requested offset
			w.WriteHeader(http.StatusPartialContent)
			w.Write(layer)
			return
		}
		w.Write(layer)
	}))
	defer ranges.Close()
	u, err = NewUpstreams([]string{ranges.URL, fallback.URL})
	require.NoError(t, err)
	r, _, err := u.Stores(func(r string) string { return r }).Use("dist/demo").Open(t.Context(), flob.Digest(digest.FromBytes(layer)))
	require.NoError(t, err)
	defer r.Close()
	_, err = r.Seek(2, io.SeekStart)
	require.NoError(t, err)
	_, err = io.ReadAll(r)
	require.ErrorContains(t, err, "Content-Range")
	require.Zero(t, fallbackCalls.Load())
}
