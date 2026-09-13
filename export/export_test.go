package export_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/export"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

type pusher struct {
	t   *testing.T
	reg http.Handler
}

func (p pusher) do(method, path string, body []byte, mt string) {
	p.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if mt != "" {
		req.Header.Set("Content-Type", mt)
	}
	w := httptest.NewRecorder()
	p.reg.ServeHTTP(w, req)
	require.Equal(p.t, http.StatusCreated, w.Code, w.Body.String())
}

func (p pusher) blob(repo string, b []byte, mt string) v1.Descriptor {
	d := digest.FromBytes(b)
	p.do("POST", "/v2/"+repo+"/blobs/uploads/?digest="+d.String(), b, "")
	return v1.Descriptor{MediaType: mt, Digest: d, Size: int64(len(b))}
}

func (p pusher) manifest(repo, ref string, m any, mt string) v1.Descriptor {
	b, _ := json.Marshal(m)
	d := digest.FromBytes(b)
	if ref == "" {
		ref = d.String()
	}
	p.do("PUT", "/v2/"+repo+"/manifests/"+ref, b, mt)
	return v1.Descriptor{MediaType: mt, Digest: d, Size: int64(len(b))}
}

func (p pusher) image(repo, ref, layer string) v1.Descriptor {
	return p.manifest(repo, ref, v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    p.blob(repo, []byte(`{"layer":"`+layer+`"}`), v1.MediaTypeImageConfig),
		Layers:    []v1.Descriptor{p.blob(repo, []byte(layer), v1.MediaTypeImageLayerGzip)},
	}, v1.MediaTypeImageManifest)
}

func TestLayout(t *testing.T) {
	ctx := context.Background()
	const repo = "acme/app"
	stores := flob.NewMemStores()
	ix := memindex.New()
	p := pusher{t: t, reg: registry.New(registry.Config{Stores: stores, Index: ix})}

	amd := p.image(repo, "", "amd64")
	arm := p.image(repo, "", "arm64")
	multi := p.manifest(repo, "multi", v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{amd, arm},
	}, v1.MediaTypeImageIndex)
	single := p.image(repo, "v1", "single")
	sig := p.manifest(repo, "", v1.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    v1.MediaTypeImageManifest,
		ArtifactType: "application/vnd.example.sig",
		Config:       v1.DescriptorEmptyJSON,
		Layers:       []v1.Descriptor{p.blob(repo, []byte("signature"), "application/octet-stream")},
		Subject:      &single,
	}, v1.MediaTypeImageManifest)
	// Not exported: another tag, when only some are asked for.
	other := p.image(repo, "other", "other")

	dir := t.TempDir()
	r, err := export.Layout(ctx, ix, stores.Use(repo), repo, dir, export.Options{Tags: []string{"multi", "v1"}, Referrers: true})
	require.NoError(t, err)
	require.Equal(t, 2, r.Tags)
	require.Equal(t, 5, r.Manifests, "the index, its two children, v1 and its signature")
	require.Empty(t, r.Skipped)

	layout, err := os.ReadFile(filepath.Join(dir, "oci-layout"))
	require.NoError(t, err)
	require.JSONEq(t, `{"imageLayoutVersion":"1.0.0"}`, string(layout))

	var idx v1.Index
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &idx))
	names := map[string]digest.Digest{}
	unnamed := []digest.Digest{}
	for _, m := range idx.Manifests {
		if n, ok := m.Annotations[v1.AnnotationRefName]; ok {
			names[n] = m.Digest
		} else {
			unnamed = append(unnamed, m.Digest)
		}
	}
	require.Equal(t, map[string]digest.Digest{"multi": multi.Digest, "v1": single.Digest}, names)
	require.Equal(t, []digest.Digest{sig.Digest}, unnamed)

	// Every blob is there and is what its name says.
	count := 0
	err = filepath.WalkDir(filepath.Join(dir, "blobs"), func(path string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		d := digest.NewDigestFromEncoded(digest.Algorithm(filepath.Base(filepath.Dir(path))), filepath.Base(path))
		require.Equal(t, d, d.Algorithm().FromBytes(content))
		count++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, r.Manifests+r.Blobs, count)
	_, err = os.Stat(filepath.Join(dir, "blobs", "sha256", other.Digest.Encoded()))
	require.ErrorIs(t, err, os.ErrNotExist)

	// Again, into the same layout, is fine and writes no blob twice.
	r, err = export.Layout(ctx, ix, stores.Use(repo), repo, dir, export.Options{Referrers: true})
	require.NoError(t, err)
	require.Equal(t, 3, r.Tags)
}
