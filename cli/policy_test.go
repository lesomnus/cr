package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lesomnus/xli/cfg"
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

	// A key under auth: that nothing reads is refused rather than ignored,
	// wherever it is -- by the loader now, for the whole file, where this
	// used to read `auth:` again on its own.
	load := func(body string) error {
		write(t, from, body)
		var c cmd.Config
		_, err := cfg.New(cmd.Name, &c, cfg.WithPaths()).Read(from, nil)
		return err
	}
	require.ErrorContains(t, load("auth:\n  enabled: true\n  bindings: []\n"), "auth.bindings: nothing reads this key")
	require.ErrorContains(t, load("auth:\n  token:\n    keyz: [a.pem]\n"), `auth.token.keyz: nothing reads this key (did you mean "keys"?)`)
	require.NoError(t, load("auth:\n  refresh: 1s\n  token:\n    ttl: 5m\n"))
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

	// A change is in force once it has read the same twice running: the first
	// read may have caught the file half written, and a policy cut short where
	// a rule was is a policy that says something else. A provider configured
	// as it was is the one there was, with the keys it had fetched.
	write(t, path, policy+"  more:\n    for: anyone\n    grant: [cr-release]\n")
	require.NoError(t, st.Refresh(ctx))
	require.Equal(t, rev, st.Revision())
	require.Empty(t, st.Current().Allow(anon, "lesomnus/cr", pull))
	require.NoError(t, st.Refresh(ctx))
	require.NotEqual(t, rev, st.Revision())
	require.Equal(t, pull, st.Current().Allow(anon, "lesomnus/cr", pull))
	require.Same(t, github, f.rules.Providers[0].Authenticator)
	rev = st.Revision()

	// A file that does not parse, or does not check, keeps the policy in
	// force, and says why from the read that would have taken it up; one that
	// is gone says so at once.
	for _, bad := range []string{
		"matches: [",
		"matches:\n  x:\n    for: github\n    grant: [cr-release]\n",
	} {
		write(t, path, bad)
		require.NoError(t, st.Refresh(ctx))
		require.Error(t, st.Refresh(ctx))
		require.Error(t, st.Refresh(ctx), "and at every read after")
		require.Equal(t, rev, st.Revision())
		require.Equal(t, pull, st.Current().Allow(anon, "lesomnus/cr", pull))
	}
	require.NoError(t, os.Remove(path))
	require.Error(t, st.Refresh(ctx))
	require.Equal(t, rev, st.Revision())

	// And put back as it was, it is the policy in force without a reload.
	write(t, path, policy+"  more:\n    for: anyone\n    grant: [cr-release]\n")
	require.NoError(t, st.Refresh(ctx))
	require.Equal(t, rev, st.Revision())
	require.Equal(t, pull, st.Current().Allow(anon, "lesomnus/cr", pull))
}

func TestReadPolicyRefuses(t *testing.T) {
	github := "  github:\n    kind: oidc\n    issuer: https://token.actions.githubusercontent.com\n    audience: cr\n"
	for name, c := range map[string]struct {
		file string
		want string
	}{
		"a field nothing reads":             {"permissions:\n  x:\n    repo: [a]\n    actions: [pull]\n", `permissions.x.repo: nothing reads this key (did you mean "repos"?)`},
		"a provider of no kind":             {"providers:\n  github:\n    issuer: x\n    audience: cr\n", "no kind"},
		"a provider of a kind there is not": {"providers:\n  github:\n    kind: saml\n", `kind "saml"`},
		"a provider with no audience":       {"providers:\n  github:\n    kind: oidc\n    issuer: x\n", "audience"},
		"two providers with one issuer":     {"providers:\n" + github + "  also:\n    kind: oidc\n    issuer: https://token.actions.githubusercontent.com\n    audience: other\n", "is also's as well"},
		"a negative exchange":               {"providers:\n  github:\n    kind: oidc\n    issuer: x\n    audience: cr\n    exchange: -1h\n", "negative"},
		"a match for a provider there is not": {
			"providers:\n" + github + "permissions:\n  p:\n    repos: [a]\n    actions: [pull]\nmatches:\n  m:\n    for: gitlab\n    grant: [p]\n    when: {sub: x}\n",
			`for "gitlab": no such provider`,
		},
		"an mtls provider with an issuer": {"providers:\n  engines:\n    kind: mtls\n    issuer: x\n", "configured by the listener's tls.client_ca_file"},
		"an mtls provider that exchanges": {"providers:\n  engines:\n    kind: mtls\n    exchange: 1h\n", "a certificate is not traded"},
		"two mtls providers":              {"providers:\n  engines:\n    kind: mtls\n  more:\n    kind: mtls\n", "there is one mtls provider or none"},
		"a tag rule that names groups":    {"tag_rules:\n  - repo: '**'\n    tag: latest\n    kind: protected\n    groups: [release]\n", "tag_rules[0].groups: nothing reads this key"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := readPolicy(write(t, filepath.Join(t.TempDir(), "p.yaml"), c.file))
			require.ErrorContains(t, err, c.want)
		})
	}
}

func TestCases(t *testing.T) {
	dir := t.TempDir()
	p, _, err := readPolicy(write(t, filepath.Join(dir, "cr.auth.yaml"), policy))
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

// A registry's policy as it reads with both: the organization said once on
// the provider, and each workflow's repositories beside its claims.
const compact = `
providers:
  github:
    kind: oidc
    issuer: https://token.actions.githubusercontent.com
    audience: cr
    when:
      repository_owner_id: "177305168"
permissions:
  pki:
    repos: [hday/pki]
    actions: [pull, push, tag]
matches:
  everyone:
    for: anyone
    grant:
      - repos: ["**"]
        actions: [pull, catalog]
  bosun-ci:
    for: github
    grant:
      - repos: [bosun]
        actions: [pull, push, tag]
    when:
      repository_id: "1292115356"
      workflow_ref: Holiday-Robot/bosun/.github/workflows/ci.yaml@refs/heads/main
  pki-main:
    for: github
    grant: [pki]
    when:
      workflow_ref: Holiday-Robot/pki/.github/workflows/build.yaml@refs/heads/main
`

func TestPolicyCompact(t *testing.T) {
	p, _, err := readPolicy(write(t, filepath.Join(t.TempDir(), "cr.auth.yaml"), compact))
	require.NoError(t, err)

	all := []auth.Action{auth.ActionPull, auth.ActionPush, auth.ActionTag}
	bosun := auth.Subject{ID: "github:bosun", Provider: "github", Claims: map[string]any{
		"repository_owner_id": "177305168",
		"repository_id":       "1292115356",
		"workflow_ref":        "Holiday-Robot/bosun/.github/workflows/ci.yaml@refs/heads/main",
	}}
	require.Equal(t, all, p.Allow(bosun, "bosun", all))
	require.Equal(t, []auth.Action{auth.ActionPull}, p.Allow(bosun, "hday/pki", all), "only what is written beside its claims")

	// The provider's claim holds for a match that says nothing of it, and
	// one that has no claims of the repository of its own.
	stranger := bosun
	stranger.Claims = map[string]any{"repository_owner_id": "1", "repository_id": "1292115356", "workflow_ref": bosun.Claims["workflow_ref"]}
	require.Equal(t, []auth.Action{auth.ActionPull}, p.Allow(stranger, "bosun", all))
	pki := auth.Subject{ID: "github:pki", Provider: "github", Claims: map[string]any{
		"repository_owner_id": "177305168",
		"workflow_ref":        "Holiday-Robot/pki/.github/workflows/build.yaml@refs/heads/main",
	}}
	require.Equal(t, all, p.Allow(pki, "hday/pki", all))
	pki.Claims["repository_owner_id"] = "1"
	require.Equal(t, []auth.Action{auth.ActionPull}, p.Allow(pki, "hday/pki", all))

	// A permission written in place is named by where it is.
	e := p.Explain(bosun, "bosun")
	var grants []string
	for _, m := range e.Matches {
		for _, g := range m.Grants {
			grants = append(grants, g.Permission)
		}
	}
	require.ElementsMatch(t, []string{"everyone.grant[0]", "bosun-ci.grant[0]", "pki"}, grants)
}

func TestPolicyCompactRefuses(t *testing.T) {
	github := "providers:\n  github:\n    kind: oidc\n    issuer: https://token.actions.githubusercontent.com\n    audience: cr\n"
	for name, c := range map[string]struct {
		file string
		want string
	}{
		"a field nothing reads, in place": {
			"matches:\n  m:\n    for: anyone\n    grant:\n      - repo: [a]\n        actions: [pull]\n",
			`unknown field "repo"`,
		},
		"an action there is not, in place": {
			"matches:\n  m:\n    for: anyone\n    grant:\n      - repos: [a]\n        actions: [pul]\n",
			`permission "m.grant[0]": no action "pul"`,
		},
		"no repos, in place": {
			"matches:\n  m:\n    for: anyone\n    grant:\n      - actions: [pull]\n",
			`permission "m.grant[0]": no repos`,
		},
		"another match's own": {
			"matches:\n  m:\n    for: anyone\n    grant:\n      - repos: [a]\n        actions: [pull]\n  n:\n    for: anyone\n    grant: [\"m.grant[0]\"]\n",
			`match "n": grant[0]: no permission "m.grant[0]"`,
		},
		"a permission called what one in place is": {
			"permissions:\n  m.grant[0]:\n    repos: [b]\n    actions: [pull]\nmatches:\n  m:\n    for: anyone\n    grant:\n      - repos: [a]\n        actions: [pull]\n",
			`a permission is already called "m.grant[0]"`,
		},
		"a match for a provider with no when, either of them": {
			github + "matches:\n  m:\n    for: github\n    grant:\n      - repos: [a]\n        actions: [pull]\n",
			"no `when`, here or on the provider",
		},
		"a match that says otherwise than its provider": {
			strings.Replace(github, "audience: cr\n", "audience: cr\n    when: {repository_owner_id: \"7\"}\n", 1) +
				"matches:\n  m:\n    for: github\n    grant:\n      - repos: [a]\n        actions: [pull]\n    when: {repository_owner_id: \"*\"}\n",
			`where the provider requires "7"`,
		},
		"a provider's claim that does not parse": {
			strings.Replace(github, "audience: cr\n", "audience: cr\n    when: {workflow_ref: \"a**\"}\n", 1),
			`provider "github": when "workflow_ref"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := readPolicy(write(t, filepath.Join(t.TempDir(), "p.yaml"), c.file))
			require.ErrorContains(t, err, c.want)
		})
	}
}

func TestPolicySecretProvider(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pass := filepath.Join(dir, "breakglass")
	path := write(t, filepath.Join(dir, "cr.auth.yaml"), `
providers:
  breakglass:
    kind: secret
    username: admin
    password: ${file:`+pass+`}
matches:
  breakglass:
    for: breakglass
    grant:
      - repos: ["**"]
        actions: ["*"]
`)
	all := []auth.Action{auth.ActionPull, auth.ActionPush, auth.ActionTag, auth.ActionDelete}

	// The file is not there yet: the policy stands, and nobody logs in by it.
	// `cr auth check` says so, as a warning.
	p, warnings, err := readPolicy(path)
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	require.ErrorContains(t, warnings[0], "providers.breakglass.password: secret file "+pass)
	_, err = p.Authenticate(ctx, "admin", "")
	require.ErrorIs(t, err, auth.ErrUnauthenticated)

	write(t, pass, "  b6c1e0f2-the-break-glass  \n")
	admin, err := p.Authenticate(ctx, "admin", "b6c1e0f2-the-break-glass")
	require.NoError(t, err, "the file's content, without the whitespace around it")
	require.Equal(t, "breakglass:admin", admin.ID)
	require.Equal(t, all, p.Allow(admin, "acme/app", all))
	_, err = p.Authenticate(ctx, "admin", "guess")
	require.ErrorIs(t, err, auth.ErrUnauthenticated)

	// Replaced as a Secret is, by a rename: the new one, without a restart
	// and without the policy changing.
	write(t, pass+".new", "rotated\n")
	require.NoError(t, os.Rename(pass+".new", pass))
	_, err = p.Authenticate(ctx, "admin", "b6c1e0f2-the-break-glass")
	require.ErrorIs(t, err, auth.ErrUnauthenticated)
	_, err = p.Authenticate(ctx, "admin", "rotated")
	require.NoError(t, err)

	// What the file is not is the password: the reference is what the
	// policy says.
	_, err = p.Authenticate(ctx, "admin", "${file:"+pass+"}")
	require.ErrorIs(t, err, auth.ErrUnauthenticated)
}

func TestPolicySecretProviderRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		file string
		want string
	}{
		"no username": {
			"providers:\n  b:\n    kind: secret\n    password: ${file:/etc/cr/b}\n",
			"username: not set",
		},
		"no password": {
			"providers:\n  b:\n    kind: secret\n    username: admin\n",
			"password: not set",
		},
		"one that exchanges": {
			"providers:\n  b:\n    kind: secret\n    username: admin\n    password: ${file:/etc/cr/b}\n    exchange: 1h\n",
			"nothing to trade it for",
		},
		"one with an issuer": {
			"providers:\n  b:\n    kind: secret\n    username: admin\n    password: ${file:/etc/cr/b}\n    issuer: https://x\n",
			"are an oidc provider's",
		},
		"a password on an oidc provider": {
			"providers:\n  github:\n    kind: oidc\n    issuer: https://token.actions.githubusercontent.com\n    audience: cr\n    password: ${file:/etc/cr/b}\n",
			"are a secret provider's",
		},
		"a username on an mtls provider": {
			"providers:\n  engines:\n    kind: mtls\n    username: admin\n",
			"are a secret provider's",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := readPolicy(write(t, filepath.Join(t.TempDir(), "p.yaml"), c.file))
			require.ErrorContains(t, err, c.want)
		})
	}
}
