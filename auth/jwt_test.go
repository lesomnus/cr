package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

func deviceKey(t *testing.T, kid string) (*ecdsa.PrivateKey, jose.JSONWebKey) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return k, jose.JSONWebKey{Key: &k.PublicKey, KeyID: kid, Algorithm: "ES256", Use: "sig"}
}

func deviceJWKS(t *testing.T, keys ...jose.JSONWebKey) literal {
	t.Helper()
	b, err := json.Marshal(jose.JSONWebKeySet{Keys: keys})
	require.NoError(t, err)
	return literal(b)
}

func signDevice(t *testing.T, k *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("JWT")
	if kid != "" {
		opts.WithHeader(jose.HeaderKey("kid"), kid)
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: k}, opts)
	require.NoError(t, err)
	token, err := jwt.Signed(s).Claims(claims).Serialize()
	require.NoError(t, err)
	return token
}

func deviceClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss": "https://issuer.invalid", "sub": "robot/42",
		"aud": []string{"registry.example"}, "exp": now.Add(time.Hour).Unix(),
		"capabilities": []string{"pull"},
	}
}

func TestJWTClaims(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	k, pub := deviceKey(t, "old")
	j, err := NewJWT(JWTConfig{Name: "fleet", Issuer: "https://issuer.invalid", Audience: "registry.example", JWKS: deviceJWKS(t, pub), AudienceHost: true})
	require.NoError(t, err)
	j.now = func() time.Time { return now }

	for name, c := range map[string]struct {
		change func(map[string]any)
		ok     bool
	}{
		"device":                             {func(map[string]any) {}, true},
		"origin audience":                    {func(c map[string]any) { c["aud"] = "https://REGISTRY.example:443" }, true},
		"array audience":                     {func(c map[string]any) { c["aud"] = []string{"elsewhere", "http://registry.example"} }, true},
		"no subject":                         {func(c map[string]any) { delete(c, "sub") }, true},
		"future iat is not a validity bound": {func(c map[string]any) { c["iat"] = now.Add(time.Hour).Unix() }, true},
		"expiry skew":                        {func(c map[string]any) { c["exp"] = now.Add(-59 * time.Second).Unix() }, true},
		"expired at boundary":                {func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }, false},
		"nbf skew":                           {func(c map[string]any) { c["nbf"] = now.Add(time.Minute).Unix() }, true},
		"not yet valid":                      {func(c map[string]any) { c["nbf"] = now.Add(61 * time.Second).Unix() }, false},
		"missing expiry":                     {func(c map[string]any) { delete(c, "exp") }, false},
		"wrong expiry type":                  {func(c map[string]any) { c["exp"] = "tomorrow" }, false},
		"wrong issuer":                       {func(c map[string]any) { c["iss"] = "https://other.invalid" }, false},
		"issuer is exact":                    {func(c map[string]any) { c["iss"] = "https://issuer.invalid/" }, false},
		"wrong audience":                     {func(c map[string]any) { c["aud"] = "other.example" }, false},
		"wrong port":                         {func(c map[string]any) { c["aud"] = "registry.example:444" }, false},
		"missing audience":                   {func(c map[string]any) { delete(c, "aud") }, false},
		"empty audience":                     {func(c map[string]any) { c["aud"] = []string{} }, false},
		"bare path":                          {func(c map[string]any) { c["aud"] = "registry.example/other" }, false},
		"bare userinfo":                      {func(c map[string]any) { c["aud"] = "user@registry.example" }, false},
	} {
		t.Run(name, func(t *testing.T) {
			claims := deviceClaims(now)
			c.change(claims)
			s, err := j.Authenticate(context.Background(), "anything", signDevice(t, k, "old", claims))
			if c.ok {
				require.NoError(t, err)
				require.Equal(t, "fleet", s.Provider)
				require.Equal(t, []string{"pull"}, s.Claims["scopes"])
			} else {
				require.Error(t, err)
			}
		})
	}

	// Host equivalence must be explicitly requested; exact audiences remain
	// distinct even when they happen to contain the same host.
	j.config.AudienceHost = false
	c := deviceClaims(now)
	c["aud"] = "https://registry.example"
	_, err = j.Authenticate(context.Background(), "", signDevice(t, k, "old", c))
	require.ErrorIs(t, err, ErrUnauthenticated)
}

func TestJWTAudiences(t *testing.T) {
	now := time.Now()
	k, pub := deviceKey(t, "fleet")
	accepted := []string{"registry.example", "hday:robot-infra"}
	j, err := NewJWT(JWTConfig{Name: "fleet", Issuer: "https://issuer.invalid", Audiences: accepted, JWKS: deviceJWKS(t, pub)})
	require.NoError(t, err)
	accepted[0] = "changed.example" // construction snapshots the configuration
	for name, c := range map[string]struct {
		audience any
		ok       bool
	}{
		"legacy scalar":           {"registry.example", true},
		"session scalar":          {"hday:robot-infra", true},
		"legacy array":            {[]string{"registry.example"}, true},
		"session array":           {[]string{"hday:robot-infra"}, true},
		"dual":                    {[]string{"registry.example", "hday:robot-infra"}, true},
		"later element":           {[]string{"elsewhere", "hday:robot-infra"}, true},
		"wrong":                   {"hday:other", false},
		"case sensitive":          {"HDAY:robot-infra", false},
		"URL not host by default": {"https://registry.example", false},
		"mutated caller":          {"changed.example", false},
		"empty":                   {[]string{}, false},
		"missing":                 {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			claims := deviceClaims(now)
			claims["aud"] = c.audience
			_, err := j.Authenticate(context.Background(), "", signDevice(t, k, "fleet", claims))
			if c.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	j.config.AudienceHost = true
	claims := deviceClaims(now)
	claims["aud"] = "https://REGISTRY.example:443"
	_, err = j.Authenticate(context.Background(), "", signDevice(t, k, "fleet", claims))
	require.NoError(t, err)
	for name, c := range map[string]JWTConfig{
		"both":        {Audience: "legacy", Audiences: []string{"session"}},
		"empty entry": {Audiences: []string{"session", ""}},
		"no audience": {},
	} {
		t.Run(name, func(t *testing.T) {
			c.Name, c.Issuer, c.JWKS = "fleet", "https://issuer.invalid", deviceJWKS(t, pub)
			_, err := NewJWT(c)
			require.Error(t, err)
		})
	}
}

func TestJWTKeys(t *testing.T) {
	now := time.Now()
	old, pub := deviceKey(t, "old")
	newKey, newPub := deviceKey(t, "new")
	j, err := NewJWT(JWTConfig{Name: "fleet", Issuer: "https://issuer.invalid", Audience: "registry.example", JWKS: deviceJWKS(t, pub)})
	require.NoError(t, err)
	check := func(k *ecdsa.PrivateKey, kid string, ok bool) {
		t.Helper()
		_, err := j.Authenticate(context.Background(), "", signDevice(t, k, kid, deviceClaims(now)))
		if ok {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrUnauthenticated)
		}
	}
	check(old, "old", true)
	check(old, "", true)
	check(old, "unknown", false)
	check(newKey, "old", false)
	j.config.JWKS = deviceJWKS(t, pub, newPub)
	check(old, "old", true)
	check(newKey, "new", true)
	check(old, "", false)
	j.config.JWKS = deviceJWKS(t, newPub)
	check(old, "old", false)
	check(newKey, "new", true)
	for _, bad := range []SecretValue{unreadable{}, literal(`{`), literal(`{"keys":[]}`), deviceJWKS(t, jose.JSONWebKey{Key: old, KeyID: "old"})} {
		j.config.JWKS = bad
		check(old, "old", false)
	}
	pub.Use = "enc"
	j.config.JWKS = deviceJWKS(t, pub)
	check(old, "old", false)
	pub.Use, pub.Algorithm = "sig", "ES384"
	j.config.JWKS = deviceJWKS(t, pub)
	check(old, "old", false)

	// A public key's bytes must never work as a symmetric signing secret.
	j.config.JWKS = deviceJWKS(t, newPub)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte(j.config.JWKS.(literal))}, nil)
	require.NoError(t, err)
	token, err := jwt.Signed(signer).Claims(deviceClaims(now)).Serialize()
	require.NoError(t, err)
	_, err = j.Authenticate(context.Background(), "", token)
	require.Error(t, err)
	for _, bad := range []string{"", "a password", "a.b.c", "eyJhbGciOiJub25lIn0.e30."} {
		_, err = j.Authenticate(context.Background(), "", bad)
		require.Error(t, err)
	}
}

func TestJWTVerifiesOffline(t *testing.T) {
	p := newProvider(t, "rsa")
	// An issuer that cannot even answer discovery must not prevent a pull.
	p.srv.Close()
	j, err := NewJWT(JWTConfig{
		Name: "offline", Issuer: p.srv.URL, Audience: "cr",
		JWKS: deviceJWKS(t, jose.JSONWebKey{Key: &p.key.PublicKey, KeyID: p.kid, Algorithm: "RS256", Use: "sig"}),
	})
	require.NoError(t, err)
	_, err = j.Authenticate(context.Background(), "", p.sign(t, p.job("release.yml")))
	require.NoError(t, err)
}

func TestJWTGuard(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	k, pub := deviceKey(t, "fleet")
	providers := []Provider{}
	matches := map[string]Match{}
	for _, name := range []string{"robot", "ui"} {
		j, err := NewJWT(JWTConfig{Name: name, Issuer: "https://" + name + ".invalid", Audience: "registry.example", JWKS: deviceJWKS(t, pub)})
		require.NoError(t, err)
		providers = append(providers, Provider{Name: name, Authenticator: j, When: map[string]string{"scopes": "pull"}})
		matches[name] = Match{For: name, Grant: []string{"read"}}
	}
	st := NewPolicyStore(time.Hour, Static{
		Providers: providers, Matches: matches,
		Permissions: map[string]Permission{"read": {Repos: []string{"**"}, Actions: []Action{ActionPull, ActionCatalog}}},
	})
	require.NoError(t, st.Refresh(ctx))
	g := &Guard{Policy: st, Issuer: issuer(t)}
	g.Issuer.ttl = 5 * time.Minute
	for _, name := range []string{"robot", "ui"} {
		for _, scope := range []bool{false, true} {
			c := deviceClaims(now)
			c["iss"] = "https://" + name + ".invalid"
			if scope {
				delete(c, "capabilities")
				c["scope"] = "other pull another"
			}
			token := signDevice(t, k, "fleet", c)
			for _, header := range []string{"Bearer " + token, "bEaReR " + token, "Basic " + base64.StdEncoding.EncodeToString([]byte("anything:"+token))} {
				r := httptest.NewRequest("GET", "/v2/", nil)
				r.Header.Set("Authorization", header)
				caller, err := g.Caller(r)
				require.NoError(t, err)
				require.Equal(t, "jwt", caller.Subject.Via)
				require.Equal(t, []Action{ActionPull}, caller.Allowed("any/image", ActionPull, ActionPush, ActionTag, ActionDelete))
				require.Equal(t, []Action{ActionCatalog}, caller.AllowedRegistry(ActionCatalog, ActionAdmin))
			}
		}
	}
	for _, c := range []map[string]any{
		{"capabilities": []string{"push"}},
		{"scope": "notpull pull-extra"},
		{"scopes": []string{"pull"}}, // a caller cannot supply the derived claim
	} {
		claims := deviceClaims(now)
		delete(claims, "capabilities")
		claims["iss"] = "https://robot.invalid"
		for k, v := range c {
			claims[k] = v
		}
		r := httptest.NewRequest("GET", "/v2/", nil)
		r.Header.Set("Authorization", "Bearer "+signDevice(t, k, "fleet", claims))
		caller, err := g.Caller(r)
		require.NoError(t, err)
		require.Empty(t, caller.Allowed("any/image", ActionPull))
	}
	anon, err := g.Caller(httptest.NewRequest("GET", "/v2/", nil))
	require.NoError(t, err)
	require.Empty(t, anon.Allowed("any/image", ActionPull))

	// A standard distribution client can still obtain an access token, but
	// doing so must not extend the lifetime of the external credential.
	c := deviceClaims(now)
	c["iss"], c["exp"] = "https://robot.invalid", now.Add(90*time.Second).Unix()
	r := httptest.NewRequest("GET", "/token?scope=repository:any/image:pull", nil)
	r.SetBasicAuth("robot", signDevice(t, k, "fleet", c))
	w := httptest.NewRecorder()
	g.ServeToken(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	var response tokenResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	access, err := g.Issuer.Verify(response.Token)
	require.NoError(t, err)
	require.True(t, now.Add(90*time.Second).Equal(access.Expiry.Time()))
	require.InDelta(t, 90, response.ExpiresIn, 1)
	r = httptest.NewRequest("GET", "/v2/", nil)
	r.Header.Set("Authorization", "Bearer "+response.Token)
	caller, err := g.Caller(r)
	require.NoError(t, err)
	require.True(t, caller.FromToken())
	require.Equal(t, []Action{ActionPull}, caller.Allowed("any/image", ActionPull, ActionPush))
}

func TestJWTBearerDoesNotReinterpretOwnIssuer(t *testing.T) {
	k, pub := deviceKey(t, "external")
	i := issuer(t)
	j, err := NewJWT(JWTConfig{Name: "collision", Issuer: i.name, Audience: "registry.example", JWKS: deviceJWKS(t, pub)})
	require.NoError(t, err)
	st := NewPolicyStore(time.Hour, Static{Providers: []Provider{{Name: "collision", Authenticator: j}}})
	require.NoError(t, st.Refresh(context.Background()))
	c := deviceClaims(time.Now())
	c["iss"] = i.name
	r := httptest.NewRequest("GET", "/v2/", nil)
	r.Header.Set("Authorization", "Bearer "+signDevice(t, k, "external", c))
	_, err = (&Guard{Policy: st, Issuer: i}).Caller(r)
	require.ErrorIs(t, err, ErrUnauthenticated)
}

func TestOIDCDoesNotAcceptDirectBearer(t *testing.T) {
	p := newProvider(t, "oidc")
	o, err := NewOIDC(OIDCConfig{Name: "oidc", Issuer: p.srv.URL, Audience: "cr"})
	require.NoError(t, err)
	st := NewPolicyStore(time.Hour, Static{Providers: []Provider{{Name: "oidc", Authenticator: o}}})
	require.NoError(t, st.Refresh(context.Background()))
	r := httptest.NewRequest("GET", "/v2/", nil)
	r.Header.Set("Authorization", "Bearer "+p.sign(t, p.job("release.yml")))
	_, err = (&Guard{Policy: st, Issuer: issuer(t)}).Caller(r)
	require.ErrorIs(t, err, ErrUnauthenticated)
}
