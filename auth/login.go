package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/lesomnus/cr/oci"
)

const useLogin = "login"

type loginClaims struct {
	jwt.Claims

	Use      string         `json:"use"`
	Groups   []string       `json:"groups,omitempty"`
	Aliases  []string       `json:"aliases,omitempty"`
	Carried  map[string]any `json:"claims,omitempty"`
	Narrowed bool           `json:"narrowed,omitempty"`
	Only     []string       `json:"only,omitempty"`
}

// IssueLogin signs a token that stands in for s's credential: given as a
// password, it is s again, claims and all, until it expires. It is never an
// access token; Verify refuses it.
func (i *Issuer) IssueLogin(s Subject, ttl time.Duration) (string, time.Time, error) {
	now := i.now()
	id := make([]byte, 16)
	rand.Read(id)
	exp := now.Add(ttl)
	c := loginClaims{
		Claims: jwt.Claims{
			Issuer:    i.name,
			Subject:   s.ID,
			Audience:  jwt.Audience{i.service},
			Expiry:    jwt.NewNumericDate(exp),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        base64.RawURLEncoding.EncodeToString(id),
		},
		Use:      useLogin,
		Groups:   s.Groups,
		Aliases:  s.Aliases,
		Carried:  s.Claims,
		Narrowed: s.Only != nil,
		Only:     Strings(s.Only),
	}
	t, err := jwt.Signed(i.signer).Claims(c).Serialize()
	return t, exp, err
}

// VerifyLogin answers the subject a login token stands for. A string that is
// not one of this issuer's login tokens is [ErrNotMine]; one that is and has
// expired is [ErrUnauthenticated].
func (i *Issuer) VerifyLogin(token string) (Subject, error) {
	if strings.Count(token, ".") != 2 {
		return Subject{}, ErrNotMine
	}
	t, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(t.Headers) == 0 {
		return Subject{}, ErrNotMine
	}
	keys := i.keys.Key(t.Headers[0].KeyID)
	if len(keys) == 0 {
		return Subject{}, ErrNotMine
	}
	var c loginClaims
	if err := t.Claims(keys[0].Key, &c); err != nil || c.Use != useLogin {
		return Subject{}, ErrNotMine
	}
	if err := c.Claims.ValidateWithLeeway(jwt.Expected{
		Issuer:      i.name,
		AnyAudience: jwt.Audience{i.service},
		Time:        i.now(),
	}, 30*time.Second); err != nil {
		return Subject{}, ErrUnauthenticated
	}
	return Subject{ID: c.Subject, Aliases: c.Aliases, Groups: c.Groups, Claims: c.Carried, Only: only(c.Narrowed, c.Only)}, nil
}

// LoginTokens authenticates the tokens `POST /token/exchange` issued.
type LoginTokens struct {
	Issuer *Issuer
}

func (l LoginTokens) Authenticate(ctx context.Context, username, password string) (Subject, error) {
	return l.Issuer.VerifyLogin(password)
}

// ServeExchange is `POST /token/exchange`: a credential the authenticators
// accept -- a CI job's ID token, above all -- traded for a token cr issued
// that stands for the same subject with the same claims, for as long as
// Exchange says. That token is what goes into `docker login` when the ID
// token behind it would expire in the middle of the job.
//
// The credential comes as `Authorization: Bearer`, as Basic, or as an
// RFC 8693 `subject_token`. A login token cannot be exchanged for another,
// or one would never expire.
func (g *Guard) ServeExchange(w http.ResponseWriter, r *http.Request) {
	if g.Exchange <= 0 {
		oci.WriteError(w, oci.NewError(http.StatusNotFound, oci.CodeUnsupported, "no exchange is served"))
		return
	}
	if r.Method != http.MethodPost {
		oci.WriteError(w, oci.ErrUnsupported("method not allowed"))
		return
	}

	user, secret := "", ""
	if u, p, ok := r.BasicAuth(); ok {
		user, secret = u, p
	} else if scheme, rest, _ := strings.Cut(r.Header.Get("Authorization"), " "); strings.EqualFold(scheme, "bearer") {
		secret = strings.TrimSpace(rest)
	} else if err := r.ParseForm(); err == nil {
		secret = r.PostForm.Get("subject_token")
	}
	if secret == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="cr"`)
		oci.WriteError(w, oci.ErrUnauthorized("no credential to exchange"))
		return
	}
	if _, err := g.Issuer.VerifyLogin(secret); !errors.Is(err, ErrNotMine) {
		oci.WriteError(w, oci.NewError(http.StatusBadRequest, oci.CodeDenied, "a token from the exchange cannot be exchanged again"))
		return
	}

	s, err := g.Authenticator.Authenticate(r.Context(), user, secret)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="cr"`)
		oci.WriteError(w, oci.ErrUnauthorized("the credential does not check"))
		return
	}
	token, exp, err := g.Issuer.IssueLogin(s, g.Exchange)
	if err != nil {
		oci.WriteError(w, err)
		return
	}
	b, _ := json.Marshal(map[string]any{
		"access_token":      token,
		"token":             token,
		"issued_token_type": "urn:ietf:params:oauth:token-type:jwt",
		"token_type":        "N_A",
		"expires_in":        int(time.Until(exp).Seconds()),
	})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	h.Set("Cache-Control", "no-store")
	w.Write(b)
}

// Kind is what the count of logins calls this authenticator.
func (l LoginTokens) Kind() string { return "exchange" }
