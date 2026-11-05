package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// JWT verifies externally issued tokens against a locally supplied public
// JWKS. It never discovers an issuer or fetches keys over the network.
type JWT struct {
	config JWTConfig
	now    func() time.Time
}

type JWTConfig struct {
	Name, Issuer, Audience string
	JWKS                   SecretValue
	SubjectClaim           string

	// AudienceHost compares host[:port] rather than the whole audience. This
	// accepts both registry.example and https://registry.example. It is off
	// by default, so an audience that names another path is not equivalent.
	AudienceHost bool
}

func NewJWT(c JWTConfig) (*JWT, error) {
	if c.Name == "" || c.Issuer == "" || c.Audience == "" || c.JWKS == nil {
		return nil, errors.New("jwt: name, issuer, audience and jwks are required")
	}
	if c.AudienceHost && audienceHost(c.Audience) == "" {
		return nil, errors.New("jwt: audience has no host")
	}
	if c.SubjectClaim == "" {
		c.SubjectClaim = "sub"
	}
	return &JWT{config: c, now: time.Now}, nil
}

func (j *JWT) Issuer() string { return j.config.Issuer }
func (j *JWT) Kind() string   { return "jwt" }

// bearer opts this provider into direct Bearer authentication. A password
// provider, and an OIDC provider used for the distribution token flow, do not
// become Bearer authenticators merely because they accept a password.
func (j *JWT) bearer() {}

var jwtAlgorithms = []jose.SignatureAlgorithm{
	jose.ES256, jose.ES384, jose.ES512,
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
}

// Authenticate takes the JWT as the password; the username is immaterial.
// scopes is a derived claim, replacing any token-supplied value: the union of
// string capabilities and space-delimited scope, for exact policy matches.
func (j *JWT) Authenticate(_ context.Context, _, password string) (Subject, error) {
	if strings.Count(password, ".") != 2 {
		return Subject{}, ErrNotMine
	}
	tok, err := jwt.ParseSigned(password, jwtAlgorithms)
	if err != nil || len(tok.Headers) != 1 {
		return Subject{}, ErrNotMine
	}
	var peek jwt.Claims
	if err := tok.UnsafeClaimsWithoutVerification(&peek); err != nil || peek.Issuer != j.config.Issuer {
		return Subject{}, ErrNotMine
	}
	key, err := j.key(tok.Headers[0])
	if err != nil {
		return Subject{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	var std jwt.Claims
	var claims map[string]any
	if err := tok.Claims(key.Key, &std, &claims); err != nil {
		return Subject{}, ErrUnauthenticated
	}
	now := j.now()
	if std.Issuer != j.config.Issuer || std.Expiry == nil ||
		!now.Before(std.Expiry.Time().Add(time.Minute)) ||
		(std.NotBefore != nil && now.Add(time.Minute).Before(std.NotBefore.Time())) {
		return Subject{}, ErrUnauthenticated
	}
	matched := false
	for _, aud := range std.Audience {
		if j.config.AudienceHost {
			matched = audienceHost(aud) == audienceHost(j.config.Audience)
		} else {
			matched = aud == j.config.Audience
		}
		if matched {
			break
		}
	}
	if !matched {
		return Subject{}, ErrUnauthenticated
	}
	sub, _ := claims[j.config.SubjectClaim].(string)
	if sub == "" {
		sub = "device"
	}
	scopes := []string{}
	if values, ok := claims["capabilities"].([]any); ok {
		for _, v := range values {
			if s, ok := v.(string); ok {
				scopes = append(scopes, s)
			}
		}
	}
	if scope, ok := claims["scope"].(string); ok {
		for _, s := range strings.Split(scope, " ") {
			if s != "" {
				scopes = append(scopes, s)
			}
		}
	}
	claims["scopes"] = scopes
	return Subject{
		ID: j.config.Name + ":" + sub, Provider: j.config.Name, Claims: claims,
		CredentialExpiry: std.Expiry.Time(),
	}, nil
}

func (j *JWT) key(h jose.Header) (jose.JSONWebKey, error) {
	raw, err := j.config.JWKS.Value()
	if err != nil {
		return jose.JSONWebKey{}, errors.New("jwt: jwks cannot be read")
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal([]byte(raw), &set); err != nil {
		return jose.JSONWebKey{}, errors.New("jwt: invalid jwks")
	}
	usable := []jose.JSONWebKey{}
	for _, key := range set.Keys {
		if !key.Valid() || !key.IsPublic() || (key.Use != "" && key.Use != "sig") ||
			(key.Algorithm != "" && key.Algorithm != h.Algorithm) {
			continue
		}
		if h.KeyID != "" && key.KeyID != h.KeyID {
			continue
		}
		usable = append(usable, key)
	}
	// With no kid, only a single usable key is unambiguous. Rotation requires
	// a kid; an unknown kid never falls back to a different key.
	if len(usable) != 1 {
		return jose.JSONWebKey{}, errors.New("jwt: no unambiguous signing key")
	}
	return usable[0], nil
}

func audienceHost(v string) string {
	if strings.Contains(v, "://") {
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || u.User != nil || strings.Contains(v, "\\") {
			return ""
		}
		host := u.Host
		if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
			host = strings.TrimSuffix(host, ":"+u.Port())
		}
		return strings.ToLower(host)
	}
	if strings.ContainsAny(v, "/?#@\\") {
		return ""
	}
	return strings.ToLower(v)
}
