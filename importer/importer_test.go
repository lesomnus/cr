package importer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/export"
	"github.com/lesomnus/cr/importer"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

// source is a registry images are pushed to, to be exported as the layouts an
// import reads.
type source struct {
	t      *testing.T
	stores flob.Stores
	ix     *memindex.Index
	reg    http.Handler

	// unnamed is the image of each repository nobody tagged.
	unnamed map[string]v1.Descriptor
}

func newSource(t *testing.T) *source {
	stores := flob.NewMemStores()
	ix := memindex.New()
	return &source{t: t, stores: stores, ix: ix, reg: registry.New(registry.Config{Stores: stores, Index: ix}), unnamed: map[string]v1.Descriptor{}}
}

func (s *source) do(method, path string, body []byte, header ...string) *http.Response {
	s.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	s.reg.ServeHTTP(w, req)
	return w.Result()
}

func (s *source) blob(repo string, b []byte, mt string) v1.Descriptor {
	s.t.Helper()
	d := digest.FromBytes(b)
	res := s.do("POST", "/v2/"+repo+"/blobs/uploads/?digest="+d.String(), b)
	require.Equal(s.t, http.StatusCreated, res.StatusCode)
	return v1.Descriptor{MediaType: mt, Digest: d, Size: int64(len(b))}
}

func (s *source) manifest(repo, tag string, v any, mt string) v1.Descriptor {
	s.t.Helper()
	b, err := json.Marshal(v)
	require.NoError(s.t, err)
	d := digest.FromBytes(b)
	ref := tag
	if ref == "" {
		ref = d.String()
	}
	res := s.do("PUT", "/v2/"+repo+"/manifests/"+ref, b, "Content-Type", mt)
	require.Equal(s.t, http.StatusCreated, res.StatusCode)
	return v1.Descriptor{MediaType: mt, Digest: d, Size: int64(len(b))}
}

func (s *source) image(repo, tag, layer string) v1.Descriptor {
	s.t.Helper()
	return s.manifest(repo, tag, v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    s.blob(repo, []byte(`{"layer":"`+layer+`"}`), v1.MediaTypeImageConfig),
		Layers:    []v1.Descriptor{s.blob(repo, []byte(layer), v1.MediaTypeImageLayerGzip)},
	}, v1.MediaTypeImageManifest)
}

// export writes repo out as a layout at dir.
func (s *source) export(repo, dir string) {
	s.t.Helper()
	_, err := export.Layout(context.Background(), s.ix, s.stores.Use(repo), repo, dir, export.Options{Referrers: true})
	require.NoError(s.t, err)
}

// release is what a repository of the source holds: a two-platform index
// tagged `multi`, an image tagged `v1` with a signature, and an image nobody
// tagged.
type release struct {
	multi, amd, arm, v1, sig, untagged v1.Descriptor
}

func (s *source) release(repo string) release {
	s.t.Helper()
	var r release
	r.amd = s.image(repo, "", repo+" amd64")
	r.arm = s.image(repo, "", repo+" arm64")
	r.multi = s.manifest(repo, "multi", v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{r.amd, r.arm},
	}, v1.MediaTypeImageIndex)
	r.v1 = s.image(repo, "v1", repo+" v1")
	r.sig = s.manifest(repo, "", v1.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    v1.MediaTypeImageManifest,
		ArtifactType: "application/vnd.example.sig",
		Config:       v1.DescriptorEmptyJSON,
		Layers:       []v1.Descriptor{s.blob(repo, []byte(repo+" signature"), "application/octet-stream")},
		Subject:      &r.v1,
	}, v1.MediaTypeImageManifest)
	s.blob(repo, []byte("{}"), v1.MediaTypeEmptyJSON)
	r.untagged = s.image(repo, "", repo+" untagged")
	s.unnamed[repo] = r.untagged
	return r
}

// untagged adds to the layout at dir what the source holds of the image d of
// repo, listed in `index.json` without a name: zot lists what nobody tagged,
// which an export leaves out.
func (s *source) untagged(repo, dir string, d v1.Descriptor) {
	s.t.Helper()
	ctx := context.Background()
	read := func(d digest.Digest) []byte {
		rc, _, err := s.stores.Use(repo).Open(ctx, flob.Digest(d))
		require.NoError(s.t, err)
		defer rc.Close()
		b, err := io.ReadAll(rc)
		require.NoError(s.t, err)
		return b
	}
	write := func(d digest.Digest, b []byte) {
		require.NoError(s.t, os.WriteFile(filepath.Join(dir, "blobs", d.Algorithm().String(), d.Encoded()), b, 0o644))
	}
	b := read(d.Digest)
	write(d.Digest, b)
	var m v1.Manifest
	require.NoError(s.t, json.Unmarshal(b, &m))
	for _, l := range append([]v1.Descriptor{m.Config}, m.Layers...) {
		write(l.Digest, read(l.Digest))
	}

	var idx v1.Index
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	require.NoError(s.t, err)
	require.NoError(s.t, json.Unmarshal(b, &idx))
	idx.Manifests = append(idx.Manifests, d)
	b, err = json.Marshal(idx)
	require.NoError(s.t, err)
	require.NoError(s.t, os.WriteFile(filepath.Join(dir, "index.json"), b, 0o644))
}

// zotRoot lays the source's repositories out as zot keeps its root: a layout
// per repository at its path, one inside another, with the directories zot
// keeps beside them, and what nobody tagged listed too.
func zotRoot(t *testing.T, s *source, repos ...string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "zot")
	for _, repo := range repos {
		dir := filepath.Join(root, repo)
		s.export(repo, dir)
		if d, ok := s.unnamed[repo]; ok {
			s.untagged(repo, dir, d)
		}
		require.NoError(t, os.MkdirAll(filepath.Join(root, repo, ".uploads", "abc"), 0o755))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".sync"), 0o755))
	return root
}

// target is the registry an import goes into.
type target struct {
	root   string
	stores flob.Stores
	ix     *memindex.Index
}

func newTarget(t *testing.T) *target {
	root := filepath.Join(t.TempDir(), "cr")
	return &target{root: root, stores: flob.NewOsStores(root), ix: memindex.New()}
}

func (x *target) run(t *testing.T, src string, o importer.Options) (importer.Report, error) {
	t.Helper()
	return importer.Import(context.Background(), src, x.stores, x.ix, o)
}

func (x *target) shared(d digest.Digest) string {
	v := d.Encoded()
	return filepath.Join(x.root, "share", d.Algorithm().String(), v[0:2], v[2:4], v[4:])
}

func repoReport(t *testing.T, r importer.Report, repo string) importer.RepoReport {
	t.Helper()
	for _, rr := range r.Repositories {
		if rr.Repo == repo {
			return rr
		}
	}
	t.Fatalf("no report for %s in %+v", repo, r)
	return importer.RepoReport{}
}

func TestImportZotRoot(t *testing.T) {
	ctx := context.Background()
	s := newSource(t)
	app := s.release("acme/app")
	tools := s.release("acme/app/tools")
	src := zotRoot(t, s, "acme/app", "acme/app/tools")

	// When the source registry stored it, which the import carries over.
	stored := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	layerPath := filepath.Join(src, "acme/app", "blobs", "sha256", app.v1.Digest.Encoded())
	require.NoError(t, os.Chtimes(layerPath, stored, stored))

	x := newTarget(t)
	r, err := x.run(t, src, importer.Options{})
	require.NoError(t, err)
	require.Len(t, r.Repositories, 2)

	rr := repoReport(t, r, "acme/app")
	require.Equal(t, 6, rr.Manifests, "the index, its two platforms, v1, its signature, and the untagged one")
	require.Equal(t, 2, rr.Tags)
	require.Zero(t, rr.Copied)
	require.Empty(t, rr.Missing)

	for repo, rel := range map[string]release{"acme/app": app, "acme/app/tools": tools} {
		for tag, want := range map[string]digest.Digest{"multi": rel.multi.Digest, "v1": rel.v1.Digest} {
			got, err := x.ix.Tag().Get(ctx, repo, tag)
			require.NoError(t, err)
			require.Equal(t, want, got.Digest, "%s:%s", repo, tag)
		}
		sig, err := x.ix.Manifest().Get(ctx, repo, rel.sig.Digest)
		require.NoError(t, err)
		require.Equal(t, rel.v1.Digest, sig.Subject)
		holders, err := x.ix.Manifest().Holders(ctx, repo, rel.amd.Digest)
		require.NoError(t, err)
		require.Equal(t, []digest.Digest{rel.multi.Digest}, holders)
		_, err = x.ix.Manifest().Get(ctx, repo, rel.untagged.Digest)
		require.NoError(t, err)
	}

	// Taken by link: the store's copy is the source's file.
	src1, err := os.Stat(layerPath)
	require.NoError(t, err)
	dst, err := os.Stat(x.shared(app.v1.Digest))
	require.NoError(t, err)
	require.True(t, os.SameFile(src1, dst))

	// And pushed when the source says.
	m, err := x.ix.Manifest().Get(ctx, "acme/app", app.v1.Digest)
	require.NoError(t, err)
	require.True(t, m.CreatedAt.Equal(stored), "created %s", m.CreatedAt)
	info, err := x.stores.Use("acme/app").Stat(ctx, flob.Digest(app.v1.Digest))
	require.NoError(t, err)
	added, err := info.Added(ctx)
	require.NoError(t, err)
	require.True(t, added.Equal(stored), "added %s", added)

	// It is a registry: a pull of the tag, the platform it lists, and a
	// layer of it.
	reg := registry.New(registry.Config{Stores: x.stores, Index: x.ix})
	get := func(path string) *http.Response {
		w := httptest.NewRecorder()
		reg.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Result()
	}
	require.Equal(t, http.StatusOK, get("/v2/acme/app/manifests/multi").StatusCode)
	res := get("/v2/acme/app/manifests/" + app.amd.Digest.String())
	require.Equal(t, http.StatusOK, res.StatusCode)
	var amd v1.Manifest
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &amd))
	res = get("/v2/acme/app/blobs/" + amd.Layers[0].Digest.String())
	require.Equal(t, http.StatusOK, res.StatusCode)
	layer, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, "acme/app amd64", string(layer))
}

func TestImportAgain(t *testing.T) {
	ctx := context.Background()
	s := newSource(t)
	s.release("acme/app")
	src := zotRoot(t, s, "acme/app")
	x := newTarget(t)
	first, err := x.run(t, src, importer.Options{})
	require.NoError(t, err)

	// Nothing to do the second time.
	r, err := x.run(t, src, importer.Options{})
	require.NoError(t, err)
	rr := repoReport(t, r, "acme/app")
	require.Zero(t, rr.Linked)
	require.Equal(t, repoReport(t, first, "acme/app").Linked, rr.Present)
	require.Zero(t, rr.Manifests)
	require.Zero(t, rr.Tags)

	// A tag the source moved since is moved here too.
	moved := s.image("acme/app", "v1", "acme/app v1, again")
	src2 := zotRoot(t, s, "acme/app")
	r, err = x.run(t, src2, importer.Options{})
	require.NoError(t, err)
	rr = repoReport(t, r, "acme/app")
	require.Equal(t, 1, rr.Tags)
	got, err := x.ix.Tag().Get(ctx, "acme/app", "v1")
	require.NoError(t, err)
	require.Equal(t, moved.Digest, got.Digest)
}

func TestImportMissing(t *testing.T) {
	ctx := context.Background()
	s := newSource(t)
	rel := s.release("acme/app")
	src := zotRoot(t, s, "acme/app")

	// One platform of the index has lost a layer.
	var amd v1.Manifest
	b, err := os.ReadFile(filepath.Join(src, "acme/app", "blobs", "sha256", rel.amd.Digest.Encoded()))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &amd))
	require.NoError(t, os.Remove(filepath.Join(src, "acme/app", "blobs", "sha256", amd.Layers[0].Digest.Encoded())))

	x := newTarget(t)
	r, err := x.run(t, src, importer.Options{})
	require.NoError(t, err)
	rr := repoReport(t, r, "acme/app")
	require.Contains(t, rr.Missing, rel.multi.Digest.String())
	require.Contains(t, rr.Missing[rel.multi.Digest.String()], "not in the layout")

	// Not the index, nor its tag; the rest is there.
	_, err = x.ix.Manifest().Get(ctx, "acme/app", rel.multi.Digest)
	require.ErrorIs(t, err, index.ErrNotFound)
	_, err = x.ix.Tag().Get(ctx, "acme/app", "multi")
	require.ErrorIs(t, err, index.ErrNotFound)
	got, err := x.ix.Tag().Get(ctx, "acme/app", "v1")
	require.NoError(t, err)
	require.Equal(t, rel.v1.Digest, got.Digest)
}

func TestImportVerify(t *testing.T) {
	s := newSource(t)
	rel := s.release("acme/app")
	src := zotRoot(t, s, "acme/app")

	// A layer of v1 whose content is not its name, its size the same.
	var m v1.Manifest
	b, err := os.ReadFile(filepath.Join(src, "acme/app", "blobs", "sha256", rel.v1.Digest.Encoded()))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &m))
	layer := filepath.Join(src, "acme/app", "blobs", "sha256", m.Layers[0].Digest.Encoded())
	content, err := os.ReadFile(layer)
	require.NoError(t, err)
	require.NoError(t, os.Remove(layer))
	require.NoError(t, os.WriteFile(layer, bytes.Repeat([]byte("x"), len(content)), 0o644))

	_, err = newTarget(t).run(t, src, importer.Options{Verify: true})
	require.ErrorIs(t, err, flob.ErrDigestMismatch)

	// Without it, a blob is taken to be what its name says.
	_, err = newTarget(t).run(t, src, importer.Options{})
	require.NoError(t, err)
}

func TestImportCopy(t *testing.T) {
	ctx := context.Background()
	s := newSource(t)
	rel := s.release("acme/app")
	src := zotRoot(t, s, "acme/app")

	// A store that is not a filesystem one cannot take a link.
	stores := flob.NewMemStores()
	ix := memindex.New()
	r, err := importer.Import(ctx, src, stores, ix, importer.Options{})
	require.ErrorContains(t, err, "cannot link")
	require.Equal(t, 0, repoReport(t, r, "acme/app").Manifests)

	r, err = importer.Import(ctx, src, stores, ix, importer.Options{Copy: true})
	require.NoError(t, err)
	rr := repoReport(t, r, "acme/app")
	require.Zero(t, rr.Linked)
	require.NotZero(t, rr.Copied)
	got, err := ix.Tag().Get(ctx, "acme/app", "multi")
	require.NoError(t, err)
	require.Equal(t, rel.multi.Digest, got.Digest)
}

func TestImportWhich(t *testing.T) {
	s := newSource(t)
	s.release("acme/app")
	s.release("acme/other")
	s.release("cache/docker.io/library/alpine")
	src := zotRoot(t, s, "acme/app", "acme/other", "cache/docker.io/library/alpine")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "Not_A_Name"), 0o755))
	s.export("acme/app", filepath.Join(src, "Not_A_Name"))

	refuse := func(repo string) string {
		if repo == "cache/docker.io/library/alpine" {
			return "a pull-through cache"
		}
		return ""
	}
	r, err := newTarget(t).run(t, src, importer.Options{Exclude: []string{"acme/other"}, Refuse: refuse})
	require.NoError(t, err)
	require.Len(t, r.Repositories, 1)
	require.Equal(t, "acme/app", r.Repositories[0].Repo)
	require.Equal(t, map[string]string{
		"acme/other":                     "excluded by acme/other",
		"cache/docker.io/library/alpine": "a pull-through cache",
		"Not_A_Name":                     "not a repository name",
	}, r.Skipped)

	r, err = newTarget(t).run(t, src, importer.Options{Only: []string{"acme/other"}})
	require.NoError(t, err)
	require.Len(t, r.Repositories, 1)
	require.Equal(t, "acme/other", r.Repositories[0].Repo)

	// One layout is one repository, named by the caller; a directory of
	// them names its own.
	one := filepath.Join(src, "acme/app")
	_, err = newTarget(t).run(t, one, importer.Options{})
	require.ErrorContains(t, err, "say which repository")
	r, err = newTarget(t).run(t, one, importer.Options{Repo: "moved/app"})
	require.NoError(t, err)
	require.Equal(t, "moved/app", r.Repositories[0].Repo)
	_, err = newTarget(t).run(t, src, importer.Options{Repo: "moved/app"})
	require.ErrorContains(t, err, "a repository name is not taken")
}

func TestImportRefNames(t *testing.T) {
	// What tools write as `ref.name`: a tag, or a whole reference.
	s := newSource(t)
	img := s.image("acme/app", "seed", "named")
	dir := filepath.Join(t.TempDir(), "layout")
	s.export("acme/app", dir)

	var idx v1.Index
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &idx))
	idx.Manifests = nil
	for _, name := range []string{"plain", "registry.example.com:5000/acme/app:full", "acme/app@" + img.Digest.String(), "acme/app", "-bad"} {
		d := img
		d.Annotations = map[string]string{v1.AnnotationRefName: name}
		idx.Manifests = append(idx.Manifests, d)
	}
	b, err = json.Marshal(idx)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.json"), b, 0o644))

	x := newTarget(t)
	r, err := x.run(t, dir, importer.Options{Repo: "acme/app"})
	require.NoError(t, err)
	rr := r.Repositories[0]
	require.Equal(t, 2, rr.Tags)
	require.ElementsMatch(t, []string{"acme/app@" + img.Digest.String(), "acme/app", "-bad"}, rr.BadTags)
	for _, tag := range []string{"plain", "full"} {
		got, err := x.ix.Tag().Get(context.Background(), "acme/app", tag)
		require.NoError(t, err)
		require.Equal(t, img.Digest, got.Digest)
	}
}
