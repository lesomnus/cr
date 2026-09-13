package entindex

import (
	"context"
	"iter"
	"slices"
	"time"

	"github.com/lesomnus/payday/pdid"
	"github.com/opencontainers/go-digest"

	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/internal/ent"
	"github.com/lesomnus/cr/internal/ent/manifest"
	"github.com/lesomnus/cr/internal/ent/manifestblob"
	"github.com/lesomnus/cr/internal/ent/repository"
	"github.com/lesomnus/cr/internal/ent/tag"
	"github.com/lesomnus/cr/server/pd"
)

type digestLike = digest.Digest

// chunk is how many values one IN list or one bulk insert carries, which
// keeps every statement under SQLite's and PostgreSQL's parameter limits.
const chunk = 500

func notFound(err error) error {
	if ent.IsNotFound(err) {
		return index.ErrNotFound
	}
	return err
}

func toRepo(v *ent.Repository) index.Repo {
	return index.Repo{Name: v.Name, Description: v.Desc, CreatedAt: v.DateCreated, UpdatedAt: v.DateUpdated}
}

type repos struct{ ix *Index }

func (r repos) c() *ent.RepositoryClient { return r.ix.client.Repository }

func (r repos) Ensure(ctx context.Context, name string) (index.Repo, error) {
	v, err := r.c().Query().Where(repository.Name(name)).Only(ctx)
	if err == nil {
		return toRepo(v), nil
	}
	if !ent.IsNotFound(err) {
		return index.Repo{}, err
	}

	now := r.ix.now()
	v, err = r.c().Create().
		SetId(pdid.New(pd.RepositoryDomain).Uuid()).
		SetName(name).
		SetDesc("").
		SetDateCreated(now).
		SetDateUpdated(now).
		Save(ctx)
	if err != nil {
		return index.Repo{}, err
	}
	return toRepo(v), nil
}

func (r repos) Get(ctx context.Context, name string) (index.Repo, error) {
	v, err := r.c().Query().Where(repository.Name(name)).Only(ctx)
	if err != nil {
		return index.Repo{}, notFound(err)
	}
	return toRepo(v), nil
}

func (r repos) Update(ctx context.Context, name string, patch index.RepoPatch) (index.Repo, error) {
	u := r.c().Update().Where(repository.Name(name)).SetDateUpdated(r.ix.now())
	if patch.Description != nil {
		u = u.SetDesc(*patch.Description)
	}
	n, err := u.Save(ctx)
	if err != nil {
		return index.Repo{}, err
	}
	if n == 0 {
		return index.Repo{}, index.ErrNotFound
	}
	return r.Get(ctx, name)
}

func (r repos) List(ctx context.Context, p index.Page) ([]string, error) {
	q := r.c().Query().Where(repository.NameGT(p.Last)).Order(repository.ByName())
	if p.N > 0 {
		q = q.Limit(p.N)
	}
	return q.Select(repository.FieldName).Strings(ctx)
}

func (r repos) Search(ctx context.Context, s string, p index.Page) ([]index.Repo, error) {
	q := r.c().Query().
		Where(
			repository.Or(repository.NameContainsFold(s), repository.DescContainsFold(s)),
			repository.NameGT(p.Last),
		).
		Order(repository.ByName())
	if p.N > 0 {
		q = q.Limit(p.N)
	}
	vs, err := q.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]index.Repo, 0, len(vs))
	for _, v := range vs {
		out = append(out, toRepo(v))
	}
	return out, nil
}

func (r repos) Erase(ctx context.Context, name string) error {
	c := r.ix.client
	if _, err := c.Tag.Delete().Where(tag.Repo(name)).Exec(ctx); err != nil {
		return err
	}
	if _, err := c.ManifestBlob.Delete().Where(manifestblob.Repo(name)).Exec(ctx); err != nil {
		return err
	}
	if _, err := c.Manifest.Delete().Where(manifest.Repo(name)).Exec(ctx); err != nil {
		return err
	}
	_, err := c.Repository.Delete().Where(repository.Name(name)).Exec(ctx)
	return err
}

func toManifest(v *ent.Manifest) index.Manifest {
	m := index.Manifest{
		Digest:       digest.Digest(v.Digest),
		MediaType:    v.MediaType,
		ArtifactType: v.ArtifactType,
		Subject:      digest.Digest(v.Subject),
		Size:         v.Size,
		Annotations:  v.Annotations,
		CreatedAt:    v.DateCreated,
	}
	if v.DatePulled != nil {
		m.PulledAt = *v.DatePulled
	}
	return m
}

type manifests struct{ ix *Index }

func (r manifests) Put(ctx context.Context, repo string, m index.Manifest, holds []digest.Digest) error {
	c := r.ix.client
	exists, err := c.Manifest.Query().Where(manifest.Repo(repo), manifest.Digest(m.Digest.String())).Exist(ctx)
	if err != nil || exists {
		return err
	}

	now := r.ix.now()
	created := m.CreatedAt.UTC()
	if m.CreatedAt.IsZero() {
		created = now
	}
	annotations := m.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}
	if _, err := c.Manifest.Create().
		SetId(pdid.New(pd.ManifestDomain).Uuid()).
		SetRepo(repo).
		SetDigest(m.Digest.String()).
		SetMediaType(m.MediaType).
		SetArtifactType(m.ArtifactType).
		SetSubject(m.Subject.String()).
		SetSize(m.Size).
		SetAnnotations(annotations).
		SetDateCreated(created).
		SetDateUpdated(now).
		Save(ctx); err != nil {
		return err
	}

	seen := map[digest.Digest]struct{}{}
	builders := make([]*ent.ManifestBlobCreate, 0, len(holds))
	for _, h := range holds {
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		builders = append(builders, c.ManifestBlob.Create().
			SetId(pdid.New(pd.ManifestBlobDomain).Uuid()).
			SetRepo(repo).
			SetManifest(m.Digest.String()).
			SetBlob(h.String()).
			SetDateCreated(now))
	}
	for part := range slices.Chunk(builders, chunk) {
		if _, err := c.ManifestBlob.CreateBulk(part...).Save(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r manifests) Get(ctx context.Context, repo string, d digest.Digest) (index.Manifest, error) {
	v, err := r.ix.client.Manifest.Query().Where(manifest.Repo(repo), manifest.Digest(d.String())).Only(ctx)
	if err != nil {
		return index.Manifest{}, notFound(err)
	}
	return toManifest(v), nil
}

func (r manifests) Erase(ctx context.Context, repo string, d digest.Digest) ([]digest.Digest, error) {
	c := r.ix.client
	v, err := c.Manifest.Query().Where(manifest.Repo(repo), manifest.Digest(d.String())).Only(ctx)
	if err != nil {
		return nil, notFound(err)
	}

	holds, err := c.ManifestBlob.Query().
		Where(manifestblob.Repo(repo), manifestblob.Manifest(d.String())).
		Select(manifestblob.FieldBlob).
		Strings(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := c.ManifestBlob.Delete().Where(manifestblob.Repo(repo), manifestblob.Manifest(d.String())).Exec(ctx); err != nil {
		return nil, err
	}
	if err := c.Manifest.DeleteOneId(v.Id).Exec(ctx); err != nil {
		return nil, err
	}

	slices.Sort(holds)
	holds = slices.Compact(holds)

	kept := map[string]struct{}{}
	for part := range slices.Chunk(holds, chunk) {
		held, err := c.ManifestBlob.Query().
			Where(manifestblob.Repo(repo), manifestblob.BlobIn(part...)).
			Select(manifestblob.FieldBlob).
			Strings(ctx)
		if err != nil {
			return nil, err
		}
		named, err := c.Manifest.Query().
			Where(manifest.Repo(repo), manifest.DigestIn(part...)).
			Select(manifest.FieldDigest).
			Strings(ctx)
		if err != nil {
			return nil, err
		}
		for _, v := range append(held, named...) {
			kept[v] = struct{}{}
		}
	}

	released := []digest.Digest{}
	for _, h := range holds {
		if _, ok := kept[h]; !ok {
			released = append(released, digest.Digest(h))
		}
	}
	return released, nil
}

func (r manifests) Referrers(ctx context.Context, repo string, subject digest.Digest, artifactType string) ([]index.Manifest, error) {
	q := r.ix.client.Manifest.Query().Where(manifest.Repo(repo), manifest.Subject(subject.String()))
	if artifactType != "" {
		q = q.Where(manifest.ArtifactType(artifactType))
	}
	vs, err := q.Order(manifest.ByDigest()).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]index.Manifest, 0, len(vs))
	for _, v := range vs {
		out = append(out, toManifest(v))
	}
	return out, nil
}

func (r manifests) Holds(ctx context.Context, repo string, d digest.Digest) (bool, error) {
	return r.ix.client.ManifestBlob.Query().Where(manifestblob.Repo(repo), manifestblob.Blob(d.String())).Exist(ctx)
}

func (r manifests) List(ctx context.Context, repo string, p index.Page) ([]index.Manifest, error) {
	q := r.ix.client.Manifest.Query().Where(manifest.Repo(repo), manifest.DigestGT(p.Last)).Order(manifest.ByDigest())
	if p.N > 0 {
		q = q.Limit(p.N)
	}
	vs, err := q.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]index.Manifest, 0, len(vs))
	for _, v := range vs {
		out = append(out, toManifest(v))
	}
	return out, nil
}

func (r manifests) Marks(ctx context.Context, repo string) iter.Seq2[digest.Digest, error] {
	return func(yield func(digest.Digest, error) bool) {
		c := r.ix.client
		last := ""
		for {
			vs, err := c.Manifest.Query().
				Where(manifest.Repo(repo), manifest.DigestGT(last)).
				Order(manifest.ByDigest()).
				Limit(1000).
				Select(manifest.FieldDigest).
				Strings(ctx)
			if err != nil {
				yield("", err)
				return
			}
			for _, v := range vs {
				if !yield(digest.Digest(v), nil) {
					return
				}
			}
			if len(vs) < 1000 {
				break
			}
			last = vs[len(vs)-1]
		}

		last = ""
		for {
			vs, err := c.ManifestBlob.Query().
				Where(manifestblob.Repo(repo), manifestblob.BlobGT(last)).
				Order(manifestblob.ByBlob()).
				Limit(1000).
				Select(manifestblob.FieldBlob).
				Strings(ctx)
			if err != nil {
				yield("", err)
				return
			}
			vs = slices.Compact(vs)
			for _, v := range vs {
				if !yield(digest.Digest(v), nil) {
					return
				}
			}
			if len(vs) == 0 {
				break
			}
			last = vs[len(vs)-1]
		}
	}
}

func toTag(v *ent.Tag) index.Tag {
	t := index.Tag{Name: v.Name, Digest: digest.Digest(v.Digest), MovedAt: v.DateMoved, CreatedAt: v.DateCreated}
	if v.DatePulled != nil {
		t.PulledAt = *v.DatePulled
	}
	return t
}

type tags struct{ ix *Index }

func (r tags) Set(ctx context.Context, repo, name string, d, from digest.Digest) error {
	c := r.ix.client
	now := r.ix.now()
	v, err := c.Tag.Query().Where(tag.Repo(repo), tag.Name(name)).Only(ctx)
	if ent.IsNotFound(err) {
		if from != "" {
			return index.ErrTagMoved
		}
		_, err := c.Tag.Create().
			SetId(pdid.New(pd.TagDomain).Uuid()).
			SetName(name).
			SetRepo(repo).
			SetDigest(d.String()).
			SetDateMoved(now).
			SetDateCreated(now).
			SetDateUpdated(now).
			Save(ctx)
		if ent.IsConstraintError(err) {
			return index.ErrTagMoved
		}
		return err
	}
	if err != nil {
		return err
	}
	if from == "" || v.Digest != from.String() {
		return index.ErrTagMoved
	}
	if from == d {
		return nil
	}
	n, err := c.Tag.Update().
		Where(tag.IdEQ(v.Id), tag.Digest(from.String())).
		SetDigest(d.String()).
		SetDateMoved(now).
		SetDateUpdated(now).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return index.ErrTagMoved
	}
	return nil
}

func (r tags) Get(ctx context.Context, repo, name string) (index.Tag, error) {
	v, err := r.ix.client.Tag.Query().Where(tag.Repo(repo), tag.Name(name)).Only(ctx)
	if err != nil {
		return index.Tag{}, notFound(err)
	}
	return toTag(v), nil
}

func (r tags) Erase(ctx context.Context, repo, name string) error {
	n, err := r.ix.client.Tag.Delete().Where(tag.Repo(repo), tag.Name(name)).Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return index.ErrNotFound
	}
	return nil
}

func (r tags) List(ctx context.Context, repo string, p index.Page) ([]string, error) {
	q := r.ix.client.Tag.Query().Where(tag.Repo(repo), tag.NameGT(p.Last)).Order(tag.ByName())
	if p.N > 0 {
		q = q.Limit(p.N)
	}
	return q.Select(tag.FieldName).Strings(ctx)
}

func (r tags) Of(ctx context.Context, repo string, d digest.Digest) ([]index.Tag, error) {
	vs, err := r.ix.client.Tag.Query().Where(tag.Repo(repo), tag.Digest(d.String())).Order(tag.ByName()).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]index.Tag, 0, len(vs))
	for _, v := range vs {
		out = append(out, toTag(v))
	}
	return out, nil
}

func (r tags) All(ctx context.Context, repo string) ([]index.Tag, error) {
	vs, err := r.ix.client.Tag.Query().Where(tag.Repo(repo)).Order(tag.ByName()).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]index.Tag, 0, len(vs))
	for _, v := range vs {
		out = append(out, toTag(v))
	}
	return out, nil
}

func (ix *Index) touchManifest(ctx context.Context, repo, d string, at time.Time) error {
	return ix.client.Manifest.Update().
		Where(
			manifest.Repo(repo),
			manifest.Digest(d),
			manifest.Or(manifest.DatePulledIsNil(), manifest.DatePulledLT(at)),
		).
		SetDatePulled(at.UTC()).
		Exec(ctx)
}

func (ix *Index) touchTag(ctx context.Context, repo, name string, at time.Time) error {
	return ix.client.Tag.Update().
		Where(
			tag.Repo(repo),
			tag.Name(name),
			tag.Or(tag.DatePulledIsNil(), tag.DatePulledLT(at)),
		).
		SetDatePulled(at.UTC()).
		Exec(ctx)
}
