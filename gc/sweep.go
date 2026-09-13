package gc

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"

	"github.com/lesomnus/cr/index"
)

// SweepReport is what sweeping one repository's store did.
type SweepReport struct {
	Blobs int
	Bytes int64

	// Missing is the manifests the index has and the store does not: what a
	// sweep cannot fix and an operator should look at.
	Missing []digest.Digest
}

// Sweep is the mark-and-sweep of one repository. Holding the repository's lock
// -- the one a manifest push or delete takes, so nothing changes what the
// repository references while it is measured -- it marks every digest the
// index says the repository holds, walks the repository's store, and erases
// what is not marked.
//
// Pulls and blob uploads go on meanwhile, and so does every other repository.
// A blob uploaded here during the walk may be erased before the manifest that
// names it arrives; that push fails with MANIFEST_BLOB_UNKNOWN and uploads
// again. Nothing a manifest already references is erased.
func (c *Collector) Sweep(ctx context.Context, repo string) (SweepReport, error) {
	var r SweepReport
	s := c.c.Stores.Use(repo)
	w, ok := flob.AsWalker(s)
	if !ok {
		return r, fmt.Errorf("%s: the store cannot be walked", repo)
	}

	unlock, err := c.c.Index.Lock(ctx, repo)
	if err != nil {
		return r, err
	}
	defer unlock()

	marks := map[digest.Digest]struct{}{}
	for d, err := range c.c.Index.Manifest().Marks(ctx, repo) {
		if err != nil {
			return r, err
		}
		marks[d] = struct{}{}
	}

	type doomed struct {
		d    flob.Digest
		size int64
	}
	var unmarked []doomed
	present := map[digest.Digest]struct{}{}
	for info, err := range w.Walk(ctx) {
		if err != nil {
			return r, err
		}
		d := digest.Digest(info.Digest())
		present[d] = struct{}{}
		if _, ok := marks[d]; !ok {
			unmarked = append(unmarked, doomed{info.Digest(), info.Size()})
		}
	}
	for _, v := range unmarked {
		if err := s.Erase(ctx, v.d); err != nil {
			return r, err
		}
		r.Blobs++
		r.Bytes += v.size
	}

	last := ""
	for {
		ms, err := c.c.Index.Manifest().List(ctx, repo, index.Page{Last: last, N: 500})
		if err != nil {
			return r, err
		}
		for _, m := range ms {
			if _, ok := present[m.Digest]; !ok {
				r.Missing = append(r.Missing, m.Digest)
			}
		}
		if len(ms) < 500 {
			break
		}
		last = ms[len(ms)-1].Digest.String()
	}
	return r, nil
}

// FullReport is what a full collection did: the online part, and the sweep of
// every repository after it.
type FullReport struct {
	Report
	Repositories int
	Blobs        int
	Bytes        int64

	// Missing is `repo@digest` of every manifest the index has and its store
	// does not.
	Missing []string
}

// Full collects online and then sweeps every repository the store or the index
// knows, one at a time. A repository whose lock was not free within the bound
// is tried once more after the rest, and reported if it is still busy.
func (c *Collector) Full(ctx context.Context) (FullReport, error) {
	var (
		r    FullReport
		errs []error
	)
	online, err := c.Run(ctx)
	r.Report = online
	if err != nil {
		errs = append(errs, err)
	}

	names, err := c.repositories(ctx)
	if err != nil {
		return r, errors.Join(append(errs, err)...)
	}

	sweep := func(name string) error {
		sr, err := c.Sweep(ctx, name)
		if err != nil {
			return err
		}
		r.Repositories++
		r.Blobs += sr.Blobs
		r.Bytes += sr.Bytes
		for _, d := range sr.Missing {
			r.Missing = append(r.Missing, name+"@"+d.String())
		}
		return nil
	}

	var busy []string
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return r, errors.Join(append(errs, err)...)
		}
		if err := sweep(name); err != nil {
			if errors.Is(err, index.ErrBusy) {
				busy = append(busy, name)
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	for _, name := range busy {
		if err := sweep(name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return r, errors.Join(errs...)
}

// repositories is every name the store has a namespace for or the index a
// repository row, in order.
func (c *Collector) repositories(ctx context.Context) ([]string, error) {
	set := map[string]struct{}{}
	if n, ok := flob.AsNamespacer(c.c.Stores); ok {
		for ns, err := range n.Namespaces(ctx) {
			if err != nil {
				return nil, err
			}
			set[ns] = struct{}{}
		}
	}
	last := ""
	for {
		names, err := c.c.Index.Repo().List(ctx, index.Page{Last: last, N: 500})
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			set[name] = struct{}{}
		}
		if len(names) < 500 {
			break
		}
		last = names[len(names)-1]
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}
