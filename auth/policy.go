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

// Provider vouches for callers: an authenticator, under the name a match's
// `for` gives it.
type Provider struct {
	Name          string
	Authenticator Authenticator

	// Exchange is how long a token `POST /token/exchange` trades one of the
	// provider's credentials for lasts; zero trades none.
	Exchange time.Duration
}

// providerName is what a provider may be called: it is written in front of
// every subject it vouches for, `github:...`, so it has no colon.
var providerName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Rules are the providers, the permissions, the matches and the tag rules, as
// a source gives them.
type Rules struct {
	Providers   []Provider
	Permissions map[string]Permission
	Matches     map[string]Match
	TagRules    []TagRule
}

type repoPattern struct {
	src  string
	glob Glob
	out  bool
}

type permission struct {
	name    string
	repos   []repoPattern
	actions []Action

	// everywhere is a permission whose only pattern is `**`: the one kind
	// that reaches the registry as a whole.
	everywhere bool
}

// selects reports whether the permission is over repo.
func (pm *permission) selects(repo string) bool {
	in, _ := pm.decide(repo)
	return in
}

// decide is whether the permission is over repo, and the pattern that said
// so, which is nil when none matched.
func (pm *permission) decide(repo string) (bool, *repoPattern) {
	var by *repoPattern
	for i := range pm.repos {
		if pm.repos[i].glob.Match(repo) {
			by = &pm.repos[i]
		}
	}
	return by != nil && !by.out, by
}

type match struct {
	name  string
	for_  string
	when  map[string]Glob
	grant []*permission
}

func (m *match) holds(s Subject) bool {
	return m.why(s) == ""
}

// why is what keeps s from being under the match, and empty when nothing
// does.
func (m *match) why(s Subject) string {
	if m.for_ != Anyone && m.for_ != s.Provider {
		if s.Provider == "" {
			return fmt.Sprintf("for %s, and the caller gave no credential", m.for_)
		}
		return fmt.Sprintf("for %s, and the caller is from %s", m.for_, s.Provider)
	}
	for _, k := range slices.Sorted(maps.Keys(m.when)) {
		g := m.when[k]
		v, ok := s.Claims[k]
		if !ok {
			return fmt.Sprintf("no claim %s", k)
		}
		if !claimMatches(g, v) {
			return fmt.Sprintf("claim %s is %v, not %s", k, v, g)
		}
	}
	return ""
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

// Policy is the providers, permissions, matches and tag rules in force at one
// moment.
type Policy struct {
	providers []Provider
	chain     Chain
	matches   []*match
	TagRules  []TagRule
}

// NewPolicy is a policy over r. What does not check is reported, and left out
// so that it grants nothing: a permission or a match with a pattern that does
// not parse, a match that names a permission there is not, one for a provider
// with no `when` or for anyone with one. A tag rule whose pattern does not
// compile refuses every tag it would have checked: a rule that cannot be
// evaluated fails closed.
func NewPolicy(r Rules) (*Policy, error) {
	var errs []error
	p := &Policy{}
	names := map[string]bool{}
	issuers := map[string]string{}
	providers := slices.Clone(r.Providers)
	slices.SortStableFunc(providers, func(a, b Provider) int { return strings.Compare(a.Name, b.Name) })
	for _, pv := range providers {
		switch {
		case pv.Name == Anyone || !providerName.MatchString(pv.Name):
			errs = append(errs, fmt.Errorf("provider %q: a provider is named with lowercase letters, digits, `-` and `_`, and is not %q", pv.Name, Anyone))
			continue
		case names[pv.Name]:
			errs = append(errs, fmt.Errorf("provider %q is written twice", pv.Name))
			continue
		}
		if i, ok := pv.Authenticator.(interface{ Issuer() string }); ok {
			if other, ok := issuers[i.Issuer()]; ok {
				errs = append(errs, fmt.Errorf("provider %q: issuer %q is %s's as well", pv.Name, i.Issuer(), other))
				continue
			}
			issuers[i.Issuer()] = pv.Name
		}
		names[pv.Name] = true
		p.providers = append(p.providers, pv)
	}
	for _, pv := range p.providers {
		if pv.Authenticator != nil {
			p.chain = append(p.chain, pv.Authenticator)
		}
	}

	perms := map[string]*permission{}
	for _, name := range slices.Sorted(maps.Keys(r.Permissions)) {
		pm, err := compilePermission(name, r.Permissions[name])
		if err != nil {
			errs = append(errs, fmt.Errorf("permission %q: %w", name, err))
			continue
		}
		perms[name] = pm
	}

	for _, name := range slices.Sorted(maps.Keys(r.Matches)) {
		m, err := compileMatch(name, r.Matches[name], perms, names)
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

func compilePermission(name string, v Permission) (*permission, error) {
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
	pm := &permission{name: name, actions: slices.Clone(v.Actions)}
	for _, raw := range v.Repos {
		src, out := raw, false
		if rest, ok := strings.CutPrefix(src, "!"); ok {
			src, out = rest, true
		}
		g, err := ParseGlob(src)
		if err != nil {
			return nil, err
		}
		pm.repos = append(pm.repos, repoPattern{src: raw, glob: g, out: out})
	}
	if !slices.ContainsFunc(pm.repos, func(p repoPattern) bool { return !p.out }) {
		return nil, errors.New("every pattern in repos takes away, so none gives")
	}
	pm.everywhere = len(v.Repos) == 1 && v.Repos[0] == "**"
	return pm, nil
}

func compileMatch(name string, v Match, perms map[string]*permission, providers map[string]bool) (*match, error) {
	switch {
	case v.For == "":
		return nil, errors.New("no `for`")
	case v.For != Anyone && !providers[v.For]:
		return nil, fmt.Errorf("for %q: no such provider", v.For)
	case v.For == Anyone && len(v.When) > 0:
		return nil, errors.New("`when` for anyone: the anonymous caller has no claims")
	case v.For != Anyone && len(v.When) == 0:
		return nil, fmt.Errorf("no `when`: every credential %q issues, to anybody, would be under it", v.For)
	case len(v.Grant) == 0:
		return nil, errors.New("grants nothing")
	}
	m := &match{name: name, for_: v.For, when: map[string]Glob{}}
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

// Authenticate asks each provider in turn, in the order of their names; the
// first that knows the credential decides.
func (p *Policy) Authenticate(ctx context.Context, username, password string) (Subject, error) {
	if p == nil {
		return Subject{}, ErrNotMine
	}
	for _, a := range p.chain {
		s, err := ask(ctx, a, username, password)
		if !errors.Is(err, ErrNotMine) {
			return s, err
		}
	}
	return Subject{}, ErrNotMine
}

// Exchange is how long a token the exchange trades a credential of provider
// for lasts; zero trades none.
func (p *Policy) Exchange(provider string) time.Duration {
	if p == nil {
		return 0
	}
	for _, pv := range p.providers {
		if pv.Name == provider {
			return pv.Exchange
		}
	}
	return 0
}

// HasProvider reports whether a provider is called name.
func (p *Policy) HasProvider(name string) bool {
	return p != nil && slices.ContainsFunc(p.providers, func(pv Provider) bool { return pv.Name == name })
}

// Exchanges reports whether any provider's credentials may be exchanged.
func (p *Policy) Exchanges() bool {
	return p != nil && slices.ContainsFunc(p.providers, func(pv Provider) bool { return pv.Exchange > 0 })
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

// Explanation is why a caller may do what it may, in a repository or to the
// registry: every match, whether it held and what kept it from holding, and
// what each permission it grants said.
type Explanation struct {
	Matches []MatchExplanation

	// Granted is every action the matches that held grant there.
	Granted []Action
}

type MatchExplanation struct {
	Name   string
	For    string
	Holds  bool
	Why    string // what kept it from holding; empty when it held
	Grants []GrantExplanation
}

type GrantExplanation struct {
	Permission string
	Over       bool   // whether the permission is over the repository
	By         string // the pattern that decided; empty when none matched
	Actions    []Action
}

// Explain is what Allow, or AllowRegistry when repo is empty, reads, with
// its reasons.
func (p *Policy) Explain(s Subject, repo string) Explanation {
	var e Explanation
	if p == nil {
		return e
	}
	for _, m := range p.matches {
		me := MatchExplanation{Name: m.name, For: m.for_, Why: m.why(s)}
		me.Holds = me.Why == ""
		for _, pm := range m.grant {
			g := GrantExplanation{Permission: pm.name, Actions: pm.actions}
			if repo == "" {
				g.Over = pm.everywhere
				if g.Over {
					g.By = "**"
				}
			} else if over, by := pm.decide(repo); by != nil {
				g.Over, g.By = over, by.src
			}
			if me.Holds && g.Over {
				for _, a := range pm.actions {
					// A repository is not asked for the registry's lists.
					registryOnly := a == ActionCatalog || a == ActionSearch
					if !slices.Contains(e.Granted, a) && (repo == "" || !registryOnly) {
						e.Granted = append(e.Granted, a)
					}
				}
			}
			me.Grants = append(me.Grants, g)
		}
		e.Matches = append(e.Matches, me)
	}
	return e
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

// Source is where rules come from: the policy file, the database. A source
// that also has `Revision() string` says which version of itself it gave.
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
	rev     atomic.Pointer[string]

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
	var (
		errs []error
		revs []string
	)
	for _, src := range st.sources {
		r, err := src.Load(ctx)
		if err != nil {
			return err
		}
		if v, ok := src.(interface{ Revision() string }); ok {
			revs = append(revs, v.Revision())
		}
		all.Providers = append(all.Providers, r.Providers...)
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
	rev := strings.Join(revs, ",")
	st.rev.Store(&rev)
	return errors.Join(append(errs, err)...)
}

// Revision is the revisions of the sources that have one, as of the last
// load: what the policy in force was read from.
func (st *PolicyStore) Revision() string {
	if v := st.rev.Load(); v != nil {
		return *v
	}
	return ""
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
