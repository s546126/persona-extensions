package main

import (
	"fmt"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/overview"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The overview's pages: a row on the start tab, the subpage "overview" (the patterns per facet, the latest forward
// check, the reports), and a report's full text ("ovr:weekly:<week>", "ovr:monthly:<month>"), which the pushed
// summary points to.

const (
	ovPerFacet  = 8 // patterns shown per facet on the subpage
	ovReports   = 8 // reports listed
	ovChunkMost = 1800
	ovMaxChunks = 100 // well below CheckView's 300-component limit
)

// overviewCard is the start tab's row opening the subpage.
func (a *app) overviewCard(t tr) wire.Section {
	sub := t.s("每周把零散条目里反复出现的取舍归纳成模式；周报全文也在这里。", "Weekly patterns across the scattered entries; the full weekly reports too.")
	var badge *wire.Badge
	if a.ov != nil {
		covered, _ := overview.Coverage(a.ov.Snapshot())
		badge = &wire.Badge{Text: t.f("%d/6 面", "%d/6 facets", covered), Tone: wire.ToneNeutral}
		if covered > 0 {
			badge.Tone = wire.TonePositive
		}
	}
	return wire.Section{Title: t.s("定期总览", "Periodic overview"), Items: []wire.Component{
		row(t.s("跨事模式与周报", "Cross-event patterns and reports"), sub, badge,
			ext.Open("open:overview", "", page{Tab: tabStart, Open: "overview"}, t.s("定期总览", "Periodic overview"))),
	}}
}

var lifecycleName = map[string][2]string{
	overview.Core:      {"稳定", "stable"},
	overview.Scoped:    {"有边界", "scoped"},
	overview.Candidate: {"观察中", "watching"},
	overview.Drifting:  {"在变", "drifting"},
	overview.EraClosed: {"阶段已结束", "era closed"},
	overview.Retired:   {"已退役", "retired"},
}

func lifecycleBadge(t tr, lc string) *wire.Badge {
	n := lifecycleName[lc]
	tone := wire.ToneNeutral
	switch lc {
	case overview.Core, overview.Scoped:
		tone = wire.TonePositive
	case overview.Drifting:
		tone = wire.ToneCaution
	}
	return &wire.Badge{Text: t.s(n[0], n[1]), Tone: tone}
}

// overviewPage is the subpage.
func (a *app) overviewPage(t tr) *wire.View {
	v := &wire.View{Title: t.s("定期总览", "Periodic overview")}
	if a.ov == nil {
		v.Sections = append(v.Sections, wire.Section{Items: []wire.Component{empty(t.s("总览的状态读不了，先关着；看 hub 日志", "The overview's state could not be read; see the hub's log"))}})
		return v
	}
	s := a.ov.Snapshot()
	covered, by := overview.Coverage(s)
	head := []wire.Component{secondary(t.f("六个方面里 %d 个已有确认的跨事模式。模式要在至少两个场景、三处独立证据、两周里出现，并经过反例检查才会用上。",
		"%d of six facets have a confirmed pattern. A pattern is used only once it shows in two scenes, three independent sources and two weeks, and passed a counter-example check.", covered))}
	if n := len(s.Holdouts); n > 0 {
		h := s.Holdouts[n-1]
		head = append(head, secondary(t.f("预测检验 %s（%d 题）：跨事模式 %.0f%%，散点条目 %.0f%%，不带记忆 %.0f%%",
			"Forward check %s (%d questions): patterns %.0f%%, scattered entries %.0f%%, no memory %.0f%%",
			h.Week, h.N, h.Rate[overview.ArmOverview]*100, h.Rate[overview.ArmScatter]*100, h.Rate[overview.ArmNone]*100)))
	}
	v.Sections = append(v.Sections, wire.Section{Items: head})
	for _, f := range overview.Facets {
		m := by[string(f)]
		ps := s.Sorted(func(p *overview.Pattern) bool { return p.Facet == string(f) && p.Lifecycle != overview.Retired })
		var items []wire.Component
		for i, p := range ps {
			if i >= ovPerFacet {
				items = append(items, secondary(t.f("……还有 %d 条", "…%d more", len(ps)-i)))
				break
			}
			sub := t.f("%d 个场景 · %d 处证据 · %d 周", "%d scenes · %d sources · %d weeks", p.ScenesCount, p.SourcesCount, len(p.ActiveWeeks))
			if p.Exception != "" {
				sub = t.s("例外：", "Unless: ") + p.Exception + " · " + sub
			}
			if len(p.Categories) > 0 {
				sub += " · " + strings.Join(p.Categories, "/")
			}
			items = append(items, row(short(p.Text, 120), sub, lifecycleBadge(t, p.Lifecycle), nil))
		}
		if len(items) == 0 {
			items = append(items, secondary(t.s("还没有：数据攒够跨场景的证据后会出现", "None yet: they appear once evidence spans scenes")))
		}
		v.Sections = append(v.Sections, wire.Section{
			Title:  facetLabelOf(t, string(f)),
			Footer: t.f("在用 %d，观察中 %d，在变 %d", "used %d, watching %d, drifting %d", m[overview.Core]+m[overview.Scoped], m[overview.Candidate], m[overview.Drifting]),
			Items:  items})
	}
	var rows []wire.Component
	for _, kind := range []string{"weekly", "monthly"} {
		for i, p := range overview.ReportsIn(a.c.St.Dir, kind) {
			if i >= ovReports {
				break
			}
			title := t.f("%s 周报", "Week %s", p)
			if kind == "monthly" {
				title = t.f("%s 月度巩固", "Month %s", p)
			}
			rows = append(rows, row(title, "", nil, ext.Open("open:ovr:"+kind+":"+p, "", page{Tab: tabStart, Open: "ovr:" + kind + ":" + p}, title)))
		}
	}
	if len(rows) == 0 {
		rows = append(rows, secondary(t.s("还没有报告", "No report yet")))
	}
	v.Sections = append(v.Sections, wire.Section{Title: t.s("报告全文", "Full reports"), Items: rows})
	return v
}

// reportPage is a report's full text, cut into schema-safe parts within CheckView limits.
func (a *app) reportPage(t tr, open string) *wire.View {
	kind, period, _ := strings.Cut(strings.TrimPrefix(open, "ovr:"), ":")
	body := overview.ReportIn(a.c.St.Dir, kind, period)
	v := &wire.View{Title: short(period, 120)}
	if strings.TrimSpace(body) == "" {
		v.Sections = append(v.Sections, wire.Section{Items: []wire.Component{empty(t.s("这份报告不见了", "This report is gone"))}})
		return v
	}
	parts := chunks(body, ovChunkMost)
	var items []wire.Component
	for i, part := range parts {
		if i >= ovMaxChunks {
			remaining := 0
			for _, rest := range parts[i:] {
				remaining += len([]rune(rest))
			}
			items = append(items, secondary(t.f("报告过长，后面还有约 %d 字未显示", "Report truncated; about %d more characters omitted.", remaining)))
			break
		}
		md := wire.Component{Type: wire.ComponentMarkdown, Text: part}
		if wire.CheckView(&wire.View{Sections: []wire.Section{{Items: []wire.Component{md}}}}) == nil {
			items = append(items, md)
		} else if plain := strings.TrimSpace(part); plain != "" {
			items = append(items, text(plain))
		}
	}
	if len(items) == 0 {
		items = []wire.Component{empty(t.s("这份报告不见了", "This report is gone"))}
	}
	v.Sections = append(v.Sections, wire.Section{Items: items})
	return v
}

// chunks cuts text at line ends into parts of at most n characters (a longer line is cut where it must).
func chunks(text string, n int) []string {
	var out []string
	var cur []rune
	for _, line := range strings.SplitAfter(text, "\n") {
		r := []rune(line)
		for len(r) > n {
			if len(cur) > 0 {
				out, cur = append(out, string(cur)), nil
			}
			out, r = append(out, string(r[:n])), r[n:]
		}
		if len(cur)+len(r) > n {
			out, cur = append(out, string(cur)), nil
		}
		cur = append(cur, r...)
	}
	if strings.TrimSpace(string(cur)) != "" {
		out = append(out, string(cur))
	}
	return out
}

func facetLabelOf(t tr, f string) string {
	names := map[string][2]string{
		"interaction": {"和 AI 打交道", "Working with the AI"}, "tools": {"AI 和工具", "The AI and tools"},
		"judgement": {"默认判断", "Default judgements"}, "workflow": {"做事方式", "How things are done"},
		"world": {"人、项目和事实", "People, projects and facts"}, "persona": {"关于你", "About you"},
	}
	n, ok := names[f]
	if !ok {
		return fmt.Sprint(f)
	}
	return t.s(n[0], n[1])
}
