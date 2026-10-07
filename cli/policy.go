package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/xli/cfg"

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

// policyFile is [auth.Source] over a policy file, which cfg reads as a file
// of its own ([cfg.File]): checked whenever it is asked, and new content is
// taken up once it has read the same twice running, so that a file caught
// while it is being written -- cut short where a rule was -- is not taken for
// the policy. Content that does not load or does not check is an error, which
// keeps the policy in force: a registry whose auth is on is never opened by a
// bad edit.
type policyFile struct {
	path string
	file *cfg.File[policyDoc]

	mu sync.Mutex
	// rev is the revision of the content rules were made from.
	rev   string
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
	return &policyFile{path: path, file: cfg.NewFile[policyDoc](path), oidcs: map[oidcKey]*auth.OIDC{}}
}

func (f *policyFile) Load(ctx context.Context) (auth.Rules, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var (
		s   *cfg.Snapshot[policyDoc]
		err error
	)
	if f.file.Current() == nil {
		s, err = f.file.Load()
	} else {
		s, _, err = f.file.Reload()
	}
	if err != nil {
		return auth.Rules{}, fmt.Errorf("auth.policy: %w", about(f.path, err))
	}
	if f.rules != nil && s.Revision == f.rev {
		return *f.rules, nil
	}
	// Checked as it was read; this is the same again, with the providers
	// that are kept.
	r, err := rulesOf((*cmd.PolicyFile)(s.Config), f.oidcs)
	if err != nil {
		return auth.Rules{}, fmt.Errorf("%s: %w", f.path, err)
	}
	f.rev, f.rules = s.Revision, &r
	log.From(ctx).InfoContext(ctx, "auth policy", slog.String("path", f.path), slog.String("revision", f.rev))
	return r, nil
}

// Revision is the policy in force, as the start of its content's SHA-256.
func (f *policyFile) Revision() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rules == nil {
		return ""
	}
	return f.rev
}

// policyDoc is a policy file as cfg reads it, checked as it is read: content
// that does not make a policy is refused by the read itself, so it is never
// the snapshot in force, and a reload keeps the one before.
type policyDoc cmd.PolicyFile

func (d *policyDoc) Validate() error {
	r, err := rulesOf((*cmd.PolicyFile)(d), map[oidcKey]*auth.OIDC{})
	if err != nil {
		return err
	}
	_, err = auth.NewPolicy(r)
	return err
}

// about is err naming the policy file at path. cfg names it for what it read,
// at the line; what the policy's own check finds is about the file as a
// whole, and cfg does not say which file that was.
func about(path string, err error) error {
	var fe *cfg.FieldError
	if errors.As(err, &fe) && fe.Origin.Name == "" {
		return fmt.Errorf("%s: %w", path, err)
	}
	return err
}

// readPolicy reads and checks the policy file at path, as `cr serve` would.
// What it cannot read yet is not an error and is in warnings: a password
// whose file is not there, which keeps its provider from letting anybody in
// until it can be read.
func readPolicy(path string) (p *auth.Policy, warnings []error, err error) {
	s, err := cfg.NewFile[policyDoc](path).Load()
	if err != nil {
		return nil, nil, about(path, err)
	}
	r, err := rulesOf((*cmd.PolicyFile)(s.Config), map[oidcKey]*auth.OIDC{})
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	p, err = auth.NewPolicy(r)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, s.Warnings, nil
}

// rulesOf is a policy file as the auth package takes it. A field the file has
// and nothing reads was refused as it was read: in a policy, a misspelt field
// is a rule that silently is not there.
func rulesOf(f *cmd.PolicyFile, oidcs map[oidcKey]*auth.OIDC) (auth.Rules, error) {
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
	if p.Kind != "secret" && (p.Username != "" || !p.Password.IsZero()) {
		return auth.Provider{}, errors.New("username, password: are a secret provider's")
	}
	switch p.Kind {
	case "secret":
		switch {
		case p.Issuer != "", p.Audience != "", p.SubjectClaim != "":
			return auth.Provider{}, errors.New("issuer, audience, subject_claim: are an oidc provider's")
		case p.Exchange != 0:
			return auth.Provider{}, errors.New("exchange: a password can be given again; there is nothing to trade it for")
		case p.Username == "":
			return auth.Provider{}, errors.New("username: not set")
		case p.Password.IsZero():
			return auth.Provider{}, errors.New("password: not set; write it as ${file:/path}, to keep it out of this file")
		}
		return auth.Provider{Name: name, Authenticator: auth.NewSecret(name, p.Username, p.Password), When: p.When}, nil
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
	return auth.Provider{}, fmt.Errorf("kind %q: the kinds there are are oidc, mtls and secret", p.Kind)
}
