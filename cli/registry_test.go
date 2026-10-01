package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lesomnus/flob"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/cmd"
)

func TestProxyHosts(t *testing.T) {
	proxies := func(ps ...cmd.ProxyConfig) error {
		_, _, _, err := Proxies(cmd.RegistryConfig{Proxies: ps}, flob.NewMemStores(), nil)
		return err
	}
	hub := func(hosts ...string) cmd.ProxyConfig {
		return cmd.ProxyConfig{Prefix: "docker.io", Upstream: "https://registry-1.docker.io", Hosts: hosts}
	}
	gh := func(hosts ...string) cmd.ProxyConfig {
		return cmd.ProxyConfig{Prefix: "ghcr.io", Upstream: "https://ghcr.io", Hosts: hosts}
	}

	require.NoError(t, proxies(hub("dockerhub.example.com", "hub.example.com"), gh("ghcr.example.com")))

	err := proxies(hub("mirror.example.com"), gh("Mirror.example.com"))
	require.ErrorContains(t, err, `registry.proxies[1].hosts[0]: "mirror.example.com" is already "docker.io"'s`)

	require.ErrorContains(t, proxies(hub("dockerhub.example.com:5000")), "is not a host name")
	require.ErrorContains(t, proxies(hub("https://dockerhub.example.com")), "is not a host name")
	require.ErrorContains(t, proxies(hub("")), "is not a host name")

	empty := hub("mirror.example.com")
	empty.Prefix = ""
	require.ErrorContains(t, proxies(empty), "the empty prefix already takes every name")
}

func TestProxyAuth(t *testing.T) {
	proxy := func(a cmd.ProxyAuthConfig) error {
		_, _, _, err := Proxies(cmd.RegistryConfig{Proxies: []cmd.ProxyConfig{{
			Prefix: "docker.io", Upstream: "https://registry-1.docker.io", Auth: a,
		}}}, flob.NewMemStores(), nil)
		return err
	}

	require.NoError(t, proxy(cmd.ProxyAuthConfig{}))
	require.NoError(t, proxy(cmd.ProxyAuthConfig{Kind: "password", Username: "someone", Password: "dckr_pat_x"}))
	require.NoError(t, proxy(cmd.ProxyAuthConfig{Kind: "password", Username: "someone", Password: "${file:/run/credentials/dockerhub}"}))
	require.NoError(t, proxy(cmd.ProxyAuthConfig{Kind: "bearer", Token: "${file:/run/robot/token}"}))

	for a, want := range map[cmd.ProxyAuthConfig]string{
		{Username: "someone", Password: "x"}:                               "registry.proxies[0].auth.kind: not set",
		{Kind: "basic", Username: "someone", Password: "x"}:                `auth.kind: "basic" is not password or bearer`,
		{Kind: "password", Password: "x"}:                                  "auth.username: not set",
		{Kind: "password", Username: "someone"}:                            "auth.password: not set",
		{Kind: "password", Username: "someone", Password: "x", Token: "y"}: "auth.token: is bearer's",
		{Kind: "bearer"}: "auth.token: not set",
		{Kind: "bearer", Username: "someone", Token: "y"}:  "are password's",
		{Kind: "bearer", Token: "${file:}"}:                "want ${file:/path}",
		{Kind: "bearer", Token: "${file:/run/robot/token"}: "want ${file:/path}",
	} {
		require.ErrorContains(t, proxy(a), want, a)
	}
}

// The fields `auth` replaced are refused, not read as an anonymous proxy that
// then fails every pull with a 401.
func TestProxyAuthMoved(t *testing.T) {
	proxy := func(pc cmd.ProxyConfig) error {
		pc.Prefix, pc.Upstream = "docker.io", "https://registry-1.docker.io"
		_, _, _, err := Proxies(cmd.RegistryConfig{Proxies: []cmd.ProxyConfig{pc}}, flob.NewMemStores(), nil)
		return err
	}
	require.ErrorContains(t, proxy(cmd.ProxyConfig{Username: "someone", Password: "x"}), "registry.proxies[0].username, password: moved to auth")
	require.ErrorContains(t, proxy(cmd.ProxyConfig{TokenFile: "/run/robot/token"}), "registry.proxies[0].token_file: moved to auth")
}

func TestSecret(t *testing.T) {
	s, err := secret("$not{file:a}")
	require.NoError(t, err)
	require.Equal(t, blob.Literal("$not{file:a}"), s)

	path := filepath.Join(t.TempDir(), "pw")
	require.NoError(t, os.WriteFile(path, []byte("hunter2\n"), 0o600))
	s, err = secret("${file:" + path + "}")
	require.NoError(t, err)
	v, err := s.Value()
	require.NoError(t, err)
	require.Equal(t, "hunter2", v)
}
