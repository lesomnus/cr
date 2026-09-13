package auth

import (
	"fmt"
	"strings"
)

// Resource types a scope names.
const (
	TypeRepository = "repository"
	TypeRegistry   = "registry"
)

// Scope is one `type:name:actions` of the distribution token flow.
type Scope struct {
	Type    string
	Name    string
	Actions []string
}

// ParseScope reads one scope. The name may itself contain colons, a registry
// host with a port, so the type is up to the first colon and the actions are
// after the last one.
func ParseScope(v string) (Scope, error) {
	i := strings.IndexByte(v, ':')
	j := strings.LastIndexByte(v, ':')
	if i <= 0 || j <= i {
		return Scope{}, fmt.Errorf("scope %q: want type:name:actions", v)
	}
	t := v[:i]
	// `repository(plugin)` is a repository to everything here.
	if k := strings.IndexByte(t, '('); k > 0 {
		t = t[:k]
	}
	s := Scope{Type: t, Name: v[i+1 : j]}
	for a := range strings.SplitSeq(v[j+1:], ",") {
		if a = strings.TrimSpace(a); a != "" {
			s.Actions = append(s.Actions, a)
		}
	}
	return s, nil
}

// ParseScopes reads every scope in values, each of which may hold several
// separated by spaces.
func ParseScopes(values []string) ([]Scope, error) {
	var out []Scope
	for _, v := range values {
		for f := range strings.FieldsSeq(v) {
			s, err := ParseScope(f)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
	}
	return out, nil
}

func (s Scope) String() string {
	return s.Type + ":" + s.Name + ":" + strings.Join(s.Actions, ",")
}
