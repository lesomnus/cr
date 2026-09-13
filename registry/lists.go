package registry

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"

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
		vs, err := g.c.Index.Repo().List(ctx, p)
		if err != nil {
			g.fail(w, r, err)
			return
		}
		repos = append(repos, vs...)
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
	ms, err := g.c.Index.Manifest().Referrers(ctx, name, d, at)
	if err != nil {
		g.fail(w, r, err)
		return
	}

	out := v1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex,
		Manifests: make([]v1.Descriptor, 0, len(ms)),
	}
	for _, m := range ms {
		out.Manifests = append(out.Manifests, v1.Descriptor{
			MediaType:    m.MediaType,
			ArtifactType: m.ArtifactType,
			Digest:       m.Digest,
			Size:         m.Size,
			Annotations:  m.Annotations,
		})
	}
	if at != "" {
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}
	writeJSON(w, r, v1.MediaTypeImageIndex, out)
}
