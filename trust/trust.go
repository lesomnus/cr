// Package trust decides whether an image is signed by somebody a pull-through
// cache was told to trust: a Notary Project (notation) signature, an X.509
// chain to one of the configured roots, from a signer whose subject is one of
// the configured identities.
//
// It is notation's own verifier, given a trust policy and a trust store made
// here from the cache's configuration rather than read from notation's
// directories. What a robot's `notation verify` accepts, this accepts.
//
// Nothing here reaches the network. Revocation is not checked -- a cache may
// stand where nothing else can be reached, and a check that cannot reach its
// OCSP responder fails closed -- and the timestamp authority's chain is not
// sent to one either.
package trust

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/notaryproject/notation-core-go/revocation"
	"github.com/notaryproject/notation-core-go/revocation/result"
	"github.com/notaryproject/notation-go"
	"github.com/notaryproject/notation-go/verifier"
	"github.com/notaryproject/notation-go/verifier/trustpolicy"
	"github.com/notaryproject/notation-go/verifier/truststore"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ArtifactType is the artifact type of a notation signature: what a referrer
// is, to be one.
const ArtifactType = "application/vnd.cncf.notary.signature"

// The envelopes a notation signature comes in, as the media type of its one
// layer.
const (
	MediaTypeJWS  = "application/jose+json"
	MediaTypeCOSE = "application/cose"
)

// Mode is what is done with an image that does not verify.
type Mode string

const (
	// Require refuses it.
	Require Mode = "require"
	// Audit serves it, and says so: for finding out what would be refused
	// before anything is.
	Audit Mode = "audit"
)

// Config is who a cache trusts to sign what it serves.
type Config struct {
	// Mode is Require when empty.
	Mode Mode

	// Roots are PEM files of the CAs a signer's chain must end at. The
	// root, not an intermediate: an envelope carries its chain up to the
	// root, and a chain that ends elsewhere does not verify.
	Roots []string

	// Identities are the subjects a signing certificate may have, as a
	// distinguished name: `C=KR, ST=Seoul, O=Holiday Robotics, CN=Image
	// Signer`. A name is matched on the attributes it gives, so a shorter one
	// admits more -- down to C, ST and O, which notation requires of every
	// name. `*` is any certificate the roots issued, and is alone if given.
	Identities []string

	// TSARoots are PEM files of the CAs a timestamp authority's chain must
	// end at. A signature whose certificate is still valid is accepted with
	// or without a timestamp; one whose certificate has expired is accepted
	// only with a timestamp these roots vouch for, from within its validity.
	// None is that an expired certificate's signature is not accepted.
	TSARoots []string
}

// Verifier checks signatures against one Config.
type Verifier struct {
	mode Mode
	v    notation.Verifier
}

const (
	policyName = "cr"
	storeName  = "cr"
)

// New reads the roots c names and makes a verifier of them.
func New(c Config) (*Verifier, error) {
	mode := c.Mode
	switch mode {
	case "":
		mode = Require
	case Require, Audit:
	default:
		return nil, fmt.Errorf("mode: %q is neither require nor audit", c.Mode)
	}

	roots, err := readRoots("roots", c.Roots)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, errors.New("roots: none; a signature is trusted by the root its chain ends at")
	}
	tsa, err := readRoots("tsa_roots", c.TSARoots)
	if err != nil {
		return nil, err
	}

	if len(c.Identities) == 0 {
		return nil, errors.New("identities: none; say which signers, or `*` for any the roots issued")
	}
	ids := make([]string, len(c.Identities))
	for i, id := range c.Identities {
		id = strings.TrimSpace(id)
		switch {
		case id == "":
			return nil, fmt.Errorf("identities[%d]: empty", i)
		case id == "*":
			ids[i] = id
		default:
			ids[i] = "x509.subject: " + id
		}
	}

	stores := []string{string(truststore.TypeCA) + ":" + storeName}
	sv := trustpolicy.SignatureVerification{
		VerificationLevel: trustpolicy.LevelStrict.Name,
		Override: map[trustpolicy.ValidationType]trustpolicy.ValidationAction{
			trustpolicy.TypeRevocation: trustpolicy.ActionSkip,
		},
	}
	if len(tsa) > 0 {
		stores = append(stores, string(truststore.TypeTSA)+":"+storeName)
		// notation's own default, written out: its empty value is `always`.
		sv.VerifyTimestamp = trustpolicy.OptionAfterCertExpiry
	}
	doc := &trustpolicy.Document{
		Version: "1.0",
		TrustPolicies: []trustpolicy.TrustPolicy{{
			Name: policyName,
			// The only wildcard notation takes; which repositories are
			// checked is the cache's to say, by which proxies verify.
			RegistryScopes:        []string{"*"},
			SignatureVerification: sv,
			TrustStores:           stores,
			TrustedIdentities:     ids,
		}},
	}
	// notation reports a policy it cannot use with its own words, which name
	// its file; this is the cache's configuration.
	if err := doc.Validate(); err != nil {
		return nil, fmt.Errorf("identities: %w", err)
	}

	v, err := verifier.NewWithOptions(doc, store{truststore.TypeCA: roots, truststore.TypeTSA: tsa}, nil, verifier.VerifierOptions{
		RevocationCodeSigningValidator:  unchecked{},
		RevocationTimestampingValidator: unchecked{},
	})
	if err != nil {
		return nil, err
	}
	return &Verifier{mode: mode, v: v}, nil
}

// Mode is what is done with an image that does not verify.
func (v *Verifier) Mode() Mode { return v.mode }

// Verify checks that envelope, a signature whose envelope is of mediaType, is
// a signature of subject by somebody this verifier trusts. ref is the image's
// name, for what is logged.
func (v *Verifier) Verify(ctx context.Context, ref string, subject ocispec.Descriptor, mediaType string, envelope []byte) error {
	switch mediaType {
	case MediaTypeJWS, MediaTypeCOSE:
	default:
		return fmt.Errorf("a signature envelope of %q is not one notation writes", mediaType)
	}
	_, err := v.v.Verify(ctx, subject, envelope, notation.VerifierVerifyOptions{
		ArtifactReference:  ref,
		SignatureMediaType: mediaType,
	})
	return err
}

// readRoots reads the certificates in files, which must each hold at least
// one, and all of them CAs.
func readRoots(field string, files []string) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for i, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", field, i, err)
		}
		n := 0
		for {
			var block *pem.Block
			block, b = pem.Decode(b)
			if block == nil {
				break
			}
			if block.Type != "CERTIFICATE" {
				continue
			}
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%s[%d]: %s: %w", field, i, f, err)
			}
			if !c.IsCA {
				return nil, fmt.Errorf("%s[%d]: %s: %q is not a CA", field, i, f, c.Subject)
			}
			certs = append(certs, c)
			n++
		}
		if n == 0 {
			return nil, fmt.Errorf("%s[%d]: %s holds no certificate", field, i, f)
		}
	}
	return certs, nil
}

// store is the trust store notation is given: the roots of each kind, under
// the one name the policy asks for.
type store map[truststore.Type][]*x509.Certificate

func (s store) GetCertificates(_ context.Context, t truststore.Type, name string) ([]*x509.Certificate, error) {
	if name != storeName || len(s[t]) == 0 {
		return nil, truststore.TrustStoreError{Msg: fmt.Sprintf("no %s roots", t)}
	}
	return s[t], nil
}

// unchecked is the revocation check that is not done. notation skips the
// signer's chain's check when the policy says so, but always checks a
// timestamp authority's, and its own checker would ask the network. Every
// certificate is answered as one there is no way to revoke.
type unchecked struct{}

func (unchecked) ValidateContext(_ context.Context, o revocation.ValidateContextOptions) ([]*result.CertRevocationResult, error) {
	out := make([]*result.CertRevocationResult, len(o.CertChain))
	for i := range out {
		out[i] = &result.CertRevocationResult{Result: result.ResultNonRevokable}
	}
	return out, nil
}
