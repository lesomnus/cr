package cli

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/lesomnus/cr/auth"
)

// The input stays separate from authCase: expansion produces the same concrete
// caller/place assertions as the original format and uses the same evaluator.
type authTests struct {
	Subjects map[string]testSubject `yaml:"subjects"`
	Cases    []authCaseInput        `yaml:"cases"`
}

type testSubject struct {
	Extends  string         `yaml:"extends"`
	Provider string         `yaml:"provider"`
	Claims   map[string]any `yaml:"claims"`
}

type authCaseInput struct {
	Name     string                 `yaml:"name"`
	As       caseValues[caseCaller] `yaml:"as"`
	Repo     caseValues[string]     `yaml:"repo"`
	Registry bool                   `yaml:"registry"`
	Allow    []string               `yaml:"allow"`
	Deny     []string               `yaml:"deny"`
}

// caseValues accepts either the legacy scalar/mapping or an explicit list.
// Remember even a one-element list so the output identifies its combination.
type caseValues[T any] struct {
	values []T
	list   bool
}

func (v *caseValues[T]) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	if _, ok := raw.([]any); ok {
		v.list = true
		return unmarshal(&v.values)
	}
	var one T
	if err := unmarshal(&one); err != nil {
		return err
	}
	v.values = []T{one}
	return nil
}

// A string is anyone or a named fixture; a mapping is the existing inline
// provider/claims syntax. Decode through the same callback to preserve YAML
// anchors and strict unknown-field checking.
type caseCaller struct {
	name   string
	inline caller
}

func (c *caseCaller) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	if name, ok := raw.(string); ok {
		if name == "" {
			return errors.New("as: empty subject name")
		}
		c.name = name
		return nil
	}
	if _, ok := raw.(map[string]any); !ok {
		return errors.New("as: want anyone, a subject name, or a provider and claims")
	}
	return unmarshal(&c.inline)
}

func checkTestCaller(c caller, p *auth.Policy) error {
	if c.Provider == "" {
		return errors.New("no provider")
	}
	if c.Provider != auth.Anyone && !p.HasProvider(c.Provider) {
		return fmt.Errorf("the policy has no provider %q", c.Provider)
	}
	return nil
}

// Resolve every fixture, including unused ones, so a misspelled parent/provider
// or an inheritance cycle cannot hide until a later case starts using it.
func (t authTests) subjects(p *auth.Policy) (map[string]caller, error) {
	resolved := map[string]caller{}
	var resolve func(string, []string) (caller, error)
	resolve = func(name string, path []string) (caller, error) {
		if name == "" || name == auth.Anyone {
			return caller{}, fmt.Errorf("subject name %q is reserved", name)
		}
		if c, ok := resolved[name]; ok {
			return c, nil
		}
		if slices.Contains(path, name) {
			return caller{}, fmt.Errorf("inheritance cycle: %s", strings.Join(append(path, name), " -> "))
		}
		def, ok := t.Subjects[name]
		if !ok {
			return caller{}, fmt.Errorf("unknown subject %q", name)
		}
		var c caller
		if def.Extends != "" {
			parent, err := resolve(def.Extends, append(path, name))
			if err != nil {
				return caller{}, fmt.Errorf("extends %q: %w", def.Extends, err)
			}
			c = caller{Provider: parent.Provider, Claims: maps.Clone(parent.Claims)}
		}
		if def.Provider != "" {
			c.Provider = def.Provider
		}
		if len(def.Claims) > 0 && c.Claims == nil {
			c.Claims = map[string]any{}
		}
		for key, value := range def.Claims {
			c.Claims[key] = value
		}
		if err := checkTestCaller(c, p); err != nil {
			return caller{}, err
		}
		resolved[name] = c
		return c, nil
	}
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(t.Subjects)) {
		if _, err := resolve(name, nil); err != nil {
			errs = append(errs, fmt.Errorf("subjects.%s: %w", name, err))
		}
	}
	return resolved, errors.Join(errs...)
}

// readCases validates the complete file before any assertions run. Each list
// is an independent axis: callers in input order, then repositories in input
// order. Scalar legacy cases keep their original names and behavior.
func readCases(path string, p *auth.Policy) ([]authCase, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t authTests
	if err := yaml.UnmarshalWithOptions(b, &t, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	subjects, err := t.subjects(p)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var cases []authCase
	var errs []error
	for i, input := range t.Cases {
		name := input.Name
		if name == "" {
			name = fmt.Sprintf("cases[%d]", i)
		}
		fail := func(format string, a ...any) {
			errs = append(errs, fmt.Errorf("%s: %s: %s", path, name, fmt.Sprintf(format, a...)))
		}
		if len(input.As.values) == 0 {
			fail("no `as`: supply a caller or a non-empty list")
		}
		repos := input.Repo.values
		if input.Registry {
			// An explicitly empty scalar repo was equivalent to omission in
			// legacy files. Lists always select the repository axis.
			if input.Repo.list || (len(repos) > 0 && repos[0] != "") {
				fail("one of `repo` and `registry: true`")
			}
			repos = []string{""}
		} else {
			if len(repos) == 0 {
				fail("one of `repo` and `registry: true`; repo lists must not be empty")
			}
			for j, repo := range repos {
				if repo == "" {
					fail("repo[%d]: empty repository; use `registry: true` for the registry", j)
				}
			}
		}
		if len(input.Allow)+len(input.Deny) == 0 {
			fail("nothing to allow or deny")
		}
		for _, a := range append(slices.Clone(input.Allow), input.Deny...) {
			switch {
			case !slices.Contains(auth.Actions, auth.Action(a)):
				fail("no action %q", a)
			case input.Registry && !slices.Contains(registryActions, auth.Action(a)):
				fail("%s is asked of a repository, not of the registry", a)
			case !input.Registry && (a == string(auth.ActionCatalog) || a == string(auth.ActionSearch)):
				fail("%s is asked of the registry: write `registry: true`", a)
			}
		}
		for j, as := range input.As.values {
			c := as.inline
			switch as.name {
			case "": // Inline provider and claims.
			case auth.Anyone:
				c = caller{Provider: auth.Anyone}
			default:
				var ok bool
				c, ok = subjects[as.name]
				if !ok {
					fail("as[%d]: unknown subject %q", j, as.name)
					continue
				}
			}
			if err := checkTestCaller(c, p); err != nil {
				fail("as[%d]: %v", j, err)
				continue
			}
			label := as.name
			if label == "" {
				label = fmt.Sprintf("as[%d]:%s", j, c.Provider)
			}
			for _, repo := range repos {
				concreteName := name
				if input.As.list || input.Repo.list || (as.name != "" && as.name != auth.Anyone) {
					place := "registry"
					if !input.Registry {
						place = "repo=" + repo
					}
					concreteName += fmt.Sprintf(" [subject=%s, %s]", label, place)
				}
				cases = append(cases, authCase{Name: concreteName, As: c, Repo: repo, Registry: input.Registry, Allow: input.Allow, Deny: input.Deny})
			}
		}
	}
	if len(t.Cases) == 0 {
		errs = append(errs, fmt.Errorf("%s: no cases", path))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return cases, nil
}
