// Package auth is who is calling the registry and what they may do there:
// subjects from providers, actions from permissions granted by matches, the
// rules on tags, and the tokens the distribution flow carries between them.
//
// Enforcement is a layer between the handler and the index. The rows it reads
// are the management plane's; nothing here writes them.
package auth

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// Action is something a caller may do in a repository, or to the registry.
type Action string

const (
	ActionPull   Action = "pull"
	ActionPush   Action = "push"
	ActionDelete Action = "delete"

	// ActionTag is creating or moving a tag. A push without it is a push by
	// digest only.
	ActionTag Action = "tag"

	// ActionCatalog and ActionSearch are the registry's lists. What either
	// answers is still narrowed to what the caller may pull.
	ActionCatalog Action = "catalog"
	ActionSearch  Action = "search"

	// ActionAdmin is moving a protected tag, and the operator's endpoints.
	ActionAdmin Action = "admin"

	// ActionAll is every action, in a permission.
	ActionAll Action = "*"
)

// Actions is every action there is, in the order they are printed.
var Actions = []Action{ActionPull, ActionPush, ActionDelete, ActionTag, ActionCatalog, ActionSearch, ActionAdmin}

// ParseActions reads a list of action names, dropping ones it does not know.
func ParseActions(vs []string) []Action {
	out := []Action{}
	for _, v := range vs {
		for part := range strings.SplitSeq(v, ",") {
			a := Action(strings.TrimSpace(part))
			if a == ActionAll || slices.Contains(Actions, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

// Strings is as for a token's `access` claim.
func Strings(as []Action) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = string(a)
	}
	return out
}

// Anonymous is the subject of a caller that gave no credential.
const Anonymous = "anonymous"

// Anyone is the match every caller is under, with a credential or without,
// so logging in never takes away what a public repository allows.
const Anyone = "anyone"

// Subject is a caller as a provider names them.
type Subject struct {
	// ID is `provider:subject`, what the logs name the caller by.
	ID string

	// Provider is the name of the provider that vouched for the caller, what
	// a match's `for` names; empty for the anonymous caller.
	Provider string

	// Claims are what the credential said, for a match's `when`.
	Claims map[string]any

	// Via is the kind of authenticator that accepted the credential --
	// `oidc`, `exchange` -- for the count of logins. Empty for the anonymous
	// caller, and for a token.
	Via string
}

func (s Subject) IsAnonymous() bool { return s.Provider == "" }

var (
	// ErrUnauthenticated is a credential that was checked and is wrong.
	ErrUnauthenticated = errors.New("unauthenticated")

	// ErrNotMine is an authenticator that does not know the name at all, so
	// the next one in a chain is asked.
	ErrNotMine = errors.New("not known to this authenticator")
)

// Authenticator checks a username and password, which is what `docker login`
// has to give.
type Authenticator interface {
	Authenticate(ctx context.Context, username, password string) (Subject, error)
}

// Chain asks each authenticator in turn. The first that knows the name
// decides; a chain nobody in knows it is [ErrUnauthenticated].
type Chain []Authenticator

func (c Chain) Authenticate(ctx context.Context, username, password string) (Subject, error) {
	for _, a := range c {
		s, err := a.Authenticate(ctx, username, password)
		if err == nil {
			if k, ok := a.(interface{ Kind() string }); ok && s.Via == "" {
				s.Via = k.Kind()
			}
			return s, nil
		}
		if errors.Is(err, ErrNotMine) {
			continue
		}
		return Subject{}, err
	}
	return Subject{}, ErrUnauthenticated
}
