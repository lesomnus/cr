package rebuild_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/index/rebuild"
	"github.com/lesomnus/cr/registry"
)

type pusher struct {
	t   *testing.T
	reg http.Handler
}

func (p pusher) blob(repo string, b []byte, mt string) v1.Descriptor {
	d := digest.FromBytes(b)
	req := httptest.NewRequest("POST", "/v2/"+repo+"/blobs/uploads/?digest="+d.String(), bytes.NewReader(b))
	w := httptest.NewRecorder()
	p.reg.ServeHTTP(w, req)
	require.Equal(p.t, http.StatusCreated, w.Code)
	return v1.Descriptor{MediaType: mt, Digest: d, Size: int64(len(b))}
}

func (p pusher) manifest(repo, ref string, m any, mt string) v1.Descriptor {
	b, _ := json.Marshal(m)
	d := digest.FromBytes(b)
	if ref == "" {
		ref = d.String()
	}
	req := httptest.NewRequest("PUT", "/v2/"+repo+"/manifests/"+ref, bytes.NewReader(b))
	req.Header.Set("Content-Type", mt)
	w := httptest.NewRecorder()
	p.reg.ServeHTTP(w, req)
	require.Equal(p.t, http.StatusCreated, w.Code)
	return v1.Descriptor{MediaType: mt, Digest: d, Size: int64(len(b))}
}

func (p pusher) image(repo, ref, layer string) (v1.Descriptor, v1.Descriptor) {
	config := p.blob(repo, []byte(`{"architecture":"amd64","layer":"`+layer+`"}`), v1.MediaTypeImageConfig)
	l := p.blob(repo, []byte(layer), v1.MediaTypeImageLayerGzip)
	return p.manifest(repo, ref, v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    config,
		Layers:    []v1.Descriptor{l},
	}, v1.MediaTypeImageManifest), l
}

func TestRebuild(t *testing.T) {
	ctx := context.Background()
	const repo = "acme/app"
	stores := flob.NewMemStores()
	from := memindex.New()
	p := pusher{t: t, reg: registry.New(registry.Config{Stores: stores, Index: from})}

	image, layer := p.image(repo, "v1", "one")
	p.manifest(repo, "latest", mustManifest(t, stores, repo, image.Digest), v1.MediaTypeImageManifest)
	child, _ := p.image(repo, "", "two")
	multi := p.manifest(repo, "multi", v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{child},
	}, v1.MediaTypeImageIndex)
	sig := p.manifest(repo, "", v1.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    v1.MediaTypeImageManifest,
		ArtifactType: "application/vnd.example.sig",
		Config:       v1.DescriptorEmptyJSON,
		Layers:       []v1.Descriptor{p.blob(repo, []byte("signature"), "application/octet-stream")},
		Subject:      &image,
		Annotations:  map[string]string{"signed": "yes"},
	}, v1.MediaTypeImageManifest)
	// A tag moved away from where it was: its label moves with it.
	p.manifest(repo, "moving", mustManifest(t, stores, repo, image.Digest), v1.MediaTypeImageManifest)
	p.manifest(repo, "moving", mustManifest(t, stores, repo, child.Digest), v1.MediaTypeImageManifest)

	to := memindex.New()
	r, err := rebuild.Rebuild(ctx, stores, to, 0)
	require.NoError(t, err)
	require.Equal(t, 1, r.Repositories)
	require.Equal(t, 4, r.Manifests)
	require.Equal(t, 4, r.Tags)
	require.Empty(t, r.Conflicts)

	for _, d := range []digest.Digest{image.Digest, child.Digest, multi.Digest, sig.Digest} {
		want, err := from.Manifest().Get(ctx, repo, d)
		require.NoError(t, err)
		got, err := to.Manifest().Get(ctx, repo, d)
		require.NoError(t, err)
		require.Equal(t, want.MediaType, got.MediaType)
		require.Equal(t, want.ArtifactType, got.ArtifactType)
		require.Equal(t, want.Subject, got.Subject)
		require.Equal(t, want.Size, got.Size)
		require.Equal(t, want.Annotations, got.Annotations)
	}

	tags, err := from.Tag().All(ctx, repo)
	require.NoError(t, err)
	for _, want := range tags {
		got, err := to.Tag().Get(ctx, repo, want.Name)
		require.NoError(t, err, want.Name)
		require.Equal(t, want.Digest, got.Digest, want.Name)
	}

	held, err := to.Manifest().Holds(ctx, repo, layer.Digest)
	require.NoError(t, err)
	require.True(t, held)
	held, err = to.Manifest().Holds(ctx, repo, child.Digest)
	require.NoError(t, err)
	require.True(t, held)

	refs, err := to.Manifest().Referrers(ctx, repo, image.Digest, "")
	require.NoError(t, err)
	require.Len(t, refs, 1)

	// Again changes nothing.
	r, err = rebuild.Rebuild(ctx, stores, to, 0)
	require.NoError(t, err)
	require.Zero(t, r.Manifests)
	require.Zero(t, r.Tags)

	names, err := to.Repo().List(ctx, index.Page{})
	require.NoError(t, err)
	require.Equal(t, []string{repo}, names)
}

// mustManifest reads back a manifest's bytes, to push them again under a tag.
func mustManifest(t *testing.T, stores flob.Stores, repo string, d digest.Digest) json.RawMessage {
	rc, _, err := stores.Use(repo).Open(context.Background(), flob.Digest(d))
	require.NoError(t, err)
	defer rc.Close()
	var b bytes.Buffer
	_, err = b.ReadFrom(rc)
	require.NoError(t, err)
	return json.RawMessage(b.Bytes())
}
