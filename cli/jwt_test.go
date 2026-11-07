package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/auth"
)

func TestPolicyJWTProvider(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keys := filepath.Join(dir, "jwks.json")
	path := write(t, filepath.Join(dir, "cr.auth.yaml"), `
providers:
  fleet:
    kind: jwt
    issuer: https://issuer.invalid
    audience: registry.example
    audience_host: true
    jwks: ${file:`+keys+`}
    when: {scopes: pull}
matches:
  fleet:
    for: fleet
    grant:
      - repos: ["**"]
        actions: [pull, catalog]
`)
	p, warnings, err := readPolicy(path)
	require.NoError(t, err)
	require.Len(t, warnings, 1, "missing keys fail closed without opening the registry")

	// Each publication changes both the trusted public key and the private
	// signer. The policy itself stays in place across rotation.
	publish := func(kid string) string {
		key, err := auth.GenerateKey()
		require.NoError(t, err)
		pub := jose.JSONWebKey{Key: &key.PublicKey, KeyID: kid, Algorithm: "ES256", Use: "sig"}
		b, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{pub}})
		require.NoError(t, err)
		write(t, keys+".new", string(b))
		require.NoError(t, os.Rename(keys+".new", keys))
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithHeader(jose.HeaderKey("kid"), kid))
		require.NoError(t, err)
		token, err := jwt.Signed(signer).Claims(map[string]any{
			"iss": "https://issuer.invalid", "sub": "robot/42", "aud": "https://registry.example",
			"exp": time.Now().Add(time.Hour).Unix(), "scope": "other pull",
		}).Serialize()
		require.NoError(t, err)
		return token
	}
	old := publish("old")
	s, err := p.Authenticate(ctx, "any username", old)
	require.NoError(t, err)
	require.Equal(t, "fleet:robot/42", s.ID)
	require.Equal(t, []auth.Action{auth.ActionPull}, p.Allow(s, "any/image", []auth.Action{auth.ActionPull, auth.ActionPush}))
	require.Equal(t, []auth.Action{auth.ActionCatalog}, p.AllowRegistry(s, []auth.Action{auth.ActionCatalog}))
	rotated := publish("new")
	_, err = p.Authenticate(ctx, "", old)
	require.Error(t, err)
	_, err = p.Authenticate(ctx, "", rotated)
	require.NoError(t, err)
	write(t, keys+".new", "{not a JWKS}")
	require.NoError(t, os.Rename(keys+".new", keys))
	_, err = p.Authenticate(ctx, "", rotated)
	require.Error(t, err)
}

func TestPolicyJWTAudiences(t *testing.T) {
	key, err := auth.GenerateKey()
	require.NoError(t, err)
	pub := jose.JSONWebKey{Key: &key.PublicKey, KeyID: "fixture", Algorithm: "ES256", Use: "sig"}
	keys, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{pub}})
	require.NoError(t, err)
	dir := t.TempDir()
	file := write(t, filepath.Join(dir, "keys.json"), string(keys))
	p, _, err := readPolicy(write(t, filepath.Join(dir, "auth.yaml"), `
providers:
  fleet:
    kind: jwt
    issuer: https://issuer.invalid
    audiences: [registry.example, hday:robot-infra]
    jwks: ${file:`+file+`}
permissions:
  production-read:
    repos: ["dist/prod/**"]
    actions: [pull]
matches:
  legacy:
    for: fleet
    when: {scopes: pull}
    grant: [production-read]
  session:
    for: fleet
    when: {capabilities: cr.pull}
    grant: [production-read]
`))
	require.NoError(t, err)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "fixture"))
	require.NoError(t, err)
	for _, audience := range []any{"registry.example", "hday:robot-infra", []string{"registry.example", "hday:robot-infra"}} {
		for _, capability := range []string{"pull", "cr.pull", "cr.pull.extra", "depot.read"} {
			token, err := jwt.Signed(signer).Claims(map[string]any{
				"iss": "https://issuer.invalid", "sub": "robot-id", "aud": audience,
				"exp": time.Now().Add(time.Hour).Unix(), "capabilities": []string{capability},
			}).Serialize()
			require.NoError(t, err)
			s, err := p.Authenticate(context.Background(), "", token)
			require.NoError(t, err)
			allowed := p.Allow(s, "dist/prod/base", []auth.Action{auth.ActionPull, auth.ActionPush})
			if capability == "pull" || capability == "cr.pull" {
				require.Equal(t, []auth.Action{auth.ActionPull}, allowed)
			} else {
				require.Empty(t, allowed)
			}
			require.Empty(t, p.Allow(s, "dist/stage/base", []auth.Action{auth.ActionPull}))
		}
	}
}

func TestPolicyJWTProviderRefuses(t *testing.T) {
	base := "providers:\n  fleet:\n    kind: jwt\n    issuer: https://issuer.invalid\n    audience: registry.example\n"
	for name, c := range map[string]struct{ file, want string }{
		"missing keys":                 {base, "jwks: not set"},
		"missing audience":             {"providers:\n  fleet:\n    kind: jwt\n    issuer: https://issuer.invalid\n    jwks: '{}'\n", "audience"},
		"bad host audience":            {"providers:\n  fleet:\n    kind: jwt\n    issuer: https://issuer.invalid\n    audience: https:///missing-host\n    audience_host: true\n    jwks: '{}'\n", "audience has no host"},
		"exchange extends credentials": {base + "    jwks: '{}'\n    exchange: 1h\n", "without extending their lifetime"},
		"oidc ignores pinned keys":     {"providers:\n  fleet:\n    kind: oidc\n    issuer: https://issuer.invalid\n    audience: registry.example\n    jwks: '{}'\n", "are a jwt provider's"},
		"secret ignores audience mode": {"providers:\n  fleet:\n    kind: secret\n    username: admin\n    password: pw\n    audience_host: true\n", "are a jwt provider's"},
		"conflicting audiences":        {base + "    jwks: '{}'\n    audiences: [hday:robot-infra]\n", "mutually exclusive"},
		"empty audience entry":         {"providers:\n  fleet:\n    kind: jwt\n    issuer: https://issuer.invalid\n    audiences: ['']\n    jwks: '{}'\n", "audience is empty"},
		"OIDC audience list":           {"providers:\n  ci:\n    kind: oidc\n    issuer: https://issuer.invalid\n    audiences: [ci]\n", "are a jwt provider's"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := readPolicy(write(t, filepath.Join(t.TempDir(), "p.yaml"), c.file))
			require.ErrorContains(t, err, c.want)
		})
	}
}
