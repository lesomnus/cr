package importer_test

import (
	"context"
	"testing"

	"github.com/lesomnus/cr/importer"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func (s *source) referrer(repo, tag string, subject v1.Descriptor) v1.Descriptor {
	layer := s.blob(repo, []byte(tag), "application/octet-stream")
	return s.manifest(repo, tag, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: "application/vnd.example.sig", Config: v1.DescriptorEmptyJSON, Layers: []v1.Descriptor{layer}, Subject: &subject}, v1.MediaTypeImageManifest)
}

func TestImportOrphaned(t *testing.T) {
	const repo = "acme/app"
	ctx := context.Background()
	s := newSource(t)
	absent := v1.Descriptor{Digest: digest.FromString("absent"), MediaType: v1.MediaTypeImageManifest, Size: 1}
	first := s.referrer(repo, "orphan", absent)
	second := s.referrer(repo, "chain", first)
	parent := s.manifest(repo, "index", v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{first}}, v1.MediaTypeImageIndex)
	keep := s.image(repo, "keep", "keep")
	src := zotRoot(t, s, repo)
	for _, skip := range []bool{false, true} {
		name := "keep"
		if skip {
			name = "skip"
		}
		t.Run(name, func(t *testing.T) {
			x := newTarget(t)
			r, err := x.run(t, src, importer.Options{SkipOrphaned: skip})
			require.NoError(t, err)
			rr := repoReport(t, r, repo)
			require.Empty(t, rr.Missing)
			if skip {
				require.Equal(t, 2, rr.Orphaned, "omitting a subject makes its referrer an orphan too")
				require.Len(t, rr.Skipped, 3, "dependent indexes cannot be imported without their children")
				require.Equal(t, 1, rr.Manifests)
				require.Equal(t, 1, rr.Tags)
				for _, d := range []digest.Digest{first.Digest, second.Digest, parent.Digest} {
					_, err = x.ix.Manifest().Get(ctx, repo, d)
					require.ErrorIs(t, err, index.ErrNotFound)
					_, err = x.stores.Use(repo).Stat(ctx, flob.Digest(d))
					require.Error(t, err, "skipped manifest bytes are not adopted")
				}
			} else {
				require.Equal(t, 1, rr.Orphaned, "only the first referrer's subject is missing")
				require.Empty(t, rr.Skipped)
				require.Equal(t, 4, rr.Manifests)
				_, err = x.ix.Orphan().Get(ctx, repo, absent.Digest)
				require.NoError(t, err)
				rerun, err := x.run(t, src, importer.Options{})
				require.NoError(t, err)
				require.Zero(t, repoReport(t, rerun, repo).Manifests)
				require.Equal(t, 1, repoReport(t, rerun, repo).Orphaned)
			}
			_, err = x.ix.Manifest().Get(ctx, repo, keep.Digest)
			require.NoError(t, err)
		})
	}
}

func TestImportOrphanedDestinationSubject(t *testing.T) {
	const repo = "acme/app"
	ctx := context.Background()
	s := newSource(t)
	subject := s.image("subjects", "image", "subject")
	ref := s.referrer(repo, "signature", subject)
	src := zotRoot(t, s, repo)
	for _, sameRepo := range []bool{false, true} {
		name := "other repository"
		if sameRepo {
			name = "same repository"
		}
		t.Run(name, func(t *testing.T) {
			x := newTarget(t)
			dest := repo
			if !sameRepo {
				dest = "other"
			}
			require.NoError(t, x.ix.Manifest().Put(ctx, dest, index.Manifest{Digest: subject.Digest, MediaType: subject.MediaType, Size: subject.Size}, nil))
			r, err := x.run(t, src, importer.Options{SkipOrphaned: true})
			require.NoError(t, err)
			rr := repoReport(t, r, repo)
			_, err = x.ix.Manifest().Get(ctx, repo, ref.Digest)
			if sameRepo {
				require.Zero(t, rr.Orphaned)
				require.NoError(t, err)
			} else {
				require.Equal(t, 1, rr.Orphaned)
				require.ErrorIs(t, err, index.ErrNotFound)
			}
		})
	}
}

func TestImportOrphanedExistingChild(t *testing.T) {
	const repo = "acme/app"
	ctx := context.Background()
	s := newSource(t)
	absent := v1.Descriptor{Digest: digest.FromString("absent"), MediaType: v1.MediaTypeImageManifest, Size: 1}
	orphan := s.referrer(repo, "orphan", absent)
	childRef := s.referrer(repo, "ref", orphan)
	parent := s.manifest(repo, "index", v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{orphan}}, v1.MediaTypeImageIndex)
	src := zotRoot(t, s, repo)
	x := newTarget(t)
	_, err := x.run(t, src, importer.Options{})
	require.NoError(t, err)
	// A second import's skip option must not erase an existing child or omit
	// otherwise usable parents/referrers merely because it skips that child.
	r, err := x.run(t, src, importer.Options{SkipOrphaned: true})
	require.NoError(t, err)
	rr := repoReport(t, r, repo)
	require.Equal(t, 1, rr.Orphaned)
	require.Len(t, rr.Skipped, 1)
	for _, d := range []digest.Digest{orphan.Digest, childRef.Digest, parent.Digest} {
		_, err = x.ix.Manifest().Get(ctx, repo, d)
		require.NoError(t, err)
	}
}
