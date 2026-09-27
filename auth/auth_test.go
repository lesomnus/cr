package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGlob(t *testing.T) {
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"acme/app", "acme/app", true},
		{"acme/app", "acme/apps", false},

		// A star stays within its segment.
		{"acme/*", "acme/app", true},
		{"acme/*", "acme/team/app", false},
		{"acme/*", "acme", false},
		{"acme/*", "acmeapp", false},
		{"*", "", true},
		{"*", "acme/app", false},
		{"v*", "v1.2.3", true},
		{"*-dev", "app-dev", true},
		{"*-dev", "app-devx", false},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "aXbY", false},
		{"a*a", "a", false},
		{"a*a", "aa", true},
		{"refs/heads/*", "refs/heads/feature/x", false},

		// Two stars are whole segments: none of them in the middle or at the
		// start, and at least one at the end.
		{"acme/**", "acme/app", true},
		{"acme/**", "acme/team/app", true},
		{"acme/**", "acme", false},
		{"**", "acme/team/app", true},
		{"**/app", "app", true},
		{"**/app", "acme/team/app", true},
		{"acme/**/app", "acme/app", true},
		{"acme/**/app", "acme/a/b/app", true},
		{"acme/**/app", "acme/a/b/apps", false},
		{"acme/**/**/app", "acme/app", true},

		// Anything else is itself.
		{"v?", "v?", true},
		{"v?", "v1", false},
		{`v\*`, "v*", true},
		{`v\*`, "v1", false},
		{"acme/app/.github/workflows/*@refs/heads/main", "acme/app/.github/workflows/release.yml@refs/heads/main", true},
	} {
		g, err := ParseGlob(c.pattern)
		require.NoError(t, err, c.pattern)
		require.Equal(t, c.want, g.Match(c.s), "%q %q", c.pattern, c.s)
	}

	for _, bad := range []string{"acme/**app", "a**", `trailing\`} {
		_, err := ParseGlob(bad)
		require.Error(t, err, bad)
	}
}

func TestParseScope(t *testing.T) {
	s, err := ParseScope("repository:acme/app:pull,push")
	require.NoError(t, err)
	require.Equal(t, Scope{Type: TypeRepository, Name: "acme/app", Actions: []string{"pull", "push"}}, s)

	s, err = ParseScope("repository:localhost:5000/foo:pull")
	require.NoError(t, err)
	require.Equal(t, "localhost:5000/foo", s.Name)

	s, err = ParseScope("repository(plugin):foo:pull")
	require.NoError(t, err)
	require.Equal(t, TypeRepository, s.Type)

	ss, err := ParseScopes([]string{"registry:catalog:* repository:a:pull"})
	require.NoError(t, err)
	require.Len(t, ss, 2)

	_, err = ParseScope("nonsense")
	require.Error(t, err)
}

// users is an authenticator for the tests: a password is the subject it
// names.
type users map[string]Subject

func (u users) Authenticate(ctx context.Context, username, password string) (Subject, error) {
	s, ok := u[password]
	if !ok {
		return Subject{}, ErrNotMine
	}
	return s, nil
}

// job is a GitHub job of acme/app, running workflow.
func job(workflow string) Subject {
	return Subject{ID: "github:repo:acme/app:ref:refs/heads/main", Provider: "github", Claims: map[string]any{
		"repository_id": "42",
		"workflow_ref":  "acme/app/.github/workflows/" + workflow + "@refs/heads/main",
		"groups":        []any{"ci", "release"},
	}}
}

func rules() Rules {
	return Rules{
		Permissions: map[string]Permission{
			"library": {Repos: []string{"library/**", "!library/busybox"}, Actions: []Action{ActionPull}},
			"catalog": {Repos: []string{"**"}, Actions: []Action{ActionCatalog}},
			"app":     {Repos: []string{"acme/app", "acme/app/**"}, Actions: []Action{ActionPull, ActionPush, ActionTag}},
			"read":    {Repos: []string{"acme/*"}, Actions: []Action{ActionPull}},
			"admin":   {Repos: []string{"**"}, Actions: []Action{ActionAll}},
			"back":    {Repos: []string{"x/**", "!x/private/**", "x/private/shared"}, Actions: []Action{ActionPull}},
		},
		Matches: map[string]Match{
			"public": {For: Anyone, Grant: []string{"library", "catalog", "back"}},
			"release": {For: "github", Grant: []string{"app"}, When: map[string]string{
				"repository_id": "42",
				"workflow_ref":  "acme/app/.github/workflows/release.yml@refs/heads/main",
			}},
			"acme": {For: "github", Grant: []string{"read"}, When: map[string]string{"repository_id": "42"}},
			"ops":  {For: "github", Grant: []string{"admin"}, When: map[string]string{"groups": "ops"}},
		},
		TagRules: []TagRule{
			{Name: "releases", Repo: "acme/**", Tag: "v*", Kind: TagImmutable},
			{Name: "latest", Repo: "acme/**", Tag: "latest", Kind: TagProtected},
			{Name: "semver", Repo: "strict/*", Tag: "*", Kind: TagPattern, Pattern: `v\d+\.\d+\.\d+`},
		},
	}
}

func policy(t *testing.T) *Policy {
	p, err := NewPolicy(rules())
	require.NoError(t, err)
	return p
}

func TestAllow(t *testing.T) {
	p := policy(t)
	anon := Subject{ID: Anonymous}
	release, sibling := job("release.yml"), job("test.yml")
	all := []Action{ActionPull, ActionPush, ActionTag, ActionDelete}

	require.Equal(t, []Action{ActionPull}, p.Allow(anon, "library/ubuntu", all))
	require.Equal(t, []Action{ActionPull}, p.Allow(anon, "library/team/tool", all))
	require.Empty(t, p.Allow(anon, "library", all), "library/** is what is under library")
	require.Empty(t, p.Allow(anon, "library/busybox", all), "taken back by a later pattern")
	require.Empty(t, p.Allow(anon, "acme/app", all))

	// The last pattern that matches decides.
	require.Equal(t, []Action{ActionPull}, p.Allow(anon, "x/a", all))
	require.Empty(t, p.Allow(anon, "x/private/a", all))
	require.Equal(t, []Action{ActionPull}, p.Allow(anon, "x/private/shared", all))

	// Everybody is under anyone too.
	require.Equal(t, []Action{ActionPull}, p.Allow(release, "library/ubuntu", all))

	// A match with `when` holds only for the credential whose claims match,
	// and matches add: the release workflow is under both of acme/app's.
	require.Equal(t, []Action{ActionPull, ActionPush, ActionTag}, p.Allow(release, "acme/app", all))
	require.Equal(t, []Action{ActionPull, ActionPush, ActionTag}, p.Allow(release, "acme/app/cache", all))
	require.Equal(t, []Action{ActionPull}, p.Allow(sibling, "acme/app", all))
	require.Equal(t, []Action{ActionPull}, p.Allow(release, "acme/web", all))
	require.Empty(t, p.Allow(release, "acme/web/cache", all))

	// Only the provider a match is for.
	gitlab := release
	gitlab.Provider = "gitlab"
	require.Empty(t, p.Allow(gitlab, "acme/app", all))

	// A list claim holds when any of its values does.
	ops := job("ops.yml")
	ops.Claims["groups"] = []any{"ci", "ops"}
	require.Equal(t, all, p.Allow(ops, "anything/at/all", all))

	// Only a permission over `**` alone reaches the registry.
	require.Equal(t, []Action{ActionCatalog}, p.AllowRegistry(anon, []Action{ActionCatalog, ActionSearch}))
	require.Equal(t, []Action{ActionAdmin}, p.AllowRegistry(ops, []Action{ActionAdmin}))
	require.Empty(t, p.AllowRegistry(release, []Action{ActionAdmin}))

}

func TestPolicyRefusesWhatDoesNotCheck(t *testing.T) {
	anon := Subject{ID: Anonymous}
	for name, c := range map[string]struct {
		edit func(*Rules)
		want string
	}{
		"a match for a provider with no when": {
			func(r *Rules) { r.Matches["m"] = Match{For: "github", Grant: []string{"library"}} },
			"no `when`",
		},
		"a match for anyone with when": {
			func(r *Rules) {
				r.Matches["m"] = Match{For: Anyone, Grant: []string{"library"}, When: map[string]string{"sub": "x"}}
			},
			"`when` for anyone",
		},
		"a grant of nothing there is": {
			func(r *Rules) { r.Matches["m"] = Match{For: Anyone, Grant: []string{"nope"}} },
			`no permission "nope"`,
		},
		"a match that grants nothing": {
			func(r *Rules) { r.Matches["m"] = Match{For: Anyone} },
			"grants nothing",
		},
		"an action there is not": {
			func(r *Rules) {
				r.Permissions["library"] = Permission{Repos: []string{"library/**"}, Actions: []Action{"pul"}}
			},
			`no action "pul"`,
		},
		"a glob that does not parse": {
			func(r *Rules) {
				r.Permissions["library"] = Permission{Repos: []string{"library**"}, Actions: []Action{ActionPull}}
			},
			"must be a segment of its own",
		},
		"only patterns that take away": {
			func(r *Rules) {
				r.Permissions["library"] = Permission{Repos: []string{"!library/busybox"}, Actions: []Action{ActionPull}}
			},
			"none gives",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := rules()
			c.edit(&r)
			p, err := NewPolicy(r)
			require.ErrorContains(t, err, c.want)

			// What does not check grants nothing, and nor does a match that
			// grants it, but the rest stands.
			require.Empty(t, p.Allow(anon, "library/busybox", []Action{ActionPull}))
			require.Equal(t, []Action{ActionPull}, p.Allow(job("test.yml"), "acme/web", []Action{ActionPull}))
		})
	}
}

func TestCheckTag(t *testing.T) {
	p := policy(t)

	require.NoError(t, p.CheckTag(nil, "acme/app", "v1.0.0", TagCreate))
	var e *TagRuleError
	require.ErrorAs(t, p.CheckTag(nil, "acme/app", "v1.0.0", TagMove), &e)
	require.Equal(t, TagImmutable, e.Kind)
	require.Error(t, p.CheckTag([]Action{ActionAdmin}, "acme/app", "v1.0.0", TagDelete))
	require.NoError(t, p.CheckTag(nil, "other/app", "v1.0.0", TagMove))
	require.Error(t, p.CheckTag(nil, "acme/team/app", "v1.0.0", TagMove))

	require.NoError(t, p.CheckTag(nil, "acme/app", "latest", TagCreate))
	require.Error(t, p.CheckTag(nil, "acme/app", "latest", TagMove))
	require.NoError(t, p.CheckTag([]Action{ActionAdmin}, "acme/app", "latest", TagMove))

	require.NoError(t, p.CheckTag(nil, "strict/app", "v1.2.3", TagCreate))
	require.Error(t, p.CheckTag(nil, "strict/app", "latest", TagCreate))
	require.Error(t, p.CheckTag(nil, "strict/app", "v1.2.3-rc", TagMove))
	require.NoError(t, p.CheckTag(nil, "strict/app", "whatever", TagDelete))

	// A rule that cannot be evaluated refuses.
	bad, err := NewPolicy(Rules{TagRules: []TagRule{{Repo: "**", Tag: "*", Kind: TagPattern, Pattern: "("}}})
	require.Error(t, err)
	require.Error(t, bad.CheckTag(nil, "any", "thing", TagCreate))
	bad, err = NewPolicy(Rules{TagRules: []TagRule{
		{Repo: "a**", Tag: "*", Kind: TagImmutable},
		{Repo: "a**", Tag: "*", Kind: TagRetention},
	}})
	require.Error(t, err)
	require.Error(t, bad.CheckTag(nil, "any", "thing", TagMove))
	require.Empty(t, bad.Retention("any"), "a retention rule that cannot be evaluated deletes nothing")
}

func TestPolicyStoreKeepsWhatItHadOnFailure(t *testing.T) {
	ctx := context.Background()
	calls := 0
	src := sourceFunc(func(context.Context) (Rules, error) {
		calls++
		if calls > 1 {
			return Rules{}, context.DeadlineExceeded
		}
		return Rules{
			Permissions: map[string]Permission{"all": {Repos: []string{"**"}, Actions: []Action{ActionPull}}},
			Matches:     map[string]Match{"all": {For: Anyone, Grant: []string{"all"}}},
		}, nil
	})
	st := NewPolicyStore(time.Hour, src)
	require.Empty(t, st.Current().Allow(Subject{}, "x", []Action{ActionPull}))
	require.NoError(t, st.Refresh(ctx))
	require.Len(t, st.Current().Allow(Subject{}, "x", []Action{ActionPull}), 1)
	require.Error(t, st.Refresh(ctx))
	require.Len(t, st.Current().Allow(Subject{}, "x", []Action{ActionPull}), 1)
}

type sourceFunc func(context.Context) (Rules, error)

func (f sourceFunc) Load(ctx context.Context) (Rules, error) { return f(ctx) }

func issuer(t *testing.T) *Issuer {
	k, err := GenerateKey()
	require.NoError(t, err)
	i, err := NewIssuer("cr", "registry.test", time.Minute, k)
	require.NoError(t, err)
	return i
}

func TestIssuer(t *testing.T) {
	i := issuer(t)
	s := job("release.yml")
	token, _, err := i.Issue(s, []Access{{Type: TypeRepository, Name: "acme/app", Actions: []string{"pull"}}})
	require.NoError(t, err)

	c, err := i.Verify(token)
	require.NoError(t, err)
	require.Equal(t, s.ID, c.Subject)
	require.Equal(t, s, c.Who())
	require.Equal(t, "acme/app", c.Access[0].Name)

	// Another issuer's key, another service, a later time.
	_, err = issuer(t).Verify(token)
	require.Error(t, err)

	k, _ := GenerateKey()
	other, err := NewIssuer("cr", "elsewhere", time.Minute, k)
	require.NoError(t, err)
	t2, _, _ := other.Issue(Subject{ID: "alice"}, nil)
	_, err = i.Verify(t2)
	require.Error(t, err)

	i.now = func() time.Time { return time.Now().Add(time.Hour) }
	_, err = i.Verify(token)
	require.Error(t, err)

	set := issuer(t).JWKS()
	require.Len(t, set.Keys, 1)
	require.NotEmpty(t, set.Keys[0].KeyID)
	require.True(t, set.Keys[0].IsPublic())
}

func TestServeToken(t *testing.T) {
	st := NewPolicyStore(time.Hour, Static(rules()))
	require.NoError(t, st.Refresh(context.Background()))
	g := &Guard{Authenticator: Chain{users{"release": job("release.yml")}}, Policy: st, Issuer: issuer(t)}

	get := func(q string, user, pass string) (int, *Claims) {
		req := httptest.NewRequest("GET", "/token?service=registry.test&"+q, nil)
		if user != "" || pass != "" {
			req.SetBasicAuth(user, pass)
		}
		w := httptest.NewRecorder()
		g.ServeToken(w, req)
		if w.Code != http.StatusOK {
			return w.Code, nil
		}
		var v tokenResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v))
		require.Equal(t, v.Token, v.AccessToken)
		c, err := g.Issuer.Verify(v.Token)
		require.NoError(t, err)
		return w.Code, c
	}

	code, c := get("scope="+url.QueryEscape("repository:acme/app:pull,push"), "ci", "release")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, job("release.yml").ID, c.Subject)
	require.Equal(t, []string{"pull", "push", "tag"}, c.Access[0].Actions)

	code, c = get("scope="+url.QueryEscape("repository:acme/app:pull,push")+"&scope="+url.QueryEscape("repository:library/ubuntu:pull"), "", "")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, Anonymous, c.Subject)
	require.Empty(t, c.Access[0].Actions)
	require.Equal(t, []string{"pull"}, c.Access[1].Actions)

	code, _ = get("", "ci", "wrong")
	require.Equal(t, http.StatusUnauthorized, code)

	form := url.Values{"grant_type": {"password"}, "username": {"x"}, "password": {"release"}, "scope": {"repository:acme/app:*"}}
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	g.ServeToken(w, req)
	require.Equal(t, http.StatusOK, w.Code)
}

// A caller that came with a token is the subject it was issued to, provider
// and claims and all, for what a token's access cannot say: which
// repositories a list shows.
func TestTokenCarriesTheSubject(t *testing.T) {
	is := issuer(t)
	st := NewPolicyStore(time.Hour, Static(rules()))
	require.NoError(t, st.Refresh(context.Background()))
	g := &Guard{Policy: st, Issuer: is}

	s := job("release.yml")
	login, _, err := is.IssueLogin(s, time.Hour)
	require.NoError(t, err)
	back, err := is.VerifyLogin(login)
	require.NoError(t, err)
	require.Equal(t, s, back)

	access, _, err := is.Issue(s, nil)
	require.NoError(t, err)
	req := httptest.NewRequest("GET", "/v2/", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	c, err := g.Caller(req)
	require.NoError(t, err)
	require.Equal(t, s, c.Subject)
	require.True(t, c.CanPull("acme/web"))
	require.False(t, c.CanPull("other/web"))
}
