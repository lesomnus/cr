package cli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/cmd"
)

func TestGuardRefusesToStart(t *testing.T) {
	github := cmd.ProviderConfig{Kind: "oidc", Issuer: "https://token.actions.githubusercontent.com", Audience: "cr"}
	for name, c := range map[string]struct {
		auth cmd.AuthConfig
		want string
	}{
		"a provider of no kind": {
			cmd.AuthConfig{Providers: map[string]cmd.ProviderConfig{"github": {Issuer: github.Issuer, Audience: "cr"}}},
			"no kind",
		},
		"a provider of a kind there is not": {
			cmd.AuthConfig{Providers: map[string]cmd.ProviderConfig{"github": {Kind: "saml"}}},
			`kind "saml"`,
		},
		"a provider with no audience": {
			cmd.AuthConfig{Providers: map[string]cmd.ProviderConfig{"github": {Kind: "oidc", Issuer: github.Issuer}}},
			"audience",
		},
		"a provider called anyone": {
			cmd.AuthConfig{Providers: map[string]cmd.ProviderConfig{"anyone": github}},
			"is not \"anyone\"",
		},
		"a provider with a colon in its name": {
			cmd.AuthConfig{Providers: map[string]cmd.ProviderConfig{"git:hub": github}},
			"lowercase letters",
		},
		"two providers with one issuer": {
			cmd.AuthConfig{Providers: map[string]cmd.ProviderConfig{"a": github, "b": github}},
			"is a's as well",
		},
		"a match for a provider there is not": {
			cmd.AuthConfig{
				Providers: map[string]cmd.ProviderConfig{"github": github},
				Matches:   map[string]cmd.MatchConfig{"release": {For: "gitlab", Grant: []string{"x"}, When: map[string]string{"sub": "x"}}},
			},
			`for "gitlab": no such provider`,
		},
		"a tag rule that names groups": {
			cmd.AuthConfig{TagRules: []cmd.TagRuleConfig{{Repo: "**", Tag: "latest", Kind: "protected", Groups: []string{"release"}}}},
			"there are no groups",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Guard(context.Background(), &cmd.Config{Auth: c.auth}, &cmd.Server{})
			require.ErrorContains(t, err, c.want)
		})
	}
}
