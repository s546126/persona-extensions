package recall

import (
	"slices"
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// Synthetic entries only (占位, host-a).
func TestSearchPoolAndRanking(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{}, Onboarding: &entry.Onboarding{Use: entry.Work}}
	add := func(id string, f entry.Facet, s entry.Status, text string) *entry.Entry {
		e := &entry.Entry{ID: id, Facet: f, Status: s, Text: text, Count: 1, Last: "2026-01-02"}
		st.Entries[id] = e
		return e
	}
	add("t-1", entry.Tools, entry.Stable, "连 host-a 要走占位跳板").Object = "host-a"
	add("t-2", entry.Tools, entry.Dormant, "host-a 以前用占位旧端口").Object = "host-a"
	add("t-3", entry.Tools, entry.Retired, "host-a 退役的占位说法")
	add("t-4", entry.Tools, entry.Stable, "host-a 的占位敏感事项").Sensitive = entry.SensFamily
	add("t-5", entry.Tools, entry.Stable, "host-a 周末占位生活安排").Domain = entry.Life
	inf := add("p-1", entry.People, entry.Stable, "推断 host-a 的管理员脾气急（占位）")
	inf.Part = entry.PartInferred
	ed := add("w-1", entry.Workflow, entry.Stable, "原文")
	ed.Export = &entry.ExportText{Text: "改写过的占位 host-a 做法"}

	res := Search(st, Query{Text: "host-a"})
	var ids []string
	for _, h := range res.Hits {
		ids = append(ids, h.ID)
	}
	got := strings.Join(ids, ",")
	if !strings.HasPrefix(got, "t-1") || !strings.Contains(got, "t-2") || !strings.Contains(got, "w-1") {
		t.Fatalf("hits %s", got)
	}
	for _, not := range []string{"t-3", "t-4", "t-5", "p-1"} {
		if strings.Contains(got, not) {
			t.Errorf("%s returned", not)
		}
	}
	for _, h := range res.Hits {
		if h.ID == "w-1" && h.Text != "改写过的占位 host-a 做法" {
			t.Errorf("exit wording not used: %q", h.Text)
		}
	}
	if r := Search(st, Query{Text: "host-a", Facets: []entry.Facet{entry.Workflow}}); len(r.Hits) != 1 {
		t.Errorf("facet filter: %+v", r.Hits)
	}
	// Scene 3: an entry in a category only for a look-up that names it (all of its categories); never an inference.
	named := Search(st, Query{Text: "host-a", Categories: []string{"relationships"}})
	var t4 *Hit
	for i, h := range named.Hits {
		if h.ID == "t-4" {
			t4 = &named.Hits[i]
		}
		if h.ID == "p-1" {
			t.Error("an inference returned for a named category")
		}
	}
	if t4 == nil || !strings.Contains(Format(*t4), "分类 relationships") {
		t.Errorf("named category: %+v", named.Hits)
	}
	if r := Search(st, Query{Text: "host-a", Categories: []string{"health"}}); slices.ContainsFunc(r.Hits, func(h Hit) bool { return h.ID == "t-4" }) {
		t.Error("another category's name returned it")
	}
	if r := Search(st, Query{Text: "   "}); len(r.Hits) != 0 {
		t.Error("an empty query found something")
	}
	if !strings.Contains(Format(res.Hits[0]), "[机器和工具·稳定]") || !strings.Contains(Format(res.Hits[0]), "id:t-1") {
		t.Errorf("format: %q", Format(res.Hits[0]))
	}
}

// A rule's line carries when it does not hold (0.2.6); a fact's and the user's own wording do not.
func TestLineException(t *testing.T) {
	e := &entry.Entry{Facet: entry.Interaction, Text: "做完一轮不要停下等批准。", Exception: "只有涉及线上、删数据时才停。"}
	if got := Line(e); got != "做完一轮不要停下等批准。（例外：只有涉及线上、删数据时才停）" {
		t.Errorf("line %q", got)
	}
	e.User = &entry.UserEdit{Text: "the user's words"}
	if got := Line(e); got != "the user's words" {
		t.Errorf("the user's own line %q", got)
	}
	if got := Line(&entry.Entry{Facet: entry.World, Text: "t", Exception: "x"}); got != "t" {
		t.Errorf("a fact %q", got)
	}
}
