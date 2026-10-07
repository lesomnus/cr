package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/xli/cfg"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/cr/registry"
	"github.com/lesomnus/cr/trust"
	"github.com/lesomnus/cr/trust/trusttest"
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
	// A credential as the configuration gives one.
	sec := func(v string) cmd.Secret {
		var s cmd.Secret
		require.NoError(t, s.UnmarshalText([]byte(v)))
		return s
	}

	require.NoError(t, proxy(cmd.ProxyAuthConfig{}))
	require.NoError(t, proxy(cmd.ProxyAuthConfig{Kind: "password", Username: "someone", Password: sec("dckr_pat_x")}))
	require.NoError(t, proxy(cmd.ProxyAuthConfig{Kind: "password", Username: "someone", Password: sec("${file:/run/credentials/dockerhub}")}))
	require.NoError(t, proxy(cmd.ProxyAuthConfig{Kind: "bearer", Token: sec("${file:/run/robot/token}")}))

	for _, tc := range []struct {
		a    cmd.ProxyAuthConfig
		want string
	}{
		{cmd.ProxyAuthConfig{Username: "someone", Password: sec("x")}, "registry.proxies[0].auth.kind: not set"},
		{cmd.ProxyAuthConfig{Kind: "basic", Username: "someone", Password: sec("x")}, `auth.kind: "basic" is not password or bearer`},
		{cmd.ProxyAuthConfig{Kind: "password", Password: sec("x")}, "auth.username: not set"},
		{cmd.ProxyAuthConfig{Kind: "password", Username: "someone"}, "auth.password: not set"},
		{cmd.ProxyAuthConfig{Kind: "password", Username: "someone", Password: sec("x"), Token: sec("y")}, "auth.token: is bearer's"},
		{cmd.ProxyAuthConfig{Kind: "bearer"}, "auth.token: not set"},
		{cmd.ProxyAuthConfig{Kind: "bearer", Username: "someone", Token: sec("y")}, "are password's"},
	} {
		require.ErrorContains(t, proxy(tc.a), tc.want)
	}
}

// A reference that is not one is refused where the configuration is read,
// before anything is built from it.
func TestProxyAuthReference(t *testing.T) {
	for ref, want := range map[string]string{
		"${file:}":                "names no file",
		"${file:/run/robot/token": "a secret is a literal or exactly one reference",
		"file:/run/robot/token":   "a reference is written ${file:/run/robot/token}",
	} {
		p := filepath.Join(t.TempDir(), "cr.yaml")
		require.NoError(t, os.WriteFile(p, []byte(`
registry:
  proxies:
    - prefix: docker.io
      upstream: https://registry-1.docker.io
      auth: {kind: bearer, token: "`+ref+`"}
`), 0o600))

		var c cmd.Config
		_, err := cfg.New(cmd.Name, &c, cfg.WithPaths()).Read(p, nil)
		require.ErrorContains(t, err, want, ref)
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

func TestS3Credentials(t *testing.T) {
	c, err := s3Credentials(cmd.S3StorageConfig{AccessKeyId: "AKIA1", SecretAccessKey: "s1"})
	require.NoError(t, err)
	require.Equal(t, flob.Credentials{AccessKeyID: "AKIA1", SecretAccessKey: "s1"}, c)

	path := filepath.Join(t.TempDir(), "credentials.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"AccessKeyId": "AKIA2", "SecretAccessKey": "s2"}`), 0o600))
	set := s3Set(t, "${file:"+path+"}")
	p, err := s3Credentials(cmd.S3StorageConfig{Credentials: set})
	require.NoError(t, err)
	got, err := p.Retrieve(t.Context())
	require.NoError(t, err)
	require.Equal(t, "AKIA2", got.AccessKeyID)

	_, err = s3Credentials(cmd.S3StorageConfig{Credentials: set, SessionToken: "t"})
	require.ErrorContains(t, err, "session_token: credentials is the whole set")

	// One file per key would be read apart; the set goes in one file.
	_, err = s3Credentials(cmd.S3StorageConfig{AccessKeyId: "AKIA1", SecretAccessKey: "${file:/run/aws/secret}"})
	require.ErrorContains(t, err, "secret_access_key: ${file:...} is not read here")
}

func TestProxyVerify(t *testing.T) {
	proxy := func(v cmd.ProxyVerifyConfig) (*registry.Proxy, error) {
		_, ps, _, err := Proxies(cmd.RegistryConfig{Proxies: []cmd.ProxyConfig{{
			Prefix: "dist", Upstream: "https://registry.example.com", Verify: v,
		}}}, flob.NewMemStores(), nil)
		if err != nil {
			return nil, err
		}
		return ps[0], nil
	}
	root := trusttest.NewCA(t, "Kamino").RootFile(t)

	p, err := proxy(cmd.ProxyVerifyConfig{})
	require.NoError(t, err)
	require.Nil(t, p.Verify, "a proxy that says nothing verifies nothing")

	p, err = proxy(cmd.ProxyVerifyConfig{Roots: []string{root}, Identities: []string{trusttest.SubjectDN}})
	require.NoError(t, err)
	require.Equal(t, trust.Require, p.Verify.Mode())

	p, err = proxy(cmd.ProxyVerifyConfig{Mode: "audit", Roots: []string{root}, Identities: []string{"*"}})
	require.NoError(t, err)
	require.Equal(t, trust.Audit, p.Verify.Mode())

	// Saying anything at all is asking for verification, so a mode alone is
	// not a cache that quietly verifies nothing.
	_, err = proxy(cmd.ProxyVerifyConfig{Mode: "require"})
	require.ErrorContains(t, err, "registry.proxies[0].verify.roots: none")
	_, err = proxy(cmd.ProxyVerifyConfig{Roots: []string{root}})
	require.ErrorContains(t, err, "registry.proxies[0].verify.identities: none")
}
