// Package blob is what wraps flob for the registry: the table of well-known
// blobs, the prefix router, and the store over a remote registry.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/opencontainers/go-digest"
)

// Digests of constant content that clients ask for constantly. They are
// answered from memory and never reach a store: the OCI 1.1 empty descriptor
// that every signature, attestation and `oras` artifact uses as its config,
// Docker's empty layer and its uncompressed form, and the empty blob.
var (
	EmptyJSON  = digest.Digest("sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a")
	EmptyLayer = digest.Digest("sha256:a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4")
	EmptyTar   = digest.Digest("sha256:5f70bf18a086007016e948b04aed3b82103a36bea41755b6cddfaf10ace3c6ef")
	Empty      = digest.Digest("sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
)

var wellKnown = map[digest.Digest][]byte{
	EmptyJSON: []byte("{}"),
	// A gzip of EmptyTar, byte for byte what Docker has pushed as the empty
	// layer since schema 1.
	EmptyLayer: {
		0x1f, 0x8b, 0x08, 0x00, 0x00, 0x09, 0x6e, 0x88, 0x00, 0xff, 0x62, 0x18, 0x05, 0xa3, 0x60, 0x14,
		0x8c, 0x58, 0x00, 0x08, 0x00, 0x00, 0xff, 0xff, 0x2e, 0xaf, 0xb5, 0xef, 0x00, 0x04, 0x00, 0x00,
	},
	// Two zeroed tar blocks, the end-of-archive marker and nothing before it.
	EmptyTar: make([]byte, 1024),
	Empty:    {},
}

func init() {
	for d, b := range wellKnown {
		sum := sha256.Sum256(b)
		if got := "sha256:" + hex.EncodeToString(sum[:]); got != d.String() {
			panic(fmt.Sprintf("well-known blob %s hashes to %s", d, got))
		}
	}
}

// WellKnown answers the content of d when it is one of the constant blobs.
// The slice is shared and must not be written to.
func WellKnown(d digest.Digest) ([]byte, bool) {
	b, ok := wellKnown[d]
	return b, ok
}
