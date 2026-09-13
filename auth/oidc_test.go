package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// provider is an OpenID Connect provider as far as a verifier sees one: its
// discovery document and its keys.
type provider struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string
}

func newProvider(t *testing.T, kid string) *provider {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	p := &provider{key: key, kid: kid}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": p.srv.URL, "jwks_uri": p.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &p.key.PublicKey, KeyID: p.kid, Algorithm: string(jose.RS256), Use: "sig"},
		}})
	})
	return p
}

func (p *provider) sign(t *testing.T, claims map[string]any) string {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: p.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), p.kid),
	)
	require.NoError(t, err)
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)
	return token
}

// job is the claims GitHub puts in an ID token for a job of workflow.
func (p *provider) job(workflow string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":          p.srv.URL,
		"aud":          "cr",
		"sub":          "repo:acme/app:ref:refs/heads/main",
		"iat":          now.Unix(),
		"nbf":          now.Add(-time.Minute).Unix(),
		"exp":          now.Add(5 * time.Minute).Unix(),
		"repository":   "acme/app",
		"workflow_ref": "acme/app/.github/workflows/" + workflow + "@refs/heads/main",
		"groups":       []string{"ci"},
	}
}

func TestOIDC(t *testing.T) {
	ctx := context.Background()
	p := newProvider(t, "k1")
	o, err := NewOIDC(OIDCConfig{Issuer: p.srv.URL, Audience: "cr", GroupsClaim: "groups", Prefix: "github:"})
	require.NoError(t, err)

	s, err := o.Authenticate(ctx, "oidc", p.sign(t, p.job("release.yml")))
	require.NoError(t, err)
	require.Equal(t, "github:repo:acme/app:ref:refs/heads/main", s.ID)
	require.Equal(t, []string{"ci"}, s.Groups)
	require.Equal(t, "acme/app", s.Claims["repository"])

	with := func(k string, v any) string {
		c := p.job("release.yml")
		c[k] = v
		return p.sign(t, c)
	}
	_, err = o.Authenticate(ctx, "oidc", with("aud", "somebody-else"))
	require.ErrorIs(t, err, ErrUnauthenticated)
	_, err = o.Authenticate(ctx, "oidc", with("exp", time.Now().Add(-time.Hour).Unix()))
	require.ErrorIs(t, err, ErrUnauthenticated)

	// Another issuer's token, or no token at all, is some other
	// authenticator's to judge.
	_, err = o.Authenticate(ctx, "oidc", with("iss", "https://elsewhere.example"))
	require.ErrorIs(t, err, ErrNotMine)
	_, err = o.Authenticate(ctx, "alice", "a password")
	require.ErrorIs(t, err, ErrNotMine)

	// Signed by a key the provider does not publish.
	forger := newProvider(t, "k2")
	c := p.job("release.yml")
	_, err = o.Authenticate(ctx, "oidc", forger.sign(t, c))
	require.ErrorIs(t, err, ErrUnauthenticated)
	forger.kid = "k1"
	_, err = o.Authenticate(ctx, "oidc", forger.sign(t, c))
	require.ErrorIs(t, err, ErrUnauthenticated)
}

// TestOIDCWhen is the point of it: one workflow may push, and a sibling in the
// same repository may not.
func TestOIDCWhen(t *testing.T) {
	p := newProvider(t, "k1")
	o, err := NewOIDC(OIDCConfig{Issuer: p.srv.URL, Audience: "cr"})
	require.NoError(t, err)

	st := NewPolicyStore(time.Hour, Static{Bindings: []Binding{{
		Group:   Authenticated,
		Repo:    "acme/app",
		Actions: []Action{ActionPull, ActionPush, ActionTag},
		When: map[string]string{
			"repository":   "acme/app",
			"workflow_ref": "acme/app/.github/workflows/release.yml@*",
		},
	}}})
	require.NoError(t, st.Refresh(context.Background()))
	g := &Guard{Authenticator: Chain{o}, Policy: st, Issuer: issuer(t)}

	token := func(idToken string) *Claims {
		req := httptest.NewRequest("GET", "/token?scope="+url.QueryEscape("repository:acme/app:pull,push"), nil)
		req.SetBasicAuth("oidc", idToken)
		w := httptest.NewRecorder()
		g.ServeToken(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		var v tokenResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v))
		c, err := g.Issuer.Verify(v.Token)
		require.NoError(t, err)
		return c
	}

	release := token(p.sign(t, p.job("release.yml")))
	require.Equal(t, []string{"pull", "push", "tag"}, release.Access[0].Actions)

	sibling := token(p.sign(t, p.job("test.yml")))
	require.Empty(t, sibling.Access[0].Actions)
	require.Equal(t, []string{"pull", "push", "tag"}, sibling.Refused[0].Actions)
}
