package cli

import (
	"testing"

	"github.com/lesomnus/flob"
	"github.com/stretchr/testify/require"

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
