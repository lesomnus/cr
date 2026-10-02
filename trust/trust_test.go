package trust_test

import (
	"context"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/trust"
	"github.com/lesomnus/cr/trust/trusttest"
)

// image is an index, as a release is pinned by.
var image = ocispec.Descriptor{
	MediaType: ocispec.MediaTypeImageIndex,
	Digest:    digest.FromString("an index"),
	Size:      1234,
}

const ref = "cr.example.com/dist/app@sha256:0"

func verifier(t *testing.T, c trust.Config) *trust.Verifier {
	t.Helper()
	v, err := trust.New(c)
	require.NoError(t, err)
	return v
}

func TestVerify(t *testing.T) {
	ctx := context.Background()
	ca := trusttest.NewCA(t, "Kamino")
	v := verifier(t, trust.Config{Roots: []string{ca.RootFile(t)}, Identities: []string{trusttest.SubjectDN}})
	require.Equal(t, trust.Require, v.Mode())

	t.Run("a signature of the image by the signer, either envelope", func(t *testing.T) {
		s := ca.Signer(t)
		for _, mt := range []string{trust.MediaTypeJWS, trust.MediaTypeCOSE} {
			require.NoError(t, v.Verify(ctx, ref, image, mt, s.Sign(t, image, mt)), mt)
		}
	})
	t.Run("not a signature of another image", func(t *testing.T) {
		other := image
		other.Digest = digest.FromString("another index")
		env := ca.Signer(t).Sign(t, other, trust.MediaTypeJWS)
		require.Error(t, v.Verify(ctx, ref, image, trust.MediaTypeJWS, env))
	})
	t.Run("not one by a signer another root issued", func(t *testing.T) {
		env := trusttest.NewCA(t, "Elsewhere").Signer(t).Sign(t, image, trust.MediaTypeJWS)
		require.Error(t, v.Verify(ctx, ref, image, trust.MediaTypeJWS, env))
	})
	t.Run("not one by another signer the root issued", func(t *testing.T) {
		s := ca.Signer(t, trusttest.WithSubject(pkix.Name{
			Country:      []string{"KR"},
			Province:     []string{"Seoul"},
			Organization: []string{"Holiday Robotics"},
			CommonName:   "Somebody Else",
		}))
		require.Error(t, v.Verify(ctx, ref, image, trust.MediaTypeJWS, s.Sign(t, image, trust.MediaTypeJWS)))
	})
	t.Run("not one whose certificate has expired", func(t *testing.T) {
		// Without a timestamp, the certificate's validity is all there is
		// to say when it was signed.
		// A signer will not sign with an expired certificate, so this one
		// expires right after it signs.
		until := time.Now().Add(time.Second).Truncate(time.Second).Add(time.Second)
		s := ca.Signer(t, trusttest.Valid(time.Now().Add(-time.Hour), until))
		env := s.Sign(t, image, trust.MediaTypeJWS)
		time.Sleep(time.Until(until) + 100*time.Millisecond)
		require.ErrorContains(t, v.Verify(ctx, ref, image, trust.MediaTypeJWS, env), "expired")
	})
	t.Run("not one changed after it was signed", func(t *testing.T) {
		env := ca.Signer(t).Sign(t, image, trust.MediaTypeCOSE)
		env[len(env)-10] ^= 0xff
		require.Error(t, v.Verify(ctx, ref, image, trust.MediaTypeCOSE, env))
	})
	t.Run("not one of an envelope notation does not write", func(t *testing.T) {
		env := ca.Signer(t).Sign(t, image, trust.MediaTypeJWS)
		require.ErrorContains(t, v.Verify(ctx, ref, image, "application/octet-stream", env), "not one notation writes")
	})
	t.Run("without asking anybody whether it was revoked", func(t *testing.T) {
		// The certificate names a responder and a CRL nothing answers at.
		s := ca.Signer(t, trusttest.Revocable())
		start := time.Now()
		require.NoError(t, v.Verify(ctx, ref, image, trust.MediaTypeJWS, s.Sign(t, image, trust.MediaTypeJWS)))
		require.Less(t, time.Since(start), time.Second)
	})
}

func TestVerifyIdentities(t *testing.T) {
	ctx := context.Background()
	ca := trusttest.NewCA(t, "Kamino")
	env := ca.Signer(t).Sign(t, image, trust.MediaTypeJWS)

	for name, ids := range map[string][]string{
		"the signer's whole name":     {trusttest.SubjectDN},
		"fewer attributes of it":      {"C=KR, ST=Seoul, O=Holiday Robotics"},
		"any one of several":          {"C=KR, ST=Seoul, O=Holiday Robotics, CN=Somebody Else", trusttest.SubjectDN},
		"anybody the roots issued to": {"*"},
	} {
		t.Run(name, func(t *testing.T) {
			v := verifier(t, trust.Config{Roots: []string{ca.RootFile(t)}, Identities: ids})
			require.NoError(t, v.Verify(ctx, ref, image, trust.MediaTypeJWS, env))
		})
	}
}

func TestNew(t *testing.T) {
	ca := trusttest.NewCA(t, "Kamino")
	root := ca.RootFile(t)
	ids := []string{trusttest.SubjectDN}

	t.Run("audit", func(t *testing.T) {
		v := verifier(t, trust.Config{Mode: trust.Audit, Roots: []string{root}, Identities: ids})
		require.Equal(t, trust.Audit, v.Mode())
	})

	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.crt")
	require.NoError(t, os.WriteFile(empty, []byte("no certificate here\n"), 0o644))
	leaf := filepath.Join(dir, "leaf.crt")
	require.NoError(t, os.WriteFile(leaf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Signer(t).Cert.Raw}), 0o644))

	for name, tc := range map[string]struct {
		c    trust.Config
		want string
	}{
		"a mode there is not":         {trust.Config{Mode: "warn", Roots: []string{root}, Identities: ids}, "mode"},
		"no roots":                    {trust.Config{Identities: ids}, "roots: none"},
		"a root that is not there":    {trust.Config{Roots: []string{filepath.Join(dir, "nope.crt")}, Identities: ids}, "roots[0]"},
		"a root file with none in it": {trust.Config{Roots: []string{empty}, Identities: ids}, "holds no certificate"},
		"a root that is not a CA":     {trust.Config{Roots: []string{leaf}, Identities: ids}, "is not a CA"},
		"a timestamp root not a CA":   {trust.Config{Roots: []string{root}, TSARoots: []string{leaf}, Identities: ids}, "tsa_roots[0]"},
		"no identities":               {trust.Config{Roots: []string{root}}, "identities: none"},
		"an empty identity":           {trust.Config{Roots: []string{root}, Identities: []string{" "}}, "identities[0]"},
		"anybody, and somebody":       {trust.Config{Roots: []string{root}, Identities: []string{"*", trusttest.SubjectDN}}, "identities"},
		"a name that is not a DN":     {trust.Config{Roots: []string{root}, Identities: []string{"Kamino Image Signer"}}, "identities"},
		"a DN without C, ST and O":    {trust.Config{Roots: []string{root}, Identities: []string{"O=Holiday Robotics, CN=Kamino Image Signer"}}, "mandatory"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := trust.New(tc.c)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestRevision(t *testing.T) {
	ca := trusttest.NewCA(t, "Kamino")
	root := ca.RootFile(t)
	ids := []string{trusttest.SubjectDN}
	base := verifier(t, trust.Config{Roots: []string{root}, Identities: ids}).Revision()

	t.Run("the same trust, read again", func(t *testing.T) {
		require.Equal(t, base, verifier(t, trust.Config{Roots: []string{root}, Identities: ids}).Revision())
	})
	t.Run("whatever the mode", func(t *testing.T) {
		require.Equal(t, base, verifier(t, trust.Config{Mode: trust.Audit, Roots: []string{root}, Identities: ids}).Revision())
	})
	t.Run("another root", func(t *testing.T) {
		other := trusttest.NewCA(t, "Elsewhere").RootFile(t)
		require.NotEqual(t, base, verifier(t, trust.Config{Roots: []string{other}, Identities: ids}).Revision())
	})
	t.Run("another identity", func(t *testing.T) {
		require.NotEqual(t, base, verifier(t, trust.Config{Roots: []string{root}, Identities: []string{"*"}}).Revision())
	})
	t.Run("a timestamp root", func(t *testing.T) {
		require.NotEqual(t, base, verifier(t, trust.Config{Roots: []string{root}, TSARoots: []string{root}, Identities: ids}).Revision())
	})
}
