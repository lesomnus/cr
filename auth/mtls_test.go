package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// engineCert is a self-signed client certificate, the shape a robot gives
// each docker engine.
func engineCert(t *testing.T, cn string) *x509.Certificate {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"Holiday Robotics"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"thorb"},
		IPAddresses:  []net.IP{net.ParseIP("192.168.10.34")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	require.NoError(t, err)
	c, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return c
}

// overTLS is r as it arrives on a listener that verified c.
func overTLS(r *http.Request, c *x509.Certificate) *http.Request {
	r.TLS = &tls.ConnectionState{HandshakeComplete: true}
	if c != nil {
		r.TLS.PeerCertificates = []*x509.Certificate{c}
		r.TLS.VerifiedChains = [][]*x509.Certificate{{c}}
	}
	return r
}

// robotRules is the policy a robot's cache runs: released images to the
// engines it trusts, a few of them to anybody, everything else open.
func robotRules() Rules {
	return Rules{
		Providers: []Provider{{Name: "engines", Authenticator: NewMTLS("engines")}},
		Permissions: map[string]Permission{
			"dist":        {Repos: []string{"dist/**"}, Actions: []Action{ActionPull}},
			"dist-public": {Repos: []string{"dist/external/**", "dist/friday", "dist/friday/**"}, Actions: []Action{ActionPull}},
			"local":       {Repos: []string{"**", "!dist", "!dist/**"}, Actions: []Action{ActionPull, ActionPush, ActionTag}},
		},
		Matches: map[string]Match{
			"robots": {For: "engines", Grant: []string{"dist"}, When: map[string]string{"cn": "engine-*", "o": "Holiday Robotics"}},
			"public": {For: Anyone, Grant: []string{"dist-public", "local"}},
		},
	}
}

func robotGuard(t *testing.T) *Guard {
	t.Helper()
	st := NewPolicyStore(time.Hour, Static(robotRules()))
	require.NoError(t, st.Refresh(context.Background()))
	return &Guard{Policy: st, Issuer: issuer(t)}
}

func TestMTLSClaims(t *testing.T) {
	c := engineCert(t, "engine-thorb")
	s := NewMTLS("engines").Certificate(c)

	sum := sha256.Sum256(c.Raw)
	require.Equal(t, "engines:engine-thorb", s.ID)
	require.Equal(t, "engines", s.Provider)
	require.Equal(t, "mtls", s.Via)
	require.Equal(t, map[string]any{
		"cn":          "engine-thorb",
		"o":           []any{"Holiday Robotics"},
		"ou":          []any{},
		"dns":         []any{"thorb"},
		"ip":          []any{"192.168.10.34"},
		"uri":         []any{},
		"email":       []any{},
		"fingerprint": hex.EncodeToString(sum[:]),
	}, s.Claims)

	// No common name: the fingerprint names it.
	c.Subject.CommonName = ""
	require.Equal(t, "engines:"+hex.EncodeToString(sum[:]), NewMTLS("engines").Certificate(c).ID)
}

func TestMTLSCaller(t *testing.T) {
	g := robotGuard(t)
	caller := func(r *http.Request) *Caller {
		t.Helper()
		c, err := g.Caller(r)
		require.NoError(t, err)
		return c
	}

	// A trusted engine pulls a release; nobody else does, and everybody gets
	// what is open.
	engine := caller(overTLS(httptest.NewRequest("GET", "/v2/", nil), engineCert(t, "engine-thorb")))
	require.Equal(t, "engines:engine-thorb", engine.Subject.ID)
	require.True(t, engine.CanPull("dist/hday/cove"))
	require.True(t, engine.CanPull("dist/external/docker.io/library/busybox"))
	require.True(t, engine.CanPull("work/app"))

	for name, r := range map[string]*http.Request{
		"plain http":                    httptest.NewRequest("GET", "/v2/", nil),
		"tls without a certificate":     overTLS(httptest.NewRequest("GET", "/v2/", nil), nil),
		"a certificate the match skips": overTLS(httptest.NewRequest("GET", "/v2/", nil), engineCert(t, "laptop")),
	} {
		c := caller(r)
		require.False(t, c.CanPull("dist/hday/cove"), name)
		require.True(t, c.CanPull("dist/external/docker.io/library/busybox"), name)
		require.True(t, c.CanPull("dist/friday"), name)
		require.False(t, c.CanPull("dist/fridays"), name)
		require.Equal(t, []Action{ActionPush, ActionTag}, c.Allowed("work/app", ActionPush, ActionTag), name)
		require.Empty(t, c.Allowed("dist/friday", ActionPush), name)
	}

	// A header says who the caller is in so many words, and is read first.
	r := overTLS(httptest.NewRequest("GET", "/v2/", nil), engineCert(t, "engine-thorb"))
	access, _, err := g.Issuer.Issue(Subject{ID: Anonymous}, nil)
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer "+access)
	require.Equal(t, Anonymous, caller(r).Subject.ID)
}

// A certificate is a credential only when the policy has someone to vouch for
// it: `/v2/` challenges whatever is not one.
func TestMTLSPresented(t *testing.T) {
	g := robotGuard(t)
	require.True(t, g.Presented(overTLS(httptest.NewRequest("GET", "/v2/", nil), engineCert(t, "laptop"))))
	require.False(t, g.Presented(overTLS(httptest.NewRequest("GET", "/v2/", nil), nil)))
	require.False(t, g.Presented(httptest.NewRequest("GET", "/v2/", nil)))

	r := rules()
	st := NewPolicyStore(time.Hour, Static(r))
	require.NoError(t, st.Refresh(context.Background()))
	none := &Guard{Policy: st, Issuer: issuer(t)}
	require.False(t, none.Presented(overTLS(httptest.NewRequest("GET", "/v2/", nil), engineCert(t, "engine-thorb"))))
}

// A runtime that is challenged asks for a token without a password; the
// connection it asks on still says who it is.
func TestMTLSToken(t *testing.T) {
	g := robotGuard(t)
	req := overTLS(httptest.NewRequest("GET", "/token?service=registry.test&scope="+url.QueryEscape("repository:dist/hday/cove:pull"), nil), engineCert(t, "engine-thorb"))
	w := httptest.NewRecorder()
	g.ServeToken(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var v tokenResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v))
	c, err := g.Issuer.Verify(v.Token)
	require.NoError(t, err)
	require.Equal(t, "engines:engine-thorb", c.Subject)
	require.Equal(t, []string{"pull"}, c.Access[0].Actions)
}

func TestPolicyRefusesTwoMTLSProviders(t *testing.T) {
	r := robotRules()
	r.Providers = append(r.Providers, Provider{Name: "more", Authenticator: NewMTLS("more")})
	_, err := NewPolicy(r)
	require.ErrorContains(t, err, "providers engines, more: there is one mtls provider or none")
}
