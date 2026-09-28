package registry_test

import (
	"context"
	"net/http"
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

// mirror is a registry caching each upstream under its prefix.
func mirror(t *testing.T, guard *auth.Guard, ups map[string]*upstream) *harness {
	var ps []*registry.Proxy
	var routes []blob.CacheRoute
	for prefix, up := range ups {
		u, err := blob.NewUpstream(up.srv.URL, "", "")
		require.NoError(t, err)
		p := &registry.Proxy{Prefix: prefix, Upstream: u, TagTTL: time.Minute}
		ps = append(ps, p)
		routes = append(routes, blob.CacheRoute{Prefix: prefix, Origin: u.Stores(p.Name)})
	}
	return &harness{t: t, h: registry.New(registry.Config{
		Stores:  blob.NewCache(flob.NewMemStores(), routes...),
		Index:   memindex.New(),
		Proxies: ps,
		Guard:   guard,
	})}
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
// prefix takes what is left, and the empty prefix still takes it.
func TestProxyByNsBesideTheEmptyPrefix(t *testing.T) {
	hub, gh := newUpstream(t, nil), newUpstream(t, nil)
	alpine, _ := hub.image("library/alpine", "alpine layer")
	require.Equal(t, http.StatusCreated, hub.pushManifest("library/alpine", "3.20", alpine, v1.MediaTypeImageManifest).StatusCode)
	other, _ := gh.image("library/alpine", "another alpine")
	require.Equal(t, http.StatusCreated, gh.pushManifest("library/alpine", "3.20", other, v1.MediaTypeImageManifest).StatusCode)

	c := mirror(t, nil, map[string]*upstream{"docker.io": hub, "": gh})

	res := c.do("GET", "/v2/library/alpine/manifests/3.20?ns=docker.io", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, alpine, read(t, res))

	res = c.do("GET", "/v2/library/alpine/manifests/3.20?ns=ghcr.io", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, other, read(t, res))
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
