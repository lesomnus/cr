// Package oci is what the distribution spec says about names, references,
// digests, manifests and errors, and nothing about where any of it is kept.
package oci

import (
	_ "crypto/sha256"
	_ "crypto/sha512"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/opencontainers/go-digest"
)

// MaxNameLength caps a repository name. The spec leaves the limit to clients,
// and most of them refuse a hostname and name longer than 255 together.
const MaxNameLength = 255

var (
	// The spec's `<name>`, anchored.
	nameRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)
	// The spec's `<reference>` when it is a tag.
	tagRe = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
)

// ValidName reports whether s is a repository name the spec allows.
func ValidName(s string) bool {
	return len(s) <= MaxNameLength && nameRe.MatchString(s)
}

// ValidTag reports whether s is a tag the spec allows.
func ValidTag(s string) bool {
	return tagRe.MatchString(s)
}

var ErrUnsupportedAlgorithm = errors.New("unsupported digest algorithm")

// ParseDigest parses a digest the registry can verify: well formed, and in an
// algorithm flob hashes with.
func ParseDigest(s string) (digest.Digest, error) {
	d := digest.Digest(s)
	i := strings.IndexByte(s, ':')
	if i <= 0 {
		return "", digest.ErrDigestInvalidFormat
	}
	switch d.Algorithm() {
	case digest.SHA256, digest.SHA384, digest.SHA512:
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedAlgorithm, s[:i])
	}
	if err := d.Validate(); err != nil {
		return "", err
	}
	return d, nil
}

// Reference is the `<reference>` of a manifest path: a tag or a digest, never
// both.
type Reference struct {
	Tag    string
	Digest digest.Digest
}

// ParseReference reads a reference as the spec does: a digest if it has a
// colon, which no tag may, and a tag otherwise.
func ParseReference(s string) (Reference, error) {
	if strings.Contains(s, ":") {
		d, err := ParseDigest(s)
		if err != nil {
			return Reference{}, err
		}
		return Reference{Digest: d}, nil
	}
	if !ValidTag(s) {
		return Reference{}, fmt.Errorf("invalid tag %q", s)
	}
	return Reference{Tag: s}, nil
}

func (r Reference) IsDigest() bool { return r.Digest != "" }

func (r Reference) String() string {
	if r.Digest != "" {
		return r.Digest.String()
	}
	return r.Tag
}
