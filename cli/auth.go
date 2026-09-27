package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"

	"github.com/lesomnus/otx/log"

	pdauth "github.com/lesomnus/payday/auth"
	"github.com/lesomnus/payday/frame"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/auth/entpolicy"
	"github.com/lesomnus/cr/auth/roster"
	"github.com/lesomnus/cr/cmd"
)

// Guard builds who may use the registry from `auth:`, and nil when the
// configuration turns nothing on -- which is said in the log, loudly, because
// it means anybody who can reach the port may push.
func Guard(ctx context.Context, c *cmd.Config, s *cmd.Server) (*auth.Guard, error) {
	a := c.Auth
	if !a.On() {
		log.From(ctx).WarnContext(ctx, "auth is off: every request may pull, push and delete; configure auth to turn it on")
		return nil, nil
	}

	chain, err := providers(a.Providers)
	if err != nil {
		return nil, err
	}
	for name, m := range a.Matches {
		if _, ok := a.Providers[m.For]; !ok && m.For != auth.Anyone {
			return nil, fmt.Errorf("auth.matches.%s: for %q: no such provider", name, m.For)
		}
	}
	for i, r := range a.TagRules {
		if len(r.Groups) > 0 {
			return nil, fmt.Errorf("auth.tag_rules[%d]: groups: there are no groups to name; a caller with admin moves a protected tag", i)
		}
	}

	var keys []*ecdsa.PrivateKey
	for _, path := range a.Token.Keys {
		k, err := auth.LoadKey(path)
		if err != nil {
			return nil, fmt.Errorf("auth.token.keys: %w", err)
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		k, err := auth.GenerateKey()
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
		log.From(ctx).WarnContext(ctx, "auth.token.keys is empty: tokens are signed with a key made now, and no other replica or restart accepts them")
	}
	name, service := a.Token.Issuer, a.Token.Service
	if name == "" {
		name = "cr"
	}
	if service == "" {
		service = "cr"
	}
	issuer, err := auth.NewIssuer(name, service, a.Token.Ttl, keys...)
	if err != nil {
		return nil, fmt.Errorf("auth.token: %w", err)
	}
	if a.Exchange.Ttl > 0 {
		chain = append(chain, auth.LoginTokens{Issuer: issuer})
	}

	static := staticPolicy(a)
	policy := auth.NewPolicyStore(a.Refresh, static, entpolicy.New(s.Ent)).Measure(meterOf(ctx))
	if err := policy.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("auth: policy: %w", err)
	}
	s.Spin = append(s.Spin, policy)

	log.From(ctx).InfoContext(ctx, "auth", slog.Int("providers", len(a.Providers)), slog.String("service", service))
	g := &auth.Guard{Authenticator: chain, Policy: policy, Issuer: issuer, Realm: a.Token.Realm, Exchange: a.Exchange.Ttl}
	return g.Measure(meterOf(ctx)), nil
}

// Management is how the management API reads a credential: a bearer token
// from `management.tokens`, each acting as the holder it names, and otherwise,
// with `management.roster`, a token roster issued, whose holder and tenant are
// mirrored into this deployment's rows on first sight. Neither is an API no
// network caller can use.
func Management(c *cmd.Config, s *cmd.Server) (pdauth.Handler, error) {
	ids := map[[32]byte]pdauth.Identity{}
	for _, t := range c.Management.Tokens {
		id, err := pdauth.ParseName(t.Holder)
		if err != nil {
			return nil, fmt.Errorf("management.tokens: holder %q: %w", t.Holder, err)
		}
		id.Grant = frame.Whole()

		var sum [32]byte
		switch {
		case t.TokenSha256 != "":
			b, err := hex.DecodeString(t.TokenSha256)
			if err != nil || len(b) != 32 {
				return nil, fmt.Errorf("management.tokens: holder %q: token_sha256 is not a SHA-256 in hex", t.Holder)
			}
			copy(sum[:], b)
		case t.Token != "":
			sum = sha256.Sum256([]byte(t.Token))
		default:
			return nil, fmt.Errorf("management.tokens: holder %q: no token", t.Holder)
		}
		ids[sum] = id
	}

	var remote pdauth.TokenStore
	if r := c.Management.Roster; r.Url != "" {
		client, err := roster.NewClient(r.Url, r.Key)
		if err != nil {
			return nil, fmt.Errorf("management.roster: %w", err)
		}
		remote = pdauth.Remote(client.TokenService())
		s.Resolver = cmd.MirrorResolver(s.Ungated)
	}

	return pdauth.Bearer(pdauth.TokenStoreFunc(func(ctx context.Context, token string) (pdauth.Identity, error) {
		if id, ok := ids[sha256.Sum256([]byte(token))]; ok {
			return id, nil
		}
		if remote != nil {
			return remote.Lookup(ctx, token)
		}
		return pdauth.Identity{}, pdauth.ErrUnknownToken
	})), nil
}

// providerName is what a provider may be called: it is written in front of
// every subject it vouches for, `github:...`, so it has no colon.
var providerName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// providers builds the authenticators `auth.providers` names, in the order of
// their names. Two with one issuer are refused, so that which provider
// vouched for a caller is never a question.
func providers(ps map[string]cmd.ProviderConfig) (auth.Chain, error) {
	chain := auth.Chain{}
	issuers := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(ps)) {
		p := ps[name]
		if name == auth.Anyone || !providerName.MatchString(name) {
			return nil, fmt.Errorf("auth.providers.%s: a provider is named with lowercase letters, digits, `-` and `_`, and is not %q", name, auth.Anyone)
		}
		switch p.Kind {
		case "oidc":
			o, err := auth.NewOIDC(auth.OIDCConfig{
				Name:         name,
				Issuer:       p.Issuer,
				Audience:     p.Audience,
				SubjectClaim: p.SubjectClaim,
			})
			if err != nil {
				return nil, fmt.Errorf("auth.providers.%s: %w", name, err)
			}
			if other, ok := issuers[o.Issuer()]; ok {
				return nil, fmt.Errorf("auth.providers.%s: issuer %q is %s's as well", name, p.Issuer, other)
			}
			issuers[o.Issuer()] = name
			chain = append(chain, o)
		case "":
			return nil, fmt.Errorf("auth.providers.%s: no kind", name)
		default:
			return nil, fmt.Errorf("auth.providers.%s: kind %q: the kind there is is oidc", name, p.Kind)
		}
	}
	return chain, nil
}

// staticPolicy is the permissions, matches and tag rules the configuration
// writes.
func staticPolicy(a cmd.AuthConfig) auth.Static {
	static := auth.Static{
		Permissions: map[string]auth.Permission{},
		Matches:     map[string]auth.Match{},
	}
	for name, p := range a.Permissions {
		as := make([]auth.Action, len(p.Actions))
		for i, v := range p.Actions {
			as[i] = auth.Action(v)
		}
		static.Permissions[name] = auth.Permission{Repos: p.Repos, Actions: as}
	}
	for name, m := range a.Matches {
		static.Matches[name] = auth.Match{For: m.For, When: m.When, Grant: m.Grant}
	}
	for _, r := range a.TagRules {
		static.TagRules = append(static.TagRules, auth.TagRule{
			Name:    r.Name,
			Repo:    r.Repo,
			Tag:     r.Tag,
			Kind:    auth.TagRuleKind(r.Kind),
			Pattern: r.Pattern,
			Keep:    r.Keep,
		})
	}
	return static
}
