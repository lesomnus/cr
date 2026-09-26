package registry

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/oci"
)

func writeJSON(w http.ResponseWriter, r *http.Request, contentType string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		oci.WriteError(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(b)
	}
}

// next sets `Link` to the page after last, as the spec's pagination has it.
func next(w http.ResponseWriter, path string, n int, last string) {
	q := url.Values{}
	q.Set("n", strconv.Itoa(n))
	q.Set("last", last)
	w.Header().Set("Link", "<"+path+"?"+q.Encode()+`>; rel="next"`)
}

func (g *Registry) listTags(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	if _, err := g.c.Index.Repo().Get(ctx, name); err != nil {
		if errors.Is(err, index.ErrNotFound) {
			err = oci.ErrNameUnknown(name)
		}
		g.fail(w, r, err)
		return
	}

	p, given := pageOf(r)
	tags := []string{}
	if !given || p.N > 0 {
		vs, err := g.c.Index.Tag().List(ctx, name, p)
		if err != nil {
			g.fail(w, r, err)
			return
		}
		tags = append(tags, vs...)
	}
	if given && p.N > 0 && len(tags) == p.N {
		next(w, "/v2/"+name+"/tags/list", p.N, tags[len(tags)-1])
	}
	writeJSON(w, r, "application/json", struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}{name, tags})
}

func (g *Registry) catalog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, given := pageOf(r)
	repos := []string{}
	if !given || p.N > 0 {
		// Only what the caller may pull, and still a full page when there
		// is one: a page is read past the repositories it may not see.
		c := auth.CallerFrom(ctx)
		batch := max(p.N, 100)
		last := p.Last
		for {
			vs, err := g.c.Index.Repo().List(ctx, index.Page{Last: last, N: batch})
			if err != nil {
				g.fail(w, r, err)
				return
			}
			for _, v := range vs {
				if c.CanPull(v) {
					repos = append(repos, v)
				}
				if given && len(repos) == p.N {
					break
				}
			}
			if len(vs) < batch || (given && len(repos) == p.N) {
				break
			}
			last = vs[len(vs)-1]
		}
	}
	if given && p.N > 0 && len(repos) == p.N {
		next(w, "/v2/_catalog", p.N, repos[len(repos)-1])
	}
	writeJSON(w, r, "application/json", struct {
		Repositories []string `json:"repositories"`
	}{repos})
}

func (g *Registry) referrers(w http.ResponseWriter, r *http.Request, name, arg string) {
	ctx := r.Context()
	d, err := oci.ParseDigest(arg)
	if err != nil {
		g.fail(w, r, oci.ErrDigestInvalid(err.Error()))
		return
	}
	at := r.URL.Query().Get("artifactType")

	if p := g.proxies.of(name); p != nil {
		g.proxiedReferrers(w, r, p, name, d, at)
		return
	}

	ms, err := g.c.Index.Manifest().Referrers(ctx, name, d, at)
	if err != nil {
		g.fail(w, r, err)
		return
	}
	ds := make([]v1.Descriptor, 0, len(ms))
	for _, m := range ms {
		ds = append(ds, v1.Descriptor{
			MediaType:    m.MediaType,
			ArtifactType: m.ArtifactType,
			Digest:       m.Digest,
			Size:         m.Size,
			Annotations:  m.Annotations,
		})
	}
	writeReferrers(w, r, ds, at)
}

// proxiedReferrers answers the referrers of d in a repository that is a
// pull-through cache, from what its upstream says: a list the upstream gave,
// a `404` for an upstream without the API, or a failure when there is
// nothing it said recently enough -- never an empty list nobody observed.
//
// An answer from before the last check carries `Age`, and one served because
// the upstream is failing also `Cr-Stale`, so that a caller verifying a
// signature can refuse what a caller browsing accepts.
func (g *Registry) proxiedReferrers(w http.ResponseWriter, r *http.Request, p *Proxy, name string, d digest.Digest, at string) {
	snap, stale, err := g.referrersThrough(r.Context(), p, name, d)
	if err != nil {
		g.fail(w, r, err)
		return
	}
	if !snap.Supported {
		g.fail(w, r, oci.NewError(http.StatusNotFound, oci.CodeUnsupported, "the upstream has no referrers API; use the tag schema"))
		return
	}

	h := w.Header()
	if age := g.c.Now().Sub(snap.ObservedAt); age >= time.Second {
		h.Set("Age", strconv.FormatInt(int64(age/time.Second), 10))
	}
	if stale {
		h.Set("Cr-Stale", "true")
	}

	ds := make([]v1.Descriptor, 0, len(snap.Descriptors))
	for _, v := range snap.Descriptors {
		if at != "" && v.ArtifactType != at {
			continue
		}
		ds = append(ds, v1.Descriptor{
			MediaType:    v.MediaType,
			ArtifactType: v.ArtifactType,
			Digest:       v.Digest,
			Size:         v.Size,
			Annotations:  v.Annotations,
		})
	}
	writeReferrers(w, r, ds, at)
}

// writeReferrers answers ds as the referrers index, saying the artifactType
// filter was applied when there was one: every caller of it applied it.
func writeReferrers(w http.ResponseWriter, r *http.Request, ds []v1.Descriptor, at string) {
	out := v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: ds,
	}
	if at != "" {
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}
	writeJSON(w, r, v1.MediaTypeImageIndex, out)
}
