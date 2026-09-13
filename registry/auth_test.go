package registry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

type guarded struct {
	*harness
	guard *auth.Guard
}

func newGuarded(t *testing.T) *guarded {
	st := auth.NewPolicyStore(time.Hour, auth.Static{
		Bindings: []auth.Binding{
			{Subject: auth.Anonymous, Repo: "public/*", Actions: []auth.Action{auth.ActionPull}},
			{Subject: auth.Anonymous, Repo: "*", Actions: []auth.Action{auth.ActionCatalog}},
			{Subject: "alice", Repo: "*", Actions: []auth.Action{auth.ActionAll}},
			{Group: "dev", Repo: "team/*", Actions: []auth.Action{auth.ActionPull, auth.ActionPush, auth.ActionTag}},
		},
		TagRules: []auth.TagRule{
			{Name: "releases", Repo: "*", Tag: "v*", Kind: auth.TagImmutable},
			{Name: "semver", Repo: "team/*", Tag: "*", Kind: auth.TagPattern, Pattern: `v\d+\.\d+\.\d+|latest`},
		},
	})
	require.NoError(t, st.Refresh(context.Background()))
	tokens, err := auth.NewTokens([]auth.StaticToken{
		{Name: "alice", Token: "alice-secret"},
		{Name: "bob", Token: "bob-secret"},
		{Name: "carol", Token: "carol-secret", Groups: []string{"dev"}},
	})
	require.NoError(t, err)
	k, err := auth.GenerateKey()
	require.NoError(t, err)
	issuer, err := auth.NewIssuer("cr", "registry.test", time.Minute, k)
	require.NoError(t, err)

	g := &auth.Guard{Authenticator: auth.Chain{tokens}, Policy: st, Issuer: issuer}
	return &guarded{
		harness: &harness{t: t, h: registry.New(registry.Config{
			Stores: flob.NewMemStores(),
			Index:  memindex.New(),
			Guard:  g,
		})},
		guard: g,
	}
}

func basic(user string) string {
	req := httptest.NewRequest("GET", "/", nil)
	req.SetBasicAuth(user, user+"-secret")
	return req.Header.Get("Authorization")
}

func (x *guarded) token(user string, scopes ...string) string {
	q := url.Values{"service": {"registry.test"}, "scope": scopes}
	req := httptest.NewRequest("GET", "/token?"+q.Encode(), nil)
	if user != "" {
		req.SetBasicAuth(user, user+"-secret")
	}
	w := httptest.NewRecorder()
	x.guard.ServeToken(w, req)
	require.Equal(x.t, http.StatusOK, w.Code)
	var v struct {
		Token string `json:"token"`
	}
	require.NoError(x.t, json.Unmarshal(w.Body.Bytes(), &v))
	return "Bearer " + v.Token
}

func TestGuardBase(t *testing.T) {
	x := newGuarded(t)

	res := x.do("GET", "/v2/", nil)
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	require.Equal(t, `Bearer realm="http://example.com/token",service="registry.test"`, res.Header.Get("WWW-Authenticate"))

	res = x.do("GET", "/v2/", nil, "Authorization", basic("alice"))
	require.Equal(t, http.StatusOK, res.StatusCode)

	res = x.do("GET", "/v2/", nil, "Authorization", "Basic "+"bm9ib2R5Om5vcGU=")
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)

	// The token `docker login` gets, with no scope, is a credential too.
	res = x.do("GET", "/v2/", nil, "Authorization", x.token("bob"))
	require.Equal(t, http.StatusOK, res.StatusCode)
}

func TestGuardPush(t *testing.T) {
	x := newGuarded(t)
	blob := []byte("layer")
	d := digest.FromBytes(blob)
	path := "/v2/acme/app/blobs/uploads/?digest=" + d.String()

	// Nobody: sent for a token.
	res := x.do("POST", path, blob)
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	require.Contains(t, res.Header.Get("WWW-Authenticate"), `scope="repository:acme/app:pull,push"`)

	// Somebody with no binding: refused.
	res = x.do("POST", path, blob, "Authorization", basic("bob"))
	require.Equal(t, http.StatusForbidden, res.StatusCode)
	require.Equal(t, "DENIED", code(t, res))

	// The same through the token flow: the scope was asked and nothing granted.
	res = x.do("POST", path, blob, "Authorization", x.token("bob", "repository:acme/app:pull,push"))
	require.Equal(t, http.StatusForbidden, res.StatusCode)

	// A token that was never asked about this repository is sent for one.
	res = x.do("POST", path, blob, "Authorization", x.token("alice"))
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	require.Contains(t, res.Header.Get("WWW-Authenticate"), `error="insufficient_scope"`)

	// And so is one asked for pull alone, which is how clients start a push.
	res = x.do("POST", path, blob, "Authorization", x.token("alice", "repository:acme/app:pull"))
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	require.Contains(t, res.Header.Get("WWW-Authenticate"), `error="insufficient_scope"`)

	// Nobody with a token that asked is told to authenticate, as nobody
	// without one is, and not to go round for another token.
	res = x.do("POST", path, blob, "Authorization", x.token("", "repository:acme/app:pull,push"))
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	require.NotContains(t, res.Header.Get("WWW-Authenticate"), `insufficient_scope`)

	res = x.do("POST", path, blob, "Authorization", x.token("alice", "repository:acme/app:pull,push"))
	require.Equal(t, http.StatusCreated, res.StatusCode)
	res = x.do("HEAD", "/v2/acme/app/blobs/"+d.String(), nil, "Authorization", basic("alice"))
	require.Equal(t, http.StatusOK, res.StatusCode)
}

func TestGuardAnonymousPull(t *testing.T) {
	x := newGuarded(t)
	d := digest.FromBytes([]byte("public layer"))
	res := x.do("POST", "/v2/public/app/blobs/uploads/?digest="+d.String(), []byte("public layer"), "Authorization", basic("alice"))
	require.Equal(t, http.StatusCreated, res.StatusCode)

	res = x.do("GET", "/v2/public/app/blobs/"+d.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)

	res = x.do("POST", "/v2/public/app/blobs/uploads/", nil)
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
}

// pushAs pushes an image under tag as whoever authorization says.
func (x *guarded) pushAs(authorization, repo, tag, layer string) *http.Response {
	x.t.Helper()
	config := []byte(`{"layer":"` + layer + `"}`)
	for _, b := range [][]byte{config, []byte(layer)} {
		res := x.do("POST", "/v2/"+repo+"/blobs/uploads/?digest="+digest.FromBytes(b).String(), b, "Authorization", authorization)
		require.Equal(x.t, http.StatusCreated, res.StatusCode)
	}
	m := v1.Manifest{
		MediaType: v1.MediaTypeImageManifest,
		Config:    descriptor(v1.MediaTypeImageConfig, config),
		Layers:    []v1.Descriptor{descriptor(v1.MediaTypeImageLayerGzip, []byte(layer))},
	}
	m.SchemaVersion = 2
	b, _ := json.Marshal(m)
	return x.do("PUT", "/v2/"+repo+"/manifests/"+tag, b, "Content-Type", v1.MediaTypeImageManifest, "Authorization", authorization)
}

func TestGuardTagRules(t *testing.T) {
	x := newGuarded(t)
	alice := basic("alice")

	require.Equal(t, http.StatusCreated, x.pushAs(alice, "acme/app", "v1.0.0", "one").StatusCode)
	// Pushing the same thing again is not a move.
	require.Equal(t, http.StatusCreated, x.pushAs(alice, "acme/app", "v1.0.0", "one").StatusCode)

	res := x.pushAs(alice, "acme/app", "v1.0.0", "two")
	require.Equal(t, http.StatusForbidden, res.StatusCode)
	require.Equal(t, "DENIED", code(t, res))

	res = x.do("GET", "/v2/acme/app/manifests/v1.0.0", nil, "Authorization", alice)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.JSONEq(t, `{"layer":"one"}`, string(read(t, x.do("GET", "/v2/acme/app/blobs/"+digest.FromBytes([]byte(`{"layer":"one"}`)).String(), nil, "Authorization", alice))))

	res = x.do("DELETE", "/v2/acme/app/manifests/v1.0.0", nil, "Authorization", alice)
	require.Equal(t, http.StatusForbidden, res.StatusCode)

	// Deleting by digest would take the immutable tag with it.
	d := res.Header.Get("Docker-Content-Digest")
	_ = d
	res = x.do("HEAD", "/v2/acme/app/manifests/v1.0.0", nil, "Authorization", alice)
	res = x.do("DELETE", "/v2/acme/app/manifests/"+res.Header.Get("Docker-Content-Digest"), nil, "Authorization", alice)
	require.Equal(t, http.StatusForbidden, res.StatusCode)

	// A pattern, for a group binding.
	carol := basic("carol")
	require.Equal(t, http.StatusCreated, x.pushAs(carol, "team/app", "v2.0.0", "three").StatusCode)
	res = x.pushAs(carol, "team/app", "nightly", "four")
	require.Equal(t, http.StatusForbidden, res.StatusCode)
	require.Contains(t, string(read(t, res)), "does not match")
}

func TestGuardCatalog(t *testing.T) {
	x := newGuarded(t)
	alice := basic("alice")
	require.Equal(t, http.StatusCreated, x.pushAs(alice, "public/app", "latest", "a").StatusCode)
	require.Equal(t, http.StatusCreated, x.pushAs(alice, "acme/secret", "latest", "b").StatusCode)

	res := x.do("GET", "/v2/_catalog", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.JSONEq(t, `{"repositories":["public/app"]}`, string(read(t, res)))

	res = x.do("GET", "/v2/_catalog", nil, "Authorization", alice)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.JSONEq(t, `{"repositories":["acme/secret","public/app"]}`, string(read(t, res)))

	res = x.do("GET", "/v2/_catalog?n=1", nil)
	require.JSONEq(t, `{"repositories":["public/app"]}`, string(read(t, res)))
}

func TestGuardMountNeedsPullOnTheSource(t *testing.T) {
	x := newGuarded(t)
	alice := basic("alice")
	d := digest.FromBytes([]byte("secret layer"))
	res := x.do("POST", "/v2/acme/secret/blobs/uploads/?digest="+d.String(), []byte("secret layer"), "Authorization", alice)
	require.Equal(t, http.StatusCreated, res.StatusCode)

	carol := basic("carol")
	res = x.do("POST", "/v2/team/app/blobs/uploads/?mount="+d.String()+"&from=acme/secret", nil, "Authorization", carol)
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	require.True(t, strings.HasPrefix(res.Header.Get("Location"), "/v2/team/app/blobs/uploads/"))
}
