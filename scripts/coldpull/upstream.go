package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
)

// blockSize is the unit synthetic content is made in: any block can be made
// without the ones before it, so a layer is seekable without being stored.
const blockSize = 64 << 10

// layer is size bytes that are the same every time they are read, and that do
// not compress: block k is ChaCha8 keyed by (image, layer, k).
type layer struct {
	image, index int
	size         int64
	digest       digest.Digest
}

func (l *layer) block(k int64, b []byte) {
	var seed [32]byte
	binary.LittleEndian.PutUint64(seed[0:], uint64(l.image))
	binary.LittleEndian.PutUint64(seed[8:], uint64(l.index))
	binary.LittleEndian.PutUint64(seed[16:], uint64(k))
	seed = sha256.Sum256(seed[:])
	rand.NewChaCha8(seed).Read(b)
}

// layerReader is an [io.ReadSeeker] over a layer, for [http.ServeContent].
type layerReader struct {
	l     *layer
	pos   int64
	k     int64
	block []byte
}

func (r *layerReader) Read(p []byte) (int, error) {
	if r.pos >= r.l.size {
		return 0, io.EOF
	}
	k := r.pos / blockSize
	if r.block == nil || k != r.k {
		if r.block == nil {
			r.block = make([]byte, blockSize)
		}
		r.l.block(k, r.block)
		r.k = k
	}
	off := r.pos % blockSize
	end := min(int64(blockSize), r.l.size-k*blockSize)
	n := copy(p, r.block[off:end])
	r.pos += int64(n)
	return n, nil
}

func (r *layerReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.pos
	case io.SeekEnd:
		offset += r.l.size
	}
	if offset < 0 {
		return 0, fmt.Errorf("seek before start")
	}
	r.pos = offset
	return offset, nil
}

type blob struct {
	layer *layer // nil for a config or a manifest
	bytes []byte
	mt    string
}

// throttled writes at most rate bytes a second, per response.
type throttled struct {
	http.ResponseWriter
	rate  float64
	start time.Time
	sent  int64
}

func (t *throttled) Write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		n := min(len(b), 32<<10)
		m, err := t.ResponseWriter.Write(b[:n])
		written += m
		t.sent += int64(m)
		if err != nil {
			return written, err
		}
		b = b[n:]
		if due := t.start.Add(time.Duration(float64(t.sent) / t.rate * float64(time.Second))); time.Until(due) > 0 {
			time.Sleep(time.Until(due))
		}
	}
	return written, nil
}

func imageName(i int) string { return fmt.Sprintf("coldpull/img%d", i) }

func upstream(args []string) error {
	fs := flag.NewFlagSet("upstream", flag.ExitOnError)
	listen := fs.String("listen", ":5000", "address to serve on")
	images := fs.Int("images", 8, "distinct images, coldpull/img0 … coldpull/imgN-1, each tagged latest")
	layers := fs.Int("layers", 4, "layers per image")
	layerMiB := fs.Int("layer-mib", 32, "size of each layer, in MiB")
	rateMiB := fs.Float64("rate-mib", 25, "bytes a second per response, in MiB; 0 is unbounded")
	fs.Parse(args)

	start := time.Now()
	blobs := map[digest.Digest]blob{}
	manifests := map[string]digest.Digest{} // name → manifest digest
	for i := range *images {
		var ls []map[string]any
		for j := range *layers {
			l := &layer{image: i, index: j, size: int64(*layerMiB) << 20}
			h := sha256.New()
			io.Copy(h, &layerReader{l: l})
			l.digest = digest.NewDigest(digest.SHA256, h)
			blobs[l.digest] = blob{layer: l, mt: "application/vnd.oci.image.layer.v1.tar"}
			ls = append(ls, map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": l.digest, "size": l.size})
		}
		config := []byte(fmt.Sprintf(`{"architecture":"amd64","os":"linux","config":{"Labels":{"coldpull":"%d"}}}`, i))
		cd := digest.FromBytes(config)
		blobs[cd] = blob{bytes: config, mt: "application/vnd.oci.image.config.v1+json"}
		m, _ := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     "application/vnd.oci.image.manifest.v1+json",
			"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": len(config)},
			"layers":        ls,
		})
		md := digest.FromBytes(m)
		blobs[md] = blob{bytes: m, mt: "application/vnd.oci.image.manifest.v1+json"}
		manifests[imageName(i)] = md
	}
	log.Printf("upstream: %d images of %d × %d MiB, made in %s, served at %.0f MiB/s a response, on %s",
		*images, *layers, *layerMiB, time.Since(start).Round(time.Millisecond), *rateMiB, *listen)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/v2/" || p == "/v2" {
			w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
			w.Write([]byte("{}"))
			return
		}
		rest, ok := strings.CutPrefix(p, "/v2/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		var name, kind, ref string
		for _, k := range []string{"/manifests/", "/blobs/", "/tags/list", "/referrers/"} {
			if i := strings.LastIndex(rest, k); i >= 0 {
				name, kind, ref = rest[:i], strings.Trim(k, "/"), rest[i+len(k):]
				break
			}
		}
		md, known := manifests[name]
		if !known {
			notFound(w, "NAME_UNKNOWN")
			return
		}
		switch kind {
		case "tags/list":
			json.NewEncoder(w).Encode(map[string]any{"name": name, "tags": []string{"latest"}})
			return
		case "manifests":
			if ref == "latest" {
				ref = md.String()
			}
		case "blobs":
		default:
			notFound(w, "UNSUPPORTED")
			return
		}
		b, ok := blobs[digest.Digest(ref)]
		if !ok || (kind == "manifests" && b.layer != nil) {
			notFound(w, strings.ToUpper(strings.TrimSuffix(kind, "s"))+"_UNKNOWN")
			return
		}
		w.Header().Set("Docker-Content-Digest", ref)
		w.Header().Set("Content-Type", b.mt)
		if kind == "blobs" {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		var rs io.ReadSeeker
		if b.layer != nil {
			rs = &layerReader{l: b.layer}
		} else {
			rs = strings.NewReader(string(b.bytes))
		}
		out := w
		if *rateMiB > 0 && b.layer != nil {
			out = &throttled{ResponseWriter: w, rate: *rateMiB * (1 << 20), start: time.Now()}
		}
		http.ServeContent(out, r, "", time.Time{}, rs)
	})
	return http.ListenAndServe(*listen, h)
}

func notFound(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, `{"errors":[{"code":%q}]}`, code)
}
