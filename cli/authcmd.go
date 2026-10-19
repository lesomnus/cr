package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"

	"github.com/lesomnus/cr/auth"
)

// NewCmdAuth is `cr auth check|test|explain`: a policy file read and asked
// about the way `cr serve` would, offline -- no database, and no provider is
// asked for anything.
func NewCmdAuth() *xli.Command {
	return &xli.Command{
		Name:  "auth",
		Brief: "check a policy file, test it, and explain what it grants",

		Commands: xli.Commands{
			{
				Name:  "check",
				Brief: "read a policy file and check it, as cr serve does before it starts",

				Args: arg.Args{&arg.String{Name: "POLICY"}},

				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					path, _ := arg.Get[string](self, "POLICY")
					_, warnings, err := readPolicy(path)
					if err != nil {
						return err
					}
					// Not an error: the policy stands without them, and only
					// their providers let nobody in. But here, before it is
					// deployed, is where to hear of it. What cannot be read is
					// a password whose file is not there, or is empty.
					for _, w := range warnings {
						self.Printf("warning: %v, and its provider lets nobody in until then\n", w)
					}
					self.Printf("%s: ok\n", path)
					return next(ctx)
				}),
			},
			{
				Name:  "test",
				Brief: "run test files or directories against a policy file",

				Args: arg.Args{&arg.String{Name: "POLICY"}, &arg.String{Name: "TESTS"}, &arg.RestStrings{Name: "MORE_TESTS"}},
				Flags: flg.Flags{
					&flg.String{Name: "pattern", Default: new("*.yaml"), Brief: "filename pattern for files directly inside test directories (Go filepath.Match syntax)"},
				},

				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					path, _ := arg.Get[string](self, "POLICY")
					tests, _ := arg.Get[string](self, "TESTS")
					more, _ := arg.Get[[]string](self, "MORE_TESTS")
					pattern := flg.MustGet[string](self, "pattern")
					paths, err := expandCasePaths(append([]string{tests}, more...), pattern)
					if err != nil {
						return err
					}
					p, _, err := readPolicy(path)
					if err != nil {
						return err
					}
					cases, err := readCaseFiles(paths, p)
					if err != nil {
						return err
					}
					failed := 0
					for _, tc := range cases {
						wrong := tc.run(p)
						if len(wrong) == 0 {
							self.Printf("ok    %s\n", tc.Name)
							continue
						}
						failed++
						self.Printf("FAIL  %s\n", tc.Name)
						for _, w := range wrong {
							self.Printf("      %s\n", w)
						}
						printExplanation(self, p.Explain(tc.As.subject(), tc.Repo), "      ")
					}
					if failed > 0 {
						return fmt.Errorf("%d of %d cases failed", failed, len(cases))
					}
					self.Printf("%d cases passed\n", len(cases))
					return next(ctx)
				}),
			},
			{
				Name:  "explain",
				Brief: "say what a policy file grants a caller, and why",

				Args: arg.Args{&arg.String{Name: "POLICY"}},
				Flags: flg.Flags{
					&flg.String{Name: "as", Brief: "the provider that vouches for the caller, or anyone for a caller with no credential"},
					&flg.Strings{Name: "claim", Brief: "a claim of the caller's credential, name=value; repeated for more"},
					&flg.String{Name: "repo", Brief: "the repository; empty is the registry as a whole"},
				},

				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					path, _ := arg.Get[string](self, "POLICY")
					p, _, err := readPolicy(path)
					if err != nil {
						return err
					}
					as := caller{Provider: auth.Anyone}
					if v, ok := flg.Find[string](self, "as"); ok && v != "" {
						as.Provider = v
					}
					if as.Provider != auth.Anyone && !p.HasProvider(as.Provider) {
						return fmt.Errorf("--as %s: the policy has no such provider", as.Provider)
					}
					claims, _ := flg.Find[[]string](self, "claim")
					for _, kv := range claims {
						k, v, ok := strings.Cut(kv, "=")
						if !ok {
							return fmt.Errorf("--claim %q: want name=value", kv)
						}
						if as.Claims == nil {
							as.Claims = map[string]any{}
						}
						as.Claims[k] = v
					}
					if as.Provider == auth.Anyone && len(as.Claims) > 0 {
						return errors.New("--claim: a caller with no credential has no claims; name its provider with --as")
					}
					repo, _ := flg.Find[string](self, "repo")
					printExplanation(self, p.Explain(as.subject(), repo), "")
					return next(ctx)
				}),
			},
		},
	}
}

// printExplanation writes e, a line for every match and one under it for
// every permission it grants.
func printExplanation(self *xli.Command, e auth.Explanation, indent string) {
	for _, m := range e.Matches {
		state := "holds"
		if !m.Holds {
			state = "does not hold: " + m.Why
		}
		self.Printf("%smatch %s (for %s): %s\n", indent, m.Name, m.For, state)
		for _, g := range m.Grants {
			over := "not over it"
			switch {
			case g.Over:
				over = fmt.Sprintf("over it, by %q", g.By)
			case g.By != "":
				over = fmt.Sprintf("not over it, by %q", g.By)
			}
			self.Printf("%s  %s: %s: %s\n", indent, g.Permission, over, strings.Join(auth.Strings(g.Actions), ", "))
		}
	}
	granted := "nothing"
	if len(e.Granted) > 0 {
		granted = strings.Join(auth.Strings(e.Granted), ", ")
	}
	self.Printf("%sgranted: %s\n", indent, granted)
}

type authCase struct {
	Name string `yaml:"name"`
	As   caller `yaml:"as"`

	// Repo is the repository; Registry, in its place, is the registry as a
	// whole: catalog, search and admin.
	Repo     string `yaml:"repo"`
	Registry bool   `yaml:"registry"`

	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

// caller is `anyone`, or a provider and the claims of a credential it issued.
type caller struct {
	Provider string
	Claims   map[string]any
}

func (c *caller) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		if s != auth.Anyone {
			return fmt.Errorf("as: %q: want anyone, or a provider and claims", s)
		}
		*c = caller{Provider: auth.Anyone}
		return nil
	}
	var v struct {
		Provider string         `yaml:"provider"`
		Claims   map[string]any `yaml:"claims"`
	}
	if err := unmarshal(&v); err != nil {
		return err
	}
	if v.Provider == "" {
		return errors.New("as: no provider")
	}
	*c = caller{Provider: v.Provider, Claims: v.Claims}
	return nil
}

func (c caller) subject() auth.Subject {
	if c.Provider == auth.Anyone || c.Provider == "" {
		return auth.Subject{ID: auth.Anonymous}
	}
	sub, _ := c.Claims["sub"].(string)
	return auth.Subject{ID: c.Provider + ":" + sub, Provider: c.Provider, Claims: c.Claims}
}

var registryActions = []auth.Action{auth.ActionCatalog, auth.ActionSearch, auth.ActionAdmin}

// run is what the case got wrong: an action that should have been allowed and
// was not, or the other way round.
func (tc authCase) run(p *auth.Policy) []string {
	s := tc.As.subject()
	var want []auth.Action
	for _, a := range append(slices.Clone(tc.Allow), tc.Deny...) {
		want = append(want, auth.Action(a))
	}
	var granted []auth.Action
	if tc.Registry {
		granted = p.AllowRegistry(s, want)
	} else {
		granted = p.Allow(s, tc.Repo, want)
	}
	var wrong []string
	for _, a := range tc.Allow {
		if !slices.Contains(granted, auth.Action(a)) {
			wrong = append(wrong, a+": denied, and it should be allowed")
		}
	}
	for _, a := range tc.Deny {
		if slices.Contains(granted, auth.Action(a)) {
			wrong = append(wrong, a+": allowed, and it should be denied")
		}
	}
	return wrong
}
