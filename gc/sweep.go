package gc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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

// sweepChunk is how many unmarked blobs a sweep looks at again under the
// repository's lock before letting it go, so that a repository with a lot to
// reclaim lets pushes in between.
const sweepChunk = 100

// missingSlack is how much older than the walk a manifest must be for its
// absence from the store to be reported: a manifest pushed as the walk began
// can name blobs the walk had already passed.
const missingSlack = time.Minute

// Sweep is the mark-and-sweep of one repository. It walks the repository's
// store and marks every digest the index says the repository holds, both
// without the lock, and then takes the repository's lock -- the one a manifest
// push or delete takes -- for a chunk of the unmarked at a time, looks at each
// again, and erases what nothing refers to and what has been in the repository
// for longer than [Config.Delay].
//
// Pulls, blob uploads and every other repository go on meanwhile, and a
// manifest push to this repository waits for a chunk, not for the walk. A blob
// a manifest references by the time its chunk is looked at is kept, and one
// the walk did not see is not touched. A blob younger than Delay is left for a
// later sweep, which is what keeps the blobs of a push whose manifest has not
// arrived; a push that takes longer than Delay to put its manifest can find
// them gone, and fails with MANIFEST_BLOB_UNKNOWN and uploads again.
func (c *Collector) Sweep(ctx context.Context, repo string) (SweepReport, error) {
	ctx = index.Waiting(ctx, "sweep")
	var r SweepReport
	s := c.c.Stores.Use(repo)
	w, ok := flob.AsWalker(s)
	if !ok {
		return r, fmt.Errorf("%s: the store cannot be walked", repo)
	}

	started := c.c.Now()
	present := map[digest.Digest]flob.Info{}
	for info, err := range w.Walk(ctx) {
		if err != nil {
			return r, err
		}
		present[digest.Digest(info.Digest())] = info
	}

	marks := map[digest.Digest]struct{}{}
	for d, err := range c.c.Index.Manifest().Marks(ctx, repo) {
		if err != nil {
			return r, err
		}
		marks[d] = struct{}{}
	}

	var unmarked []flob.Info
	for d, info := range present {
		if _, ok := marks[d]; !ok {
			unmarked = append(unmarked, info)
		}
	}
	slices.SortFunc(unmarked, func(a, b flob.Info) int {
		return strings.Compare(string(a.Digest()), string(b.Digest()))
	})

	for chunk := range slices.Chunk(unmarked, sweepChunk) {
		n, bytes, err := c.erase(ctx, s, repo, chunk, started)
		r.Blobs += n
		r.Bytes += bytes
		if err != nil {
			return r, err
		}
	}

	before := started.Add(-missingSlack)
	last := ""
	for {
		ms, err := c.c.Index.Manifest().List(ctx, repo, index.Page{Last: last, N: 500})
		if err != nil {
			return r, err
		}
		for _, m := range ms {
			if _, ok := present[m.Digest]; !ok && m.CreatedAt.Before(before) {
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

// erase looks at a chunk of the unmarked again under the repository's lock
// and erases those still referenced by nothing: held by no manifest, not a
// manifest, and in the repository since before the sweep's delay. What the
// store has to say -- when a blob arrived, and its size for the report -- is
// asked before the lock, so that the lock waits on the index alone.
func (c *Collector) erase(ctx context.Context, s flob.Store, repo string, chunk []flob.Info, started time.Time) (int, int64, error) {
	type doomed struct {
		d    flob.Digest
		size int64
	}
	var ds []doomed
	for _, info := range chunk {
		if leave, err := c.leave(ctx, info, started); err != nil {
			return 0, 0, err
		} else if leave {
			continue
		}
		size, err := info.Size(ctx)
		if errors.Is(err, flob.ErrNotExist) {
			continue // gone since the walk
		}
		if err != nil {
			return 0, 0, err
		}
		ds = append(ds, doomed{info.Digest(), size})
	}
	if len(ds) == 0 {
		return 0, 0, nil
	}

	unlock, err := c.c.Index.Lock(ctx, repo)
	if err != nil {
		return 0, 0, err
	}
	defer unlock()

	n, bytes := 0, int64(0)
	ms := c.c.Index.Manifest()
	for _, v := range ds {
		d := digest.Digest(v.d)
		if held, err := ms.Holds(ctx, repo, d); err != nil {
			return n, bytes, err
		} else if held {
			continue
		}
		if _, err := ms.Get(ctx, repo, d); err == nil {
			continue
		} else if !errors.Is(err, index.ErrNotFound) {
			return n, bytes, err
		}
		if err := s.Erase(ctx, v.d); err != nil {
			return n, bytes, err
		}
		n++
		bytes += v.size
	}
	return n, bytes, nil
}

// leave reports whether the sweep leaves a blob alone: it entered the
// repository within the delay before the sweep started, or has gone since the
// walk. A negative delay asks nothing, and a store that does not know when a
// blob arrived leaves it to be erased.
func (c *Collector) leave(ctx context.Context, info flob.Info, started time.Time) (bool, error) {
	if c.c.Delay < 0 {
		return false, nil
	}
	added, err := info.Added(ctx)
	switch {
	case errors.Is(err, flob.ErrNotExist):
		return true, nil
	case errors.Is(err, errors.ErrUnsupported):
		return false, nil
	case err != nil:
		return false, err
	}
	return !added.Before(started.Add(-c.delay())), nil
}

// delay is [Config.Delay] with its default: an hour.
func (c *Collector) delay() time.Duration {
	if c.c.Delay == 0 {
		return time.Hour
	}
	return c.c.Delay
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
