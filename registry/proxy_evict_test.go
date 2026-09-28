package registry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/index"
)

// indexOf is an OCI index listing each manifest, all linux/amd64: which
// platform does not matter here, only what the index holds.
func indexOf(t *testing.T, ms ...[]byte) []byte {
	t.Helper()
	ds := make([]v1.Descriptor, 0, len(ms))
	for _, m := range ms {
		ds = append(ds, v1.Descriptor{
			MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(m), Size: int64(len(m)),
			Platform: &v1.Platform{OS: "linux", Architecture: "amd64"},
		})
	}
	b, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: ds})
	require.NoError(t, err)
	return b
}

// pullThrough fetches an index by tag and one child with its layer through the
// cache, the way a client pulls one platform.
func pullThrough(t *testing.T, c *cache, repo, tag string, child []byte, m v1.Manifest) {
	t.Helper()
	require.Equal(t, http.StatusOK, c.do("GET", "/v2/"+repo+"/manifests/"+tag, nil).StatusCode)
	require.Equal(t, http.StatusOK, c.do("GET", "/v2/"+repo+"/manifests/"+digest.FromBytes(child).String(), nil).StatusCode)
	res := c.do("GET", "/v2/"+repo+"/blobs/"+m.Layers[0].Digest.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	read(t, res)
	require.Eventually(t, func() bool { return c.cached(repo, m.Layers[0].Digest) }, 5*time.Second, 10*time.Millisecond)
}

// In a cache a delete evicts, and an index goes with the manifests it brought
// in: the child a client pulled by digest, and the layers only it held. Its
// pull time is not asked -- the child of an image in use was always pulled
// just now.
func TestProxyDeleteEvictsWhatTheIndexBroughtIn(t *testing.T) {
	up := newUpstream(t, nil)
	child, m := up.image("library/app", "app layer")
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", digest.FromBytes(child).String(), child, v1.MediaTypeImageManifest).StatusCode)
	idx := indexOf(t, child)
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", "1", idx, v1.MediaTypeImageIndex).StatusCode)

	ctx := context.Background()
	c := newCache(t, up, "", "")
	const repo = "docker.io/library/app"
	pullThrough(t, c, repo, "1", child, m)

	res := c.do("DELETE", "/v2/"+repo+"/manifests/"+digest.FromBytes(idx).String(), nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)

	_, err := c.ix.Manifest().Get(ctx, repo, digest.FromBytes(child))
	require.ErrorIs(t, err, index.ErrNotFound, "the child outlived the index that brought it in")
	require.Eventually(t, func() bool { return !c.cached(repo, m.Layers[0].Digest) }, 5*time.Second, 10*time.Millisecond,
		"the child's layer is still on disk")

	// Upstream gone: nothing of the image is answered from the cache any more.
	up.srv.Close()
	res = c.do("GET", "/v2/"+repo+"/manifests/"+digest.FromBytes(child).String(), nil)
	require.NotEqual(t, http.StatusOK, res.StatusCode)
}

// A child another index still lists stays, and so does its layer: a release
// re-tagged under a new index shares its platform manifests with the old one.
func TestProxyDeleteKeepsAChildAnotherIndexLists(t *testing.T) {
	up := newUpstream(t, nil)
	child, m := up.image("library/app", "shared layer")
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", digest.FromBytes(child).String(), child, v1.MediaTypeImageManifest).StatusCode)
	older := indexOf(t, child)
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", "1", older, v1.MediaTypeImageIndex).StatusCode)
	other, _ := up.image("library/app", "another platform")
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", digest.FromBytes(other).String(), other, v1.MediaTypeImageManifest).StatusCode)
	newer := indexOf(t, child, other)
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", "2", newer, v1.MediaTypeImageIndex).StatusCode)

	ctx := context.Background()
	c := newCache(t, up, "", "")
	const repo = "docker.io/library/app"
	pullThrough(t, c, repo, "1", child, m)
	require.Equal(t, http.StatusOK, c.do("GET", "/v2/"+repo+"/manifests/2", nil).StatusCode)

	res := c.do("DELETE", "/v2/"+repo+"/manifests/"+digest.FromBytes(older).String(), nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)

	_, err := c.ix.Manifest().Get(ctx, repo, digest.FromBytes(child))
	require.NoError(t, err, "a child the newer index lists was evicted with the older one")
	require.True(t, c.cached(repo, m.Layers[0].Digest))
}

// A child a tag points at is the tag's, and stays.
func TestProxyDeleteKeepsATaggedChild(t *testing.T) {
	up := newUpstream(t, nil)
	child, m := up.image("library/app", "tagged layer")
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", "1-amd64", child, v1.MediaTypeImageManifest).StatusCode)
	idx := indexOf(t, child)
	require.Equal(t, http.StatusCreated, up.pushManifest("library/app", "1", idx, v1.MediaTypeImageIndex).StatusCode)

	ctx := context.Background()
	c := newCache(t, up, "", "")
	const repo = "docker.io/library/app"
	pullThrough(t, c, repo, "1", child, m)
	require.Equal(t, http.StatusOK, c.do("GET", "/v2/"+repo+"/manifests/1-amd64", nil).StatusCode)

	res := c.do("DELETE", "/v2/"+repo+"/manifests/"+digest.FromBytes(idx).String(), nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)

	_, err := c.ix.Manifest().Get(ctx, repo, digest.FromBytes(child))
	require.NoError(t, err, "a tagged child was evicted")
	require.True(t, c.cached(repo, m.Layers[0].Digest))
}

// Outside a cache nothing cascades: a child may be referenced by its own
// digest, and there nothing could fetch it back.
func TestHostedDeleteLeavesTheChildren(t *testing.T) {
	up := newUpstream(t, nil)
	c := newCache(t, up, "", "")
	const repo = "acme/app"
	child, _ := c.image(repo, "hosted layer")
	require.Equal(t, http.StatusCreated, c.pushManifest(repo, digest.FromBytes(child).String(), child, v1.MediaTypeImageManifest).StatusCode)
	idx := indexOf(t, child)
	require.Equal(t, http.StatusCreated, c.pushManifest(repo, "1", idx, v1.MediaTypeImageIndex).StatusCode)

	res := c.do("DELETE", "/v2/"+repo+"/manifests/"+digest.FromBytes(idx).String(), nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)

	_, err := c.ix.Manifest().Get(context.Background(), repo, digest.FromBytes(child))
	require.NoError(t, err, "a hosted repository's child went with its index")
}
