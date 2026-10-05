package importer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/importer"
	"github.com/lesomnus/cr/index/memindex"
	"github.com/lesomnus/cr/registry"
)

// A registry moved off zot: images pushed to a real zot, its root taken in
// while it serves, and every question asked of both answered the same.
//
// CR_TEST_ZOT is the zot binary; scripts/zot-import.sh fetches the pinned one
// and runs this, as CI does.
func TestZot(t *testing.T) {
	bin := os.Getenv("CR_TEST_ZOT")
	if bin == "" {
		t.Skip("CR_TEST_ZOT is not set")
	}
	// zot either keeps one file per digest and hard-links it into every
	// repository, or a copy in each.
	for _, dedupe := range []bool{true, false} {
		t.Run(fmt.Sprintf("dedupe=%v", dedupe), func(t *testing.T) { testZot(t, bin, dedupe) })
	}
}

func testZot(t *testing.T, bin string, dedupe bool) {
	ctx := context.Background()
	work := t.TempDir()
	root := filepath.Join(work, "zot")
	z := startZot(t, bin, root, dedupe)

	// What a registry in use holds.
	const app, tools, alpine = "acme/app", "acme/app/tools", "library/alpine"
	shared := z.blob(app, []byte("a base layer, in more than one repository"), v1.MediaTypeImageLayerGzip)
	amd := z.image(app, "", "amd64", shared)
	arm := z.image(app, "", "arm64", shared)
	z.manifest(app, "multi", v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: []v1.Descriptor{platformed(amd, "amd64"), platformed(arm, "arm64")},
	}, v1.MediaTypeImageIndex)
	signed := z.image(app, "v1", "v1", shared)
	sig := z.manifest(app, "", v1.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    v1.MediaTypeImageManifest,
		ArtifactType: "application/vnd.cncf.notary.signature",
		Config:       z.blob(app, []byte("{}"), v1.MediaTypeEmptyJSON),
		Layers:       []v1.Descriptor{z.blob(app, []byte("an envelope"), "application/jose+json")},
		Subject:      &signed,
	}, v1.MediaTypeImageManifest)
	z.image(app, "", "nobody tagged this", shared)

	// Docker's own media types, as most of what a registry holds was pushed.
	const (
		dockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
		dockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
		dockerConfig   = "application/vnd.docker.container.image.v1+json"
		dockerLayer    = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	)
	dm := z.manifest(app, "", map[string]any{
		"schemaVersion": 2,
		"mediaType":     dockerManifest,
		"config":        z.blob(app, []byte(`{"architecture":"amd64","os":"linux"}`), dockerConfig),
		"layers":        []v1.Descriptor{z.blob(app, []byte("a docker layer"), dockerLayer)},
	}, dockerManifest)
	z.manifest(app, "docker", map[string]any{
		"schemaVersion": 2,
		"mediaType":     dockerList,
		"manifests":     []v1.Descriptor{platformed(dm, "amd64")},
	}, dockerList)

	// A repository inside another, sharing a layer with it: pushed there
	// too, as a client does, which zot either links or copies.
	z.blob(tools, []byte("a base layer, in more than one repository"), v1.MediaTypeImageLayerGzip)
	z.image(tools, "latest", "tools", shared)

	// A tag that moved: the first image is untagged now.
	z.image(alpine, "3", "alpine 3.20", z.blob(alpine, []byte("alpine 3.20 rootfs"), v1.MediaTypeImageLayerGzip))
	z.image(alpine, "3", "alpine 3.21", z.blob(alpine, []byte("alpine 3.21 rootfs"), v1.MediaTypeImageLayerGzip))

	// Taken in while zot still serves, into a store on the same filesystem.
	x := &target{root: filepath.Join(work, "cr"), ix: memindex.New()}
	x.stores = flob.NewOsStores(x.root)
	r, err := importer.Import(ctx, root, x.stores, x.ix, importer.Options{Verify: true})
	require.NoError(t, err)
	got := map[string]bool{}
	for _, rr := range r.Repositories {
		got[rr.Repo] = true
		require.Empty(t, rr.Missing, rr.Repo)
		require.Empty(t, rr.BadTags, rr.Repo)
		require.Zero(t, rr.Copied, rr.Repo)
	}
	require.Equal(t, map[string]bool{app: true, tools: true, alpine: true}, got)

	cr := httptest.NewServer(registry.New(registry.Config{Stores: x.stores, Index: x.ix}))
	t.Cleanup(cr.Close)

	// The same answers, byte for byte: tags, and everything each one reaches.
	for _, repo := range []string{app, tools, alpine} {
		tags := z.tags(z.url, repo)
		require.Equal(t, tags, z.tags(cr.URL, repo), "%s: tags", repo)
		for _, tag := range tags {
			z.same(cr.URL, repo, tag)
		}
	}
	// What nobody tagged, by digest.
	z.same(cr.URL, app, dm.Digest.String())
	// The signature, found as zot finds it.
	require.Equal(t, z.referrers(z.url, app, signed.Digest), z.referrers(cr.URL, app, signed.Digest))
	require.Contains(t, z.referrers(cr.URL, app, signed.Digest), sig.Digest)

	// Linked, not copied: the store's one copy of the shared layer is one of
	// zot's files.
	dst, err := os.Stat(x.shared(shared.Digest))
	require.NoError(t, err)
	src, err := os.Stat(filepath.Join(root, app, "blobs", "sha256", shared.Digest.Encoded()))
	require.NoError(t, err)
	require.True(t, os.SameFile(src, dst))
	// zot's file in the other repository is the same one when it dedupes,
	// and a copy, which the store does not take, when it does not.
	other, err := os.Stat(filepath.Join(root, tools, "blobs", "sha256", shared.Digest.Encoded()))
	require.NoError(t, err)
	require.Equal(t, dedupe, os.SameFile(other, dst))

	// zot still serves what it did.
	z.same(z.url, app, "multi")
}

func platformed(d v1.Descriptor, arch string) v1.Descriptor {
	d.Platform = &v1.Platform{Architecture: arch, OS: "linux"}
	return d
}

type zot struct {
	t   *testing.T
	url string
	c   *http.Client
}

func startZot(t *testing.T, bin, root string, dedupe bool) *zot {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	config := filepath.Join(filepath.Dir(root), "zot.json")
	b, err := json.Marshal(map[string]any{
		"distSpecVersion": "1.1.1",
		"storage":         map[string]any{"rootDirectory": root, "dedupe": dedupe, "gc": false},
		// Docker's media types are refused unless asked for, and a registry
		// that holds them, as cr.hday.io's does, has asked.
		"http": map[string]any{"address": "127.0.0.1", "port": fmt.Sprint(port), "compat": []string{"docker2s2"}},
		"log":  map[string]any{"level": "error"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(config, b, 0o644))

	var logs bytes.Buffer
	cmd := exec.Command(bin, "serve", config)
	cmd.Stdout, cmd.Stderr = &logs, &logs
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			t.Logf("zot:\n%s", logs.String())
		}
	})

	z := &zot{t: t, url: fmt.Sprintf("http://127.0.0.1:%d", port), c: &http.Client{Timeout: 30 * time.Second}}
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := z.c.Get(z.url + "/v2/")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return z
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("zot did not come up: %v\n%s", err, logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (z *zot) do(method, u string, body []byte, header ...string) (*http.Response, []byte) {
	z.t.Helper()
	req, err := http.NewRequest(method, u, bytes.NewReader(body))
	require.NoError(z.t, err)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := z.c.Do(req)
	require.NoError(z.t, err)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	require.NoError(z.t, err)
	return res, b
}

// blob pushes b, as a client does: an upload, then its one PUT.
func (z *zot) blob(repo string, b []byte, mt string) v1.Descriptor {
	z.t.Helper()
	d := digest.FromBytes(b)
	res, body := z.do("POST", z.url+"/v2/"+repo+"/blobs/uploads/", nil)
	require.Equal(z.t, http.StatusAccepted, res.StatusCode, string(body))
	loc, err := url.Parse(res.Header.Get("Location"))
	require.NoError(z.t, err)
	u, err := url.Parse(z.url)
	require.NoError(z.t, err)
	loc = u.ResolveReference(loc)
	q := loc.Query()
	q.Set("digest", d.String())
	loc.RawQuery = q.Encode()
	res, body = z.do("PUT", loc.String(), b, "Content-Type", "application/octet-stream")
	require.Equal(z.t, http.StatusCreated, res.StatusCode, string(body))
	return v1.Descriptor{MediaType: mt, Digest: d, Size: int64(len(b))}
}

func (z *zot) manifest(repo, tag string, v any, mt string) v1.Descriptor {
	z.t.Helper()
	b, err := json.Marshal(v)
	require.NoError(z.t, err)
	d := digest.FromBytes(b)
	ref := tag
	if ref == "" {
		ref = d.String()
	}
	res, body := z.do("PUT", z.url+"/v2/"+repo+"/manifests/"+ref, b, "Content-Type", mt)
	require.Equal(z.t, http.StatusCreated, res.StatusCode, string(body))
	return v1.Descriptor{MediaType: mt, Digest: d, Size: int64(len(b))}
}

func (z *zot) image(repo, tag, what string, base v1.Descriptor) v1.Descriptor {
	z.t.Helper()
	return z.manifest(repo, tag, v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    z.blob(repo, []byte(`{"architecture":"amd64","os":"linux","what":"`+what+`"}`), v1.MediaTypeImageConfig),
		Layers:    []v1.Descriptor{base, z.blob(repo, []byte(what), v1.MediaTypeImageLayerGzip)},
	}, v1.MediaTypeImageManifest)
}

func (z *zot) tags(base, repo string) []string {
	z.t.Helper()
	res, b := z.do("GET", base+"/v2/"+repo+"/tags/list", nil)
	require.Equal(z.t, http.StatusOK, res.StatusCode, "%s %s: %s", base, repo, b)
	var v struct {
		Tags []string `json:"tags"`
	}
	require.NoError(z.t, json.Unmarshal(b, &v))
	return v.Tags
}

func (z *zot) referrers(base, repo string, d digest.Digest) []digest.Digest {
	z.t.Helper()
	res, b := z.do("GET", base+"/v2/"+repo+"/referrers/"+d.String(), nil)
	require.Equal(z.t, http.StatusOK, res.StatusCode, "%s: %s", base, b)
	var idx v1.Index
	require.NoError(z.t, json.Unmarshal(b, &idx))
	out := []digest.Digest{}
	for _, m := range idx.Manifests {
		out = append(out, m.Digest)
	}
	return out
}

var accept = strings.Join([]string{
	v1.MediaTypeImageIndex, v1.MediaTypeImageManifest,
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// same asks zot and the registry at other for ref in repo, and for everything
// it reaches, and wants the same bytes and the same digest from both.
func (z *zot) same(other, repo, ref string) {
	z.t.Helper()
	zr, zb := z.do("GET", z.url+"/v2/"+repo+"/manifests/"+ref, nil, "Accept", accept)
	require.Equal(z.t, http.StatusOK, zr.StatusCode, "zot %s:%s: %s", repo, ref, zb)
	cr, cb := z.do("GET", other+"/v2/"+repo+"/manifests/"+ref, nil, "Accept", accept)
	require.Equal(z.t, http.StatusOK, cr.StatusCode, "%s:%s: %s", repo, ref, cb)
	require.Equal(z.t, zb, cb, "%s:%s", repo, ref)
	require.Equal(z.t, zr.Header.Get("Docker-Content-Digest"), cr.Header.Get("Docker-Content-Digest"), "%s:%s", repo, ref)
	require.Equal(z.t, zr.Header.Get("Content-Type"), cr.Header.Get("Content-Type"), "%s:%s", repo, ref)

	var m struct {
		Config    *v1.Descriptor  `json:"config"`
		Layers    []v1.Descriptor `json:"layers"`
		Manifests []v1.Descriptor `json:"manifests"`
	}
	require.NoError(z.t, json.Unmarshal(zb, &m))
	for _, c := range m.Manifests {
		z.same(other, repo, c.Digest.String())
	}
	blobs := m.Layers
	if m.Config != nil {
		blobs = append(blobs, *m.Config)
	}
	for _, b := range blobs {
		zr, zb := z.do("GET", z.url+"/v2/"+repo+"/blobs/"+b.Digest.String(), nil)
		require.Equal(z.t, http.StatusOK, zr.StatusCode)
		cr, cb := z.do("GET", other+"/v2/"+repo+"/blobs/"+b.Digest.String(), nil)
		require.Equal(z.t, http.StatusOK, cr.StatusCode, "%s@%s", repo, b.Digest)
		require.Equal(z.t, zb, cb, "%s@%s", repo, b.Digest)
	}
}
