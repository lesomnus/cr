// Package importer takes OCI image layouts into a registry: a layout of one
// repository, or a directory of them as zot keeps its root. It is the other
// half of package export, and what moving a registry onto cr is made of.
//
// Blobs are taken in by hard link ([flob.OsStore.Adopt]), not copied: the
// source and the store share each file, so a registry of terabytes moves in
// the time it takes to make the links, and nothing is written twice. The index
// is written from the layout -- manifests, what they hold, and the tags its
// `index.json` names -- in one transaction per repository, so a manifest is
// never in the index without its tag, and the untagged collection never sees
// a tagged manifest as untagged.
//
// What is taken is what `index.json` reaches: the manifests it lists, what
// they hold, and so on down. A blob in the layout that nothing reaches is left
// where it is.
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/oci"
)

// Options narrow and shape an import.
type Options struct {
	// Repo names the repository a single layout is taken in as. It is
	// required when the source is one layout, and refused when it is a
	// directory of them, whose repositories are named by their paths.
	Repo string

	// Only, when not empty, is the repositories to take; the rest are
	// skipped.
	Only []string

	// Exclude skips the repositories under any of these prefixes: a prefix
	// covers itself and the names that continue it after a slash.
	Exclude []string

	// Refuse reports a repository that must not be taken in, and why: one
	// that is a pull-through cache, say. nil refuses nothing.
	Refuse func(repo string) string

	// Verify hashes every blob before it is taken in, and refuses one that
	// is not its digest. Without it a blob is taken to be what its name
	// says: one that is not is what every repository in the store holding
	// that digest is served from then on (see [flob.OsStore.Adopt]).
	Verify bool

	// Copy takes in by copying a blob that cannot be linked: one on another
	// filesystem, or one the process may not link. Without it either stops
	// the import.
	Copy bool

	// MaxManifestSize is the largest manifest read; zero is 4 MiB.
	MaxManifestSize int64
}

// Report is what an import found and did.
type Report struct {
	Repositories []RepoReport `json:"repositories"`

	// Skipped is the repositories found and not taken, with why.
	Skipped map[string]string `json:"skipped,omitempty"`
}

// RepoReport is one repository's import.
type RepoReport struct {
	Repo string `json:"repo"`

	Manifests int `json:"manifests"`
	Tags      int `json:"tags"`

	// Linked is the blobs taken in by link, Copied by copy, and Present
	// the ones the repository already held.
	Linked  int   `json:"linked"`
	Copied  int   `json:"copied"`
	Present int   `json:"present"`
	Bytes   int64 `json:"bytes"`

	// Missing is the manifests the layout lists that it does not hold all
	// of, which are not taken, nor are the tags on them, with why.
	Missing map[string]string `json:"missing,omitempty"`

	// BadTags is `ref.name`s that are not a tag, which are not taken.
	BadTags []string `json:"bad_tags,omitempty"`
}

// Import takes the layout, or the directory of layouts, at src into stores
// and ix. It runs again over what it took as over anything else: what the
// store and the index already have is left, and tags are pointed where the
// layout points them.
//
// Each repository is taken whole or reported; one that fails does not stop
// the others, except for a blob that cannot be linked without Copy, which
// would fail the same way for every one.
func Import(ctx context.Context, src string, stores flob.Stores, ix index.Index, o Options) (Report, error) {
	if o.MaxManifestSize <= 0 {
		o.MaxManifestSize = 4 << 20
	}
	r := Report{Skipped: map[string]string{}}

	layouts, err := find(src, o.Repo)
	if err != nil {
		return r, err
	}

	var errs []error
	for _, l := range layouts {
		if why := o.skip(l.repo); why != "" {
			r.Skipped[l.repo] = why
			continue
		}
		rr, err := importRepo(ctx, l, stores.Use(l.repo), ix, o)
		r.Repositories = append(r.Repositories, rr)
		if err == nil {
			continue
		}
		err = fmt.Errorf("%s: %w", l.repo, err)
		if errors.Is(err, errCannotLink) {
			return r, errors.Join(append(errs, err)...)
		}
		errs = append(errs, err)
	}
	if len(r.Skipped) == 0 {
		r.Skipped = nil
	}
	return r, errors.Join(errs...)
}

func (o Options) skip(repo string) string {
	if !oci.ValidName(repo) {
		return "not a repository name"
	}
	if len(o.Only) > 0 && !slices.Contains(o.Only, repo) {
		return "not asked for"
	}
	for _, p := range o.Exclude {
		if blob.Covers(p, repo) {
			return "excluded by " + p
		}
	}
	if o.Refuse != nil {
		return o.Refuse(repo)
	}
	return ""
}

// layout is an OCI image layout and the repository it is taken in as.
type layout struct {
	dir  string
	repo string
}

// find is the layouts at src: src itself when it is one, or every layout
// under it, named by its path. A layout may hold another below it, as zot
// keeps `a` and `a/b`; directories whose names begin with a dot (zot's
// `.uploads`, `.sync`) and `blobs` are not searched.
func find(src, repo string) ([]layout, error) {
	if isLayout(src) {
		if repo == "" {
			return nil, errors.New("the source is one layout: say which repository it is")
		}
		return []layout{{dir: src, repo: repo}}, nil
	}
	if repo != "" {
		return nil, errors.New("the source is a directory of layouts, each named by its path: a repository name is not taken")
	}
	var out []layout
	err := filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() {
			return nil
		}
		if p != src && (strings.HasPrefix(e.Name(), ".") || e.Name() == "blobs") {
			return filepath.SkipDir
		}
		if p != src && isLayout(p) {
			rel, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			out = append(out, layout{dir: p, repo: filepath.ToSlash(rel)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no OCI image layout in it", src)
	}
	return out, nil
}

func isLayout(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, v1.ImageLayoutFile))
	return err == nil && fi.Mode().IsRegular()
}

// errCannotLink is a blob that cannot be linked into the store, without Copy
// to fall back on.
var errCannotLink = errors.New("cannot link")

// manifest is one manifest the layout reaches, read and parsed.
type manifest struct {
	desc  v1.Descriptor
	m     *oci.Manifest
	added time.Time
}

func importRepo(ctx context.Context, l layout, s flob.Store, ix index.Index, o Options) (RepoReport, error) {
	rr := RepoReport{Repo: l.repo, Missing: map[string]string{}}
	defer func() {
		if len(rr.Missing) == 0 {
			rr.Missing = nil
		}
	}()

	if err := checkLayoutVersion(l.dir); err != nil {
		return rr, err
	}
	var top v1.Index
	if err := readJSON(filepath.Join(l.dir, v1.ImageIndexFile), 4<<20, &top); err != nil {
		return rr, err
	}

	// Every manifest the layout reaches, and every blob, by digest. A
	// manifest that is not whole -- it or something it holds is not in the
	// layout -- is not taken, and neither is anything that holds it.
	var (
		manifests = map[digest.Digest]*manifest{}
		order     []digest.Digest
		blobs     = map[digest.Digest]int64{}
		broken    = map[digest.Digest]string{}
	)
	var visit func(d v1.Descriptor) string
	visit = func(d v1.Descriptor) string {
		if why, ok := broken[d.Digest]; ok {
			return why
		}
		if _, ok := manifests[d.Digest]; ok {
			return ""
		}
		mf, why := readManifest(l.dir, d, o.MaxManifestSize)
		if why == "" {
			for _, c := range mf.m.Manifests {
				if w := visit(c); w != "" {
					why = c.Digest.String() + ": " + w
					break
				}
			}
		}
		if why == "" {
			for _, b := range mf.m.Blobs() {
				if !oci.Distributable(b) {
					continue
				}
				fi, err := os.Stat(blobPath(l.dir, b.Digest))
				if err != nil || !fi.Mode().IsRegular() {
					why = b.Digest.String() + ": not in the layout"
					break
				}
				if fi.Size() != b.Size {
					why = fmt.Sprintf("%s: %d bytes, and listed as %d", b.Digest, fi.Size(), b.Size)
					break
				}
			}
		}
		if why != "" {
			broken[d.Digest] = why
			return why
		}
		manifests[d.Digest] = mf
		order = append(order, d.Digest)
		blobs[d.Digest] = d.Size
		for _, b := range mf.m.Blobs() {
			if oci.Distributable(b) {
				blobs[b.Digest] = b.Size
			}
		}
		return ""
	}

	type tag struct {
		name string
		d    digest.Digest
	}
	var tags []tag
	for _, d := range top.Manifests {
		if !oci.IsManifestMediaType(d.MediaType) {
			continue
		}
		if why := visit(d); why != "" {
			rr.Missing[d.Digest.String()] = why
			continue
		}
		name, ok := d.Annotations[v1.AnnotationRefName]
		if !ok {
			continue
		}
		t, ok := tagOf(name)
		if !ok {
			rr.BadTags = append(rr.BadTags, name)
			continue
		}
		tags = append(tags, tag{t, d.Digest})
	}

	// The blobs first, so that nothing in the index is ever without its
	// content. Sorted, so a run that stopped and a run again do the same
	// work in the same order.
	ds := make([]digest.Digest, 0, len(blobs))
	for d := range blobs {
		ds = append(ds, d)
	}
	slices.Sort(ds)
	fsStore, linkable := osStore(s)
	for _, d := range ds {
		if err := ctx.Err(); err != nil {
			return rr, err
		}
		how, err := take(ctx, s, fsStore, linkable, l.dir, d, blobs[d], o)
		if err != nil {
			return rr, fmt.Errorf("%s: %w", d, err)
		}
		switch how {
		case "linked":
			rr.Linked++
			rr.Bytes += blobs[d]
		case "copied":
			rr.Copied++
			rr.Bytes += blobs[d]
		case "present":
			rr.Present++
		}
	}

	err := ix.Tx(ctx, l.repo, func(tx index.Index) error {
		if _, err := tx.Repo().Ensure(ctx, l.repo); err != nil {
			return err
		}
		for _, d := range order {
			mf := manifests[d]
			rec := index.Manifest{
				Digest:       d,
				MediaType:    mf.m.MediaType,
				ArtifactType: mf.m.ArtifactType,
				Size:         mf.desc.Size,
				Annotations:  mf.m.Annotations,
				CreatedAt:    mf.added,
			}
			if mf.m.Subject != nil {
				rec.Subject = mf.m.Subject.Digest
			}
			if _, err := tx.Manifest().Get(ctx, l.repo, d); err == nil {
				continue
			} else if !errors.Is(err, index.ErrNotFound) {
				return err
			}
			if err := tx.Manifest().Put(ctx, l.repo, rec, mf.m.Holds()); err != nil {
				return err
			}
			rr.Manifests++
		}
		for _, t := range tags {
			from := digest.Digest("")
			cur, err := tx.Tag().Get(ctx, l.repo, t.name)
			switch {
			case err == nil:
				from = cur.Digest
			case !errors.Is(err, index.ErrNotFound):
				return err
			}
			if from == t.d {
				continue
			}
			if err := tx.Tag().Set(ctx, l.repo, t.name, t.d, from); err != nil {
				return fmt.Errorf("tag %s: %w", t.name, err)
			}
			rr.Tags++
		}
		return nil
	})
	return rr, err
}

// take puts the blob d of the layout at dir into s, answering how: `linked`,
// `copied`, or `present` when s already held it.
func take(ctx context.Context, s flob.Store, fsStore flob.OsStore, linkable bool, dir string, d digest.Digest, size int64, o Options) (string, error) {
	path := blobPath(dir, d)
	if linkable {
		fi, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		_, err = fsStore.Adopt(ctx, flob.Meta{Digest: flob.Digest(d)}, path, flob.AdoptOptions{Added: fi.ModTime(), Verify: o.Verify})
		switch {
		case err == nil:
			return "linked", nil
		case errors.Is(err, flob.ErrAlreadyExists):
			return "present", nil
		case errors.Is(err, syscall.EXDEV), errors.Is(err, fs.ErrPermission):
			if !o.Copy {
				return "", fmt.Errorf("%w: %w (copy instead with Copy)", errCannotLink, err)
			}
		default:
			return "", err
		}
	} else if !o.Copy {
		return "", fmt.Errorf("%w: the repository's store is not a filesystem one (copy instead with Copy)", errCannotLink)
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// Add checks what it reads against the digest, so a copy is verified
	// whether or not it was asked.
	_, err = s.Add(ctx, flob.Meta{Digest: flob.Digest(d), Size: size}, f)
	switch {
	case err == nil:
		return "copied", nil
	case errors.Is(err, flob.ErrAlreadyExists):
		return "present", nil
	}
	return "", err
}

// osStore is the filesystem store under s, through whatever wraps it.
func osStore(s flob.Store) (flob.OsStore, bool) {
	for {
		switch v := s.(type) {
		case flob.OsStore:
			return v, true
		case *flob.OsStore:
			if v == nil {
				return flob.OsStore{}, false
			}
			return *v, true
		}
		u, ok := s.(interface{ Unwrap() flob.Store })
		if !ok {
			return flob.OsStore{}, false
		}
		s = u.Unwrap()
	}
}

// readManifest reads the manifest d of the layout at dir, or says why it
// cannot be taken.
func readManifest(dir string, d v1.Descriptor, max int64) (*manifest, string) {
	if err := d.Digest.Validate(); err != nil {
		return nil, "not a digest"
	}
	if d.Size > max {
		return nil, fmt.Sprintf("%d bytes, more than a manifest may be", d.Size)
	}
	path := blobPath(dir, d.Digest)
	f, err := os.Open(path)
	if err != nil {
		return nil, "not in the layout"
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err.Error()
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err.Error()
	}
	if int64(len(b)) != d.Size {
		return nil, fmt.Sprintf("%d bytes, and listed as %d", len(b), d.Size)
	}
	// A manifest is small, so it is checked whether or not blobs are: what
	// it says it holds is what the index will say.
	if !d.Digest.Algorithm().Available() {
		return nil, "a digest algorithm this registry does not compute"
	}
	if d.Digest.Algorithm().FromBytes(b) != d.Digest {
		return nil, "its content is not its digest"
	}
	m, err := oci.ParseManifest(d.MediaType, b)
	if err != nil {
		return nil, err.Error()
	}
	return &manifest{desc: d, m: m, added: fi.ModTime()}, ""
}

func blobPath(dir string, d digest.Digest) string {
	return filepath.Join(dir, v1.ImageBlobsDir, d.Algorithm().String(), d.Encoded())
}

func checkLayoutVersion(dir string) error {
	var v v1.ImageLayout
	if err := readJSON(filepath.Join(dir, v1.ImageLayoutFile), 1<<10, &v); err != nil {
		return err
	}
	if v.Version != v1.ImageLayoutVersion {
		return fmt.Errorf("%s: imageLayoutVersion %q, not %q", v1.ImageLayoutFile, v.Version, v1.ImageLayoutVersion)
	}
	return nil
}

func readJSON(path string, max int64, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > max {
		return fmt.Errorf("%s: more than %d bytes", filepath.Base(path), max)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

// tagOf is the tag a layout's `ref.name` names. The specification has it a
// tag, and tools write a whole reference there too, `registry/repo:tag`; one
// that names a digest, or nothing a tag can be, is not one.
func tagOf(name string) (string, bool) {
	if strings.Contains(name, "@") {
		return "", false
	}
	if i := strings.LastIndex(name, ":"); i >= 0 && i > strings.LastIndex(name, "/") {
		name = name[i+1:]
	} else if strings.Contains(name, "/") {
		return "", false
	}
	return name, oci.ValidTag(name)
}
