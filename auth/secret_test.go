package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type literal string

func (l literal) Value() (string, error) { return string(l), nil }

type unreadable struct{}

func (unreadable) Value() (string, error) {
	return "", errors.New("secret file /etc/cr/breakglass: no such file")
}

func TestSecret(t *testing.T) {
	ctx := context.Background()
	s := NewSecret("breakglass", "admin", literal("correct horse battery staple"))

	got, err := s.Authenticate(ctx, "admin", "correct horse battery staple")
	require.NoError(t, err)
	require.Equal(t, Subject{ID: "breakglass:admin", Provider: "breakglass", Claims: map[string]any{"username": "admin"}}, got)

	// Another name is somebody else's to answer for, as `oidc` for a CI job.
	_, err = s.Authenticate(ctx, "oidc", "eyJ.a.b")
	require.ErrorIs(t, err, ErrNotMine)

	// The name, and not the password, is a refusal: nobody further down is
	// asked to let in what claimed to be this caller.
	_, err = s.Authenticate(ctx, "admin", "correct horse battery")
	require.ErrorIs(t, err, ErrUnauthenticated)
	_, err = s.Authenticate(ctx, "admin", "")
	require.ErrorIs(t, err, ErrUnauthenticated)

	// A password that cannot be read lets nobody in.
	_, err = NewSecret("breakglass", "admin", unreadable{}).Authenticate(ctx, "admin", "")
	require.ErrorIs(t, err, ErrUnauthenticated)
}

func TestSecretMatch(t *testing.T) {
	r := rules()
	r.Providers = append(r.Providers, Provider{Name: "breakglass", Authenticator: NewSecret("breakglass", "admin", literal("pw"))})
	// One caller: a match for it says nothing more.
	r.Matches["breakglass"] = Match{For: "breakglass", Grant: []string{"admin"}}
	st := NewPolicyStore(time.Hour, Static(r))
	require.NoError(t, st.Refresh(context.Background()))
	p := st.Current()

	admin, err := p.Authenticate(context.Background(), "admin", "pw")
	require.NoError(t, err)
	require.Equal(t, "secret", admin.Via)
	all := []Action{ActionPull, ActionPush, ActionTag, ActionDelete}
	require.Equal(t, all, p.Allow(admin, "anything/at/all", all))
	require.Equal(t, []Action{ActionAdmin}, p.AllowRegistry(admin, []Action{ActionAdmin}))

	// Nobody else is under it.
	require.Empty(t, p.Allow(job("release.yml"), "anything/at/all", []Action{ActionDelete}))

	// Through /token, as `docker login` and then a push get there.
	g := &Guard{Policy: st, Issuer: issuer(t)}
	token := func(user, pass string) int {
		req := httptest.NewRequest("GET", "/token?service=registry.test&scope="+url.QueryEscape("repository:acme/app:pull,push"), nil)
		req.SetBasicAuth(user, pass)
		w := httptest.NewRecorder()
		g.ServeToken(w, req)
		if w.Code == http.StatusOK {
			var v tokenResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v))
			c, err := g.Issuer.Verify(v.Token)
			require.NoError(t, err)
			require.Equal(t, "breakglass:admin", c.Subject)
			require.Equal(t, []string{"pull", "push", "tag"}, c.Access[0].Actions)
		}
		return w.Code
	}
	require.Equal(t, http.StatusOK, token("admin", "pw"))
	require.Equal(t, http.StatusUnauthorized, token("admin", "nope"))

	// It is still a provider, and one with its own `when` narrows it like
	// any other.
	r.Providers[len(r.Providers)-1].When = map[string]string{"username": "root"}
	p2, err := NewPolicy(r)
	require.NoError(t, err)
	require.Empty(t, p2.Allow(admin, "acme/app", all))
}
