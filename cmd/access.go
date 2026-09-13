package cmd

import (
	"time"
)

// AuthConfig is who may use the registry and what they may do there.
//
// Bindings and tag rules come from here and from the management plane's rows
// together; what is written here is what a deployment needs before anybody can
// write a row, the operator's own access first of all.
type AuthConfig struct {
	// Enabled turns the guard on even when nothing below would, for a
	// deployment whose bindings are all rows. Any authenticator, binding or
	// tag rule configured here turns it on as well.
	Enabled bool `yaml:"enabled"`

	Htpasswd HtpasswdConfig      `yaml:"htpasswd"`
	Static   []StaticTokenConfig `yaml:"static"`
	Token    TokenConfig         `yaml:"token"`

	Bindings []BindingConfig `yaml:"bindings"`
	TagRules []TagRuleConfig `yaml:"tag_rules"`

	// Refresh is how often bindings and tag rules are read again from the
	// database; zero is five seconds.
	Refresh time.Duration `yaml:"refresh"`
}

// On reports whether the registry is guarded.
func (c AuthConfig) On() bool {
	return c.Enabled || c.Htpasswd.Path != "" || len(c.Static) > 0 || len(c.Bindings) > 0 || len(c.TagRules) > 0
}

type HtpasswdConfig struct {
	// Path is a bcrypt htpasswd file (`htpasswd -B`), read again when it
	// changes.
	Path string `yaml:"path"`

	// Groups are the groups each user is in.
	Groups map[string][]string `yaml:"groups"`
}

// StaticTokenConfig is a long-lived token for CI where there is no roster.
type StaticTokenConfig struct {
	Name string `yaml:"name"`

	// Token is the secret, or TokenSha256 its SHA-256 in hex, which is what a
	// file that is not itself secret should carry.
	Token       string `yaml:"token"`
	TokenSha256 string `yaml:"token_sha256"`

	Groups []string `yaml:"groups"`
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

type BindingConfig struct {
	Subject string            `yaml:"subject"`
	Group   string            `yaml:"group"`
	Repo    string            `yaml:"repo"`
	Actions []string          `yaml:"actions"`
	When    map[string]string `yaml:"when"`
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
	// Tokens are bearer tokens, each acting as a holder. None closes the API
	// to every caller over the network; `cr <entity> ...` on the host still
	// reaches it, in-process.
	Tokens []ManagementTokenConfig `yaml:"tokens"`

	// As is the holder `cr <entity> ...` acts as, `@tenant/alias`; empty is
	// `@operator/admin`, which `cr init` puts up.
	As string `yaml:"as"`
}

type ManagementTokenConfig struct {
	Holder      string `yaml:"holder"`
	Token       string `yaml:"token"`
	TokenSha256 string `yaml:"token_sha256"`
}
