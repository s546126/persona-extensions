package export

import (
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

func TestChineseProjectSlugMappingAndT2Block(t *testing.T) {
	dir := t.TempDir()
	projects := []entry.KBProject{
		{ID: "占位中文项目", Name: "占位中文项目", Aliases: []string{"中文子项"}},
		{ID: "raw_with_underscore", Name: "占位下划线项目"},
		{ID: "demo-repo", Name: "Demo Repo"},
	}
	st := &store.State{
		Entries: map[string]*entry.Entry{},
		Contacts: map[string]*entry.Contact{
			"person:p1": {Person: "person:p1", Name: "占位负责人甲", Score: 10},
		},
		Graph: &store.Graph{
			Nodes: map[string]*store.Node{
				"n-proj-cn": {ID: "n-proj-cn", Type: "project", Name: "占位中文项目", Status: "active"},
				"n-dep":     {ID: "n-dep", Type: "repo", Name: "占位上游依赖", Status: "active"},
				"n-owner":   {ID: "n-owner", Type: "person", Name: "占位负责人甲", Status: "active"},
			},
			Assertions: map[string]*store.Assertion{
				"a-own": {ID: "a-own", S: "n-owner", P: "owns", O: "n-proj-cn", Status: "active", ValidFrom: "2026-11-18"},
				"a-dep": {ID: "a-dep", S: "n-proj-cn", P: "depends_on", O: "n-dep", Status: "active", ValidFrom: "2026-11-19"},
			},
		},
	}

	// Goal anchor matched via display-form name with trailing brackets and one-liner.
	goal := ent("o-goal", entry.World, entry.Action, "占位中文项目 Q4 目标是完成架构收敛")
	goal.Type = "goal"
	goal.Scopes = []entry.Scope{{Kind: entry.ScopeProject, Name: "占位中文项目（中文子项）：一期"}}
	st.Entries[goal.ID] = goal

	// Project-specific rule matched via Impact.Object (not in Scopes).
	rule := ent("i-proj-rule", entry.Interaction, entry.Class, "占位中文项目改动前先核对接口契约")
	rule.Impact = entry.Impact{Moment: "dispatch", Action: "核对接口契约", Object: "占位中文项目"}
	st.Entries[rule.ID] = rule

	// Repo-suffix match ("org/demo-repo" -> "demo-repo").
	repoRule := ent("w-repo", entry.Workflow, entry.Class, "发版前必须同步文档")
	repoRule.Scopes = []entry.Scope{{Kind: entry.ScopeProject, Name: "example-org/demo-repo"}}
	st.Entries[repoRule.ID] = repoRule

	// Project with underscore raw ID normalized via KBSlug.
	usRule := ent("i-us", entry.Interaction, entry.Class, "下划线项目禁止直接推主干")
	usRule.Scopes = []entry.Scope{{Kind: entry.ScopeProject, Name: "占位下划线项目"}}
	st.Entries[usRule.ID] = usRule

	compileWith(t, st, dir, projects)

	cn := read(t, dir, "agent/projects/占位中文项目.md")
	for _, want := range []string{
		"# 项目背景：占位中文项目",
		"## 项目目标锚点与关键决定",
		"Q4 目标是完成架构收敛",
		"## 项目专属规矩",
		"核对接口契约",
		"## 负责人与核心上下游依赖",
		"负责人：占位负责人甲 负责 占位中文项目",
		"依赖：占位中文项目 依赖 占位上游依赖",
	} {
		if !strings.Contains(cn, want) {
			t.Errorf("Chinese T2 project block missing %q:\n%s", want, cn)
		}
	}
	if len(cn) > ProjectMax {
		t.Errorf("Chinese T2 project block exceeded ProjectMax: %d > %d", len(cn), ProjectMax)
	}

	if got := read(t, dir, "agent/projects/demo-repo.md"); !strings.Contains(got, "同步文档") {
		t.Errorf("org/repo suffix did not map to demo-repo.md:\n%s", got)
	}
	if got := read(t, dir, "agent/projects/raw-with-underscore.md"); !strings.Contains(got, "禁止直接推主干") {
		t.Errorf("underscore project ID did not normalize via KBSlug:\n%s", got)
	}
}
