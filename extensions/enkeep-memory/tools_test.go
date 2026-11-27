package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/curate"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/kbsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/schema"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/stub"
)

func toolApp(t *testing.T) (*app, *store.Store) {
	t.Helper()
	fx, err := kbsrc.LoadFixture("testdata/sample")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := curate.Defaults()
	cfg.Mode, cfg.AdmitAllBelowTokens, cfg.BackfillDays = "shadow", 0, 0
	now := time.Date(2026, 11, 9, 4, 0, 0, 0, time.UTC)
	a := newApp()
	a.c = &curate.Curator{Src: fx, Model: &stub.Model{}, St: st, Cfg: cfg, Loc: time.UTC, Now: func() time.Time { return now }}
	return a, st
}

// call runs a read tool with tools.call, a write with actions.do (as the hub does once its gate let it through).
func call(t *testing.T, a *app, name string, args any) (string, error) {
	t.Helper()
	method, params := "tools.call", map[string]any{"contribution": connectorID, "name": name, "args": args}
	if listed(writeTools, name) {
		method, params["request_id"] = "actions.do", fmt.Sprintf("test-%d", time.Now().UnixNano())
	}
	raw, _ := json.Marshal(params)
	out, err := a.connector(t.Context(), method, raw)
	if err != nil {
		return "", err
	}
	return out.(map[string]any)["text"].(string), nil
}

// The connector lists five read tools and five writes; the rerun tool queues the days once, refuses days not over and more than 31, and the
// status tool says what is done and what waits.
func TestRerunTool(t *testing.T) {
	a, st := toolApp(t)
	list, err := a.connector(t.Context(), "tools.list", nil)
	if err != nil || len(list.(map[string]any)["tools"].([]tool)) != 5 {
		t.Fatalf("tools.list: %v %v", list, err)
	}
	if list, err = a.connector(t.Context(), "actions.list", nil); err != nil || len(list.(map[string]any)["tools"].([]tool)) != 5 {
		t.Fatalf("actions.list: %v %v", list, err)
	}
	st.S.Reruns = []string{"2026-11-03@" + schema.DailyPrompt} // done with the current prompt already
	text, err := call(t, a, "enkeep_rerun", map[string]string{"from": "2026-11-02", "to": "2026-11-04"})
	if err != nil || !strings.Contains(text, "Queued 2 day(s)") || !strings.Contains(text, "skipped: 2026-11-03") {
		t.Fatalf("rerun: %q %v", text, err)
	}
	if !slices.Equal(st.S.RerunQueue, []string{"2026-11-02", "2026-11-04"}) {
		t.Fatalf("queue: %v", st.S.RerunQueue)
	}
	if text, _ := call(t, a, "enkeep_rerun", map[string]string{"from": "2026-11-02", "to": "2026-11-02"}); !strings.Contains(text, "Queued 0") {
		t.Fatalf("asked twice: %q", text)
	}
	for _, bad := range []map[string]string{
		{"from": "2026-11-08", "to": "2026-11-09"},           // today is not over
		{"from": "2026-09-01", "to": "2026-10-15"},           // more than 31 days
		{"from": "2026-11-04", "to": "2026-11-02"},           // backwards
		{"from": "yesterday", "to": "2026-11-02"},            // not a day
		{"from": "2026-11-01", "to": "2026-11-02", "x": "y"}, // unknown field
	} {
		if _, err := call(t, a, "enkeep_rerun", bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	status, err := call(t, a, "enkeep_rerun_status", map[string]any{})
	if err != nil || !strings.Contains(status, "0 day(s) done, 2 waiting") || !strings.Contains(status, "2026-11-02, 2026-11-04") {
		t.Fatalf("status: %q %v", status, err)
	}
	if !strings.Contains(status, "Model route: off") {
		t.Fatalf("status without a route: %q", status)
	}
	a.route = curate.Defaults().Route()
	a.route.Stats = model.OpenStats(t.TempDir(), time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC))
	status, _ = call(t, a, "enkeep_rerun_status", map[string]any{})
	for _, w := range []string{"Model route: no primary model; fallback above 16000 estimated tokens claude-sonnet-5-5, else claude-haiku-5-5.",
		"Model calls since 2026-11-18T00:00:00Z: 0, answered by a fallback 0 (0.0%)",
		// design v2 §4.7: the cost lines, the call limit and the reruns' progress.
		"Cost 2026-11-09: 0 call(s)", "Reruns since", "Call limit: at most 4 at once and 20 a minute",
		"Rerun progress: 0 day(s) done, 2 left, 0 new usable"} {
		if !strings.Contains(status, w) {
			t.Fatalf("status lacks %q: %q", w, status)
		}
	}
}

// Days the tool queued run like rerun_range's, once each, and the status says they are done.
func TestRerunToolDaysRun(t *testing.T) {
	a, st := toolApp(t)
	if _, err := call(t, a, "enkeep_rerun", map[string]string{"from": "2026-11-03", "to": "2026-11-04"}); err != nil {
		t.Fatal(err)
	}
	settle := func() {
		for i := 0; i < 600; i++ {
			a.mu.Lock()
			idle := a.running == ""
			a.mu.Unlock()
			if idle {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("a run never ended")
	}
	for i := 0; i < 8; i++ {
		a.tick(t.Context())
		settle()
	}
	runs := 0
	for _, r := range st.S.Runs {
		if r.Kind == "daily" && (r.Period == "2026-11-03" || r.Period == "2026-11-04") {
			runs++
		}
	}
	if runs != 2 {
		t.Errorf("each queued day once, got %d runs", runs)
	}
	// Once they are digested and filed they leave the queue; their markers stay, so asking again queues nothing.
	if len(st.S.RerunQueue) != 0 {
		t.Errorf("queue after the reruns: %v", st.S.RerunQueue)
	}
	status, _ := call(t, a, "enkeep_rerun_status", map[string]any{})
	if !strings.Contains(status, "0 day(s) done, 0 waiting") {
		t.Errorf("status: %q", status)
	}
	if text, _ := call(t, a, "enkeep_rerun", map[string]string{"from": "2026-11-03", "to": "2026-11-04"}); !strings.Contains(text, "Queued 0") {
		t.Errorf("asked again: %q", text)
	}
}

// enkeep_resettle_preview reports a dry run, enkeep_resettle (a write) applies, both refuse unknown arguments.
func TestResettleTool(t *testing.T) {
	a, st := toolApp(t)
	st.S.ActiveDays = []string{"2026-11-01", "2026-11-02"}
	st.S.Entries = map[string]*entry.Entry{"e": {ID: "e", Grain: entry.Action, Facet: entry.Workflow, Origin: entry.Behavior,
		Status: entry.Dormant, First: "2026-11-01", Last: "2026-11-02",
		Evidence: []entry.Evidence{{Ref: "chats/a", Day: "2026-11-01"}, {Ref: "chats/b", Day: "2026-11-02"}}}}
	text, err := call(t, a, "enkeep_resettle_preview", map[string]any{})
	if err != nil || !strings.Contains(text, "Dry run as of 2026-11-09") || !strings.Contains(text, "dormant 1 → 0") ||
		!strings.Contains(text, "workflow 0 → 1") || !strings.Contains(text, "dormant→stable 1") {
		t.Fatalf("dry run: %q %v", text, err)
	}
	if st.S.Entries["e"].Status != entry.Dormant {
		t.Fatal("the dry run changed the entry")
	}
	if text, err = call(t, a, "enkeep_resettle", map[string]any{}); err != nil || !strings.Contains(text, "Applied") ||
		st.S.Entries["e"].Status != entry.Stable {
		t.Fatalf("apply: %q %v", text, err)
	}
	if text, _ = call(t, a, "enkeep_resettle", map[string]any{}); !strings.Contains(text, "0 entr(ies) changed") {
		t.Fatalf("again: %q", text)
	}
	if _, err := call(t, a, "enkeep_resettle", map[string]any{"dry_run": true}); err == nil {
		t.Fatal("accepted an unknown argument")
	}
	if _, err := call(t, a, "enkeep_resettle_preview", map[string]any{"dry_run": false}); err == nil {
		t.Fatal("the preview accepted an unknown argument")
	}
}

// The program says it has the manifest's connector, so the hub does not fall back to the manifest with a warning.
func TestInitDeclaresTheConnector(t *testing.T) {
	var got []map[string]any
	if err := json.Unmarshal(manifestConnectors(), &got); err != nil || len(got) != 1 || got[0]["id"] != connectorID {
		t.Fatalf("connectors: %s %v", manifestConnectors(), err)
	}
	caps, _ := got[0]["capabilities"].([]any)
	if !slices.Contains(caps, any("tools")) || !slices.Contains(caps, any("actions")) {
		t.Fatalf("capabilities: %v", caps)
	}
}

// The hub's own connector checks (tool names start with the connector's id and "_", each has a description and an
// object schema, status answers): what keeps the tools on the assistant's list.
func TestConnectorConformance(t *testing.T) {
	a, _ := toolApp(t)
	// The connector side only: the full program's Init starts the passes, which would still write to the test's
	// directories when it is cleaned up.
	p := program(a)
	p.Init = func(context.Context, *ext.Init, *ext.Hub) (map[string]any, error) {
		return map[string]any{"connectors": manifestConnectors()}, nil
	}
	exttest.CheckConnectors(t, manifest, exttest.Serve(t, p))
}

// force queues a day already read with the current prompt; it runs again once, then leaves the force list; the status
// says which days wait with force.
func TestRerunToolForce(t *testing.T) {
	a, st := toolApp(t)
	st.S.Reruns = []string{"2026-11-03@" + schema.DailyPrompt}
	if text, _ := call(t, a, "enkeep_rerun", map[string]string{"from": "2026-11-03", "to": "2026-11-03"}); !strings.Contains(text, "Queued 0") {
		t.Fatalf("without force: %q", text)
	}
	text, err := call(t, a, "enkeep_rerun", map[string]any{"from": "2026-11-03", "to": "2026-11-03", "force": true})
	if err != nil || !strings.Contains(text, "Queued 1 day(s) with force") || !slices.Equal(st.S.ForceReruns, []string{"2026-11-03"}) {
		t.Fatalf("force: %q %v %v", text, err, st.S.ForceReruns)
	}
	if text, _ := call(t, a, "enkeep_rerun", map[string]any{"from": "2026-11-03", "to": "2026-11-03", "force": true}); !strings.Contains(text, "Already queued with force") {
		t.Fatalf("force twice: %q", text)
	}
	if _, err := call(t, a, "enkeep_rerun", map[string]any{"from": "2026-11-09", "to": "2026-11-09", "force": true}); err == nil {
		t.Fatal("force accepted a day that is not over")
	}
	status, _ := call(t, a, "enkeep_rerun_status", map[string]any{})
	if !strings.Contains(status, "Queued with force (read again although already read with this prompt), waiting: 2026-11-03.") {
		t.Fatalf("status: %q", status)
	}
	for i := 0; i < 8 && len(st.S.ForceReruns) > 0; i++ {
		a.tick(t.Context())
		for j := 0; j < 600; j++ {
			a.mu.Lock()
			idle := a.running == ""
			a.mu.Unlock()
			if idle {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	runs := 0
	for _, r := range st.S.Runs {
		if r.Kind == "daily" && r.Period == "2026-11-03" {
			runs++
		}
	}
	if runs != 1 || len(st.S.ForceReruns) != 0 {
		t.Fatalf("forced day: %d run(s), force list %v", runs, st.S.ForceReruns)
	}
}

// enkeep_mark_wrong (a write) takes an entry out by id, as the context or decisions_query names it
// (memory-extension:<id>), recorded as the assistant's.
func TestMarkWrongTool(t *testing.T) {
	a, st := toolApp(t)
	add := func(id string, f entry.Facet, text string) *entry.Entry {
		e := &entry.Entry{ID: id, Facet: f, Grain: entry.Class, Text: text, Status: entry.Stable, Origin: entry.Behavior,
			Count: 2, Kinds: []string{"S3"}, Last: "2026-11-08"}
		st.S.Entries[id] = e
		return e
	}
	if st.S.Entries == nil {
		st.S.Entries = map[string]*entry.Entry{}
	}
	add("i-0000000001", entry.Interaction, "部署验证时只回复 OK（占位错条目）")
	add("i-0000000002", entry.Interaction, "第二条占位错条目")
	text, err := call(t, a, "enkeep_mark_wrong", map[string]any{"ids": []string{"id:i-0000000001", "memory-extension:i-0000000002", "i-missing"}, "reason": "测试提示，不是用户的规矩"})
	if err != nil || !strings.Contains(text, "Marked wrong: 2") || !strings.Contains(text, "Not found: i-missing") {
		t.Fatalf("mark_wrong: %q %v", text, err)
	}
	e := st.S.Entries["i-0000000001"]
	if e.Live() || e.Verdicts[len(e.Verdicts)-1].By != "assistant" || e.Verdicts[len(e.Verdicts)-1].Note == "" || st.S.Entries["i-0000000002"].Live() {
		t.Fatalf("entry after mark_wrong: %+v", e.Verdicts)
	}
	if _, err := call(t, a, "enkeep_mark_wrong", map[string]any{"ids": []string{"i-0000000001"}}); err == nil {
		t.Fatal("a mark without a reason was taken")
	}
}

// The split the hub reads (Persona #13586): reads in tools.list and only there, writes in actions.list and only there,
// no recall tool; tools.call refuses a write, actions.do and actions.describe a read; each write's card is described
// from checked arguments without the store's lock, and an argument error refuses it before any card; a repeated
// request id runs the write once; agent_tools names every read; the manifest declares memory_sources and actions.
func TestToolSplit(t *testing.T) {
	a, st := toolApp(t)
	names := map[string]bool{}
	for _, tl := range allTools() {
		if names[tl.Name] || !strings.HasPrefix(tl.Name, connectorID+"_") || tl.Description == "" || tl.InputSchema["type"] != "object" {
			t.Errorf("tool %q: repeated, unprefixed or undescribed", tl.Name)
		}
		names[tl.Name] = true
	}
	if names["enkeep_recall"] {
		t.Fatal("a recall tool is still offered: the entries go through memory.entries only")
	}
	for _, w := range []string{"enkeep_rerun", "enkeep_resettle", "enkeep_seed_import", "enkeep_mark_wrong"} {
		if !listed(writeTools, w) || listed(readTools, w) {
			t.Errorf("%s is not a write only", w)
		}
	}
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	if _, err := a.connector(t.Context(), "tools.call", raw(map[string]any{"contribution": connectorID, "name": "enkeep_rerun",
		"args": map[string]string{"from": "2026-11-02", "to": "2026-11-02"}})); err == nil || len(st.S.RerunQueue) != 0 {
		t.Fatalf("tools.call ran a write: %v %v", err, st.S.RerunQueue)
	}
	if _, err := a.connector(t.Context(), "actions.do", raw(map[string]any{"contribution": connectorID, "name": "enkeep_rerun_status", "request_id": "r"})); err == nil {
		t.Fatal("actions.do ran a read")
	}
	if _, err := a.connector(t.Context(), "actions.describe", raw(map[string]any{"contribution": connectorID, "name": "enkeep_seed_status"})); err == nil {
		t.Fatal("actions.describe described a read")
	}
	st.S.Entries = map[string]*entry.Entry{"i-0000000001": {ID: "i-0000000001", Facet: entry.Interaction, Grain: entry.Class,
		Text: "部署验证时只回复 OK（占位错条目）", Status: entry.Stable, Origin: entry.Behavior, Count: 2, Last: "2026-11-08"}}
	a.c.Live = &curate.Live{}
	a.c.Publish()
	st.Lock() // a pass holds the lock: the cards are described all the same
	defer st.Unlock()
	describe := func(name string, args any) (string, error) {
		t.Helper()
		out, err := a.connector(t.Context(), "actions.describe", raw(map[string]any{"contribution": connectorID, "name": name, "args": args}))
		if err != nil {
			return "", err
		}
		m := out.(map[string]any)
		if act := m["action"].(map[string]any); act["name"] != name || act["visible"] == "" {
			t.Errorf("%s: action %v", name, act)
		}
		return m["summary"].(string), nil
	}
	for name, args := range map[string]any{
		"enkeep_rerun":       map[string]any{"from": "2026-11-01", "to": "2026-11-03", "force": true},
		"enkeep_resettle":    map[string]any{},
		"enkeep_seed_import": map[string]any{"source": "notes", "items": []map[string]any{{"id": "s1", "kind": "rule", "text": "先给结论再给细节", "confirmed": "2026-11-01"}}},
		"enkeep_mark_wrong":  map[string]any{"ids": []string{"memory-extension:i-0000000001"}, "reason": "测试提示"},
	} {
		text, err := describe(name, args)
		if err != nil || !strings.HasPrefix(text, "enkeep 记忆：") {
			t.Errorf("%s: %q %v", name, text, err)
		}
		switch name {
		case "enkeep_rerun":
			if !strings.Contains(text, "共 3 天") || !strings.Contains(text, "包括已经用当前提示词读过的天") {
				t.Errorf("rerun card: %q", text)
			}
		case "enkeep_seed_import":
			if !strings.Contains(text, "导入 1 条种子") || !strings.Contains(text, "先给结论再给细节") {
				t.Errorf("seed card: %q", text)
			}
		case "enkeep_mark_wrong":
			if !strings.Contains(text, "i-0000000001：部署验证时只回复 OK") || !strings.Contains(text, "理由：测试提示") {
				t.Errorf("mark card: %q", text)
			}
		}
	}
	for name, args := range map[string]any{
		"enkeep_rerun":       map[string]any{"from": "2026-11-01"},
		"enkeep_resettle":    map[string]any{"dry_run": false},
		"enkeep_seed_import": map[string]any{"source": "notes"},
		"enkeep_mark_wrong":  map[string]any{"ids": []string{}},
	} {
		if _, err := describe(name, args); err == nil {
			t.Errorf("%s: a card for bad arguments %v", name, args)
		}
	}

	runs := 0
	for i := 0; i < 3; i++ {
		_, _ = a.doOnce("same", func() (string, error) { runs++; return "done", nil })
	}
	if text, _ := a.doOnce("same", nil); runs != 1 || text != "done" {
		t.Fatalf("one request id ran %d time(s)", runs)
	}

	var m struct {
		Contributes struct {
			MemorySources []map[string]any `json:"memory_sources"`
			Connectors    []struct {
				AgentTools []string `json:"agent_tools"`
			} `json:"connectors"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil || len(m.Contributes.MemorySources) != 1 || len(m.Contributes.Connectors) != 1 {
		t.Fatalf("manifest: %v", err)
	}
	agents := m.Contributes.Connectors[0].AgentTools
	for _, r := range readTools {
		if !slices.Contains(agents, r.Name) {
			t.Errorf("agent_tools lacks the read %s", r.Name)
		}
	}
	for _, n := range agents {
		if !names[n] {
			t.Errorf("agent_tools names %s, which is no tool", n)
		}
	}
}

// While a pass holds the store's lock (a daily run holds it for tens of minutes), the tools answer at once: the status
// from the pages' snapshot, mark_wrong and rerun queued and applied when the lock frees, resettle "busy".
func TestToolsWhileAPassRuns(t *testing.T) {
	a, st := toolApp(t)
	toolWait = 100 * time.Millisecond
	t.Cleanup(func() { toolWait = 5 * time.Second })
	a.c.Live = &curate.Live{}
	st.S.Entries = map[string]*entry.Entry{"i-0000000001": {ID: "i-0000000001", Facet: entry.Interaction, Grain: entry.Class,
		Text: "部署验证时只回复 OK（占位错条目）", Status: entry.Stable, Origin: entry.Behavior, Count: 2, Kinds: []string{"S3"}, Last: "2026-11-08"}}
	st.S.RerunQueue = []string{"2026-11-01"}
	st.Lock()
	a.c.Publish()
	// the pass starts: it holds the lock from here
	a.mu.Lock()
	a.running = "daily 2026-11-08"
	a.mu.Unlock()

	quick := func(name string, args any) (string, error) {
		t.Helper()
		start := time.Now()
		text, err := call(t, a, name, args)
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("%s waited %v for the run", name, d)
		}
		return text, err
	}
	text, err := quick("enkeep_rerun_status", map[string]any{})
	if err != nil || !strings.Contains(text, "1 waiting") || !strings.Contains(text, "Running now: daily 2026-11-08") || !strings.Contains(text, "State as of") {
		t.Fatalf("status during a run: %q %v", text, err)
	}
	// Invalid rerun arguments still fail immediately even while busy.
	if _, err = quick("enkeep_rerun", map[string]any{"from": "2026-11-08", "to": "2026-11-09"}); err == nil {
		t.Fatal("rerun accepted today while busy")
	}
	// Valid rerun (normal and force) returns Queued instead of busy, persists reruns-queue.json, and shows in status.
	text, err = quick("enkeep_rerun", map[string]any{"from": "2026-11-02", "to": "2026-11-03"})
	if err != nil || !strings.Contains(text, "Queued 2 day(s)") || !strings.Contains(text, "daily 2026-11-08 is running") {
		t.Fatalf("rerun during a run: %q %v", text, err)
	}
	text, err = quick("enkeep_rerun", map[string]any{"from": "2026-11-05", "to": "2026-11-06", "force": true})
	if err != nil || !strings.Contains(text, "Queued 2 day(s) with force") || !strings.Contains(text, "daily 2026-11-08 is running") {
		t.Fatalf("force rerun during a run: %q %v", text, err)
	}
	status, err := quick("enkeep_rerun_status", map[string]any{})
	if err != nil || !strings.Contains(status, "Pending queue (joins after current run): 2026-11-02, 2026-11-03") ||
		!strings.Contains(status, "Pending force queue (joins at front after current run): 2026-11-05, 2026-11-06") {
		t.Fatalf("status with pending reruns: %q %v", status, err)
	}
	if _, err = quick("enkeep_resettle_preview", map[string]any{}); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("resettle during a run: %v", err)
	}
	text, err = quick("enkeep_mark_wrong", map[string]any{"ids": []string{"i-0000000001", "i-missing"}, "reason": "测试提示，不是用户的规矩"})
	if err != nil || !strings.Contains(text, "Queued") || !strings.Contains(text, "i-missing") {
		t.Fatalf("mark_wrong during a run: %q %v", text, err)
	}
	if !st.S.Entries["i-0000000001"].Live() {
		t.Fatal("marked while the run held the lock")
	}
	// the pass ends: both queued marks and queued reruns are applied without disturbing already-queued days
	a.mu.Lock()
	a.running = ""
	a.mu.Unlock()
	st.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st.Lock()
		live := st.S.Entries["i-0000000001"].Live()
		rq := slices.Clone(st.S.RerunQueue)
		fq := slices.Clone(st.S.ForceReruns)
		st.Unlock()
		_, merr := os.Stat(filepath.Join(st.Dir, marksFile))
		_, rerr := os.Stat(filepath.Join(st.Dir, rerunsFile))
		if !live && os.IsNotExist(merr) && os.IsNotExist(rerr) &&
			slices.Equal(rq, []string{"2026-11-01", "2026-11-02", "2026-11-03"}) &&
			slices.Equal(fq, []string{"2026-11-05", "2026-11-06"}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued writes not applied: live=%v marks=%v reruns=%v rq=%v fq=%v", live, merr, rerr, rq, fq)
		}
		time.Sleep(20 * time.Millisecond)
	}
	st.Lock()
	v := st.S.Entries["i-0000000001"].Verdicts
	st.Unlock()
	if v[len(v)-1].By != "assistant" || v[len(v)-1].Note == "" {
		t.Fatalf("queued verdict: %+v", v)
	}
}

func TestQueuedMarksDoNotBlockSecondCall(t *testing.T) {
	toolWait = 100 * time.Millisecond
	t.Cleanup(func() { toolWait = 5 * time.Second })

	t.Run("second call while applyMarks waits for store lock", func(t *testing.T) {
		a, st := toolApp(t)
		a.c.Live = &curate.Live{}
		st.Lock()
		st.S.Entries = map[string]*entry.Entry{
			"i-0000000001": {
				ID: "i-0000000001", Facet: entry.Interaction, Text: "first rule",
				Status: entry.Stable, Count: 2, First: "2026-11-01", Last: "2026-11-05",
			},
			"i-0000000002": {
				ID: "i-0000000002", Facet: entry.Interaction, Text: "second rule",
				Status: entry.Stable, Count: 2, First: "2026-11-02", Last: "2026-11-05",
			},
		}
		if err := st.Save(); err != nil {
			st.Unlock()
			t.Fatal(err)
		}
		a.c.Publish()
		locked := true
		unlock := func() {
			if locked {
				locked = false
				st.Unlock()
			}
		}
		defer unlock()

		// First call queues and spawns applyMarks, which blocks on st.Lock().
		if text, err := call(t, a, "enkeep_mark_wrong", map[string]any{"ids": []string{"i-0000000001"}, "reason": "first reason"}); err != nil || !strings.Contains(text, "Queued") {
			t.Fatalf("first mark_wrong: %q %v", text, err)
		}
		// Give applyMarks time to enter its wait for st.Lock().
		time.Sleep(50 * time.Millisecond)

		// Second call while st.Lock() is still held must return Queued promptly instead of deadlocking on marksMu.
		type res struct {
			text string
			err  error
		}
		ch := make(chan res, 1)
		go func() {
			txt, err := call(t, a, "enkeep_mark_wrong", map[string]any{"ids": []string{"i-0000000002"}, "reason": "second reason"})
			ch <- res{txt, err}
		}()
		select {
		case r := <-ch:
			if r.err != nil || !strings.Contains(r.text, "Queued") {
				t.Fatalf("second mark_wrong: %q %v", r.text, r.err)
			}
		case <-time.After(1 * time.Second):
			unlock()
			<-ch
			t.Fatal("second enkeep_mark_wrong blocked waiting for marksMu while applyMarks held it across st.Lock()")
		}

		// Release the store lock and verify both queued marks are applied and the queue file is drained.
		unlock()
		deadline := time.Now().Add(5 * time.Second)
		for {
			st.Lock()
			live1 := st.S.Entries["i-0000000001"].Live()
			live2 := st.S.Entries["i-0000000002"].Live()
			st.Unlock()
			_, merr := os.Stat(filepath.Join(st.Dir, marksFile))
			if !live1 && !live2 && os.IsNotExist(merr) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("both queued marks not applied: live1=%v live2=%v marks=%v", live1, live2, merr)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	t.Run("first call before snapshot published does not deadlock on Curator.Read", func(t *testing.T) {
		a, st := toolApp(t)
		a.c.Live = &curate.Live{} // no Publish yet: Snapshot() is nil
		st.Lock()
		st.S.Entries = map[string]*entry.Entry{
			"i-0000000001": {
				ID: "i-0000000001", Facet: entry.Interaction, Text: "first rule",
				Status: entry.Stable, Count: 2, First: "2026-11-01", Last: "2026-11-05",
			},
		}
		if err := st.Save(); err != nil {
			st.Unlock()
			t.Fatal(err)
		}
		locked := true
		unlock := func() {
			if locked {
				locked = false
				st.Unlock()
			}
		}
		defer unlock()

		type res struct {
			text string
			err  error
		}
		ch := make(chan res, 1)
		go func() {
			txt, err := call(t, a, "enkeep_mark_wrong", map[string]any{"ids": []string{"i-0000000001"}, "reason": "early reason"})
			ch <- res{txt, err}
		}()
		select {
		case r := <-ch:
			if r.err != nil || !strings.Contains(r.text, "Queued") {
				t.Fatalf("early mark_wrong: %q %v", r.text, r.err)
			}
		case <-time.After(1 * time.Second):
			unlock()
			<-ch
			t.Fatal("enkeep_mark_wrong blocked on st.Lock() via Curator.Read when snapshot was nil")
		}
		unlock()
		deadline := time.Now().Add(5 * time.Second)
		for {
			st.Lock()
			live := st.S.Entries["i-0000000001"].Live()
			st.Unlock()
			_, merr := os.Stat(filepath.Join(st.Dir, marksFile))
			if !live && os.IsNotExist(merr) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("early queued mark not applied: live=%v marks=%v", live, merr)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}
