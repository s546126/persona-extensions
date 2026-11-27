package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// adviceFleet fills st with four computers (fake probes only):
//   - build-box: 92% used (fullest), no disklessly tool, Homebrew there;
//   - local: 85% used, tool and network drive ready, a 建议先看 item;
//   - offline-box: checked before (85%), not answering now;
//   - never-box: never checked.
func adviceFleet(t *testing.T, st *State, now time.Time) {
	t.Helper()
	st.ensureDefaults(now)
	local := st.machine("local")
	local.Reachable = true
	st.Scans["local"] = ParseProbeOutput(local, sampleProbeLocal, now)
	if _, err := RefreshColdIndex(context.Background(), coldRunner(nil), st, local, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"build-box", "offline-box", "never-box"} {
		m, err := st.upsertMachine(MachineInput{ID: id, Host: id, Name: id, Source: "manual"}, now)
		if err != nil {
			t.Fatal(err)
		}
		switch id {
		case "build-box":
			m.Reachable = true
			st.Scans[id] = ParseProbeOutput(m, sampleProbeARMBOX, now)
			st.setCold(id, &ColdCatalog{MachineID: id, State: ColdNoCLI, Brew: "/opt/homebrew/bin/brew", BrewKnown: true, Platform: "linux-arm64", RefreshedAt: now, CheckedAt: now})
		case "offline-box":
			st.Scans[id] = ParseProbeOutput(m, sampleProbeLocal, now.Add(-50*time.Hour))
			m.Reachable, m.LastError = false, "SSH 连接 offline-box 失败：ssh: connect to host offline-box port 22: Connection timed out"
		}
	}
}

func newAdviceState(t *testing.T) *State {
	st := &State{}
	adviceFleet(t, st, t0)
	return st
}

func mustAdvice(t *testing.T, st *State, opt adviceOptions) *advice {
	t.Helper()
	adv, err := buildAdvice(st, t0, opt)
	if err != nil {
		t.Fatal(err)
	}
	return adv
}

func stepNames(steps []adviceStep) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.MachineID+"/"+s.Action+"/"+s.Kind)
	}
	return out
}

// What can be done now first, then what cannot (0.5.4); within each, the fullest computer
// first and on each, caches (biggest first), then conversations, then old logs; 建议先看 apart.
func TestAdviseRanking(t *testing.T) {
	st := newAdviceState(t)
	adv := mustAdvice(t, st, adviceOptions{})
	want := []string{
		"build-box/clean/go-cache", "build-box/clean/node-python-cache", "build-box/clean/docker-unused",
		"build-box/clean/tmp-logs",
		"local/clean/go-cache", "local/clean/docker-unused", "local/clean/node-python-cache", "local/clean/rust-xcode-build",
		"local/move_conversations/cold-agent-sessions", "local/clean/tmp-logs",
		// build-box has no helper yet; offline-box cannot be reached.
		"build-box/move_conversations/cold-agent-sessions",
		"offline-box/clean/go-cache", "offline-box/clean/docker-unused", "offline-box/clean/node-python-cache", "offline-box/clean/rust-xcode-build",
		"offline-box/move_conversations/cold-agent-sessions", "offline-box/clean/tmp-logs",
	}
	if got := stepNames(adv.Steps); !slices.Equal(got, want) {
		t.Fatalf("order:\n got %q\nwant %q", got, want)
	}
	for i, s := range adv.Steps {
		if i > 0 && s.Possible && !adv.Steps[i-1].Possible {
			t.Errorf("step %d can be done now but comes after one that cannot", i+1)
		}
		if s.Rank != i+1 || s.Risk == "" || s.What == "" || s.Frees == "" || s.Page == "" || len(s.PageButtons) == 0 {
			t.Errorf("step %d incomplete: %+v", i, s)
		}
	}
	if got := stepNames(adv.LookFirst); !slices.Equal(got, []string{"local/look_first/rust-xcode-build", "offline-box/look_first/rust-xcode-build"}) {
		t.Fatalf("look first: %q", got)
	}
	for _, s := range adv.Steps {
		for _, it := range s.Items {
			if it.ID == "cache-playwright" {
				t.Fatalf("建议先看 item among the steps: %+v", s)
			}
		}
	}
	// The kind holding a 建议先看 item names its safe items' own rows, not the kind's 删掉 row.
	rx := adv.Steps[7]
	if rx.Kind != CatRustXcodeBuild || slices.Contains(rx.PageButtons, "删掉："+categoryName(CatRustXcodeBuild)) {
		t.Fatalf("rust step buttons %q", rx.PageButtons)
	}
	if !strings.Contains(adv.Steps[9].Risk, "找不回来") || !strings.Contains(adv.Steps[8].Risk, "读回核对") || !strings.Contains(adv.Steps[0].Risk, "重新") {
		t.Fatalf("risks: %q / %q / %q", adv.Steps[0].Risk, adv.Steps[8].Risk, adv.Steps[9].Risk)
	}
	if adv.Machines[0].MachineID != "build-box" || adv.Machines[len(adv.Machines)-1].MachineID != "never-box" {
		t.Fatalf("machines: %+v", adv.Machines)
	}
	if !adv.ReadOnly || !strings.Contains(adv.HowToAct, "确认") || len(adv.NotTouched) < 5 {
		t.Fatalf("answer: %+v", adv)
	}
}

func TestAdviseGoalCutOff(t *testing.T) {
	st := newAdviceState(t)
	// build-box: Go 5 GiB + pnpm 4 GiB reach 6 GiB.
	adv := mustAdvice(t, st, adviceOptions{Goal: adviceGoal{FreeGiB: 6}})
	if got := stepNames(adv.Steps); !slices.Equal(got, []string{"build-box/clean/go-cache", "build-box/clean/node-python-cache"}) {
		t.Fatalf("cut off: %q", got)
	}
	if adv.Goal == nil || !adv.Goal.Met || adv.MoreSteps == 0 || len(adv.LookFirst) != 0 {
		t.Fatalf("goal: %+v more %d look %d", adv.Goal, adv.MoreSteps, len(adv.LookFirst))
	}
	// Steps that cannot be done now do not count toward the goal.
	big := mustAdvice(t, st, adviceOptions{Goal: adviceGoal{FreeGiB: 10000}})
	if big.Goal.Met || len(big.Steps) != 17 || len(big.LookFirst) == 0 || !strings.Contains(big.Goal.Note, "不到") {
		t.Fatalf("unreachable goal: %+v steps %d", big.Goal, len(big.Steps))
	}
	var possible uint64
	for _, s := range big.Steps {
		if s.Possible {
			possible += s.FreesBytes
		} else if s.MachineID != "offline-box" && s.Action != adviceMove {
			t.Errorf("unexpectedly blocked: %+v", s)
		}
	}
	if big.Goal.PlannedBytes != possible {
		t.Fatalf("planned %d, possible %d", big.Goal.PlannedBytes, possible)
	}
	// Only the fullest computer.
	full := mustAdvice(t, st, adviceOptions{Goal: adviceGoal{Fullest: true}})
	if len(full.Machines) != 1 || full.Machines[0].MachineID != "build-box" {
		t.Fatalf("fullest: %+v", full.Machines)
	}
	for _, s := range full.Steps {
		if s.MachineID != "build-box" {
			t.Fatalf("fullest has %s", s.MachineID)
		}
	}
	// One named computer.
	one := mustAdvice(t, st, adviceOptions{Machine: "local"})
	if len(one.Machines) != 1 || one.Steps[0].MachineID != "local" {
		t.Fatalf("machine: %+v", one.Machines)
	}
	if _, err := buildAdvice(st, t0, adviceOptions{Machine: "gone-box"}); err == nil {
		t.Fatal("unknown computer accepted")
	}
}

func TestAdviseReadinessReasons(t *testing.T) {
	st := newAdviceState(t)
	adv := mustAdvice(t, st, adviceOptions{})
	find := func(id, action, kind string) adviceStep {
		for _, s := range adv.Steps {
			if s.MachineID == id && s.Action == action && s.Kind == kind {
				return s
			}
		}
		t.Fatalf("no step %s/%s/%s", id, action, kind)
		return adviceStep{}
	}
	// No tool: says so, and the fix is the Homebrew install (a confirmed write).
	mv := find("build-box", adviceMove, CatColdAgentSession)
	if mv.Possible || !strings.Contains(mv.Blocked, "配套小工具") || mv.UnblockCall == nil || mv.UnblockCall.Tool != "disklessly_cli_install" ||
		mv.UnblockCall.Arguments["method"] != "brew" || !strings.Contains(mv.Unblock, "确认") {
		t.Fatalf("no tool: %+v", mv)
	}
	// Ready: possible, with the offload call and its dry run.
	lm := find("local", adviceMove, CatColdAgentSession)
	if !lm.Possible || lm.Call.Tool != "disklessly_sessions_offload" || lm.PreviewCall == nil || lm.PreviewCall.Arguments["dry_run"] != true {
		t.Fatalf("ready: %+v", lm)
	}
	// Unreachable: every step blocked, with why and a read-only re-check.
	for _, s := range adv.Steps {
		if s.MachineID != "offline-box" {
			continue
		}
		if s.Possible || !strings.Contains(s.Blocked, "连不上") || s.UnblockCall == nil || s.UnblockCall.Tool != "disklessly_advise" {
			t.Fatalf("offline: %+v", s)
		}
	}
	var offline, never adviceMachine
	for _, am := range adv.Machines {
		switch am.MachineID {
		case "offline-box":
			offline = am
		case "never-box":
			never = am
		}
	}
	if !offline.Stale || !strings.Contains(offline.Checked, "2 天前") || !strings.Contains(offline.Problem, "连不上") {
		t.Fatalf("offline machine: %+v", offline)
	}
	if never.CheckedAt != nil || never.NextCall == nil || never.NextCall.Arguments["refresh"] != true || !strings.Contains(never.Problem, "还没检查") {
		t.Fatalf("never checked: %+v", never)
	}
	// No network drive set up: says so and gives the setup command.
	st.setCold("local", &ColdCatalog{MachineID: "local", State: ColdNoBackend, RefreshedAt: t0})
	adv = mustAdvice(t, st, adviceOptions{Machine: "local"})
	nd := adviceStep{}
	for _, s := range adv.Steps {
		if s.Action == adviceMove {
			nd = s
		}
	}
	if nd.Possible || !strings.Contains(nd.Blocked, "网盘") || !strings.Contains(nd.Command, "disklessly backend") {
		t.Fatalf("no drive: %+v", nd)
	}
}

func TestAdviseDeterministic(t *testing.T) {
	a := newAdviceState(t)
	first, _ := json.Marshal(mustAdvice(t, a, adviceOptions{}))
	for i := range 5 {
		b := newAdviceState(t)
		r := rand.New(rand.NewSource(int64(i)))
		r.Shuffle(len(b.Machines), func(x, y int) { b.Machines[x], b.Machines[y] = b.Machines[y], b.Machines[x] })
		again, _ := json.Marshal(mustAdvice(t, b, adviceOptions{}))
		if string(again) != string(first) {
			t.Fatalf("run %d differs:\n%s\n%s", i, first, again)
		}
	}
}

// Nothing in use, nothing protected: an in-use item is in no step and is named as kept.
func TestAdviseSkipsInUseAndProtected(t *testing.T) {
	st := newAdviceState(t)
	sc := st.Scans["local"]
	for i := range sc.Items {
		if sc.Items[i].ID == "cache-gobuild" {
			sc.Items[i].InUse = true
		}
	}
	sc.Items = append(sc.Items, ScanItem{ID: "evil", Category: CatGoCache, Title: "keys", Path: "/home/dev/.ssh", Bytes: gib, Safety: SafetySafe, Action: "clean"})
	adv := mustAdvice(t, st, adviceOptions{Machine: "local"})
	for _, s := range append(slices.Clone(adv.Steps), adv.LookFirst...) {
		for _, it := range s.Items {
			if it.ID == "cache-gobuild" || it.ID == "evil" {
				t.Fatalf("proposed %s: %+v", it.ID, s)
			}
		}
	}
	if !slices.ContainsFunc(adv.NotTouched, func(s string) bool { return strings.Contains(s, "正在用的「Go 编译缓存」") }) {
		t.Fatalf("not touched: %q", adv.NotTouched)
	}
	if !slices.ContainsFunc(adv.NotTouched, func(s string) bool { return strings.Contains(s, "最近一周用过的") }) {
		t.Fatalf("recent conversations not named: %q", adv.NotTouched)
	}
}

// validateArgs checks args against a tool's input schema (the subset the tools use).
func validateArgs(schema map[string]any, args map[string]any) error {
	props, _ := schema["properties"].(map[string]any)
	for _, r := range schema["required"].([]string) {
		if _, ok := args[r]; !ok {
			return fmt.Errorf("missing %s", r)
		}
	}
	for k, v := range args {
		p, ok := props[k].(map[string]any)
		if !ok {
			return fmt.Errorf("unknown argument %s", k)
		}
		if err := validateValue(p, v); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

func validateValue(p map[string]any, v any) error {
	switch p["type"] {
	case "string":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("not a string: %v", v)
		}
		if enum, ok := p["enum"]; ok {
			var list []string
			switch e := enum.(type) {
			case []string:
				list = e
			}
			if !slices.Contains(list, s) {
				return fmt.Errorf("%q not in %v", s, list)
			}
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("not a boolean: %v", v)
		}
	case "array":
		list, ok := v.([]string)
		if !ok {
			return fmt.Errorf("not a string array: %T", v)
		}
		if mi, ok := p["minItems"].(int); ok && len(list) < mi {
			return fmt.Errorf("fewer than %d items", mi)
		}
		for _, s := range list {
			if err := validateValue(p["items"].(map[string]any), s); err != nil {
				return err
			}
		}
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("not an object")
		}
		return validateArgs(map[string]any{"properties": p["properties"], "required": []string{}}, m)
	default:
		return fmt.Errorf("type %v not handled", p["type"])
	}
	return nil
}

func toolSchema(t *testing.T, name string) map[string]any {
	t.Helper()
	for _, tl := range tools {
		if tl.name == name {
			s := tl.schema()["inputSchema"].(map[string]any)
			if _, ok := s["required"]; !ok {
				s["required"] = []string{}
			}
			return s
		}
	}
	t.Fatalf("no tool %s", name)
	return nil
}

// Every call the plan names is an existing tool with arguments its schema accepts; a clean call
// selects exactly the step's items, all 放心删 (look_first: the one 建议先看 item).
func TestAdviseCallsFitExistingTools(t *testing.T) {
	st := newAdviceState(t)
	adv := mustAdvice(t, st, adviceOptions{})
	check := func(what string, c *adviceCall) {
		t.Helper()
		if c == nil {
			return
		}
		if err := validateArgs(toolSchema(t, c.Tool), c.Arguments); err != nil {
			t.Errorf("%s: %s %v: %v", what, c.Tool, c.Arguments, err)
		}
	}
	for _, s := range append(slices.Clone(adv.Steps), adv.LookFirst...) {
		what := s.MachineID + "/" + s.Kind
		check(what, &s.Call)
		check(what+" preview", s.PreviewCall)
		check(what+" unblock", s.UnblockCall)
		switch s.Action {
		case adviceClean, adviceLookFirst:
			if s.Call.Tool != "disklessly_clean" {
				t.Fatalf("%s: %s", what, s.Call.Tool)
			}
			ids := s.Call.Arguments["items"].([]string)
			plan, err := BuildCleanPlan(st.machine(s.MachineID), st.Scans[s.MachineID], nil, ids, false)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			var bytes uint64
			for _, it := range plan.Items {
				got = append(got, it.ID)
				bytes += it.Bytes
				if (it.Safety == SafetySafe) != (s.Action == adviceClean) || it.Category != s.Kind || it.InUse {
					t.Errorf("%s selects %+v", what, it)
				}
			}
			if !slices.Equal(got, ids) || bytes != s.FreesBytes {
				t.Errorf("%s: plan %v (%d), step %v (%d)", what, got, bytes, ids, s.FreesBytes)
			}
		case adviceMove:
			if s.Call.Tool != "disklessly_sessions_offload" || s.Call.Arguments["machine"] != s.MachineID {
				t.Fatalf("%s: %+v", what, s.Call)
			}
		}
	}
	for _, am := range adv.Machines {
		check(am.MachineID+" next", am.NextCall)
	}
	// The advise tool's own example arguments fit its schema.
	if err := validateArgs(toolSchema(t, "disklessly_advise"), map[string]any{"machine": "local", "refresh": true,
		"goal": map[string]any{"fullest": true}}); err != nil {
		t.Fatal(err)
	}
}

// disklessly_advise is read-only and allowed; it is the only newly allowed tool; with refresh it
// runs the read-only check and nothing that deletes or moves.
func TestAdviseToolReadOnlyAndAllowed(t *testing.T) {
	var m struct {
		Contributes struct {
			MCPServers []struct {
				Allow []string `json:"allow"`
			} `json:"mcp_servers"`
			Prompts []struct {
				Kind, Title, Text string
			} `json:"prompts"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	allow := m.Contributes.MCPServers[0].Allow
	want := []string{"disklessly_machines_status", "disklessly_scan", "disklessly_history", "disklessly_sessions_search", "disklessly_sessions_status", "disklessly_sessions_find", "disklessly_advise"}
	if !slices.Equal(allow, want) {
		t.Fatalf("allow %v", allow)
	}
	for _, tl := range tools {
		ann := tl.schema()["annotations"].(map[string]any)
		if slices.Contains(allow, tl.name) != (ann["readOnlyHint"] == true) {
			t.Errorf("%s: allowed %v, read-only %v", tl.name, slices.Contains(allow, tl.name), ann["readOnlyHint"])
		}
		if tl.name == "disklessly_advise" && (ann["destructiveHint"] != false || !strings.Contains(tl.description, "只读") || !strings.Contains(tl.description, "确认")) {
			t.Errorf("advise annotations %v / description", ann)
		}
	}
	if len(m.Contributes.Prompts) != 1 || m.Contributes.Prompts[0].Kind != "quick" || !strings.Contains(m.Contributes.Prompts[0].Text, assistantPrompt) {
		t.Fatalf("quick prompt: %+v", m.Contributes.Prompts)
	}

	a := newTestApp(t)
	if err := a.db().update(func(st *State, now time.Time) error { adviceFleet(t, st, now); return nil }); err != nil {
		t.Fatal(err)
	}
	var scripts []string
	srv := &mcpServer{store: a.db(), runner: coldRunner(&scripts)}
	out, err := srv.call(context.Background(), "disklessly_advise", json.RawMessage(`{"goal":{"free_gib":6}}`))
	if err != nil {
		t.Fatal(err)
	}
	adv := out.(*advice)
	if len(adv.Steps) != 2 || !strings.Contains(adv.Source, "上次检查") || len(scripts) != 0 {
		t.Fatalf("cached: %d steps, %q, %d scripts", len(adv.Steps), adv.Source, len(scripts))
	}
	out, err = srv.call(context.Background(), "disklessly_advise", json.RawMessage(`{"machine":"local","refresh":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if adv := out.(*advice); !strings.Contains(adv.Source, "刚刚重新检查") || len(adv.Steps) == 0 {
		t.Fatalf("refresh: %+v", adv)
	}
	for _, s := range scripts {
		if strings.Contains(s, "@@BEFORE_B") || strings.Contains(s, "sessions --apply") || strings.Contains(s, "restore") {
			t.Fatalf("refresh ran a write script:\n%s", s)
		}
	}
	if err := a.db().read(func(st *State) {
		if len(st.History) != 0 {
			t.Errorf("history written: %v", st.History)
		}
	}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"goal":{"free_gib":-1}}`, `{"goal":{"gib":3}}`, `{"machine":"gone-box"}`, `{"force":true}`} {
		if _, err := srv.call(context.Background(), "disklessly_advise", json.RawMessage(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// adviceView draws 硬盘空间's 所有电脑 tab on the advice fleet.
func adviceApp(t *testing.T) *app {
	t.Helper()
	a := newTestApp(t)
	if err := a.db().update(func(st *State, now time.Time) error { adviceFleet(t, st, now); return nil }); err != nil {
		t.Fatal(err)
	}
	return a
}

// checkAdviceButtons follows each 帮我腾空间 row of v (a 硬盘空间 view) and checks that the
// page it opens draws the button the row names. It returns the number of rows followed.
func checkAdviceButtons(t *testing.T, a *app, what string, v *wire.View) int {
	t.Helper()
	ctx := context.Background()
	var adv *advice
	if err := a.db().read(func(st *State) { adv, _ = buildAdvice(st, a.now(), adviceOptions{}) }); err != nil || adv == nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range v.Sections {
		if s.Key != "advice" {
			continue
		}
		for _, c := range s.Items {
			if c.Type != wire.ComponentRow || c.Action == nil || !strings.HasPrefix(c.Key, "advice-") || c.Key == "advice-assistant" {
				continue
			}
			for _, d := range []string{"删掉放心删的缓存", "按勾选", "全部"} {
				if strings.Contains(c.Title, d) {
					t.Errorf("%s: a do-everything row %q", what, c.Title)
				}
			}
			if c.Action.Confirm != "" || strings.HasPrefix(c.Action.ID, "clean") || strings.HasPrefix(c.Action.ID, "cold-") {
				t.Errorf("%s: advice row %q acts by itself: %+v", what, c.Title, c.Action)
			}
			var rank int
			fmt.Sscanf(c.Title, "%d.", &rank)
			step := adv.Steps[rank-1]
			if !step.Possible || strings.Contains(c.Subtitle, "忙") {
				continue
			}
			var next *wire.View
			if c.Action.Push != nil {
				var sp sessPage
				if err := (&ext.UIRequest{UIParams: wire.UIParams{State: c.Action.Push.State}}).DecodeState(&sp); err != nil {
					t.Fatal(err)
				}
				next = a.mustSessions(t, sp)
			} else {
				res, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: v.State, Action: c.Action.ID, Values: c.Action.Values}})
				if err != nil {
					t.Fatal(err)
				}
				next = res.View
			}
			can := clickables(next)
			for _, b := range step.PageButtons {
				if !can[b] {
					t.Errorf("%s: step %q names 「%s」, which the page it opens does not draw:\n%s", what, c.Title, b, visibleText(next))
				}
				if !strings.Contains(c.Subtitle, b) && len(step.PageButtons) == 1 {
					t.Errorf("%s: row %q does not name 「%s」: %s", what, c.Title, b, c.Subtitle)
				}
			}
			n++
		}
	}
	return n
}

func (a *app) mustSessions(t *testing.T, sp sessPage) *wire.View {
	t.Helper()
	v, err := a.sessionsView(&ext.UIRequest{}, sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The block sits right below the summary numbers, lists three steps with gain and risk, the
// assistant row, and fits a phone.
func TestAdviceBlockOnDiskPage(t *testing.T) {
	noSandbox(t)
	a := adviceApp(t)
	v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, v, false)
	assertMobileReadability360px(t, "advice", v)
	checkTapNames(t, "advice", v)
	idx := slices.IndexFunc(v.Sections, func(s wire.Section) bool { return s.Key == "advice" })
	if idx < 1 || v.Sections[idx-1].Title != "每台电脑的硬盘" || v.Sections[idx].Title != adviceTitle {
		t.Fatalf("block at %d", idx)
	}
	sec := v.Sections[idx]
	// The steps, the row that opens every step, the assistant row.
	if len(sec.Items) != adviceShown+2 || sec.Items[adviceShown].Title != adviceAllRow || sec.Items[len(sec.Items)-1].Title != assistantRowTitle ||
		!strings.Contains(sec.Items[len(sec.Items)-1].Subtitle, "「"+assistantPrompt+"」") {
		t.Fatalf("block items: %+v", sec.Items)
	}
	if !strings.Contains(sec.Items[0].Title, "1. build-box：删掉") || sec.Items[0].Badge == nil || !strings.HasPrefix(sec.Items[0].Badge.Text, "+") ||
		!strings.Contains(sec.Items[0].Subtitle, "重新") {
		t.Fatalf("first step: %+v", sec.Items[0])
	}
	if !strings.Contains(sec.Footer, "还有") || !strings.Contains(sec.Footer, "建议先看") {
		t.Fatalf("footer: %q", sec.Footer)
	}
	for _, c := range sec.Items {
		if c.Type == wire.ComponentButton {
			t.Fatalf("a button in the block: %+v", c)
		}
	}
	if n := checkAdviceButtons(t, a, "advice", v); n != adviceShown {
		t.Fatalf("followed %d rows", n)
	}
	// The summary numbers stay first: the block costs no more than its rows.
	if s := v.Sections[idx-1]; s.Items[0].Type != wire.ComponentStatGrid {
		t.Fatalf("summary first item %s", s.Items[0].Type)
	}

	// A conversation step leads to AI 对话, whose row has the upload button.
	b := adviceApp(t)
	if err := b.db().update(func(st *State, now time.Time) error {
		for _, id := range []string{"build-box", "offline-box", "never-box"} {
			st.machine(id).Enabled = false
		}
		sc := st.Scans["local"]
		sc.Items = slices.DeleteFunc(sc.Items, func(it ScanItem) bool { return it.Category == CatGoCache || it.Category == CatNodePythonCache })
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	v, err = b.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8}})
	if err != nil {
		t.Fatal(err)
	}
	if txt := visibleText(v); !strings.Contains(txt, "把一周没动的 AI 对话搬到网盘") {
		t.Fatalf("no conversation step:\n%s", txt)
	}
	if n := checkAdviceButtons(t, b, "conversations", v); n == 0 {
		t.Fatal("no row followed")
	}

	// Nothing checked: one line and the assistant row.
	c := newTestApp(t)
	v, err = c.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8}})
	if err != nil {
		t.Fatal(err)
	}
	idx = slices.IndexFunc(v.Sections, func(s wire.Section) bool { return s.Key == "advice" })
	if idx < 0 || len(v.Sections[idx].Items) != 2 {
		t.Fatalf("unchecked block: %+v", v.Sections)
	}
}
