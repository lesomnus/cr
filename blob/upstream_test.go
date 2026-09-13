package blob

import (
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

func TestProxyName(t *testing.T) {
	require.True(t, Covers("docker.io", "docker.io/library/ubuntu"))
	require.False(t, Covers("docker.io", "docker.iox/library"))
	require.True(t, Covers("", "anything"))
}
