package memsrc

import (
	"slices"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

func TestEntityAnchoredRuleRankAndSkeletonSubjects(t *testing.T) {
	st := &store.State{
		Entries: map[string]*entry.Entry{},
		Graph: &store.Graph{
			Nodes: map[string]*store.Node{
				"n-p1": {ID: "n-p1", Type: "project", Name: "占位项目甲", Status: "active"},
				"n-u1": {ID: "n-u1", Type: "person", Name: "占位同事乙", Status: "active"},
			},
			Assertions: map[string]*store.Assertion{
				"a-1": {ID: "a-1", S: "n-u1", P: "owns", O: "n-p1", Status: "active", ValidFrom: "2026-11-10"},
			},
		},
	}
	rule := &entry.Entry{
		ID: "i-ent-1", Facet: entry.Interaction, Status: entry.Stable, Count: 1, Last: "2026-11-20",
		Text:   "在占位项目甲提评审前先跑静态检查",
		Scopes: []entry.Scope{{Kind: entry.ScopeProject, Name: "占位项目甲"}},
		Impact: entry.Impact{Moment: "review", Action: "先跑静态检查", Object: "占位项目甲"},
	}
	if r := Rank(rule); r < 750 || r > 891 {
		t.Errorf("entity-anchored active rule Rank = %d, want in [750, 891]", r)
	}
	subjs := Subjects(st, rule)
	for _, want := range []string{"占位项目甲", "评审", "review", "占位同事乙"} {
		if !slices.Contains(subjs, want) {
			t.Errorf("Subjects = %v, missing %q", subjs, want)
		}
	}
	if len(subjs) > MaxSubjects {
		t.Errorf("Subjects exceeded MaxSubjects: %d", len(subjs))
	}
}
