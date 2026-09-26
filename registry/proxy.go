package registry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx/log"
	"github.com/opencontainers/go-digest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/oci"
)

// Proxy makes the repositories under Prefix a pull-through cache of an
// upstream registry. Their manifests are fetched on demand, a tag at a time
// and a digest at a time, so a pull brings the index of a multi-platform image,
// the one platform's manifest the client then asks for, and that platform's
// layers, and nothing else; the blobs come through the store the registry is
// given, which a [blob.Cache] makes read through.
type Proxy struct {
	Prefix   string
	Upstream *blob.Upstream

	// Remote is the upstream repository the prefix stands for; empty maps
	// what follows the prefix to itself.
	Remote string

	// TagTTL is how long a tag is answered from the cache before the upstream
	// is asked again; zero is five minutes. A digest is never asked again.
	TagTTL time.Duration

	// ReferrersTTL is how long a referrers list is answered from what the
	// upstream last said before it is asked again, which is also how long a
	// referrer the upstream removed can still be listed; zero is TagTTL.
	ReferrersTTL time.Duration

	// ReferrersMaxStale is how old what the upstream last said about a
	// referrers list may be and still be answered while the upstream is
	// failing; past it the request fails. Zero is an hour.
	ReferrersMaxStale time.Duration
}

// Name is the upstream repository for repo.
func (p *Proxy) Name(repo string) string {
	rest := strings.TrimPrefix(strings.TrimPrefix(repo, p.Prefix), "/")
	switch {
	case p.Remote == "":
		return rest
	case rest == "":
		return p.Remote
	default:
		return p.Remote + "/" + rest
	}
}

func (p *Proxy) ttl() time.Duration {
	if p.TagTTL <= 0 {
		return 5 * time.Minute
	}
	return p.TagTTL
}

func (p *Proxy) referrersTTL() time.Duration {
	if p.ReferrersTTL <= 0 {
		return p.ttl()
	}
	return p.ReferrersTTL
}

func (p *Proxy) referrersMaxStale() time.Duration {
	if p.ReferrersMaxStale <= 0 {
		return time.Hour
	}
	return p.ReferrersMaxStale
}

// proxies is the proxies, longest prefix first, and when each tag and each
// referrers list was last checked against its upstream.
//
// That is kept in memory on purpose, while what the upstream answered is in
// the index: a restart, or another replica, checks everything again once
// before it answers from the cache, and still has the answer to fall back on.
type proxies struct {
	list []*Proxy

	mu      sync.Mutex
	checked map[string]time.Time
}

func (ps *proxies) of(repo string) *Proxy {
	for _, p := range ps.list {
		if blob.Covers(p.Prefix, repo) {
			return p
		}
	}
	return nil
}

// tagKey and referrersKey are what [proxies.fresh] and [proxies.mark] keep a
// tag and a referrers list under; a tag has no `@`.
func tagKey(repo, tag string) string { return repo + ":" + tag }

func referrersKey(repo string, subject digest.Digest) string {
	return repo + "@" + subject.String()
}

func (ps *proxies) fresh(key string, ttl time.Duration, now time.Time) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	at, ok := ps.checked[key]
	return ok && now.Sub(at) < ttl
}

func (ps *proxies) mark(key string, now time.Time) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.checked == nil {
		ps.checked = map[string]time.Time{}
	}
	// A bound, not an eviction policy: past it every tag is checked again.
	if len(ps.checked) > 100_000 {
		clear(ps.checked)
	}
	ps.checked[key] = now
}

// pullThrough makes sure the manifest ref names in a proxied repository is in
// the cache, fetching it when it is not there or its tag is due for a check.
//
// The upstream being down is not the client's problem when the cache can
// answer: a tag already cached is served as it is, and the failure logged.
func (g *Registry) pullThrough(ctx context.Context, p *Proxy, name string, ref oci.Reference) error {
	outcome, err := g.pullThroughOutcome(ctx, p, name, ref)
	prefix := p.Prefix
	if prefix == "" {
		prefix = "*"
	}
	g.cacheRequests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("cr.cache.proxy", prefix),
		attribute.String("cr.cache.outcome", outcome),
	))
	return err
}

// pullThroughOutcome is pullThrough, saying how the request was answered:
// `hit` from the cache alone, `revalidated` after the upstream said the tag
// had not moved, `refreshed` after it had, `miss` for what was not cached,
// `stale` for a cached tag served because the upstream failed, `unknown` for
// what the upstream does not have, and `error` for the rest.
func (g *Registry) pullThroughOutcome(ctx context.Context, p *Proxy, name string, ref oci.Reference) (string, error) {
	ix := g.c.Index
	remote := p.Name(name)

	if ref.IsDigest() {
		if _, err := ix.Manifest().Get(ctx, name, ref.Digest); err == nil {
			return "hit", nil
		} else if !errors.Is(err, index.ErrNotFound) {
			return "error", err
		}
		if err := g.fetch(ctx, p, name, remote, ref.Digest.String(), ""); err != nil {
			return failedOutcome(err), err
		}
		return "miss", nil
	}

	now := g.c.Now()
	cur, err := ix.Tag().Get(ctx, name, ref.Tag)
	cached := err == nil
	if err != nil && !errors.Is(err, index.ErrNotFound) {
		return "error", err
	}
	if cached && g.proxies.fresh(tagKey(name, ref.Tag), p.ttl(), now) {
		return "hit", nil
	}

	d, err := p.Upstream.HeadManifest(ctx, remote, ref.Tag)
	switch {
	case errors.Is(err, flob.ErrNotExist):
		// Gone upstream. The cache keeps what it has until retention says
		// otherwise; a client asking is told what upstream says.
		return "unknown", oci.ErrManifestUnknown(ref.Tag)
	case err != nil:
		if cached {
			log.From(ctx).WarnContext(ctx, "pull-through: upstream unavailable, serving the cached tag",
				slog.String("repo", name), slog.String("tag", ref.Tag), slog.String("err", err.Error()))
			return "stale", nil
		}
		return "error", err
	}
	if cached && d != "" && d == cur.Digest {
		if _, err := ix.Manifest().Get(ctx, name, d); err == nil {
			g.proxies.mark(tagKey(name, ref.Tag), now)
			return "revalidated", nil
		}
	}
	if err := g.fetch(ctx, p, name, remote, ref.Tag, ref.Tag); err != nil {
		if cached {
			log.From(ctx).WarnContext(ctx, "pull-through: fetch failed, serving the cached tag",
				slog.String("repo", name), slog.String("tag", ref.Tag), slog.String("err", err.Error()))
			return "stale", nil
		}
		return failedOutcome(err), err
	}
	g.proxies.mark(tagKey(name, ref.Tag), now)
	if cached {
		return "refreshed", nil
	}
	return "miss", nil
}

// failedOutcome is `unknown` for what the upstream does not have, and
// `error` for everything else that went wrong.
func failedOutcome(err error) string {
	var e *oci.Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return "unknown"
	}
	return "error"
}

// referrersThrough answers what the upstream says the referrers of subject in
// a proxied repository are, and how old that answer is when it is served
// stale. It is never computed from the manifests the cache holds: those were
// fetched one digest at a time and were never seen to be the whole list, and
// the ones the upstream has since removed are among them.
//
// The upstream's answer is kept as a [index.Snapshot] and answered for the
// proxy's referrers TTL. Past it the upstream is asked again, and when that
// fails the snapshot is answered stale for as long as the bound allows; with
// no snapshot, or one past the bound, the failure is the answer.
func (g *Registry) referrersThrough(ctx context.Context, p *Proxy, name string, subject digest.Digest) (index.Snapshot, bool, error) {
	snap, stale, outcome, err := g.referrersOutcome(ctx, p, name, subject)
	prefix := p.Prefix
	if prefix == "" {
		prefix = "*"
	}
	g.cacheReferrers.Add(ctx, 1, metric.WithAttributes(
		attribute.String("cr.cache.proxy", prefix),
		attribute.String("cr.cache.outcome", outcome),
	))
	return snap, stale, err
}

func (g *Registry) referrersOutcome(ctx context.Context, p *Proxy, name string, subject digest.Digest) (index.Snapshot, bool, string, error) {
	ix := g.c.Index
	now := g.c.Now()
	key := referrersKey(name, subject)

	cur, err := ix.Snapshot().Get(ctx, name, subject)
	have := err == nil
	if err != nil && !errors.Is(err, index.ErrNotFound) {
		return index.Snapshot{}, false, "error", err
	}
	if have && g.proxies.fresh(key, p.referrersTTL(), now) {
		return cur, false, "hit", nil
	}

	next := index.Snapshot{Subject: subject, Supported: true, ObservedAt: now}
	ds, err := p.Upstream.Referrers(ctx, p.Name(name), subject, g.c.MaxManifestSize)
	switch {
	case errors.Is(err, flob.ErrNotExist):
		// No referrers API upstream, which is an answer too: the client is
		// told so and falls back to the tag schema, which a cache serves.
		next.Supported = false
	case err != nil:
		if have && now.Sub(cur.ObservedAt) <= p.referrersMaxStale() {
			log.From(ctx).WarnContext(ctx, "pull-through: upstream unavailable, serving the referrers it last listed",
				slog.String("repo", name), slog.String("subject", subject.String()),
				slog.Time("observed", cur.ObservedAt), slog.String("err", err.Error()))
			return cur, true, "stale", nil
		}
		return index.Snapshot{}, false, "error", err
	default:
		next.Descriptors = make([]index.Descriptor, 0, len(ds))
		for _, v := range ds {
			next.Descriptors = append(next.Descriptors, index.Descriptor{
				MediaType:    v.MediaType,
				ArtifactType: v.ArtifactType,
				Digest:       v.Digest,
				Size:         v.Size,
				Annotations:  v.Annotations,
			})
		}
	}

	err = ix.Tx(index.Waiting(ctx, "referrers fetch"), name, func(ix index.Index) error {
		if _, err := ix.Repo().Ensure(ctx, name); err != nil {
			return err
		}
		return ix.Snapshot().Put(ctx, name, next)
	})
	if err != nil {
		return index.Snapshot{}, false, "error", err
	}
	g.proxies.mark(key, now)
	switch {
	case !have:
		return next, false, "miss", nil
	case sameAnswer(cur, next):
		return next, false, "revalidated", nil
	default:
		return next, false, "refreshed", nil
	}
}

func sameAnswer(a, b index.Snapshot) bool {
	return a.Supported == b.Supported && slices.EqualFunc(a.Descriptors, b.Descriptors, func(x, y index.Descriptor) bool {
		return x.Digest == y.Digest && x.MediaType == y.MediaType && x.ArtifactType == y.ArtifactType &&
			x.Size == y.Size && maps.Equal(x.Annotations, y.Annotations)
	})
}

// fetch copies one manifest from upstream into the cache, and points tag at
// it when tag is not empty. What the manifest holds is recorded without being
// fetched: the blobs come when a client asks for them.
func (g *Registry) fetch(ctx context.Context, p *Proxy, name, remote, reference, tag string) error {
	ctx = index.Waiting(ctx, "cache fetch")
	body, mt, d, err := p.Upstream.GetManifest(ctx, remote, reference, g.c.MaxManifestSize)
	if errors.Is(err, flob.ErrNotExist) {
		return oci.ErrManifestUnknown(reference)
	}
	if err != nil {
		return err
	}
	m, err := oci.ParseManifest(mt, body)
	if err != nil {
		return err
	}

	s := g.store(name)
	if _, err := s.Add(ctx, flob.Meta{Digest: flob.Digest(d)}, bytes.NewReader(body)); err != nil && !errors.Is(err, flob.ErrAlreadyExists) {
		return err
	}
	rec := index.Manifest{
		Digest:       d,
		MediaType:    m.MediaType,
		ArtifactType: m.ArtifactType,
		Size:         int64(len(body)),
		Annotations:  m.Annotations,
		CreatedAt:    g.c.Now(),
	}
	if m.Subject != nil {
		rec.Subject = m.Subject.Digest
	}

	err = g.c.Index.Tx(ctx, name, func(ix index.Index) error {
		if _, err := ix.Repo().Ensure(ctx, name); err != nil {
			return err
		}
		if err := ix.Manifest().Put(ctx, name, rec, m.Holds()); err != nil {
			return err
		}
		if tag == "" {
			return nil
		}
		from := digest.Digest("")
		if cur, err := ix.Tag().Get(ctx, name, tag); err == nil {
			from = cur.Digest
		} else if !errors.Is(err, index.ErrNotFound) {
			return err
		}
		if from == d {
			return nil
		}
		return ix.Tag().Set(ctx, name, tag, d, from)
	})
	if err != nil {
		return err
	}
	log.From(ctx).InfoContext(ctx, "pull-through: fetched",
		slog.String("repo", name), slog.String("upstream", p.Upstream.String()+"/"+remote),
		slog.String("reference", reference), slog.String("digest", d.String()))
	return nil
}
