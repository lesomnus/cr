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

// oidcKey is what an OIDC provider is made of: one whose `when` alone
// changed keeps the keys it fetched.
type oidcKey struct {
	name, issuer, audience, subjectClaim string
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
		r.Permissions[name] = permissionOf(p)
	}
	for name, m := range f.Matches {
		grant := make([]string, 0, len(m.Grant))
		for i, g := range m.Grant {
			if g.Inline == nil {
				// Only a permission the file names: what is written in
				// place is the match's own, and not another's to grant.
				if _, ok := f.Permissions[g.Name]; !ok {
					errs = append(errs, fmt.Errorf("match %q: grant[%d]: no permission %q", name, i, g.Name))
				}
				grant = append(grant, g.Name)
				continue
			}
			// Named by where it is written, which is how explain and the
			// errors about it point at it.
			pn := fmt.Sprintf("%s.grant[%d]", name, i)
			if _, ok := f.Permissions[pn]; ok {
				errs = append(errs, fmt.Errorf("match %q: grant[%d]: a permission is already called %q", name, i, pn))
				continue
			}
			r.Permissions[pn] = permissionOf(*g.Inline)
			grant = append(grant, pn)
		}
		r.Matches[name] = auth.Match{For: m.For, When: m.When, Grant: grant}
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

func permissionOf(p cmd.PermissionConfig) auth.Permission {
	as := make([]auth.Action, len(p.Actions))
	for i, v := range p.Actions {
		as[i] = auth.Action(v)
	}
	return auth.Permission{Repos: p.Repos, Actions: as}
}

func provider(name string, p cmd.ProviderConfig, oidcs map[oidcKey]*auth.OIDC) (auth.Provider, error) {
	if p.Exchange < 0 {
		return auth.Provider{}, errors.New("exchange: a negative duration")
	}
	switch p.Kind {
	case "oidc":
		k := oidcKey{name: name, issuer: p.Issuer, audience: p.Audience, subjectClaim: p.SubjectClaim}
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
		return auth.Provider{Name: name, Authenticator: o, Exchange: p.Exchange, When: p.When}, nil
	case "mtls":
		// Which certificates are good is the listener's `client_ca_file`; a
		// field here would be a second place to say it, and disagree.
		switch {
		case p.Issuer != "", p.Audience != "", p.SubjectClaim != "":
			return auth.Provider{}, errors.New("issuer, audience, subject_claim: an mtls provider is configured by the listener's tls.client_ca_file")
		case p.Exchange != 0:
			return auth.Provider{}, errors.New("exchange: a certificate is not traded for a token")
		}
		return auth.Provider{Name: name, Authenticator: auth.NewMTLS(name), When: p.When}, nil
	case "":
		return auth.Provider{}, errors.New("no kind")
	}
	return auth.Provider{}, fmt.Errorf("kind %q: the kinds there are are oidc and mtls", p.Kind)
}
