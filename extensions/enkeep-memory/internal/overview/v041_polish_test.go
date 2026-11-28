package overview

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/profile"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// A bookkeeping contract ("整理财务账目时按用户口径核对借贷") that an older detector labelled finance is a working rule:
// the profile does not carry it into profile:cat:finance. A contract whose evidence the user labelled finance keeps
// it, and a persona pattern keeps its category as stored.
func TestEffectiveCategoriesDropsStaleWorkFinance(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		"i-book": {ID: "i-book", Facet: entry.Interaction, Grain: entry.Action, Status: entry.Stable, Origin: entry.Behavior, Tagged: true,
			Text: "整理财务收支时，助手按用户指定的口径准确分类", Count: 2,
			Categories: []entry.Category{{Name: entry.CatFinance, By: entry.ByModel}}},
		"i-user": {ID: "i-user", Facet: entry.Interaction, Grain: entry.Action, Status: entry.Stable, Origin: entry.Behavior, Tagged: true,
			Pinned: true, Text: "核对报销单时先对齐口径", Count: 2,
			Categories: []entry.Category{{Name: entry.CatFinance, By: entry.ByUser}}},
	}}
	s := newState()
	s.Patterns["ov:book"] = &Pattern{ID: "ov:book", Facet: string(entry.Interaction), Kind: KindContract, Lifecycle: Core,
		Text:      "在整理财务账目时，助手按用户指定的口径、对象归类和合并方式计算，核对借贷方向与往来余额，不漏记不误判。",
		Situation: "整理财务收支或核对流水", Choice: "按用户口径核对并采纳其说明",
		EvidenceIDs: []string{"i-book"}, Categories: []string{entry.CatFinance}, ScenesCount: 1, SourcesCount: 9}
	s.Patterns["ov:userfin"] = &Pattern{ID: "ov:userfin", Facet: string(entry.Interaction), Kind: KindContract, Lifecycle: Core,
		Text: "核对报销单时，助手先和用户对齐口径", EvidenceIDs: []string{"i-user"}, Categories: []string{entry.CatFinance},
		ScenesCount: 1, SourcesCount: 2}
	s.Patterns["ov:persona"] = &Pattern{ID: "ov:persona", Facet: string(entry.Persona), Lifecycle: Core,
		Text: "用户把退休目标定为一笔净资产", Categories: []string{entry.CatFinance}, ScenesCount: 2, SourcesCount: 2}

	if got := effectiveCategories(st, s.Patterns["ov:book"]); len(got) != 0 {
		t.Errorf("bookkeeping contract keeps %v", got)
	}
	if got := effectiveCategories(st, s.Patterns["ov:userfin"]); !slices.Equal(got, []string{entry.CatFinance}) {
		t.Errorf("user-labelled contract lost its category: %v", got)
	}
	if got := effectiveCategories(st, s.Patterns["ov:persona"]); !slices.Equal(got, []string{entry.CatFinance}) {
		t.Errorf("persona pattern lost its category: %v", got)
	}

	items := OverviewBackedProvider{Snapshot: func() *State { return s }}.Items(st)
	for _, it := range items {
		if it.ID == "ov:book" && len(it.Categories) != 0 {
			t.Errorf("bookkeeping contract item carries %v into the profile", it.Categories)
		}
	}
	sum := profile.BuildWith(st, OverviewBackedProvider{Snapshot: func() *State { return s }}, time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), "zh-Hans")
	for _, ssec := range sum.Sensitive {
		for _, pt := range ssec.Points {
			if pt.ID == "ov:book" {
				t.Errorf("bookkeeping contract in profile:cat:%s", ssec.Category)
			}
		}
	}
}

// A pattern with two ends stands as "A时→X；B时→Y" with no "（两端：…）" wrapper; a statement that only sums up its ends
// is not said twice; one that adds something keeps its statement before the ends.
func TestStandingPartsCompactPoles(t *testing.T) {
	restated := &Pattern{Text: "方案未定时先出方案并对齐推进方式；方向已定时按推荐做法持续推进并分阶段汇报。",
		Poles: []Pole{{When: "实现方式或方向尚未确定时", Choice: "先出方案、对齐推进方式，再进入实现"},
			{When: "事项与方向已由用户定下时", Choice: "持续推进并分阶段汇报，不停下等批准"}}}
	body, suffix := StandingParts(restated)
	if suffix != "" || strings.Contains(body, "两端") || strings.Contains(body, "方案未定时先出方案") ||
		body != "实现方式或方向尚未确定时→先出方案、对齐推进方式，再进入实现；事项与方向已由用户定下时→持续推进并分阶段汇报，不停下等批准。" {
		t.Errorf("restated poles: body=%q suffix=%q", body, suffix)
	}
	full := strings.TrimSpace(restated.Text) + suffixOf(restated)
	if len(body) >= len(full) {
		t.Errorf("compact line (%dB) not shorter than the old form (%dB)", len(body), len(full))
	}

	adds := &Pattern{Text: "遇到需要深入分析的任务，用户交给多个子智能体做饱和分析，再自己复核并与人讨论。",
		Poles: []Pole{{When: "需要系统分析多个角度的问题时", Choice: "让子智能体并行做多维度深度分析"},
			{When: "分析需要理解完整背景的人员行为时", Choice: "优先完整阅读原始材料"}}}
	body, suffix = StandingParts(adds)
	if suffix != "" || !strings.HasPrefix(body, "遇到需要深入分析的任务，用户交给多个子智能体做饱和分析，再自己复核并与人讨论：") ||
		!strings.Contains(body, "需要系统分析多个角度的问题时→让子智能体并行做多维度深度分析；分析需要理解完整背景的人员行为时→优先完整阅读原始材料") ||
		strings.Contains(body, "两端") {
		t.Errorf("poles with a statement: body=%q suffix=%q", body, suffix)
	}

	plain := &Pattern{Text: "上线前先灰度。", Exception: "紧急回滚时直接全量"}
	if body, suffix := StandingParts(plain); body != "上线前先灰度。" || suffix != "（例外：紧急回滚时直接全量）" {
		t.Errorf("no poles: body=%q suffix=%q", body, suffix)
	}
}
