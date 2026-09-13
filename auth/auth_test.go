package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestGlob(t *testing.T) {
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true},
		{"*", "acme/app", true},
		{"acme/*", "acme/app", true},
		{"acme/*", "acme/team/app", true},
		{"acme/*", "acmeapp", false},
		{"acme/app", "acme/app", true},
		{"acme/app", "acme/apps", false},
		{"v*", "v1.2.3", true},
		{"v?", "v1", true},
		{"v?", "v12", false},
		{"*-dev", "app-dev", true},
		{"*-dev", "app-devx", false},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "aXbY", false},
	} {
		require.Equal(t, c.want, Glob(c.pattern, c.s), "%q %q", c.pattern, c.s)
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

func policy(t *testing.T) *Policy {
	p, err := NewPolicy([]Binding{
		{Subject: Anonymous, Repo: "library/*", Actions: []Action{ActionPull}},
		{Subject: Anonymous, Repo: "*", Actions: []Action{ActionCatalog}},
		{Group: "ci", Repo: "acme/*", Actions: []Action{ActionPull, ActionPush}},
		{Subject: "alice", Repo: "acme/app", Actions: []Action{ActionAll}},
		{Group: Authenticated, Repo: "shared/*", Actions: []Action{ActionPull}},
		{Group: "ci", Repo: "gh/app", Actions: []Action{ActionPush, ActionTag}, When: map[string]string{
			"repository":   "acme/app",
			"workflow_ref": "acme/app/.github/workflows/release.yml@refs/heads/*",
		}},
		{Subject: "ops", Repo: "*", Actions: []Action{ActionAdmin}},
	}, []TagRule{
		{Name: "releases", Repo: "acme/*", Tag: "v*", Kind: TagImmutable},
		{Name: "latest", Repo: "acme/*", Tag: "latest", Kind: TagProtected, Groups: []string{"release"}},
		{Name: "semver", Repo: "strict/*", Tag: "*", Kind: TagPattern, Pattern: `v\d+\.\d+\.\d+`},
	})
	require.NoError(t, err)
	return p
}

func TestAllow(t *testing.T) {
	p := policy(t)
	anon := Subject{ID: Anonymous}
	ci := Subject{ID: "runner", Groups: []string{"ci"}}
	alice := Subject{ID: "alice"}

	require.Equal(t, []Action{ActionPull}, p.Allow(anon, "library/ubuntu", []Action{ActionPull, ActionPush}))
	require.Empty(t, p.Allow(anon, "acme/app", []Action{ActionPull}))

	// Everybody is anonymous too.
	require.Equal(t, []Action{ActionPull}, p.Allow(alice, "library/ubuntu", []Action{ActionPull}))

	require.Equal(t, []Action{ActionPull, ActionPush}, p.Allow(ci, "acme/web", []Action{ActionPull, ActionPush, ActionDelete}))
	require.Equal(t, []Action{ActionPull, ActionPush, ActionDelete}, p.Allow(alice, "acme/app", []Action{ActionPull, ActionPush, ActionDelete}))
	require.Empty(t, p.Allow(alice, "acme/web", []Action{ActionPush}))

	require.Equal(t, []Action{ActionPull}, p.Allow(alice, "shared/x", []Action{ActionPull}))
	require.Empty(t, p.Allow(anon, "shared/x", []Action{ActionPull}))

	// A binding with `when` holds only for the credential whose claims match.
	release := Subject{ID: "gh", Groups: []string{"ci"}, Claims: map[string]any{
		"repository":   "acme/app",
		"workflow_ref": "acme/app/.github/workflows/release.yml@refs/heads/main",
	}}
	sibling := Subject{ID: "gh", Groups: []string{"ci"}, Claims: map[string]any{
		"repository":   "acme/app",
		"workflow_ref": "acme/app/.github/workflows/test.yml@refs/heads/main",
	}}
	require.Equal(t, []Action{ActionPush}, p.Allow(release, "gh/app", []Action{ActionPush}))
	require.Empty(t, p.Allow(sibling, "gh/app", []Action{ActionPush}))

	// A credential made for less than its holder may do is used for that
	// alone, whatever the bindings grant.
	narrow := Subject{ID: "alice", Only: []Action{ActionPull}}
	require.Equal(t, []Action{ActionPull}, p.Allow(narrow, "acme/app", []Action{ActionPull, ActionPush, ActionDelete}))
	require.Empty(t, p.Allow(Subject{ID: "alice", Only: []Action{}}, "acme/app", []Action{ActionPull}))
	require.Empty(t, p.AllowRegistry(Subject{ID: "ops", Only: []Action{ActionPull}}, []Action{ActionAdmin}))

	require.Equal(t, []Action{ActionCatalog}, p.AllowRegistry(anon, []Action{ActionCatalog, ActionSearch}))
	require.Equal(t, []Action{ActionAdmin}, p.AllowRegistry(Subject{ID: "ops"}, []Action{ActionAdmin}))
	// A binding narrower than `*` grants nothing registry-wide.
	require.Empty(t, p.AllowRegistry(alice, []Action{ActionAdmin}))
}

func TestCheckTag(t *testing.T) {
	p := policy(t)
	dev := Subject{ID: "dev"}

	require.NoError(t, p.CheckTag(dev, nil, "acme/app", "v1.0.0", TagCreate))
	var e *TagRuleError
	require.ErrorAs(t, p.CheckTag(dev, nil, "acme/app", "v1.0.0", TagMove), &e)
	require.Equal(t, TagImmutable, e.Kind)
	require.Error(t, p.CheckTag(dev, []Action{ActionAdmin}, "acme/app", "v1.0.0", TagDelete))
	require.NoError(t, p.CheckTag(dev, nil, "other/app", "v1.0.0", TagMove))

	require.NoError(t, p.CheckTag(dev, nil, "acme/app", "latest", TagCreate))
	require.Error(t, p.CheckTag(dev, nil, "acme/app", "latest", TagMove))
	require.NoError(t, p.CheckTag(dev, []Action{ActionAdmin}, "acme/app", "latest", TagMove))
	require.NoError(t, p.CheckTag(Subject{ID: "rel", Groups: []string{"release"}}, nil, "acme/app", "latest", TagDelete))

	require.NoError(t, p.CheckTag(dev, nil, "strict/app", "v1.2.3", TagCreate))
	require.Error(t, p.CheckTag(dev, nil, "strict/app", "latest", TagCreate))
	require.Error(t, p.CheckTag(dev, nil, "strict/app", "v1.2.3-rc", TagMove))
	require.NoError(t, p.CheckTag(dev, nil, "strict/app", "whatever", TagDelete))

	// A rule that cannot be evaluated refuses.
	bad, err := NewPolicy(nil, []TagRule{{Repo: "*", Tag: "*", Kind: TagPattern, Pattern: "("}})
	require.Error(t, err)
	require.Error(t, bad.CheckTag(dev, nil, "any", "thing", TagCreate))
}

func TestPolicyStoreKeepsWhatItHadOnFailure(t *testing.T) {
	ctx := context.Background()
	calls := 0
	src := sourceFunc(func(context.Context) ([]Binding, []TagRule, error) {
		calls++
		if calls > 1 {
			return nil, nil, context.DeadlineExceeded
		}
		return []Binding{{Subject: Anonymous, Repo: "*", Actions: []Action{ActionPull}}}, nil, nil
	})
	st := NewPolicyStore(time.Hour, src)
	require.Empty(t, st.Current().Allow(Subject{}, "x", []Action{ActionPull}))
	require.NoError(t, st.Refresh(ctx))
	require.Len(t, st.Current().Allow(Subject{}, "x", []Action{ActionPull}), 1)
	require.Error(t, st.Refresh(ctx))
	require.Len(t, st.Current().Allow(Subject{}, "x", []Action{ActionPull}), 1)
}

type sourceFunc func(context.Context) ([]Binding, []TagRule, error)

func (f sourceFunc) Load(ctx context.Context) ([]Binding, []TagRule, error) { return f(ctx) }

func writeHtpasswd(t *testing.T, path string, users map[string]string) {
	var b strings.Builder
	for u, p := range users {
		h, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.MinCost)
		require.NoError(t, err)
		b.WriteString(u + ":" + string(h) + "\n")
	}
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))
}

func TestHtpasswd(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "htpasswd")
	writeHtpasswd(t, path, map[string]string{"alice": "wonderland"})

	h, err := NewHtpasswd(path, map[string][]string{"alice": {"dev"}})
	require.NoError(t, err)

	s, err := h.Authenticate(ctx, "alice", "wonderland")
	require.NoError(t, err)
	require.Equal(t, Subject{ID: "alice", Groups: []string{"dev"}}, s)

	_, err = h.Authenticate(ctx, "alice", "wrong")
	require.ErrorIs(t, err, ErrUnauthenticated)
	_, err = h.Authenticate(ctx, "bob", "x")
	require.ErrorIs(t, err, ErrNotMine)

	// A changed file is read again, and a remembered password with it.
	writeHtpasswd(t, path, map[string]string{"bob": "builder"})
	future := time.Now().Add(time.Hour)
	os.Chtimes(path, future, future)
	h.now = func() time.Time { return time.Now().Add(2 * time.Second) }
	_, err = h.Authenticate(ctx, "alice", "wonderland")
	require.ErrorIs(t, err, ErrNotMine)
	_, err = h.Authenticate(ctx, "bob", "builder")
	require.NoError(t, err)

	_, err = NewHtpasswd(filepath.Join(t.TempDir(), "missing"), nil)
	require.Error(t, err)
}

func TestTokens(t *testing.T) {
	ctx := context.Background()
	sum := sha256.Sum256([]byte("hashed-secret"))
	ts, err := NewTokens([]StaticToken{
		{Name: "ci", Token: "plain-secret", Groups: []string{"ci"}},
		{Name: "deploy", TokenSHA256: hex.EncodeToString(sum[:])},
	})
	require.NoError(t, err)

	s, err := ts.Authenticate(ctx, "whoever", "plain-secret")
	require.NoError(t, err)
	require.Equal(t, "ci", s.ID)
	s, err = ts.Authenticate(ctx, "", "hashed-secret")
	require.NoError(t, err)
	require.Equal(t, "deploy", s.ID)
	_, err = ts.Authenticate(ctx, "ci", "nope")
	require.ErrorIs(t, err, ErrNotMine)

	_, err = NewTokens([]StaticToken{{Name: "x"}})
	require.Error(t, err)
}

func issuer(t *testing.T) *Issuer {
	k, err := GenerateKey()
	require.NoError(t, err)
	i, err := NewIssuer("cr", "registry.test", time.Minute, k)
	require.NoError(t, err)
	return i
}

func TestIssuer(t *testing.T) {
	i := issuer(t)
	token, _, err := i.Issue(Subject{ID: "alice", Groups: []string{"dev"}}, []Access{{Type: TypeRepository, Name: "acme/app", Actions: []string{"pull"}}})
	require.NoError(t, err)

	c, err := i.Verify(token)
	require.NoError(t, err)
	require.Equal(t, "alice", c.Subject)
	require.Equal(t, []string{"dev"}, c.Groups)
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
	st := NewPolicyStore(time.Hour, Static{Bindings: []Binding{
		{Subject: Anonymous, Repo: "library/*", Actions: []Action{ActionPull}},
		{Subject: "ci", Repo: "acme/*", Actions: []Action{ActionAll}},
	}})
	require.NoError(t, st.Refresh(context.Background()))
	ts, err := NewTokens([]StaticToken{{Name: "ci", Token: "secret"}})
	require.NoError(t, err)
	g := &Guard{Authenticator: Chain{ts}, Policy: st, Issuer: issuer(t)}

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

	code, c := get("scope="+url.QueryEscape("repository:acme/app:pull,push"), "ci", "secret")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "ci", c.Subject)
	require.Equal(t, []string{"pull", "push", "tag"}, c.Access[0].Actions)

	code, c = get("scope="+url.QueryEscape("repository:acme/app:pull,push")+"&scope="+url.QueryEscape("repository:library/ubuntu:pull"), "", "")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, Anonymous, c.Subject)
	require.Empty(t, c.Access[0].Actions)
	require.Equal(t, []string{"pull"}, c.Access[1].Actions)

	code, _ = get("", "ci", "wrong")
	require.Equal(t, http.StatusUnauthorized, code)

	form := url.Values{"grant_type": {"password"}, "username": {"x"}, "password": {"secret"}, "scope": {"repository:acme/app:*"}}
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	g.ServeToken(w, req)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestActionMethod(t *testing.T) {
	require.Equal(t, "/cr.Registry/Pull", ActionPull.Method())
	require.Equal(t, "/cr.Registry/Catalog", ActionCatalog.Method())
	require.Equal(t, "/cr.Registry/*", ActionAll.Method())
}
