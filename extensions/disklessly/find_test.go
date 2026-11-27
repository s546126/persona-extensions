package main

import (
	"context"
	"encoding/json"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const findUUID = "0199aaaa-bbbb-cccc-dddd-eeeeffff0001"

// findFleet fills st with two computers and a made-up index (no conversation files, no runner).
// t0 is Monday 2026-11-09; 上周 is 11-02 to 11-08.
func findFleet(st *State, now time.Time) {
	st.ensureDefaults(now)
	local := st.machine("local")
	local.Reachable = true
	nas, _ := st.upsertMachine(MachineInput{ID: "nas-box", Host: "nas-box", Name: "nas-box", Source: "manual"}, now)
	nas.Reachable = true
	up := true
	cat := func(id, dev string, entries ...ColdEntry) *ColdCatalog {
		return &ColdCatalog{MachineID: id, DeviceID: dev, State: ColdReady, RefreshedAt: now, CheckedAt: now,
			Backend: ColdBackend{ID: "main", Type: "rclone", Reachable: &up, CheckedAt: now}, Entries: entries, Total: len(entries)}
	}
	st.setCold("local", cat("local", "dev-local",
		ColdEntry{ID: "aaaaaaaaaaa1", SessionID: "s-checkout", Tool: "claude", Title: "Fix checkout flow for empty cart",
			FirstPrompt: "Please fix the checkout when the cart is empty", Keywords: []string{"checkout", "cart"},
			Project: "shop", Cwd: "/home/dev/shop", Mtime: "2026-11-03T09:00:00Z", Bytes: 4096, State: "archived", Segment: "s1", Local: true},
		ColdEntry{ID: "aaaaaaaaaaa2", SessionID: findUUID, Tool: "codex", Title: findUUID,
			FirstPrompt: "重构购物车结算的价格计算，顺便补上测试", Project: "shop", Cwd: "/home/dev/shop/pricing",
			Mtime: "2026-10-20T08:00:00Z", Bytes: 2048, State: "archived", Segment: "s1", Local: true},
		ColdEntry{ID: "aaaaaaaaaaa3", SessionID: "s-notes", Tool: "claude", FirstPrompt: "ok", Project: "_unknown", Cwd: "/home/dev/notes",
			Mtime: "2026-11-08T22:15:00Z", Bytes: 1024, State: "restored", Local: true},
	))
	st.setCold("nas-box", cat("nas-box", "dev-nas",
		ColdEntry{ID: "bbbbbbbbbbb1", SessionID: "s-blog", Tool: "claude", Title: "Deploy the blog", Keywords: []string{"deploy", "blog"},
			Project: "blog", Cwd: "/srv/blog", Mtime: "2026-09-01T12:00:00Z", State: "remote", Segment: "s9", DeviceID: "dev-elsewhere"},
		ColdEntry{ID: "bbbbbbbbbbb2", SessionID: "s-badge", Tool: "claude", Title: "cart badge color", Project: "shop-ui", Cwd: "/srv/shop-ui",
			Mtime: "2026-11-05T16:30:00Z", Bytes: 8192, State: "archived", Segment: "s2", Local: true},
	))
}

func newFindState() *State {
	st := &State{}
	findFleet(st, t0)
	return st
}

func mustFind(t *testing.T, st *State, in findInput) *findAnswer {
	t.Helper()
	ans, err := findSessions(st, in, t0, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return ans
}

func candIDs(ans *findAnswer) []string {
	var out []string
	for _, c := range ans.Candidates {
		out = append(out, c.ID)
	}
	return out
}

func TestFindScoring(t *testing.T) {
	st := newFindState()
	ans := mustFind(t, st, findInput{Keywords: []string{"checkout", "cart", "购物车"}})
	// Two keywords in the title > one in the title > one in the first message.
	if got := candIDs(ans); !slices.Equal(got, []string{"aaaaaaaaaaa1", "bbbbbbbbbbb2", "aaaaaaaaaaa2"}) {
		t.Fatalf("order %v", got)
	}
	if c := ans.Candidates[0]; !slices.Contains(c.Why, "标题里有「checkout」") || c.Score <= ans.Candidates[1].Score {
		t.Fatalf("why/score: %+v", c)
	}
	if got := candIDs(mustFind(t, st, findInput{Keywords: []string{"checkout", "cart"}, Match: "all"})); !slices.Equal(got, []string{"aaaaaaaaaaa1"}) {
		t.Fatalf("all: %v", got)
	}
	// Case does not matter; the folder counts.
	if got := candIDs(mustFind(t, st, findInput{Keywords: []string{"PRICING"}})); !slices.Equal(got, []string{"aaaaaaaaaaa2"}) {
		t.Fatalf("folder: %v", got)
	}
	// No hint: newest first.
	if got := candIDs(mustFind(t, st, findInput{Limit: 2})); !slices.Equal(got, []string{"aaaaaaaaaaa3", "bbbbbbbbbbb2"}) {
		t.Fatalf("newest: %v", got)
	}
	if ans := mustFind(t, st, findInput{Limit: 1}); ans.Count != 5 || len(ans.Candidates) != 1 || ans.Suggest == "" {
		t.Fatalf("limit: %+v", ans)
	}
}

func TestFindDateToolProjectFilters(t *testing.T) {
	st := newFindState()
	cases := []struct {
		name string
		in   findInput
		want []string
	}{
		{"window", findInput{After: "2026-11-01", Before: "2026-11-06"}, []string{"bbbbbbbbbbb2", "aaaaaaaaaaa1"}},
		{"before is inclusive", findInput{After: "2026-11-08", Before: "2026-11-08"}, []string{"aaaaaaaaaaa3"}},
		{"after only", findInput{After: "2026-11-04"}, []string{"aaaaaaaaaaa3", "bbbbbbbbbbb2"}},
		{"rfc3339", findInput{Before: "2026-10-21T00:00:00Z"}, []string{"aaaaaaaaaaa2", "bbbbbbbbbbb1"}},
		{"codex", findInput{Tool: "Codex"}, []string{"aaaaaaaaaaa2"}},
		{"claude code", findInput{Tool: "Claude Code", Keywords: []string{"cart"}}, []string{"bbbbbbbbbbb2", "aaaaaaaaaaa1"}},
		{"project", findInput{Project: "shop"}, []string{"bbbbbbbbbbb2", "aaaaaaaaaaa1", "aaaaaaaaaaa2"}},
		{"project folder words", findInput{Project: "shop pricing"}, []string{"aaaaaaaaaaa2"}},
		{"on computer", findInput{Location: findOnComputer}, []string{"aaaaaaaaaaa3"}},
		{"on drive", findInput{Location: findOnDrive, Project: "blog"}, []string{"bbbbbbbbbbb1"}},
		{"machine", findInput{Machine: "nas-box"}, []string{"bbbbbbbbbbb2", "bbbbbbbbbbb1"}},
	}
	for _, c := range cases {
		if got := candIDs(mustFind(t, st, c.in)); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	for _, bad := range []findInput{{Match: "some"}, {After: "last week"}, {After: "2026-11-09", Before: "2026-11-01"},
		{Limit: 99}, {Location: "cloud"}, {Machine: "gone-box"}} {
		if _, err := findSessions(st, bad, t0, time.UTC); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestFindEmptyResultGuidance(t *testing.T) {
	st := newFindState()
	ans := mustFind(t, st, findInput{Keywords: []string{"checkout"}, Tool: "codex"})
	// Either hint alone leaves nothing: each is named with what dropping it would find.
	if ans.Count != 0 || len(ans.Candidates) != 0 || len(ans.TooNarrow) != 2 || !strings.Contains(ans.TooNarrow[0], "用的是 Codex") ||
		!strings.Contains(ans.TooNarrow[0], "1 个") || !strings.Contains(ans.Suggest, "不要自己猜") {
		t.Fatalf("tool too narrow: %+v", ans)
	}
	ans = mustFind(t, st, findInput{Keywords: []string{"checkout"}, Before: "2026-10-01"})
	if len(ans.TooNarrow) != 2 || !strings.Contains(ans.TooNarrow[0], "时间在 很早以前 到 2026-10-01 之间") {
		t.Fatalf("date too narrow: %+v", ans.TooNarrow)
	}
	ans = mustFind(t, st, findInput{Keywords: []string{"checkout", "blog"}, Match: "all"})
	if len(ans.TooNarrow) != 1 || !strings.Contains(ans.TooNarrow[0], "match=any") || !strings.Contains(ans.TooNarrow[0], "2 个") {
		t.Fatalf("all too narrow: %+v", ans.TooNarrow)
	}
	ans = mustFind(t, st, findInput{Keywords: []string{"kubernetes"}, Tool: "codex"})
	if len(ans.TooNarrow) != 1 || !strings.Contains(ans.TooNarrow[0], "不用关键词能找到 1 个") || !strings.Contains(ans.Suggest, "deep=true") {
		t.Fatalf("keyword too narrow: %+v", ans)
	}
	ans = mustFind(t, st, findInput{Keywords: []string{"kubernetes"}, Tool: "gemini"})
	if len(ans.TooNarrow) != 0 || !strings.Contains(ans.Suggest, "每个条件单独去掉也找不到") {
		t.Fatalf("nothing anywhere: %+v", ans)
	}
	// Two hints that only together leave nothing.
	ans = mustFind(t, st, findInput{Keywords: []string{"checkout"}, Tool: "codex", Before: "2026-10-01"})
	if len(ans.TooNarrow) == 0 || !strings.Contains(ans.TooNarrow[0], "合起来太窄：都去掉能找到 1 个") || ans.Candidates == nil {
		t.Fatalf("together too narrow: %+v", ans)
	}
	empty := &State{}
	empty.ensureDefaults(t0)
	if ans, err := findSessions(empty, findInput{Keywords: []string{"x"}}, t0, time.UTC); err != nil || !strings.Contains(ans.Suggest, "disklessly_sessions_status") {
		t.Fatalf("no index: %+v %v", ans, err)
	}
}

// Titles follow the page's rules and are never ids; each candidate carries the restore call.
func TestFindTitlesAndRestoreCall(t *testing.T) {
	st := newFindState()
	ans := mustFind(t, st, findInput{Limit: 10})
	d := draw{st: st, now: t0, loc: time.UTC}
	for _, c := range ans.Candidates {
		if idLikeRE.MatchString(c.Title) || strings.Contains(c.Title, findUUID) || strings.Contains(c.Title, c.ID) {
			t.Errorf("title is an id: %+v", c)
		}
		for _, h := range SearchCold(st, "", "", 0, t0) {
			if h.ID == c.ID {
				if want, _ := d.coldTitle(h.ColdEntry); want != c.Title {
					t.Errorf("title %q, the page shows %q", c.Title, want)
				}
			}
		}
		if c.When == "" || c.Tool == "" || c.MachineName == "" || c.Location == "" {
			t.Errorf("incomplete: %+v", c)
		}
		switch c.ID {
		case "aaaaaaaaaaa3": // still on the computer
			if c.CanRestore || c.RestoreCall != nil || !strings.HasPrefix(c.Title, untitledText) {
				t.Errorf("local: %+v", c)
			}
		case "bbbbbbbbbbb1": // another device's
			if c.CanRestore || c.RestoreCall != nil || !strings.Contains(c.Note, "另一台设备") {
				t.Errorf("other device: %+v", c)
			}
		default:
			if !c.CanRestore || c.RestoreCall == nil || c.RestoreCall.Tool != "disklessly_sessions_restore" ||
				c.RestoreCall.Arguments["machine"] != c.MachineID || c.RestoreCall.Arguments["id"] != c.ID {
				t.Fatalf("restore call: %+v", c)
			}
			if err := validateArgs(toolSchema(t, "disklessly_sessions_restore"), c.RestoreCall.Arguments); err != nil {
				t.Fatal(err)
			}
		}
	}
	two := slices.IndexFunc(ans.Candidates, func(c findCandidate) bool { return c.ID == "aaaaaaaaaaa2" })
	if c := ans.Candidates[two]; !strings.HasPrefix(c.Title, "重构购物车结算") || c.Resume != "codex resume "+findUUID || c.Folder != "pricing" {
		t.Fatalf("prompt title: %+v", c)
	}
}

func TestFindDeterministic(t *testing.T) {
	in := findInput{Keywords: []string{"cart", "shop"}}
	first, _ := json.Marshal(mustFind(t, newFindState(), in))
	for i := range 5 {
		st := newFindState()
		r := rand.New(rand.NewSource(int64(i)))
		r.Shuffle(len(st.Machines), func(x, y int) { st.Machines[x], st.Machines[y] = st.Machines[y], st.Machines[x] })
		again, _ := json.Marshal(mustFind(t, st, in))
		if string(again) != string(first) {
			t.Fatalf("run %d differs", i)
		}
	}
}

// The tool over MCP: read-only and allowed, never runs anything on a computer, and
// disklessly_sessions_search still answers as before.
func TestFindToolReadOnly(t *testing.T) {
	a := newTestApp(t)
	if err := a.db().update(func(st *State, now time.Time) error { findFleet(st, now); return nil }); err != nil {
		t.Fatal(err)
	}
	srv := &mcpServer{store: a.db(), runner: func(context.Context, *Machine, string, time.Duration) (string, error) {
		t.Fatal("find ran something on a computer")
		return "", nil
	}}
	out, err := srv.call(context.Background(), "disklessly_sessions_find", json.RawMessage(`{"keywords":["checkout"],"after":"2026-11-01","tool":"claude"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ans := out.(*findAnswer); !ans.ReadOnly || len(ans.Candidates) != 1 || ans.Candidates[0].ID != "aaaaaaaaaaa1" || !strings.Contains(ans.HowToAct, "确认") {
		t.Fatalf("find: %+v", ans)
	}
	if _, err := srv.call(context.Background(), "disklessly_sessions_find", json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Fatal("unknown argument accepted")
	}
	old, err := srv.call(context.Background(), "disklessly_sessions_search", json.RawMessage(`{"query":"checkout"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res := old.(map[string]any); res["count"] != 1 || res["query"] != "checkout" {
		t.Fatalf("search: %v", res)
	}
	for _, tl := range tools {
		if tl.name == "disklessly_sessions_find" {
			ann := tl.schema()["annotations"].(map[string]any)
			if ann["readOnlyHint"] != true || ann["destructiveHint"] != false || !strings.Contains(tl.description, "只读") ||
				!strings.Contains(tl.description, "disklessly_sessions_restore") {
				t.Fatalf("find tool: %v %q", ann, tl.description)
			}
			if err := validateArgs(toolSchema(t, tl.name), map[string]any{"keywords": []string{"cart"}, "match": "all",
				"after": "2026-11-01", "location": findOnDrive}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The AI 对话 page: several words, a time word, and the hint that names the assistant.
func TestSessionsPageFindHintAndDateWords(t *testing.T) {
	noSandbox(t)
	a := newTestApp(t)
	if err := a.db().update(func(st *State, now time.Time) error { findFleet(st, now); return nil }); err != nil {
		t.Fatal(err)
	}
	search := func(q string) []string {
		t.Helper()
		res, err := a.actionSessions(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, TZ: "UTC", Action: "search", Values: map[string]string{"q": q}}})
		if err != nil {
			t.Fatal(err)
		}
		checkStrictView(t, res.View, false)
		checkTapNames(t, "search "+q, res.View)
		var keys []string
		for _, s := range res.View.Sections {
			for _, c := range s.Items {
				if strings.HasPrefix(c.Key, "cold-") {
					keys = append(keys, strings.TrimPrefix(c.Key, "cold-"))
				}
			}
			if strings.HasPrefix(s.Title, "搜索") || strings.HasPrefix(s.Title, "已经搬到网盘") {
				if !strings.Contains(s.Footer, "「上周」") {
					t.Errorf("hint missing: %q", s.Footer)
				}
			}
			// The assistant's way to find one is said once, in the page's advice block.
			if s.Key == "sessions-advice" && !strings.Contains(componentsText(s.Items), "跟 Persona 助手说") {
				t.Errorf("assistant hint missing: %+v", s.Items)
			}
		}
		return keys
	}
	if got := search("cart shop"); !slices.Equal(got, []string{"nas-box-bbbbbbbbbbb2", "local-aaaaaaaaaaa1"}) {
		t.Fatalf("several words: %v", got)
	}
	if got := search("上周"); !slices.Equal(got, []string{"local-aaaaaaaaaaa3", "nas-box-bbbbbbbbbbb2", "local-aaaaaaaaaaa1"}) {
		t.Fatalf("上周: %v", got)
	}
	if got := search("cart 上周 local"); !slices.Equal(got, []string{"local-aaaaaaaaaaa1"}) {
		t.Fatalf("cart 上周 local: %v", got)
	}
	if got := search("上个月"); !slices.Equal(got, []string{"local-aaaaaaaaaaa2"}) {
		t.Fatalf("上个月: %v", got)
	}
	if got := search("今天"); len(got) != 0 {
		t.Fatalf("今天: %v", got)
	}
	for _, w := range []string{"今天", "昨天", "前天", "这周", "上周", "本月", "上个月", "今年", "去年"} {
		from, to, ok := dateWindow(w, t0, time.UTC)
		if !ok || !from.Before(to) {
			t.Errorf("%s: %v %v", w, from, to)
		}
	}
}
