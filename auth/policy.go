package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lesomnus/otx/log"
	"go.opentelemetry.io/otel/metric"
)

// Permission is actions on the repositories its patterns select.
type Permission struct {
	// Repos are globs read in order, and the last that matches a repository
	// decides: one with `!` in front takes back what it matches, and one after
	// that can give it again, as in a `.gitignore`.
	Repos   []string
	Actions []Action
}

// Match grants permissions to the callers a provider vouches for, when every
// claim in When holds. Matches only add: what a caller may do is the union of
// what every match it is under grants, and a caller under none may do nothing.
type Match struct {
	// For is the provider, or [Anyone] for every caller.
	For string

	// When is claims of the credential that must all hold, each value a
	// glob; a claim that is a list holds when any of its values does. A
	// match for a provider must have one, since without it every credential
	// the provider issues to anybody is under it, and one for anyone cannot,
	// since the anonymous caller has no claims.
	When map[string]string

	// Grant is the permissions granted, by name.
	Grant []string
}

// Rules are the permissions, the matches and the tag rules, as a source
// gives them.
type Rules struct {
	Permissions map[string]Permission
	Matches     map[string]Match
	TagRules    []TagRule
}

type repoPattern struct {
	glob Glob
	out  bool
}

type permission struct {
	repos   []repoPattern
	actions []Action

	// everywhere is a permission whose only pattern is `**`: the one kind
	// that reaches the registry as a whole.
	everywhere bool
}

// selects reports whether the permission is over repo.
func (pm *permission) selects(repo string) bool {
	in := false
	for _, p := range pm.repos {
		if p.glob.Match(repo) {
			in = !p.out
		}
	}
	return in
}

type match struct {
	for_  string
	when  map[string]Glob
	grant []*permission
}

func (m *match) holds(s Subject) bool {
	if m.for_ != Anyone && m.for_ != s.Provider {
		return false
	}
	for k, g := range m.when {
		v, ok := s.Claims[k]
		if !ok || !claimMatches(g, v) {
			return false
		}
	}
	return true
}

// claimMatches is a claim against a glob; a list claim matches when any of
// its values does.
func claimMatches(g Glob, v any) bool {
	switch v := v.(type) {
	case string:
		return g.Match(v)
	case []string:
		return slices.ContainsFunc(v, g.Match)
	case []any:
		return slices.ContainsFunc(v, func(e any) bool { return claimMatches(g, e) })
	case bool:
		return g.Match(strconv.FormatBool(v))
	case float64:
		return g.Match(strconv.FormatFloat(v, 'f', -1, 64))
	case json.Number:
		return g.Match(v.String())
	case nil:
		return false
	default:
		return g.Match(fmt.Sprint(v))
	}
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
	Keep    int

	repo, tag Glob
	ok        bool // repo and tag are globs that parsed
	re        *regexp.Regexp
}

// covers reports whether the rule is over tag in repo. A rule whose globs do
// not parse covers every tag, so that one that refuses fails closed, except a
// retention rule, which covers none rather than delete what it cannot tell.
func (r TagRule) covers(repo, tag string) bool {
	if !r.ok {
		return r.Kind != TagRetention
	}
	return r.repo.Match(repo) && r.tag.Match(tag)
}

// MatchesTag reports whether the rule's tag glob matches tag.
func (r TagRule) MatchesTag(tag string) bool {
	return r.ok && r.tag.Match(tag)
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

// Policy is the permissions, matches and tag rules in force at one moment.
type Policy struct {
	matches  []*match
	TagRules []TagRule
}

// NewPolicy is a policy over r. What does not check is reported, and left out
// so that it grants nothing: a permission or a match with a pattern that does
// not parse, a match that names a permission there is not, one for a provider
// with no `when` or for anyone with one. A tag rule whose pattern does not
// compile refuses every tag it would have checked: a rule that cannot be
// evaluated fails closed.
func NewPolicy(r Rules) (*Policy, error) {
	var errs []error
	perms := map[string]*permission{}
	for _, name := range slices.Sorted(maps.Keys(r.Permissions)) {
		pm, err := compilePermission(r.Permissions[name])
		if err != nil {
			errs = append(errs, fmt.Errorf("permission %q: %w", name, err))
			continue
		}
		perms[name] = pm
	}

	p := &Policy{}
	for _, name := range slices.Sorted(maps.Keys(r.Matches)) {
		m, err := compileMatch(r.Matches[name], perms)
		if err != nil {
			errs = append(errs, fmt.Errorf("match %q: %w", name, err))
			continue
		}
		p.matches = append(p.matches, m)
	}

	p.TagRules = make([]TagRule, len(r.TagRules))
	for i, t := range r.TagRules {
		repo, err1 := ParseGlob(t.Repo)
		tag, err2 := ParseGlob(t.Tag)
		if err := errors.Join(err1, err2); err != nil {
			errs = append(errs, fmt.Errorf("tag rule %q: %w", t.Name, err))
		} else {
			t.repo, t.tag, t.ok = repo, tag, true
		}
		if t.Kind == TagPattern {
			re, err := regexp.Compile("^(?:" + t.Pattern + ")$")
			if err != nil {
				errs = append(errs, fmt.Errorf("tag rule %q: pattern: %w", t.Name, err))
			} else {
				t.re = re
			}
		}
		p.TagRules[i] = t
	}
	return p, errors.Join(errs...)
}

func compilePermission(v Permission) (*permission, error) {
	if len(v.Repos) == 0 {
		return nil, errors.New("no repos")
	}
	if len(v.Actions) == 0 {
		return nil, errors.New("no actions")
	}
	for _, a := range v.Actions {
		if a != ActionAll && !slices.Contains(Actions, a) {
			return nil, fmt.Errorf("no action %q", a)
		}
	}
	pm := &permission{actions: slices.Clone(v.Actions)}
	for _, src := range v.Repos {
		out := false
		if rest, ok := strings.CutPrefix(src, "!"); ok {
			src, out = rest, true
		}
		g, err := ParseGlob(src)
		if err != nil {
			return nil, err
		}
		pm.repos = append(pm.repos, repoPattern{glob: g, out: out})
	}
	if !slices.ContainsFunc(pm.repos, func(p repoPattern) bool { return !p.out }) {
		return nil, errors.New("every pattern in repos takes away, so none gives")
	}
	pm.everywhere = len(v.Repos) == 1 && v.Repos[0] == "**"
	return pm, nil
}

func compileMatch(v Match, perms map[string]*permission) (*match, error) {
	switch {
	case v.For == "":
		return nil, errors.New("no `for`")
	case v.For == Anyone && len(v.When) > 0:
		return nil, errors.New("`when` for anyone: the anonymous caller has no claims")
	case v.For != Anyone && len(v.When) == 0:
		return nil, fmt.Errorf("no `when`: every credential %q issues, to anybody, would be under it", v.For)
	case len(v.Grant) == 0:
		return nil, errors.New("grants nothing")
	}
	m := &match{for_: v.For, when: map[string]Glob{}}
	for k, src := range v.When {
		g, err := ParseGlob(src)
		if err != nil {
			return nil, fmt.Errorf("when %q: %w", k, err)
		}
		m.when[k] = g
	}
	for _, g := range v.Grant {
		pm, ok := perms[g]
		if !ok {
			return nil, fmt.Errorf("no permission %q", g)
		}
		m.grant = append(m.grant, pm)
	}
	return m, nil
}

// granted is every action the permissions s is granted, of those over, give.
func (p *Policy) granted(s Subject, over func(*permission) bool) []Action {
	var out []Action
	for _, m := range p.matches {
		if !m.holds(s) {
			continue
		}
		for _, pm := range m.grant {
			if over(pm) {
				out = append(out, pm.actions...)
			}
		}
	}
	return out
}

// Allow answers which of want s may do in repo.
func (p *Policy) Allow(s Subject, repo string, want []Action) []Action {
	if p == nil {
		return nil
	}
	return intersect(want, Strings(p.granted(s, func(pm *permission) bool { return pm.selects(repo) })))
}

// AllowRegistry answers which of want s may do to the registry as a whole:
// its lists and its operator endpoints. Only a permission over every
// repository, `**` and nothing else, grants those.
func (p *Policy) AllowRegistry(s Subject, want []Action) []Action {
	if p == nil {
		return nil
	}
	return intersect(want, Strings(p.granted(s, func(pm *permission) bool { return pm.everywhere })))
}

// CheckTag answers whether op on tag in repo is allowed by the tag rules, for
// a caller holding granted in repo.
func (p *Policy) CheckTag(granted []Action, repo, tag string, op TagOp) error {
	if p == nil {
		return nil
	}
	for _, r := range p.TagRules {
		if !r.covers(repo, tag) {
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
		if r.Kind == TagRetention && r.Keep >= 0 && r.ok && r.repo.Match(repo) {
			out = append(out, r)
		}
	}
	return out
}

// Source is where rules come from: the configuration, the database.
type Source interface {
	Load(ctx context.Context) (Rules, error)
}

// Static is a source that never changes.
type Static Rules

func (s Static) Load(context.Context) (Rules, error) {
	return Rules(s), nil
}

// PolicyStore holds the policy the registry enforces and reloads it from its
// sources. A request reads the snapshot and nothing else, so no request waits
// on a database for a decision.
type PolicyStore struct {
	sources []Source
	every   time.Duration
	cur     atomic.Pointer[Policy]

	// When the last load that succeeded was, in Unix nanoseconds, and the
	// loads that failed; see Measure.
	last          atomic.Int64
	refreshErrors metric.Int64Counter
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
	if err := st.refresh(ctx); err != nil {
		if st.refreshErrors != nil {
			st.refreshErrors.Add(ctx, 1)
		}
		return err
	}
	st.last.Store(time.Now().UnixNano())
	return nil
}

func (st *PolicyStore) refresh(ctx context.Context) error {
	all := Rules{Permissions: map[string]Permission{}, Matches: map[string]Match{}}
	var errs []error
	for _, src := range st.sources {
		r, err := src.Load(ctx)
		if err != nil {
			return err
		}
		for k, v := range r.Permissions {
			if _, ok := all.Permissions[k]; ok {
				errs = append(errs, fmt.Errorf("permission %q is written twice", k))
			}
			all.Permissions[k] = v
		}
		for k, v := range r.Matches {
			if _, ok := all.Matches[k]; ok {
				errs = append(errs, fmt.Errorf("match %q is written twice", k))
			}
			all.Matches[k] = v
		}
		all.TagRules = append(all.TagRules, r.TagRules...)
	}
	p, err := NewPolicy(all)
	st.cur.Store(p)
	return errors.Join(append(errs, err)...)
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
