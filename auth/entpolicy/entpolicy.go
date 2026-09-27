// Package entpolicy reads tag rules from the rows the management plane
// writes.
package entpolicy

import (
	"context"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/internal/ent"
	"github.com/lesomnus/cr/internal/ent/tagrule"
)

// Source is [auth.Source] over the ent client. It reads every tenant's rows:
// the wall is the management plane's, and a rule is in force wherever it was
// written.
type Source struct {
	client *ent.Client
}

func New(client *ent.Client) *Source {
	return &Source{client: client}
}

func (s *Source) Load(ctx context.Context) (auth.Rules, error) {
	rs, err := s.client.TagRule.Query().Where(tagrule.DateErasedIsNil()).All(ctx)
	if err != nil {
		return auth.Rules{}, err
	}

	rules := make([]auth.TagRule, 0, len(rs))
	for _, v := range rs {
		rules = append(rules, auth.TagRule{
			Name:    v.Alias,
			Repo:    v.Repo,
			Tag:     v.Tag,
			Kind:    auth.TagRuleKind(v.Kind),
			Pattern: v.Pattern,
			Keep:    int(v.Keep),
		})
	}
	return auth.Rules{TagRules: rules}, nil
}
