package graph

import (
	"context"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// Names ending in a software or service word are not people; ordinary names (ASCII or Chinese) are left alone.
func TestMisfiledPersonType(t *testing.T) {
	for _, tc := range []struct{ name, typ, base string }{
		{"enkeep-memory 插件", "project", "enkeep-memory"},
		{"demo-cli", "project", "demo"},
		{"AsterGate 网关", "system", "AsterGate"},
		{"alpha 平台", "system", "alpha"},
		{"tools repo", "repo", "tools"},
		{"陈明", "", ""},
		{"Ruozhen Yang", "", ""},
		{"Eric", "", ""},
		{"Mercli", "", ""}, // ends in "cli" letters, but no separator: a name
		{"插件", "", ""},     // nothing left before the suffix
	} {
		typ, base := MisfiledPersonType(tc.name)
		if typ != tc.typ || base != tc.base {
			t.Errorf("MisfiledPersonType(%q) = (%q, %q), want (%q, %q)", tc.name, typ, base, tc.typ, tc.base)
		}
	}
}

// Phase 1 graph clean-up: a person node merged out of "enkeep-memory 插件" goes into the existing project node; a lone
// "foo-cli" person is retyped; a person with a people card or a plain name stays a person; Unmerge restores it.
func TestResolveFixesMisfiledPersonNodes(t *testing.T) {
	st := &store.State{
		ActiveDays: []string{"2026-11-26"},
		Graph: &store.Graph{
			Nodes: map[string]*store.Node{
				"me":          {ID: "me", Type: "person", Name: "我", Status: "active"},
				"n-project-1": {ID: "n-project-1", Type: "project", Name: "enkeep-memory", Status: "active"},
				"n-person-2": {ID: "n-person-2", Type: "person", Name: "enkeep-memory", Status: "candidate",
					Aliases: []store.Alias{{Name: "enkeep-memory"}, {Name: "enkeep-memory 插件", From: "merge:n-person-3"}}},
				"n-person-3": {ID: "n-person-3", Type: "person", Name: "enkeep-memory 插件", MergedInto: "n-person-2", Status: "candidate"},
				"n-person-4": {ID: "n-person-4", Type: "person", Name: "foo-cli", Status: "candidate"},
				"n-person-5": {ID: "n-person-5", Type: "person", Name: "陈明", Status: "active"},
				"n-person-6": {ID: "n-person-6", Type: "person", Name: "bar 平台", Refs: []string{"people/kb-6"}, Status: "active"},
			},
			Assertions: map[string]*store.Assertion{},
		},
	}
	rep, err := ResolveAndMergeAt(context.Background(), st, nil, "2026-11-26")
	if err != nil {
		t.Fatal(err)
	}
	g := st.Graph
	if got := g.Resolve("n-person-2"); got != "n-project-1" {
		t.Errorf("enkeep-memory person node resolves to %q, want the project", got)
	}
	if got := g.Resolve("n-person-3"); got != "n-project-1" {
		t.Errorf("enkeep-memory 插件 resolves to %q, want the project", got)
	}
	if n := g.Nodes["n-person-4"]; n.Type != "project" || n.Name != "foo" || n.MergedInto != "" {
		t.Errorf("foo-cli node = %+v, want retyped to project foo", n)
	}
	if g.Nodes["n-person-5"].Type != "person" || g.Nodes["n-person-6"].Type != "person" {
		t.Error("a person (plain name, or with a people card) was retyped")
	}
	if rep.MisfiledPersonMerged != 1 || rep.MisfiledPersonRetyped != 1 {
		t.Errorf("misfiled merged=%d retyped=%d, want 1 and 1", rep.MisfiledPersonMerged, rep.MisfiledPersonRetyped)
	}
	// Idempotent.
	rep2, err := ResolveAndMergeAt(context.Background(), st, nil, "2026-11-26")
	if err != nil || rep2.MisfiledPersonMerged != 0 || rep2.MisfiledPersonRetyped != 0 {
		t.Errorf("second run moved more: %+v err=%v", rep2, err)
	}
	b := &Builder{G: g, Today: "2026-11-26"}
	if err := b.Unmerge("n-person-2"); err != nil || g.Resolve("n-person-2") != "n-person-2" {
		t.Errorf("unmerge: %v resolve=%q", err, g.Resolve("n-person-2"))
	}
}

// A new mention typed person but named like software resolves to the existing project, or is made with its real type.
func TestResolveRoutesMisfiledPersonMention(t *testing.T) {
	g := &store.Graph{Nodes: map[string]*store.Node{
		"n-project-1": {ID: "n-project-1", Type: "project", Name: "enkeep-memory", Status: "active"},
	}, Assertions: map[string]*store.Assertion{}}
	b := &Builder{G: g, Today: "2026-11-26", Reg: &Registry{Types: append([]string{}, Types...)}}
	if id, ok := b.Resolve("enkeep-memory 插件", "person", "", nil); !ok || id != "n-project-1" {
		t.Errorf("resolve = (%q, %v), want the project", id, ok)
	}
	id, ok := b.Resolve("AsterGate 网关", "person", "", nil)
	if !ok || g.Nodes[id] == nil || g.Nodes[id].Type != "system" {
		t.Errorf("AsterGate 网关 made as %+v", g.Nodes[id])
	}
}
