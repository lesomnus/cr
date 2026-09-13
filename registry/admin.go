package registry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/gc"
	"github.com/lesomnus/cr/oci"
)

// Collector is what the operator's garbage collection endpoints drive.
type Collector interface {
	Trigger(ctx context.Context, trigger string) (gc.Run, error)
	Runs() gc.Runs
}

func respond(w http.ResponseWriter, r *http.Request, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		oci.WriteError(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write(b)
	}
}

var errNoRun = oci.NewError(http.StatusNotFound, oci.CodeUnsupported, "no such run")

// Admin answers the operator's endpoints, each of which needs `admin` from a
// binding over `*`:
//
//	POST /admin/gc        start a full collection; 202 and the run, or 409 and the one running
//	GET  /admin/gc        the recent runs, newest first
//	GET  /admin/gc/<id>   one run
func (g *Registry) Admin() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		col := g.c.Collector
		if col == nil {
			g.fail(w, r, errNoRoute)
			return
		}
		if r = g.guard(w, r, "", auth.ActionAdmin); r == nil {
			return
		}
		ctx := r.Context()

		switch p := r.URL.Path; {
		case p == "/admin/gc":
			switch r.Method {
			case http.MethodPost:
				run, err := col.Trigger(ctx, gc.TriggerAdmin)
				switch {
				case errors.Is(err, gc.ErrRunning):
					w.Header().Set("Location", "/admin/gc/"+run.ID)
					respond(w, r, http.StatusConflict, run)
				case err != nil:
					g.fail(w, r, err)
				default:
					w.Header().Set("Location", "/admin/gc/"+run.ID)
					respond(w, r, http.StatusAccepted, run)
				}
			case http.MethodGet, http.MethodHead:
				runs := []gc.Run{}
				if rs := col.Runs(); rs != nil {
					vs, err := rs.List(ctx, 20)
					if err != nil {
						g.fail(w, r, err)
						return
					}
					runs = append(runs, vs...)
				}
				respond(w, r, http.StatusOK, struct {
					Runs []gc.Run `json:"runs"`
				}{runs})
			default:
				g.fail(w, r, errMethod)
			}
		case strings.HasPrefix(p, "/admin/gc/"):
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				g.fail(w, r, errMethod)
				return
			}
			rs := col.Runs()
			if rs == nil {
				g.fail(w, r, errNoRun)
				return
			}
			run, err := rs.Get(ctx, strings.TrimPrefix(p, "/admin/gc/"))
			if errors.Is(err, gc.ErrRunNotFound) {
				g.fail(w, r, errNoRun)
				return
			}
			if err != nil {
				g.fail(w, r, err)
				return
			}
			respond(w, r, http.StatusOK, run)
		default:
			g.fail(w, r, errNoRoute)
		}
	})
}
