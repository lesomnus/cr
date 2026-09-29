package blob

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseChallenge(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/ubuntu:pull"`)
	require.Equal(t, "Bearer", scheme)
	require.Equal(t, map[string]string{
		"realm":   "https://auth.docker.io/token",
		"service": "registry.docker.io",
		"scope":   "repository:library/ubuntu:pull",
	}, params)

	// A scope with two actions has a comma inside its quotes.
	_, params = parseChallenge(`Bearer realm="https://r.example/token", scope="repository:a:pull,push", error="insufficient_scope"`)
	require.Equal(t, "repository:a:pull,push", params["scope"])
	require.Equal(t, "insufficient_scope", params["error"])

	scheme, params = parseChallenge(`Basic realm=cr`)
	require.Equal(t, "Basic", scheme)
	require.Equal(t, "cr", params["realm"])
}

// TestUpstreamWithoutAChallenge: a 401 with no challenge, which Docker Hub
// answers for a name it does not have, is refused, and says it had none.
func TestUpstreamWithoutAChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	u, err := NewUpstream(srv.URL, "", "")
	require.NoError(t, err)

	_, err = u.HeadManifest(context.Background(), "docker.io/library/alpine", "3.20")
	require.ErrorIs(t, err, ErrUpstreamUnauthorized)
	require.ErrorContains(t, err, "401 without a challenge")
}

func TestUpstreamRegistry(t *testing.T) {
	for raw, want := range map[string]string{
		"https://registry-1.docker.io": "docker.io",
		"https://index.docker.io/":     "docker.io",
		"https://GHCR.io":              "ghcr.io",
		"http://127.0.0.1:5000/base":   "127.0.0.1:5000",
	} {
		u, err := NewUpstream(raw, "", "")
		require.NoError(t, err)
		require.Equal(t, want, u.Registry(), raw)
	}
}

func TestProxyName(t *testing.T) {
	require.True(t, Covers("docker.io", "docker.io/library/ubuntu"))
	require.False(t, Covers("docker.io", "docker.iox/library"))
	require.True(t, Covers("", "anything"))
}
