// Package e2e runs cr as it is deployed, from a binary built out of this
// checkout, against what a deployment gives it.
package e2e

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

// Two `cr serve` on one PostgreSQL database and one `os` store, as two pods
// of one Deployment on the node that holds the store: the replicas of a
// rolling update, or more than one for good. Each takes requests for what the
// other wrote, an upload goes on wherever its next request lands, writers of
// one digest on both are serialized by the store's file locks, a collection
// on one sees what the other holds, and one stopping mid-upload loses nothing.
//
// CR_TEST_POSTGRES is the database, as for the index's own PostgreSQL tests;
// each run takes a schema of its own.
func TestPostgresReplicasShareOsStore(t *testing.T) {
	dsn := os.Getenv("CR_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("CR_TEST_POSTGRES is not set")
	}
	work := t.TempDir()
	bin := build(t, work)
	db := schema(t, dsn)
	root := filepath.Join(work, "store")

	a := serve(t, bin, work, "a", db, root)
	b := serve(t, bin, work, "b", db, root)

	t.Run("what one wrote, the other serves", func(t *testing.T) {
		img := a.image("acme/app", "v1", "the layer", "")
		require.Equal(t, []string{"v1"}, b.tags("acme/app"))
		b.same(a, "acme/app", "v1")
		require.Equal(t, img.Digest.String(), b.head("acme/app", "v1"))
	})

	t.Run("an upload goes on wherever its next request lands", func(t *testing.T) {
		content := bytes.Repeat([]byte("chunked across replicas "), 4096)
		d := digest.FromBytes(content)
		loc := a.begin("acme/chunked")
		third := len(content) / 3
		loc = b.patch(loc, content[:third], 0)
		loc = a.patch(loc, content[third:2*third], third)
		b.finish(loc, content[2*third:], 2*third, d)
		require.Equal(t, content, a.blob("acme/chunked", d))
	})

	t.Run("writers of one digest on both are one copy", func(t *testing.T) {
		layer := bytes.Repeat([]byte("shared by every repository "), 8192)
		d := digest.FromBytes(layer)
		const n = 24
		var wg sync.WaitGroup
		var failed atomic.Int64
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				x := a
				if i%2 == 1 {
					x = b
				}
				res := x.do("POST", fmt.Sprintf("/v2/shared/r%d/blobs/uploads/?digest=%s", i, d), layer)
				if res.StatusCode != http.StatusCreated {
					failed.Add(1)
				}
			}()
		}
		wg.Wait()
		require.Zero(t, failed.Load())
		for i := range n {
			x := []*replica{a, b}[(i+1)%2]
			require.Equal(t, layer, x.blob(fmt.Sprintf("shared/r%d", i), d), "r%d", i)
		}
		// One inode for the digest, linked from every repository and the
		// store's own copy.
		fi, err := os.Stat(shared(root, d))
		require.NoError(t, err)
		require.EqualValues(t, n+1, fi.Sys().(*syscall.Stat_t).Nlink)
	})

	t.Run("tags written on both at once are all there", func(t *testing.T) {
		const n = 20
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				x := []*replica{a, b}[i%2]
				x.image("acme/tags", fmt.Sprintf("t%02d", i), fmt.Sprintf("layer %d", i), "")
			}()
		}
		wg.Wait()
		for _, x := range []*replica{a, b} {
			require.Len(t, x.tags("acme/tags"), n)
		}
	})

	t.Run("a collection on one sees what the other holds", func(t *testing.T) {
		base := "a base layer two repositories share"
		gone := a.image("acme/old", "v1", "only the old image has this", base)
		kept := b.image("acme/new", "v1", "only the new image has this", base)
		var m v1.Manifest
		require.NoError(t, json.Unmarshal(a.manifest("acme/old", gone.Digest.String()), &m))
		res := a.do("DELETE", "/v2/acme/old/manifests/"+gone.Digest.String(), nil)
		require.Equal(t, http.StatusAccepted, res.StatusCode)

		// Past the sweep's delay, so what was just pushed is not left for
		// being young.
		time.Sleep(2 * time.Second)
		run := b.collect()
		require.Equal(t, "done", run.State, run.Error)

		for _, x := range []*replica{a, b} {
			for _, l := range m.Layers {
				res := x.do("GET", "/v2/acme/old/blobs/"+l.Digest.String(), nil)
				require.Equal(t, http.StatusNotFound, res.StatusCode, "%s in acme/old", l.Digest)
			}
			x.same(a, "acme/new", "v1")
		}
		fi, err := os.Stat(shared(root, digest.FromString(base)))
		require.NoError(t, err, "the shared layer's one copy")
		require.EqualValues(t, 2, fi.Sys().(*syscall.Stat_t).Nlink, "the store's and acme/new's")
		_ = kept
	})

	t.Run("one stopping mid-upload loses nothing", func(t *testing.T) {
		// A rolling update: the old pod is told to stop while an upload is
		// half done, and the rest of it lands on the new one.
		content := bytes.Repeat([]byte("through a rolling update "), 4096)
		d := digest.FromBytes(content)
		loc := a.begin("acme/rolling")
		half := len(content) / 2
		loc = a.patch(loc, content[:half], 0)
		a.stop()

		b.finish(loc, content[half:], half, d)
		require.Equal(t, content, b.blob("acme/rolling", d))

		a = serve(t, bin, work, "a2", db, root)
		require.Equal(t, content, a.blob("acme/rolling", d))
		a.same(b, "acme/app", "v1")
	})
}

// build is cr, from this checkout.
func build(t *testing.T, dir string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	bin := filepath.Join(dir, "cr")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/cr")
	cmd.Dir = filepath.Join(filepath.Dir(file), "..")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	return bin
}

var schemas atomic.Int64

// schema is dsn with a schema of this run's own, dropped when it is over.
func schema(t *testing.T, dsn string) string {
	t.Helper()
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })
	name := fmt.Sprintf("replicas%d_%d", os.Getpid(), schemas.Add(1))
	_, err = admin.Exec("CREATE SCHEMA " + name)
	require.NoError(t, err)
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + name + " CASCADE") })

	u, err := url.Parse(dsn)
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", name)
	u.RawQuery = q.Encode()
	return u.String()
}

// shared is the store's own copy of d, which every repository holding it is
// a link to.
func shared(root string, d digest.Digest) string {
	v := d.Encoded()
	return filepath.Join(root, "share", d.Algorithm().String(), v[0:2], v[2:4], v[4:])
}

type replica struct {
	t    *testing.T
	name string
	url  string
	cmd  *exec.Cmd
	logs *bytes.Buffer
	c    *http.Client
}

// serve starts `cr serve` named name, on db and the store at root, and waits
// for it to be ready.
func serve(t *testing.T, bin, work, name, db, root string) *replica {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	config := filepath.Join(work, name+".yaml")
	require.NoError(t, os.WriteFile(config, []byte(fmt.Sprintf(`db:
  driver: pgx
  dsn: %q
  migrate: true
server:
  addr: "127.0.0.1:0"
  http:
    addr: "127.0.0.1:%d"
watch:
  broker: postgres
shutdown:
  drain: 0s
  timeout: 10s
registry:
  gc:
    every: -1s
    delay: 1s
  storage:
    driver: os
    os:
      root: %q
`, db, port, root)), 0o644))

	x := &replica{t: t, name: name, url: fmt.Sprintf("http://127.0.0.1:%d", port), logs: &bytes.Buffer{}, c: &http.Client{Timeout: time.Minute}}
	x.cmd = exec.Command(bin, "--config", config, "serve")
	x.cmd.Stdout, x.cmd.Stderr = x.logs, x.logs
	require.NoError(t, x.cmd.Start())
	t.Cleanup(func() {
		if x.cmd.ProcessState == nil {
			x.cmd.Process.Kill()
			x.cmd.Wait()
		}
		if t.Failed() {
			t.Logf("%s:\n%s", name, x.logs.String())
		}
	})

	deadline := time.Now().Add(time.Minute)
	for {
		res, err := x.c.Get(x.url + "/readyz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return x
			}
		}
		if time.Now().After(deadline) || x.cmd.ProcessState != nil {
			t.Fatalf("%s did not come up: %v\n%s", name, err, x.logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stop is SIGTERM, as a pod is told, and waits for the process to end.
func (x *replica) stop() {
	x.t.Helper()
	require.NoError(x.t, x.cmd.Process.Signal(syscall.SIGTERM))
	done := make(chan error, 1)
	go func() { done <- x.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		x.t.Fatalf("%s did not stop", x.name)
	}
}

func (x *replica) do(method, path string, body []byte, header ...string) *http.Response {
	x.t.Helper()
	u := path
	if !strings.HasPrefix(u, "http") {
		u = x.url + path
	}
	req, err := http.NewRequest(method, u, bytes.NewReader(body))
	require.NoError(x.t, err)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := x.c.Do(req)
	require.NoError(x.t, err)
	b, err := io.ReadAll(res.Body)
	res.Body.Close()
	require.NoError(x.t, err)
	res.Body = io.NopCloser(bytes.NewReader(b))
	return res
}

func read(t *testing.T, res *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return b
}

func (x *replica) push(repo string, b []byte, mt string) v1.Descriptor {
	x.t.Helper()
	d := digest.FromBytes(b)
	res := x.do("POST", "/v2/"+repo+"/blobs/uploads/?digest="+d.String(), b)
	require.Equal(x.t, http.StatusCreated, res.StatusCode, "%s: %s", x.name, read(x.t, res))
	return v1.Descriptor{MediaType: mt, Digest: d, Size: int64(len(b))}
}

// image pushes an image of one layer, and a base layer under it when base is
// not empty, tagged tag.
func (x *replica) image(repo, tag, layer, base string) v1.Descriptor {
	x.t.Helper()
	layers := []v1.Descriptor{}
	if base != "" {
		layers = append(layers, x.push(repo, []byte(base), v1.MediaTypeImageLayerGzip))
	}
	layers = append(layers, x.push(repo, []byte(layer), v1.MediaTypeImageLayerGzip))
	b, err := json.Marshal(v1.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config:    x.push(repo, []byte(`{"layer":"`+layer+`"}`), v1.MediaTypeImageConfig),
		Layers:    layers,
	})
	require.NoError(x.t, err)
	res := x.do("PUT", "/v2/"+repo+"/manifests/"+tag, b, "Content-Type", v1.MediaTypeImageManifest)
	require.Equal(x.t, http.StatusCreated, res.StatusCode, "%s: %s", x.name, read(x.t, res))
	return v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: digest.FromBytes(b), Size: int64(len(b))}
}

func (x *replica) tags(repo string) []string {
	x.t.Helper()
	res := x.do("GET", "/v2/"+repo+"/tags/list", nil)
	require.Equal(x.t, http.StatusOK, res.StatusCode)
	var v struct {
		Tags []string `json:"tags"`
	}
	require.NoError(x.t, json.Unmarshal(read(x.t, res), &v))
	return v.Tags
}

func (x *replica) head(repo, ref string) string {
	x.t.Helper()
	res := x.do("HEAD", "/v2/"+repo+"/manifests/"+ref, nil, "Accept", v1.MediaTypeImageManifest)
	require.Equal(x.t, http.StatusOK, res.StatusCode)
	return res.Header.Get("Docker-Content-Digest")
}

func (x *replica) manifest(repo, ref string) []byte {
	x.t.Helper()
	res := x.do("GET", "/v2/"+repo+"/manifests/"+ref, nil, "Accept", v1.MediaTypeImageManifest)
	require.Equal(x.t, http.StatusOK, res.StatusCode, "%s: %s:%s", x.name, repo, ref)
	return read(x.t, res)
}

func (x *replica) blob(repo string, d digest.Digest) []byte {
	x.t.Helper()
	res := x.do("GET", "/v2/"+repo+"/blobs/"+d.String(), nil)
	require.Equal(x.t, http.StatusOK, res.StatusCode, "%s: %s@%s", x.name, repo, d)
	return read(x.t, res)
}

// same wants x and other to answer ref in repo, and every blob of it, alike.
func (x *replica) same(other *replica, repo, ref string) {
	x.t.Helper()
	b := x.manifest(repo, ref)
	require.Equal(x.t, b, other.manifest(repo, ref))
	var m v1.Manifest
	require.NoError(x.t, json.Unmarshal(b, &m))
	for _, l := range append([]v1.Descriptor{m.Config}, m.Layers...) {
		require.Equal(x.t, x.blob(repo, l.Digest), other.blob(repo, l.Digest))
	}
}

// begin starts an upload, answering where it goes on.
func (x *replica) begin(repo string) string {
	x.t.Helper()
	res := x.do("POST", "/v2/"+repo+"/blobs/uploads/", nil)
	require.Equal(x.t, http.StatusAccepted, res.StatusCode)
	return res.Header.Get("Location")
}

// patch sends a chunk at offset, answering where the upload goes on.
func (x *replica) patch(loc string, chunk []byte, offset int) string {
	x.t.Helper()
	res := x.do("PATCH", loc, chunk, "Content-Type", "application/octet-stream",
		"Content-Range", fmt.Sprintf("%d-%d", offset, offset+len(chunk)-1))
	require.Equal(x.t, http.StatusAccepted, res.StatusCode, "%s: %s", x.name, read(x.t, res))
	return res.Header.Get("Location")
}

// finish sends the last chunk at offset with the PUT that ends the upload.
func (x *replica) finish(loc string, chunk []byte, offset int, d digest.Digest) {
	x.t.Helper()
	sep := "?"
	if strings.Contains(loc, "?") {
		sep = "&"
	}
	res := x.do("PUT", loc+sep+"digest="+d.String(), chunk, "Content-Type", "application/octet-stream",
		"Content-Range", fmt.Sprintf("%d-%d", offset, offset+len(chunk)-1))
	require.Equal(x.t, http.StatusCreated, res.StatusCode, "%s: %s", x.name, read(x.t, res))
}

type run struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Error string `json:"error"`
}

// collect runs a full collection on x and waits for it to end.
func (x *replica) collect() run {
	x.t.Helper()
	res := x.do("POST", "/admin/gc", nil)
	body := read(x.t, res)
	require.Equal(x.t, http.StatusAccepted, res.StatusCode, "%s: %s", x.name, body)
	var r run
	require.NoError(x.t, json.Unmarshal(body, &r))
	deadline := time.Now().Add(time.Minute)
	for r.State == "running" {
		if time.Now().After(deadline) {
			x.t.Fatalf("%s: the collection %s did not end", x.name, r.ID)
		}
		time.Sleep(200 * time.Millisecond)
		res := x.do("GET", "/admin/gc/"+r.ID, nil)
		require.Equal(x.t, http.StatusOK, res.StatusCode)
		require.NoError(x.t, json.Unmarshal(read(x.t, res), &r))
	}
	return r
}
