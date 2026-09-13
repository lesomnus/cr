package registry

import (
	"net/http"
	"slices"
	"strings"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/oci"
)

// need is what a request to rt with method must be allowed in its repository,
// and false for a method the route does not have.
func need(rt route, method, arg string) ([]auth.Action, bool) {
	switch rt {
	case routeBlob:
		switch method {
		case http.MethodGet, http.MethodHead:
			return []auth.Action{auth.ActionPull}, true
		case http.MethodDelete:
			return []auth.Action{auth.ActionDelete}, true
		}
	case routeUpload:
		switch method {
		case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodGet, http.MethodDelete:
			return []auth.Action{auth.ActionPush}, true
		}
	case routeManifest:
		switch method {
		case http.MethodGet, http.MethodHead:
			return []auth.Action{auth.ActionPull}, true
		case http.MethodPut:
			if strings.Contains(arg, ":") {
				return []auth.Action{auth.ActionPush}, true
			}
			return []auth.Action{auth.ActionPush, auth.ActionTag}, true
		case http.MethodDelete:
			return []auth.Action{auth.ActionDelete}, true
		}
	case routeTags, routeReferrers:
		switch method {
		case http.MethodGet, http.MethodHead:
			return []auth.Action{auth.ActionPull}, true
		}
	}
	return nil, false
}

func challengeScopes(repo string, want []auth.Action) []auth.Scope {
	if repo == "" {
		return []auth.Scope{{Type: auth.TypeRegistry, Name: "catalog", Actions: []string{"*"}}}
	}
	actions := auth.Strings(want)
	// Distribution's clients ask for pull with push, and a token without
	// pull cannot check what it is about to overwrite.
	if slices.Contains(want, auth.ActionPush) && !slices.Contains(want, auth.ActionPull) {
		actions = append([]string{string(auth.ActionPull)}, actions...)
	}
	return []auth.Scope{{Type: auth.TypeRepository, Name: repo, Actions: actions}}
}

// guard checks r may do want in repo, or to the registry when repo is empty,
// and answers r with the caller on its context. It answers nil having written
// the refusal: 401 and a challenge to a caller that may do better with another
// credential or another token, 403 to one that asked and was told no.
func (g *Registry) guard(w http.ResponseWriter, r *http.Request, repo string, want ...auth.Action) *http.Request {
	gd := g.c.Guard
	if gd == nil {
		return r
	}
	c, err := gd.Caller(r)
	if err != nil {
		g.fail(w, r, gd.Challenge(r, challengeScopes(repo, want), false))
		return nil
	}
	r = r.WithContext(auth.WithCaller(r.Context(), c))

	var allowed []auth.Action
	if repo == "" {
		allowed = c.AllowedRegistry(want...)
	} else {
		allowed = c.Allowed(repo, want...)
	}
	if len(allowed) == len(want) {
		return r
	}

	missing := slices.DeleteFunc(slices.Clone(want), func(a auth.Action) bool { return slices.Contains(allowed, a) })
	switch {
	case c.FromToken() && repo != "" && !c.Refused(repo, missing):
		g.fail(w, r, gd.Challenge(r, challengeScopes(repo, want), true))
	case c.Subject.IsAnonymous():
		g.fail(w, r, gd.Challenge(r, challengeScopes(repo, want), false))
	default:
		g.fail(w, r, oci.ErrDenied("missing "+strings.Join(auth.Strings(missing), ",")))
	}
	return nil
}
