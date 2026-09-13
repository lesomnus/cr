package blob

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/lesomnus/flob"
	"github.com/opencontainers/go-digest"
)

// TagLabel is the label a manifest in the store carries once for each tag
// pointing at it, which is how a rebuild finds tags without the index.
const TagLabel = "Tag"

// TagsOf is the tags in a manifest's labels. A store that keeps a label's
// values as one header -- S3 metadata does -- hands them back joined with
// commas, and a tag cannot contain one, so they are split here.
func TagsOf(ls flob.Labels) []string {
	var out []string
	for _, v := range ls.Values(TagLabel) {
		for part := range strings.SplitSeq(v, ",") {
			if part = strings.TrimSpace(part); part != "" && !slices.Contains(out, part) {
				out = append(out, part)
			}
		}
	}
	return out
}

// LabelTag adds or removes one tag in d's labels. It is best effort: the index
// is what answers, and a label that did not land costs a rebuild one tag.
func LabelTag(ctx context.Context, s flob.Store, d digest.Digest, tag string, add bool) error {
	info, err := s.Stat(ctx, flob.Digest(d))
	if err != nil {
		if errors.Is(err, flob.ErrNotExist) {
			return nil
		}
		return err
	}
	ls, err := info.Labels(ctx)
	if err != nil {
		return err
	}
	next := ls.Clone()
	if next == nil {
		next = flob.Labels{}
	}
	vs := slices.DeleteFunc(TagsOf(next), func(v string) bool { return v == tag })
	if add {
		vs = append(vs, tag)
	}
	if len(vs) == 0 {
		next.Del(TagLabel)
	} else {
		next[TagLabel] = vs
	}
	if err := s.Label(ctx, flob.Digest(d), next); err != nil && !errors.Is(err, flob.ErrNotExist) {
		return err
	}
	return nil
}
