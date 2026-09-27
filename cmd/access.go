package cmd

import (
	"time"
)

// AuthConfig is who may use the registry and what they may do there.
//
// Providers vouch for callers, permissions say what may be done where, and
// matches grant permissions to the callers a provider vouches for. Tag rules
// come from here and from the management plane's rows together.
type AuthConfig struct {
	// Enabled turns the guard on even when nothing below would, for a
	// deployment whose tag rules are all rows. Any provider, permission,
	// match or tag rule configured here turns it on as well.
	Enabled bool `yaml:"enabled"`

	// Providers are who vouches for a caller, by the name a match's `for`
	// gives them. `anyone` is not one: it is every caller.
	Providers map[string]ProviderConfig `yaml:"providers"`

	// Permissions are actions on repositories, by the name a match's `grant`
	// gives them.
	Permissions map[string]PermissionConfig `yaml:"permissions"`

	// Matches grant permissions to callers. They only add: a caller may do
	// what any match it is under grants, and nothing else.
	Matches map[string]MatchConfig `yaml:"matches"`

	Token    TokenConfig    `yaml:"token"`
	Exchange ExchangeConfig `yaml:"exchange"`

	TagRules []TagRuleConfig `yaml:"tag_rules"`

	// Refresh is how often tag rules are read again from the database; zero
	// is five seconds.
	Refresh time.Duration `yaml:"refresh"`
}

// On reports whether the registry is guarded.
func (c AuthConfig) On() bool {
	return c.Enabled || len(c.Providers) > 0 || len(c.Permissions) > 0 || len(c.Matches) > 0 || len(c.TagRules) > 0
}

// ProviderConfig is one provider of credentials.
type ProviderConfig struct {
	// Kind is what the provider is; `oidc` is the one there is.
	Kind string `yaml:"kind"`

	// Issuer is an `oidc` provider as its ID tokens name it:
	// `https://token.actions.githubusercontent.com`.
	Issuer string `yaml:"issuer"`

	// Audience is what an ID token must be issued for: a GitHub job asks
	// for it with `audience=`. It keeps a token the provider issued for
	// another service from being given here.
	Audience string `yaml:"audience"`

	// SubjectClaim is the claim an ID token's subject is read from; empty is
	// `sub`.
	SubjectClaim string `yaml:"subject_claim"`
}

// PermissionConfig is actions on repositories.
type PermissionConfig struct {
	// Repos are globs, read in order: the last that matches a repository
	// decides, and one with `!` in front takes back what it matches.
	Repos []string `yaml:"repos"`

	// Actions are pull, push, delete, tag, catalog, search, admin, or `*`
	// for all of them.
	Actions []string `yaml:"actions"`
}

// MatchConfig grants permissions to the callers a provider vouches for.
type MatchConfig struct {
	// For is a provider's name, or `anyone` for every caller.
	For string `yaml:"for"`

	// Grant is the names of the permissions granted.
	Grant []string `yaml:"grant"`

	// When is claims of the caller's credential that must all hold, each
	// value a glob. Required for a provider, and not allowed for anyone.
	When map[string]string `yaml:"when"`
}

// ExchangeConfig is `POST /token/exchange`: a credential traded for a token
// cr issued, to be given as a password where the credential behind it lives
// shorter than the job using it.
type ExchangeConfig struct {
	// Ttl is how long the token lasts; zero serves no exchange.
	Ttl time.Duration `yaml:"ttl"`
}

type TokenConfig struct {
	// Issuer is the tokens' `iss`; empty is `cr`.
	Issuer string `yaml:"issuer"`

	// Service is their `aud`, and the `service` of a challenge; empty is `cr`.
	Service string `yaml:"service"`

	// Realm is the token endpoint's URL as a client reaches it; empty is
	// `/token` on the host and scheme a request came in on.
	Realm string `yaml:"realm"`

	// Keys are PEM files of P-256 private keys. The first signs and every one
	// verifies. None is a key made at startup, which is right for one process
	// and wrong for two: a token one issued the other cannot check.
	Keys []string `yaml:"keys"`

	// Ttl is how long a token lasts; zero is five minutes.
	Ttl time.Duration `yaml:"ttl"`
}

type TagRuleConfig struct {
	Name    string   `yaml:"name"`
	Repo    string   `yaml:"repo"`
	Tag     string   `yaml:"tag"`
	Kind    string   `yaml:"kind"`
	Pattern string   `yaml:"pattern"`
	Groups  []string `yaml:"groups"`
	Keep    int      `yaml:"keep"`
}

// ManagementConfig is who may use the management API, which is payday's:
// every entity service, over gRPC and over HTTP beside the registry.
type ManagementConfig struct {
	// Roster is roster answering for the management API's callers: a
	// bearer token is introspected there, and the holder and tenant it names
	// are mirrored into this deployment's rows on first sight.
	Roster RosterConfig `yaml:"roster"`

	// Tokens are bearer tokens, each acting as a holder. None closes the API
	// to every caller over the network; `cr <entity> ...` on the host still
	// reaches it, in-process.
	Tokens []ManagementTokenConfig `yaml:"tokens"`

	// As is the holder `cr <entity> ...` acts as, `@tenant/alias`; empty is
	// `@operator/admin`, which `cr init` puts up.
	As string `yaml:"as"`
}

// RosterConfig is roster answering for the management API's callers.
type RosterConfig struct {
	// Url is roster's data plane over HTTP, `server.http` in roster's
	// configuration: the listener its people and apps call.
	Url string `yaml:"url"`

	// Key is the `rk_` key `roster key add --service cr` made, which must be
	// allowed `/payday.TokenService/Introspect`, `/roster.HolderService/Get`
	// and `/roster.TenantService/Get`.
	Key string `yaml:"key"`
}

type ManagementTokenConfig struct {
	Holder      string `yaml:"holder"`
	Token       string `yaml:"token"`
	TokenSha256 string `yaml:"token_sha256"`
}
