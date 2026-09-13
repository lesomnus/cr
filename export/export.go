// Package export writes a repository out of cr as an OCI image layout: the
// directory `oras`, `skopeo`, `crane` and containerd read and write.
package export

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/oci"
)

// Report is what an export wrote.
type Report struct {
	Tags      int
	Manifests int
	Blobs     int
	Bytes     int64

	// Skipped is the blobs a manifest names that the store does not have
	// and that need not be there: non-distributable layers, and whatever a
	// pull-through cache never fetched.
	Skipped []string
}

type Options struct {
	// Tags are the tags to export; none is every tag.
	Tags []string

	// Referrers also writes the manifests that refer to what is exported:
	// signatures, attestations, SBOMs.
	Referrers bool
}

// Layout writes repo's tags under dir as an OCI image layout, with every
// manifest and blob they need. A layout already at dir is added to: blobs it
// has are not written again, and index.json is replaced with what this
// export named.
func Layout(ctx context.Context, ix index.Index, s flob.Store, repo, dir string, o Options) (Report, error) {
	var r Report
	tags := o.Tags
	if len(tags) == 0 {
		ts, err := ix.Tag().All(ctx, repo)
		if err != nil {
			return r, err
		}
		for _, t := range ts {
			tags = append(tags, t.Name)
		}
	}
	if len(tags) == 0 {
		return r, fmt.Errorf("%s: nothing to export", repo)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return r, err
	}
	w := &writer{ctx: ctx, ix: ix, s: s, repo: repo, dir: dir, r: &r, done: map[digest.Digest]bool{}}

	var entries []v1.Descriptor
	for _, tag := range tags {
		t, err := ix.Tag().Get(ctx, repo, tag)
		if errors.Is(err, index.ErrNotFound) {
			return r, fmt.Errorf("%s:%s: no such tag", repo, tag)
		}
		if err != nil {
			return r, err
		}
		d, err := w.manifest(t.Digest)
		if err != nil {
			return r, err
		}
		d.Annotations = map[string]string{v1.AnnotationRefName: tag}
		entries = append(entries, d)
		r.Tags++
	}

	if o.Referrers {
		for i := 0; i < len(w.manifests); i++ {
			refs, err := ix.Manifest().Referrers(ctx, repo, w.manifests[i], "")
			if err != nil {
				return r, err
			}
			for _, m := range refs {
				if w.done[m.Digest] {
					continue
				}
				d, err := w.manifest(m.Digest)
				if err != nil {
					return r, err
				}
				entries = append(entries, d)
			}
		}
	}

	idx := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: entries}
	if err := writeJSON(filepath.Join(dir, v1.ImageIndexFile), idx); err != nil {
		return r, err
	}
	if err := writeJSON(filepath.Join(dir, v1.ImageLayoutFile), v1.ImageLayout{Version: v1.ImageLayoutVersion}); err != nil {
		return r, err
	}
	return r, nil
}

type writer struct {
	ctx  context.Context
	ix   index.Index
	s    flob.Store
	repo string
	dir  string
	r    *Report

	done      map[digest.Digest]bool
	manifests []digest.Digest
}

// manifest writes the manifest d and everything it holds, and answers its
// descriptor.
func (w *writer) manifest(d digest.Digest) (v1.Descriptor, error) {
	m, err := w.ix.Manifest().Get(w.ctx, w.repo, d)
	if err != nil {
		return v1.Descriptor{}, fmt.Errorf("%s@%s: %w", w.repo, d, err)
	}
	desc := v1.Descriptor{MediaType: m.MediaType, ArtifactType: m.ArtifactType, Digest: d, Size: m.Size}
	if w.done[d] {
		return desc, nil
	}

	b, err := w.read(d, m.Size)
	if err != nil {
		return desc, err
	}
	if err := w.put(d, b); err != nil {
		return desc, err
	}
	w.done[d] = true
	w.manifests = append(w.manifests, d)
	w.r.Manifests++

	p, err := oci.ParseManifest(m.MediaType, b)
	if err != nil {
		return desc, fmt.Errorf("%s@%s: %w", w.repo, d, err)
	}
	for _, c := range p.Manifests {
		if _, err := w.manifest(c.Digest); err != nil {
			if errors.Is(err, index.ErrNotFound) {
				w.r.Skipped = append(w.r.Skipped, c.Digest.String())
				continue
			}
			return desc, err
		}
	}
	blobs := slices.Clone(p.Layers)
	if p.Config != nil {
		blobs = append(blobs, *p.Config)
	}
	for _, l := range blobs {
		if err := w.blob(l); err != nil {
			return desc, err
		}
	}
	return desc, nil
}

func (w *writer) read(d digest.Digest, size int64) ([]byte, error) {
	rc, _, err := w.s.Open(w.ctx, flob.Digest(d))
	if err != nil {
		return nil, fmt.Errorf("%s@%s: %w", w.repo, d, err)
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, size+1))
}

func (w *writer) path(d digest.Digest) string {
	return filepath.Join(w.dir, v1.ImageBlobsDir, d.Algorithm().String(), d.Encoded())
}

func (w *writer) put(d digest.Digest, b []byte) error {
	if d.Algorithm().FromBytes(b) != d {
		return fmt.Errorf("%s@%s: the store's bytes do not hash to it", w.repo, d)
	}
	return atomic(w.path(d), func(f *os.File) error {
		_, err := f.Write(b)
		return err
	})
}

// blob writes one blob, verifying it on the way, unless the layout has it.
func (w *writer) blob(desc v1.Descriptor) error {
	d := desc.Digest
	if w.done[d] {
		return nil
	}
	w.done[d] = true
	if _, err := os.Stat(w.path(d)); err == nil {
		return nil
	}

	var src io.ReadCloser
	if b, ok := blob.WellKnown(d); ok {
		src = io.NopCloser(bytes.NewReader(b))
	} else {
		rc, _, err := w.s.Open(w.ctx, flob.Digest(d))
		if errors.Is(err, flob.ErrNotExist) {
			// Not there, and the manifest was accepted without it: a
			// non-distributable layer, or a blob a cache never fetched.
			w.r.Skipped = append(w.r.Skipped, d.String())
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s@%s: %w", w.repo, d, err)
		}
		src = rc
	}
	defer src.Close()

	n := int64(0)
	err := atomic(w.path(d), func(f *os.File) error {
		v := d.Verifier()
		k, err := io.Copy(io.MultiWriter(f, v), src)
		n = k
		if err != nil {
			return err
		}
		if !v.Verified() {
			return fmt.Errorf("%s@%s: the store's bytes do not hash to it", w.repo, d)
		}
		return nil
	})
	if err != nil {
		return err
	}
	w.r.Blobs++
	w.r.Bytes += n
	return nil
}

// atomic writes path through a temporary file beside it, so a layout never
// holds a blob that is only partly there.
func atomic(path string, write func(*os.File) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := write(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomic(path, func(f *os.File) error {
		_, err := f.Write(b)
		return err
	})
}
