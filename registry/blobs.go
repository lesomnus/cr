package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx/log"
	"github.com/opencontainers/go-digest"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/oci"
)

func wellKnown(d digest.Digest) ([]byte, bool) { return blob.WellKnown(d) }

func (g *Registry) getBlob(w http.ResponseWriter, r *http.Request, name, arg string) {
	ctx := r.Context()
	d, err := oci.ParseDigest(arg)
	if err != nil {
		g.fail(w, r, oci.ErrDigestInvalid(err.Error()))
		return
	}

	h := w.Header()
	h.Set("Docker-Content-Digest", d.String())
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Etag", `"`+d.String()+`"`)

	if b, ok := g.wellKnown(d); ok {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
		return
	}

	s := g.store(name)
	if r.Method == http.MethodHead {
		info, err := s.Stat(ctx, flob.Digest(d))
		if err != nil {
			g.fail(w, r, blobErr(d, err))
			return
		}
		size, err := info.Size(ctx)
		if err != nil {
			g.fail(w, r, blobErr(d, err))
			return
		}
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		h.Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		return
	}

	if g.c.Redirect {
		if p, ok := flob.AsPresigner(s); ok {
			// A store behind a cache signs only what it already holds; a
			// miss is answered by streaming, which is what fills it.
			loc, _, err := p.PresignOpen(ctx, flob.Digest(d), g.c.RedirectTTL)
			if err == nil {
				h.Del("Content-Type")
				h.Del("Etag")
				h.Set("Location", loc)
				w.WriteHeader(http.StatusTemporaryRedirect)
				return
			}
			if !errors.Is(err, flob.ErrNotExist) {
				log.From(ctx).WarnContext(ctx, "presign", slog.String("repo", name), slog.String("digest", d.String()), slog.String("err", err.Error()))
			}
		}
	}

	// A read through a cache fills the store as it goes, and the fill is
	// tied to the context the blob was opened with. The request's context
	// ends the moment this handler returns, which is before a store slower
	// than the client -- a bucket -- has finished writing what the client
	// already has. Closing the reader is still what stops a fill short.
	openCtx := ctx
	if g.proxies.of(name) != nil {
		openCtx = context.WithoutCancel(ctx)
	}
	rc, _, err := s.Open(openCtx, flob.Digest(d))
	if err != nil {
		g.fail(w, r, blobErr(d, err))
		return
	}
	defer rc.Close()
	http.ServeContent(w, r, "", time.Time{}, rc)
}

func blobErr(d digest.Digest, err error) error {
	if errors.Is(err, flob.ErrNotExist) {
		return oci.ErrBlobUnknown(d.String())
	}
	if errors.Is(err, flob.ErrInvalidDigest) {
		return oci.ErrDigestInvalid(err.Error())
	}
	return err
}

func (g *Registry) deleteBlob(w http.ResponseWriter, r *http.Request, name, arg string) {
	ctx := r.Context()
	d, err := oci.ParseDigest(arg)
	if err != nil {
		g.fail(w, r, oci.ErrDigestInvalid(err.Error()))
		return
	}
	if _, ok := g.wellKnown(d); ok {
		// Constant content is in every repository, so there is nothing to
		// delete; saying it was would be contradicted by the next HEAD.
		g.fail(w, r, oci.ErrUnsupported("a constant blob is present in every repository and cannot be deleted"))
		return
	}

	s := g.store(name)
	err = g.c.Index.Tx(ctx, name, func(ix index.Index) error {
		// A blob a manifest still holds would leave that manifest unpullable,
		// which is worse than refusing.
		held, err := ix.Manifest().Holds(ctx, name, d)
		if err != nil {
			return err
		}
		if held {
			return oci.ErrDenied("the blob is held by a manifest in this repository")
		}
		if _, err := ix.Manifest().Get(ctx, name, d); err == nil {
			return oci.ErrDenied("the digest is a manifest; delete it through the manifests endpoint")
		} else if !errors.Is(err, index.ErrNotFound) {
			return err
		}
		if _, err := s.Stat(ctx, flob.Digest(d)); err != nil {
			return blobErr(d, err)
		}
		return s.Erase(ctx, flob.Digest(d))
	})
	if err != nil {
		g.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (g *Registry) created(w http.ResponseWriter, name string, d digest.Digest) {
	h := w.Header()
	h.Set("Location", blobPath(name, d))
	h.Set("Docker-Content-Digest", d.String())
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// postUpload is end-4a, end-4b and end-11: a session, a whole blob, or a mount.
func (g *Registry) postUpload(w http.ResponseWriter, r *http.Request, name string) {
	q := r.URL.Query()
	switch {
	case q.Has("mount"):
		g.mount(w, r, name)
	case q.Has("digest"):
		g.putBlob(w, r, name, q.Get("digest"))
	default:
		g.beginUpload(w, r, name)
	}
}

func (g *Registry) putBlob(w http.ResponseWriter, r *http.Request, name, arg string) {
	ctx := r.Context()
	d, err := oci.ParseDigest(arg)
	if err != nil {
		g.fail(w, r, oci.ErrDigestInvalid(err.Error()))
		return
	}

	if b, ok := g.wellKnown(d); ok {
		got, err := io.ReadAll(io.LimitReader(r.Body, int64(len(b))+1))
		if err != nil {
			g.fail(w, r, err)
			return
		}
		if !bytes.Equal(got, b) {
			g.fail(w, r, oci.ErrDigestInvalid(d.String()))
			return
		}
		g.created(w, name, d)
		return
	}

	_, err = g.store(name).Add(ctx, flob.Meta{Digest: flob.Digest(d)}, r.Body)
	switch {
	case err == nil:
	case errors.Is(err, flob.ErrAlreadyExists):
		// flob answers before reading when the digest is already there; the
		// body still has to be read or the connection stalls.
		io.Copy(io.Discard, r.Body)
	case errors.Is(err, flob.ErrDigestMismatch), errors.Is(err, flob.ErrInvalidDigest):
		g.fail(w, r, oci.ErrDigestInvalid(err.Error()))
		return
	default:
		g.fail(w, r, err)
		return
	}
	g.created(w, name, d)
}

func (g *Registry) beginUpload(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))

	st, ok := flob.AsStager(g.store(name))
	if !ok {
		g.fail(w, r, oci.ErrUnsupported("the store takes no chunked uploads; push with ?digest="))
		return
	}

	// The algorithm is fixed when the session starts, and the spec gives a
	// client no way to say it before the final PUT. A client that knows asks
	// with `digest-algorithm`; the rest get sha256.
	var algo digest.Algorithm
	if v := r.URL.Query().Get("digest-algorithm"); v != "" {
		algo = digest.Algorithm(v)
		switch algo {
		case digest.SHA256, digest.SHA384, digest.SHA512:
		default:
			g.fail(w, r, oci.ErrDigestInvalid(fmt.Sprintf("unsupported algorithm %q", v)))
			return
		}
	}

	stage, err := st.Begin(ctx, algo)
	if err != nil {
		g.fail(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Location", uploadPath(name, stage.ID()))
	h.Set("Docker-Upload-UUID", stage.ID())
	h.Set("Range", "0-0")
	h.Set("Content-Length", "0")
	if g.c.ChunkMinLength > 0 {
		h.Set("OCI-Chunk-Min-Length", strconv.FormatInt(g.c.ChunkMinLength, 10))
	}
	w.WriteHeader(http.StatusAccepted)
}

func (g *Registry) mount(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	q := r.URL.Query()
	d, err := oci.ParseDigest(q.Get("mount"))
	from := q.Get("from")
	if err != nil || from == "" || !oci.ValidName(from) || len(auth.CallerFrom(ctx).Allowed(from, auth.ActionPull)) == 0 {
		// The spec's answer to a mount that cannot happen is a session, and
		// a source the caller may not pull is a mount that cannot happen.
		g.beginUpload(w, r, name)
		return
	}
	if _, ok := g.wellKnown(d); ok {
		g.created(w, name, d)
		return
	}

	dst := g.store(name)
	src := g.store(from)
	if l, ok := flob.AsLinker(dst); ok {
		_, err := l.Link(ctx, flob.Digest(d), src)
		switch {
		case err == nil, errors.Is(err, flob.ErrAlreadyExists):
			g.created(w, name, d)
			return
		case errors.Is(err, flob.ErrNotExist):
			g.beginUpload(w, r, name)
			return
		case errors.Is(err, flob.ErrIncompatibleStore):
			// The two repositories are on different pools; copy below.
		default:
			g.fail(w, r, err)
			return
		}
	}

	rc, _, err := src.Open(ctx, flob.Digest(d))
	if errors.Is(err, flob.ErrNotExist) {
		g.beginUpload(w, r, name)
		return
	}
	if err != nil {
		g.fail(w, r, err)
		return
	}
	defer rc.Close()
	if _, err := dst.Add(ctx, flob.Meta{Digest: flob.Digest(d)}, rc); err != nil && !errors.Is(err, flob.ErrAlreadyExists) {
		g.fail(w, r, err)
		return
	}
	g.created(w, name, d)
}

func validStageID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// resume answers the session named by id in the repository's store. Every way
// of it not being there is BLOB_UPLOAD_UNKNOWN; which one goes in the detail.
func (g *Registry) resume(r *http.Request, name, id string) (flob.Stage, error) {
	if !validStageID(id) {
		return nil, oci.ErrBlobUploadUnknown("no such upload")
	}
	st, ok := flob.AsStager(g.store(name))
	if !ok {
		return nil, oci.ErrBlobUploadUnknown("no such upload")
	}
	stage, err := st.Resume(r.Context(), id)
	if err != nil {
		return nil, stageErr(err)
	}
	return stage, nil
}

func stageErr(err error) error {
	switch {
	case errors.Is(err, flob.ErrNotExist):
		return oci.ErrBlobUploadUnknown("no such upload")
	case errors.Is(err, flob.ErrStageExpired):
		return oci.ErrBlobUploadUnknown("the upload expired")
	case errors.Is(err, flob.ErrStageClosed):
		return oci.ErrBlobUploadUnknown("the upload was committed or cancelled")
	case errors.Is(err, flob.ErrStageConflict):
		return oci.ErrBlobUploadInvalid(err.Error())
	case errors.Is(err, flob.ErrDigestMismatch), errors.Is(err, flob.ErrInvalidDigest):
		return oci.ErrDigestInvalid(err.Error())
	}
	return err
}

// rangeOf is the `Range` a session at offset reports: the last byte it holds,
// inclusive, and `0-0` for none.
func rangeOf(offset int64) string {
	if offset <= 0 {
		return "0-0"
	}
	return "0-" + strconv.FormatInt(offset-1, 10)
}

func (g *Registry) session(w http.ResponseWriter, name, id string, offset int64) {
	h := w.Header()
	h.Set("Location", uploadPath(name, id))
	h.Set("Docker-Upload-UUID", id)
	h.Set("Range", rangeOf(offset))
	h.Set("Content-Length", "0")
}

func (g *Registry) statUpload(w http.ResponseWriter, r *http.Request, name, id string) {
	stage, err := g.resume(r, name, id)
	if err != nil {
		g.fail(w, r, err)
		return
	}
	info, err := stage.Stat(r.Context())
	if err != nil {
		g.fail(w, r, stageErr(err))
		return
	}
	if info.State != flob.StageActive {
		g.fail(w, r, oci.ErrBlobUploadUnknown("the upload was committed or cancelled"))
		return
	}
	g.session(w, name, id, info.Offset)
	w.WriteHeader(http.StatusNoContent)
}

// parseContentRange reads `<start>-<end>`, with or without a `bytes ` unit
// and a `/<size>` suffix, which clients disagree about.
func parseContentRange(v string) (start, end int64, ok bool) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "bytes"))
	v, _, _ = strings.Cut(v, "/")
	a, b, found := strings.Cut(v, "-")
	if !found {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	end, err2 := strconv.ParseInt(strings.TrimSpace(b), 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		return 0, 0, false
	}
	return start, end, true
}

// appendBody appends the request body at the offset its `Content-Range`
// names, or at the session's current offset when it names none.
func (g *Registry) appendBody(w http.ResponseWriter, r *http.Request, name string, stage flob.Stage) (int64, bool) {
	ctx := r.Context()

	expected := int64(-1)
	if v := r.Header.Get("Content-Range"); v != "" {
		start, end, ok := parseContentRange(v)
		if !ok || (r.ContentLength >= 0 && r.ContentLength != end-start+1) {
			g.outOfRange(w, r, name, stage)
			return 0, false
		}
		expected = start
	}
	if expected < 0 {
		info, err := stage.Stat(ctx)
		if err != nil {
			g.fail(w, r, stageErr(err))
			return 0, false
		}
		if info.State != flob.StageActive {
			g.fail(w, r, oci.ErrBlobUploadUnknown("the upload was committed or cancelled"))
			return 0, false
		}
		expected = info.Offset
	}

	offset, err := stage.Append(ctx, expected, r.Body)
	if errors.Is(err, flob.ErrOffsetMismatch) {
		g.outOfRange(w, r, name, stage)
		return 0, false
	}
	if err != nil {
		g.fail(w, r, stageErr(err))
		return 0, false
	}
	return offset, true
}

// outOfRange is 416 with the range the session actually holds, so the client
// can resume from it.
func (g *Registry) outOfRange(w http.ResponseWriter, r *http.Request, name string, stage flob.Stage) {
	info, err := stage.Stat(r.Context())
	if err != nil {
		g.fail(w, r, stageErr(err))
		return
	}
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	g.fail(w, r, oci.NewError(http.StatusRequestedRangeNotSatisfiable, oci.CodeBlobUploadInvalid, "out of order chunk").
		WithHeader("Location", uploadPath(name, stage.ID())).
		WithHeader("Range", rangeOf(info.Offset)).
		WithHeader("Docker-Upload-UUID", stage.ID()))
}

func (g *Registry) patchUpload(w http.ResponseWriter, r *http.Request, name, id string) {
	stage, err := g.resume(r, name, id)
	if err != nil {
		g.fail(w, r, err)
		return
	}
	offset, ok := g.appendBody(w, r, name, stage)
	if !ok {
		return
	}
	g.session(w, name, id, offset)
	w.WriteHeader(http.StatusAccepted)
}

func (g *Registry) putUpload(w http.ResponseWriter, r *http.Request, name, id string) {
	ctx := r.Context()
	d, err := oci.ParseDigest(r.URL.Query().Get("digest"))
	if err != nil {
		g.fail(w, r, oci.ErrDigestInvalid(err.Error()))
		return
	}
	stage, err := g.resume(r, name, id)
	if err != nil {
		g.fail(w, r, err)
		return
	}
	if r.ContentLength != 0 {
		if _, ok := g.appendBody(w, r, name, stage); !ok {
			return
		}
	}
	_, err = stage.Commit(ctx, flob.Meta{Digest: flob.Digest(d)})
	if errors.Is(err, flob.ErrDigestMismatch) {
		// A mismatch leaves the session as it was. It may only be the
		// algorithm: a session hashes with what it began with, and a client
		// that says nothing about it until this PUT got sha256.
		if info, serr := stage.Stat(ctx); serr == nil && info.State == flob.StageActive && info.Algorithm != d.Algorithm() {
			err = g.rehash(ctx, name, stage, d)
		}
	}
	if err != nil {
		g.fail(w, r, stageErr(err))
		return
	}
	g.created(w, name, d)
}

// rehash finishes a session whose algorithm is not the one the client named:
// it publishes the bytes under the digest the session computed and reads them
// back in under d. The first copy is left for the sweep rather than erased
// here, since a blob of that digest may also have been pushed on purpose.
// Rare, and twice the I/O; a client that says `digest-algorithm` on the POST
// never comes here.
func (g *Registry) rehash(ctx context.Context, name string, stage flob.Stage, d digest.Digest) error {
	meta, err := stage.Commit(ctx, flob.Meta{})
	if err != nil {
		return err
	}
	s := g.store(name)
	rc, _, err := s.Open(ctx, meta.Digest)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = s.Add(ctx, flob.Meta{Digest: flob.Digest(d)}, rc)
	if errors.Is(err, flob.ErrAlreadyExists) {
		return nil
	}
	return err
}

func (g *Registry) cancelUpload(w http.ResponseWriter, r *http.Request, name, id string) {
	stage, err := g.resume(r, name, id)
	if err != nil {
		g.fail(w, r, err)
		return
	}
	if err := stage.Abort(r.Context()); err != nil {
		g.fail(w, r, stageErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
