package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx/log"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/oci"
	"github.com/lesomnus/cr/trust"
)

// A proxy with Verify serves a manifest only when somebody it trusts signed
// it, and these are the ways one is not served.
//
// What is checked is the signatures the upstream lists for the manifest -- by
// its referrers API, or the referrers tag schema where it has none -- read
// through the cache like any other manifest and blob, and so read again from
// the cache when the upstream cannot be reached. A signature the upstream no
// longer lists stops counting when the cache next hears the list.
//
// A manifest the signatures were not about may still be served: a platform's
// manifest, when an index that holds it is signed. A release is signed by its
// index, and a client asks for the index first and then the one platform it
// runs, by digest, which nobody signed on its own.

var (
	// errUnsigned is a manifest the upstream lists no signature for.
	errUnsigned = errors.New("not signed")
)

// invalidError is a manifest whose every signature failed to verify, with why
// each did.
type invalidError struct {
	reasons []string
}

func (e *invalidError) Error() string {
	return "no signature verifies: " + strings.Join(e.reasons, "; ")
}

// uncheckedError is a manifest whose signatures could not be read: the
// upstream did not answer, or answered with an error, and the cache had no
// copy. Not the manifest's fault, and not a verdict.
type uncheckedError struct {
	err error
}

func (e *uncheckedError) Error() string { return "could not check its signature: " + e.err.Error() }
func (e *uncheckedError) Unwrap() error { return e.err }

// admit is nil when the manifest m, which ref asked for in name, may be
// served. In audit mode it is always nil, and what would have been refused is
// logged.
func (g *Registry) admit(ctx context.Context, p *Proxy, name string, ref oci.Reference, m index.Manifest) error {
	if p.Verify == nil || m.ArtifactType == trust.ArtifactType {
		// A signature itself is what a verifier reads to decide; it runs
		// nowhere.
		return nil
	}
	if g.isReferrersTagIndex(ctx, name, ref, m) {
		return nil
	}

	err := g.verified(ctx, p, name, m)
	outcome := "verified"
	var invalid *invalidError
	switch {
	case err == nil:
	case errors.Is(err, errUnsigned):
		outcome = "unsigned"
	case errors.As(err, &invalid):
		outcome = "invalid"
	default:
		outcome = "unchecked"
	}
	enforced := p.Verify.Mode() == trust.Require
	prefix := p.Prefix
	if prefix == "" {
		prefix = "*"
	}
	g.verifies.Add(ctx, 1, metric.WithAttributes(
		attribute.String("cr.cache.proxy", prefix),
		attribute.String("cr.verify.outcome", outcome),
		attribute.String("cr.verify.mode", string(p.Verify.Mode())),
	))
	if err == nil {
		return nil
	}

	what := name + "@" + m.Digest.String()
	l := log.From(ctx).With(slog.String("repo", name), slog.String("digest", m.Digest.String()),
		slog.String("outcome", outcome), slog.String("err", err.Error()))
	if !enforced {
		l.WarnContext(ctx, "verify: served unverified (audit)")
		return nil
	}
	l.WarnContext(ctx, "verify: refused")

	if outcome != "unchecked" {
		// The message, not the detail, is what docker shows: `denied: …`.
		e := oci.ErrDenied(err.Error())
		e.Message = what + " is not signed by anybody this registry trusts: " + err.Error()
		return e
	}
	var e *oci.Error
	switch {
	case errors.Is(err, blob.ErrUpstreamUnreachable):
		e = oci.ErrGatewayTimeout(err.Error())
	case upstreamAnswered(err):
		e = oci.ErrBadGateway(err.Error())
	default:
		return err
	}
	e.Message = what + ": " + err.Error()
	return e
}

// verified is nil when m, or an index in name that holds it, is signed by
// somebody p trusts. For a platform's manifest the indexes are asked first:
// that is where a release's signature is, and the manifest's own list of
// signatures, asked of the upstream, is almost always empty.
func (g *Registry) verified(ctx context.Context, p *Proxy, name string, m index.Manifest) error {
	if !oci.IsIndexMediaType(m.MediaType) {
		holders, err := g.c.Index.Manifest().Holders(ctx, name, m.Digest)
		if err != nil {
			return &uncheckedError{err}
		}
		for _, h := range holders {
			hm, err := g.c.Index.Manifest().Get(ctx, name, h)
			if err != nil || !oci.IsIndexMediaType(hm.MediaType) {
				continue
			}
			if g.signed(ctx, p, name, hm) == nil {
				return nil
			}
		}
	}
	return g.signed(ctx, p, name, m)
}

// signed is nil when one of the signatures the upstream lists for m verifies.
func (g *Registry) signed(ctx context.Context, p *Proxy, name string, m index.Manifest) error {
	sigs, err := g.signatures(ctx, p, name, m.Digest)
	if err != nil {
		return &uncheckedError{err}
	}
	if len(sigs) == 0 {
		return errUnsigned
	}

	key := make([]string, 0, len(sigs)+2)
	key = append(key, name, m.Digest.String())
	for _, s := range sigs {
		key = append(key, s.Digest.String())
	}
	k := strings.Join(key, "\x00")
	if err, ok := g.verdicts.get(k); ok {
		return err
	}

	subject := v1.Descriptor{MediaType: m.MediaType, Digest: m.Digest, Size: m.Size}
	// notation names what it verifies as a reference, without a scheme.
	host := p.Upstream.String()
	if _, rest, ok := strings.Cut(host, "://"); ok {
		host = rest
	}
	ref := strings.TrimSuffix(host, "/") + "/" + p.Name(name) + "@" + m.Digest.String()
	var (
		reasons   []string
		unchecked error
	)
	for _, s := range sigs {
		mt, env, err := g.envelope(ctx, p, name, s.Digest)
		if err != nil {
			var bad *invalidError
			if errors.As(err, &bad) {
				reasons = append(reasons, s.Digest.String()+": "+strings.Join(bad.reasons, "; "))
			} else {
				unchecked = err
			}
			continue
		}
		if err := p.Verify.Verify(ctx, ref, subject, mt, env); err != nil {
			reasons = append(reasons, s.Digest.String()+": "+err.Error())
			continue
		}
		g.verdicts.put(k, nil)
		return nil
	}
	if unchecked != nil {
		// The one not read might have been the one that verifies.
		return &uncheckedError{unchecked}
	}
	verdict := &invalidError{reasons}
	g.verdicts.put(k, verdict)
	return verdict
}

// signatures is the notation signatures the upstream lists for d in name: by
// its referrers API, or by the referrers tag schema where it has none.
func (g *Registry) signatures(ctx context.Context, p *Proxy, name string, d digest.Digest) ([]index.Descriptor, error) {
	snap, _, err := g.referrersThrough(ctx, p, name, d)
	if err != nil {
		// Past referrers_max_stale a client is told the upstream failed,
		// since a list that old is not one to hand out as current. A
		// signature is not about now, though: what the upstream last listed
		// was signed then and is signed still, and a cache that has lost its
		// upstream for a day goes on serving what it verified. A signature
		// the upstream removed meanwhile is the cost, until it answers again.
		old, gerr := g.c.Index.Snapshot().Get(ctx, name, d)
		if gerr != nil {
			return nil, err
		}
		log.From(ctx).WarnContext(ctx, "verify: upstream unavailable, reading the referrers it last listed",
			slog.String("repo", name), slog.String("subject", d.String()),
			slog.Time("checked", old.CheckedAt), slog.String("err", err.Error()))
		snap = old
	}
	var all []index.Descriptor
	if snap.Supported {
		all = snap.Descriptors
	} else {
		all, err = g.taggedReferrers(ctx, p, name, d)
		if err != nil {
			return nil, err
		}
	}
	var out []index.Descriptor
	for _, v := range all {
		if v.ArtifactType == trust.ArtifactType {
			out = append(out, v)
		}
	}
	return out, nil
}

// taggedReferrers is what the referrers tag schema lists for d: the index the
// `sha256-<hex>` tag points at. No such tag is no referrers.
func (g *Registry) taggedReferrers(ctx context.Context, p *Proxy, name string, d digest.Digest) ([]index.Descriptor, error) {
	tag := oci.Reference{Tag: referrersTag(d)}
	if err := g.pullThrough(ctx, p, name, tag); err != nil {
		var e *oci.Error
		if errors.As(err, &e) && e.Code == oci.CodeManifestUnknown {
			return nil, nil
		}
		return nil, err
	}
	t, err := g.c.Index.Tag().Get(ctx, name, tag.Tag)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m, err := g.manifest(ctx, name, t.Digest)
	if err != nil {
		return nil, err
	}
	out := make([]index.Descriptor, 0, len(m.Manifests))
	for _, v := range m.Manifests {
		out = append(out, index.Descriptor{
			MediaType:    v.MediaType,
			ArtifactType: v.ArtifactType,
			Digest:       v.Digest,
			Size:         v.Size,
			Annotations:  v.Annotations,
		})
	}
	return out, nil
}

// envelope is the signature manifest d's envelope and its media type, read
// through the cache. A signature that is not one notation writes is an
// [invalidError]; one that cannot be read is any other error.
func (g *Registry) envelope(ctx context.Context, p *Proxy, name string, d digest.Digest) (string, []byte, error) {
	if err := g.pullThrough(ctx, p, name, oci.Reference{Digest: d}); err != nil {
		var e *oci.Error
		if errors.As(err, &e) && e.Code == oci.CodeManifestUnknown {
			return "", nil, &invalidError{[]string{"listed, and not there"}}
		}
		return "", nil, err
	}
	m, err := g.manifest(ctx, name, d)
	if err != nil {
		return "", nil, err
	}
	if len(m.Layers) != 1 {
		return "", nil, &invalidError{[]string{fmt.Sprintf("%d layers, not the one envelope", len(m.Layers))}}
	}
	l := m.Layers[0]
	switch l.MediaType {
	case trust.MediaTypeJWS, trust.MediaTypeCOSE:
	default:
		return "", nil, &invalidError{[]string{fmt.Sprintf("an envelope of %q", l.MediaType)}}
	}
	if l.Size > g.c.MaxManifestSize {
		return "", nil, &invalidError{[]string{fmt.Sprintf("an envelope of %d bytes", l.Size)}}
	}
	env, err := g.read(ctx, name, l.Digest)
	if errors.Is(err, flob.ErrNotExist) {
		return "", nil, &invalidError{[]string{"its envelope is not there"}}
	}
	if err != nil {
		return "", nil, err
	}
	return l.MediaType, env, nil
}

// manifest is the manifest d in name, as the cache holds it.
func (g *Registry) manifest(ctx context.Context, name string, d digest.Digest) (*oci.Manifest, error) {
	rec, err := g.c.Index.Manifest().Get(ctx, name, d)
	if err != nil {
		return nil, err
	}
	body, err := g.read(ctx, name, d)
	if err != nil {
		return nil, err
	}
	return oci.ParseManifest(rec.MediaType, body)
}

// isReferrersTagIndex reports whether ref is a tag of the referrers tag schema
// and m the list it points at: an index of referrers, every one of them with
// an artifact type, which a client reads to find a signature and nothing
// runs. It is served as the referrers API's answer would be.
func (g *Registry) isReferrersTagIndex(ctx context.Context, name string, ref oci.Reference, m index.Manifest) bool {
	if !isReferrersTag(ref.Tag) || !oci.IsIndexMediaType(m.MediaType) {
		return false
	}
	idx, err := g.manifest(ctx, name, m.Digest)
	if err != nil {
		return false
	}
	for _, v := range idx.Manifests {
		if v.ArtifactType == "" || v.Platform != nil {
			return false
		}
	}
	return true
}

// referrersTag is the tag the referrers tag schema lists d's referrers under.
func referrersTag(d digest.Digest) string {
	return d.Algorithm().String() + "-" + d.Encoded()
}

func isReferrersTag(tag string) bool {
	alg, enc, ok := strings.Cut(tag, "-")
	if !ok {
		return false
	}
	return digest.NewDigestFromEncoded(digest.Algorithm(alg), enc).Validate() == nil
}

// verdicts remembers what signatures decided, by the manifest and the
// signatures listed for it: a list that changes is a key not seen before.
// Only verdicts are kept, not a failure to reach one. It is bounded, and
// forgets everything when it is full; a verdict forgotten is one reached
// again.
type verdicts struct {
	mu sync.Mutex
	m  map[string]error
}

const maxVerdicts = 4096

func (v *verdicts) get(k string) (error, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	err, ok := v.m[k]
	return err, ok
}

func (v *verdicts) put(k string, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.m == nil || len(v.m) >= maxVerdicts {
		v.m = map[string]error{}
	}
	v.m[k] = err
}
