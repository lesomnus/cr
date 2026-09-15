package registry

import (
	"net/http"
	"strconv"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/index"
)

// V1 answers the two endpoints of the old API that clients still call:
// `/v1/_ping`, which `docker search` sends before searching, and `/v1/search`,
// which is Docker Hub's search as the CLI and registry-ui speak it.
func (g *Registry) V1() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/_ping":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				g.fail(w, r, errMethod)
				return
			}
			// Standalone: a client authenticates to this registry itself
			// rather than to an index in front of it.
			w.Header().Set("X-Docker-Registry-Standalone", "true")
			writeJSON(w, r, "application/json", struct {
				Standalone bool `json:"standalone"`
			}{true})
		case "/v1/search":
			if r = g.guard(w, r, "", auth.ActionSearch); r == nil {
				return
			}
			g.only(w, r, g.search, http.MethodGet)
		default:
			g.fail(w, r, errNoRoute)
		}
	})
}

type searchResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	StarCount   int    `json:"star_count"`
	IsOfficial  bool   `json:"is_official"`
	IsAutomated bool   `json:"is_automated"`
}

type searchResults struct {
	Query      string         `json:"query"`
	NumResults int            `json:"num_results"`
	NumPages   int            `json:"num_pages"`
	Page       int            `json:"page"`
	PageSize   int            `json:"page_size"`
	Results    []searchResult `json:"results"`
}

// annotationDescription is where an image says what it is when the
// repository says nothing.
const annotationDescription = "org.opencontainers.image.description"

func (g *Registry) search(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	term := q.Get("q")
	n := 25
	if v, err := strconv.Atoi(q.Get("n")); err == nil {
		n = min(max(v, 1), 100)
	}

	c := auth.CallerFrom(ctx)
	out := []searchResult{}
	last := ""
	batch := max(n, 50)
	for len(out) < n {
		vs, err := g.c.Index.Repo().Search(ctx, term, index.Page{Last: last, N: batch})
		if err != nil {
			g.fail(w, r, err)
			return
		}
		for _, v := range vs {
			if !c.CanPull(v.Name) {
				continue
			}
			desc := v.Description
			if desc == "" {
				desc = g.describe(r, v.Name)
			}
			out = append(out, searchResult{Name: v.Name, Description: desc})
			if len(out) == n {
				break
			}
		}
		if len(vs) < batch {
			break
		}
		last = vs[len(vs)-1].Name
	}

	writeJSON(w, r, "application/json", searchResults{
		Query:      term,
		NumResults: len(out),
		NumPages:   1,
		Page:       1,
		PageSize:   n,
		Results:    out,
	})
}

// describe is the description annotation of the manifest the most recently
// moved tag points at, or nothing.
func (g *Registry) describe(r *http.Request, repo string) string {
	ctx := r.Context()
	newest, err := g.c.Index.Tag().Newest(ctx, repo)
	if err != nil {
		return ""
	}
	m, err := g.c.Index.Manifest().Get(ctx, repo, newest.Digest)
	if err != nil {
		return ""
	}
	return m.Annotations[annotationDescription]
}
