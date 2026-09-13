package oci

import (
	"encoding/json"
	"fmt"
	"mime"
	"strings"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	MediaTypeImageManifest = v1.MediaTypeImageManifest
	MediaTypeImageIndex    = v1.MediaTypeImageIndex
	MediaTypeEmptyJSON     = v1.MediaTypeEmptyJSON

	MediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"

	MediaTypeDockerSchema1       = "application/vnd.docker.distribution.manifest.v1+json"
	MediaTypeDockerSchema1Signed = "application/vnd.docker.distribution.manifest.v1+prettyjws"

	mediaTypeDockerForeignLayer = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"
)

// IsManifestMediaType reports whether cr stores and serves t as a manifest.
func IsManifestMediaType(t string) bool {
	switch t {
	case MediaTypeImageManifest, MediaTypeImageIndex, MediaTypeDockerManifest, MediaTypeDockerManifestList:
		return true
	}
	return false
}

// IsIndexMediaType reports whether t is a list of manifests rather than one.
func IsIndexMediaType(t string) bool {
	return t == MediaTypeImageIndex || t == MediaTypeDockerManifestList
}

// Manifest is what the registry reads out of a pushed manifest: enough to
// validate it, to index it, and to answer referrers without opening it again.
type Manifest struct {
	MediaType string

	// ArtifactType is the manifest's own `artifactType`, or, for an image
	// manifest that has none, its config's media type: what the referrers API
	// reports.
	ArtifactType string

	Subject     *v1.Descriptor
	Config      *v1.Descriptor
	Layers      []v1.Descriptor
	Manifests   []v1.Descriptor
	Annotations map[string]string
}

// Blobs are the descriptors that must exist as blobs in the repository for
// the manifest to be pullable: the config and the layers, less the layers the
// image spec says are not distributed.
func (m *Manifest) Blobs() []v1.Descriptor {
	vs := make([]v1.Descriptor, 0, len(m.Layers)+1)
	if m.Config != nil {
		vs = append(vs, *m.Config)
	}
	for _, l := range m.Layers {
		if !Distributable(l) {
			continue
		}
		vs = append(vs, l)
	}
	return vs
}

// Holds is every digest the manifest refers to other than its subject: config,
// layers and children, deduplicated, in the order they appear.
func (m *Manifest) Holds() []digest.Digest {
	seen := map[digest.Digest]struct{}{}
	vs := []digest.Digest{}
	add := func(d digest.Digest) {
		if _, ok := seen[d]; ok {
			return
		}
		seen[d] = struct{}{}
		vs = append(vs, d)
	}
	if m.Config != nil {
		add(m.Config.Digest)
	}
	for _, l := range m.Layers {
		add(l.Digest)
	}
	for _, c := range m.Manifests {
		add(c.Digest)
	}
	return vs
}

// Distributable reports whether a layer is expected to be in the registry. A
// non-distributable layer, or one that carries its own URLs, may legitimately
// never be pushed.
func Distributable(d v1.Descriptor) bool {
	if strings.Contains(d.MediaType, ".nondistributable.") || d.MediaType == mediaTypeDockerForeignLayer {
		return false
	}
	return len(d.URLs) == 0
}

type rawManifest struct {
	SchemaVersion *int              `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType"`
	Config        *v1.Descriptor    `json:"config"`
	Layers        []v1.Descriptor   `json:"layers"`
	Manifests     []v1.Descriptor   `json:"manifests"`
	Subject       *v1.Descriptor    `json:"subject"`
	Annotations   map[string]string `json:"annotations"`

	// Schema 1 only, and only to be told apart.
	FsLayers json.RawMessage `json:"fsLayers"`
}

// ParseManifest reads body as what contentType says it is. A body whose own
// `mediaType` disagrees with the header is refused; a header that names
// nothing specific defers to the body.
func ParseManifest(contentType string, body []byte) (*Manifest, error) {
	var raw rawManifest
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}

	t := ""
	if contentType != "" {
		v, _, err := mime.ParseMediaType(contentType)
		if err != nil {
			return nil, fmt.Errorf("content type %q: %w", contentType, err)
		}
		t = v
	}
	switch t {
	case "", "application/json", "application/octet-stream":
		t = raw.MediaType
	}
	if t == "" {
		// Neither the header nor the body says. The image spec lets a manifest
		// leave `mediaType` out, so the shape is all there is.
		switch {
		case raw.Manifests != nil:
			t = MediaTypeImageIndex
		case raw.Config != nil:
			t = MediaTypeImageManifest
		}
	}
	if raw.MediaType != "" && raw.MediaType != t {
		return nil, fmt.Errorf("mediaType %q does not match content type %q", raw.MediaType, t)
	}

	switch t {
	case MediaTypeDockerSchema1, MediaTypeDockerSchema1Signed:
		return nil, fmt.Errorf("schema 1 manifests are not supported")
	case MediaTypeImageManifest, MediaTypeDockerManifest:
		if raw.Manifests != nil {
			return nil, fmt.Errorf("an image manifest has no manifests")
		}
		if raw.Config == nil {
			return nil, fmt.Errorf("config is required")
		}
	case MediaTypeImageIndex, MediaTypeDockerManifestList:
		if raw.Config != nil || raw.Layers != nil {
			return nil, fmt.Errorf("an index has no config or layers")
		}
	case "":
		return nil, fmt.Errorf("media type is not given")
	default:
		return nil, fmt.Errorf("unsupported manifest media type %q", t)
	}
	if raw.SchemaVersion == nil || *raw.SchemaVersion != 2 {
		return nil, fmt.Errorf("schemaVersion must be 2")
	}

	m := &Manifest{
		MediaType:    t,
		ArtifactType: raw.ArtifactType,
		Subject:      raw.Subject,
		Config:       raw.Config,
		Layers:       raw.Layers,
		Manifests:    raw.Manifests,
		Annotations:  raw.Annotations,
	}
	if m.Config != nil {
		if err := validDescriptor("config", *m.Config); err != nil {
			return nil, err
		}
		if m.ArtifactType == "" {
			m.ArtifactType = m.Config.MediaType
		}
	}
	for i, l := range m.Layers {
		if err := validDescriptor(fmt.Sprintf("layers[%d]", i), l); err != nil {
			return nil, err
		}
	}
	for i, c := range m.Manifests {
		if err := validDescriptor(fmt.Sprintf("manifests[%d]", i), c); err != nil {
			return nil, err
		}
	}
	if m.Subject != nil {
		if err := validDescriptor("subject", *m.Subject); err != nil {
			return nil, err
		}
	}

	return m, nil
}

func validDescriptor(at string, d v1.Descriptor) error {
	if d.MediaType == "" {
		return fmt.Errorf("%s: mediaType is required", at)
	}
	if err := d.Digest.Validate(); err != nil {
		return fmt.Errorf("%s: digest: %w", at, err)
	}
	if d.Size < 0 {
		return fmt.Errorf("%s: negative size", at)
	}
	return nil
}
