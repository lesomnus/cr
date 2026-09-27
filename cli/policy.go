package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/payday/config"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/cmd"
)

// PolicyName is the policy file's name beside the configuration's, when
// `auth.policy` names none.
const PolicyName = cmd.Name + ".auth.yaml"

// policyPath is the policy file c names, and whether it was named rather than
// taken by default. A relative path is beside the configuration's file. With
// no configuration file there is no default, and the path is empty.
func policyPath(c *cmd.Config) (string, bool) {
	dir := "."
	if c.From != "" {
		dir = filepath.Dir(c.From)
	}
	switch {
	case c.Auth.Policy != "" && filepath.IsAbs(c.Auth.Policy):
		return c.Auth.Policy, true
	case c.Auth.Policy != "":
		return filepath.Join(dir, c.Auth.Policy), true
	case c.From != "":
		return filepath.Join(dir, PolicyName), false
	}
	return "", false
}

func policyPathOf(c *cmd.Config) string {
	p, _ := policyPath(c)
	return p
}

// policySource is the policy file c names, and nil when there is none and
// nothing asks for one: the registry is then open. A file that was named, or
// asked for with `auth.enabled`, must be there.
func policySource(c *cmd.Config) (*policyFile, error) {
	if err := strictAuth(c.From); err != nil {
		return nil, err
	}
	path, named := policyPath(c)
	if path == "" {
		if c.Auth.Enabled {
			return nil, errors.New("auth.enabled, and there is no policy file: name one with auth.policy")
		}
		return nil, nil
	}
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return newPolicyFile(path), nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("auth.policy: %w", err)
	case named:
		return nil, fmt.Errorf("auth.policy: %s is not there", path)
	case c.Auth.Enabled:
		return nil, fmt.Errorf("auth.enabled, and %s is not there", path)
	}
	return nil, nil
}

// strictAuth reads `auth:` of the configuration file at path again, and
// refuses what it has that nothing reads. The loader ignores such a key, and
// under `auth:` an ignored key is a rule that silently is not there.
func strictAuth(path string) error {
	if path == "" {
		return nil
	}
	var raw struct {
		Auth map[string]any `yaml:"auth"`
	}
	if err := config.ReadFile(path, &raw); err != nil {
		return err
	}
	if raw.Auth == nil {
		return nil
	}
	b, err := yaml.Marshal(map[string]any{"auth": raw.Auth})
	if err != nil {
		return err
	}
	var v struct {
		Auth cmd.AuthConfig `yaml:"auth"`
	}
	if err := yaml.UnmarshalWithOptions(b, &v, yaml.Strict()); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// policyFile is [auth.Source] over a policy file. It is read whole every time
// it is asked, and parsed only when its content changed. A file that is not
// there, does not parse or does not check is an error, which keeps the policy
// in force: a registry whose auth is on is never opened by a bad edit.
type policyFile struct {
	path string

	mu    sync.Mutex
	sum   [sha256.Size]byte
	rules *auth.Rules

	// oidcs are the providers built from the file, kept across reloads so
	// that one configured as it was keeps the keys it fetched.
	oidcs map[oidcKey]*auth.OIDC
}

type oidcKey struct {
	name string
	c    cmd.ProviderConfig
}

func newPolicyFile(path string) *policyFile {
	return &policyFile{path: path, oidcs: map[oidcKey]*auth.OIDC{}}
}

func (f *policyFile) Load(ctx context.Context) (auth.Rules, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return auth.Rules{}, fmt.Errorf("auth.policy: %w", err)
	}
	sum := sha256.Sum256(b)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rules != nil && sum == f.sum {
		return *f.rules, nil
	}
	r, err := parsePolicy(b, f.oidcs)
	if err == nil {
		_, err = auth.NewPolicy(r)
	}
	if err != nil {
		return auth.Rules{}, fmt.Errorf("%s: %w", f.path, err)
	}
	f.sum, f.rules = sum, &r
	log.From(ctx).InfoContext(ctx, "auth policy", slog.String("path", f.path), slog.String("revision", f.revision()))
	return r, nil
}

// Revision is the policy in force, as the start of its content's SHA-256.
func (f *policyFile) Revision() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revision()
}

func (f *policyFile) revision() string {
	if f.rules == nil {
		return ""
	}
	return hex.EncodeToString(f.sum[:6])
}

// readPolicy reads and checks the policy file at path, as `cr serve` would.
func readPolicy(path string) (*auth.Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r, err := parsePolicy(b, map[oidcKey]*auth.OIDC{})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	p, err := auth.NewPolicy(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// parsePolicy is a policy file as the auth package takes it. A field the file
// has and nothing reads is an error: in a policy, a misspelt field is a rule
// that silently is not there.
func parsePolicy(b []byte, oidcs map[oidcKey]*auth.OIDC) (auth.Rules, error) {
	var f cmd.PolicyFile
	if err := yaml.UnmarshalWithOptions(b, &f, yaml.Strict()); err != nil {
		return auth.Rules{}, err
	}

	r := auth.Rules{
		Permissions: map[string]auth.Permission{},
		Matches:     map[string]auth.Match{},
	}
	var errs []error
	for name, p := range f.Providers {
		pv, err := provider(name, p, oidcs)
		if err != nil {
			errs = append(errs, fmt.Errorf("providers.%s: %w", name, err))
			continue
		}
		r.Providers = append(r.Providers, pv)
	}
	for name, p := range f.Permissions {
		as := make([]auth.Action, len(p.Actions))
		for i, v := range p.Actions {
			as[i] = auth.Action(v)
		}
		r.Permissions[name] = auth.Permission{Repos: p.Repos, Actions: as}
	}
	for name, m := range f.Matches {
		r.Matches[name] = auth.Match{For: m.For, When: m.When, Grant: m.Grant}
	}
	for _, t := range f.TagRules {
		r.TagRules = append(r.TagRules, auth.TagRule{
			Name:    t.Name,
			Repo:    t.Repo,
			Tag:     t.Tag,
			Kind:    auth.TagRuleKind(t.Kind),
			Pattern: t.Pattern,
			Keep:    t.Keep,
		})
	}
	return r, errors.Join(errs...)
}

func provider(name string, p cmd.ProviderConfig, oidcs map[oidcKey]*auth.OIDC) (auth.Provider, error) {
	if p.Exchange < 0 {
		return auth.Provider{}, errors.New("exchange: a negative duration")
	}
	switch p.Kind {
	case "oidc":
		k := oidcKey{name: name, c: p}
		o, ok := oidcs[k]
		if !ok {
			var err error
			o, err = auth.NewOIDC(auth.OIDCConfig{
				Name:         name,
				Issuer:       p.Issuer,
				Audience:     p.Audience,
				SubjectClaim: p.SubjectClaim,
			})
			if err != nil {
				return auth.Provider{}, err
			}
			oidcs[k] = o
		}
		return auth.Provider{Name: name, Authenticator: o, Exchange: p.Exchange}, nil
	case "":
		return auth.Provider{}, errors.New("no kind")
	}
	return auth.Provider{}, fmt.Errorf("kind %q: the kind there is is oidc", p.Kind)
}
