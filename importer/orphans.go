package importer

import (
	"context"
	"errors"
	"slices"

	"github.com/lesomnus/cr/index"
	"github.com/opencontainers/go-digest"
)

// planOrphans reads destination existence before copying blobs, so omission
// avoids transferring their content. The report is this planning snapshot;
// ordinary candidate tracking handles a subject deleted during the import.
// Exclusion propagates through index children and referrer chains, but a
// subject or child already in the destination remains usable.
func planOrphans(ctx context.Context, repo string, manifests map[digest.Digest]*manifest, ix index.Index, skip bool) (int, map[string]string, error) {
	present := map[digest.Digest]bool{}
	exists := func(d digest.Digest) (bool, error) {
		if ok, known := present[d]; known {
			return ok, nil
		}
		_, err := ix.Manifest().Get(ctx, repo, d)
		if err != nil && !errors.Is(err, index.ErrNotFound) {
			return false, err
		}
		present[d] = err == nil
		return present[d], nil
	}
	orphaned := map[digest.Digest]bool{}
	skipped := map[string]string{}
	refs := map[digest.Digest][]digest.Digest{}
	parents := map[digest.Digest][]digest.Digest{}
	keys := make([]digest.Digest, 0, len(manifests))
	for d, m := range manifests {
		keys = append(keys, d)
		if m.m.Subject != nil {
			refs[m.m.Subject.Digest] = append(refs[m.m.Subject.Digest], d)
		}
		for _, child := range m.m.Manifests {
			parents[child.Digest] = append(parents[child.Digest], d)
		}
	}
	slices.Sort(keys)
	var queue []digest.Digest
	omit := func(d digest.Digest, why string) {
		if _, seen := skipped[d.String()]; seen {
			return
		}
		skipped[d.String()] = why
		queue = append(queue, d)
	}
	for _, d := range keys {
		m := manifests[d]
		if m.m.Subject == nil {
			continue
		}
		subject := m.m.Subject.Digest
		if _, imported := manifests[subject]; imported {
			continue
		}
		ok, err := exists(subject)
		if err != nil {
			return 0, nil, err
		}
		if ok {
			continue
		}
		orphaned[d] = true
		if skip {
			omit(d, "subject "+subject.String()+" is absent")
		}
	}
	for head := 0; head < len(queue); head++ {
		d := queue[head]
		ok, err := exists(d)
		if err != nil {
			return 0, nil, err
		}
		if ok {
			continue
		}
		for _, ref := range refs[d] {
			orphaned[ref] = true
			omit(ref, "subject "+d.String()+" was skipped")
		}
		for _, parent := range parents[d] {
			omit(parent, "child "+d.String()+" was skipped")
		}
	}
	if len(skipped) == 0 {
		skipped = nil
	}
	return len(orphaned), skipped, nil
}
