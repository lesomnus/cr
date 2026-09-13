package registry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx/log"
	"github.com/opencontainers/go-digest"
	"golang.org/x/sync/errgroup"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/gc"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/oci"
)

// referenceOf parses a manifest path's reference, answering what the spec
// wants for each way it can be malformed.
func referenceOf(arg string, missing func(any) *oci.Error) (oci.Reference, error) {
	ref, err := oci.ParseReference(arg)
	if err == nil {
		return ref, nil
	}
	if strings.Contains(arg, ":") {
		return ref, oci.ErrDigestInvalid(err.Error())
	}
	return ref, missing(err.Error())
}

// accepts reports whether a request's `Accept` admits a manifest of type t.
// Only a request that names manifest types, and not t, is refused: one that
// names none, or only `application/json`, gets what is there.
func accepts(values []string, t string) bool {
	named := false
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil {
				continue
			}
			if mt == t || mt == "*/*" {
				return true
			}
			if oci.IsManifestMediaType(mt) {
				named = true
			}
		}
	}
	return !named
}

func (g *Registry) getManifest(w http.ResponseWriter, r *http.Request, name, arg string) {
	ctx := r.Context()
	ref, err := referenceOf(arg, oci.ErrManifestUnknown)
	if err != nil {
		g.fail(w, r, err)
		return
	}

	ix := g.c.Index
	d := ref.Digest
	if ref.Tag != "" {
		t, err := ix.Tag().Get(ctx, name, ref.Tag)
		if errors.Is(err, index.ErrNotFound) {
			g.fail(w, r, oci.ErrManifestUnknown(ref.Tag))
			return
		}
		if err != nil {
			g.fail(w, r, err)
			return
		}
		d = t.Digest
	}

	m, err := ix.Manifest().Get(ctx, name, d)
	if errors.Is(err, index.ErrNotFound) {
		g.fail(w, r, oci.ErrManifestUnknown(ref.String()))
		return
	}
	if err != nil {
		g.fail(w, r, err)
		return
	}
	if !accepts(r.Header.Values("Accept"), m.MediaType) {
		g.fail(w, r, oci.ErrManifestUnknown("the manifest is "+m.MediaType+", which the request does not accept"))
		return
	}

	h := w.Header()
	h.Set("Content-Type", m.MediaType)
	h.Set("Docker-Content-Digest", d.String())
	h.Set("Etag", `"`+d.String()+`"`)
	if r.Method == http.MethodHead {
		h.Set("Content-Length", strconv.FormatInt(m.Size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	rc, info, err := g.store(name).Open(ctx, flob.Digest(d))
	if errors.Is(err, flob.ErrNotExist) {
		log.From(ctx).WarnContext(ctx, "indexed manifest missing from the store",
			slog.String("repo", name), slog.String("digest", d.String()))
		g.fail(w, r, oci.ErrManifestUnknown(ref.String()))
		return
	}
	if err != nil {
		g.fail(w, r, err)
		return
	}
	defer rc.Close()

	h.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, rc)

	ix.Pulled().Touch(name, ref.Tag, d, g.c.Now())
}

func (g *Registry) putManifest(w http.ResponseWriter, r *http.Request, name, arg string) {
	ctx := r.Context()
	ref, err := referenceOf(arg, oci.ErrManifestInvalid)
	if err != nil {
		g.fail(w, r, err)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, g.c.MaxManifestSize+1))
	if err != nil {
		g.fail(w, r, err)
		return
	}
	if int64(len(body)) > g.c.MaxManifestSize {
		g.fail(w, r, oci.NewError(http.StatusRequestEntityTooLarge, oci.CodeSizeInvalid, "the manifest is larger than "+strconv.FormatInt(g.c.MaxManifestSize, 10)+" bytes"))
		return
	}

	algo := digest.Canonical
	if ref.Digest != "" {
		algo = ref.Digest.Algorithm()
	}
	d := algo.FromBytes(body)
	if ref.Digest != "" && ref.Digest != d {
		g.fail(w, r, oci.ErrDigestInvalid("the body hashes to "+d.String()))
		return
	}

	m, err := oci.ParseManifest(r.Header.Get("Content-Type"), body)
	if err != nil {
		g.fail(w, r, oci.ErrManifestInvalid(err.Error()))
		return
	}

	// The bytes first and the names after: a crash between the two leaves a
	// manifest nothing points at, which retention covers.
	s := g.store(name)
	if _, err := s.Add(ctx, flob.Meta{Digest: flob.Digest(d)}, bytes.NewReader(body)); err != nil && !errors.Is(err, flob.ErrAlreadyExists) {
		g.fail(w, r, err)
		return
	}

	rec := index.Manifest{
		Digest:       d,
		MediaType:    m.MediaType,
		ArtifactType: m.ArtifactType,
		Size:         int64(len(body)),
		Annotations:  m.Annotations,
		CreatedAt:    g.c.Now(),
	}
	if m.Subject != nil {
		rec.Subject = m.Subject.Digest
	}

	err = g.c.Index.Tx(ctx, name, func(ix index.Index) error {
		// Inside the lock, because a blob delete or a sweep erases under it:
		// a blob seen here cannot be erased before this commits, and one erased
		// before this looked is reported missing rather than lost.
		missing, err := g.missing(ctx, ix, s, name, m)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return oci.ErrManifestBlobUnknown(missing)
		}
		if _, err := ix.Repo().Ensure(ctx, name); err != nil {
			return err
		}
		if err := ix.Manifest().Put(ctx, name, rec, m.Holds()); err != nil {
			return err
		}
		if ref.Tag == "" {
			return nil
		}
		return g.setTag(ctx, ix, s, name, ref.Tag, d)
	})
	if err != nil {
		g.fail(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Location", manifestPath(name, d))
	h.Set("Docker-Content-Digest", d.String())
	h.Set("Content-Length", "0")
	if m.Subject != nil {
		h.Set("OCI-Subject", m.Subject.Digest.String())
	}
	w.WriteHeader(http.StatusCreated)
}

// missing answers the digests m names that the repository does not have:
// blobs by a stat of the store, child manifests by the index.
func (g *Registry) missing(ctx context.Context, ix index.Index, s flob.Store, name string, m *oci.Manifest) ([]string, error) {
	var (
		mu      sync.Mutex
		missing []string
	)

	eg, ectx := errgroup.WithContext(ctx)
	eg.SetLimit(8)
	seen := map[digest.Digest]struct{}{}
	for _, b := range m.Blobs() {
		if _, ok := seen[b.Digest]; ok {
			continue
		}
		seen[b.Digest] = struct{}{}
		if _, ok := g.wellKnown(b.Digest); ok {
			continue
		}
		eg.Go(func() error {
			_, err := s.Stat(ectx, flob.Digest(b.Digest))
			if errors.Is(err, flob.ErrNotExist) || errors.Is(err, flob.ErrInvalidDigest) {
				mu.Lock()
				missing = append(missing, b.Digest.String())
				mu.Unlock()
				return nil
			}
			return err
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	for _, c := range m.Manifests {
		if _, ok := seen[c.Digest]; ok {
			continue
		}
		seen[c.Digest] = struct{}{}
		_, err := ix.Manifest().Get(ctx, name, c.Digest)
		if errors.Is(err, index.ErrNotFound) {
			missing = append(missing, c.Digest.String())
			continue
		}
		if err != nil {
			return nil, err
		}
	}

	slices.Sort(missing)
	return missing, nil
}

// setTag points tag at d in a transaction that holds the repository's lock,
// and keeps the tag as a label on the manifest so a rebuild can recover it.
func (g *Registry) setTag(ctx context.Context, ix index.Index, s flob.Store, name, tag string, d digest.Digest) error {
	from := digest.Digest("")
	cur, err := ix.Tag().Get(ctx, name, tag)
	switch {
	case err == nil:
		from = cur.Digest
	case !errors.Is(err, index.ErrNotFound):
		return err
	}
	if from != d {
		op := auth.TagCreate
		if from != "" {
			op = auth.TagMove
		}
		if err := auth.CallerFrom(ctx).CheckTag(name, tag, op); err != nil {
			return oci.ErrDenied(err.Error())
		}
	}
	if err := ix.Tag().Set(ctx, name, tag, d, from); err != nil {
		return err
	}
	if from != d {
		if from != "" {
			g.label(ctx, s, from, tag, false)
		}
		g.label(ctx, s, d, tag, true)
	}
	return nil
}

// label adds or removes one tag in d's labels, best effort.
func (g *Registry) label(ctx context.Context, s flob.Store, d digest.Digest, tag string, add bool) {
	if err := blob.LabelTag(ctx, s, d, tag, add); err != nil {
		log.From(ctx).WarnContext(ctx, "tag label", slog.String("digest", d.String()), slog.String("tag", tag), slog.String("err", err.Error()))
	}
}

func (g *Registry) deleteManifest(w http.ResponseWriter, r *http.Request, name, arg string) {
	ctx := r.Context()
	ref, err := referenceOf(arg, oci.ErrManifestUnknown)
	if err != nil {
		g.fail(w, r, err)
		return
	}
	s := g.store(name)

	if ref.Tag != "" {
		err := g.c.Index.Tx(ctx, name, func(ix index.Index) error {
			t, err := ix.Tag().Get(ctx, name, ref.Tag)
			if errors.Is(err, index.ErrNotFound) {
				return oci.ErrManifestUnknown(ref.Tag)
			}
			if err != nil {
				return err
			}
			if err := auth.CallerFrom(ctx).CheckTag(name, ref.Tag, auth.TagDelete); err != nil {
				return oci.ErrDenied(err.Error())
			}
			if err := ix.Tag().Erase(ctx, name, ref.Tag); err != nil {
				return err
			}
			g.label(ctx, s, t.Digest, ref.Tag, false)
			return nil
		})
		if err != nil {
			g.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	d := ref.Digest
	var released []digest.Digest
	err = g.c.Index.Tx(ctx, name, func(ix index.Index) error {
		if _, err := ix.Manifest().Get(ctx, name, d); errors.Is(err, index.ErrNotFound) {
			return oci.ErrManifestUnknown(d.String())
		} else if err != nil {
			return err
		}
		ts, err := ix.Tag().Of(ctx, name, d)
		if err != nil {
			return err
		}
		for _, t := range ts {
			if err := auth.CallerFrom(ctx).CheckTag(name, t.Name, auth.TagDelete); err != nil {
				return oci.ErrDenied(err.Error())
			}
		}
		for _, t := range ts {
			if err := ix.Tag().Erase(ctx, name, t.Name); err != nil {
				return err
			}
		}
		released, err = ix.Manifest().Erase(ctx, name, d)
		return err
	})
	if err != nil {
		g.fail(w, r, err)
		return
	}

	g.release(context.WithoutCancel(ctx), name, append(released, d))
	w.WriteHeader(http.StatusAccepted)
}

// release erases what a delete released, leaking to the sweep on failure.
func (g *Registry) release(ctx context.Context, name string, ds []digest.Digest) {
	if err := gc.Release(ctx, g.c.Index, g.store(name), name, ds); err != nil {
		log.From(ctx).WarnContext(ctx, "release", slog.String("repo", name), slog.String("err", err.Error()))
	}
}
