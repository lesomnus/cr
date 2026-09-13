package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/lesomnus/otx/log"
)

// Binding grants actions on the repositories Repo matches to a subject or a
// group, when every claim in When holds.
type Binding struct {
	Subject string
	Group   string
	Repo    string
	Actions []Action
	When    map[string]string
}

func (b Binding) matches(s Subject) bool {
	switch {
	case b.Subject != "":
		if b.Subject != Anonymous && b.Subject != s.ID {
			return false
		}
	case b.Group != "":
		if !s.In(b.Group) {
			return false
		}
	default:
		return false
	}
	for k, pattern := range b.When {
		v, ok := s.Claims[k]
		if !ok || !claimMatches(pattern, v) {
			return false
		}
	}
	return true
}

// claimMatches is a claim against a glob; a list claim matches when any of
// its values does.
func claimMatches(pattern string, v any) bool {
	switch v := v.(type) {
	case string:
		return Glob(pattern, v)
	case []string:
		return slices.ContainsFunc(v, func(e string) bool { return Glob(pattern, e) })
	case []any:
		return slices.ContainsFunc(v, func(e any) bool { return claimMatches(pattern, e) })
	case bool:
		return Glob(pattern, strconv.FormatBool(v))
	case float64:
		return Glob(pattern, strconv.FormatFloat(v, 'f', -1, 64))
	case json.Number:
		return Glob(pattern, v.String())
	case nil:
		return false
	default:
		return Glob(pattern, fmt.Sprint(v))
	}
}

func (b Binding) grants(a Action) bool {
	return slices.Contains(b.Actions, ActionAll) || slices.Contains(b.Actions, a)
}

type TagRuleKind string

const (
	TagImmutable TagRuleKind = "immutable"
	TagProtected TagRuleKind = "protected"
	TagPattern   TagRuleKind = "pattern"
	TagRetention TagRuleKind = "retention"
)

// TagRule constrains the tags Tag matches in the repositories Repo matches.
type TagRule struct {
	Name    string
	Repo    string
	Tag     string
	Kind    TagRuleKind
	Pattern string
	Groups  []string
	Keep    int

	re *regexp.Regexp
}

type TagOp int

const (
	TagCreate TagOp = iota
	TagMove
	TagDelete
)

func (op TagOp) String() string {
	switch op {
	case TagCreate:
		return "create"
	case TagMove:
		return "move"
	case TagDelete:
		return "delete"
	}
	return "unknown"
}

// TagRuleError is a tag operation a rule refuses.
type TagRuleError struct {
	Rule string
	Kind TagRuleKind
	Tag  string
	Op   TagOp
}

func (e *TagRuleError) Error() string {
	name := ""
	if e.Rule != "" {
		name = " " + strconv.Quote(e.Rule)
	}
	switch e.Kind {
	case TagImmutable:
		return fmt.Sprintf("tag %q is immutable (rule%s): it may not %s", e.Tag, name, e.Op)
	case TagProtected:
		return fmt.Sprintf("tag %q is protected (rule%s): it may %s only with admin", e.Tag, name, e.Op)
	case TagPattern:
		return fmt.Sprintf("tag %q does not match the pattern the rule%s requires", e.Tag, name)
	}
	return fmt.Sprintf("tag %q is refused by rule%s", e.Tag, name)
}

// Policy is the bindings and tag rules in force at one moment.
type Policy struct {
	Bindings []Binding
	TagRules []TagRule
}

// NewPolicy is a policy over bs and rs. A pattern that does not compile is
// reported, and its rule refuses every tag it would have checked: a rule that
// cannot be evaluated fails closed.
func NewPolicy(bs []Binding, rs []TagRule) (*Policy, error) {
	var errs []error
	rules := make([]TagRule, len(rs))
	for i, r := range rs {
		if r.Kind == TagPattern {
			re, err := regexp.Compile("^(?:" + r.Pattern + ")$")
			if err != nil {
				errs = append(errs, fmt.Errorf("tag rule %q: pattern: %w", r.Name, err))
			} else {
				r.re = re
			}
		}
		rules[i] = r
	}
	p := &Policy{Bindings: slices.Clone(bs), TagRules: rules}
	if len(errs) > 0 {
		return p, fmt.Errorf("%v", errs)
	}
	return p, nil
}

// Allow answers which of want s may do in repo.
func (p *Policy) Allow(s Subject, repo string, want []Action) []Action {
	if p == nil {
		return nil
	}
	out := []Action{}
	for _, a := range want {
		for _, b := range p.Bindings {
			if b.grants(a) && b.matches(s) && Glob(b.Repo, repo) {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// AllowRegistry answers which of want s may do to the registry as a whole:
// its lists and its operator endpoints. Only a binding over every repository,
// `*`, grants those.
func (p *Policy) AllowRegistry(s Subject, want []Action) []Action {
	if p == nil {
		return nil
	}
	out := []Action{}
	for _, a := range want {
		for _, b := range p.Bindings {
			if b.Repo == "*" && b.grants(a) && b.matches(s) {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// CheckTag answers whether op on tag in repo is allowed by the tag rules, for
// s holding granted in repo.
func (p *Policy) CheckTag(s Subject, granted []Action, repo, tag string, op TagOp) error {
	if p == nil {
		return nil
	}
	for _, r := range p.TagRules {
		if !Glob(r.Repo, repo) || !Glob(r.Tag, tag) {
			continue
		}
		refuse := &TagRuleError{Rule: r.Name, Kind: r.Kind, Tag: tag, Op: op}
		switch r.Kind {
		case TagImmutable:
			if op != TagCreate {
				return refuse
			}
		case TagProtected:
			if op == TagCreate {
				continue
			}
			if slices.Contains(granted, ActionAdmin) || slices.Contains(granted, ActionAll) {
				continue
			}
			if slices.ContainsFunc(r.Groups, s.In) {
				continue
			}
			return refuse
		case TagPattern:
			if op == TagDelete {
				continue
			}
			if r.re == nil || !r.re.MatchString(tag) {
				return refuse
			}
		}
	}
	return nil
}

// Retention is the retention rules that apply to repo.
func (p *Policy) Retention(repo string) []TagRule {
	if p == nil {
		return nil
	}
	var out []TagRule
	for _, r := range p.TagRules {
		if r.Kind == TagRetention && r.Keep >= 0 && Glob(r.Repo, repo) {
			out = append(out, r)
		}
	}
	return out
}

// Source is where bindings and tag rules come from: the configuration, the
// database.
type Source interface {
	Load(ctx context.Context) ([]Binding, []TagRule, error)
}

// Static is a source that never changes.
type Static struct {
	Bindings []Binding
	TagRules []TagRule
}

func (s Static) Load(context.Context) ([]Binding, []TagRule, error) {
	return s.Bindings, s.TagRules, nil
}

// PolicyStore holds the policy the registry enforces and reloads it from its
// sources. A request reads the snapshot and nothing else, so no request waits
// on a database for a decision.
type PolicyStore struct {
	sources []Source
	every   time.Duration
	cur     atomic.Pointer[Policy]
}

// NewPolicyStore is a store over sources, reloaded every `every`. Until the
// first load it holds the empty policy, which allows nothing.
func NewPolicyStore(every time.Duration, sources ...Source) *PolicyStore {
	if every <= 0 {
		every = 5 * time.Second
	}
	st := &PolicyStore{sources: sources, every: every}
	st.cur.Store(&Policy{})
	return st
}

// Current is the policy in force.
func (st *PolicyStore) Current() *Policy { return st.cur.Load() }

// Refresh loads every source. When one fails, the policy in force stays in
// force: an outage of the database is not a reason to change who may push.
func (st *PolicyStore) Refresh(ctx context.Context) error {
	var bs []Binding
	var rs []TagRule
	for _, src := range st.sources {
		b, r, err := src.Load(ctx)
		if err != nil {
			return err
		}
		bs = append(bs, b...)
		rs = append(rs, r...)
	}
	p, err := NewPolicy(bs, rs)
	st.cur.Store(p)
	return err
}

func (st *PolicyStore) Spin(ctx context.Context) error {
	t := time.NewTicker(st.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := st.Refresh(ctx); err != nil && ctx.Err() == nil {
				log.From(ctx).WarnContext(ctx, "policy", slog.String("err", err.Error()))
			}
		}
	}
}
