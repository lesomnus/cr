// Package rebuild recreates the index from the store alone: the repositories
// that have manifests in their namespaces, the manifests, and what they hold.
// Tags are the index's alone, so a rebuild cannot bring them back.
package rebuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"

	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/oci"
)

// Report is what a rebuild found and wrote.
type Report struct {
	Repositories int
	Manifests    int

	// Skipped is namespaces that are not repository names.
	Skipped []string
}

// Rebuild reads every namespace of stores into ix. It only adds: a manifest
// already indexed is left as it is, so it can run over a partial index as well
// as an empty one.
//
// A manifest is a blob of at most maxManifest bytes that parses as one; the
// time it was pushed is not in the store and becomes now.
func Rebuild(ctx context.Context, stores flob.Stores, ix index.Index, maxManifest int64) (Report, error) {
	var r Report
	if maxManifest <= 0 {
		maxManifest = 4 << 20
	}
	n, ok := flob.AsNamespacer(stores)
	if !ok {
		return r, errors.New("rebuild: the store cannot list its namespaces")
	}

	var errs []error
	for ns, err := range n.Namespaces(ctx) {
		if err != nil {
			return r, errors.Join(append(errs, err)...)
		}
		if !oci.ValidName(ns) {
			r.Skipped = append(r.Skipped, ns)
			continue
		}
		if err := r.repository(ctx, stores.Use(ns), ns, ix, maxManifest); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ns, err))
		}
	}
	return r, errors.Join(errs...)
}

type found struct {
	m     index.Manifest
	holds []digest.Digest
}

func (r *Report) repository(ctx context.Context, s flob.Store, repo string, ix index.Index, maxManifest int64) error {
	w, ok := flob.AsWalker(s)
	if !ok {
		return errors.New("the store cannot be walked")
	}

	var fs []found
	for info, err := range w.Walk(ctx) {
		if err != nil {
			return err
		}
		if info.Size() < 2 || info.Size() > maxManifest {
			continue
		}
		f, ok, err := read(ctx, s, info)
		if err != nil {
			return err
		}
		if ok {
			fs = append(fs, f)
		}
	}
	if len(fs) == 0 {
		return nil
	}

	return ix.Tx(ctx, repo, func(tx index.Index) error {
		if _, err := tx.Repo().Ensure(ctx, repo); err != nil {
			return err
		}
		r.Repositories++
		for _, f := range fs {
			if _, err := tx.Manifest().Get(ctx, repo, f.m.Digest); err == nil {
				continue
			}
			if err := tx.Manifest().Put(ctx, repo, f.m, f.holds); err != nil {
				return err
			}
			r.Manifests++
		}
		return nil
	})
}

// read answers the manifest a blob is, if it is one.
func read(ctx context.Context, s flob.Store, info flob.Info) (found, bool, error) {
	rc, _, err := s.Open(ctx, info.Digest())
	if errors.Is(err, flob.ErrNotExist) {
		return found{}, false, nil
	}
	if err != nil {
		return found{}, false, err
	}
	b, err := io.ReadAll(io.LimitReader(rc, info.Size()+1))
	rc.Close()
	if err != nil {
		return found{}, false, err
	}
	if t := bytes.TrimLeft(b, " \t\r\n"); len(t) == 0 || t[0] != '{' {
		return found{}, false, nil
	}
	p, err := oci.ParseManifest("", b)
	if err != nil {
		return found{}, false, nil
	}

	m := index.Manifest{
		Digest:       digest.Digest(info.Digest()),
		MediaType:    p.MediaType,
		ArtifactType: p.ArtifactType,
		Size:         int64(len(b)),
		Annotations:  p.Annotations,
	}
	if p.Subject != nil {
		m.Subject = p.Subject.Digest
	}
	return found{m: m, holds: p.Holds()}, true, nil
}
