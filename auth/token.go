package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Access is one entry of a token's `access` claim.
type Access struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// Claims is what a token cr issues says.
type Claims struct {
	jwt.Claims

	Access []Access `json:"access"`

	// Refused is what was asked for and not granted. It is cr's own claim,
	// and what tells "asked and told no", which is 403, from "never asked",
	// which is a 401 that sends the client back for a token that asks.
	Refused []Access `json:"refused,omitempty"`

	// Groups is the subject's groups when the token was issued, for the
	// decisions a token's access cannot carry: protected tags, and which
	// repositories a list shows.
	Groups []string `json:"groups,omitempty"`
}

// Issuer signs the tokens `/token` hands out and verifies the ones `/v2/` is
// given, offline, with ES256 keys.
type Issuer struct {
	name    string
	service string
	ttl     time.Duration

	signer jose.Signer
	keys   jose.JSONWebKeySet

	now func() time.Time
}

// NewIssuer is an issuer named name for service. The first key signs; every
// key verifies, which is how a key is rotated.
func NewIssuer(name, service string, ttl time.Duration, keys ...*ecdsa.PrivateKey) (*Issuer, error) {
	if len(keys) == 0 {
		return nil, errors.New("token: no key")
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	i := &Issuer{name: name, service: service, ttl: ttl, now: time.Now}
	for n, k := range keys {
		if k.Curve != elliptic.P256() {
			return nil, fmt.Errorf("token: key %d: ES256 wants P-256", n)
		}
		pub := jose.JSONWebKey{Key: &k.PublicKey, Algorithm: string(jose.ES256), Use: "sig"}
		tp, err := pub.Thumbprint(crypto.SHA256)
		if err != nil {
			return nil, err
		}
		pub.KeyID = base64.RawURLEncoding.EncodeToString(tp)
		i.keys.Keys = append(i.keys.Keys, pub)
		if n == 0 {
			s, err := jose.NewSigner(
				jose.SigningKey{Algorithm: jose.ES256, Key: k},
				(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), pub.KeyID),
			)
			if err != nil {
				return nil, err
			}
			i.signer = s
		}
	}
	return i, nil
}

func (i *Issuer) Service() string    { return i.service }
func (i *Issuer) TTL() time.Duration { return i.ttl }

// Issue signs a token for s granting access, and saying refused was asked for
// and not granted.
func (i *Issuer) Issue(s Subject, access []Access, refused ...Access) (string, Claims, error) {
	now := i.now()
	id := make([]byte, 16)
	rand.Read(id)
	c := Claims{
		Claims: jwt.Claims{
			Issuer:    i.name,
			Subject:   s.ID,
			Audience:  jwt.Audience{i.service},
			Expiry:    jwt.NewNumericDate(now.Add(i.ttl)),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        base64.RawURLEncoding.EncodeToString(id),
		},
		Access:  access,
		Refused: refused,
		Groups:  s.Groups,
	}
	if c.Access == nil {
		c.Access = []Access{}
	}
	t, err := jwt.Signed(i.signer).Claims(c).Serialize()
	if err != nil {
		return "", Claims{}, err
	}
	return t, c, nil
}

// Verify checks a token's signature, issuer, audience and time, and answers
// what it says.
func (i *Issuer) Verify(token string) (*Claims, error) {
	t, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return nil, err
	}
	if len(t.Headers) == 0 {
		return nil, errors.New("token: no header")
	}
	keys := i.keys.Key(t.Headers[0].KeyID)
	if len(keys) == 0 {
		return nil, errors.New("token: unknown key")
	}
	var c Claims
	if err := t.Claims(keys[0].Key, &c); err != nil {
		return nil, err
	}
	if err := c.Claims.ValidateWithLeeway(jwt.Expected{
		Issuer:      i.name,
		AnyAudience: jwt.Audience{i.service},
		Time:        i.now(),
	}, 30*time.Second); err != nil {
		return nil, err
	}
	return &c, nil
}

// JWKS is the public half of every key, as `/.well-known/jwks.json` serves it.
func (i *Issuer) JWKS() jose.JSONWebKeySet { return i.keys }

// LoadKey reads a P-256 private key from a PEM file, in SEC 1 or PKCS #8.
func LoadKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	switch block.Type {
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		ec, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s: not an EC key", path)
		}
		return ec, nil
	}
	return nil, fmt.Errorf("%s: unexpected PEM block %q", path, block.Type)
}

// GenerateKey makes a P-256 key, for a deployment that named none and whose
// tokens therefore last only as long as the process.
func GenerateKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}
