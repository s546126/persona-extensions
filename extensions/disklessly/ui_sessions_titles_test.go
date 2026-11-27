package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	untitledUUID = "5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d"
	idTitleUUID  = "9e8d7c6b-5a4f-4e3d-8c2b-1a0f9e8d7c6b"
	promptUUID   = "0d9c8b7a-6f5e-4d3c-8b2a-1f0e9d8c7b6a"
)

// Conversations as the CLI lists them when it has no title: none, the session id as title, and
// the first message only; two of them in a project it could not place (_unknown).
var untitledEntries = []map[string]any{
	{"id": "c1b2c3d4e5f6", "local": true, "tool": "claude", "path": "/home/dev/.claude/projects/-home-dev/" + untitledUUID + ".jsonl",
		"sessionId": untitledUUID, "project": "_unknown", "mtime": "2026-11-08T09:30:00Z",
		"bytes": 1024, "state": "archived", "title": "", "firstPrompt": "", "segment": "seg-missing"},
	{"id": "d1b2c3d4e5f6", "local": true, "tool": "", "path": "/home/dev/.claude/projects/-home-dev-notes/" + idTitleUUID + ".jsonl",
		"sessionId": idTitleUUID, "cwd": "/home/dev/notes", "project": "_unknown", "mtime": "2025-10-28T09:30:00Z",
		"bytes": 1024, "state": "archived", "title": idTitleUUID, "segment": "seg-slow"},
	{"id": "b1b2c3d4e5f6", "local": true, "tool": "codex", "path": "/home/dev/.codex/sessions/2026/11/02/rollout-" + promptUUID + ".jsonl",
		"sessionId": promptUUID, "project": "blog", "mtime": "2026-11-02T10:00:00Z", "bytes": 2048, "state": "archived",
		"firstPrompt": "帮我看看为什么 nightly 构建总是失败。日志里一直报磁盘空间不够", "segment": "seg-a"},
}

func untitledApp(t *testing.T) *app {
	t.Helper()
	a := newTestApp(t)
	a.runner = func(ctx context.Context, m *Machine, s string, d time.Duration) (string, error) {
		if strings.Contains(s, "sessions list --state all") {
			return coldRefreshOut(coldListJSON(append(untitledEntries, coldEntries[0])...), coldStatusJSON), nil
		}
		return fakeRunner(ctx, m, s, d)
	}
	if _, err := a.actionSessions(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Action: "cold-refresh", Values: map[string]string{"m": ""}}}); err != nil {
		t.Fatal(err)
	}
	a.waitJobs()
	return a
}

func sessionsSearch(t *testing.T, a *app, machine, q string) *wire.View {
	t.Helper()
	res, err := a.actionSessions(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		State: ext.EncodeState(sessPage{Machine: machine}), Action: "search", Values: map[string]string{"q": q}}})
	if err != nil {
		t.Fatal(err)
	}
	return res.View
}

func conversationRows(v *wire.View) map[string]wire.Component {
	out := map[string]wire.Component{}
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Type == wire.ComponentRow && c.Icon == "chat" && strings.HasPrefix(c.Key, "cold-") {
				out[c.Key] = c
			}
		}
	}
	return out
}

func TestConversationTitleIsNeverAnID(t *testing.T) {
	d := draw{now: t0, loc: time.UTC}
	cases := []struct {
		name string
		e    ColdEntry
		want string
	}{
		{"real title", ColdEntry{ID: "a1", SessionID: untitledUUID, Title: "Fix the checkout flow", FirstPrompt: "Please fix it"}, "Fix the checkout flow"},
		{"session id as title, first message", ColdEntry{ID: "a1", SessionID: untitledUUID, Title: untitledUUID, FirstPrompt: "Please fix the checkout flow"}, "Please fix the checkout flow"},
		{"entry id as title", ColdEntry{ID: "c1b2c3d4e5f6", Title: "c1b2c3d4e5f6", Tool: "claude", Mtime: "2026-11-08T09:30:00Z"}, "没有标题的对话 · 1 天前 09:30 · Claude Code"},
		{"first sentence", ColdEntry{FirstPrompt: "帮我看看为什么 nightly 构建总是失败。日志里一直报磁盘空间不够"}, "帮我看看为什么 nightly 构建总是失败。"},
		{"long first message", ColdEntry{FirstPrompt: strings.Repeat("长", 80)}, strings.Repeat("长", promptTitleRunes-1) + "…"},
		{"nothing, tool from path, other year", ColdEntry{SessionID: idTitleUUID, Path: "/home/dev/.codex/sessions/x.jsonl", Mtime: "2025-10-28T09:30:00Z"}, "没有标题的对话 · 2025年10月28日 09:30 · Codex"},
		{"nothing at all", ColdEntry{ID: "c1b2c3d4e5f6"}, "没有标题的对话"},
		{"placeholder title", ColdEntry{Title: "_unknown", Tool: "claude"}, "没有标题的对话 · Claude Code"},
	}
	for _, c := range cases {
		if got, _ := d.coldTitle(c.e); got != c.want {
			t.Errorf("%s: title %q, want %q", c.name, got, c.want)
		}
	}
	if p := coldProject(ColdEntry{Project: "_unknown"}); p != "" {
		t.Errorf("_unknown is a project: %q", p)
	}
	if p := coldProject(ColdEntry{Project: "_unknown", Cwd: "/home/dev/notes/"}); p != "notes" {
		t.Errorf("project from the working folder: %q", p)
	}
}

// The list shows no id as a title and no _unknown, keeps the id as a detail, and search still
// finds the conversations by id and by the text it found them by before.
func TestConversationListReadable(t *testing.T) {
	a := untitledApp(t)
	for _, m := range []string{"", "local"} {
		v := sessionsSearch(t, a, m, "")
		rows := conversationRows(v)
		if len(rows) != 4 {
			t.Fatalf("%q: %d conversation rows, want 4:\n%s", m, len(rows), visibleText(v))
		}
		for _, r := range rows {
			if idLikeRE.MatchString(r.Title) || strings.Contains(r.Title, "-4") {
				t.Errorf("%q: an id as title: %q", m, r.Title)
			}
			if !strings.Contains(r.Detail, "对话编号 ") {
				t.Errorf("%q: %q has no id in its detail: %q", m, r.Title, r.Detail)
			}
		}
		if txt := visibleText(v); strings.Contains(txt, "_unknown") || !strings.Contains(txt, noProjectText) {
			t.Errorf("%q: project not worded:\n%s", m, txt)
		}
		want := map[string]string{
			untitledUUID: "没有标题的对话 · 1 天前 09:30 · Claude Code",
			idTitleUUID:  "没有标题的对话 · 2025年10月28日 09:30 · Claude Code",
			promptUUID:   "帮我看看为什么 nightly 构建总是失败。",
			uuidClaude:   "Fix the checkout flow",
		}
		for _, r := range rows {
			for id, title := range want {
				if strings.Contains(r.Detail, id) && r.Title != title {
					t.Errorf("%q: %s titled %q, want %q", m, id, r.Title, title)
				}
			}
			if strings.Contains(r.Detail, idTitleUUID) && !strings.Contains(r.Subtitle, "notes") {
				t.Errorf("project from the working folder not shown: %q", r.Subtitle)
			}
		}
		checkTapNames(t, "conversations "+m, v)
	}
	for q, id := range map[string]string{untitledUUID: untitledUUID, "_unknown": untitledUUID,
		"nightly": promptUUID, "checkout": uuidClaude, idTitleUUID[:8]: idTitleUUID} {
		rows := conversationRows(sessionsSearch(t, a, "", q))
		found := false
		for _, r := range rows {
			found = found || strings.Contains(r.Detail, id)
		}
		if !found {
			t.Errorf("search %q does not find %s: %v", q, id, rows)
		}
	}
}

// The counts say what 还没核对过 and 网盘里找不到 mean, apart, and name a button the page draws.
func TestWhereaboutsCountsExplained(t *testing.T) {
	a := untitledApp(t)
	for _, m := range []string{"", "local"} {
		v := sessionsSearch(t, a, m, "")
		var stats []wire.Stat
		var note string
		for _, c := range v.Sections[0].Items {
			if c.Type == wire.ComponentStatGrid {
				stats = c.Stats
			}
			if strings.Contains(c.Text, "「还没核对过」") {
				note = c.Text
			}
		}
		got := map[string]string{}
		for _, s := range stats {
			got[s.Label] = s.Value
			if strings.Contains(s.Label, "/") {
				t.Errorf("%q: combined stat %q", m, s.Label)
			}
		}
		// seg-slow is unchecked (暂时不确定), seg-missing missing.
		if got["还没核对过"] != "1 个" || got[LocMissing] != "1 个" {
			t.Errorf("%q: stats %v", m, got)
		}
		for _, w := range []string{"不是丢了", "24 小时", "拿不回来"} {
			if !strings.Contains(note, w) {
				t.Errorf("%q: note lacks %q: %q", m, w, note)
			}
		}
		// One word for the state on the page: the stat's, not the MCP value.
		if txt := visibleText(v); strings.Contains(txt, LocUnknown) {
			t.Errorf("%q: the page still says %q:\n%s", m, LocUnknown, txt)
		}
		button := coldRefreshText
		if m != "" {
			button = "重新检查 " + localMachineName
		}
		if !strings.Contains(note, "点「"+button+"」") {
			t.Errorf("%q: note does not name %q: %q", m, button, note)
		}
		checkTapNames(t, "counts "+m, v)
	}
	// Nothing unchecked or missing: no note, and the stats say so.
	d := draw{st: &State{}, now: t0, loc: time.UTC}
	if n := d.whereaboutsNote(nil, 0, 0); n != "" {
		t.Errorf("note without anything to explain: %q", n)
	}
	if s := missingStat(0); s.Tone != "" || s.Value != "0 个" {
		t.Errorf("missing 0: %+v", s)
	}
}
