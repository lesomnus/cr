package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// OIDC authenticates an ID token an OpenID Connect provider issued, given as
// the password: what a CI job has instead of a secret. The provider is asked
// for its keys once and again when a token names one it did not have, and the
// token is checked offline against them.
//
// Every claim of the token is the subject's, for a binding's `when`: that is
// what lets one GitHub workflow, and no other, push a repository.
type OIDC struct {
	issuer   string
	audience string
	subject  string
	groups   string
	prefix   string

	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	jwksURI string
	keys    jose.JSONWebKeySet
	fetched time.Time
}

type OIDCConfig struct {
	// Issuer is the provider, as its tokens name it:
	// `https://token.actions.githubusercontent.com`.
	Issuer string

	// Audience is what a token must be for; a GitHub job asks for it with
	// `audience=`.
	Audience string

	// SubjectClaim is the claim the subject is read from; empty is `sub`.
	SubjectClaim string

	// GroupsClaim is a claim holding the subject's groups; empty reads none.
	GroupsClaim string

	// Prefix goes in front of every subject from this issuer, so that one
	// cannot be mistaken for a subject another authenticator names.
	Prefix string
}

func NewOIDC(c OIDCConfig) (*OIDC, error) {
	if c.Issuer == "" || c.Audience == "" {
		return nil, errors.New("oidc: issuer and audience are both required")
	}
	subject := c.SubjectClaim
	if subject == "" {
		subject = "sub"
	}
	return &OIDC{
		issuer:   strings.TrimSuffix(c.Issuer, "/"),
		audience: c.Audience,
		subject:  subject,
		groups:   c.GroupsClaim,
		prefix:   c.Prefix,
		client:   &http.Client{Timeout: 10 * time.Second},
		now:      time.Now,
	}, nil
}

var oidcAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

func (o *OIDC) Authenticate(ctx context.Context, username, password string) (Subject, error) {
	if strings.Count(password, ".") != 2 {
		return Subject{}, ErrNotMine
	}
	tok, err := jwt.ParseSigned(password, oidcAlgorithms)
	if err != nil || len(tok.Headers) == 0 {
		return Subject{}, ErrNotMine
	}
	var peek struct {
		Issuer string `json:"iss"`
	}
	if err := tok.UnsafeClaimsWithoutVerification(&peek); err != nil || strings.TrimSuffix(peek.Issuer, "/") != o.issuer {
		return Subject{}, ErrNotMine
	}

	key, err := o.key(ctx, tok.Headers[0].KeyID)
	if err != nil {
		return Subject{}, err
	}
	var (
		std    jwt.Claims
		claims map[string]any
	)
	if err := tok.Claims(key, &std, &claims); err != nil {
		return Subject{}, ErrUnauthenticated
	}
	if err := std.ValidateWithLeeway(jwt.Expected{
		Issuer:      peek.Issuer,
		AnyAudience: jwt.Audience{o.audience},
		Time:        o.now(),
	}, time.Minute); err != nil {
		return Subject{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}

	sub, _ := claims[o.subject].(string)
	if sub == "" {
		return Subject{}, fmt.Errorf("%w: no %q claim", ErrUnauthenticated, o.subject)
	}
	s := Subject{ID: o.prefix + sub, Claims: claims}
	if o.groups != "" {
		switch v := claims[o.groups].(type) {
		case string:
			s.Groups = []string{v}
		case []any:
			for _, g := range v {
				if g, ok := g.(string); ok {
					s.Groups = append(s.Groups, g)
				}
			}
		}
	}
	return s, nil
}

// key is the provider's key named kid, asking the provider again when it is
// not among the keys already had, but not more than once a minute.
func (o *OIDC) key(ctx context.Context, kid string) (jose.JSONWebKey, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ks := o.keys.Key(kid); len(ks) > 0 {
		return ks[0], nil
	}
	if !o.fetched.IsZero() && o.now().Sub(o.fetched) < time.Minute {
		return jose.JSONWebKey{}, fmt.Errorf("%w: unknown key %q", ErrUnauthenticated, kid)
	}
	if err := o.fetch(ctx); err != nil {
		return jose.JSONWebKey{}, err
	}
	if ks := o.keys.Key(kid); len(ks) > 0 {
		return ks[0], nil
	}
	return jose.JSONWebKey{}, fmt.Errorf("%w: unknown key %q", ErrUnauthenticated, kid)
}

func (o *OIDC) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: %s answered %s", url, res.Status)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(v)
}

func (o *OIDC) fetch(ctx context.Context) error {
	o.fetched = o.now()
	if o.jwksURI == "" {
		var d struct {
			JwksURI string `json:"jwks_uri"`
		}
		if err := o.getJSON(ctx, o.issuer+"/.well-known/openid-configuration", &d); err != nil {
			return err
		}
		if d.JwksURI == "" {
			return errors.New("oidc: the provider names no jwks_uri")
		}
		o.jwksURI = d.JwksURI
	}
	var ks jose.JSONWebKeySet
	if err := o.getJSON(ctx, o.jwksURI, &ks); err != nil {
		return err
	}
	o.keys = ks
	return nil
}
