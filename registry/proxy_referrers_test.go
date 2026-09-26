package registry_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

// countingUpstream is an upstream cr that counts the referrers requests and
// the manifest requests it is sent, and answers the referrers ones `404` when
// it has no referrers API.
type countingUpstream struct {
	*harness
	srv       *httptest.Server
	referrers atomic.Int64
	manifests atomic.Int64
	noAPI     atomic.Bool
}

func newCountingUpstream(t *testing.T) *countingUpstream {
	reg := registry.New(registry.Config{Stores: flob.NewMemStores(), Index: memindex.New()})
	u := &countingUpstream{harness: &harness{t: t, h: reg}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			u.manifests.Add(1)
		}
		if strings.Contains(r.URL.Path, "/referrers/") {
			u.referrers.Add(1)
			if u.noAPI.Load() {
				http.NotFound(w, r)
				return
			}
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// sign pushes a referrer of subject to repo, answering its digest.
func (u *countingUpstream) sign(repo string, subject digest.Digest, size int, at, what string) digest.Digest {
	u.t.Helper()
	layer := u.pushBlob(repo, []byte(what))
	m := v1.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    v1.MediaTypeImageManifest,
		ArtifactType: at,
		Config:       v1.DescriptorEmptyJSON,
		Layers:       []v1.Descriptor{{MediaType: "application/octet-stream", Digest: layer, Size: int64(len(what))}},
		Subject:      &v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: subject, Size: int64(size)},
	}
	b, err := json.Marshal(m)
	require.NoError(u.t, err)
	u.pushBlob(repo, []byte("{}"))
	d := digest.FromBytes(b)
	require.Equal(u.t, http.StatusCreated, u.pushManifest(repo, d.String(), b, v1.MediaTypeImageManifest).StatusCode)
	return d
}

type referrersCache struct {
	*harness
	ix    *memindex.Index
	clock *fakeClock
	proxy *registry.Proxy
}

func newReferrersCache(t *testing.T, up *countingUpstream, ix *memindex.Index, clock *fakeClock) *referrersCache {
	u, err := blob.NewUpstream(up.srv.URL, "", "")
	require.NoError(t, err)
	p := &registry.Proxy{
		Prefix:            "docker.io",
		Upstream:          u,
		TagTTL:            time.Minute,
		ReferrersTTL:      time.Minute,
		ReferrersMaxStale: 10 * time.Minute,
	}
	reg := registry.New(registry.Config{
		Stores:  blob.NewCache(flob.NewMemStores(), blob.CacheRoute{Prefix: p.Prefix, Origin: u.Stores(p.Name)}),
		Index:   ix,
		Proxies: []*registry.Proxy{p},
		Now:     clock.Now,
	})
	return &referrersCache{harness: &harness{t: t, h: reg}, ix: ix, clock: clock, proxy: p}
}

func (c *referrersCache) referrers(subject digest.Digest, q string) (*http.Response, []digest.Digest) {
	c.t.Helper()
	res := c.do("GET", "/v2/docker.io/library/app/referrers/"+subject.String()+q, nil)
	if res.StatusCode != http.StatusOK {
		return res, nil
	}
	require.Equal(c.t, v1.MediaTypeImageIndex, res.Header.Get("Content-Type"))
	var v v1.Index
	require.NoError(c.t, json.Unmarshal(read(c.t, res), &v))
	ds := []digest.Digest{}
	for _, m := range v.Manifests {
		ds = append(ds, m.Digest)
	}
	return res, ds
}

func setup(t *testing.T) (*countingUpstream, *referrersCache, digest.Digest, int) {
	up := newCountingUpstream(t)
	body, _ := up.image("library/app", "layer")
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", "latest", body, v1.MediaTypeImageManifest).StatusCode)
	clock := &fakeClock{now: time.Now()}
	ix := memindex.New()
	ix.Now = clock.Now
	return up, newReferrersCache(t, up, ix, clock), digest.FromBytes(body), len(body)
}

func TestProxyReferrersAskTheUpstream(t *testing.T) {
	up, c, subject, size := setup(t)
	sig := up.sign("library/app", subject, size, "application/vnd.example.sig", "sig")
	sbom := up.sign("library/app", subject, size, "application/vnd.example.sbom", "sbom")

	// Nothing of it was ever fetched through the cache, and it is listed.
	res, ds := c.referrers(subject, "")
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.ElementsMatch(t, []digest.Digest{sig, sbom}, ds)
	require.Empty(t, res.Header.Get("OCI-Filters-Applied"))
	require.Empty(t, res.Header.Get("Cr-Stale"))

	// Filtered here, so the header is true whatever the upstream does.
	res, ds = c.referrers(subject, "?artifactType=application/vnd.example.sbom")
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, []digest.Digest{sbom}, ds)
	require.Equal(t, "artifactType", res.Header.Get("OCI-Filters-Applied"))
	require.EqualValues(t, 1, up.referrers.Load(), "one upstream request answers every filter")

	// A referrer is then pulled by digest, the way a verifier does.
	res = c.do("GET", "/v2/docker.io/library/app/manifests/"+sig.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
}

func TestProxyReferrersFollowTheUpstream(t *testing.T) {
	up, c, subject, size := setup(t)
	sig := up.sign("library/app", subject, size, "application/vnd.example.sig", "sig")

	_, ds := c.referrers(subject, "")
	require.Equal(t, []digest.Digest{sig}, ds)
	res := c.do("GET", "/v2/docker.io/library/app/manifests/"+sig.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)

	// Added upstream: not seen within the TTL, seen after it.
	sig2 := up.sign("library/app", subject, size, "application/vnd.example.sig", "sig2")
	_, ds = c.referrers(subject, "")
	require.Equal(t, []digest.Digest{sig}, ds)
	c.clock.Add(2 * time.Minute)
	res, ds = c.referrers(subject, "")
	require.ElementsMatch(t, []digest.Digest{sig, sig2}, ds)
	require.Empty(t, res.Header.Get("Age"), "just checked")

	// Removed upstream: gone from the list, although the cache holds it.
	// Merging with what is here would bring a revoked signature back.
	require.Equal(t, http.StatusAccepted, up.do("DELETE", "/v2/library/app/manifests/"+sig.String(), nil).StatusCode)
	c.clock.Add(2 * time.Minute)
	_, ds = c.referrers(subject, "")
	require.Equal(t, []digest.Digest{sig2}, ds)
}

func TestProxyReferrersStale(t *testing.T) {
	up, c, subject, size := setup(t)
	sig := up.sign("library/app", subject, size, "application/vnd.example.sig", "sig")
	_, ds := c.referrers(subject, "")
	require.Equal(t, []digest.Digest{sig}, ds)

	up.srv.Close()

	// Within the bound: what the upstream last said, marked as such.
	c.clock.Add(5 * time.Minute)
	res, ds := c.referrers(subject, "")
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, []digest.Digest{sig}, ds)
	require.Equal(t, "true", res.Header.Get("Cr-Stale"))
	require.Equal(t, strconv.Itoa(5*60), res.Header.Get("Age"))

	// Past it: a failure, and not a list nobody has checked.
	c.clock.Add(10 * time.Minute)
	res, _ = c.referrers(subject, "")
	require.Equal(t, http.StatusGatewayTimeout, res.StatusCode)

	// Never observed at all: a failure too, never an empty list and never
	// the 404 that would send a client to the tag schema.
	res, _ = c.referrers(digest.FromString("other"), "")
	require.Equal(t, http.StatusGatewayTimeout, res.StatusCode)
}

func TestProxyReferrersWithoutTheAPI(t *testing.T) {
	up, c, subject, size := setup(t)
	up.sign("library/app", subject, size, "application/vnd.example.sig", "sig")
	up.noAPI.Store(true)

	res, _ := c.referrers(subject, "")
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	res, _ = c.referrers(subject, "")
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	require.EqualValues(t, 1, up.referrers.Load(), "the answer is remembered for the TTL")

	// The tag schema a client falls back to is a tag like any other.
	up.noAPI.Store(false)
	c.clock.Add(2 * time.Minute)
	res, _ = c.referrers(subject, "")
	require.Equal(t, http.StatusOK, res.StatusCode)
}

func TestProxyReferrersSharedBetweenReplicas(t *testing.T) {
	up, c, subject, size := setup(t)
	sig := up.sign("library/app", subject, size, "application/vnd.example.sig", "sig")
	_, ds := c.referrers(subject, "")
	require.Equal(t, []digest.Digest{sig}, ds)
	require.EqualValues(t, 1, up.referrers.Load())

	// Another replica over the same index, or this one restarted: the check
	// one of them made is the check, so the upstream is asked once per TTL
	// however many there are.
	other := newReferrersCache(t, up, c.ix, c.clock)
	_, ds = other.referrers(subject, "")
	require.Equal(t, []digest.Digest{sig}, ds)
	require.EqualValues(t, 1, up.referrers.Load())

	c.clock.Add(2 * time.Minute)
	_, _ = other.referrers(subject, "")
	require.EqualValues(t, 2, up.referrers.Load())
	_, _ = c.referrers(subject, "")
	require.EqualValues(t, 2, up.referrers.Load(), "checked by the other replica")

	// And what either checked is what a new one falls back on.
	up.srv.Close()
	c.clock.Add(2 * time.Minute)
	again := newReferrersCache(t, up, c.ix, c.clock)
	res, ds := again.referrers(subject, "")
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, []digest.Digest{sig}, ds)
	require.Equal(t, "true", res.Header.Get("Cr-Stale"))
}

func TestProxyTagsSharedBetweenReplicas(t *testing.T) {
	up, c, _, _ := setup(t)
	get := func(c *referrersCache) int {
		return c.do("GET", "/v2/docker.io/library/app/manifests/latest", nil).StatusCode
	}

	require.Equal(t, http.StatusOK, get(c))
	asked := up.manifests.Load()

	// Within the TTL no replica asks, whichever made the check.
	other := newReferrersCache(t, up, c.ix, c.clock)
	require.Equal(t, http.StatusOK, get(other))
	require.Equal(t, asked, up.manifests.Load())

	// Past it one asks, and the other has nothing to ask.
	c.clock.Add(2 * time.Minute)
	require.Equal(t, http.StatusOK, get(other))
	require.Equal(t, asked+1, up.manifests.Load(), "one HEAD")
	require.Equal(t, http.StatusOK, get(c))
	require.Equal(t, asked+1, up.manifests.Load())
}

func TestProxyTagMaxStale(t *testing.T) {
	up, c, _, _ := setup(t)
	get := func() int {
		return c.do("GET", "/v2/docker.io/library/app/manifests/latest", nil).StatusCode
	}
	require.Equal(t, http.StatusOK, get())
	up.srv.Close()

	// A bound, when one is asked for, is measured from the last check.
	c.proxy.TagMaxStale = 10 * time.Minute
	c.clock.Add(5 * time.Minute)
	require.Equal(t, http.StatusOK, get(), "within the bound")
	c.clock.Add(10 * time.Minute)
	require.Equal(t, http.StatusGatewayTimeout, get(), "past it")

	// No bound, the default: served for as long as the upstream is down.
	c.proxy.TagMaxStale = 0
	c.clock.Add(24 * time.Hour)
	require.Equal(t, http.StatusOK, get())
}
