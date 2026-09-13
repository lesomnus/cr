// Package auth is who is calling the registry and what they may do there:
// subjects from authenticators, actions from bindings, the rules on tags, and
// the tokens the distribution flow carries between them.
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

	// ActionAll is every action, in a binding.
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

// Anonymous is the subject of a caller that gave no credential. Every caller
// is also anonymous, so a binding to it is a binding to everyone.
const Anonymous = "anonymous"

// Authenticated is the group every caller with a credential that checked is
// in.
const Authenticated = "authenticated"

// Subject is a caller as an authenticator names them.
type Subject struct {
	ID string

	// Aliases are other names for the same subject that a binding may use:
	// roster's `@tenant/alias` beside a holder's identifier.
	Aliases []string

	Groups []string

	// Claims are what the credential said beyond the subject, for a binding's
	// `when`. Empty for credentials that say nothing more.
	Claims map[string]any
}

func (s Subject) IsAnonymous() bool { return s.ID == "" || s.ID == Anonymous }

// Is reports whether name names the subject.
func (s Subject) Is(name string) bool {
	return name == s.ID || slices.Contains(s.Aliases, name)
}

// In reports whether the subject is in group g.
func (s Subject) In(g string) bool {
	if g == Authenticated {
		return !s.IsAnonymous()
	}
	return slices.Contains(s.Groups, g)
}

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
			return s, nil
		}
		if errors.Is(err, ErrNotMine) {
			continue
		}
		return Subject{}, err
	}
	return Subject{}, ErrUnauthenticated
}
