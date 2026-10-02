package registry_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	"github.com/lesomnus/cr/trust"
	"github.com/lesomnus/cr/trust/trusttest"
)

// signingUpstream is an upstream cr a release is published to and signed on,
// that can be made to fail in the ways a verifier has to tell apart.
type signingUpstream struct {
	*harness
	srv *httptest.Server

	// noAPI answers referrers requests 404, as a registry without the API.
	noAPI atomic.Bool
	// referrersFail answers referrers requests 500.
	referrersFail atomic.Bool
	// down drops every connection: an upstream nobody can reach.
	down atomic.Bool
}

func newSigningUpstream(t *testing.T) *signingUpstream {
	reg := registry.New(registry.Config{Stores: flob.NewMemStores(), Index: memindex.New()})
	u := &signingUpstream{harness: &harness{t: t, h: reg}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u.down.Load() {
			panic(http.ErrAbortHandler)
		}
		if strings.Contains(r.URL.Path, "/referrers/") {
			switch {
			case u.noAPI.Load():
				http.NotFound(w, r)
				return
			case u.referrersFail.Load():
				http.Error(w, "broken", http.StatusInternalServerError)
				return
			}
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// release publishes an index of one platform under tag, answering both.
func (u *signingUpstream) release(repo, tag, layer string) (index, platform v1.Descriptor) {
	u.t.Helper()
	body, _ := u.image(repo, layer)
	platform = descriptor(v1.MediaTypeImageManifest, body)
	require.Equal(u.t, http.StatusCreated, u.pushManifest(repo, platform.Digest.String(), body, v1.MediaTypeImageManifest).StatusCode)

	p := platform
	p.Platform = &v1.Platform{Architecture: "amd64", OS: "linux"}
	idx, err := json.Marshal(v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{p},
	})
	require.NoError(u.t, err)
	index = descriptor(v1.MediaTypeImageIndex, idx)
	require.Equal(u.t, http.StatusCreated, u.pushManifest(repo, tag, idx, v1.MediaTypeImageIndex).StatusCode)
	return index, platform
}

// sign pushes a notation signature of subject by s, as `notation sign` does,
// answering the signature manifest's descriptor.
func (u *signingUpstream) sign(repo string, subject v1.Descriptor, s *trusttest.Signer, mt string) v1.Descriptor {
	u.t.Helper()
	env := s.Sign(u.t, subject, mt)
	u.pushBlob(repo, env)
	u.pushBlob(repo, []byte("{}"))
	sub := subject
	m, err := json.Marshal(v1.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    v1.MediaTypeImageManifest,
		ArtifactType: trust.ArtifactType,
		Config:       v1.DescriptorEmptyJSON,
		Layers:       []v1.Descriptor{descriptor(mt, env)},
		Subject:      &sub,
	})
	require.NoError(u.t, err)
	sig := descriptor(v1.MediaTypeImageManifest, m)
	sig.ArtifactType = trust.ArtifactType
	require.Equal(u.t, http.StatusCreated, u.pushManifest(repo, sig.Digest.String(), m, v1.MediaTypeImageManifest).StatusCode)
	return sig
}

// tagReferrers publishes the referrers tag schema's index for subject: what a
// registry without the API is given beside a signature.
func (u *signingUpstream) tagReferrers(repo string, subject digest.Digest, refs ...v1.Descriptor) {
	u.t.Helper()
	b, err := json.Marshal(v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: refs,
	})
	require.NoError(u.t, err)
	tag := subject.Algorithm().String() + "-" + subject.Encoded()
	require.Equal(u.t, http.StatusCreated, u.pushManifest(repo, tag, b, v1.MediaTypeImageIndex).StatusCode)
}

type verifyingCache struct {
	*harness
	clock *fakeClock
}

const verifyRepo = "dist/hday/app"

// newVerifyingCache is a cache of up under `dist` that trusts ca, sharing ix
// and stores with whatever else is given them: a cache restarted is one made
// again over the same.
func newVerifyingCache(t *testing.T, up *signingUpstream, ca *trusttest.CA, mode trust.Mode, ix *memindex.Index, stores flob.Stores, clock *fakeClock) *verifyingCache {
	u, err := blob.NewUpstream(up.srv.URL)
	require.NoError(t, err)
	v, err := trust.New(trust.Config{Mode: mode, Roots: []string{ca.RootFile(t)}, Identities: []string{trusttest.SubjectDN}})
	require.NoError(t, err)
	p := &registry.Proxy{
		Prefix:            "dist",
		Remote:            "dist",
		Upstream:          u,
		TagTTL:            time.Minute,
		ReferrersTTL:      time.Minute,
		ReferrersMaxStale: 10 * time.Minute,
		Verify:            v,
	}
	reg := registry.New(registry.Config{
		Stores:  blob.NewCache(stores, blob.CacheRoute{Prefix: p.Prefix, Origin: u.Stores(p.Name)}),
		Index:   ix,
		Proxies: []*registry.Proxy{p},
		Now:     clock.Now,
	})
	return &verifyingCache{harness: &harness{t: t, h: reg}, clock: clock}
}

// get is a pull's request for ref in the release's repository, accepting what
// docker accepts.
func (c *verifyingCache) get(method, ref string) *http.Response {
	c.t.Helper()
	return c.do(method, "/v2/"+verifyRepo+"/manifests/"+ref, nil,
		"Accept", strings.Join([]string{v1.MediaTypeImageIndex, v1.MediaTypeImageManifest}, ", "))
}

// refusal is the error a refused request was answered with.
func refusal(t *testing.T, res *http.Response) (code, message string) {
	t.Helper()
	var v struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(read(t, res), &v))
	require.Len(t, v.Errors, 1)
	return v.Errors[0].Code, v.Errors[0].Message
}

type verifyWorld struct {
	up     *signingUpstream
	ca     *trusttest.CA
	ix     *memindex.Index
	stores flob.Stores
	clock  *fakeClock
}

func newVerifyWorld(t *testing.T) *verifyWorld {
	clock := &fakeClock{now: time.Now()}
	ix := memindex.New()
	ix.Now = clock.Now
	return &verifyWorld{up: newSigningUpstream(t), ca: trusttest.NewCA(t, "Kamino"), ix: ix, stores: flob.NewMemStores(), clock: clock}
}

func (w *verifyWorld) cache(t *testing.T, mode trust.Mode) *verifyingCache {
	return newVerifyingCache(t, w.up, w.ca, mode, w.ix, w.stores, w.clock)
}

const upstreamRepo = "dist/hday/app"

func TestProxyVerifyServesASignedRelease(t *testing.T) {
	w := newVerifyWorld(t)
	index, platform := w.up.release(upstreamRepo, "1", "signed")
	sig := w.up.sign(upstreamRepo, index, w.ca.Signer(t), trust.MediaTypeJWS)
	c := w.cache(t, trust.Require)

	// As docker pulls: the tag resolved, the index, then its one platform
	// by digest, which nobody signed on its own.
	for _, step := range []struct{ method, ref string }{
		{"HEAD", "1"},
		{"GET", "1"},
		{"GET", index.Digest.String()},
		{"GET", platform.Digest.String()},
		{"HEAD", platform.Digest.String()},
	} {
		res := c.get(step.method, step.ref)
		require.Equal(t, http.StatusOK, res.StatusCode, "%s %s", step.method, step.ref)
	}

	// The signature is read by whoever verifies on their own, and runs
	// nowhere: it is served unsigned.
	require.Equal(t, http.StatusOK, c.get("GET", sig.Digest.String()).StatusCode)

	// Either envelope.
	index2, _ := w.up.release(upstreamRepo, "2", "cose")
	w.up.sign(upstreamRepo, index2, w.ca.Signer(t), trust.MediaTypeCOSE)
	require.Equal(t, http.StatusOK, c.get("GET", "2").StatusCode)
}

func TestProxyVerifyRefuses(t *testing.T) {
	w := newVerifyWorld(t)
	c := w.cache(t, trust.Require)

	t.Run("a release nobody signed", func(t *testing.T) {
		_, platform := w.up.release(upstreamRepo, "unsigned", "unsigned")
		res := c.get("GET", "unsigned")
		require.Equal(t, http.StatusForbidden, res.StatusCode)
		code, msg := refusal(t, res)
		require.Equal(t, "DENIED", code)
		require.Contains(t, msg, "is not signed by anybody this registry trusts: not signed")

		// Nor its platform, asked for on its own.
		require.Equal(t, http.StatusForbidden, c.get("GET", platform.Digest.String()).StatusCode)
	})
	t.Run("a release signed by somebody the roots did not issue", func(t *testing.T) {
		index, _ := w.up.release(upstreamRepo, "elsewhere", "elsewhere")
		w.up.sign(upstreamRepo, index, trusttest.NewCA(t, "Elsewhere").Signer(t), trust.MediaTypeJWS)
		res := c.get("GET", "elsewhere")
		require.Equal(t, http.StatusForbidden, res.StatusCode)
		_, msg := refusal(t, res)
		require.Contains(t, msg, "no signature verifies")
	})
	t.Run("a release whose signature is of another", func(t *testing.T) {
		index, _ := w.up.release(upstreamRepo, "borrowed", "borrowed")
		other, _ := w.up.release(upstreamRepo, "other", "other")
		// A signature of `other`, listed as `borrowed`'s: its subject is
		// borrowed, its payload is not.
		env := w.ca.Signer(t).Sign(t, other, trust.MediaTypeJWS)
		w.up.pushBlob(upstreamRepo, env)
		w.up.pushBlob(upstreamRepo, []byte("{}"))
		m, err := json.Marshal(v1.Manifest{
			Versioned:    specs.Versioned{SchemaVersion: 2},
			MediaType:    v1.MediaTypeImageManifest,
			ArtifactType: trust.ArtifactType,
			Config:       v1.DescriptorEmptyJSON,
			Layers:       []v1.Descriptor{descriptor(trust.MediaTypeJWS, env)},
			Subject:      &index,
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, w.up.pushManifest(upstreamRepo, digest.FromBytes(m).String(), m, v1.MediaTypeImageManifest).StatusCode)
		require.Equal(t, http.StatusForbidden, c.get("GET", "borrowed").StatusCode)
	})
}

func TestProxyVerifyAudit(t *testing.T) {
	w := newVerifyWorld(t)
	w.up.release(upstreamRepo, "unsigned", "unsigned")
	c := w.cache(t, trust.Audit)
	require.Equal(t, http.StatusOK, c.get("GET", "unsigned").StatusCode)
}

func TestProxyVerifyCannotCheck(t *testing.T) {
	w := newVerifyWorld(t)
	index, _ := w.up.release(upstreamRepo, "1", "signed")
	w.up.sign(upstreamRepo, index, w.ca.Signer(t), trust.MediaTypeJWS)
	c := w.cache(t, trust.Require)

	// The manifest comes; the list of its signatures does not. That is the
	// upstream's failure, not the release's, and is answered as one.
	w.up.referrersFail.Store(true)
	res := c.get("GET", "1")
	require.Equal(t, http.StatusBadGateway, res.StatusCode)
	code, msg := refusal(t, res)
	require.NotEqual(t, "DENIED", code)
	require.Contains(t, msg, "could not check its signature")

	// Answered once it is back.
	w.up.referrersFail.Store(false)
	require.Equal(t, http.StatusOK, c.get("GET", "1").StatusCode)
}

func TestProxyVerifyOffline(t *testing.T) {
	w := newVerifyWorld(t)
	index, platform := w.up.release(upstreamRepo, "1", "signed")
	w.up.sign(upstreamRepo, index, w.ca.Signer(t), trust.MediaTypeJWS)
	unsigned, _ := w.up.release(upstreamRepo, "unsigned", "unsigned")

	c := w.cache(t, trust.Require)
	require.Equal(t, http.StatusOK, c.get("GET", "1").StatusCode)
	require.Equal(t, http.StatusOK, c.get("GET", platform.Digest.String()).StatusCode)
	require.Equal(t, http.StatusForbidden, c.get("GET", "unsigned").StatusCode)

	// A day without the upstream, well past referrers_max_stale, and the
	// cache restarted, so nothing it decided is remembered: what it read is
	// read again, and decides the same.
	w.up.down.Store(true)
	w.clock.mu.Lock()
	w.clock.now = w.clock.now.Add(24 * time.Hour)
	w.clock.mu.Unlock()
	c = w.cache(t, trust.Require)

	require.Equal(t, http.StatusOK, c.get("GET", index.Digest.String()).StatusCode)
	require.Equal(t, http.StatusOK, c.get("GET", platform.Digest.String()).StatusCode)
	require.Equal(t, http.StatusForbidden, c.get("GET", unsigned.Digest.String()).StatusCode)
}

func TestProxyVerifyRevoked(t *testing.T) {
	w := newVerifyWorld(t)
	index, _ := w.up.release(upstreamRepo, "1", "signed")
	sig := w.up.sign(upstreamRepo, index, w.ca.Signer(t), trust.MediaTypeJWS)
	c := w.cache(t, trust.Require)
	require.Equal(t, http.StatusOK, c.get("GET", "1").StatusCode)

	// The way a signature is withdrawn: removed upstream. Counted until the
	// cache next hears the list, and not after.
	res := w.up.do("DELETE", "/v2/"+upstreamRepo+"/manifests/"+sig.Digest.String(), nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	require.Equal(t, http.StatusOK, c.get("GET", index.Digest.String()).StatusCode)

	w.clock.mu.Lock()
	w.clock.now = w.clock.now.Add(2 * time.Minute)
	w.clock.mu.Unlock()
	res = c.get("GET", index.Digest.String())
	require.Equal(t, http.StatusForbidden, res.StatusCode)
	_, msg := refusal(t, res)
	require.Contains(t, msg, "not signed")
}

func TestProxyVerifyTagSchema(t *testing.T) {
	w := newVerifyWorld(t)
	w.up.noAPI.Store(true)
	index, platform := w.up.release(upstreamRepo, "1", "signed")
	sig := w.up.sign(upstreamRepo, index, w.ca.Signer(t), trust.MediaTypeJWS)
	w.up.tagReferrers(upstreamRepo, index.Digest, sig)
	w.up.release(upstreamRepo, "unsigned", "unsigned")
	c := w.cache(t, trust.Require)

	require.Equal(t, http.StatusOK, c.get("GET", "1").StatusCode)
	require.Equal(t, http.StatusOK, c.get("GET", platform.Digest.String()).StatusCode)
	require.Equal(t, http.StatusForbidden, c.get("GET", "unsigned").StatusCode)

	// The schema's own tag is how a client without the API finds the
	// signature, and is served.
	tag := index.Digest.Algorithm().String() + "-" + index.Digest.Encoded()
	require.Equal(t, http.StatusOK, c.get("GET", tag).StatusCode)
}

func TestProxyVerifyTagSchemaRunsNothing(t *testing.T) {
	// A tag spelled like the schema's is served unsigned only when it is a
	// list of referrers. An image someone tagged so is checked like any.
	w := newVerifyWorld(t)
	_, platform := w.up.release(upstreamRepo, "1", "unsigned")
	tag := platform.Digest.Algorithm().String() + "-" + platform.Digest.Encoded()
	p := platform
	p.Platform = &v1.Platform{Architecture: "amd64", OS: "linux"}
	w.up.tagReferrers(upstreamRepo, platform.Digest, p)
	c := w.cache(t, trust.Require)
	require.Equal(t, http.StatusForbidden, c.get("GET", tag).StatusCode)
}
