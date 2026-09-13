// Package gc reclaims what nothing refers to.
//
// This is the tier that runs on its own and never loses data: expired uploads,
// tags past a retention rule, and manifests nothing tags, holds, refers to or
// pulled within a grace period. Every step removes a reference the index no
// longer has, so a race with a push leaks a blob to the sweep and never takes
// away one something still points at.
package gc

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx/log"
	"github.com/opencontainers/go-digest"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/index"
)

// Leader decides which of several replicas runs the work called name.
type Leader interface {
	Lead(ctx context.Context, name string, fn func(context.Context) error) (bool, error)
}

type Config struct {
	Stores flob.Stores
	Index  index.Index

	// Policy answers the tag rules in force; nil is none.
	Policy func() *auth.Policy

	// Untagged is how old a manifest nothing tags, holds, refers to or pulled
	// must be before it goes; zero keeps them.
	Untagged time.Duration

	// Every is how often Spin runs; zero is an hour and negative is never.
	Every time.Duration

	// Leader picks the one replica that runs; nil runs here.
	Leader Leader

	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Report is what one run did.
type Report struct {
	Stages    int
	Tags      int
	Manifests int
}

type Collector struct {
	c Config
}

func New(c Config) *Collector {
	if c.Now == nil {
		c.Now = time.Now
	}
	return &Collector{c: c}
}

func (c *Collector) Spin(ctx context.Context) error {
	if c.c.Every < 0 {
		<-ctx.Done()
		return nil
	}
	every := c.c.Every
	if every == 0 {
		every = time.Hour
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			c.tick(ctx)
		}
	}
}

func (c *Collector) tick(ctx context.Context) {
	l := log.From(ctx)
	run := func(ctx context.Context) error {
		r, err := c.Run(ctx)
		attrs := []any{slog.Int("stages", r.Stages), slog.Int("tags", r.Tags), slog.Int("manifests", r.Manifests)}
		if err != nil {
			l.WarnContext(ctx, "gc", append(attrs, slog.String("err", err.Error()))...)
		} else {
			l.InfoContext(ctx, "gc", attrs...)
		}
		return nil
	}
	if c.c.Leader == nil {
		run(ctx)
		return
	}
	if _, err := c.c.Leader.Lead(ctx, "gc", run); err != nil {
		l.WarnContext(ctx, "gc: leader", slog.String("err", err.Error()))
	}
}

// Run collects once, every repository in turn. A failure in one repository
// is reported and the next is collected anyway.
func (c *Collector) Run(ctx context.Context) (Report, error) {
	var (
		r    Report
		errs []error
	)
	if sc, ok := flob.AsStageCleaner(c.c.Stores); ok {
		n, err := sc.PruneStages(ctx)
		r.Stages = n
		if err != nil {
			errs = append(errs, err)
		}
	}

	var p *auth.Policy
	if c.c.Policy != nil {
		p = c.c.Policy()
	}

	last := ""
	for {
		names, err := c.c.Index.Repo().List(ctx, index.Page{Last: last, N: 100})
		if err != nil {
			return r, errors.Join(append(errs, err)...)
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return r, errors.Join(append(errs, err)...)
			}
			n, err := c.retention(ctx, name, p)
			r.Tags += n
			if err != nil {
				errs = append(errs, err)
			}
			if c.c.Untagged > 0 {
				n, err := c.untagged(ctx, name)
				r.Manifests += n
				if err != nil {
					errs = append(errs, err)
				}
			}
		}
		if len(names) < 100 {
			break
		}
		last = names[len(names)-1]
	}
	return r, errors.Join(errs...)
}

// retention deletes the tags that every retention rule matching them puts
// past its count. A tag a rule keeps is kept, and so is one another rule
// makes immutable.
func (c *Collector) retention(ctx context.Context, repo string, p *auth.Policy) (int, error) {
	rules := p.Retention(repo)
	if len(rules) == 0 {
		return 0, nil
	}
	tags, err := c.c.Index.Tag().All(ctx, repo)
	if err != nil {
		return 0, err
	}

	matched := map[string]bool{}
	kept := map[string]bool{}
	for _, rule := range rules {
		var ts []index.Tag
		for _, t := range tags {
			if auth.Glob(rule.Tag, t.Name) {
				ts = append(ts, t)
			}
		}
		slices.SortFunc(ts, func(a, b index.Tag) int {
			if n := b.MovedAt.Compare(a.MovedAt); n != 0 {
				return n
			}
			return strings.Compare(a.Name, b.Name)
		})
		for i, t := range ts {
			matched[t.Name] = true
			if i < rule.Keep {
				kept[t.Name] = true
			}
		}
	}

	n := 0
	var errs []error
	for _, t := range tags {
		if !matched[t.Name] || kept[t.Name] {
			continue
		}
		if err := p.CheckTag(auth.Subject{ID: "gc"}, []auth.Action{auth.ActionAdmin}, repo, t.Name, auth.TagDelete); err != nil {
			continue
		}
		erased := false
		err := c.c.Index.Tx(ctx, repo, func(ix index.Index) error {
			cur, err := ix.Tag().Get(ctx, repo, t.Name)
			if errors.Is(err, index.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			// Moved since it was counted: counted again next time.
			if cur.Digest != t.Digest || !cur.MovedAt.Equal(t.MovedAt) {
				return nil
			}
			if err := ix.Tag().Erase(ctx, repo, t.Name); err != nil {
				return err
			}
			erased = true
			return nil
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if erased {
			n++
			LabelTag(ctx, c.c.Stores.Use(repo), t.Digest, t.Name, false)
		}
	}
	return n, errors.Join(errs...)
}

// untagged deletes the manifests past the grace period that nothing needs.
func (c *Collector) untagged(ctx context.Context, repo string) (int, error) {
	cutoff := c.c.Now().Add(-c.c.Untagged)
	var candidates []index.Manifest
	last := ""
	for {
		ms, err := c.c.Index.Manifest().List(ctx, repo, index.Page{Last: last, N: 500})
		if err != nil {
			return 0, err
		}
		for _, m := range ms {
			if old(m, cutoff) {
				candidates = append(candidates, m)
			}
		}
		if len(ms) < 500 {
			break
		}
		last = ms[len(ms)-1].Digest.String()
	}

	n := 0
	var errs []error
	for _, m := range candidates {
		ok, err := c.deleteManifest(ctx, repo, m.Digest, cutoff)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			n++
		}
	}
	return n, errors.Join(errs...)
}

func old(m index.Manifest, cutoff time.Time) bool {
	return m.CreatedAt.Before(cutoff) && (m.PulledAt.IsZero() || m.PulledAt.Before(cutoff))
}

// deleteManifest deletes d if, looked at again under the repository's lock,
// it is still old and still needed by nothing: no tag points at it, no
// manifest holds it, and it is not a referrer of a manifest that is here.
func (c *Collector) deleteManifest(ctx context.Context, repo string, d digest.Digest, cutoff time.Time) (bool, error) {
	var released []digest.Digest
	deleted := false
	err := c.c.Index.Tx(ctx, repo, func(ix index.Index) error {
		m, err := ix.Manifest().Get(ctx, repo, d)
		if errors.Is(err, index.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !old(m, cutoff) {
			return nil
		}
		if ts, err := ix.Tag().Of(ctx, repo, d); err != nil || len(ts) > 0 {
			return err
		}
		if held, err := ix.Manifest().Holds(ctx, repo, d); err != nil || held {
			return err
		}
		if m.Subject != "" {
			if _, err := ix.Manifest().Get(ctx, repo, m.Subject); err == nil {
				return nil
			} else if !errors.Is(err, index.ErrNotFound) {
				return err
			}
		}
		released, err = ix.Manifest().Erase(ctx, repo, d)
		if err != nil {
			return err
		}
		deleted = true
		return nil
	})
	if err != nil || !deleted {
		return false, err
	}
	return true, Release(ctx, c.c.Index, c.c.Stores.Use(repo), repo, append(released, d))
}

// Release erases from s what repo's index no longer refers to among ds. It
// takes the repository's lock and looks at each again under it, so a digest a
// manifest pushed since holds, or that is itself a manifest again, is kept.
// A failure part way is a leak for the sweep.
func Release(ctx context.Context, ix index.Index, s flob.Store, repo string, ds []digest.Digest) error {
	return ix.Tx(ctx, repo, func(tx index.Index) error {
		for _, d := range ds {
			held, err := tx.Manifest().Holds(ctx, repo, d)
			if err != nil {
				return err
			}
			if held {
				continue
			}
			if _, err := tx.Manifest().Get(ctx, repo, d); err == nil {
				continue
			} else if !errors.Is(err, index.ErrNotFound) {
				return err
			}
			if err := s.Erase(ctx, flob.Digest(d)); err != nil {
				return err
			}
		}
		return nil
	})
}

// TagLabel is the label a manifest in the store carries once for each tag
// pointing at it, which is how a rebuild finds tags without the index.
const TagLabel = "Tag"

// LabelTag adds or removes one tag in d's labels. It is best effort: the index
// is what answers, and a label that did not land costs a rebuild one tag.
func LabelTag(ctx context.Context, s flob.Store, d digest.Digest, tag string, add bool) error {
	info, err := s.Stat(ctx, flob.Digest(d))
	if err != nil {
		if errors.Is(err, flob.ErrNotExist) {
			return nil
		}
		return err
	}
	ls, err := info.Labels(ctx)
	if err != nil {
		return err
	}
	next := ls.Clone()
	if next == nil {
		next = flob.Labels{}
	}
	vs := slices.DeleteFunc(slices.Clone(next.Values(TagLabel)), func(v string) bool { return v == tag })
	if add {
		vs = append(vs, tag)
	}
	if len(vs) == 0 {
		next.Del(TagLabel)
	} else {
		next[TagLabel] = vs
	}
	if err := s.Label(ctx, flob.Digest(d), next); err != nil && !errors.Is(err, flob.ErrNotExist) {
		return err
	}
	return nil
}
