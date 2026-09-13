package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExchange(t *testing.T) {
	ctx := context.Background()
	p := newProvider(t, "k1")
	o, err := NewOIDC(OIDCConfig{Issuer: p.srv.URL, Audience: "cr"})
	require.NoError(t, err)
	is := issuer(t)

	st := NewPolicyStore(time.Hour, Static{Bindings: []Binding{{
		Group:   Authenticated,
		Repo:    "acme/app",
		Actions: []Action{ActionPull, ActionPush},
		When:    map[string]string{"workflow_ref": "acme/app/.github/workflows/release.yml@*"},
	}}})
	require.NoError(t, st.Refresh(ctx))
	g := &Guard{Authenticator: Chain{o, LoginTokens{Issuer: is}}, Policy: st, Issuer: is, Exchange: time.Hour}

	exchange := func(g *Guard, authorization string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/token/exchange", nil)
		req.Header.Set("Authorization", authorization)
		w := httptest.NewRecorder()
		g.ServeExchange(w, req)
		return w
	}

	w := exchange(g, "Bearer "+p.sign(t, p.job("release.yml")))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	require.Greater(t, res.ExpiresIn, 3500)
	login := res.AccessToken

	// Given as a password, it is the job again, with the job's claims.
	s, err := g.Authenticator.Authenticate(ctx, "anything", login)
	require.NoError(t, err)
	require.Equal(t, "acme/app/.github/workflows/release.yml@refs/heads/main", s.Claims["workflow_ref"])

	req := httptest.NewRequest("GET", "/token?scope="+url.QueryEscape("repository:acme/app:pull,push"), nil)
	req.SetBasicAuth("oidc", login)
	tw := httptest.NewRecorder()
	g.ServeToken(tw, req)
	require.Equal(t, http.StatusOK, tw.Code)
	var tr tokenResponse
	require.NoError(t, json.Unmarshal(tw.Body.Bytes(), &tr))
	c, err := is.Verify(tr.Token)
	require.NoError(t, err)
	require.Equal(t, []string{"pull", "push"}, c.Access[0].Actions)

	// It is never an access token, and never exchanged again.
	_, err = is.Verify(login)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, exchange(g, "Bearer "+login).Code)

	require.Equal(t, http.StatusUnauthorized, exchange(g, "Bearer not-a-credential").Code)

	closed := *g
	closed.Exchange = 0
	require.Equal(t, http.StatusNotFound, exchange(&closed, "Bearer "+p.sign(t, p.job("release.yml"))).Code)
}

// A credential narrowed to some actions stays narrowed through every token
// that stands in for it, a narrowing to nothing included.
func TestNarrowedTokens(t *testing.T) {
	is := issuer(t)
	st := NewPolicyStore(time.Hour, Static{})
	require.NoError(t, st.Refresh(context.Background()))
	g := &Guard{Policy: st, Issuer: is}

	for _, only := range [][]Action{nil, {}, {ActionPull}} {
		s := Subject{ID: "ci", Only: only}

		login, _, err := is.IssueLogin(s, time.Hour)
		require.NoError(t, err)
		back, err := is.VerifyLogin(login)
		require.NoError(t, err)
		require.Equal(t, only, back.Only)

		access, _, err := is.Issue(s, nil)
		require.NoError(t, err)
		req := httptest.NewRequest("GET", "/v2/", nil)
		req.Header.Set("Authorization", "Bearer "+access)
		c, err := g.Caller(req)
		require.NoError(t, err)
		require.Equal(t, only, c.Subject.Only)
	}
}
