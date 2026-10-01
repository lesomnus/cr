package registry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

// mirror is a registry caching each upstream under its prefix, which is also
// reached by the hosts listed for it.
func mirror(t *testing.T, guard *auth.Guard, ups map[string]*upstream, hosts ...map[string][]string) *harness {
	var ps []*registry.Proxy
	var routes []blob.CacheRoute
	for prefix, up := range ups {
		u, err := blob.NewUpstream(up.srv.URL)
		require.NoError(t, err)
		p := &registry.Proxy{Prefix: prefix, Upstream: u, TagTTL: time.Minute}
		for _, h := range hosts {
			p.Hosts = h[prefix]
		}
		ps = append(ps, p)
		routes = append(routes, blob.CacheRoute{Prefix: prefix, Origin: u.Stores(p.Name)})
	}
	reg := registry.New(registry.Config{
		Stores:  blob.NewCache(flob.NewMemStores(), routes...),
		Index:   memindex.New(),
		Proxies: ps,
		Guard:   guard,
	})
	mux := http.NewServeMux()
	mux.Handle("/v2/", reg)
	if guard != nil {
		guard.Name = reg.Name
		mux.HandleFunc("/token", guard.ServeToken)
	}
	return &harness{t: t, h: mux}
}

// TestProxyByNs: a client that mirrors a registry asks for the name as the
// registry knows it and names the registry in `ns`, and the proxy whose
// prefix that is answers.
func TestProxyByNs(t *testing.T) {
	hub, gh := newUpstream(t, nil), newUpstream(t, nil)
	alpine, m := hub.image("library/alpine", "alpine layer")
	require.Equal(t, http.StatusCreated, hub.pushManifest("library/alpine", "3.20", alpine, v1.MediaTypeImageManifest).StatusCode)
	cr, _ := gh.image("lesomnus/cr", "cr layer")
	require.Equal(t, http.StatusCreated, gh.pushManifest("lesomnus/cr", "edge", cr, v1.MediaTypeImageManifest).StatusCode)

	c := mirror(t, nil, map[string]*upstream{"docker.io": hub, "ghcr.io": gh})

	res := c.do("GET", "/v2/library/alpine/manifests/3.20?ns=docker.io", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, alpine, read(t, res))
	res = c.do("GET", "/v2/library/alpine/blobs/"+m.Layers[0].Digest.String()+"?ns=docker.io", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, []byte("alpine layer"), read(t, res))

	res = c.do("GET", "/v2/lesomnus/cr/manifests/edge?ns=ghcr.io", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, cr, read(t, res))

	// Cached under the prefix: the same as a client that spelled it.
	res = c.do("GET", "/v2/_catalog", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.JSONEq(t, `{"repositories":["docker.io/library/alpine","ghcr.io/lesomnus/cr"]}`, string(read(t, res)))

	// A name a prefix covers is that prefix's, whatever ns says.
	res = c.do("GET", "/v2/ghcr.io/lesomnus/cr/manifests/edge?ns=docker.io", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, cr, read(t, res))

	// No proxy for ns, or no ns: the name is the name.
	require.Equal(t, http.StatusNotFound, c.do("GET", "/v2/library/alpine/manifests/3.20?ns=quay.io", nil).StatusCode)
	require.Equal(t, http.StatusNotFound, c.do("GET", "/v2/library/alpine/manifests/3.20", nil).StatusCode)

	// Still a cache: a write is refused, not put beside the name.
	res = c.do("PUT", "/v2/library/alpine/manifests/mine?ns=docker.io", alpine, "Content-Type", v1.MediaTypeImageManifest)
	require.Equal(t, "UNSUPPORTED", code(t, res))
}

// TestProxyByNsBesideTheEmptyPrefix: ns picks its proxy before the empty
// prefix, which takes what is left only when ns is its own upstream or there
// is none: another registry's name is not its name.
func TestProxyByNsBesideTheEmptyPrefix(t *testing.T) {
	hub, gh := newUpstream(t, nil), newUpstream(t, nil)
	alpine, _ := hub.image("library/alpine", "alpine layer")
	require.Equal(t, http.StatusCreated, hub.pushManifest("library/alpine", "3.20", alpine, v1.MediaTypeImageManifest).StatusCode)
	other, _ := gh.image("library/alpine", "another alpine")
	require.Equal(t, http.StatusCreated, gh.pushManifest("library/alpine", "3.20", other, v1.MediaTypeImageManifest).StatusCode)

	c := mirror(t, nil, map[string]*upstream{"docker.io": hub, "": gh})
	ns := strings.TrimPrefix(gh.srv.URL, "http://")

	res := c.do("GET", "/v2/library/alpine/manifests/3.20?ns=docker.io", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, alpine, read(t, res))

	res = c.do("GET", "/v2/library/alpine/manifests/3.20?ns="+ns, nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, other, read(t, res))

	res = c.do("GET", "/v2/library/alpine/manifests/3.20", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, other, read(t, res))

	// Not the empty prefix's upstream's alpine, cached or not.
	res = c.do("GET", "/v2/library/alpine/manifests/3.20?ns=quay.io", nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	require.Equal(t, "NAME_UNKNOWN", code(t, res))
}

// TestProxyByNsIsGuarded: the policy is asked about the repository ns puts
// the name under, and so is the client told to be.
func TestProxyByNsIsGuarded(t *testing.T) {
	hub, gh := newUpstream(t, nil), newUpstream(t, nil)
	alpine, _ := hub.image("library/alpine", "alpine layer")
	require.Equal(t, http.StatusCreated, hub.pushManifest("library/alpine", "3.20", alpine, v1.MediaTypeImageManifest).StatusCode)
	cr, _ := gh.image("lesomnus/cr", "cr layer")
	require.Equal(t, http.StatusCreated, gh.pushManifest("lesomnus/cr", "edge", cr, v1.MediaTypeImageManifest).StatusCode)

	st := auth.NewPolicyStore(time.Hour, auth.Static{
		Permissions: map[string]auth.Permission{"hub": {Repos: []string{"docker.io/**"}, Actions: []auth.Action{auth.ActionPull}}},
		Matches:     map[string]auth.Match{"hub": {For: auth.Anyone, Grant: []string{"hub"}}},
	})
	require.NoError(t, st.Refresh(context.Background()))
	k, err := auth.GenerateKey()
	require.NoError(t, err)
	issuer, err := auth.NewIssuer("cr", "registry.test", time.Minute, k)
	require.NoError(t, err)

	c := mirror(t, &auth.Guard{Policy: st, Issuer: issuer}, map[string]*upstream{"docker.io": hub, "ghcr.io": gh})

	require.Equal(t, http.StatusOK, c.do("GET", "/v2/library/alpine/manifests/3.20?ns=docker.io", nil).StatusCode)

	res := c.do("GET", "/v2/lesomnus/cr/manifests/edge?ns=ghcr.io", nil)
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	require.Contains(t, res.Header.Get("WWW-Authenticate"), `scope="repository:ghcr.io/lesomnus/cr:pull"`)
}

// TestProxyByHost: a client given a mirror by host, and not saying ns, is
// answered by the proxy that lists the host.
func TestProxyByHost(t *testing.T) {
	hub, gh := newUpstream(t, nil), newUpstream(t, nil)
	alpine, _ := hub.image("library/alpine", "alpine layer")
	require.Equal(t, http.StatusCreated, hub.pushManifest("library/alpine", "3.20", alpine, v1.MediaTypeImageManifest).StatusCode)
	cr, _ := gh.image("lesomnus/cr", "cr layer")
	require.Equal(t, http.StatusCreated, gh.pushManifest("lesomnus/cr", "edge", cr, v1.MediaTypeImageManifest).StatusCode)

	c := mirror(t, nil, map[string]*upstream{"docker.io": hub, "ghcr.io": gh}, map[string][]string{
		"docker.io": {"dockerhub.example.com"},
		"ghcr.io":   {"ghcr.example.com"},
	})
	get := func(host, path string) *http.Response {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = host
		w := httptest.NewRecorder()
		c.h.ServeHTTP(w, req)
		return w.Result()
	}

	res := get("dockerhub.example.com", "/v2/library/alpine/manifests/3.20")
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, alpine, read(t, res))

	// With a port, in whatever case.
	res = get("GHCR.example.com:5000", "/v2/lesomnus/cr/manifests/edge")
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, cr, read(t, res))

	// ns says which registry, over the host it came in on.
	res = get("dockerhub.example.com", "/v2/lesomnus/cr/manifests/edge?ns=ghcr.io")
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, cr, read(t, res))

	// And a prefix in the name over both.
	res = get("dockerhub.example.com", "/v2/ghcr.io/lesomnus/cr/manifests/edge")
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, cr, read(t, res))

	// ns names a registry nothing here caches: not the host's either.
	require.Equal(t, http.StatusNotFound, get("dockerhub.example.com", "/v2/library/alpine/manifests/3.20?ns=quay.io").StatusCode)

	// Another host is not a mirror.
	require.Equal(t, http.StatusNotFound, get("cr.example.com", "/v2/library/alpine/manifests/3.20").StatusCode)
}

// TestProxyByHostWithAToken: a client that sends no ns and fetches a token
// first -- Docker on its own image store, given a mirror -- asks for the name
// it knows, and the token is good for the name the host puts it under.
func TestProxyByHostWithAToken(t *testing.T) {
	hub := newUpstream(t, nil)
	alpine, _ := hub.image("library/alpine", "alpine layer")
	require.Equal(t, http.StatusCreated, hub.pushManifest("library/alpine", "3.20", alpine, v1.MediaTypeImageManifest).StatusCode)

	st := auth.NewPolicyStore(time.Hour, auth.Static{
		Permissions: map[string]auth.Permission{"hub": {Repos: []string{"docker.io/**"}, Actions: []auth.Action{auth.ActionPull}}},
		Matches:     map[string]auth.Match{"hub": {For: auth.Anyone, Grant: []string{"hub"}}},
	})
	require.NoError(t, st.Refresh(context.Background()))
	k, err := auth.GenerateKey()
	require.NoError(t, err)
	issuer, err := auth.NewIssuer("cr", "registry.test", time.Minute, k)
	require.NoError(t, err)

	c := mirror(t, &auth.Guard{Policy: st, Issuer: issuer}, map[string]*upstream{"docker.io": hub}, map[string][]string{
		"docker.io": {"dockerhub.example.com"},
	})
	on := func(path string, header ...string) *http.Response {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "dockerhub.example.com"
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		w := httptest.NewRecorder()
		c.h.ServeHTTP(w, req)
		return w.Result()
	}

	require.Equal(t, http.StatusUnauthorized, on("/v2/").StatusCode)
	res := on("/token?service=registry.test&scope=repository:library/alpine:pull")
	require.Equal(t, http.StatusOK, res.StatusCode)
	var v struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(read(t, res), &v))

	res = on("/v2/library/alpine/manifests/3.20", "Authorization", "Bearer "+v.Token)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, alpine, read(t, res))
}
