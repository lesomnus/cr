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
	"sync"
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

	// Delay is how long a blob must have been in a repository before a sweep
	// erases it, which keeps the blobs of a push whose manifest has not
	// arrived; zero is an hour, and a negative duration erases at once.
	Delay time.Duration

	// Every is how often Spin runs; zero is an hour and negative is never.
	Every time.Duration

	// FullEvery is how often a full collection runs on its own; zero never.
	FullEvery time.Duration

	// Leader picks the one replica that runs; nil runs here.
	Leader Leader

	// Runs records every run; nil records nothing.
	Runs Runs

	// Cache answers how long a pull-through cache keeps what nobody pulls,
	// for a repository that is one, and zero otherwise. A cache's manifests
	// go by that rather than by Untagged, tagged or not.
	Cache func(repo string) time.Duration

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

	mu   sync.Mutex
	full *Run
}

func New(c Config) *Collector {
	if c.Now == nil {
		c.Now = time.Now
	}
	return &Collector{c: c}
}

// Runs is where this collector records its runs, or nil.
func (c *Collector) Runs() Runs { return c.c.Runs }

func (c *Collector) Spin(ctx context.Context) error {
	var online, full <-chan time.Time
	if c.c.Every >= 0 {
		every := c.c.Every
		if every == 0 {
			every = time.Hour
		}
		t := time.NewTicker(every)
		defer t.Stop()
		online = t.C
	}
	if c.c.FullEvery > 0 {
		t := time.NewTicker(c.c.FullEvery)
		defer t.Stop()
		full = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-online:
			if _, err := c.Collect(ctx, KindOnline, TriggerSchedule); err != nil && ctx.Err() == nil {
				log.From(ctx).WarnContext(ctx, "gc", slog.String("err", err.Error()))
			}
		case <-full:
			if _, err := c.Trigger(ctx, TriggerSchedule); err != nil && !errors.Is(err, ErrRunning) {
				log.From(ctx).WarnContext(ctx, "gc: full", slog.String("err", err.Error()))
			}
		}
	}
}

// Collect runs a collection of kind now, on the replica that wins the right
// to, recording it when there is somewhere to.
func (c *Collector) Collect(ctx context.Context, kind, trigger string) (Run, error) {
	run := Run{Kind: kind, Trigger: trigger, State: StateRunning, Missing: []string{}, Started: c.c.Now().UTC()}
	if c.c.Runs != nil {
		r, err := c.c.Runs.Start(ctx, kind, trigger)
		if err != nil {
			return run, err
		}
		run = r
	}
	return c.collect(ctx, run)
}

func (c *Collector) collect(ctx context.Context, run Run) (Run, error) {
	var (
		rep FullReport
		err error
	)
	work := func(ctx context.Context) error {
		if run.Kind == KindFull {
			rep, err = c.Full(ctx)
		} else {
			rep.Report, err = c.Run(ctx)
		}
		return nil
	}
	if c.c.Leader == nil {
		work(ctx)
	} else if won, lerr := c.c.Leader.Lead(ctx, "gc/"+run.Kind, work); lerr != nil {
		err = lerr
	} else if !won {
		err = errors.New("another replica is collecting")
	}

	run.finish(rep, err, c.c.Now().UTC())
	if c.c.Runs != nil {
		if ferr := c.c.Runs.Finish(context.WithoutCancel(ctx), run); ferr != nil && err == nil {
			err = ferr
		}
	}
	attrs := []any{
		slog.String("kind", run.Kind), slog.String("trigger", run.Trigger),
		slog.Int("stages", run.Stages), slog.Int("tags", run.Tags), slog.Int("manifests", run.Manifests),
		slog.Int("repositories", run.Repositories), slog.Int("blobs", run.Blobs), slog.Int64("bytes", run.Bytes),
		slog.Int("missing", len(run.Missing)),
	}
	if err != nil {
		log.From(ctx).WarnContext(ctx, "gc", append(attrs, slog.String("err", err.Error()))...)
	} else {
		log.From(ctx).InfoContext(ctx, "gc", attrs...)
	}
	return run, err
}

// Trigger starts a full collection in the background and answers its run. One
// already running in this process is [ErrRunning], with that run.
func (c *Collector) Trigger(ctx context.Context, trigger string) (Run, error) {
	c.mu.Lock()
	if c.full != nil {
		r := *c.full
		c.mu.Unlock()
		return r, ErrRunning
	}
	run := Run{Kind: KindFull, Trigger: trigger, State: StateRunning, Missing: []string{}, Started: c.c.Now().UTC()}
	if c.c.Runs != nil {
		r, err := c.c.Runs.Start(ctx, KindFull, trigger)
		if err != nil {
			c.mu.Unlock()
			return run, err
		}
		run = r
	}
	c.full = &run
	c.mu.Unlock()

	go func() {
		c.collect(context.WithoutCancel(ctx), run)
		c.mu.Lock()
		c.full = nil
		c.mu.Unlock()
	}()
	return run, nil
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
			if keep := c.cache(name); keep > 0 {
				t, m, err := c.evict(ctx, name, keep)
				r.Tags += t
				r.Manifests += m
				if err != nil {
					errs = append(errs, err)
				}
				continue
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
		}
	}
	return n, errors.Join(errs...)
}

func (c *Collector) cache(repo string) time.Duration {
	if c.c.Cache == nil {
		return 0
	}
	return c.c.Cache(repo)
}

func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// evict empties a pull-through cache of what nobody used within keep, by two
// rules that do not look at each other: a tag goes when nobody pulled it by
// name within keep and it did not move, and then a manifest goes when nothing
// needs it and nobody pulled it, by name or by digest, within keep. A manifest
// still pulled by digest outlives its tag, and a tag a client pulls again is
// fetched again.
func (c *Collector) evict(ctx context.Context, repo string, keep time.Duration) (int, int, error) {
	cutoff := c.c.Now().Add(-keep)
	tags, err := c.c.Index.Tag().All(ctx, repo)
	if err != nil {
		return 0, 0, err
	}

	n := 0
	var errs []error
	for _, t := range tags {
		if latest(t.PulledAt, t.MovedAt).After(cutoff) {
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
		}
	}

	m, err := c.untaggedBefore(ctx, repo, cutoff)
	if err != nil {
		errs = append(errs, err)
	}
	return n, m, errors.Join(errs...)
}

// untagged deletes the manifests past the grace period that nothing needs.
func (c *Collector) untagged(ctx context.Context, repo string) (int, error) {
	return c.untaggedBefore(ctx, repo, c.c.Now().Add(-c.c.Untagged))
}

func (c *Collector) untaggedBefore(ctx context.Context, repo string, cutoff time.Time) (int, error) {
	// What the index says nothing needs, read without the lock; each is
	// looked at again under it before it goes.
	var candidates []index.Manifest
	last := ""
	for {
		ms, err := c.c.Index.Manifest().Unneeded(ctx, repo, cutoff, index.Page{Last: last, N: 500})
		if err != nil {
			return 0, err
		}
		candidates = append(candidates, ms...)
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
