package auth

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"net/http"
)

// MTLS vouches for a caller by the client certificate it presented.
//
// # The listener verified it; this names it
//
// Which certificates are good is the TLS handshake's to say, against the
// listener's `client_ca_file`, before a request exists: a certificate that
// does not verify is a connection that was never made. What is left by the
// time a request reaches the guard is a certificate that verified, and the
// question of who that is -- which is what a provider answers, and what a
// match's `when` reads.
//
// So there is nothing to configure here, and there is one of it: two
// providers of this kind would vouch for the same certificate under two names.
//
// # A credential no client has to be told to send
//
// A container runtime sends a password only to the registry an image is named
// after, never to a mirror in front of it. A client certificate is part of
// the connection to the mirror itself, configured for that host, so a runtime
// that only ever reaches cr as a mirror can still be told apart from one that
// is not trusted at all.
type MTLS struct {
	name string
}

func NewMTLS(name string) *MTLS { return &MTLS{name: name} }

func (m *MTLS) Kind() string { return "mtls" }

// Authenticate is not how a certificate arrives, so it never knows the name.
func (m *MTLS) Authenticate(context.Context, string, string) (Subject, error) {
	return Subject{}, ErrNotMine
}

// Certificate is the caller c is. Its claims, for a match's `when`:
//
//   - `cn`: the subject's common name
//   - `o`, `ou`: the subject's organizations and units
//   - `dns`, `ip`, `uri`, `email`: the subject alternative names
//   - `fingerprint`: the SHA-256 of the certificate, lowercase hex, which is
//     what pins one certificate rather than whatever a CA signs
//
// The subject is `<provider>:<cn>`, or the fingerprint when there is no
// common name.
func (m *MTLS) Certificate(c *x509.Certificate) Subject {
	sum := sha256.Sum256(c.Raw)
	fp := hex.EncodeToString(sum[:])

	ips := make([]any, len(c.IPAddresses))
	for i, ip := range c.IPAddresses {
		ips[i] = ip.String()
	}
	uris := make([]any, len(c.URIs))
	for i, u := range c.URIs {
		uris[i] = u.String()
	}

	id := c.Subject.CommonName
	if id == "" {
		id = fp
	}
	return Subject{
		ID:       m.name + ":" + id,
		Provider: m.name,
		Claims: map[string]any{
			"cn":          c.Subject.CommonName,
			"o":           anys(c.Subject.Organization),
			"ou":          anys(c.Subject.OrganizationalUnit),
			"dns":         anys(c.DNSNames),
			"ip":          ips,
			"uri":         uris,
			"email":       anys(c.EmailAddresses),
			"fingerprint": fp,
		},
		Via: m.Kind(),
	}
}

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// certifier is a provider that vouches for a verified client certificate.
type certifier interface {
	Certificate(c *x509.Certificate) Subject
}

// Certificate is the caller the policy's `mtls` provider says c is, and false
// when the policy has none: a certificate nobody vouches for is no credential.
func (p *Policy) Certificate(c *x509.Certificate) (Subject, bool) {
	if p == nil {
		return Subject{}, false
	}
	for _, pv := range p.providers {
		if v, ok := pv.Authenticator.(certifier); ok {
			return v.Certificate(c), true
		}
	}
	return Subject{}, false
}

// verified is the client certificate r's connection verified, or nil: none
// was asked for, none was given, or r did not come over TLS.
func verified(r *http.Request) *x509.Certificate {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return nil
	}
	return r.TLS.VerifiedChains[0][0]
}

// certificate is the caller r's verified client certificate is, under p.
func (g *Guard) certificate(r *http.Request, p *Policy) (Subject, bool) {
	c := verified(r)
	if c == nil {
		return Subject{}, false
	}
	s, ok := p.Certificate(c)
	if ok {
		g.login(r.Context(), s.Via, "ok")
	}
	return s, ok
}

// Presented reports whether r carries a credential: an Authorization header,
// or a verified client certificate the policy vouches for. A request that
// carries none is the one a registry's `/v2/` challenges.
func (g *Guard) Presented(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return true
	}
	c := verified(r)
	if c == nil {
		return false
	}
	_, ok := g.Policy.Current().Certificate(c)
	return ok
}
