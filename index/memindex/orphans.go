package memindex

import (
	"context"
	"github.com/lesomnus/cr/index"
	"github.com/opencontainers/go-digest"
	"slices"
	"strings"
	"time"
)

type orphanScan struct {
	cursor   string
	complete bool
}
type orphans view

func observe(s *state, repo string, subject digest.Digest, at time.Time, refresh bool) {
	if s.orphans[repo] == nil {
		s.orphans[repo] = map[digest.Digest]index.OrphanCandidate{}
	}
	if _, exists := s.orphans[repo][subject]; exists && !refresh {
		return
	}
	s.orphans[repo][subject] = index.OrphanCandidate{Subject: subject, ObservedAt: at}
}

func (r orphans) Seed(ctx context.Context, repo string) (bool, error) {
	done := false
	err := view(r).do(ctx, func(s *state) error {
		scan := s.scans[repo]
		if scan.complete {
			done = true
			return nil
		}
		var keys []string
		for d := range s.manifests[repo] {
			if d.String() > scan.cursor {
				keys = append(keys, d.String())
			}
		}
		slices.Sort(keys)
		done = len(keys) < 500
		if len(keys) > 500 {
			keys = keys[:500]
		}
		for _, key := range keys {
			m := s.manifests[repo][digest.Digest(key)].m
			if m.Subject != "" {
				if _, exists := s.manifests[repo][m.Subject]; !exists {
					observe(s, repo, m.Subject, r.ix.now(), false)
				}
			}
			scan.cursor = key
		}
		scan.complete = done
		s.scans[repo] = scan
		return nil
	})
	return done, err
}

func (r orphans) Get(ctx context.Context, repo string, subject digest.Digest) (index.OrphanCandidate, error) {
	var out index.OrphanCandidate
	err := view(r).do(ctx, func(s *state) error {
		var ok bool
		out, ok = s.orphans[repo][subject]
		if !ok {
			return index.ErrNotFound
		}
		return nil
	})
	return out, err
}

func (r orphans) List(ctx context.Context, repo string, p index.Page) ([]index.OrphanCandidate, error) {
	var out []index.OrphanCandidate
	err := view(r).do(ctx, func(s *state) error {
		var vs []index.OrphanCandidate
		for _, v := range s.orphans[repo] {
			vs = append(vs, v)
		}
		slices.SortFunc(vs, func(a, b index.OrphanCandidate) int { return strings.Compare(a.Subject.String(), b.Subject.String()) })
		out = page(vs, func(v index.OrphanCandidate) string { return v.Subject.String() }, p)
		return nil
	})
	return out, err
}

func (r orphans) Erase(ctx context.Context, repo string, subject digest.Digest) error {
	return view(r).do(ctx, func(s *state) error { delete(s.orphans[repo], subject); return nil })
}
