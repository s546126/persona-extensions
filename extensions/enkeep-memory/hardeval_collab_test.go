package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/overview"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// Synthetic fixtures only.
func TestCollabMemoryCands(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("in.md", "---\nname: a\nmetadata:\n  type: feedback\n---\n\n11-20 09:10, the user said: just go ahead.\n")
	write("early.md", "---\nname: b\nmetadata:\n  type: feedback\n---\n\n11-02 the user said something.\n")
	write("ref.md", "---\nname: c\nmetadata:\n  type: reference\n---\n\n11-20 a pointer.\n")
	cs, err := collabMemoryCands(dir, "2026-11-16", "2026-11-24")
	if err != nil || len(cs) != 1 || cs[0].Day != "2026-11-20" || cs[0].Source != "memory" || strings.Contains(cs[0].Text, "type: feedback") {
		t.Fatalf("%v %+v", err, cs)
	}
}

// A correction in range becomes a candidate with the request before it and the assistant's move between; a line
// without a cue, out of range, or with no move before it does not.
func TestCollabSessionCands(t *testing.T) {
	dir := t.TempDir()
	jsonl := strings.Join([]string{
		`{"type":"user","timestamp":"2026-11-20T01:00:00Z","message":{"content":"clean up the disk"}}`,
		`{"type":"assistant","timestamp":"2026-11-20T01:00:05Z","message":{"content":[{"type":"text","text":"Shall I start?"},{"type":"tool_use","name":"AskUser","input":{"q":"start?"}}]}}`,
		`{"type":"user","timestamp":"2026-11-20T01:01:00Z","message":{"content":"我说过了，直接做"}}`,
		`{"type":"user","timestamp":"2026-11-20T01:02:00Z","message":{"content":"thanks"}}`,
		`{"type":"user","timestamp":"2026-11-01T01:02:00Z","message":{"content":"别再问了"}}`,
		`{"type":"user","timestamp":"2026-11-20T01:03:00Z","message":{"content":"<system-reminder>又</system-reminder>"}}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := collabSessionCands([]string{dir}, "2026-11-16", "2026-11-24")
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v %+v", err, cs)
	}
	c := cs[0].Text
	if !strings.Contains(c, "clean up the disk") || !strings.Contains(c, "[AskUser") || !strings.Contains(c, "我说过了，直接做") {
		t.Errorf("context %q", c)
	}
}

func TestCollabTableEntity(t *testing.T) {
	r := collabResult{Model: "m", Judge: "j", N: 2, Repeats: 1, Entity: []string{"a"},
		Score:  map[string]map[string]map[string]float64{"1400": {"resident": {"avoided": 0.5}}},
		Bytes:  map[string]map[string]int{"1400": {"resident": 1400}},
		PerInc: map[string]map[string]map[string]float64{"1400": {"resident": {"a": 1, "b": 0}}}}
	md := collabTable(r, []string{"resident"})
	if !strings.Contains(md, "entity incidents avoided (1)") || !strings.Contains(md, "| 0/2/0 | 100% |") {
		t.Fatalf("entity column missing:\n%s", md)
	}
	if !collabCue.MatchString("你又搞混人了，这个不是他负责的") {
		t.Fatal("entity cue not matched")
	}
}

// The recall arms bring in what the incident's names point at, before the day; the ablation leaves out the world and
// people entries.
func TestCollabRecall(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		"w-own":  {ID: "w-own", Facet: entry.World, Predicate: "owns", Object: "project-x", Text: "user-b is responsible for project-x", Status: entry.Stable, Count: 3, First: "2026-11-01", Last: "2026-11-10", Tagged: true, Origin: entry.Behavior},
		"j-rule": {ID: "j-rule", Facet: entry.Judgement, Text: "for project-x, release only after the gate", Status: entry.Stable, Count: 3, First: "2026-11-01", Last: "2026-11-10", Tagged: true, Origin: entry.Behavior, Scopes: []entry.Scope{{Kind: entry.ScopeProject, Name: "project-x"}}},
		"j-late": {ID: "j-late", Facet: entry.Judgement, Text: "for project-x, a rule learnt later", Status: entry.Stable, Count: 3, First: "2026-11-20", Last: "2026-11-20", Tagged: true, Origin: entry.Behavior, Scopes: []entry.Scope{{Kind: entry.ScopeProject, Name: "project-x"}}},
	}}
	f := heFilter{leak: map[string]bool{}, units: map[string]bool{}}
	idx := collabNames(st)
	all := idx.recall(st, f, "2026-11-16", "the user asks who should review project-x", "", 2048, true)
	if !strings.Contains(all, "user-b is responsible") || !strings.Contains(all, "release only after the gate") || strings.Contains(all, "learnt later") {
		t.Fatalf("recall = %q", all)
	}
	noent := idx.recall(st, f, "2026-11-16", "the user asks who should review project-x", "", 2048, false)
	if strings.Contains(noent, "user-b is responsible") || !strings.Contains(noent, "release only after the gate") {
		t.Fatalf("ablation = %q", noent)
	}
}

// A world fact that names a repository only in its text is found by that name; a name behind many entries weighs
// less than the repository's own.
func TestCollabRecallByIdentifier(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		"w-repo": {ID: "w-repo", Facet: entry.World, Type: "decision", Text: "repo-shared-x is a shared repository; personal parts get their own", Status: entry.Stable, Count: 1, First: "2026-11-01", Last: "2026-11-01", Tagged: true, Origin: entry.Behavior},
	}}
	for i := 0; i < 30; i++ {
		id := "j-" + strings.Repeat("a", i+1)
		st.Entries[id] = &entry.Entry{ID: id, Facet: entry.Judgement, Text: "general rule " + id, Status: entry.Stable, Count: 5, First: "2026-11-01", Last: "2026-11-10", Tagged: true, Origin: entry.Behavior, Scopes: []entry.Scope{{Kind: entry.ScopeProject, Name: "platform-x"}}}
	}
	f := heFilter{leak: map[string]bool{}, units: map[string]bool{}}
	got := collabNames(st).recall(st, f, "2026-11-16", "the user asks whether to create a repo next to repo-shared-x on platform-x", "", 200, true)
	if !strings.HasPrefix(got, "- repo-shared-x is a shared repository") {
		t.Fatalf("recall = %q", got)
	}
}

func TestHardEvalPatternOutRespectsCutoff(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		"e-before": {
			ID: "e-before", Facet: entry.Judgement, Text: "prefer explicit tests before refactoring",
			Status: entry.Stable, Count: 4, First: "2026-03-01", Last: "2026-03-25",
			Tagged: true, Origin: entry.Behavior,
		},
		"e-future": {
			ID: "e-future", Facet: entry.Judgement, Text: "future rule discovered after cutoff",
			Status: entry.Stable, Count: 4, First: "2026-03-20", Last: "2026-03-20",
			Tagged: true, Origin: entry.Behavior,
		},
	}}
	ov := &overview.State{Patterns: map[string]*overview.Pattern{
		"ov:before": {
			ID: "ov:before", Facet: string(entry.Judgement), Lifecycle: overview.Core,
			Text: "keep pre-cutoff pattern even if sighted later", EvidenceIDs: []string{"e-before"},
			ScenesCount: 2, SourcesCount: 3, LastEvidence: "2026-03-25",
		},
		"ov:future": {
			ID: "ov:future", Facet: string(entry.Judgement), Lifecycle: overview.Core,
			Text: "leaked post-cutoff pattern text", EvidenceIDs: []string{"e-future"},
			ScenesCount: 2, SourcesCount: 3, LastEvidence: "2026-03-20",
		},
		"ov:pruned-future": {
			ID: "ov:pruned-future", Facet: string(entry.Judgement), Lifecycle: overview.Core,
			Text: "leaked pruned post-cutoff pattern", EvidenceIDs: []string{"e-gone"},
			ScenesCount: 2, SourcesCount: 3, LastEvidence: "2026-03-20",
		},
		"ov:mixed": {
			ID: "ov:mixed", Facet: string(entry.Judgement), Lifecycle: overview.Core,
			Text: "leaked mixed pre-and-post-cutoff pattern", EvidenceIDs: []string{"e-before", "e-future"},
			ScenesCount: 2, SourcesCount: 3, LastEvidence: "2026-03-20",
		},
	}}
	f := heFilter{leak: map[string]bool{}, units: map[string]bool{}}
	mems := heMemories(st, ov, nil, f, "2026-03-10", 8000)
	for _, arm := range []string{"overview", "resident"} {
		if strings.Contains(mems[arm], "leaked post-cutoff pattern text") {
			t.Errorf("%s arm leaked post-cutoff pattern:\n%s", arm, mems[arm])
		}
		if strings.Contains(mems[arm], "leaked pruned post-cutoff pattern") {
			t.Errorf("%s arm leaked entry-less post-cutoff pattern:\n%s", arm, mems[arm])
		}
		if strings.Contains(mems[arm], "leaked mixed pre-and-post-cutoff pattern") {
			t.Errorf("%s arm leaked mixed pre/post-cutoff pattern:\n%s", arm, mems[arm])
		}
		if !strings.Contains(mems[arm], "keep pre-cutoff pattern") {
			t.Errorf("%s arm dropped legitimate pre-cutoff pattern:\n%s", arm, mems[arm])
		}
	}
	// Empty cutoff means no cutoff: both entry-backed and entry-less patterns are kept.
	if f.patternOut(st, ov.Patterns["ov:before"], "") {
		t.Error("empty cutoff unexpectedly excluded entry-backed pattern")
	}
	if f.patternOut(st, ov.Patterns["ov:pruned-future"], "") {
		t.Error("empty cutoff unexpectedly excluded entry-less pattern")
	}
}

func TestCollabWindowsAndExclude(t *testing.T) {
	ws, err := parseCollabWindows("2026-11-01..2026-11-15,2026-11-25..2026-11-26", "", "")
	if err != nil || len(ws) != 2 {
		t.Fatalf("parseCollabWindows: %v %+v", err, ws)
	}
	if w, ok := windowFor("2026-11-10", ws); !ok || w != "2026-11-01..2026-11-15" {
		t.Fatalf("expected window 1, got %q %v", w, ok)
	}
	if _, ok := windowFor("2026-11-20", ws); ok {
		t.Fatal("dev-window day unexpectedly matched holdout windows")
	}
	if w, ok := windowFor("2026-11-25", ws); !ok || w != "2026-11-25..2026-11-26" {
		t.Fatalf("expected window 2, got %q %v", w, ok)
	}

	// Balanced candidate cap keeps both windows represented.
	var cands []collabCand
	for i := 0; i < 6; i++ {
		cands = append(cands, collabCand{ID: fmt.Sprintf("a-%d", i), Window: "2026-11-01..2026-11-15"})
	}
	for i := 0; i < 6; i++ {
		cands = append(cands, collabCand{ID: fmt.Sprintf("b-%d", i), Window: "2026-11-25..2026-11-26"})
	}
	bal := collabBalanceCands(cands, ws, 4)
	if len(bal) != 4 {
		t.Fatalf("got %d balanced cands", len(bal))
	}
	cnt := map[string]int{}
	for _, c := range bal {
		cnt[c.Window]++
	}
	if cnt["2026-11-01..2026-11-15"] != 2 || cnt["2026-11-25..2026-11-26"] != 2 {
		t.Fatalf("unbalanced windows: %+v", cnt)
	}

	// Exclusion by ID and by same-day want Jaccard.
	dir := t.TempDir()
	devPath := filepath.Join(dir, "dev.json")
	devSet := collabSet{
		Incidents: []collabIncident{
			{ID: "s-dev001", Day: "2026-11-25", Want: "派活前先检查目标仓库的权限与分支状态"},
		},
	}
	raw, _ := json.Marshal(devSet)
	if err := os.WriteFile(devPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ex, err := loadCollabExclusions(devPath)
	if err != nil {
		t.Fatal(err)
	}
	if !ex.matches(collabIncident{ID: "s-dev001", Day: "2026-11-02", Want: "different"}) {
		t.Error("expected exact ID exclusion")
	}
	if !ex.matches(collabIncident{ID: "m-other", Day: "2026-11-25", Want: "派活前先检查目标仓库的权限与分支"}) {
		t.Error("expected same-day want exclusion")
	}
	if ex.matches(collabIncident{ID: "m-other", Day: "2026-11-26", Want: "派活前先检查目标仓库的权限与分支状态"}) {
		t.Error("different day should not exclude by want")
	}
}

func TestCollabEntityCue(t *testing.T) {
	for _, s := range []string{
		"你搞错仓库了，不是这个库",
		"这个任务依赖另一个服务，归属搞混了",
		"派错主管了，应该找负责网关的同学",
		"哪台机器跑门禁，主机名弄错了",
	} {
		if !collabEntityCue.MatchString(s) {
			t.Errorf("collabEntityCue missed %q", s)
		}
	}
}

func TestHubDecisionsQuery(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		"e-dispatch": {
			ID: "e-dispatch", Facet: entry.Interaction, Text: "for project-x, verify branch before dispatch",
			Status: entry.Stable, Count: 5, First: "2026-11-01", Last: "2026-11-10", Tagged: true, Origin: entry.Behavior,
			Scopes: []entry.Scope{{Kind: entry.ScopeProject, Name: "project-x"}},
			Impact: entry.Impact{Moment: "dispatch"},
		},
		"e-future": {
			ID: "e-future", Facet: entry.Interaction, Text: "project-x future rule",
			Status: entry.Stable, Count: 5, First: "2026-11-20", Last: "2026-11-20", Tagged: true, Origin: entry.Behavior,
			Scopes: []entry.Scope{{Kind: entry.ScopeProject, Name: "project-x"}},
		},
		"e-sensitive": {
			ID: "e-sensitive", Facet: entry.Interaction, Text: "project-x private finance rule",
			Status: entry.Stable, Count: 5, First: "2026-11-01", Last: "2026-11-10", Tagged: true, Origin: entry.Behavior,
			Categories: []entry.Category{{Name: entry.CatFinance}},
		},
	}}
	f := heFilter{leak: map[string]bool{}, units: map[string]bool{}}
	page := collabHubPage1(st, nil, f, "2026-11-15", 4096)
	// Multi-word AND across subject ("project-x") and occasion word ("派活"): must match e-dispatch only.
	got := hubDecisionsQuery(page, []string{"project-x 派活"}, decisionsQueryMaxBytes)
	if !strings.Contains(got, "[e-dispatch]") {
		t.Fatalf("expected e-dispatch in hubDecisionsQuery result, got %q", got)
	}
	if strings.Contains(got, "e-future") || strings.Contains(got, "e-sensitive") {
		t.Fatalf("leaked future or sensitive entry: %q", got)
	}
	// Missing second AND term must not match.
	if none := hubDecisionsQuery(page, []string{"project-x nonexistent-term"}, decisionsQueryMaxBytes); none != "" {
		t.Fatalf("expected empty result for unmatched AND term, got %q", none)
	}
	// Truncation at maxBytes.
	var big []memsrc.Entry
	for i := 0; i < 40; i++ {
		big = append(big, memsrc.Entry{
			ID:   fmt.Sprintf("e-%02d", i),
			Text: "project-x " + strings.Repeat("x", 120),
		})
	}
	clipped := hubDecisionsQuery(big, []string{"project-x"}, 500)
	if len(clipped) == 0 || len(clipped) > 500 {
		t.Fatalf("expected truncated output <= 500 bytes, got %d", len(clipped))
	}
}

func TestCollabTableSegments(t *testing.T) {
	r := collabResult{
		Model: "m", Judge: "j", N: 3, Repeats: 1,
		Score:  map[string]map[string]map[string]float64{"4096": {"resident": {"avoided": 0.5}}},
		Bytes:  map[string]map[string]int{"4096": {"resident": 4000}},
		PerInc: map[string]map[string]map[string]float64{"4096": {"resident": {"a": 1, "b": 0, "c": 0.5}}},
		SegmentN: map[string]int{
			"2026-11-01..2026-11-15": 2,
			"2026-11-25..2026-11-26": 1,
		},
		Segments: map[string]map[string]map[string]map[string]float64{
			"2026-11-01..2026-11-15": {"4096": {"resident": {"avoided": 0.5, "wrong_action": 0.25}}},
			"2026-11-25..2026-11-26": {"4096": {"resident": {"avoided": 1.0, "wrong_action": 0.0}}},
		},
	}
	md := collabTable(r, []string{"resident"})
	if !strings.Contains(md, "#### segment 2026-11-01..2026-11-15 (2 incidents)") || !strings.Contains(md, "#### segment 2026-11-25..2026-11-26 (1 incidents)") {
		t.Fatalf("segments missing from table:\n%s", md)
	}
}

func TestDiscoverCollabDirsAndMemoryIDs(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	primary := mk("proj-assistant")
	primaryMem := mk("proj-assistant/memory")
	sup := mk("proj-supervisor-a")
	supMem := mk("proj-supervisor-a/memory")
	agent := mk("ws-agent-1")
	agentMem := mk("ws-agent-1/memory")
	_ = mk("unrelated-other")

	sessDirs, memDirs, explicitMem, err := discoverCollabDirs(primaryMem, primary, root, "proj-,ws-")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessDirs) != 3 {
		t.Fatalf("expected 3 deduplicated session dirs, got %d: %v", len(sessDirs), sessDirs)
	}
	if len(memDirs) != 3 {
		t.Fatalf("expected 3 deduplicated memory dirs, got %d: %v", len(memDirs), memDirs)
	}
	if !explicitMem[primaryMem] || explicitMem[supMem] || explicitMem[agentMem] {
		t.Fatalf("unexpected explicitMem map: %v", explicitMem)
	}

	// Identically named feedback files in two discovered project memory dirs must produce distinct IDs, while the
	// explicit primary memory dir preserves its legacy basename-hashed ID.
	fb := "---\nname: fb\nmetadata:\n  type: feedback\n---\n\n11-10 the user corrected ownership of project-alpha.\n"
	for _, d := range []string{primaryMem, supMem, agentMem} {
		if err := os.WriteFile(filepath.Join(d, "same.md"), []byte(fb), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ws := []collabWindow{{Start: "2026-11-01", End: "2026-11-26"}}
	cPrim, err := collabMemoryCandsInDir(primaryMem, ws, explicitMem[primaryMem])
	if err != nil || len(cPrim) != 1 {
		t.Fatalf("primary mem cands: %v %+v", err, cPrim)
	}
	if want := "m-" + heHash("same.md")[:10]; cPrim[0].ID != want {
		t.Fatalf("primary mem ID = %q, want legacy %q", cPrim[0].ID, want)
	}
	cSup, err := collabMemoryCandsInDir(supMem, ws, explicitMem[supMem])
	cAgt, err2 := collabMemoryCandsInDir(agentMem, ws, explicitMem[agentMem])
	if err != nil || err2 != nil || len(cSup) != 1 || len(cAgt) != 1 {
		t.Fatalf("discovered mem cands: %v %v %+v %+v", err, err2, cSup, cAgt)
	}
	if cSup[0].ID == cAgt[0].ID || cSup[0].ID == cPrim[0].ID || cAgt[0].ID == cPrim[0].ID {
		t.Fatalf("expected distinct IDs across memory dirs, got primary=%s sup=%s agt=%s", cPrim[0].ID, cSup[0].ID, cAgt[0].ID)
	}
	_ = sup
	_ = agent
}

func TestCollabReadStripsPersonaContextAndKeepsLegacyIDs(t *testing.T) {
	dir := t.TempDir()
	jsonl := strings.Join([]string{
		`{"type":"user","timestamp":"2026-11-10T01:00:00Z","message":{"content":[{"type":"text","text":"<persona-context kind=\"context\">meta</persona-context>\ncheck repo-alpha owner"}]}}`,
		`{"type":"assistant","timestamp":"2026-11-10T01:00:05Z","message":{"content":[{"type":"text","text":"I asked user-b about repo-beta."}]}}`,
		`{"type":"user","timestamp":"2026-11-10T01:01:00Z","message":{"content":[{"type":"text","text":"<persona-context kind=\"context\">meta</persona-context>\n你搞错仓库了，不是这个库"}]}}`,
		`{"type":"user","timestamp":"2026-11-10T01:02:00Z","message":{"content":"<system-reminder>谁负责</system-reminder>"}}`,
		`{"type":"assistant","timestamp":"2026-11-10T01:02:05Z","message":{"content":[{"type":"text","text":"checking now"}]}}`,
		`{"type":"user","timestamp":"2026-11-10T01:03:00Z","message":{"content":"你又搞混人了，不是他负责"}}`,
	}, "\n")
	path := filepath.Join(dir, "managed.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := []collabWindow{{Start: "2026-11-01", End: "2026-11-26"}}
	cs, err := collabSessionCandsWindows([]string{dir, dir}, ws, collabEntityCue)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("expected 2 deduplicated entity candidates (1 ctx-stripped + 1 legacy), got %d: %+v", len(cs), cs)
	}
	// Legacy candidate is after two assistant lines (legacyIdx 0 and 1), so its legacyIdx must remain 2 regardless of
	// the two context-wrapped user lines before it.
	wantLegacyID := "s-" + heHash(path + "2")[:10]
	wantCtxID := "s-" + heHash(path + ":ctx:3")[:10]
	gotIDs := map[string]string{}
	for _, c := range cs {
		gotIDs[c.ID] = c.Text
	}
	if _, ok := gotIDs[wantLegacyID]; !ok {
		t.Errorf("legacy ID %q shifted; got IDs %v", wantLegacyID, gotIDs)
	}
	if txt, ok := gotIDs[wantCtxID]; !ok {
		t.Errorf("ctx-stripped candidate %q missing; got IDs %v", wantCtxID, gotIDs)
	} else if !strings.Contains(txt, "check repo-alpha owner") || !strings.Contains(txt, "你搞错仓库了") || strings.Contains(txt, "persona-context") {
		t.Errorf("unexpected ctx-stripped text: %q", txt)
	}
}

func TestSliceStateBeforeAndOverviewCheckpoint(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		"e-old": {
			ID: "e-old", Facet: entry.Interaction, Text: "rule old", Status: entry.Stable,
			First: "2026-10-10", Last: "2026-11-20", Count: 4, Kinds: []string{entry.SrcAssistant, entry.SrcAsk},
			Evidence: []entry.Evidence{
				{Source: entry.SrcAssistant, Ref: "a", Day: "2026-10-10", Quote: "q1"},
				{Source: entry.SrcAssistant, Ref: "b", Day: "2026-11-05", Quote: "q2"},
				{Source: entry.SrcAsk, Ref: "c", Day: "2026-11-20", Quote: "q3"},
			},
		},
		"e-new": {
			ID: "e-new", Facet: entry.Interaction, Text: "rule new", Status: entry.Stable,
			First: "2026-11-15", Last: "2026-11-15", Count: 1,
		},
	}}
	sliced := sliceStateBefore(st, "2026-11-08")
	if sliced.Entries["e-new"] != nil {
		t.Fatalf("entry on/after cutoff leaked into sliced state")
	}
	old := sliced.Entries["e-old"]
	if old == nil || len(old.Evidence) != 2 || old.Last != "2026-11-05" || old.Count != 2 {
		t.Fatalf("unexpected sliced entry: %+v", old)
	}
	// Original state must not be mutated.
	if len(st.Entries["e-old"].Evidence) != 3 || st.Entries["e-old"].Last != "2026-11-20" {
		t.Fatalf("original state was mutated")
	}
	ovs := map[string]*overview.State{
		"2026-11-01": {Refreshed: "11-01"},
		"2026-11-08": {Refreshed: "11-08"},
		"2026-11-25": {Refreshed: "11-25"},
	}
	if got := collabOverviewFor(ovs, "2026-11-01"); got == nil || got.Refreshed != "11-01" {
		t.Fatalf("expected 11-01 checkpoint for 11-01, got %+v", got)
	}
	if got := collabOverviewFor(ovs, "2026-11-14"); got == nil || got.Refreshed != "11-08" {
		t.Fatalf("expected 11-08 checkpoint for 11-14, got %+v", got)
	}
	if got := collabOverviewFor(ovs, "2026-10-31"); got != nil {
		t.Fatalf("expected nil checkpoint before earliest snapshot, got %+v", got)
	}
}

func TestCollabPriorEntitiesAndSessionIntersection(t *testing.T) {
	st := &store.State{
		Contacts: map[string]*entry.Contact{
			"c-alice": {Person: "c-alice", Name: "Alice"},
		},
		Entries: map[string]*entry.Entry{
			"w-proj": {
				ID: "w-proj", Facet: entry.World, Object: "project-apollo",
				Text: "Alice owns project-apollo", Status: entry.Stable, First: "2026-09-20", Last: "2026-09-20", Tagged: true,
			},
			"p-alice": {
				ID: "p-alice", Facet: entry.People, Person: "c-alice",
				Text: "Alice leads release engineering", Status: entry.Stable, First: "2026-09-10", Last: "2026-09-10", Tagged: true,
			},
			"w-late": {
				ID: "w-late", Facet: entry.World, Object: "project-future",
				Text: "project-future created later", Status: entry.Stable, First: "2026-11-15", Last: "2026-11-15", Tagged: true,
			},
		},
	}
	prior := buildCollabPriorEntities(st)
	if !prior.hasPrior("project-apollo", "2026-11-10") || !prior.hasPrior("Alice", "2026-11-10") {
		t.Fatalf("expected prior entities before 2026-11-10")
	}
	if prior.hasPrior("project-future", "2026-11-10") || prior.hasPrior("project-future", "2026-11-15") {
		t.Fatalf("future or same-day entity must not satisfy prior gate")
	}

	dir := t.TempDir()
	jsonl := strings.Join([]string{
		// Turn 1-3: Non-correction technical question mentioning "依赖" (matched old collabEntityCue alone, must be rejected).
		`{"type":"user","timestamp":"2026-11-10T01:00:00Z","message":{"content":"帮我看下构建脚本"}}`,
		`{"type":"assistant","timestamp":"2026-11-10T01:00:05Z","message":{"content":[{"type":"text","text":"好的，正在查看"}]}}`,
		`{"type":"user","timestamp":"2026-11-10T01:01:00Z","message":{"content":"这个模块有哪些依赖需要升级？"}}`,
		// Turn 4-6: Real correction mentioning prior entity "project-apollo" WITHOUT literal collabEntityCue words (must be kept!).
		`{"type":"user","timestamp":"2026-11-10T02:00:00Z","message":{"content":"推进一下 project-apollo 的发布"}}`,
		`{"type":"assistant","timestamp":"2026-11-10T02:00:05Z","message":{"content":[{"type":"text","text":"我先发给 Bob 确认"}]}}`,
		`{"type":"user","timestamp":"2026-11-10T02:01:00Z","message":{"content":"不是，我说的是直接找 Alice"}}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := []collabWindow{{Start: "2026-11-01", End: "2026-11-26"}}
	cs, err := collabSessionCandsEntity([]string{dir}, ws, prior)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || !strings.Contains(cs[0].Text, "Alice") || strings.Contains(cs[0].Text, "有哪些依赖") {
		t.Fatalf("expected only the real correction mentioning prior entity, got %+v", cs)
	}
}

func TestCollabStateCandsAndAntiLeak(t *testing.T) {
	st := &store.State{
		Entries: map[string]*entry.Entry{
			"w-alpha": {
				ID: "w-alpha", Facet: entry.World, Object: "project-alpha",
				Text: "project-alpha is maintained on host-a", Status: entry.Stable, First: "2026-09-15", Last: "2026-09-15", Tagged: true,
			},
			"w-new": {
				ID: "w-new", Facet: entry.World, Object: "project-new",
				Text: "project-new started today", Status: entry.Stable, First: "2026-10-12", Last: "2026-10-12", Tagged: true,
			},
			// Eligible S8 Feishu/KB entity-bound choice in Oct with prior World entity.
			"s8-ok": {
				ID: "s8-ok", Facet: entry.Judgement, Grain: entry.Action, Status: entry.Stable,
				First: "2026-10-12", Last: "2026-10-12", Tagged: true,
				Scopes:    []entry.Scope{{Kind: entry.ScopeProject, Name: "project-alpha"}},
				Situation: "deploying project-alpha patch",
				Choice:    "route deployment to host-a after check",
				Rejected:  "push directly to host-b without check",
				Evidence:  []entry.Evidence{{Source: entry.SrcChat, Ref: "chats/room-1", Day: "2026-10-12", Quote: "project-alpha goes to host-a"}},
			},
			// Same-unit sibling entry that must be blocked by Leak/Units.
			"s8-sib": {
				ID: "s8-sib", Facet: entry.World, Grain: entry.Action, Status: entry.Stable,
				First: "2026-10-10", Last: "2026-10-12", Tagged: true,
				Text:     "sibling fact on same unit",
				Evidence: []entry.Evidence{{Source: entry.SrcChat, Ref: "chats/room-1", Day: "2026-10-12", Quote: "same room"}},
			},
			// Eligible S3 interaction correction in Nov with prior World entity.
			"s3-ok": {
				ID: "s3-ok", Facet: entry.Interaction, Grain: entry.Action, Type: "correction", Status: entry.Stable,
				First: "2026-11-05", Last: "2026-11-05", Tagged: true,
				Scopes:    []entry.Scope{{Kind: entry.ScopeProject, Name: "project-alpha"}},
				Situation: "running gate for project-alpha",
				Choice:    "run full check on remote build host",
				Rejected:  "compile locally on laptop",
				Evidence:  []entry.Evidence{{Source: entry.SrcAgent, Ref: "agents/sess-1", Day: "2026-11-05", Quote: "do not build project-alpha locally"}},
			},
			// Same-day entity (no prior World/People before 2026-10-12): must be rejected.
			"s8-noprior": {
				ID: "s8-noprior", Facet: entry.Judgement, Grain: entry.Action, Status: entry.Stable,
				First: "2026-10-12", Last: "2026-10-12", Tagged: true,
				Scopes:    []entry.Scope{{Kind: entry.ScopeProject, Name: "project-new"}},
				Situation: "starting project-new",
				Choice:    "use template repo layout",
				Rejected:  "copy legacy layout",
				Evidence:  []entry.Evidence{{Source: entry.SrcChat, Ref: "chats/room-2", Day: "2026-10-12", Quote: "use template for project-new"}},
			},
			// Sensitive S8 entry: must be rejected.
			"s8-sens": {
				ID: "s8-sens", Facet: entry.Judgement, Grain: entry.Action, Status: entry.Stable,
				First: "2026-10-12", Last: "2026-10-12", Tagged: true,
				Categories: []entry.Category{{Name: entry.CatFinance}},
				Scopes:     []entry.Scope{{Kind: entry.ScopeProject, Name: "project-alpha"}},
				Situation:  "budget review for project-alpha",
				Choice:     "defer purchase order",
				Rejected:   "approve immediately",
				Evidence:   []entry.Evidence{{Source: entry.SrcChat, Ref: "chats/room-3", Day: "2026-10-12", Quote: "defer budget"}},
			},
		},
	}
	prior := buildCollabPriorEntities(st)
	ws := []collabWindow{{Start: "2026-10-01", End: "2026-11-26"}}
	allowed := map[string]bool{entry.SrcAssistant: true, entry.SrcAsk: true, entry.SrcAgent: true, entry.SrcMemory: true, entry.SrcChat: true}
	cands := collabStateCands(st, prior, ws, allowed)
	if len(cands) != 2 {
		t.Fatalf("expected 2 eligible state candidates (s8-ok, s3-ok), got %d: %+v", len(cands), cands)
	}
	var s8Cand *collabCand
	for i := range cands {
		if cands[i].Source == "state:S8" {
			s8Cand = &cands[i]
		}
	}
	if s8Cand == nil || len(s8Cand.Leak) == 0 || len(s8Cand.Units) == 0 {
		t.Fatalf("expected state:S8 candidate with Leak and Units, got %+v", cands)
	}
	dayF := collabDayFilter([]collabIncident{{ID: s8Cand.ID, Day: s8Cand.Day, Leak: s8Cand.Leak, Units: s8Cand.Units}})
	if !dayF.entryOut(st, st.Entries["s8-ok"], "2026-10-15") {
		t.Error("source entry s8-ok must be excluded by dayF")
	}
	if !dayF.entryOut(st, st.Entries["s8-sib"], "2026-10-15") {
		t.Error("same-unit sibling s8-sib must be excluded by dayF")
	}
	if dayF.entryOut(st, st.Entries["w-alpha"], "2026-10-12") {
		t.Error("prior World entry w-alpha must NOT be excluded by dayF")
	}
}

func TestHasPriorRuleMemoryAndTableSplit(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		"i-prior": {
			ID: "i-prior", Facet: entry.Interaction, Text: "已放行的任务直接执行，严禁再弹等待确认的卡片",
			Status: entry.Stable, First: "2026-10-10", Last: "2026-11-10", Count: 5, Tagged: true, Origin: entry.Handwritten,
		},
		"i-late": {
			ID: "i-late", Facet: entry.Interaction, Text: "多余待办用 cancelled 关掉不弹卡",
			Status: entry.Stable, First: "2026-11-22", Last: "2026-11-22", Count: 1, Tagged: true, Origin: entry.Behavior,
		},
	}}
	recur := collabIncident{ID: "a", Day: "2026-11-16", Want: "已放行的任务直接执行，不要再生成等待确认的卡片"}
	novel := collabIncident{ID: "b", Day: "2026-11-22", Want: "多余待办用 cancelled 关掉不弹卡"}
	if !hasPriorRuleMemory(st, recur) {
		t.Fatalf("expected prior rule memory match for recurrence incident")
	}
	if hasPriorRuleMemory(st, novel) {
		t.Fatalf("same-day entry must not satisfy prior rule memory gate")
	}
	r := collabResult{
		Model: "m", Judge: "j", N: 2, Repeats: 1,
		PriorMemory: []string{"a"},
		Score:       map[string]map[string]map[string]float64{"4096": {"resident": {"avoided": 0.5, "wrong_action": 0.5}}},
		Bytes:       map[string]map[string]int{"4096": {"resident": 4000}},
		PerInc:      map[string]map[string]map[string]float64{"4096": {"resident": {"a": 1, "b": 0}}},
		PerWrong:    map[string]map[string]map[string]float64{"4096": {"resident": {"a": 0, "b": 1}}},
		PerAsk:      map[string]map[string]map[string]float64{"4096": {"resident": {"a": 0, "b": 0}}},
	}
	md := collabTable(r, []string{"resident"})
	if !strings.Contains(md, "#### prior-memory recurrences (1/2 incidents)") {
		t.Fatalf("expected prior-memory recurrences split table, got:\n%s", md)
	}
}

func TestTwoLayerRecallAndHubPage1(t *testing.T) {
	st := &store.State{
		Graph: &store.Graph{
			Nodes: map[string]*store.Node{
				"n-proj":  {ID: "n-proj", Type: "project", Name: "polaris", Status: "active", Created: "2026-10-01"},
				"n-owner": {ID: "n-owner", Type: "person", Name: "Lin", Status: "active", Created: "2026-10-01"},
				"n-later": {ID: "n-later", Type: "person", Name: "FutureOwner", Status: "active", Created: "2026-11-25"},
			},
			Assertions: map[string]*store.Assertion{
				"a-own":   {ID: "a-own", S: "n-owner", P: "owns", O: "n-proj", Status: "active", ValidFrom: "2026-10-02"},
				"a-later": {ID: "a-later", S: "n-later", P: "owns", O: "n-proj", Status: "active", ValidFrom: "2026-11-25"},
			},
		},
		Entries: map[string]*entry.Entry{
			"w-polaris": {
				ID: "w-polaris", Facet: entry.World, Text: "polaris is owned by Lin and hosts longtask code",
				Status: entry.Stable, Count: 3, First: "2026-10-02", Last: "2026-10-10", Tagged: true, Origin: entry.Behavior,
				Object: "polaris",
			},
			"i-polaris": {
				ID: "i-polaris", Facet: entry.Interaction, Text: "polaris 改动合并前必须先跑 sj-wsl 门禁",
				Status: entry.Stable, Count: 3, First: "2026-10-05", Last: "2026-10-12", Tagged: true, Origin: entry.Behavior,
				Scopes: []entry.Scope{{Kind: entry.ScopeProject, Name: "polaris"}},
			},
		},
	}
	ov := &overview.State{Patterns: map[string]*overview.Pattern{
		"ov:tp": {
			ID: "ov:tp", Facet: string(entry.Tools), Kind: overview.KindContract, Lifecycle: overview.Core,
			Text: "用 task_plan 派活时填全 cwd、launcher、model 并写明理由。", ScenesCount: 2, SourcesCount: 2, LastEvidence: "2026-11-10",
		},
		"ov:future": {
			ID: "ov:future", Facet: string(entry.Tools), Kind: overview.KindContract, Lifecycle: overview.Core,
			Text: "未来才出现的 task_plan 规则不能泄漏。", ScenesCount: 2, SourcesCount: 2, LastEvidence: "2026-11-25",
		},
	}}
	f := heFilter{leak: map[string]bool{}, units: map[string]bool{}}
	// 1. collabHubPage1 uses real overview.Exits + enriched subjects, excluding post-cutoff patterns.
	page := collabHubPage1(st, ov, f, "2026-11-20", 4096)
	dq := hubDecisionsQuery(page, []string{"task_plan"}, decisionsQueryMaxBytes)
	if !strings.Contains(dq, "填全 cwd") {
		t.Fatalf("expected non-standing L1 contract via enriched task_plan tag, got %q", dq)
	}
	if strings.Contains(dq, "未来才出现") {
		t.Fatalf("post-cutoff L1 pattern leaked into hubDecisionsQuery: %q", dq)
	}
	// 2. recallTwoLayer brings in non-standing L1 + Block 2 1-hop skeleton edges (time-clean), and entities=false ablates skeleton.
	idx := collabNames(st)
	sit := "准备用 task_plan 给 polaris 派一个功能开发任务。"
	withEnt := idx.recallTwoLayer(st, ov, f, "2026-11-20", sit, "", 2048, true)
	if !strings.Contains(withEnt, "填全 cwd") {
		t.Errorf("recallTwoLayer missing non-standing L1 rule: %q", withEnt)
	}
	if !strings.Contains(withEnt, "[实体骨架]") || !strings.Contains(withEnt, "Lin") {
		t.Errorf("recallTwoLayer missing Block 2 1-hop skeleton edge: %q", withEnt)
	}
	if strings.Contains(withEnt, "FutureOwner") || strings.Contains(withEnt, "未来才出现") {
		t.Errorf("recallTwoLayer leaked post-cutoff graph/L1 data: %q", withEnt)
	}
	noEnt := idx.recallTwoLayer(st, ov, f, "2026-11-20", sit, "", 2048, false)
	if !strings.Contains(noEnt, "填全 cwd") {
		t.Errorf("recall-noent should keep non-standing L1 rule: %q", noEnt)
	}
	if strings.Contains(noEnt, "[实体骨架]") || strings.Contains(noEnt, "FutureOwner") {
		t.Errorf("recall-noent must ablate entity skeleton: %q", noEnt)
	}
	// Whole-word ASCII guard prevents "Launch" from matching "launcher" and "ST" from matching "astergate",
	// and ephemeral PR ticket lines are excluded from the entity supplement slot.
	if collabContainsEntity("填全 launcher 参数", "Launch") {
		t.Error("Launch must not substring-match launcher")
	}
	if collabContainsEntity("检查 astergate 配置", "ST") {
		t.Error("short ASCII ST must not match astergate")
	}
	if collabContainsEntity("重写飞书文档", "文档") {
		t.Error("generic common noun 文档 must not trigger entity expansion")
	}
	if !collabContainsEntity("检查 astergate 配置", "astergate") {
		t.Error("exact whole-word entity astergate should match")
	}
	if !collabEphemeralFact.MatchString("Tivility负责PR #13346") {
		t.Error("ephemeral PR ticket fact should be filtered")
	}
}

// sliceStateBefore must downgrade Stable->Candidate only when post-cutoff evidence was actually stripped,
// never falsely downgrading pre-cutoff single-evidence Stable entries (which would evict entity-anchored rules
// from collabHubPage1).
func TestSliceStateBeforeStatusDowngradePrecision(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		"pre-stable": {
			ID: "pre-stable", Facet: entry.Tools, Origin: entry.Behavior, Status: entry.Stable,
			Text: "task_plan 必须带 role 字段", Count: 1, First: "2026-11-10", Last: "2026-11-10",
			Tagged: true, Object: "task_plan",
			Evidence: []entry.Evidence{{Day: "2026-11-10", Source: "S3", Quote: "role required"}},
		},
		"post-promoted": {
			ID: "post-promoted", Facet: entry.Tools, Origin: entry.Behavior, Status: entry.Stable,
			Text: "事后新证据才升格的条目", Count: 2, First: "2026-11-10", Last: "2026-11-25",
			Tagged: true, Object: "task_plan",
			Evidence: []entry.Evidence{
				{Day: "2026-11-10", Source: "S3", Quote: "first"},
				{Day: "2026-11-25", Source: "S3", Quote: "second"},
			},
		},
		"hw-post-ev": {
			ID: "hw-post-ev", Facet: entry.Interaction, Origin: entry.SelfReport, Status: entry.Stable,
			Text: "用户自述规矩不被降级", Count: 2, First: "2026-11-10", Last: "2026-11-25",
			Tagged: true,
			Evidence: []entry.Evidence{
				{Day: "2026-11-10", Source: "S7", Quote: "first"},
				{Day: "2026-11-25", Source: "S7", Quote: "second"},
			},
		},
	}}
	sliced := sliceStateBefore(st, "2026-11-20")
	if got := sliced.Entries["pre-stable"].Status; got != entry.Stable {
		t.Errorf("pre-cutoff Stable entry with no stripped evidence falsely downgraded to %q", got)
	}
	if got := sliced.Entries["post-promoted"].Status; got != entry.Candidate {
		t.Errorf("post-cutoff promoted non-owned entry should downgrade to Candidate, got %q", got)
	}
	if got := sliced.Entries["hw-post-ev"].Status; got != entry.Stable {
		t.Errorf("handwritten entry must stay Stable, got %q", got)
	}
	// Verify pre-stable keeps its active rank >= 750 in collabHubPage1.
	page := collabHubPage1(sliced, nil, heFilter{leak: map[string]bool{}, units: map[string]bool{}}, "2026-11-20", 4096)
	dq := hubDecisionsQuery(page, []string{"task_plan"}, decisionsQueryMaxBytes)
	if !strings.Contains(dq, "pre-stable") {
		t.Errorf("pre-stable entity-anchored rule missing from collabHubPage1 decisions_query: %q", dq)
	}
}

// Verify heMemories uses L1 patterns without old S6/UserOwned pass-through suppression, and recallNonStandingL1
// retrieves non-standing user-owned rules.
func TestHeMemoriesAndRecallUserOwnedVerbatim(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		"hw-stand": {
			ID: "hw-stand", Facet: entry.Interaction, Origin: entry.SelfReport, Status: entry.Stable,
			Text: "已放行运维活直接自主执行，严禁弹确认卡。", Count: 20, Kinds: []string{"S1", "S7"},
			First: "2026-10-01", Last: "2026-11-15", Tagged: true,
		},
		"hw-recall": {
			ID: "hw-recall", Facet: entry.Tools, Origin: entry.SelfReport, Status: entry.Stable,
			Text: "用 task_plan 派活时按要求填全 cwd 与 model 字段。", Count: 1, Kinds: []string{"S7"},
			First: "2026-10-05", Last: "2026-10-05", Tagged: true,
		},
	}}
	ov := &overview.State{Patterns: map[string]*overview.Pattern{
		"ov:wrap": {
			ID: "ov:wrap", Facet: string(entry.Interaction), Kind: overview.KindContract, Lifecycle: overview.Core,
			Text: "已放行的常规任务直接自主推进，不弹确认卡。", CoveredIDs: []string{"hw-stand"}, EvidenceIDs: []string{"hw-stand"},
			Hard: true, Polarity: overview.PolarityAct, ScenesCount: 3, SourcesCount: 5, LastEvidence: "2026-11-15",
		},
		"ov:pure": {
			ID: "ov:pure", Facet: string(entry.Interaction), Kind: overview.KindContract, Lifecycle: overview.Core,
			Text:        "发消息给用户必须调用直接投递工具，不要只存本地路径。",
			ScenesCount: 3, SourcesCount: 10, LastEvidence: "2026-11-15",
		},
	}}
	f := heFilter{leak: map[string]bool{}, units: map[string]bool{}}
	mems := heMemories(st, ov, nil, f, "2026-11-20", 4096)
	res := mems["resident"]
	if !strings.Contains(res, "已放行的常规任务直接自主推进") {
		t.Errorf("resident missing L1 contract: %s", res)
	}
	if !strings.Contains(res, "直接投递工具") {
		t.Errorf("resident missing second L1 contract: %s", res)
	}
	// Non-standing user-owned rule is retrieved by recallNonStandingL1.
	lines := recallNonStandingL1(st, ov, f, "2026-11-20", "准备用 task_plan 派一个开发任务", "- 已放行运维活直接自主执行", 850)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "填全 cwd 与 model 字段") {
		t.Errorf("recallNonStandingL1 missing non-standing user-owned rule: %q", joined)
	}
}

// Verify recallTwoLayer eliminates cross-pass near-duplicates, places persistent World/People entity facts ahead of
// 1-hop ec.Rules (preventing secondary-entity rules from evicting core ownership facts), backfills unused entity-slot
// room on non-entity scenes, and rejects loose single-occasion-tag spam.
func TestRecallTwoLayerDedupAndEntityFactPriority(t *testing.T) {
	st := &store.State{
		Graph: &store.Graph{
			Nodes: map[string]*store.Node{
				"n-ag":  {ID: "n-ag", Type: "project", Name: "astergate", Status: "active", Created: "2026-10-01"},
				"n-pg":  {ID: "n-pg", Type: "system", Name: "PG", Status: "active", Created: "2026-10-01"},
				"n-red": {ID: "n-red", Type: "system", Name: "Redis", Status: "active", Created: "2026-10-01"},
				"n-cpa": {ID: "n-cpa", Type: "system", Name: "cpa", Status: "active", Created: "2026-10-01"},
			},
			Assertions: map[string]*store.Assertion{
				"a1": {ID: "a1", S: "n-ag", P: "depends_on", O: "n-pg", Status: "active", ValidFrom: "2026-10-01"},
				"a2": {ID: "a2", S: "n-ag", P: "depends_on", O: "n-red", Status: "active", ValidFrom: "2026-10-01"},
				"a3": {ID: "a3", S: "n-ag", P: "depends_on", O: "n-cpa", Status: "active", ValidFrom: "2026-10-01"},
			},
		},
		Entries: map[string]*entry.Entry{
			"hw-dup1": {
				ID: "hw-dup1", Facet: entry.Workflow, Origin: entry.Handwritten, Status: entry.Stable,
				Text: "派发 Agent 任务时一律配置 bypassPermissions，不向用户提示权限申请。", Count: 4, Kinds: []string{"S6"},
				First: "2026-10-10", Last: "2026-11-10", Tagged: true,
			},
			"i-dup2": {
				ID: "i-dup2", Facet: entry.Interaction, Origin: entry.Behavior, Status: entry.Stable,
				Text: "派发 Agent 任务时一律配置 bypassPermissions，不向用户提示权限确认。", Count: 3, Kinds: []string{"S3"},
				First: "2026-10-12", Last: "2026-11-12", Tagged: true, Object: "astergate",
			},
			"i-ag-rule1": {
				ID: "i-ag-rule1", Facet: entry.Tools, Origin: entry.Behavior, Status: entry.Stable,
				Text: "在门控脚本中判断 astergate 会话状态时仅使用本机文件时间戳，禁止调用 ops/sess.py 或读取 hub.json 接口，避免触发敏感凭据拦截。", Count: 3,
				First: "2026-10-10", Last: "2026-11-10", Tagged: true, Object: "astergate",
			},
			"i-ag-rule2": {
				ID: "i-ag-rule2", Facet: entry.Tools, Origin: entry.Behavior, Status: entry.Stable,
				Text: "排查 astergate 网关限流报错时优先检查上游配额与路由健康状态，不要盲目重启容器或反复重试，并把排查日志整理到报告目录。", Count: 3,
				First: "2026-10-10", Last: "2026-11-10", Tagged: true, Object: "astergate",
			},
			"w-repo": {
				ID: "w-repo", Facet: entry.World, Origin: entry.Behavior, Status: entry.Stable,
				Text: "repo-internal-shared 是团队共用私有仓库，个人扩展组件不应混入其中，需在个人名下建专属私有仓库。", Count: 3,
				First: "2026-10-10", Last: "2026-11-10", Tagged: true, Object: "repo-internal-shared",
			},
			"hw-unrelated": {
				ID: "hw-unrelated", Facet: entry.Tools, Origin: entry.Handwritten, Status: entry.Stable,
				Text: "运行升级任务时使用标准启动器，不要切换实验分支。", Count: 2, Kinds: []string{"S6"},
				First: "2026-10-10", Last: "2026-11-10", Tagged: true,
			},
		},
	}
	f := heFilter{leak: map[string]bool{}, units: map[string]bool{}}
	idx := collabNames(st)
	sit := "用户问要不要把个人组件放进 repo-internal-shared（里面已有 astergate 扩展），需要派发 Agent 调研。"
	// Tight budget (760 B) where ec.Rules would previously evict w-repo.
	got := idx.recallTwoLayer(st, nil, f, "2026-11-20", sit, "", 760, true)
	if strings.Count(got, "bypassPermissions") > 1 {
		t.Errorf("expected cross-pass semantic dedup to keep at most 1 bypassPermissions line, got:\n%s", got)
	}
	if !strings.Contains(got, "团队共用私有仓库") {
		t.Errorf("persistent World ownership fact w-repo was evicted by ec.Rules:\n%s", got)
	}
	if strings.Contains(got, "不要切换实验分支") {
		t.Errorf("unrelated single-occasion rule should be filtered by >=5.0 score gate:\n%s", got)
	}
	// Non-entity scene: unused entity slot flows back to non-entity rules so recall matches recall-noent.
	nonEntSit := "需要派发一个普通任务并配置权限。"
	rEnt := idx.recallTwoLayer(st, nil, f, "2026-11-20", nonEntSit, "", 1024, true)
	rNoEnt := idx.recallTwoLayer(st, nil, f, "2026-11-20", nonEntSit, "", 1024, false)
	if rEnt != rNoEnt {
		t.Errorf("on pure non-entity scene, recall should match recall-noent:\nent=%q\nnoent=%q", rEnt, rNoEnt)
	}
}
