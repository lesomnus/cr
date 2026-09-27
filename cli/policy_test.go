package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/cmd"
)

const policy = `
providers:
  github:
    kind: oidc
    issuer: https://token.actions.githubusercontent.com
    audience: cr
    exchange: 1h
permissions:
  library-read:
    repos: ["library/**", "!library/busybox"]
    actions: [pull]
  cr-release:
    repos: ["lesomnus/cr", "lesomnus/cr/**"]
    actions: [pull, push, tag]
matches:
  public:
    for: anyone
    grant: [library-read]
  cr-release:
    for: github
    grant: [cr-release]
    when:
      repository_id: "123456789"
      workflow_ref: lesomnus/cr/.github/workflows/release.yml@refs/heads/main
`

func write(t *testing.T, path, content string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestPolicyPath(t *testing.T) {
	for _, c := range []struct {
		from, policy string
		want         string
		named        bool
	}{
		{"/etc/cr/cr.yaml", "", "/etc/cr/cr.auth.yaml", false},
		{"cr.yaml", "", "cr.auth.yaml", false},
		{"/etc/cr/cr.yaml", "policy/auth.yaml", "/etc/cr/policy/auth.yaml", true},
		{"/etc/cr/cr.yaml", "/srv/auth.yaml", "/srv/auth.yaml", true},
		{"", "auth.yaml", "auth.yaml", true},
		{"", "", "", false},
	} {
		path, named := policyPath(&cmd.Config{From: c.from, Auth: cmd.AuthConfig{Policy: c.policy}})
		require.Equal(t, c.want, path, "%+v", c)
		require.Equal(t, c.named, named, "%+v", c)
	}
}

func TestPolicySource(t *testing.T) {
	dir := t.TempDir()
	from := write(t, filepath.Join(dir, "cr.yaml"), "registry: {}\n")
	config := func(a cmd.AuthConfig) *cmd.Config { return &cmd.Config{From: from, Auth: a} }

	// No file by default: open, unless it may not be.
	f, err := policySource(config(cmd.AuthConfig{}))
	require.NoError(t, err)
	require.Nil(t, f)
	_, err = policySource(config(cmd.AuthConfig{Enabled: true}))
	require.ErrorContains(t, err, "cr.auth.yaml is not there")
	_, err = policySource(&cmd.Config{Auth: cmd.AuthConfig{Enabled: true}})
	require.ErrorContains(t, err, "no policy file")

	// A file that was named must be there.
	_, err = policySource(config(cmd.AuthConfig{Policy: "elsewhere.yaml"}))
	require.ErrorContains(t, err, "elsewhere.yaml is not there")

	write(t, filepath.Join(dir, "cr.auth.yaml"), policy)
	f, err = policySource(config(cmd.AuthConfig{}))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "cr.auth.yaml"), f.path)

	// What was once written under auth: is refused rather than ignored, which
	// would leave the registry open.
	write(t, from, "auth:\n  bindings: []\n  exchange:\n    ttl: 1h\n")
	_, err = policySource(config(cmd.AuthConfig{}))
	require.ErrorContains(t, err, "auth.exchange, auth.bindings: no longer read from here")
}

func TestPolicyFileReloads(t *testing.T) {
	ctx := context.Background()
	path := write(t, filepath.Join(t.TempDir(), "cr.auth.yaml"), policy)
	f := newPolicyFile(path)
	st := auth.NewPolicyStore(0, f)
	anon := auth.Subject{ID: auth.Anonymous}
	pull := []auth.Action{auth.ActionPull}

	require.NoError(t, st.Refresh(ctx))
	rev := st.Revision()
	require.Len(t, rev, 12)
	require.Equal(t, pull, st.Current().Allow(anon, "library/ubuntu", pull))
	github := f.rules.Providers[0].Authenticator

	// The same content is the same policy, providers and all.
	require.NoError(t, st.Refresh(ctx))
	require.Equal(t, rev, st.Revision())

	// A change is in force on the next read, and a provider configured as it
	// was is the one there was, with the keys it had fetched.
	write(t, path, policy+"  more:\n    for: anyone\n    grant: [cr-release]\n")
	require.NoError(t, st.Refresh(ctx))
	require.NotEqual(t, rev, st.Revision())
	require.Equal(t, pull, st.Current().Allow(anon, "lesomnus/cr", pull))
	require.Same(t, github, f.rules.Providers[0].Authenticator)
	rev = st.Revision()

	// A file that does not parse, does not check, or is gone keeps the
	// policy in force.
	for _, bad := range []func(){
		func() { write(t, path, "matches: [") },
		func() { write(t, path, "matches:\n  x:\n    for: github\n    grant: [cr-release]\n") },
		func() { require.NoError(t, os.Remove(path)) },
	} {
		bad()
		require.Error(t, st.Refresh(ctx))
		require.Equal(t, rev, st.Revision())
		require.Equal(t, pull, st.Current().Allow(anon, "lesomnus/cr", pull))
	}
}

func TestReadPolicyRefuses(t *testing.T) {
	github := "  github:\n    kind: oidc\n    issuer: https://token.actions.githubusercontent.com\n    audience: cr\n"
	for name, c := range map[string]struct {
		file string
		want string
	}{
		"a field nothing reads":             {"permissions:\n  x:\n    repo: [a]\n    actions: [pull]\n", `unknown field "repo"`},
		"a provider of no kind":             {"providers:\n  github:\n    issuer: x\n    audience: cr\n", "no kind"},
		"a provider of a kind there is not": {"providers:\n  github:\n    kind: saml\n", `kind "saml"`},
		"a provider with no audience":       {"providers:\n  github:\n    kind: oidc\n    issuer: x\n", "audience"},
		"two providers with one issuer":     {"providers:\n" + github + "  also:\n    kind: oidc\n    issuer: https://token.actions.githubusercontent.com\n    audience: other\n", "is also's as well"},
		"a negative exchange":               {"providers:\n  github:\n    kind: oidc\n    issuer: x\n    audience: cr\n    exchange: -1h\n", "negative"},
		"a match for a provider there is not": {
			"providers:\n" + github + "permissions:\n  p:\n    repos: [a]\n    actions: [pull]\nmatches:\n  m:\n    for: gitlab\n    grant: [p]\n    when: {sub: x}\n",
			`for "gitlab": no such provider`,
		},
		"a tag rule that names groups": {"tag_rules:\n  - repo: '**'\n    tag: latest\n    kind: protected\n    groups: [release]\n", "there are no groups"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readPolicy(write(t, filepath.Join(t.TempDir(), "p.yaml"), c.file))
			require.ErrorContains(t, err, c.want)
		})
	}
}

func TestCases(t *testing.T) {
	dir := t.TempDir()
	p, err := readPolicy(write(t, filepath.Join(dir, "cr.auth.yaml"), policy))
	require.NoError(t, err)

	cases, err := readCases(write(t, filepath.Join(dir, "ok.yaml"), `
cases:
  - name: the release workflow pushes cr
    as:
      provider: github
      claims:
        repository_id: "123456789"
        workflow_ref: lesomnus/cr/.github/workflows/release.yml@refs/heads/main
    repo: lesomnus/cr/cache
    allow: [pull, push, tag]
    deny: [delete]
  - name: a number is the claim as it is written
    as:
      provider: github
      claims:
        repository_id: 123456789
        workflow_ref: lesomnus/cr/.github/workflows/release.yml@refs/heads/main
    repo: lesomnus/cr
    allow: [push]
  - name: busybox is not public
    as: anyone
    repo: library/busybox
    deny: [pull]
  - name: nobody lists the registry
    as: anyone
    registry: true
    deny: [catalog]
`), p)
	require.NoError(t, err)
	for _, tc := range cases {
		require.Empty(t, tc.run(p), tc.Name)
	}

	wrong, err := readCases(write(t, filepath.Join(dir, "wrong.yaml"), `
cases:
  - name: busybox is public
    as: anyone
    repo: library/busybox
    allow: [pull]
    deny: [push]
`), p)
	require.NoError(t, err)
	require.Equal(t, []string{"pull: denied, and it should be allowed"}, wrong[0].run(p))

	for name, c := range map[string]struct {
		file string
		want string
	}{
		"no such provider":       {"cases:\n  - as: {provider: gitlab}\n    repo: a\n    allow: [pull]\n", `no provider "gitlab"`},
		"as neither":             {"cases:\n  - as: somebody\n    repo: a\n    allow: [pull]\n", "want anyone"},
		"no place":               {"cases:\n  - as: anyone\n    allow: [pull]\n", "one of `repo` and `registry: true`"},
		"both places":            {"cases:\n  - as: anyone\n    repo: a\n    registry: true\n    allow: [catalog]\n", "one of `repo` and `registry: true`"},
		"no such action":         {"cases:\n  - as: anyone\n    repo: a\n    allow: [pul]\n", `no action "pul"`},
		"a list of a repository": {"cases:\n  - as: anyone\n    repo: a\n    allow: [catalog]\n", "write `registry: true`"},
		"nothing to check":       {"cases:\n  - as: anyone\n    repo: a\n", "nothing to allow or deny"},
		"no cases":               {"cases: []\n", "no cases"},
		"a field nothing reads":  {"cases:\n  - as: anyone\n    repo: a\n    allowed: [pull]\n", `unknown field "allowed"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readCases(write(t, filepath.Join(t.TempDir(), "t.yaml"), c.file), p)
			require.ErrorContains(t, err, c.want)
		})
	}
}
