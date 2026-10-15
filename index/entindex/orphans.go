package entindex

import (
	"context"

	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/internal/ent"
	"github.com/lesomnus/cr/internal/ent/manifest"
	"github.com/lesomnus/cr/internal/ent/orphancandidate"
	"github.com/lesomnus/cr/internal/ent/orphanscan"
	"github.com/lesomnus/cr/server/pd"
	"github.com/lesomnus/payday/pdid"
	"github.com/opencontainers/go-digest"
)

type orphans struct{ ix *Index }

// observe runs under the repository transaction. Seeding does not refresh a
// candidate already maintained by writes; only a new change restarts its grace.
func (r orphans) observe(ctx context.Context, repo string, subject digest.Digest, refresh bool) error {
	c := r.ix.client.OrphanCandidate
	where := orphancandidate.And(orphancandidate.Repo(repo), orphancandidate.Subject(subject.String()))
	now := r.ix.now()
	if refresh {
		n, err := c.Update().Where(where).SetDateObserved(now).Save(ctx)
		if err != nil || n > 0 {
			return err
		}
	} else {
		exists, err := c.Query().Where(where).Exist(ctx)
		if err != nil || exists {
			return err
		}
	}
	return c.Create().SetId(pdid.New(pd.OrphanCandidateDomain).Uuid()).SetRepo(repo).SetSubject(subject.String()).SetDateObserved(now).Exec(ctx)
}

func (r orphans) Seed(ctx context.Context, repo string) (bool, error) {
	// Completed repositories need one indexed read and no write lock.
	complete, err := r.ix.client.OrphanScan.Query().Where(orphanscan.Repo(repo), orphanscan.Complete(true)).Exist(ctx)
	if err != nil || complete {
		return complete, err
	}
	done := false
	err = r.ix.Tx(ctx, repo, func(ix index.Index) error {
		tx := ix.(*Index)
		scans := tx.client.OrphanScan
		scan, err := scans.Query().Where(orphanscan.Repo(repo)).Only(ctx)
		if err != nil && !ent.IsNotFound(err) {
			return err
		}
		if scan != nil && scan.Complete {
			done = true
			return nil
		}
		last := ""
		if scan != nil {
			last = scan.Cursor
		}
		// Project only relationship columns; seeding never reads manifest bytes.
		var rows []struct {
			Digest  string
			Subject string
		}
		err = tx.client.Manifest.Query().Where(manifest.Repo(repo), manifest.DigestGT(last)).Order(manifest.ByDigest()).Limit(chunk).Select(manifest.FieldDigest, manifest.FieldSubject).Scan(ctx, &rows)
		if err != nil {
			return err
		}
		for _, m := range rows {
			last = m.Digest
			if m.Subject == "" {
				continue
			}
			exists, err := tx.client.Manifest.Query().Where(manifest.Repo(repo), manifest.Digest(m.Subject)).Exist(ctx)
			if err != nil {
				return err
			}
			if !exists {
				if err := (orphans{tx}).observe(ctx, repo, digest.Digest(m.Subject), false); err != nil {
					return err
				}
			}
		}
		done = len(rows) < chunk
		if scan == nil {
			return scans.Create().SetId(pdid.New(pd.OrphanScanDomain).Uuid()).SetRepo(repo).SetCursor(last).SetComplete(done).Exec(ctx)
		}
		return scans.UpdateOneId(scan.Id).SetCursor(last).SetComplete(done).Exec(ctx)
	})
	return done, err
}

func (r orphans) Get(ctx context.Context, repo string, subject digest.Digest) (index.OrphanCandidate, error) {
	v, err := r.ix.client.OrphanCandidate.Query().Where(orphancandidate.Repo(repo), orphancandidate.Subject(subject.String())).Only(ctx)
	if err != nil {
		return index.OrphanCandidate{}, notFound(err)
	}
	return index.OrphanCandidate{Subject: digest.Digest(v.Subject), ObservedAt: v.DateObserved}, nil
}

func (r orphans) List(ctx context.Context, repo string, p index.Page) ([]index.OrphanCandidate, error) {
	q := r.ix.client.OrphanCandidate.Query().Where(orphancandidate.Repo(repo), orphancandidate.SubjectGT(p.Last)).Order(orphancandidate.BySubject())
	if p.N > 0 {
		q = q.Limit(p.N)
	}
	vs, err := q.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]index.OrphanCandidate, 0, len(vs))
	for _, v := range vs {
		out = append(out, index.OrphanCandidate{Subject: digest.Digest(v.Subject), ObservedAt: v.DateObserved})
	}
	return out, nil
}

func (r orphans) Erase(ctx context.Context, repo string, subject digest.Digest) error {
	_, err := r.ix.client.OrphanCandidate.Delete().Where(orphancandidate.Repo(repo), orphancandidate.Subject(subject.String())).Exec(ctx)
	return err
}
