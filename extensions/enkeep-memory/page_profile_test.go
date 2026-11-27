package main

import (
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/profile"
)

func TestProfilePageBlankUser(t *testing.T) {
	a := v4App(t)
	a.c.St.Lock()
	a.c.Publish()
	a.c.St.Unlock()

	// Start page shows the profile entry card with 0/6 facets.
	_, rawStart := render(t, a, page{Tab: tabStart}, "zh-Hans")
	if !strings.Contains(rawStart, "我是怎样的人") || !strings.Contains(rawStart, "已覆盖 0/6 面") {
		t.Fatalf("start page missing blank profile card: %s", rawStart)
	}

	// Subpage renders cleanly in both zh-Hans and en.
	_, rawZh := render(t, a, page{Tab: tabStart, Open: "profile"}, "zh-Hans")
	if !strings.Contains(rawZh, "无需导入外部画像") {
		t.Errorf("blank profile page missing new-user guidance: %s", rawZh)
	}
	_, rawEn := render(t, a, page{Tab: tabStart, Open: "profile"}, "en")
	if !strings.Contains(rawEn, "No external profile seed is needed") {
		t.Errorf("blank en profile page missing guidance: %s", rawEn)
	}
}

func TestProfilePageSixFacetsSensitiveAndRefresh(t *testing.T) {
	a := v4App(t)
	seed(a)
	// Add stable entries for all 6 facets plus one sensitive entry.
	a.c.St.Lock()
	a.c.Observe(&entry.Entry{
		Facet: entry.Judgement, Grain: entry.Class, Text: "变更先灰度再全量", Status: entry.Stable,
		Count: 3, Last: "2026-11-23", Tagged: true,
		Impact:   entry.Impact{Moment: "ask_user", Action: "suggest canary"},
		Evidence: ev(entry.SrcAssistant, "2026-11-23", "先灰度"),
	}, "test")
	a.c.Observe(&entry.Entry{
		Facet: entry.Workflow, Grain: entry.Class, Text: "合并前在测试机跑全量门禁", Status: entry.Stable,
		Count: 3, Last: "2026-11-23", Tagged: true,
		Impact:   entry.Impact{Moment: "task_start", Action: "run gate"},
		Evidence: ev(entry.SrcAssistant, "2026-11-23", "先跑门禁"),
	}, "test")
	a.c.Observe(&entry.Entry{
		Facet: entry.Persona, Grain: entry.Action, Text: "每晚十一点前休息", Status: entry.Stable,
		Count: 3, Last: "2026-11-23", Tagged: true, OriginSpace: "health",
		Categories: []entry.Category{{Name: entry.CatHealth, By: entry.ByModel}},
		Impact:     entry.Impact{Moment: "every_turn", Action: "respect schedule"},
		Evidence:   ev(entry.SrcAssistant, "2026-11-23", "早点睡"),
	}, "test")
	a.c.Publish()
	a.c.St.Unlock()

	_, raw := render(t, a, page{Tab: tabStart, Open: "profile"}, "zh-Hans")
	for _, want := range []string{
		"覆盖 6/6 个方面",
		"我是怎样的人（总览）",
		"用平台更丝滑",
		"理解更准",
		"受控敏感侧写（按场景分级）",
		"锁屏、外发、派活与第三方场景中不出",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("profile subpage missing %q", want)
		}
	}

	// Trigger profile:refresh action and verify profile-summary.json is saved.
	res, _ := act(t, a, page{Tab: tabStart, Open: "profile"}, "profile:refresh", nil, "zh-Hans")
	if res == nil || !strings.Contains(res.Toast, "已按当前稳定条目刷新侧写汇总") {
		t.Fatalf("unexpected refresh result: %+v", res)
	}
	loaded, err := profile.Load(a.c.St.Dir)
	if err != nil || loaded == nil || loaded.CoveredCount != 6 {
		t.Fatalf("profile-summary.json not persisted cleanly: %+v err=%v", loaded, err)
	}
}
