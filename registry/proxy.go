package registry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx/log"
	"github.com/opencontainers/go-digest"

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

// proxies is the proxies, longest prefix first, and when each tag was last
// checked against its upstream.
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

func (ps *proxies) fresh(repo, tag string, ttl time.Duration, now time.Time) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	at, ok := ps.checked[repo+":"+tag]
	return ok && now.Sub(at) < ttl
}

func (ps *proxies) mark(repo, tag string, now time.Time) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.checked == nil {
		ps.checked = map[string]time.Time{}
	}
	// A bound, not an eviction policy: past it every tag is checked again.
	if len(ps.checked) > 100_000 {
		clear(ps.checked)
	}
	ps.checked[repo+":"+tag] = now
}

// pullThrough makes sure the manifest ref names in a proxied repository is in
// the cache, fetching it when it is not there or its tag is due for a check.
//
// The upstream being down is not the client's problem when the cache can
// answer: a tag already cached is served as it is, and the failure logged.
func (g *Registry) pullThrough(ctx context.Context, p *Proxy, name string, ref oci.Reference) error {
	ix := g.c.Index
	remote := p.Name(name)

	if ref.IsDigest() {
		if _, err := ix.Manifest().Get(ctx, name, ref.Digest); err == nil {
			return nil
		} else if !errors.Is(err, index.ErrNotFound) {
			return err
		}
		return g.fetch(ctx, p, name, remote, ref.Digest.String(), "")
	}

	now := g.c.Now()
	cur, err := ix.Tag().Get(ctx, name, ref.Tag)
	cached := err == nil
	if err != nil && !errors.Is(err, index.ErrNotFound) {
		return err
	}
	if cached && g.proxies.fresh(name, ref.Tag, p.ttl(), now) {
		return nil
	}

	d, err := p.Upstream.HeadManifest(ctx, remote, ref.Tag)
	switch {
	case errors.Is(err, flob.ErrNotExist):
		// Gone upstream. The cache keeps what it has until retention says
		// otherwise; a client asking is told what upstream says.
		return oci.ErrManifestUnknown(ref.Tag)
	case err != nil:
		if cached {
			log.From(ctx).WarnContext(ctx, "pull-through: upstream unavailable, serving the cached tag",
				slog.String("repo", name), slog.String("tag", ref.Tag), slog.String("err", err.Error()))
			return nil
		}
		return err
	}
	if cached && d != "" && d == cur.Digest {
		if _, err := ix.Manifest().Get(ctx, name, d); err == nil {
			g.proxies.mark(name, ref.Tag, now)
			return nil
		}
	}
	if err := g.fetch(ctx, p, name, remote, ref.Tag, ref.Tag); err != nil {
		if cached {
			log.From(ctx).WarnContext(ctx, "pull-through: fetch failed, serving the cached tag",
				slog.String("repo", name), slog.String("tag", ref.Tag), slog.String("err", err.Error()))
			return nil
		}
		return err
	}
	g.proxies.mark(name, ref.Tag, now)
	return nil
}

// fetch copies one manifest from upstream into the cache, and points tag at
// it when tag is not empty. What the manifest holds is recorded without being
// fetched: the blobs come when a client asks for them.
func (g *Registry) fetch(ctx context.Context, p *Proxy, name, remote, reference, tag string) error {
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
		if err := ix.Tag().Set(ctx, name, tag, d, from); err != nil {
			return err
		}
		if from != "" {
			g.label(ctx, s, from, tag, false)
		}
		g.label(ctx, s, d, tag, true)
		return nil
	})
	if err != nil {
		return err
	}
	log.From(ctx).InfoContext(ctx, "pull-through: fetched",
		slog.String("repo", name), slog.String("upstream", p.Upstream.String()+"/"+remote),
		slog.String("reference", reference), slog.String("digest", d.String()))
	return nil
}
