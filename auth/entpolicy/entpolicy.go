// Package entpolicy reads bindings and tag rules from the rows the management
// plane writes.
package entpolicy

import (
	"context"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/internal/ent"
	"github.com/lesomnus/cr/internal/ent/binding"
	"github.com/lesomnus/cr/internal/ent/tagrule"
)

// Source is [auth.Source] over the ent client. It reads every tenant's rows:
// the wall is the management plane's, and a binding is in force wherever it
// was written.
type Source struct {
	client *ent.Client
}

func New(client *ent.Client) *Source {
	return &Source{client: client}
}

func (s *Source) Load(ctx context.Context) ([]auth.Binding, []auth.TagRule, error) {
	bs, err := s.client.Binding.Query().Where(binding.DateErasedIsNil()).All(ctx)
	if err != nil {
		return nil, nil, err
	}
	rs, err := s.client.TagRule.Query().Where(tagrule.DateErasedIsNil()).All(ctx)
	if err != nil {
		return nil, nil, err
	}

	bindings := make([]auth.Binding, 0, len(bs))
	for _, v := range bs {
		bindings = append(bindings, auth.Binding{
			Subject: v.Subject,
			Group:   v.Group,
			Repo:    v.Repo,
			Actions: auth.ParseActions(v.Actions),
			When:    v.When,
		})
	}
	rules := make([]auth.TagRule, 0, len(rs))
	for _, v := range rs {
		rules = append(rules, auth.TagRule{
			Name:    v.Alias,
			Repo:    v.Repo,
			Tag:     v.Tag,
			Kind:    auth.TagRuleKind(v.Kind),
			Pattern: v.Pattern,
			Groups:  v.Groups,
			Keep:    int(v.Keep),
		})
	}
	return bindings, rules, nil
}
