// Package memindex is the index in memory, for the handler's tests and for
// nothing that has to survive a restart.
package memindex

import (
	"context"
	"iter"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lesomnus/cr/index"
)

// Index keeps everything behind one lock, which a transaction holds for its
// whole length. That is stricter than the per-repository lock the port asks
// for, and indistinguishable from it in a test.
type Index struct {
	sem chan struct{}
	s   *state

	// Wait bounds how long a [index.Index.Tx] waits for the lock before it is
	// [index.ErrBusy]; zero waits for as long as the context allows.
	Wait time.Duration

	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

var _ index.Index = (*Index)(nil)

func New() *Index {
	return &Index{sem: make(chan struct{}, 1), s: newState()}
}

type entry struct {
	m     index.Manifest
	holds []digest.Digest
}

type state struct {
	repos     map[string]index.Repo
	manifests map[string]map[digest.Digest]entry
	tags      map[string]map[string]index.Tag
}

func newState() *state {
	return &state{
		repos:     map[string]index.Repo{},
		manifests: map[string]map[digest.Digest]entry{},
		tags:      map[string]map[string]index.Tag{},
	}
}

func (s *state) clone() *state {
	c := newState()
	maps.Copy(c.repos, s.repos)
	for k, v := range s.manifests {
		c.manifests[k] = maps.Clone(v)
	}
	for k, v := range s.tags {
		c.tags[k] = maps.Clone(v)
	}
	return c
}

func (ix *Index) now() time.Time {
	if ix.Now != nil {
		return ix.Now()
	}
	return time.Now()
}

func (ix *Index) lock(ctx context.Context) error {
	if ix.Wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ix.Wait)
		defer cancel()
	}
	select {
	case ix.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		if ix.Wait > 0 && ctx.Err() == context.DeadlineExceeded {
			return index.ErrBusy
		}
		return ctx.Err()
	}
}

func (ix *Index) unlock() { <-ix.sem }

// view is the Index as a transaction sees it when s is set, and as everyone
// else does when it is not.
type view struct {
	ix *Index
	s  *state
}

func (v view) do(ctx context.Context, f func(s *state) error) error {
	if v.s != nil {
		return f(v.s)
	}
	if err := v.ix.lock(ctx); err != nil {
		return err
	}
	defer v.ix.unlock()
	return f(v.ix.s)
}

func (ix *Index) Repo() index.Repos         { return repos(view{ix: ix}) }
func (ix *Index) Manifest() index.Manifests { return manifests(view{ix: ix}) }
func (ix *Index) Tag() index.Tags           { return tags(view{ix: ix}) }
func (ix *Index) Pulled() index.Pulled      { return pulled(view{ix: ix}) }

func (ix *Index) Tx(ctx context.Context, repo string, fn func(index.Index) error) error {
	return view{ix: ix}.Tx(ctx, repo, fn)
}

func (v view) Repo() index.Repos         { return repos(v) }
func (v view) Manifest() index.Manifests { return manifests(v) }
func (v view) Tag() index.Tags           { return tags(v) }
func (v view) Pulled() index.Pulled      { return pulled(v) }

func (v view) Tx(ctx context.Context, repo string, fn func(index.Index) error) error {
	if v.s != nil {
		return fn(v)
	}
	if err := v.ix.lock(ctx); err != nil {
		return err
	}
	defer v.ix.unlock()

	c := v.ix.s.clone()
	if err := fn(view{ix: v.ix, s: c}); err != nil {
		return err
	}
	v.ix.s = c
	return nil
}

func page[T any](vs []T, key func(T) string, p index.Page) []T {
	i, _ := slices.BinarySearchFunc(vs, p.Last, func(v T, last string) int {
		return strings.Compare(key(v), last)
	})
	for i < len(vs) && p.Last != "" && key(vs[i]) <= p.Last {
		i++
	}
	vs = vs[i:]
	if p.N > 0 && len(vs) > p.N {
		vs = vs[:p.N]
	}
	return slices.Clone(vs)
}

type repos view

func (r repos) Ensure(ctx context.Context, name string) (index.Repo, error) {
	var out index.Repo
	err := view(r).do(ctx, func(s *state) error {
		v, ok := s.repos[name]
		if !ok {
			now := r.ix.now()
			v = index.Repo{Name: name, CreatedAt: now, UpdatedAt: now}
			s.repos[name] = v
		}
		out = v
		return nil
	})
	return out, err
}

func (r repos) Get(ctx context.Context, name string) (index.Repo, error) {
	var out index.Repo
	err := view(r).do(ctx, func(s *state) error {
		v, ok := s.repos[name]
		if !ok {
			return index.ErrNotFound
		}
		out = v
		return nil
	})
	return out, err
}

func (r repos) Update(ctx context.Context, name string, patch index.RepoPatch) (index.Repo, error) {
	var out index.Repo
	err := view(r).do(ctx, func(s *state) error {
		v, ok := s.repos[name]
		if !ok {
			return index.ErrNotFound
		}
		if patch.Description != nil {
			v.Description = *patch.Description
		}
		v.UpdatedAt = r.ix.now()
		s.repos[name] = v
		out = v
		return nil
	})
	return out, err
}

func (r repos) sorted(s *state) []index.Repo {
	vs := slices.Collect(maps.Values(s.repos))
	slices.SortFunc(vs, func(a, b index.Repo) int { return strings.Compare(a.Name, b.Name) })
	return vs
}

func (r repos) List(ctx context.Context, p index.Page) ([]string, error) {
	var out []string
	err := view(r).do(ctx, func(s *state) error {
		for _, v := range page(r.sorted(s), func(v index.Repo) string { return v.Name }, p) {
			out = append(out, v.Name)
		}
		return nil
	})
	return out, err
}

func (r repos) Search(ctx context.Context, q string, p index.Page) ([]index.Repo, error) {
	var out []index.Repo
	q = strings.ToLower(q)
	err := view(r).do(ctx, func(s *state) error {
		vs := slices.DeleteFunc(r.sorted(s), func(v index.Repo) bool {
			return !strings.Contains(strings.ToLower(v.Name), q) && !strings.Contains(strings.ToLower(v.Description), q)
		})
		out = page(vs, func(v index.Repo) string { return v.Name }, p)
		return nil
	})
	return out, err
}

func (r repos) Erase(ctx context.Context, name string) error {
	return view(r).do(ctx, func(s *state) error {
		delete(s.repos, name)
		delete(s.manifests, name)
		delete(s.tags, name)
		return nil
	})
}

type manifests view

func (r manifests) Put(ctx context.Context, repo string, m index.Manifest, holds []digest.Digest) error {
	return view(r).do(ctx, func(s *state) error {
		vs := s.manifests[repo]
		if vs == nil {
			vs = map[digest.Digest]entry{}
			s.manifests[repo] = vs
		}
		if _, ok := vs[m.Digest]; ok {
			return nil
		}
		if m.CreatedAt.IsZero() {
			m.CreatedAt = r.ix.now()
		}
		m.Annotations = maps.Clone(m.Annotations)
		vs[m.Digest] = entry{m: m, holds: slices.Clone(holds)}
		return nil
	})
}

func (r manifests) Get(ctx context.Context, repo string, d digest.Digest) (index.Manifest, error) {
	var out index.Manifest
	err := view(r).do(ctx, func(s *state) error {
		e, ok := s.manifests[repo][d]
		if !ok {
			return index.ErrNotFound
		}
		out = e.m
		return nil
	})
	return out, err
}

func holds(s *state, repo string, d digest.Digest) bool {
	for _, e := range s.manifests[repo] {
		if slices.Contains(e.holds, d) {
			return true
		}
	}
	return false
}

func (r manifests) Erase(ctx context.Context, repo string, d digest.Digest) ([]digest.Digest, error) {
	var released []digest.Digest
	err := view(r).do(ctx, func(s *state) error {
		e, ok := s.manifests[repo][d]
		if !ok {
			return index.ErrNotFound
		}
		delete(s.manifests[repo], d)
		for _, b := range e.holds {
			if holds(s, repo, b) {
				continue
			}
			if _, ok := s.manifests[repo][b]; ok {
				continue
			}
			released = append(released, b)
		}
		return nil
	})
	return released, err
}

func (r manifests) Referrers(ctx context.Context, repo string, subject digest.Digest, artifactType string) ([]index.Manifest, error) {
	var out []index.Manifest
	err := view(r).do(ctx, func(s *state) error {
		for _, e := range s.manifests[repo] {
			if e.m.Subject != subject {
				continue
			}
			if artifactType != "" && e.m.ArtifactType != artifactType {
				continue
			}
			out = append(out, e.m)
		}
		return nil
	})
	slices.SortFunc(out, func(a, b index.Manifest) int { return strings.Compare(a.Digest.String(), b.Digest.String()) })
	return out, err
}

func (r manifests) Holds(ctx context.Context, repo string, d digest.Digest) (bool, error) {
	var out bool
	err := view(r).do(ctx, func(s *state) error {
		out = holds(s, repo, d)
		return nil
	})
	return out, err
}

func (r manifests) List(ctx context.Context, repo string, p index.Page) ([]index.Manifest, error) {
	var out []index.Manifest
	err := view(r).do(ctx, func(s *state) error {
		vs := []index.Manifest{}
		for _, e := range s.manifests[repo] {
			vs = append(vs, e.m)
		}
		slices.SortFunc(vs, func(a, b index.Manifest) int { return strings.Compare(a.Digest.String(), b.Digest.String()) })
		out = page(vs, func(v index.Manifest) string { return v.Digest.String() }, p)
		return nil
	})
	return out, err
}

func (r manifests) Marks(ctx context.Context, repo string) iter.Seq2[digest.Digest, error] {
	return func(yield func(digest.Digest, error) bool) {
		seen := map[digest.Digest]struct{}{}
		err := view(r).do(ctx, func(s *state) error {
			for d, e := range s.manifests[repo] {
				seen[d] = struct{}{}
				for _, b := range e.holds {
					seen[b] = struct{}{}
				}
			}
			return nil
		})
		if err != nil {
			yield("", err)
			return
		}
		for d := range seen {
			if !yield(d, nil) {
				return
			}
		}
	}
}

type tags view

func (r tags) Set(ctx context.Context, repo, name string, d, from digest.Digest) error {
	return view(r).do(ctx, func(s *state) error {
		vs := s.tags[repo]
		if vs == nil {
			vs = map[string]index.Tag{}
			s.tags[repo] = vs
		}
		now := r.ix.now()
		v, ok := vs[name]
		if ok != (from != "") || (ok && v.Digest != from) {
			return index.ErrTagMoved
		}
		if !ok {
			v = index.Tag{Name: name, CreatedAt: now}
		}
		if v.Digest != d {
			v.MovedAt = now
		}
		v.Digest = d
		vs[name] = v
		return nil
	})
}

func (r tags) Get(ctx context.Context, repo, name string) (index.Tag, error) {
	var out index.Tag
	err := view(r).do(ctx, func(s *state) error {
		v, ok := s.tags[repo][name]
		if !ok {
			return index.ErrNotFound
		}
		out = v
		return nil
	})
	return out, err
}

func (r tags) Erase(ctx context.Context, repo, name string) error {
	return view(r).do(ctx, func(s *state) error {
		if _, ok := s.tags[repo][name]; !ok {
			return index.ErrNotFound
		}
		delete(s.tags[repo], name)
		return nil
	})
}

func (r tags) sorted(s *state, repo string) []index.Tag {
	vs := slices.Collect(maps.Values(s.tags[repo]))
	slices.SortFunc(vs, func(a, b index.Tag) int { return strings.Compare(a.Name, b.Name) })
	return vs
}

func (r tags) List(ctx context.Context, repo string, p index.Page) ([]string, error) {
	var out []string
	err := view(r).do(ctx, func(s *state) error {
		for _, v := range page(r.sorted(s, repo), func(v index.Tag) string { return v.Name }, p) {
			out = append(out, v.Name)
		}
		return nil
	})
	return out, err
}

func (r tags) Of(ctx context.Context, repo string, d digest.Digest) ([]index.Tag, error) {
	var out []index.Tag
	err := view(r).do(ctx, func(s *state) error {
		for _, v := range r.sorted(s, repo) {
			if v.Digest == d {
				out = append(out, v)
			}
		}
		return nil
	})
	return out, err
}

func (r tags) All(ctx context.Context, repo string) ([]index.Tag, error) {
	var out []index.Tag
	err := view(r).do(ctx, func(s *state) error {
		out = r.sorted(s, repo)
		return nil
	})
	return out, err
}

type pulled view

func (r pulled) Touch(repo, tag string, d digest.Digest, at time.Time) {
	view(r).do(context.Background(), func(s *state) error {
		if e, ok := s.manifests[repo][d]; ok && at.After(e.m.PulledAt) {
			e.m.PulledAt = at
			s.manifests[repo][d] = e
		}
		if tag == "" {
			return nil
		}
		if v, ok := s.tags[repo][tag]; ok && at.After(v.PulledAt) {
			v.PulledAt = at
			s.tags[repo][tag] = v
		}
		return nil
	})
}
