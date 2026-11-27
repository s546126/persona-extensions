package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

// viewText renders a view as readable text: what a user reads on the page, in order. It backs the copy tests and,
// with SPACES_DUMP_DIR set, TestDumpPages writes it to files for review.
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
			componentText(it, line)
		}
		if sec.More != nil {
			line(1, "[按钮 %s]", sec.More.Title)
		}
		if sec.Footer != "" {
			line(1, "(脚注) %s", sec.Footer)
		}
	}
	return b.String()
}

func componentText(it wire.Component, line func(int, string, ...any)) {
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
	case wire.ComponentStatGrid:
		var ss []string
		for _, s := range it.Stats {
			v := s.Label + "：" + s.Value
			if s.Detail != "" {
				v += "（" + s.Detail + "）"
			}
			ss = append(ss, v)
		}
		line(1, "%s", strings.Join(ss, " · "))
	case wire.ComponentProgress:
		line(1, "[进度条] %s %s", it.Label, it.Detail)
	case wire.ComponentLink:
		line(1, "[链接 %s]", it.Title)
	case wire.ComponentTable:
		var cs []string
		for _, c := range it.Columns {
			cs = append(cs, c.Title)
		}
		line(1, "| %s |", strings.Join(cs, " | "))
		for _, r := range it.Rows {
			line(1, "| %s |", strings.Join(r, " | "))
		}
	case wire.ComponentSegmented:
		var cs []string
		for _, c := range it.Choices {
			s := c.Title
			if c.Value == it.Value {
				s = "*" + s + "*"
			}
			cs = append(cs, s)
		}
		line(1, "[切换] %s", strings.Join(cs, " | "))
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

// pageState is one page in one state, rendered for review and for the copy tests.
type pageState struct {
	name  string
	text  string
	view  *wire.View
	toast string
}

// idOf is the id of the first entity of st titled title.
func idOf(t *testing.T, st kernel.Storage, title string) string {
	t.Helper()
	id := ""
	_ = st.View(func(b *kernel.Base) error {
		for _, sp := range b.Spaces {
			for _, e := range sp.Entities {
				if e != nil && e.Title == title && id == "" {
					id = e.ID
				}
			}
		}
		return nil
	})
	if id == "" {
		t.Fatalf("no entity %q", title)
	}
	return id
}

// pageStates renders every page state the fixtures reach: nothing created, the template list and a template's form,
// a space with entries (overview, tabs, entity page, editor), empty spaces and types, the overview card, and the
// toasts of refused actions.
func pageStates(t *testing.T) []pageState {
	t.Helper()
	var out []pageState
	add := func(name string, v *wire.View, toast string) {
		t.Helper()
		if v != nil {
			checkView(t, name, v, false)
		}
		text := viewText(v)
		if toast != "" {
			text = "(提示条) " + toast + "\n" + text
		}
		out = append(out, pageState{name: name, text: text, view: v, toast: toast})
	}
	card := func(a *App) *wire.View {
		v, err := a.Card(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
			Contribution: CardUI, Placement: "overview_card", Size: "compact", UIVersion: 5, TZ: "Asia/Shanghai",
		}})
		if err != nil {
			t.Fatal(err)
		}
		checkView(t, "card", v, true)
		return v
	}
	refused := func(name string, a *App, s state, action string, values map[string]string) {
		t.Helper()
		res := act(t, a, s, action, values)
		add(name, res.View, res.Toast)
	}

	// 1. Nothing created yet.
	empty := newApp(storeWith(t, true))
	add("01-empty-page", drawPage(t, empty, state{}, 5), "")
	add("01-empty-card", card(empty), "")
	add("01-empty-template-form", drawPage(t, empty, state{C: "reading"}, 5), "")
	add("01-empty-template-form-blank", drawPage(t, empty, state{C: "blank"}, 5), "")

	// 2. One space with entries: the reading template with its samples.
	st := storeWith(t, true, "reading")
	a := newApp(st)
	sp := firstSpace(t, st)
	add("02-space-overview", drawPage(t, a, state{S: sp}, 5), "")
	add("02-space-settings", drawPage(t, a, state{S: sp, F: "space"}, 5), "")
	add("02-space-new-space-list", drawPage(t, a, state{S: sp, C: pickSlot}, 5), "")
	add("02-tab-book-progress", drawPage(t, a, state{S: sp, T: "book"}, 5), "")
	add("02-tab-book-board", drawPage(t, a, state{S: sp, T: "book", V: 1}, 5), "")
	add("02-tab-book-quick-add", drawPage(t, a, state{S: sp, T: "book", F: "new"}, 5), "")
	add("02-tab-note-feed", drawPage(t, a, state{S: sp, T: "note"}, 5), "")
	add("02-tab-clip-table", drawPage(t, a, state{S: sp, T: "clip"}, 5), "")
	add("02-tab-book-search-none", drawPage(t, a, state{S: sp, T: "book", Q: "没有这本"}, 5), "")
	book := idOf(t, st, "《慢读的方法》")
	add("02-entity-book", drawPage(t, a, state{S: sp, E: book}, 5), "")
	add("02-entity-book-edit", drawPage(t, a, state{S: sp, E: book, F: "edit"}, 5), "")
	add("02-entity-book-derive", drawPage(t, a, state{S: sp, E: book, F: "derive"}, 5), "")
	add("02-entity-note", drawPage(t, a, state{S: sp, E: idOf(t, st, "先写自己的话，再抄原文")}, 5), "")
	add("02-card", card(a), "")

	// 3. The editor of a type, closed and with each fold open, and the form of a new type.
	add("03-editor-book", drawPage(t, a, state{S: sp, D: "book"}, 5), "")
	for _, f := range []string{"name", "stage:0", "stage:+", "prop:author", "prop:notes", "addprop", "new:relation", "new:rollup", "card", "view:0", "view:+"} {
		add("03-editor-book-"+strings.ReplaceAll(strings.ReplaceAll(f, ":", "-"), "+", "new"), drawPage(t, a, state{S: sp, D: "book", F: f}, 5), "")
	}
	add("03-editor-note-new-rollup", drawPage(t, a, state{S: sp, D: "note", F: "new:rollup"}, 5), "")
	add("03-editor-new-type", drawPage(t, a, state{S: sp, D: "+"}, 5), "")

	// 4. Empty spaces and types: a blank space, and a template without samples.
	blank := newApp(storeWith(t, false, "blank"))
	add("04-blank-space", drawPage(t, blank, state{}, 5), "")
	subs := storeWith(t, false, "subscription")
	sa := newApp(subs)
	add("04-subscription-no-samples", drawPage(t, sa, state{S: firstSpace(t, subs)}, 5), "")
	add("04-subscription-tab-empty", drawPage(t, sa, state{S: firstSpace(t, subs), T: "sub"}, 5), "")

	// 5. A second space: the filter and the card over two spaces.
	two := storeWith(t, true, "reading", "subscription")
	ta := newApp(two)
	add("05-two-spaces-overview", drawPage(t, ta, state{}, 5), "")
	add("05-two-spaces-card", card(ta), "")

	// 6. Refused actions: their toasts, over the page they leave as it was.
	refused("06-error-add-empty-title", a, state{S: sp, T: "book", F: "new"}, "add", map[string]string{"title": ""})
	refused("06-error-type-key-taken", a, state{S: sp, D: "+"}, "typenew", map[string]string{"t_key": "book", "t_name": "书"})
	refused("06-error-type-key-bad", a, state{S: sp, D: "+"}, "typenew", map[string]string{"t_key": "Book!", "t_name": "书"})
	refused("06-error-type-name-empty", a, state{S: sp, D: "+"}, "typenew", map[string]string{"t_key": "film", "t_name": ""})
	refused("06-error-status-jump", a, state{S: sp, E: idOf(t, st, "《给笔记一个结构》")}, "status:读完", nil)
	refused("06-error-stage-in-use", a, state{S: sp, D: "book", F: "stage:1"}, "stagedrop:1:在读", nil)
	refused("06-error-prop-in-use", a, state{S: sp, D: "book", F: "prop:author"}, "propdrop:author", nil)
	refused("06-error-pill-unknown", a, state{S: sp, D: "book", F: "card"}, "cardsave", map[string]string{"c_pills": "封面"})
	refused("06-error-number-bad", a, state{S: sp, E: book, F: "edit"}, "update", map[string]string{"p_pages": "很多"})
	refused("06-error-tab-gone", a, state{S: sp}, "tab:film", nil)
	refused("06-error-samples-bad", empty, state{C: "reading"}, "create:reading", map[string]string{"samples": "maybe"})
	refused("06-error-stale-stage", a, state{S: sp, D: "book", F: "stage:0"}, "stagesave:0:旧名字", map[string]string{"stage_name": "x"})
	return out
}

// TestDumpPages writes every page state as text into $SPACES_DUMP_DIR (skipped without it).
func TestDumpPages(t *testing.T) {
	dir := os.Getenv("SPACES_DUMP_DIR")
	if dir == "" {
		t.Skip("set SPACES_DUMP_DIR to write the page dumps")
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
