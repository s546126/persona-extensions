package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// viewText renders a view as readable text: what a user reads on the page, in order. It backs the copy tests
// and, with SAC_DUMP_DIR set, TestDumpPages writes it to files for review.
func viewText(v *wire.View) string {
	var b strings.Builder
	line := func(indent int, format string, args ...any) {
		b.WriteString(strings.Repeat("  ", indent))
		fmt.Fprintf(&b, format, args...)
		b.WriteByte('\n')
	}
	if v == nil {
		return "(no view)\n"
	}
	if v.Title != "" {
		line(0, "# %s", v.Title)
	}
	if v.Filter != nil {
		var cs []string
		for _, c := range v.Filter.Choices {
			cs = append(cs, c.Title)
		}
		line(0, "[筛选 %s] %s", v.Filter.Title, strings.Join(cs, " | "))
	}
	if v.Search != nil {
		line(0, "[搜索框] %s", v.Search.Placeholder)
	}
	if v.Tabs != nil {
		var ts []string
		for _, t := range v.Tabs.Items {
			s := t.Title
			if t.Badge != "" {
				s += "(" + t.Badge + ")"
			}
			if t.Value == v.Tabs.Value {
				s = "*" + s + "*"
			}
			ts = append(ts, s)
		}
		line(0, "[标签页] %s", strings.Join(ts, " | "))
	}
	for _, sec := range v.Sections {
		b.WriteByte('\n')
		if sec.Title != "" {
			line(0, "## %s", sec.Title)
		}
		for _, it := range sec.Items {
			componentText(&b, it, line)
		}
		if sec.Footer != "" {
			line(1, "(脚注) %s", sec.Footer)
		}
	}
	return b.String()
}

func componentText(b *strings.Builder, it wire.Component, line func(int, string, ...any)) {
	badge := func(bd *wire.Badge) string {
		if bd == nil {
			return ""
		}
		return " [" + bd.Text + "]"
	}
	switch it.Type {
	case wire.ComponentText:
		line(1, "%s", it.Text)
	case wire.ComponentHeader:
		line(1, "【%s】 %s", it.Title, it.Subtitle)
		for _, bd := range it.Badges {
			line(2, "[%s]", bd.Text)
		}
		for _, f := range it.Facts {
			line(2, "%s：%s", f.Label, f.Value)
		}
	case wire.ComponentRow:
		s := "- " + it.Title + badge(it.Badge)
		if it.Value != "" {
			s += "  " + it.Value
		}
		line(1, "%s", s)
		if it.Subtitle != "" {
			line(2, "%s", it.Subtitle)
		}
		for _, ra := range it.Actions {
			line(2, "(滑动操作) %s", ra.Title)
			if ra.Action.Confirm != "" {
				line(3, "(确认) %s", ra.Action.Confirm)
			}
		}
	case wire.ComponentStatGrid:
		var ss []string
		for _, s := range it.Stats {
			ss = append(ss, s.Label+"："+s.Value)
		}
		line(1, "%s", strings.Join(ss, " · "))
	case wire.ComponentCode:
		line(1, "```%s", it.Path)
		for _, l := range strings.Split(it.Text, "\n") {
			line(1, "%s", l)
		}
		line(1, "```")
	case wire.ComponentTable:
		var cs []string
		for _, c := range it.Columns {
			cs = append(cs, c.Title)
		}
		line(1, "| %s |", strings.Join(cs, " | "))
		for _, r := range it.Rows {
			line(1, "| %s |", strings.Join(r, " | "))
		}
	case wire.ComponentField:
		s := "[输入 " + it.Label + "]"
		if it.Placeholder != "" {
			s += " 提示：" + it.Placeholder
		}
		if it.Value != "" {
			s += " 值：" + it.Value
		}
		line(1, "%s", s)
		if len(it.Choices) > 0 {
			var cs []string
			for _, c := range it.Choices {
				cs = append(cs, c.Title)
			}
			line(2, "选项：%s", strings.Join(cs, " | "))
		}
		if it.Detail != "" {
			line(2, "%s", it.Detail)
		}
	case wire.ComponentButton:
		s := "[按钮 " + it.Title + "]"
		if it.Action != nil && it.Action.Confirm != "" {
			s += " (确认：" + it.Action.Confirm + ")"
		}
		line(1, "%s", s)
	case wire.ComponentEmpty:
		line(1, "（空）%s", it.Title)
		if it.Detail != "" {
			line(2, "%s", it.Detail)
		}
		if it.Button != nil {
			line(2, "[按钮 %s]", it.Button.Title)
		}
	default:
		line(1, "<%s> %s %s %s", it.Type, it.Title, it.Text, it.Subtitle)
	}
}

var tmpPath = regexp.MustCompile(`(/private)?` + regexp.QuoteMeta(filepath.Clean(os.TempDir())) + `/[^/\s，,)）]*/\d+`)

// pageState is one page in one state, rendered for review and for the copy tests.
type pageState struct {
	name       string
	text       string
	view       *wire.View // nil when the action failed
	err, toast string
}

// pageStates renders every page state the fixtures reach: nothing set up, set up and idle, results, no results,
// a restore running, errors, and several computers.
func pageStates(t *testing.T) []pageState {
	t.Helper()
	ctx := context.Background()
	var out []pageState
	add := func(name string, v *wire.View, err error, toast string) {
		t.Helper()
		var text, errText string
		if err != nil {
			errText = err.Error()
			text = "(错误) " + errText + "\n"
		} else {
			if cerr := wire.CheckView(v); cerr != nil {
				t.Fatalf("%s: CheckView: %v", name, cerr)
			}
			text = viewText(v)
		}
		if toast != "" {
			text = "(提示条) " + toast + "\n" + text
		}
		out = append(out, pageState{name: name, text: tmpPath.ReplaceAllString(text, "<tmp>"), view: v, err: errText, toast: toast})
	}
	render := func(a *app, p page) (*wire.View, error) {
		return a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(p)}})
	}
	act := func(a *app, p page, action string, values map[string]string) (*wire.ActionResult, error) {
		return a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(p), Action: action, Values: values}})
	}

	// 1. Nothing set up: no transcripts found anywhere.
	empty := newApp(t.TempDir())
	empty.now = func() time.Time { return t0 }
	for _, tab := range []string{"results", "sources"} {
		v, err := render(empty, page{Tab: tab})
		add("01-empty-"+tab, v, err, "")
	}
	cv, err := empty.card(ctx, &ext.UIRequest{})
	add("01-empty-card", cv, err, "")

	// 2. Set up and idle: transcripts are there, nothing was searched yet.
	st, _, _ := seedColdFixtures(t)
	a := newApp(t.TempDir())
	a.now = func() time.Time { return t0 }
	_ = a.db().update(func(s *State, _ time.Time) error {
		s.SessionDirs, s.RepoDirs, s.ColdTextDirs = st.SessionDirs, st.RepoDirs, st.ColdTextDirs
		return nil
	})
	for _, tab := range []string{"results", "code", "recipes", "sources"} {
		v, err := render(a, page{Tab: tab})
		add("02-idle-"+tab, v, err, "")
	}
	cv, err = a.card(ctx, &ext.UIRequest{})
	add("02-idle-card", cv, err, "")

	// 3. Results: a typed search, its detail page and the recipe form.
	res, err := act(a, page{}, "search", map[string]string{"q": "quota"})
	add("03-results", res.View, err, res.Toast)
	v, err := render(a, page{Tab: "code", Query: "quota"})
	add("03-results-code", v, err, "")
	v, err = render(a, page{Mode: modeHit, HitID: "session:sess-claude-01", Query: "quota"})
	add("03-results-detail", v, err, "")
	res, err = act(a, page{}, "search", map[string]string{"q": "zephyrquill"})
	add("03-results-archived", res.View, err, res.Toast)
	v, err = render(a, page{Mode: modeHit, HitID: "session:sess-cold-01", Query: "zephyrquill"})
	add("03-results-archived-detail", v, err, "")
	v, err = render(a, page{Mode: modeRecipeEdit})
	add("03-recipe-new", v, err, "")
	res, err = act(a, page{}, "run-recipe", map[string]string{"id": "error-triage"})
	add("03-recipe-run", res.View, err, res.Toast)
	cv, err = a.card(ctx, &ext.UIRequest{})
	add("03-results-card", cv, err, "")

	// 4. No results.
	res, err = act(a, page{}, "search", map[string]string{"q": "nothing-matches-this-xyz"})
	add("04-no-results", res.View, err, res.Toast)

	// 5. A restore running, one done and one failed (searches themselves finish before the page answers).
	_ = a.db().update(func(s *State, now time.Time) error {
		s.setRestore(RestoreNotice{EntryID: "entry-failed", Title: "Failed restore", Path: "/work/a.jsonl", Message: "云端不可达，稍后再试", StartedAt: now, At: now})
		s.setRestore(RestoreNotice{EntryID: "entry-done", Title: "Done restore", Path: "/work/b.jsonl", OK: true, Resume: "claude --resume sess-b", StartedAt: now, At: now})
		s.setRestore(RestoreNotice{EntryID: "entry-cold-01", Title: "Orbit planner", Path: "/work/c.jsonl", Running: true, StartedAt: now})
		return nil
	})
	v, err = render(a, page{Query: "zephyrquill"})
	add("05-restore-running", v, err, "")

	// 6. Errors: code that does not parse, a broken saved search, a computer that is gone.
	_, err = act(a, page{Tab: "code"}, "run-code", map[string]string{"custom_code": "scan_sessions(query=\"x\") |> frobnicate()"})
	add("06-error-bad-code", nil, err, "")
	_, err = act(a, page{Mode: modeRecipeEdit}, "save-recipe", map[string]string{"id": "Bad ID", "name": "x", "code": "scan_sessions()"})
	add("06-error-bad-recipe", nil, err, "")
	_, err = act(a, page{}, "run-recipe", map[string]string{"id": "no-such-recipe"})
	add("06-error-no-recipe", nil, err, "")
	v, err = render(a, page{Mode: modeHit, HitID: "session:not-there"})
	add("06-error-hit-gone", v, err, "")

	// 7. Several computers: one online, one offline with a synced copy, one never synced.
	ma, tf, _ := newAPIApp(t)
	tf.f.syncAll(ctx)
	tf.nodes.setDown("m-3c4d", true)
	res, err = act(ma, page{}, "search", map[string]string{"q": "quota"})
	add("07-machines-results", res.View, err, res.Toast)
	for _, tab := range []string{"sources", "code"} {
		v, err := render(ma, page{Tab: tab, Query: "quota"})
		add("07-machines-"+tab, v, err, "")
	}
	cv, err = ma.card(ctx, &ext.UIRequest{})
	add("07-machines-card", cv, err, "")

	// 8. Computers picked, nothing to read on the hub's own computer: before the first search reached them, when
	// none of them answered, and when they answered but have no chats.
	for _, c := range []struct {
		name   string
		online bool
		search bool
	}{
		{"08-picked-not-searched", true, false},
		{"09-picked-none-answered", false, true},
		{"10-picked-no-chats", true, true},
	} {
		fa := emptyFleetApp(t, c.online)
		if c.search {
			res, err := act(fa, page{}, "reset", nil)
			add(c.name+"-results", res.View, err, res.Toast)
		} else {
			v, err := render(fa, page{})
			add(c.name+"-results", v, err, "")
		}
		cv, err := fa.card(ctx, &ext.UIRequest{})
		add(c.name+"-card", cv, err, "")
	}
	return out
}

// emptyFleetApp is a hub with no chats of its own and two picked computers, both online (with empty chat
// folders) or both offline and never synced.
func emptyFleetApp(t *testing.T, online bool) *app {
	t.Helper()
	nodes := &fakeNodes{workers: map[string]*nodeWorker{}, down: map[string]bool{}}
	lister := &fakeLister{}
	for _, m := range []hubMachine{{ID: "m-1a2b", Name: "Laptop 2"}, {ID: "m-3c4d", Name: "desktop"}} {
		w := newNodeWorker(t.TempDir(), configSpec{SessionDirs: t.TempDir()})
		w.now = func() time.Time { return t0 }
		nodes.workers[m.ID] = w
		nodes.down[m.ID] = !online
		m.Online = online
		lister.machines = append(lister.machines, m)
	}
	f := newFleet(nodes, lister, t.TempDir(), []string{"m-1a2b", "m-3c4d"})
	f.now = func() time.Time { return t0 }
	a := newApp(t.TempDir())
	a.now = func() time.Time { return t0 }
	a.fleet = f
	return a
}

// TestDumpPages writes every page state as text into $SAC_DUMP_DIR (skipped without it).
func TestDumpPages(t *testing.T) {
	dir := os.Getenv("SAC_DUMP_DIR")
	if dir == "" {
		t.Skip("set SAC_DUMP_DIR to write the page dumps")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range pageStates(t) {
		if err := os.WriteFile(filepath.Join(dir, s.name+".txt"), []byte(s.text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
