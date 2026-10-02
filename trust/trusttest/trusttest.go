// Package trusttest signs the way a notation signer does, for tests: a root,
// an intermediate under it, and code-signing certificates under that, the
// shape of a chain a signature carries up to the root.
package trusttest

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/notaryproject/notation-go"
	"github.com/notaryproject/notation-go/signer"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

// Subject is the signer's subject a CA issues to unless told otherwise.
var Subject = pkix.Name{
	Country:      []string{"KR"},
	Province:     []string{"Seoul"},
	Organization: []string{"Holiday Robotics"},
	CommonName:   "Kamino Image Signer",
}

// SubjectDN is Subject as a trust policy writes it.
const SubjectDN = "C=KR, ST=Seoul, O=Holiday Robotics, CN=Kamino Image Signer"

var serial atomic.Int64

// CA is a root and an intermediate under it.
type CA struct {
	Root *x509.Certificate

	mid    *x509.Certificate
	midKey crypto.Signer
}

// NewCA makes a root and an intermediate under it, named by name.
func NewCA(t testing.TB, name string) *CA {
	t.Helper()
	rootKey := key(t)
	root := issue(t, &x509.Certificate{
		Subject:               pkix.Name{Organization: []string{"Holiday Robotics"}, CommonName: name + " Root"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}, nil, rootKey, rootKey)
	midKey := key(t)
	mid := issue(t, &x509.Certificate{
		Subject:               pkix.Name{Organization: []string{"Holiday Robotics"}, CommonName: name + " Intermediate"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}, root, rootKey, midKey)
	return &CA{Root: root, mid: mid, midKey: midKey}
}

// RootFile writes the root, PEM, into a file of t's, and answers its path.
func (ca *CA) RootFile(t testing.TB) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "root.crt")
	require.NoError(t, os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Root.Raw}), 0o644))
	return p
}

// Signer is a code-signing certificate and its key.
type Signer struct {
	Cert  *x509.Certificate
	chain []*x509.Certificate
	key   crypto.Signer
}

// SignerOption shapes the certificate a CA issues.
type SignerOption func(*x509.Certificate)

// WithSubject issues to s rather than Subject.
func WithSubject(s pkix.Name) SignerOption {
	return func(c *x509.Certificate) { c.Subject = s }
}

// Valid makes the certificate valid from notBefore to notAfter.
func Valid(notBefore, notAfter time.Time) SignerOption {
	return func(c *x509.Certificate) { c.NotBefore, c.NotAfter = notBefore, notAfter }
}

// Revocable names an OCSP responder and a CRL that nothing answers at, so a
// verifier that asked them would fail.
func Revocable() SignerOption {
	return func(c *x509.Certificate) {
		c.OCSPServer = []string{"http://127.0.0.1:1/ocsp"}
		c.CRLDistributionPoints = []string{"http://127.0.0.1:1/crl"}
	}
}

// Signer issues a code-signing certificate under the intermediate.
func (ca *CA) Signer(t testing.TB, opts ...SignerOption) *Signer {
	t.Helper()
	k := key(t)
	tmpl := &x509.Certificate{
		Subject:     Subject,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}
	for _, o := range opts {
		o(tmpl)
	}
	leaf := issue(t, tmpl, ca.mid, ca.midKey, k)
	return &Signer{Cert: leaf, chain: []*x509.Certificate{leaf, ca.mid, ca.Root}, key: k}
}

// Sign signs subject, in an envelope of mediaType, and answers the envelope.
func (s *Signer) Sign(t testing.TB, subject ocispec.Descriptor, mediaType string) []byte {
	t.Helper()
	g, err := signer.NewGenericSigner(s.key, s.chain)
	require.NoError(t, err)
	env, _, err := g.Sign(context.Background(), subject, notation.SignerSignOptions{SignatureMediaType: mediaType})
	require.NoError(t, err)
	return env
}

func key(t testing.TB) crypto.Signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return k
}

// issue signs tmpl with parentKey, as parent, for k; a nil parent is tmpl
// itself. A template with no validity is valid from an hour ago to a day on.
func issue(t testing.TB, tmpl, parent *x509.Certificate, parentKey, k crypto.Signer) *x509.Certificate {
	t.Helper()
	tmpl.SerialNumber = big.NewInt(serial.Add(1))
	if tmpl.NotBefore.IsZero() {
		tmpl.NotBefore = time.Now().Add(-time.Hour)
	}
	if tmpl.NotAfter.IsZero() {
		tmpl.NotAfter = time.Now().Add(24 * time.Hour)
	}
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, k.Public(), parentKey)
	require.NoError(t, err)
	c, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return c
}
