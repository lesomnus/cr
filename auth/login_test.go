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
	o, err := NewOIDC(OIDCConfig{Name: "github", Issuer: p.srv.URL, Audience: "cr"})
	require.NoError(t, err)
	is := issuer(t)

	store := func(exchange time.Duration, others ...Provider) *PolicyStore {
		st := NewPolicyStore(time.Hour, Static{
			Providers:   append([]Provider{{Name: "github", Authenticator: o, Exchange: exchange}}, others...),
			Permissions: map[string]Permission{"app": {Repos: []string{"acme/app"}, Actions: []Action{ActionPull, ActionPush}}},
			Matches: map[string]Match{"release": {For: "github", Grant: []string{"app"}, When: map[string]string{
				"workflow_ref": "acme/app/.github/workflows/release.yml@refs/heads/*",
			}}},
		})
		require.NoError(t, st.Refresh(ctx))
		return st
	}
	g := &Guard{Policy: store(time.Hour), Issuer: is}

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

	// Given as a password, it is the job again, from its provider and with
	// its claims.
	s, err := g.authenticate(ctx, g.Policy.Current(), "anything", login)
	require.NoError(t, err)
	require.Equal(t, "github", s.Provider)
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

	// A provider that says nothing of exchange trades nothing, and with no
	// provider that does, there is no exchange at all.
	id := "Bearer " + p.sign(t, p.job("release.yml"))
	other := &Guard{Policy: store(0, Provider{Name: "other", Exchange: time.Hour}), Issuer: is}
	require.Equal(t, http.StatusForbidden, exchange(other, id).Code)
	closed := &Guard{Policy: store(0), Issuer: is}
	require.Equal(t, http.StatusNotFound, exchange(closed, id).Code)

	// A token exchanged while it could be stands for its caller until it
	// expires.
	_, err = closed.authenticate(ctx, closed.Policy.Current(), "anything", login)
	require.NoError(t, err)
}
