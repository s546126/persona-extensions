package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// All names, projects, and systems in this file are synthetic placeholders.

func TestPersistentWhitelistAndCanonicalName(t *testing.T) {
	b := newBuilder(t)

	// CanonicalName strips leading type prefixes and collapses whitespace while keeping URLs intact.
	cases := map[string]string{
		"person:陈明":                  "陈明",
		"project: Alpha  Core ":      "Alpha Core",
		"system:node-a":              "node-a",
		"repo:acme-core":             "acme-core",
		"task:fix-bug":               "fix-bug",
		"person:project:nested-name": "nested-name",
		"https://example.com/a":      "https://example.com/a",
	}
	for raw, want := range cases {
		if got := CanonicalName(raw, ""); got != want {
			t.Errorf("CanonicalName(%q) = %q, want %q", raw, got, want)
		}
	}

	// Prefix stripping on Resolve resolves both prefixed and clean surfaces to the same canonical node.
	id1, ok := b.Resolve("project:Alpha", "project", "", nil)
	if !ok || b.G.Nodes[id1].Name != "Alpha" {
		t.Fatalf("resolve prefixed project: ok=%v node=%+v", ok, b.G.Nodes[id1])
	}
	id2, ok := b.Resolve("alpha", "project", "", nil)
	if !ok || id2 != id1 {
		t.Fatalf("same canonical key should resolve to existing node: got %q want %q", id2, id1)
	}

	// Disallowed types, local file paths, and Issue/PR IDs are rejected.
	for _, bad := range []struct{ surface, typ string }{
		{"finish Q4 report", "task"},
		{"weekly sync", "meeting"},
		{"group-chat-1", "chat"},
		{"/Users/demo/workspace/main.go", "doc"},
		{"/Users/demo/workspace/main.go", "project"},
		{"~/notes/spec.md", "system"},
		{"PR #1234", "decision"},
		{"Issue #42", "project"},
		{"#6100", "repo"},
	} {
		if id, ok := b.Resolve(bad.surface, bad.typ, "", nil); ok || id != "" {
			t.Errorf("Resolve(%q, %q) = (%q, %v), want rejected", bad.surface, bad.typ, id, ok)
		}
	}

	// Conditional types (topic/resource) are blocked on unconditional Resolve and allowed only via
	// ResolveConditional when sessions >= 3 and hasStructuralEdge == true.
	if _, ok := b.Resolve("distributed-tracing", "topic", "", nil); ok {
		t.Error("topic should not be created unconditionally")
	}
	if _, ok := b.ResolveConditional("distributed-tracing", "topic", 2, true, nil); ok {
		t.Error("topic with < 3 sessions should be rejected")
	}
	if _, ok := b.ResolveConditional("distributed-tracing", "topic", 4, false, nil); ok {
		t.Error("topic without structural edge should be rejected")
	}
	if tid, ok := b.ResolveConditional("distributed-tracing", "topic", 3, true, nil); !ok || tid == "" {
		t.Error("topic with >= 3 sessions and structural edge should be admitted")
	}
}

func TestUndirectedCollaboratesWithAndStoppedPredicates(t *testing.T) {
	b := newBuilder(t)
	p1 := b.Seed("person", "people/p1", "陈明", nil)
	p2 := b.Seed("person", "people/p2", "林明远", nil)
	proj := b.Seed("project", "projects/a", "Alpha", nil)

	// Stopped predicates about and same_circle are never written.
	if id, out := b.Add(p1, "about", proj, nil, "", ev("2026-11-01"), "high", nil, ""); id != "" || out != "skipped" {
		t.Fatalf("about should be skipped: %q %q", id, out)
	}
	if id, out := b.Add(p1, "same_circle", p2, nil, "", ev("2026-11-01"), "high", nil, ""); id != "" || out != "skipped" {
		t.Fatalf("same_circle should be skipped: %q %q", id, out)
	}

	// collaborates_with normalizes to min(s,o) -> max(s,o), so the reverse direction merges as duplicate.
	idA, outA := b.Add(p2, "collaborates_with", p1, map[string]string{"on": "Alpha"}, "", ev("2026-11-01"), "high", nil, "")
	idB, outB := b.Add(p1, "collaborates_with", p2, map[string]string{"on": "Alpha"}, "", ev("2026-11-02"), "high", nil, "")
	if outA != "new" || outB != "duplicate" || idA != idB {
		t.Fatalf("undirected collaborates_with: A=(%s,%s) B=(%s,%s)", idA, outA, idB, outB)
	}
	a := b.G.Assertions[idA]
	if a.S > a.O {
		t.Errorf("expected canonical order S < O, got %s -> %s", a.S, a.O)
	}
	if len(a.Evidence) != 2 {
		t.Errorf("expected merged evidence len=2, got %d", len(a.Evidence))
	}
}

func TestResolveAndMergeTier1AndUnmerge(t *testing.T) {
	st := &store.State{
		ActiveDays: []string{"2026-11-26"},
		Contacts: map[string]*entry.Contact{
			"person:kb-1": {Person: "person:kb-1", Name: "陈明", Source: "lark", Score: 10},
			"person:kb-2": {Person: "person:kb-2", Name: "林明远", Source: "lark", Score: 8},
			"person:kb-3": {Person: "person:kb-3", Name: "张文博", Source: "lark", Score: 5},
			"person:kb-4": {Person: "person:kb-4", Name: "李文博", Source: "lark", Score: 5},
			"person:kb-5": {Person: "person:kb-5", Name: "周海涛 Haitao", Source: "lark", Score: 7},
		},
		Graph: &store.Graph{
			Nodes: map[string]*store.Node{
				"me":        {ID: "me", Type: "person", Name: "我", Status: "active"},
				"n-p-1":     {ID: "n-p-1", Type: "person", Name: "陈明", Refs: []string{"people/kb-1"}, Status: "active"},
				"n-p-1-pre": {ID: "n-p-1-pre", Type: "person", Name: "person:陈明", Status: "candidate"},
				"n-p-1-ext": {ID: "n-p-1-ext", Type: "person", Name: "陈明", Status: "candidate"},
				"n-p-2":     {ID: "n-p-2", Type: "person", Name: "林明远", Refs: []string{"people/kb-2"}, Status: "active"},
				"n-p-2-sh":  {ID: "n-p-2-sh", Type: "person", Name: "明远", Status: "candidate"},
				// Bilingual contact anchor: "周海涛 Haitao" (reffed) + "周海涛" (un-reffed) -> Tier 1 contact merge.
				"n-p-5":    {ID: "n-p-5", Type: "person", Name: "周海涛 Haitao", Refs: []string{"people/kb-5"}, Status: "active"},
				"n-p-5-cn": {ID: "n-p-5-cn", Type: "person", Name: "周海涛", Status: "candidate"},
				// Ambiguous 2-char suffix: both 张文博 and 李文博 exist -> must NOT merge 文博.
				"n-p-3":    {ID: "n-p-3", Type: "person", Name: "张文博", Refs: []string{"people/kb-3"}, Status: "active"},
				"n-p-4":    {ID: "n-p-4", Type: "person", Name: "李文博", Refs: []string{"people/kb-4"}, Status: "active"},
				"n-p-amb":  {ID: "n-p-amb", Type: "person", Name: "文博", Status: "candidate"},
				"n-proj-1": {ID: "n-proj-1", Type: "project", Name: "alpha", Status: "active"},
				"n-proj-2": {ID: "n-proj-2", Type: "project", Name: "project:Alpha", Status: "candidate"},
				"n-task-1": {ID: "n-task-1", Type: "task", Name: "fix flaky test", Status: "active"},
			},
			Assertions: map[string]*store.Assertion{
				"a-collab-1": {ID: "a-collab-1", S: "n-p-1", P: "collaborates_with", O: "n-p-2", Status: "active", ValidFrom: "2026-11-10",
					Evidence: []store.Evidence{{Chat: "chats/c1", Day: "2026-11-10", N: 1}}},
				"a-collab-2": {ID: "a-collab-2", S: "n-p-2", P: "collaborates_with", O: "n-p-1", Status: "active", ValidFrom: "2026-11-12",
					Evidence: []store.Evidence{{Chat: "chats/c2", Day: "2026-11-12", N: 2}}},
				"a-about":  {ID: "a-about", S: "n-task-1", P: "about", O: "n-proj-1", Status: "active", ValidFrom: "2026-11-10"},
				"a-circle": {ID: "a-circle", S: "n-p-1", P: "same_circle", O: "n-p-2", Status: "active", ValidFrom: "2026-11-10"},
				"a-owns-1": {ID: "a-owns-1", S: "n-p-2", P: "owns", O: "n-proj-1", Status: "active", ValidFrom: "2026-11-10"},
				"a-owns-2": {ID: "a-owns-2", S: "n-p-2-sh", P: "owns", O: "n-proj-1", Status: "candidate", ValidFrom: "2026-11-11"},
			},
		},
	}

	rep, err := ResolveAndMerge(context.Background(), st, nil)
	if err != nil {
		t.Fatalf("ResolveAndMerge Tier 1 error: %v", err)
	}
	if rep.ArchivedTasks != 1 || st.Graph.Nodes["n-task-1"].Status != "archived" {
		t.Errorf("ArchivedTasks = %d, status = %q", rep.ArchivedTasks, st.Graph.Nodes["n-task-1"].Status)
	}
	if rep.ArchivedAbout != 1 || st.Graph.Assertions["a-about"].Status != "archived" {
		t.Errorf("ArchivedAbout = %d", rep.ArchivedAbout)
	}
	if rep.ArchivedSameCircle != 1 || st.Graph.Assertions["a-circle"].Status != "archived" {
		t.Errorf("ArchivedSameCircle = %d", rep.ArchivedSameCircle)
	}
	if rep.DedupedCollabEdges != 1 {
		t.Errorf("DedupedCollabEdges = %d, want 1", rep.DedupedCollabEdges)
	}
	if rep.PrefixedCleaned != 2 {
		t.Errorf("PrefixedCleaned = %d, want 2", rep.PrefixedCleaned)
	}
	if st.Graph.Resolve("n-p-1-pre") != "n-p-1" || st.Graph.Resolve("n-p-1-ext") != "n-p-1" {
		t.Errorf("陈明 variants did not merge into n-p-1: pre=%s ext=%s", st.Graph.Resolve("n-p-1-pre"), st.Graph.Resolve("n-p-1-ext"))
	}
	if st.Graph.Resolve("n-proj-2") != "n-proj-1" {
		t.Errorf("project:Alpha did not merge into alpha: %s", st.Graph.Resolve("n-proj-2"))
	}
	if st.Graph.Resolve("n-p-2-sh") != "n-p-2" {
		t.Errorf("unique 2-char Chinese short name 明远 did not merge into 林明远: %s", st.Graph.Resolve("n-p-2-sh"))
	}
	if st.Graph.Resolve("n-p-5-cn") != "n-p-5" || rep.Tier1ContactMerged < 1 {
		t.Errorf("contact-anchored bilingual merge failed: resolved=%s contactMerged=%d", st.Graph.Resolve("n-p-5-cn"), rep.Tier1ContactMerged)
	}
	if st.Graph.Resolve("n-p-amb") != "n-p-amb" {
		t.Errorf("ambiguous 2-char short name 文博 must NOT merge, got %s", st.Graph.Resolve("n-p-amb"))
	}

	// Verify idempotence: a second run merges/archives/dedups nothing more.
	rep2, err := ResolveAndMerge(context.Background(), st, nil)
	if err != nil {
		t.Fatalf("second ResolveAndMerge error: %v", err)
	}
	if rep2.TotalMerged != 0 || rep2.ArchivedTasks != 0 || rep2.ArchivedAbout != 0 || rep2.DedupedCollabEdges != 0 {
		t.Errorf("expected idempotent 2nd run, got %+v", rep2)
	}

	// Verify every merge is recorded in MergeRec and fully reversible via Builder.Unmerge.
	if len(st.Graph.Merges) != rep.TotalMerged {
		t.Fatalf("MergeRec count %d != TotalMerged %d", len(st.Graph.Merges), rep.TotalMerged)
	}
	b := &Builder{G: st.Graph, Today: "2026-11-27"}
	if err := b.Unmerge("n-p-2-sh"); err != nil {
		t.Fatalf("Unmerge failed: %v", err)
	}
	if st.Graph.Resolve("n-p-2-sh") != "n-p-2-sh" {
		t.Errorf("Unmerge did not restore n-p-2-sh: resolved to %s", st.Graph.Resolve("n-p-2-sh"))
	}
	for _, al := range st.Graph.Nodes["n-p-2"].Aliases {
		if al.From == "merge:n-p-2-sh" {
			t.Errorf("Unmerge left behind merge alias: %+v", al)
		}
	}
}

type stubResolveModel struct {
	calls int
	fn    func(req model.Request) (string, error)
}

func (m *stubResolveModel) Complete(ctx context.Context, r model.Request) (model.Response, error) {
	m.calls++
	txt, err := m.fn(r)
	return model.Response{Text: txt}, err
}

func TestResolveAndMergeTier2HaikuAndUnmerge(t *testing.T) {
	st := &store.State{
		ActiveDays: []string{"2026-11-26"},
		Graph: &store.Graph{
			Nodes: map[string]*store.Node{
				"n-sys-1":  {ID: "n-sys-1", Type: "system", Name: "nebulagate", Status: "active"},
				"n-sys-2":  {ID: "n-sys-2", Type: "system", Name: "nebula", Status: "candidate"},
				"n-repo-1": {ID: "n-repo-1", Type: "repo", Name: "enkeep-core", Status: "active"},
				"n-proj-1": {ID: "n-proj-1", Type: "project", Name: "enkeep 记忆插件", Status: "candidate"},
				"n-sys-3":  {ID: "n-sys-3", Type: "system", Name: "auth-gateway", Status: "active"},
				"n-sys-4":  {ID: "n-sys-4", Type: "system", Name: "billing-gateway", Status: "active"},
			},
			Assertions: map[string]*store.Assertion{
				"a1": {ID: "a1", S: "n-sys-3", P: "depends_on", O: "n-sys-1", Status: "active"},
				"a2": {ID: "a2", S: "n-sys-4", P: "depends_on", O: "n-sys-1", Status: "active"},
			},
		},
	}

	mock := &stubResolveModel{
		fn: func(req model.Request) (string, error) {
			if req.Tier != model.Small {
				t.Errorf("Tier 2 should use model.Small, got %s", req.Tier)
			}
			var in struct {
				Pairs []struct {
					PairID string                    `json:"pair_id"`
					A      struct{ ID, Name string } `json:"a"`
					B      struct{ ID, Name string } `json:"b"`
				} `json:"pairs"`
			}
			if err := json.Unmarshal([]byte(req.User), &in); err != nil {
				t.Fatalf("unmarshal Tier 2 request: %v", err)
			}
			type dec struct {
				PairID     string `json:"pair_id"`
				Verdict    string `json:"verdict"`
				Confidence string `json:"confidence"`
				Canonical  string `json:"canonical"`
				Reason     string `json:"reason"`
			}
			var decs []dec
			for _, p := range in.Pairs {
				names := p.A.Name + "|" + p.B.Name
				switch {
				case strings.Contains(names, "nebulagate") && strings.Contains(names, "nebula"):
					canon := "a"
					if p.B.Name == "nebulagate" {
						canon = "b"
					}
					decs = append(decs, dec{PairID: p.PairID, Verdict: "same_entity", Confidence: "high", Canonical: canon, Reason: "abbreviation"})
				case strings.Contains(names, "enkeep-core") && strings.Contains(names, "enkeep 记忆插件"):
					canon := "a"
					if p.B.Name == "enkeep-core" {
						canon = "b"
					}
					decs = append(decs, dec{PairID: p.PairID, Verdict: "same_entity", Confidence: "high", Canonical: canon, Reason: "bilingual repo/project alias"})
				default:
					decs = append(decs, dec{PairID: p.PairID, Verdict: "distinct", Confidence: "high", Reason: "different services"})
				}
			}
			b, _ := json.Marshal(map[string]any{"decisions": decs})
			return string(b), nil
		},
	}

	rep, err := ResolveAndMerge(context.Background(), st, mock)
	if err != nil {
		t.Fatalf("Tier 2 ResolveAndMerge error: %v", err)
	}
	if mock.calls != 1 || rep.Tier2Batches != 1 || rep.Tier2Merged != 2 {
		t.Fatalf("Tier 2 counts: calls=%d rep=%+v", mock.calls, rep)
	}
	if st.Graph.Resolve("n-sys-2") != "n-sys-1" {
		t.Errorf("nebula did not merge into nebulagate: %s", st.Graph.Resolve("n-sys-2"))
	}
	if st.Graph.Resolve("n-proj-1") != "n-repo-1" {
		t.Errorf("enkeep 记忆插件 did not merge into enkeep-core: %s", st.Graph.Resolve("n-proj-1"))
	}
	if st.Graph.Resolve("n-sys-3") == st.Graph.Resolve("n-sys-4") {
		t.Error("distinct services auth-gateway and billing-gateway were wrongly merged")
	}
	// Check Proposal == "auto-haiku-v04" and Unmerge reversibility.
	foundProposal := false
	for _, mr := range st.Graph.Merges {
		if mr.From == "n-sys-2" && mr.Proposal == ProposalTier2Haiku {
			foundProposal = true
		}
	}
	if !foundProposal {
		t.Errorf("expected Proposal=%q on Tier 2 MergeRec, got %+v", ProposalTier2Haiku, st.Graph.Merges)
	}
	b := &Builder{G: st.Graph, Today: "2026-11-27"}
	if err := b.Unmerge("n-sys-2"); err != nil || st.Graph.Resolve("n-sys-2") != "n-sys-2" {
		t.Fatalf("Tier 2 Unmerge failed: err=%v resolved=%s", err, st.Graph.Resolve("n-sys-2"))
	}
}

func TestTier2CandidatesRejectsSharedNeighborsOnlyAndAllowsAliasEvidence(t *testing.T) {
	g := &store.Graph{
		Nodes: map[string]*store.Node{
			"p-en": {ID: "p-en", Type: "person", Name: "Alice Wang", Status: "active"},
			"p-cn": {ID: "p-cn", Type: "person", Name: "李四", Status: "active"},
			// Bilingual person with explicit alias evidence should still be admitted.
			"p-bi-1": {ID: "p-bi-1", Type: "person", Name: "Bob Chen", Status: "active", Aliases: []store.Alias{{Name: "陈明远"}}},
			"p-bi-2": {ID: "p-bi-2", Type: "person", Name: "陈明远", Status: "active"},
		},
		Assertions: map[string]*store.Assertion{},
	}
	// Wire 19 shared project neighbors between p-en and p-cn.
	for i := 0; i < 19; i++ {
		pid := fmt.Sprintf("proj-%02d", i)
		g.Nodes[pid] = &store.Node{ID: pid, Type: "project", Name: fmt.Sprintf("alpha-svc-%02d", i), Status: "active"}
		g.Assertions[fmt.Sprintf("a-en-%02d", i)] = &store.Assertion{ID: fmt.Sprintf("a-en-%02d", i), S: "p-en", P: "works_on", O: pid, Status: "active"}
		g.Assertions[fmt.Sprintf("a-cn-%02d", i)] = &store.Assertion{ID: fmt.Sprintf("a-cn-%02d", i), S: "p-cn", P: "works_on", O: pid, Status: "active"}
	}

	pairs := tier2Candidates(g)
	foundAlias := false
	for _, p := range pairs {
		if (p.A == "p-en" && p.B == "p-cn") || (p.A == "p-cn" && p.B == "p-en") {
			t.Fatalf("unrelated people sharing 19 neighbors must NOT enter tier2Candidates: %+v", p)
		}
		if (p.A == "p-bi-1" && p.B == "p-bi-2") || (p.A == "p-bi-2" && p.B == "p-bi-1") {
			foundAlias = true
		}
	}
	if !foundAlias {
		t.Fatalf("expected alias-backed bilingual pair (p-bi-1, p-bi-2) to be admitted into tier2Candidates")
	}
}

func TestTier2CandidatesBoundedToMaxPairs(t *testing.T) {
	g := &store.Graph{
		Nodes:      map[string]*store.Node{},
		Assertions: map[string]*store.Assertion{},
	}
	// Construct 270 distinct fuzzy-name candidate pairs (> 260) using unique 6-char prefixes.
	for i := 0; i < 270; i++ {
		base := fmt.Sprintf("svc%03d", i)
		idA := fmt.Sprintf("n-%03d-a", i)
		idB := fmt.Sprintf("n-%03d-b", i)
		g.Nodes[idA] = &store.Node{ID: idA, Type: "system", Name: base + "-core", Status: "active"}
		g.Nodes[idB] = &store.Node{ID: idB, Type: "system", Name: base + "-core-v2", Status: "active"}
	}
	// Give pair #0 a shared neighbor so its Score is strictly higher and must survive truncation.
	g.Nodes["n-hub"] = &store.Node{ID: "n-hub", Type: "org", Name: "acme-org", Status: "active"}
	g.Assertions["a-top-1"] = &store.Assertion{ID: "a-top-1", S: "n-000-a", P: "belongs_to", O: "n-hub", Status: "active"}
	g.Assertions["a-top-2"] = &store.Assertion{ID: "a-top-2", S: "n-000-b", P: "belongs_to", O: "n-hub", Status: "active"}

	pairs1 := tier2Candidates(g)
	if len(pairs1) != tier2MaxPairs {
		t.Fatalf("expected tier2Candidates capped at %d, got %d", tier2MaxPairs, len(pairs1))
	}
	batches := (len(pairs1) + tier2BatchSize - 1) / tier2BatchSize
	if batches > 8 {
		t.Fatalf("expected <= 8 batches, got %d", batches)
	}
	if pairs1[0].A != "n-000-a" || pairs1[0].B != "n-000-b" {
		t.Fatalf("expected highest-scored pair first, got %+v", pairs1[0])
	}
	// Verify deterministic truncation on repeated calls.
	pairs2 := tier2Candidates(g)
	if len(pairs2) != len(pairs1) {
		t.Fatalf("non-deterministic length: %d vs %d", len(pairs1), len(pairs2))
	}
	for i := range pairs1 {
		if pairs1[i] != pairs2[i] {
			t.Fatalf("non-deterministic pair at %d: %+v vs %+v", i, pairs1[i], pairs2[i])
		}
	}
}

func TestTier2SystemPromptNegativeGuardrails(t *testing.T) {
	for _, want := range []string{"Launch", "Platform", "*-dist", "*-sdk", "*-cli", "*-字节版", "Data-arch-push", "data_tiktok_push"} {
		if !strings.Contains(tier2SystemPrompt, want) {
			t.Errorf("tier2SystemPrompt missing explicit negative guardrail %q", want)
		}
	}
}
