package registry_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

type publicKeys string

func (p publicKeys) Value() (string, error) { return string(p), nil }

func TestJWTReadOnlyRegistry(t *testing.T) {
	// Exercise the HTTP routes over a filesystem store. The index's backend
	// does not participate in authenticating the request.
	cfg := registry.Config{Stores: flob.NewOsStores(t.TempDir()), Index: memindex.New()}
	h := &harness{t: t, h: registry.New(cfg)}
	manifest, _ := h.image("fleet/image", "a layer")
	res := h.do("PUT", "/v2/fleet/image/manifests/release", manifest, "Content-Type", v1.MediaTypeImageManifest)
	require.Equal(t, http.StatusCreated, res.StatusCode)

	key, err := auth.GenerateKey()
	require.NoError(t, err)
	b, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "device", Algorithm: "ES256", Use: "sig"}}})
	require.NoError(t, err)
	j, err := auth.NewJWT(auth.JWTConfig{Name: "devices", Issuer: "https://devices.invalid", Audience: "registry.example", JWKS: publicKeys(b)})
	require.NoError(t, err)
	st := auth.NewPolicyStore(time.Hour, auth.Static{
		Providers:   []auth.Provider{{Name: "devices", Authenticator: j, When: map[string]string{"scopes": "pull"}}},
		Permissions: map[string]auth.Permission{"read": {Repos: []string{"**"}, Actions: []auth.Action{auth.ActionPull, auth.ActionCatalog}}},
		Matches:     map[string]auth.Match{"devices": {For: "devices", Grant: []string{"read"}}},
	})
	require.NoError(t, st.Refresh(context.Background()))
	ownKey, err := auth.GenerateKey()
	require.NoError(t, err)
	issuer, err := auth.NewIssuer("cr", "registry.example", time.Minute, ownKey)
	require.NoError(t, err)
	cfg.Guard = &auth.Guard{Policy: st, Issuer: issuer}
	h.h = registry.New(cfg)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithHeader(jose.HeaderKey("kid"), "device"))
	require.NoError(t, err)
	token, err := jwt.Signed(signer).Claims(map[string]any{
		"iss": "https://devices.invalid", "aud": "registry.example",
		"exp": time.Now().Add(time.Hour).Unix(), "capabilities": []string{"pull"},
	}).Serialize()
	require.NoError(t, err)
	for _, header := range []string{"Bearer " + token, "Basic " + base64.StdEncoding.EncodeToString([]byte("anything:"+token))} {
		for _, path := range []string{
			"/v2/", "/v2/_catalog", "/v2/fleet/image/tags/list",
			"/v2/fleet/image/manifests/release",
			"/v2/fleet/image/referrers/" + digest.FromBytes(manifest).String(),
			"/v2/fleet/image/blobs/" + digest.FromString("a layer").String(),
		} {
			require.Equal(t, http.StatusOK, h.do("GET", path, nil, "Authorization", header).StatusCode, path)
			require.Equal(t, http.StatusUnauthorized, h.do("GET", path, nil).StatusCode, path)
		}
		for _, c := range []struct{ method, path string }{
			{"POST", "/v2/fleet/image/blobs/uploads/"},
			{"PUT", "/v2/fleet/image/manifests/changed"},
			{"DELETE", "/v2/fleet/image/blobs/" + digest.FromString("a layer").String()},
		} {
			require.Equal(t, http.StatusForbidden, h.do(c.method, c.path, nil, "Authorization", header).StatusCode, c.path)
		}
	}
}
