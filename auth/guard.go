package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lesomnus/cr/oci"
)

// Guard is the registry's front door: who a request is, and what they may do.
type Guard struct {
	Authenticator Authenticator
	Policy        *PolicyStore
	Issuer        *Issuer

	// Realm is where a client is sent for a token. Empty is `/token` on
	// whatever host and scheme the request came in on.
	Realm string
}

// Caller is a request's subject and, for a bearer token, what it grants.
type Caller struct {
	Subject Subject

	claims *Claims
	policy *Policy
}

// Open is the caller of a registry with no guard: everything is allowed.
var Open = &Caller{Subject: Subject{ID: Anonymous}}

type callerKey struct{}

func WithCaller(ctx context.Context, c *Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom is the caller a guard put on ctx, or [Open].
func CallerFrom(ctx context.Context) *Caller {
	if c, ok := ctx.Value(callerKey{}).(*Caller); ok {
		return c
	}
	return Open
}

func (c *Caller) open() bool { return c == Open }

// FromToken reports whether the caller came with a bearer token.
func (c *Caller) FromToken() bool { return c.claims != nil }

func intersect(want []Action, granted []string) []Action {
	out := []Action{}
	for _, a := range want {
		if slices.Contains(granted, string(a)) || slices.Contains(granted, string(ActionAll)) {
			out = append(out, a)
		}
	}
	return out
}

// Allowed answers which of want the caller may do in repo: what its token
// grants, or what the policy does.
func (c *Caller) Allowed(repo string, want ...Action) []Action {
	if c.open() {
		return want
	}
	if c.claims != nil {
		for _, a := range c.claims.Access {
			if a.Type == TypeRepository && a.Name == repo {
				return intersect(want, a.Actions)
			}
		}
		return []Action{}
	}
	return c.policy.Allow(c.Subject, repo, want)
}

// Refused reports whether the caller was told no to every one of actions in
// repo: always, except for a token that was never asked for one of them, whose
// holder should be sent for a token that asks. Clients ask for what they need
// as they go -- pull for the checks, then pull and push to write -- so a token
// missing an action is usually one that was not asked for it yet.
func (c *Caller) Refused(repo string, actions []Action) bool {
	if c.claims == nil {
		return true
	}
	var refused []string
	for _, a := range c.claims.Refused {
		if a.Type == TypeRepository && a.Name == repo {
			refused = a.Actions
		}
	}
	for _, a := range actions {
		if !slices.Contains(refused, string(a)) {
			return false
		}
	}
	return true
}

// AllowedRegistry answers which of want the caller may do to the registry.
func (c *Caller) AllowedRegistry(want ...Action) []Action {
	if c.open() {
		return want
	}
	if c.claims != nil {
		for _, a := range c.claims.Access {
			if a.Type == TypeRegistry && a.Name == "catalog" {
				return intersect(want, a.Actions)
			}
		}
		return []Action{}
	}
	return c.policy.AllowRegistry(c.Subject, want)
}

// CanPull answers whether the caller may pull repo by the policy, whatever
// its token was issued for: what a list shows is not what a token was asked
// about.
func (c *Caller) CanPull(repo string) bool {
	if c.open() {
		return true
	}
	return len(c.policy.Allow(c.Subject, repo, []Action{ActionPull})) == 1
}

// CheckTag is the tag rules for this caller.
func (c *Caller) CheckTag(repo, tag string, op TagOp) error {
	if c.open() {
		return nil
	}
	return c.policy.CheckTag(c.Subject, c.Allowed(repo, ActionAdmin), repo, tag, op)
}

// Caller reads a request's credential. None is the anonymous caller; one that
// does not check is [ErrUnauthenticated].
func (g *Guard) Caller(r *http.Request) (*Caller, error) {
	p := g.Policy.Current()
	h := r.Header.Get("Authorization")
	if h == "" {
		return &Caller{Subject: Subject{ID: Anonymous}, policy: p}, nil
	}
	scheme, rest, _ := strings.Cut(h, " ")
	switch strings.ToLower(scheme) {
	case "bearer":
		c, err := g.Issuer.Verify(strings.TrimSpace(rest))
		if err != nil {
			return nil, ErrUnauthenticated
		}
		return &Caller{Subject: Subject{ID: c.Subject, Groups: c.Groups}, claims: c, policy: p}, nil
	case "basic":
		user, pass, ok := r.BasicAuth()
		if !ok {
			return nil, ErrUnauthenticated
		}
		s, err := g.Authenticator.Authenticate(r.Context(), user, pass)
		if err != nil {
			if errors.Is(err, ErrNotMine) {
				err = ErrUnauthenticated
			}
			return nil, err
		}
		return &Caller{Subject: s, policy: p}, nil
	}
	return nil, ErrUnauthenticated
}

func (g *Guard) realm(r *http.Request) string {
	if g.Realm != "" {
		return g.Realm
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	return scheme + "://" + host + "/token"
}

// Challenge is the 401 that sends a client for a token for scopes.
func (g *Guard) Challenge(r *http.Request, scopes []Scope, insufficient bool) *oci.Error {
	v := `Bearer realm="` + g.realm(r) + `",service="` + g.Issuer.Service() + `"`
	if len(scopes) > 0 {
		ss := make([]string, len(scopes))
		for i, s := range scopes {
			ss[i] = s.String()
		}
		v += `,scope="` + strings.Join(ss, " ") + `"`
	}
	if insufficient {
		v += `,error="insufficient_scope"`
	}
	return oci.ErrUnauthorized(nil).WithHeader("WWW-Authenticate", v)
}

type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// repositoryActions is what a token may carry for a repository.
var repositoryActions = []Action{ActionPull, ActionPush, ActionDelete, ActionTag, ActionAdmin}

// ServeToken is the distribution token endpoint: `GET` with Basic, or `POST`
// with a password grant, for the scopes asked. Every scope asked is in the
// answer with what the policy grants of it, possibly nothing, and what it did
// not grant is in `refused`, so a client refused an action is refused rather
// than sent back for another token.
func (g *Guard) ServeToken(w http.ResponseWriter, r *http.Request) {
	var (
		user, pass string
		given      bool
		scopes     []string
	)
	switch r.Method {
	case http.MethodGet:
		user, pass, given = r.BasicAuth()
		scopes = r.URL.Query()["scope"]
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			oci.WriteError(w, oci.ErrUnsupported(err.Error()))
			return
		}
		switch r.PostForm.Get("grant_type") {
		case "password":
			user, pass, given = r.PostForm.Get("username"), r.PostForm.Get("password"), true
		default:
			oci.WriteError(w, oci.NewError(http.StatusBadRequest, oci.CodeUnsupported, "unsupported grant_type"))
			return
		}
		scopes = r.PostForm["scope"]
	default:
		oci.WriteError(w, oci.ErrUnsupported("method not allowed"))
		return
	}

	s := Subject{ID: Anonymous}
	if given {
		var err error
		s, err = g.Authenticator.Authenticate(r.Context(), user, pass)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="cr"`)
			oci.WriteError(w, oci.ErrUnauthorized("the credentials do not check"))
			return
		}
	}

	ss, err := ParseScopes(scopes)
	if err != nil {
		oci.WriteError(w, oci.NewError(http.StatusBadRequest, oci.CodeUnsupported, err.Error()))
		return
	}

	p := g.Policy.Current()
	access := []Access{}
	refused := []Access{}
	refuse := func(t, name string, want, granted []Action) {
		no := slices.DeleteFunc(slices.Clone(want), func(a Action) bool { return slices.Contains(granted, a) })
		if len(no) > 0 {
			refused = append(refused, Access{Type: t, Name: name, Actions: Strings(no)})
		}
	}
	for _, sc := range ss {
		want := ParseActions(sc.Actions)
		switch sc.Type {
		case TypeRepository:
			if !oci.ValidName(sc.Name) {
				continue
			}
			if slices.Contains(want, ActionAll) {
				want = repositoryActions
			}
			// Distribution's clients ask for push and know nothing of tags.
			if slices.Contains(want, ActionPush) && !slices.Contains(want, ActionTag) {
				want = append(want, ActionTag)
			}
			granted := p.Allow(s, sc.Name, want)
			access = append(access, Access{Type: TypeRepository, Name: sc.Name, Actions: Strings(granted)})
			refuse(TypeRepository, sc.Name, want, granted)
		case TypeRegistry:
			if sc.Name != "catalog" {
				continue
			}
			if slices.Contains(want, ActionAll) {
				want = []Action{ActionCatalog, ActionSearch}
			}
			granted := p.AllowRegistry(s, want)
			access = append(access, Access{Type: TypeRegistry, Name: sc.Name, Actions: Strings(granted)})
			refuse(TypeRegistry, sc.Name, want, granted)
		}
	}

	token, c, err := g.Issuer.Issue(s, access, refused...)
	if err != nil {
		oci.WriteError(w, err)
		return
	}
	b, _ := json.Marshal(tokenResponse{
		Token:       token,
		AccessToken: token,
		ExpiresIn:   int(g.Issuer.TTL() / time.Second),
		IssuedAt:    c.IssuedAt.Time().UTC().Format(time.RFC3339),
	})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	h.Set("Cache-Control", "no-store")
	w.Write(b)
}

// ServeJWKS publishes the keys tokens are verified with.
func (g *Guard) ServeJWKS(w http.ResponseWriter, r *http.Request) {
	b, _ := json.Marshal(g.Issuer.JWKS())
	h := w.Header()
	h.Set("Content-Type", "application/jwk-set+json")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.Write(b)
}
