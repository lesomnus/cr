package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
)

// UpstreamTarget configures one origin. Options (including credentials) apply
// only to this URL, never to another target in the group.
type UpstreamTarget struct {
	URL     string
	Options []UpstreamOption
}

// NewUpstreams configures explicit ordered origins. Each has its own HTTP
// client, credentials and challenge-token cache.
// String and Registry retain the primary origin's identity.
func NewUpstreams(targets []UpstreamTarget) (*Upstream, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("upstreams: at least one URL is required")
	}
	origins := make([]*Upstream, len(targets))
	for i, target := range targets {
		u, err := NewUpstream(target.URL, target.Options...)
		if err != nil {
			return nil, fmt.Errorf("upstreams[%d].url: %w", i, err)
		}
		if u.base.User != nil || u.base.RawQuery != "" || u.base.ForceQuery || u.base.Fragment != "" {
			return nil, fmt.Errorf("upstreams[%d].url: URL must not contain credentials, query or fragment", i)
		}
		origins[i] = u
	}
	origins[0].fallbacks = origins[1:]
	return origins[0], nil
}

func (u *Upstream) origins() []*Upstream {
	return append([]*Upstream{u}, u.fallbacks...)
}

func retryUpstream(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	var status *UpstreamStatusError
	if errors.As(err, &status) {
		return status.Status >= 500 && status.Status <= 599
	}
	return errors.Is(err, ErrUpstreamUnreachable)
}

// Invalid content stays terminal. Only failures while reading response bytes
// are classified here, before any JSON/digest validation.
func upstreamReadError(err error) error {
	var network net.Error
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &network) {
		return fmt.Errorf("%w: %w", ErrUpstreamUnreachable, err)
	}
	return err
}

func logUpstreamFallback(ctx context.Context, from, to *Upstream, operation string) {
	slog.WarnContext(ctx, "upstream fallback", "from", from.base.Host, "to", to.base.Host, "operation", operation)
}

func tryUpstreams[T any](ctx context.Context, u *Upstream, operation string, run func(*Upstream) (T, error)) (T, error) {
	origins := u.origins()
	var zero T
	var failures []error
	for i, origin := range origins {
		value, err := run(origin)
		if err == nil || !retryUpstream(ctx, err) || len(origins) == 1 {
			return value, err
		}
		failures = append(failures, fmt.Errorf("upstream %s: %w", origin.base.Host, err))
		if i+1 < len(origins) {
			logUpstreamFallback(ctx, origin, origins[i+1], operation)
		}
	}
	return zero, errors.Join(failures...)
}

// GetManifest answers the manifest reference names in repo: its bytes, its
// media type, and its digest, checked against reference when that is one.
func (u *Upstream) GetManifest(ctx context.Context, repo, reference string, max int64) ([]byte, string, digest.Digest, error) {
	if len(u.fallbacks) > 0 && !strings.Contains(reference, ":") {
		// Resolve once. A fallback's mutable tag must not select other content.
		d, err := u.HeadManifest(ctx, repo, reference)
		if err != nil {
			return nil, "", "", err
		}
		if d == "" {
			return nil, "", "", fmt.Errorf("upstream: tag %q did not resolve to a digest", reference)
		}
		reference = d.String()
	}
	type manifest struct {
		body      []byte
		mediaType string
		digest    digest.Digest
	}
	m, err := tryUpstreams(ctx, u, "manifest get", func(origin *Upstream) (manifest, error) {
		body, mt, d, err := origin.getManifest(ctx, repo, reference, max)
		return manifest{body, mt, d}, err
	})
	return m.body, m.mediaType, m.digest, err
}

func (s *upstreamStore) Stat(ctx context.Context, d flob.Digest) (flob.Info, error) {
	return tryUpstreams(ctx, s.u, "blob head", func(origin *Upstream) (flob.Info, error) {
		return (&upstreamStore{u: origin, repo: s.repo}).stat(ctx, d)
	})
}

func (s *upstreamStore) Open(ctx context.Context, d flob.Digest) (io.ReadSeekCloser, flob.Info, error) {
	type opened struct {
		reader *upstreamReader
		info   flob.Info
	}
	r, err := tryUpstreams(ctx, s.u, "blob get", func(origin *Upstream) (opened, error) {
		reader, info, err := (&upstreamStore{u: origin, repo: s.repo}).open(ctx, d)
		return opened{reader, info}, err
	})
	if err != nil {
		return nil, nil, err
	}
	origins := s.u.origins()
	r.reader.ordered = len(origins) > 1
	for i, origin := range origins {
		if origin == r.reader.s.u {
			r.reader.remaining = origins[i+1:]
			break
		}
	}
	return r.reader, r.info, nil
}
