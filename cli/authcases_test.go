package cli

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Four caller variants, four repositories, forward references and a YAML alias.
const matrixCases = `
subjects:
  branch:
    extends: release
    claims:
      workflow_ref: lesomnus/cr/.github/workflows/release.yml@refs/heads/feature
  workflow:
    extends: release
    claims:
      workflow_ref: lesomnus/cr/.github/workflows/other.yml@refs/heads/main
  release:
    provider: github
    claims:
      repository_id: "123456789"
      workflow_ref: lesomnus/cr/.github/workflows/release.yml@refs/heads/main
cases:
  - name: publish
    as: release
    repo: &repos [lesomnus/cr, lesomnus/cr/cache, lesomnus/cr/stage, lesomnus/cr/prod]
    allow: [pull, push, tag]
    deny: [delete, admin]
  - name: refuse writes
    as: [branch, workflow, anyone]
    repo: *repos
    deny: [push, tag, delete, admin]
`

func TestAuthCaseMatrix(t *testing.T) {
	dir := t.TempDir()
	p, _, err := readPolicy(write(t, filepath.Join(dir, "policy.yaml"), policy))
	require.NoError(t, err)
	cases, err := readCases(write(t, filepath.Join(dir, "tests.yaml"), matrixCases), p)
	require.NoError(t, err)
	require.Len(t, cases, 16)
	repos := []string{"lesomnus/cr", "lesomnus/cr/cache", "lesomnus/cr/stage", "lesomnus/cr/prod"}
	for i, subject := range []string{"release", "branch", "workflow", "anyone"} {
		name := "refuse writes"
		if i == 0 {
			name = "publish"
		}
		for j, repo := range repos {
			tc := cases[i*len(repos)+j]
			require.Equal(t, fmt.Sprintf("%s [subject=%s, repo=%s]", name, subject, repo), tc.Name)
			require.Equal(t, repo, tc.Repo)
			require.Empty(t, tc.run(p), tc.Name)
		}
	}
}

func TestAuthCaseInheritanceAndInline(t *testing.T) {
	dir := t.TempDir()
	p, _, err := readPolicy(write(t, filepath.Join(dir, "policy.yaml"), policy))
	require.NoError(t, err)
	cases, err := readCases(write(t, filepath.Join(dir, "tests.yaml"), `
subjects:
  base: &base
    provider: github
    claims:
      repository_id: "123456789"
      workflow_ref: lesomnus/cr/.github/workflows/release.yml@refs/heads/main
      count: 42
      enabled: true
      roles: [reader, writer]
      metadata: {original: true, shared: base}
  child:
    extends: base
    claims:
      enabled: false
      roles: []
      count: null
      metadata: {replacement: true}
  grandchild:
    extends: child
    claims: {extra: present}
  sibling:
    extends: base
    claims: {extra: sibling}
cases:
  - name: inherited
    as: [base, child, grandchild, sibling, *base]
    repo: lesomnus/cr
    allow: [push]
  - name: registry
    as: [base, anyone]
    registry: true
    deny: [catalog, search, admin]
  - name: legacy
    as: *base
    repo: lesomnus/cr
    allow: [push]
  - as: anyone
    repo: library/busybox
    deny: [pull]
  - name: legacy empty repo
    as: anyone
    repo: ""
    registry: true
    deny: [catalog]
`), p)
	require.NoError(t, err)
	require.Len(t, cases, 10)
	for _, tc := range cases {
		require.Empty(t, tc.run(p), tc.Name)
	}
	base, child, grandchild, sibling := cases[0].As, cases[1].As, cases[2].As, cases[3].As
	require.Equal(t, "github", grandchild.Provider)
	require.Equal(t, "123456789", grandchild.Claims["repository_id"])
	require.Equal(t, true, base.Claims["enabled"])
	require.Equal(t, false, child.Claims["enabled"])
	require.Len(t, base.Claims["roles"], 2)
	require.Empty(t, child.Claims["roles"])
	require.EqualValues(t, 42, base.Claims["count"])
	require.Contains(t, child.Claims, "count")
	require.Nil(t, child.Claims["count"], "null overrides a value, it does not remove the claim")
	require.Equal(t, map[string]any{"replacement": true}, child.Claims["metadata"], "nested values are replaced, not recursively merged")
	require.NotContains(t, base.Claims, "extra")
	require.Equal(t, "present", grandchild.Claims["extra"])
	require.Equal(t, "sibling", sibling.Claims["extra"])
	require.Equal(t, true, sibling.Claims["enabled"], "a child's overrides do not alter its parent or siblings")
	require.Equal(t, "inherited [subject=as[4]:github, repo=lesomnus/cr]", cases[4].Name)
	require.Equal(t, "registry [subject=base, registry]", cases[5].Name)
	require.Equal(t, "registry [subject=anyone, registry]", cases[6].Name)
	require.Equal(t, "legacy", cases[7].Name)
	require.Equal(t, "cases[3]", cases[8].Name)
	require.Equal(t, "legacy empty repo", cases[9].Name)
}

func TestAuthCaseMatrixRefuses(t *testing.T) {
	dir := t.TempDir()
	p, _, err := readPolicy(write(t, filepath.Join(dir, "policy.yaml"), policy))
	require.NoError(t, err)
	const valid = "cases:\n  - as: anyone\n    repo: library/busybox\n    deny: [pull]\n"
	for name, tc := range map[string]struct{ body, want string }{
		"unused missing parent":    {"subjects:\n  release: {extends: missing}\n" + valid, `unknown subject "missing"`},
		"unused unknown provider":  {"subjects:\n  release: {provider: missing}\n" + valid, `no provider "missing"`},
		"no provider":              {"subjects:\n  release: {claims: {sub: x}}\n" + valid, "subjects.release: no provider"},
		"reserved name":            {"subjects:\n  anyone: {provider: github}\n" + valid, `subject name "anyone" is reserved`},
		"empty name":               {"subjects:\n  '': {provider: github}\n" + valid, `subject name "" is reserved`},
		"self cycle":               {"subjects:\n  a: {extends: a}\n" + valid, "inheritance cycle: a -> a"},
		"indirect cycle":           {"subjects:\n  a: {extends: b}\n  b: {extends: c}\n  c: {extends: a}\n" + valid, "inheritance cycle: a -> b -> c -> a"},
		"subject typo":             {"subjects:\n  a: {provider: github, claim: {sub: x}}\n" + valid, `unknown field "claim"`},
		"duplicate subject":        {"subjects:\n  a: {provider: github}\n  a: {provider: github}\n" + valid, "already defined"},
		"unknown subject in list":  {"cases: [{as: [anyone, missing], repo: a, deny: [push]}]", `unknown subject "missing"`},
		"empty subject name":       {"cases: [{as: [''], repo: a, deny: [push]}]", "empty subject name"},
		"empty callers":            {"cases: [{as: [], repo: a, deny: [push]}]", "no `as`"},
		"null caller":              {"cases: [{as: [null], repo: a, deny: [push]}]", "no provider"},
		"number caller":            {"cases: [{as: [42], repo: a, deny: [push]}]", "want anyone"},
		"inline typo in list":      {"cases: [{as: [{provider: github, claim: {sub: x}}], repo: a, deny: [push]}]", `unknown field "claim"`},
		"empty repos":              {"cases: [{as: anyone, repo: [], deny: [push]}]", "repo lists must not be empty"},
		"empty repo in list":       {"cases: [{as: anyone, repo: [a, ''], deny: [push]}]", "repo[1]: empty repository"},
		"null repo in list":        {"cases: [{as: anyone, repo: [a, null], deny: [push]}]", "repo[1]: empty repository"},
		"both places":              {"cases: [{as: anyone, repo: [a], registry: true, deny: [admin]}]", "one of `repo` and `registry: true`"},
		"registry and empty repos": {"cases: [{as: anyone, repo: [], registry: true, deny: [admin]}]", "one of `repo` and `registry: true`"},
		"catalog on repos":         {"cases: [{as: anyone, repo: [a], deny: [catalog]}]", "write `registry: true`"},
		"push on registry":         {"cases: [{as: [anyone], registry: true, deny: [push]}]", "asked of a repository"},
		"top level typo":           {"subject: {}\n" + valid, `unknown field "subject"`},
	} {
		t.Run(name, func(t *testing.T) {
			cases, err := readCases(write(t, filepath.Join(t.TempDir(), "tests.yaml"), tc.body), p)
			require.ErrorContains(t, err, tc.want)
			require.Empty(t, cases, "an invalid file must not yield runnable assertions")
		})
	}
}

func TestAuthCaseMatrixCommand(t *testing.T) {
	dir := t.TempDir()
	policyPath := write(t, filepath.Join(dir, "policy.yaml"), policy)
	for _, tc := range []struct {
		name, body, want, failure string
	}{
		{"pass", matrixCases, "16 cases passed", ""},
		{"fail", strings.Replace(matrixCases, "deny: [push, tag, delete, admin]", "allow: [push]", 1), "FAIL  refuse writes [subject=branch, repo=lesomnus/cr/prod]", "12 of 16 cases failed"},
		{"invalid", matrixCases + "  - as: [unknown]\n    repo: a\n    allow: [push]\n", "", `unknown subject "unknown"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			command := NewCmdAuth()
			command.Writer = &output
			err := command.Run(t.Context(), []string{"test", policyPath, write(t, filepath.Join(dir, tc.name+".yaml"), tc.body)})
			if tc.failure == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.failure)
			}
			if tc.want == "" {
				require.Empty(t, output.String(), "validation finishes before running any cases")
			} else {
				require.Contains(t, output.String(), tc.want)
			}
			if tc.name == "fail" {
				require.Contains(t, output.String(), "push: denied, and it should be allowed")
				require.Contains(t, output.String(), "match cr-release")
			}
		})
	}
}
