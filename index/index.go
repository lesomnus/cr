// Package index is the registry's view of names over the content flob holds:
// repositories, the manifests in them and what each holds, and tags.
//
// It is derived data over immutable content and authoritative for names. Blob
// `HEAD`, `GET` and `PUT` never touch it.
package index

import (
	"context"
	"errors"
	"iter"
	"time"

	"github.com/opencontainers/go-digest"
)

var (
	ErrNotFound = errors.New("not found")

	// ErrTagMoved is a tag that is no longer where the caller last saw it.
	ErrTagMoved = errors.New("tag moved")

	// ErrBusy is a repository whose lock was not free within the bound.
	ErrBusy = errors.New("repository busy")
)

// Index is one accessor per resource, the way payday's Server has one per
// entity, and the only thing the /v2 handler talks to besides flob.
type Index interface {
	Repo() Repos
	Manifest() Manifests
	Tag() Tags
	Pulled() Pulled

	// Tx runs fn inside one transaction and hands it the Index to use; the
	// outer one sees nothing until fn returns nil. When repo is not empty the
	// transaction first takes that repository's lock, which every write that
	// changes what a repository references holds: manifest put and erase,
	// blob delete, repository erase, and the repository's sweep. A lock not
	// taken within the implementation's bound is [ErrBusy].
	//
	// A Tx inside fn runs in the same transaction.
	Tx(ctx context.Context, repo string, fn func(Index) error) error

	// Lock takes repo's lock outside any transaction and holds it until the
	// answered function is called: for work that must not race the writes
	// that change what the repository references and is not itself a
	// transaction, the sweep's walk of a store. A lock not taken within the
	// bound is [ErrBusy].
	Lock(ctx context.Context, repo string) (func(), error)
}

// Page is (last, n) as the spec paginates: names after Last, at most N. An N
// of zero or less is the implementation's default.
type Page struct {
	Last string
	N    int
}

type Repo struct {
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RepoPatch is what the management plane may change about a repository.
type RepoPatch struct {
	Description *string
}

type Repos interface {
	// Ensure answers the repository, creating it on first use. Called from a
	// manifest put, never from a blob.
	Ensure(ctx context.Context, name string) (Repo, error)
	Get(ctx context.Context, name string) (Repo, error)
	Update(ctx context.Context, name string, patch RepoPatch) (Repo, error)

	// List is `_catalog`: names in lexical order after p.Last.
	List(ctx context.Context, p Page) ([]string, error)

	// Search is `/v1/search`: repositories whose name or description
	// contains q, in lexical order of name after p.Last.
	Search(ctx context.Context, q string, p Page) ([]Repo, error)

	// Erase removes the repository and every row under it. The caller has
	// already erased the repository's blobs from flob, or accepts leaking
	// them to the sweep.
	Erase(ctx context.Context, name string) error
}

type Manifest struct {
	Digest    digest.Digest
	MediaType string

	// ArtifactType is the manifest's `artifactType`, or its config's media
	// type when it has none.
	ArtifactType string

	// Subject is the digest of the manifest's subject, or empty.
	Subject digest.Digest

	Size        int64
	Annotations map[string]string

	CreatedAt time.Time

	// PulledAt is when the manifest was last pulled, as far as the batched
	// bookkeeping knows; zero if never.
	PulledAt time.Time
}

type Manifests interface {
	// Put records a manifest and what it holds. Idempotent on (repo, digest):
	// a second put of the same manifest changes nothing.
	Put(ctx context.Context, repo string, m Manifest, holds []digest.Digest) error
	Get(ctx context.Context, repo string, d digest.Digest) (Manifest, error)

	// Erase removes the manifest and answers what it held that nothing in
	// the repository still refers to: no other manifest holds it and it is
	// not itself a manifest there. The caller erases those from flob.
	Erase(ctx context.Context, repo string, d digest.Digest) (released []digest.Digest, err error)

	// Referrers is the manifests whose subject is d, narrowed to
	// artifactType when it is not empty.
	Referrers(ctx context.Context, repo string, subject digest.Digest, artifactType string) ([]Manifest, error)

	// Holds reports whether any manifest in repo holds d.
	Holds(ctx context.Context, repo string, d digest.Digest) (bool, error)

	// List is every manifest in repo, in digest order after p.Last.
	List(ctx context.Context, repo string, p Page) ([]Manifest, error)

	// Marks is everything the index believes repo holds: its manifests and
	// every digest they hold. The mark phase of the sweep.
	Marks(ctx context.Context, repo string) iter.Seq2[digest.Digest, error]
}

type Tag struct {
	Name      string
	Digest    digest.Digest
	MovedAt   time.Time
	PulledAt  time.Time
	CreatedAt time.Time
}

type Tags interface {
	// Set points name at d. from is the digest the caller resolved the tag to
	// a moment ago, or empty for a tag it believes does not exist; a tag that
	// is elsewhere by now fails with [ErrTagMoved], so a rule checked against
	// from holds for the write.
	Set(ctx context.Context, repo, name string, d, from digest.Digest) error
	Get(ctx context.Context, repo, name string) (Tag, error)
	Erase(ctx context.Context, repo, name string) error

	// List is `tags/list`: names in lexical order after p.Last.
	List(ctx context.Context, repo string, p Page) ([]string, error)

	// Of is the tags pointing at d.
	Of(ctx context.Context, repo string, d digest.Digest) ([]Tag, error)

	// All is every tag in repo.
	All(ctx context.Context, repo string) ([]Tag, error)
}

// Pulled is the one write a read makes, and it is not on the request: Touch
// queues and returns, and an implementation writes in batches. Losing a batch
// costs a retention decision nothing noticeable.
type Pulled interface {
	// Touch records that the manifest d was pulled from repo at at, by tag
	// when tag is not empty.
	Touch(repo, tag string, d digest.Digest, at time.Time)
}

// Resolve answers the digest a tag points at.
func Resolve(ctx context.Context, ix Index, repo, tag string) (digest.Digest, error) {
	t, err := ix.Tag().Get(ctx, repo, tag)
	if err != nil {
		return "", err
	}
	return t.Digest, nil
}
