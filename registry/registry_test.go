package registry_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

type harness struct {
	t *testing.T
	h http.Handler
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, h: registry.New(registry.Config{
		Stores: flob.NewMemStores(),
		Index:  memindex.New(),
	})}
}

func (x *harness) do(method, path string, body []byte, header ...string) *http.Response {
	x.t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	x.h.ServeHTTP(w, req)
	return w.Result()
}

func read(t *testing.T, res *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return b
}

func code(t *testing.T, res *http.Response) string {
	t.Helper()
	var v struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(read(t, res), &v))
	require.Len(t, v.Errors, 1)
	return v.Errors[0].Code
}

func (x *harness) pushBlob(repo string, b []byte) digest.Digest {
	x.t.Helper()
	d := digest.FromBytes(b)
	res := x.do("POST", "/v2/"+repo+"/blobs/uploads/?digest="+d.String(), b)
	require.Equal(x.t, http.StatusCreated, res.StatusCode)
	return d
}

func descriptor(mt string, b []byte) v1.Descriptor {
	return v1.Descriptor{MediaType: mt, Digest: digest.FromBytes(b), Size: int64(len(b))}
}

// image pushes a config and one layer and answers an image manifest over
// them, not yet pushed.
func (x *harness) image(repo string, layer string) ([]byte, v1.Manifest) {
	x.t.Helper()
	config := []byte(`{"architecture":"amd64","os":"linux","layer":"` + layer + `"}`)
	l := []byte(layer)
	x.pushBlob(repo, config)
	x.pushBlob(repo, l)
	m := v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    descriptor(v1.MediaTypeImageConfig, config),
		Layers:    []v1.Descriptor{descriptor(v1.MediaTypeImageLayerGzip, l)},
	}
	b, err := json.Marshal(m)
	require.NoError(x.t, err)
	return b, m
}

func (x *harness) pushManifest(repo, ref string, b []byte, mt string) *http.Response {
	x.t.Helper()
	return x.do("PUT", "/v2/"+repo+"/manifests/"+ref, b, "Content-Type", mt)
}

func TestBase(t *testing.T) {
	x := newHarness(t)
	res := x.do("GET", "/v2/", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "registry/2.0", res.Header.Get("Docker-Distribution-API-Version"))
}

func TestInvalidName(t *testing.T) {
	x := newHarness(t)
	res := x.do("GET", "/v2/Not_Valid/tags/list", nil)
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	require.Equal(t, "NAME_INVALID", code(t, res))

	res = x.do("GET", "/v2/foo/nothing", nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestMonolithic(t *testing.T) {
	x := newHarness(t)
	b := []byte("hello, registry")
	d := x.pushBlob("acme/app", b)

	res := x.do("HEAD", "/v2/acme/app/blobs/"+d.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "15", res.Header.Get("Content-Length"))
	require.Equal(t, d.String(), res.Header.Get("Docker-Content-Digest"))

	res = x.do("GET", "/v2/acme/app/blobs/"+d.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, b, read(t, res))

	res = x.do("GET", "/v2/acme/app/blobs/"+d.String(), nil, "Range", "bytes=7-14")
	require.Equal(t, http.StatusPartialContent, res.StatusCode)
	require.Equal(t, []byte("registry"), read(t, res))

	// Another repository does not see it.
	res = x.do("HEAD", "/v2/acme/other/blobs/"+d.String(), nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)

	// Pushed twice is still created.
	x.pushBlob("acme/app", b)

	res = x.do("POST", "/v2/acme/app/blobs/uploads/?digest="+digest.FromString("other").String(), b)
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	require.Equal(t, "DIGEST_INVALID", code(t, res))
}

func TestChunked(t *testing.T) {
	x := newHarness(t)
	b := []byte("0123456789abcdefghij")
	d := digest.FromBytes(b)

	res := x.do("POST", "/v2/acme/app/blobs/uploads/", nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	loc := res.Header.Get("Location")
	require.True(t, strings.HasPrefix(loc, "/v2/acme/app/blobs/uploads/"))

	res = x.do("PATCH", loc, b[:10], "Content-Range", "0-9", "Content-Type", "application/octet-stream")
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	require.Equal(t, "0-9", res.Header.Get("Range"))

	// Out of order.
	res = x.do("PATCH", loc, b[15:], "Content-Range", "15-19")
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, res.StatusCode)
	require.Equal(t, "0-9", res.Header.Get("Range"))

	res = x.do("GET", loc, nil)
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	require.Equal(t, "0-9", res.Header.Get("Range"))

	// Streamed, without a range.
	res = x.do("PATCH", loc, b[10:15])
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	require.Equal(t, "0-14", res.Header.Get("Range"))

	// The last chunk rides on the PUT.
	res = x.do("PUT", loc+"?digest="+d.String(), b[15:], "Content-Range", "15-19")
	require.Equal(t, http.StatusCreated, res.StatusCode)
	require.Equal(t, "/v2/acme/app/blobs/"+d.String(), res.Header.Get("Location"))

	res = x.do("GET", "/v2/acme/app/blobs/"+d.String(), nil)
	require.Equal(t, b, read(t, res))

	res = x.do("GET", loc, nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	require.Equal(t, "BLOB_UPLOAD_UNKNOWN", code(t, res))
}

func TestChunkedDigestMismatch(t *testing.T) {
	x := newHarness(t)
	res := x.do("POST", "/v2/acme/app/blobs/uploads/", nil)
	loc := res.Header.Get("Location")
	res = x.do("PUT", loc+"?digest="+digest.FromString("nope").String(), []byte("yes"))
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	require.Equal(t, "DIGEST_INVALID", code(t, res))
}

func TestCancel(t *testing.T) {
	x := newHarness(t)
	res := x.do("POST", "/v2/acme/app/blobs/uploads/", nil)
	loc := res.Header.Get("Location")

	res = x.do("DELETE", loc, nil)
	require.Equal(t, http.StatusNoContent, res.StatusCode)

	res = x.do("PATCH", loc, []byte("late"))
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	require.Equal(t, "BLOB_UPLOAD_UNKNOWN", code(t, res))

	res = x.do("GET", "/v2/acme/app/blobs/uploads/00000000000000000000000000000000", nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestManifest(t *testing.T) {
	x := newHarness(t)
	b, _ := x.image("acme/app", "layer")
	d := digest.FromBytes(b)

	res := x.pushManifest("acme/app", "latest", b, v1.MediaTypeImageManifest)
	require.Equal(t, http.StatusCreated, res.StatusCode)
	require.Equal(t, d.String(), res.Header.Get("Docker-Content-Digest"))
	require.Equal(t, "/v2/acme/app/manifests/"+d.String(), res.Header.Get("Location"))

	for _, ref := range []string{"latest", d.String()} {
		res = x.do("GET", "/v2/acme/app/manifests/"+ref, nil)
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, v1.MediaTypeImageManifest, res.Header.Get("Content-Type"))
		require.Equal(t, d.String(), res.Header.Get("Docker-Content-Digest"))
		require.Equal(t, b, read(t, res))

		res = x.do("HEAD", "/v2/acme/app/manifests/"+ref, nil)
		require.Equal(t, http.StatusOK, res.StatusCode)
	}

	// What was pushed is what is answered, whatever the request accepts: an
	// existing manifest is `200`, with its own type in `Content-Type`.
	for _, accept := range []string{
		v1.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json",
	} {
		res = x.do("GET", "/v2/acme/app/manifests/latest", nil, "Accept", accept)
		require.Equal(t, http.StatusOK, res.StatusCode, accept)
		require.Equal(t, v1.MediaTypeImageManifest, res.Header.Get("Content-Type"))
		require.Equal(t, b, read(t, res))
	}

	res = x.do("GET", "/v2/acme/app/manifests/nope", nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)

	res = x.do("PUT", "/v2/acme/app/manifests/"+digest.FromString("x").String(), b, "Content-Type", v1.MediaTypeImageManifest)
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	require.Equal(t, "DIGEST_INVALID", code(t, res))

	res = x.pushManifest("acme/app", "bad", []byte(`{"schemaVersion":2`), v1.MediaTypeImageManifest)
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	require.Equal(t, "MANIFEST_INVALID", code(t, res))
}

func TestManifestBlobUnknown(t *testing.T) {
	x := newHarness(t)
	b, _ := x.image("acme/app", "layer")

	// The same manifest in a repository that has none of its blobs.
	res := x.pushManifest("acme/other", "latest", b, v1.MediaTypeImageManifest)
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	require.Equal(t, "MANIFEST_BLOB_UNKNOWN", code(t, res))
}

func TestWellKnown(t *testing.T) {
	x := newHarness(t)
	for _, d := range []digest.Digest{blob.EmptyJSON, blob.Empty, blob.EmptyTar, blob.EmptyLayer} {
		want, _ := blob.WellKnown(d)
		res := x.do("GET", "/v2/fresh/blobs/"+d.String(), nil)
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, want, read(t, res))
	}

	// An artifact whose config is the empty descriptor needs no push of it.
	layer := x.pushBlob("acme/art", []byte("payload"))
	m := v1.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    v1.MediaTypeImageManifest,
		ArtifactType: "application/vnd.example",
		Config:       v1.DescriptorEmptyJSON,
		Layers:       []v1.Descriptor{{MediaType: "application/octet-stream", Digest: layer, Size: 7}},
	}
	b, _ := json.Marshal(m)
	res := x.pushManifest("acme/art", "v1", b, v1.MediaTypeImageManifest)
	require.Equal(t, http.StatusCreated, res.StatusCode)
}

func TestTagsList(t *testing.T) {
	x := newHarness(t)
	b, _ := x.image("acme/app", "layer")
	for _, tag := range []string{"c", "a", "b"} {
		res := x.pushManifest("acme/app", tag, b, v1.MediaTypeImageManifest)
		require.Equal(t, http.StatusCreated, res.StatusCode)
	}

	list := func(q string) ([]string, *http.Response) {
		res := x.do("GET", "/v2/acme/app/tags/list"+q, nil)
		require.Equal(t, http.StatusOK, res.StatusCode)
		var v struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		}
		require.NoError(t, json.Unmarshal(read(t, res), &v))
		require.Equal(t, "acme/app", v.Name)
		return v.Tags, res
	}

	tags, _ := list("")
	require.Equal(t, []string{"a", "b", "c"}, tags)

	tags, res := list("?n=2")
	require.Equal(t, []string{"a", "b"}, tags)
	require.Equal(t, `</v2/acme/app/tags/list?last=b&n=2>; rel="next"`, res.Header.Get("Link"))

	tags, res = list("?n=2&last=b")
	require.Equal(t, []string{"c"}, tags)
	require.Empty(t, res.Header.Get("Link"))

	res = x.do("GET", "/v2/acme/none/tags/list", nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	require.Equal(t, "NAME_UNKNOWN", code(t, res))

	res = x.do("GET", "/v2/_catalog", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.JSONEq(t, `{"repositories":["acme/app"]}`, string(read(t, res)))
}

func TestReferrers(t *testing.T) {
	x := newHarness(t)
	b, _ := x.image("acme/app", "layer")
	subject := digest.FromBytes(b)
	require.Equal(t, http.StatusCreated, x.pushManifest("acme/app", "latest", b, v1.MediaTypeImageManifest).StatusCode)

	sig := x.pushBlob("acme/app", []byte("signature"))
	for _, at := range []string{"application/vnd.example.sig", "application/vnd.example.sbom"} {
		m := v1.Manifest{
			Versioned:    specs.Versioned{SchemaVersion: 2},
			MediaType:    v1.MediaTypeImageManifest,
			ArtifactType: at,
			Config:       v1.DescriptorEmptyJSON,
			Layers:       []v1.Descriptor{{MediaType: "application/octet-stream", Digest: sig, Size: 9}},
			Subject:      &v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: subject, Size: int64(len(b))},
			Annotations:  map[string]string{"kind": at},
		}
		mb, _ := json.Marshal(m)
		res := x.pushManifest("acme/app", digest.FromBytes(mb).String(), mb, v1.MediaTypeImageManifest)
		require.Equal(t, http.StatusCreated, res.StatusCode)
		require.Equal(t, subject.String(), res.Header.Get("OCI-Subject"))
	}

	get := func(q string) (v1.Index, *http.Response) {
		res := x.do("GET", "/v2/acme/app/referrers/"+subject.String()+q, nil)
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, v1.MediaTypeImageIndex, res.Header.Get("Content-Type"))
		var v v1.Index
		require.NoError(t, json.Unmarshal(read(t, res), &v))
		return v, res
	}

	v, res := get("")
	require.Len(t, v.Manifests, 2)
	require.Empty(t, res.Header.Get("OCI-Filters-Applied"))

	v, res = get("?artifactType=application/vnd.example.sbom")
	require.Len(t, v.Manifests, 1)
	require.Equal(t, "application/vnd.example.sbom", v.Manifests[0].ArtifactType)
	require.Equal(t, "application/vnd.example.sbom", v.Manifests[0].Annotations["kind"])
	require.Equal(t, "artifactType", res.Header.Get("OCI-Filters-Applied"))

	res = x.do("GET", "/v2/acme/app/referrers/"+digest.FromString("nothing").String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.JSONEq(t, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`, string(read(t, res)))
}

func TestDelete(t *testing.T) {
	x := newHarness(t)
	b, m := x.image("acme/app", "layer")
	d := digest.FromBytes(b)
	require.Equal(t, http.StatusCreated, x.pushManifest("acme/app", "latest", b, v1.MediaTypeImageManifest).StatusCode)
	require.Equal(t, http.StatusCreated, x.pushManifest("acme/app", "stable", b, v1.MediaTypeImageManifest).StatusCode)

	// Held by the manifest.
	res := x.do("DELETE", "/v2/acme/app/blobs/"+m.Layers[0].Digest.String(), nil)
	require.Equal(t, http.StatusForbidden, res.StatusCode)
	require.Equal(t, "DENIED", code(t, res))

	res = x.do("DELETE", "/v2/acme/app/manifests/latest", nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	res = x.do("GET", "/v2/acme/app/manifests/latest", nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	res = x.do("GET", "/v2/acme/app/manifests/stable", nil)
	require.Equal(t, http.StatusOK, res.StatusCode)

	// By digest takes the remaining tags with it and releases the blobs.
	res = x.do("DELETE", "/v2/acme/app/manifests/"+d.String(), nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	res = x.do("GET", "/v2/acme/app/manifests/stable", nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	for _, dd := range []digest.Digest{m.Config.Digest, m.Layers[0].Digest, d} {
		res = x.do("HEAD", "/v2/acme/app/blobs/"+dd.String(), nil)
		require.Equal(t, http.StatusNotFound, res.StatusCode, dd)
	}

	res = x.do("DELETE", "/v2/acme/app/manifests/"+d.String(), nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)

	// A blob nothing holds.
	loose := x.pushBlob("acme/app", []byte("loose"))
	res = x.do("DELETE", "/v2/acme/app/blobs/"+loose.String(), nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	res = x.do("HEAD", "/v2/acme/app/blobs/"+loose.String(), nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestDeleteKeepsWhatAnotherManifestHolds(t *testing.T) {
	x := newHarness(t)
	b1, m1 := x.image("acme/app", "shared")
	config := []byte(`{"other":true}`)
	x.pushBlob("acme/app", config)
	m2 := m1
	m2.Config = descriptor(v1.MediaTypeImageConfig, config)
	b2, _ := json.Marshal(m2)

	require.Equal(t, http.StatusCreated, x.pushManifest("acme/app", "one", b1, v1.MediaTypeImageManifest).StatusCode)
	require.Equal(t, http.StatusCreated, x.pushManifest("acme/app", "two", b2, v1.MediaTypeImageManifest).StatusCode)

	res := x.do("DELETE", "/v2/acme/app/manifests/"+digest.FromBytes(b1).String(), nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)

	res = x.do("HEAD", "/v2/acme/app/blobs/"+m1.Layers[0].Digest.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	res = x.do("HEAD", "/v2/acme/app/blobs/"+m1.Config.Digest.String(), nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestIndex(t *testing.T) {
	x := newHarness(t)
	b, _ := x.image("acme/app", "amd64")
	child := digest.FromBytes(b)

	idx := v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{{MediaType: v1.MediaTypeImageManifest, Digest: child, Size: int64(len(b)), Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
	}
	ib, _ := json.Marshal(idx)

	res := x.pushManifest("acme/app", "latest", ib, v1.MediaTypeImageIndex)
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	require.Equal(t, "MANIFEST_BLOB_UNKNOWN", code(t, res))

	require.Equal(t, http.StatusCreated, x.pushManifest("acme/app", child.String(), b, v1.MediaTypeImageManifest).StatusCode)
	require.Equal(t, http.StatusCreated, x.pushManifest("acme/app", "latest", ib, v1.MediaTypeImageIndex).StatusCode)

	// The child is held by the index, so deleting it by digest keeps its bytes
	// until the index goes too.
	res = x.do("DELETE", "/v2/acme/app/manifests/"+child.String(), nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	res = x.do("HEAD", "/v2/acme/app/blobs/"+child.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
}

func TestMount(t *testing.T) {
	x := newHarness(t)
	d := x.pushBlob("acme/a", []byte("mounted"))

	res := x.do("POST", "/v2/acme/b/blobs/uploads/?mount="+d.String()+"&from=acme/a", nil)
	require.Equal(t, http.StatusCreated, res.StatusCode)
	require.Equal(t, "/v2/acme/b/blobs/"+d.String(), res.Header.Get("Location"))
	res = x.do("GET", "/v2/acme/b/blobs/"+d.String(), nil)
	require.Equal(t, []byte("mounted"), read(t, res))

	res = x.do("POST", "/v2/acme/c/blobs/uploads/?mount="+digest.FromString("absent").String()+"&from=acme/a", nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	require.True(t, strings.HasPrefix(res.Header.Get("Location"), "/v2/acme/c/blobs/uploads/"))

	res = x.do("POST", "/v2/acme/c/blobs/uploads/?mount="+d.String(), nil)
	require.Equal(t, http.StatusAccepted, res.StatusCode)
}

func TestChunkedAlgorithmNamedLate(t *testing.T) {
	x := newHarness(t)
	b := []byte("hashed with sha512, said only at the end")
	d := digest.SHA512.FromBytes(b)

	res := x.do("POST", "/v2/acme/app/blobs/uploads/", nil)
	loc := res.Header.Get("Location")
	res = x.do("PATCH", loc, b[:10], "Content-Range", "0-9")
	require.Equal(t, http.StatusAccepted, res.StatusCode)
	res = x.do("PUT", loc+"?digest="+d.String(), b[10:])
	require.Equal(t, http.StatusCreated, res.StatusCode)

	res = x.do("GET", "/v2/acme/app/blobs/"+d.String(), nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, b, read(t, res))

	// Said on the POST, the session hashes with it from the start.
	res = x.do("POST", "/v2/acme/app/blobs/uploads/?digest-algorithm=sha512", nil)
	loc = res.Header.Get("Location")
	res = x.do("PUT", loc+"?digest="+digest.SHA512.FromString("other").String(), []byte("other"))
	require.Equal(t, http.StatusCreated, res.StatusCode)

	// And a digest that is wrong in its own algorithm is still wrong.
	res = x.do("POST", "/v2/acme/app/blobs/uploads/", nil)
	loc = res.Header.Get("Location")
	res = x.do("PUT", loc+"?digest="+digest.SHA512.FromString("not this").String(), []byte("this"))
	require.Equal(t, http.StatusBadRequest, res.StatusCode)
	require.Equal(t, "DIGEST_INVALID", code(t, res))
}

func TestWellKnownDelete(t *testing.T) {
	x := newHarness(t)
	res := x.do("DELETE", "/v2/acme/app/blobs/"+blob.EmptyJSON.String(), nil)
	require.Equal(t, http.StatusMethodNotAllowed, res.StatusCode)
	require.Equal(t, "UNSUPPORTED", code(t, res))
}
