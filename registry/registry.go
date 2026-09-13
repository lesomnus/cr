// Package registry is the /v2 handler, written against flob and the index and
// nothing concrete.
package registry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx/log"
	"github.com/opencontainers/go-digest"

	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/oci"
)

// Config is what the handler is built from.
type Config struct {
	Stores flob.Stores
	Index  index.Index

	// MaxManifestSize is the largest manifest a push may carry; zero is 4 MiB.
	MaxManifestSize int64

	// ChunkMinLength is advertised as `OCI-Chunk-Min-Length` when an upload
	// starts; zero advertises nothing.
	ChunkMinLength int64

	// DisableWellKnown sends the constant blobs to the store like any other.
	DisableWellKnown bool

	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Registry answers the distribution API.
type Registry struct {
	c Config
}

func New(c Config) *Registry {
	if c.MaxManifestSize <= 0 {
		c.MaxManifestSize = 4 << 20
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return &Registry{c: c}
}

type route int

const (
	routeNone route = iota
	routeBlob
	routeUpload
	routeManifest
	routeTags
	routeReferrers
)

// parse splits what follows `/v2/` into the repository name, the route and
// the route's argument. A name may contain any of the words the routes are
// made of, so the route is read from the right.
func parse(rest string) (name string, r route, arg string) {
	segs := strings.Split(rest, "/")
	n := len(segs)
	join := func(k int) string { return strings.Join(segs[:k], "/") }
	switch {
	case n >= 3 && segs[n-2] == "tags" && segs[n-1] == "list":
		return join(n - 2), routeTags, ""
	case n >= 4 && segs[n-3] == "blobs" && segs[n-2] == "uploads":
		return join(n - 3), routeUpload, segs[n-1]
	case n >= 3 && segs[n-2] == "blobs" && segs[n-1] == "uploads":
		return join(n - 2), routeUpload, ""
	case n >= 3 && segs[n-2] == "blobs":
		return join(n - 2), routeBlob, segs[n-1]
	case n >= 3 && segs[n-2] == "manifests":
		return join(n - 2), routeManifest, segs[n-1]
	case n >= 3 && segs[n-2] == "referrers":
		return join(n - 2), routeReferrers, segs[n-1]
	}
	return "", routeNone, ""
}

func (g *Registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")

	p := r.URL.Path
	if p == "/v2" || p == "/v2/" {
		g.base(w, r)
		return
	}
	rest, ok := strings.CutPrefix(p, "/v2/")
	if !ok {
		g.fail(w, r, errNoRoute)
		return
	}
	if rest == "_catalog" {
		g.only(w, r, g.catalog, http.MethodGet)
		return
	}

	name, rt, arg := parse(rest)
	if rt == routeNone {
		g.fail(w, r, errNoRoute)
		return
	}
	if !oci.ValidName(name) {
		g.fail(w, r, oci.ErrNameInvalid(name))
		return
	}

	switch rt {
	case routeBlob:
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			g.getBlob(w, r, name, arg)
		case http.MethodDelete:
			g.deleteBlob(w, r, name, arg)
		default:
			g.fail(w, r, errMethod)
		}
	case routeUpload:
		if arg == "" {
			if r.Method != http.MethodPost {
				g.fail(w, r, errMethod)
				return
			}
			g.postUpload(w, r, name)
			return
		}
		switch r.Method {
		case http.MethodGet:
			g.statUpload(w, r, name, arg)
		case http.MethodPatch:
			g.patchUpload(w, r, name, arg)
		case http.MethodPut:
			g.putUpload(w, r, name, arg)
		case http.MethodDelete:
			g.cancelUpload(w, r, name, arg)
		default:
			g.fail(w, r, errMethod)
		}
	case routeManifest:
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			g.getManifest(w, r, name, arg)
		case http.MethodPut:
			g.putManifest(w, r, name, arg)
		case http.MethodDelete:
			g.deleteManifest(w, r, name, arg)
		default:
			g.fail(w, r, errMethod)
		}
	case routeTags:
		g.only(w, r, func(w http.ResponseWriter, r *http.Request) { g.listTags(w, r, name) }, http.MethodGet)
	case routeReferrers:
		g.only(w, r, func(w http.ResponseWriter, r *http.Request) { g.referrers(w, r, name, arg) }, http.MethodGet)
	}
}

var (
	errNoRoute = oci.NewError(http.StatusNotFound, oci.CodeUnsupported, "no such endpoint")
	errMethod  = oci.NewError(http.StatusMethodNotAllowed, oci.CodeUnsupported, "method not allowed")
)

func (g *Registry) only(w http.ResponseWriter, r *http.Request, h http.HandlerFunc, methods ...string) {
	for _, m := range methods {
		if r.Method == m || (m == http.MethodGet && r.Method == http.MethodHead) {
			h(w, r)
			return
		}
	}
	g.fail(w, r, errMethod)
}

func (g *Registry) base(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		g.fail(w, r, errMethod)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", "2")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		w.Write([]byte("{}"))
	}
}

// fail answers err. Anything that is not already an envelope error is the
// registry's own failure: logged here, and answered as 500 with nothing of it
// in the body.
func (g *Registry) fail(w http.ResponseWriter, r *http.Request, err error) {
	var e *oci.Error
	switch {
	case errors.As(err, &e):
	case errors.Is(err, index.ErrBusy):
		err = oci.ErrUnavailable(1, "the repository is being written to; retry")
	case errors.Is(err, context.Canceled):
		// The client went away; there is nobody to answer.
		return
	default:
		ctx := r.Context()
		log.From(ctx).ErrorContext(ctx, "registry",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("err", err.Error()),
		)
	}
	oci.WriteError(w, err)
}

func (g *Registry) store(name string) flob.Store {
	return g.c.Stores.Use(name)
}

func (g *Registry) wellKnown(d digest.Digest) ([]byte, bool) {
	if g.c.DisableWellKnown {
		return nil, false
	}
	return wellKnown(d)
}

func blobPath(name string, d digest.Digest) string {
	return "/v2/" + name + "/blobs/" + d.String()
}

func uploadPath(name, id string) string {
	return "/v2/" + name + "/blobs/uploads/" + id
}

func manifestPath(name string, d digest.Digest) string {
	return "/v2/" + name + "/manifests/" + d.String()
}

// pageOf reads `n` and `last`. An `n` that is not a number is ignored, which
// is what distribution does.
func pageOf(r *http.Request) (index.Page, bool) {
	q := r.URL.Query()
	p := index.Page{Last: q.Get("last")}
	given := false
	if s := q.Get("n"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			p.N = n
			given = true
		}
	}
	return p, given
}
