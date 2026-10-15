package gc

import (
	"context"
	"errors"
	"time"

	"github.com/lesomnus/cr/index"
	"github.com/opencontainers/go-digest"
)

// orphaned drains work recorded by manifest writes. Only the first run for
// an existing repository scans its manifests unless a rescan was requested;
// the checkpoint persists and each seeding batch releases the repository lock.
func (c *Collector) orphaned(ctx context.Context, repo string) (int, error) {
	if c.c.RescanOrphans {
		if err := c.c.Index.Orphan().ResetScan(ctx, repo); err != nil {
			return 0, err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		done, err := c.c.Index.Orphan().Seed(ctx, repo)
		if err != nil {
			return 0, err
		}
		if done {
			break
		}
	}
	cutoff := c.c.Now().Add(-c.c.Orphaned)
	n := 0
	var errs []error
	last := ""
	for {
		cs, err := c.c.Index.Orphan().List(ctx, repo, index.Page{Last: last, N: 500})
		if err != nil {
			return n, errors.Join(append(errs, err)...)
		}
		for _, candidate := range cs {
			if err := ctx.Err(); err != nil {
				return n, errors.Join(append(errs, err)...)
			}
			refs, err := c.orphanReferrers(ctx, repo, candidate.Subject, cutoff)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, m := range refs {
				removed, err := c.deleteOrphan(ctx, repo, candidate.Subject, m.Digest, cutoff)
				if removed {
					n++
				}
				if err != nil {
					errs = append(errs, err)
				}
			}
			// A protected referrer retains its candidate. Once none remain, forget
			// it under the lock, so a concurrent insertion cannot lose its work.
			if len(refs) > 0 {
				_, err := c.orphanReferrers(ctx, repo, candidate.Subject, cutoff)
				if err != nil {
					errs = append(errs, err)
				}
			}
		}
		if len(cs) < 500 {
			break
		}
		last = cs[len(cs)-1].Subject.String()
	}
	return n, errors.Join(errs...)
}

func (c *Collector) orphanReferrers(ctx context.Context, repo string, subject digest.Digest, cutoff time.Time) ([]index.Manifest, error) {
	var refs []index.Manifest
	err := c.c.Index.Tx(ctx, repo, func(ix index.Index) error {
		candidate, err := ix.Orphan().Get(ctx, repo, subject)
		if errors.Is(err, index.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := ix.Manifest().Get(ctx, repo, subject); err == nil {
			return ix.Orphan().Erase(ctx, repo, subject)
		} else if !errors.Is(err, index.ErrNotFound) {
			return err
		}
		if candidate.ObservedAt.After(cutoff) {
			return nil
		}
		refs, err = ix.Manifest().Referrers(ctx, repo, subject, "")
		if err != nil {
			return err
		}
		if len(refs) == 0 {
			return ix.Orphan().Erase(ctx, repo, subject)
		}
		return nil
	})
	return refs, err
}

func (c *Collector) deleteOrphan(ctx context.Context, repo string, subject, d digest.Digest, cutoff time.Time) (bool, error) {
	return c.removeManifest(ctx, repo, d, func(ix index.Index, m index.Manifest) (bool, error) {
		if m.Subject != subject || !old(m, cutoff) {
			return false, nil
		}
		candidate, err := ix.Orphan().Get(ctx, repo, subject)
		if errors.Is(err, index.ErrNotFound) {
			return false, nil
		}
		if err != nil || candidate.ObservedAt.After(cutoff) {
			return false, err
		}
		// A cache's upstream snapshot cannot override a subject that returned.
		_, err = ix.Manifest().Get(ctx, repo, subject)
		if errors.Is(err, index.ErrNotFound) {
			return true, nil
		}
		return false, err
	})
}
