package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/lesomnus/otx/log"

	pdauth "github.com/lesomnus/payday/auth"
	"github.com/lesomnus/payday/frame"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/auth/entpolicy"
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

	chain := auth.Chain{}
	if a.Htpasswd.Path != "" {
		h, err := auth.NewHtpasswd(a.Htpasswd.Path, a.Htpasswd.Groups)
		if err != nil {
			return nil, fmt.Errorf("auth.htpasswd: %w", err)
		}
		chain = append(chain, h)
	}
	if len(a.Static) > 0 {
		ts := make([]auth.StaticToken, 0, len(a.Static))
		for _, v := range a.Static {
			ts = append(ts, auth.StaticToken{Name: v.Name, Token: v.Token, TokenSHA256: v.TokenSha256, Groups: v.Groups})
		}
		t, err := auth.NewTokens(ts)
		if err != nil {
			return nil, fmt.Errorf("auth.static: %w", err)
		}
		chain = append(chain, t)
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

	static := auth.Static{}
	for _, b := range a.Bindings {
		static.Bindings = append(static.Bindings, auth.Binding{
			Subject: b.Subject,
			Group:   b.Group,
			Repo:    b.Repo,
			Actions: auth.ParseActions(b.Actions),
			When:    b.When,
		})
	}
	for _, r := range a.TagRules {
		static.TagRules = append(static.TagRules, auth.TagRule{
			Name:    r.Name,
			Repo:    r.Repo,
			Tag:     r.Tag,
			Kind:    auth.TagRuleKind(r.Kind),
			Pattern: r.Pattern,
			Groups:  r.Groups,
			Keep:    r.Keep,
		})
	}

	policy := auth.NewPolicyStore(a.Refresh, static, entpolicy.New(s.Ent))
	if err := policy.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("auth: policy: %w", err)
	}
	s.Spin = append(s.Spin, policy)

	log.From(ctx).InfoContext(ctx, "auth", slog.Int("authenticators", len(chain)), slog.String("service", service))
	return &auth.Guard{Authenticator: chain, Policy: policy, Issuer: issuer, Realm: a.Token.Realm}, nil
}

// Management is how the management API reads a credential: a bearer token
// from `management.tokens`, each acting as the holder it names. None is an API
// no network caller can use.
func Management(c cmd.ManagementConfig) (pdauth.Handler, error) {
	ids := map[[32]byte]pdauth.Identity{}
	for _, t := range c.Tokens {
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

	return pdauth.Bearer(pdauth.TokenStoreFunc(func(ctx context.Context, token string) (pdauth.Identity, error) {
		id, ok := ids[sha256.Sum256([]byte(token))]
		if !ok {
			return pdauth.Identity{}, pdauth.ErrUnknownToken
		}
		return id, nil
	})), nil
}
