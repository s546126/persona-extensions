package profile

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

type fakeModel struct {
	calls  int
	resp   string
	err    error
	seen   string
	maxTok int
}

func (f *fakeModel) Complete(_ context.Context, r model.Request) (model.Response, error) {
	f.calls++
	f.seen = r.User
	f.maxTok = r.MaxTokens
	if f.err != nil {
		return model.Response{}, f.err
	}
	return model.Response{Text: f.resp}, nil
}

func sampleState() *store.State {
	return &store.State{
		Entries: map[string]*entry.Entry{
			"e-int": {
				ID: "e-int", Facet: entry.Interaction, Grain: entry.Class,
				Text: "先给结论再展开原因", Status: entry.Stable, Count: 5, Kinds: []string{"S1", "S3"}, Last: "2026-11-23", Tagged: true, Origin: entry.Behavior,
			},
			"e-tool": {
				ID: "e-tool", Facet: entry.Tools, Grain: entry.Class, Object: "host-a",
				Text: "在 host-a 上运行全量测试，不在本机跑", Status: entry.Stable, Count: 4, Kinds: []string{"S3"}, Last: "2026-11-22", Tagged: true, Origin: entry.Behavior,
			},
			"e-judge": {
				ID: "e-judge", Facet: entry.Judgement, Grain: entry.Abstract,
				Text: "上线变更优先小步灰度并保留回滚方案", Status: entry.Stable, Count: 6, Kinds: []string{"S1", "S8"}, Last: "2026-11-21", Tagged: true, Origin: entry.Behavior,
			},
			"e-work": {
				ID: "e-work", Facet: entry.Workflow, Grain: entry.Class, Object: "project-x",
				Text: "排查故障时先查真实日志读数再改配置", Status: entry.Stable, Count: 4, Kinds: []string{"S1"}, Last: "2026-11-20", Tagged: true, Origin: entry.Behavior,
			},
			"e-world": {
				ID: "e-world", Facet: entry.World, Grain: entry.Action, Object: "project-x",
				Text: "负责 project-x 的架构设计与发布", Status: entry.Stable, Count: 3, Kinds: []string{"S8"}, Last: "2026-11-23", Tagged: true, Origin: entry.Behavior,
			},
			"e-person-fact": {
				ID: "e-person-fact", Facet: entry.People, Person: "person:p1", Part: entry.PartFact,
				Text: "负责发布评审", Status: entry.Stable, Count: 2, Kinds: []string{"S8"}, Last: "2026-11-22", Tagged: true, Origin: entry.Behavior,
			},
			"e-person-eval": {
				ID: "e-person-eval", Facet: entry.People, Person: "person:p1", Part: entry.PartUserEval,
				Text: "内部评价不应进侧写汇总", Status: entry.Stable, Count: 2, Kinds: []string{"S1"}, Last: "2026-11-22", Tagged: true, Origin: entry.Behavior,
			},
			"e-person-inf": {
				ID: "e-person-inf", Facet: entry.People, Person: "person:p1", Part: entry.PartInferred, Inferred: true,
				Text: "推断性格不应进侧写汇总", Status: entry.Stable, Count: 2, Kinds: []string{"S8"}, Last: "2026-11-22", Tagged: true, Origin: entry.Behavior,
			},
			"e-persona": {
				ID: "e-persona", Facet: entry.Persona, Grain: entry.Action, Type: "current_focus",
				Text: "正在推进：project-x 稳定性治理", Status: entry.Stable, Count: 2, Kinds: []string{"S1"}, Last: "2026-11-24", Tagged: true, Origin: entry.FromOnboarding,
			},
			"e-health": {
				ID: "e-health", Facet: entry.Persona, Grain: entry.Action,
				Text: "每周跑步三次控制心率", Status: entry.Stable, Count: 3, Kinds: []string{"S1"}, Last: "2026-11-23", Tagged: true, Origin: entry.Behavior,
				OriginSpace: "health", Categories: []entry.Category{{Name: entry.CatHealth, By: entry.ByModel}},
			},
			"e-finance": {
				ID: "e-finance", Facet: entry.Persona, Grain: entry.Action,
				Text: "每月定投指数基金", Status: entry.Stable, Count: 2, Kinds: []string{"S1"}, Last: "2026-11-20", Tagged: true, Origin: entry.Behavior,
				Categories: []entry.Category{{Name: entry.CatFinance, By: entry.ByModel}},
			},
			"e-cand": {
				ID: "e-cand", Facet: entry.Interaction, Grain: entry.Action,
				Text: "候选条目尚未稳定", Status: entry.Candidate, Count: 1, Last: "2026-11-24", Tagged: true, Origin: entry.Behavior,
			},
		},
		Contacts: map[string]*entry.Contact{
			"person:p1": {Person: "person:p1", Name: "user-a", Score: 20},
		},
	}
}

func TestBuildSixFacetsAndPrivacyIsolation(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	sum := Build(sampleState(), now, "zh-Hans")

	if sum.CoveredCount != 6 {
		t.Fatalf("covered = %d, want 6", sum.CoveredCount)
	}
	if len(sum.Sections) != 6 {
		t.Fatalf("sections = %d, want 6", len(sum.Sections))
	}
	if sum.Overview == "" {
		t.Fatalf("expected non-empty overview")
	}
	// Sensitive items and person eval/inferred must NEVER appear in general overview or sections.
	for _, forbidden := range []string{"跑步", "心率", "定投", "内部评价", "推断性格", "候选条目"} {
		if strings.Contains(sum.Overview, forbidden) {
			t.Errorf("overview leaked forbidden text %q: %s", forbidden, sum.Overview)
		}
		for _, sec := range sum.Sections {
			if strings.Contains(sec.Summary, forbidden) {
				t.Errorf("section %s summary leaked %q", sec.Facet, forbidden)
			}
			for _, pt := range sec.Points {
				if strings.Contains(pt.Text, forbidden) {
					t.Errorf("section %s point leaked %q", sec.Facet, forbidden)
				}
			}
		}
	}
	// World section should include person fact prefixed with contact name.
	world, ok := sum.SectionByFacet(entry.World)
	if !ok {
		t.Fatal("missing world section")
	}
	foundPersonFact := false
	for _, pt := range world.Points {
		if strings.Contains(pt.Text, "user-a：负责发布评审") {
			foundPersonFact = true
		}
	}
	if !foundPersonFact {
		t.Errorf("world section missing person fact: %+v", world.Points)
	}
	// Sensitive sections should isolate health and finance.
	if len(sum.Sensitive) != 2 {
		t.Fatalf("sensitive sections = %d, want 2: %+v", len(sum.Sensitive), sum.Sensitive)
	}
	if sum.Sensitive[0].Category != entry.CatHealth || sum.Sensitive[0].OriginSpace != "health" {
		t.Errorf("unexpected health sensitive section: %+v", sum.Sensitive[0])
	}
	if sum.Sensitive[1].Category != entry.CatFinance {
		t.Errorf("unexpected finance sensitive section: %+v", sum.Sensitive[1])
	}
	// Recent should include current_focus.
	if len(sum.Recent) == 0 || !strings.Contains(sum.Recent[0], "project-x 稳定性治理") {
		t.Errorf("recent = %v, want current_focus first", sum.Recent)
	}
}

func TestBuildBlankNewUser(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	sum := Build(&store.State{}, now, "zh-Hans")
	if sum.CoveredCount != 0 || sum.SourceCount != 0 || sum.Overview != "" {
		t.Fatalf("blank user should have empty summary, got covered=%d sources=%d overview=%q", sum.CoveredCount, sum.SourceCount, sum.Overview)
	}
	if len(sum.Sections) != 6 {
		t.Fatalf("expected 6 empty sections, got %d", len(sum.Sections))
	}
}

func TestBuildWorkOnlyExcludesLife(t *testing.T) {
	st := &store.State{
		Onboarding: &entry.Onboarding{Use: entry.Work},
		Entries: map[string]*entry.Entry{
			"life": {ID: "life", Facet: entry.Persona, Domain: entry.Life, Text: "周末喜欢爬山", Status: entry.Stable, Count: 2, Last: "2026-11-23", Tagged: true},
			"work": {ID: "work", Facet: entry.Interaction, Domain: entry.Work, Text: "汇报先写结论", Status: entry.Stable, Count: 2, Last: "2026-11-23", Tagged: true},
		},
	}
	sum := Build(st, time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), "zh-Hans")
	if sum.CoveredCount != 1 || strings.Contains(sum.Overview, "爬山") {
		t.Fatalf("work-only leaked life entry: covered=%d overview=%q", sum.CoveredCount, sum.Overview)
	}
}

func TestRefreshSaveLoadAndModelFallback(t *testing.T) {
	dir := t.TempDir()
	now := func() time.Time { return time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC) }
	st := sampleState()

	// 1. Model synthesis succeeds and never sees sensitive text.
	fm := &fakeModel{
		resp: `{"overview":"注重结论先行与灰度回滚的工程师，当前推进 project-x 稳定性治理。","sections":{"interaction":"先给结论再展开。","tools":"全量测试放 host-a。"}}`,
	}
	sum, err := Refresh(context.Background(), st, dir, Options{Model: fm, Tier: model.Small, Lang: "zh-Hans", Day: "2026-11-23", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if fm.calls != 1 || sum.Mode != ModeModel {
		t.Fatalf("calls=%d mode=%s", fm.calls, sum.Mode)
	}
	if strings.Contains(fm.seen, "跑步") || strings.Contains(fm.seen, "定投") {
		t.Errorf("sensitive data leaked to model prompt: %s", fm.seen)
	}

	// 2. Second refresh with unchanged state skips model call.
	sum2, err := Refresh(context.Background(), st, dir, Options{Model: fm, Tier: model.Small, Lang: "zh-Hans", Day: "2026-11-24", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if fm.calls != 1 || sum2.Overview != sum.Overview {
		t.Fatalf("expected cache hit without model call, calls=%d", fm.calls)
	}

	// 3. Load from disk matches.
	loaded, err := Load(dir)
	if err != nil || loaded == nil || loaded.BasisHash != sum.BasisHash {
		t.Fatalf("load mismatch: %+v err=%v", loaded, err)
	}

	// 4. Force refresh when model errors falls back cleanly to deterministic.
	fmErr := &fakeModel{err: errors.New("model paused")}
	sum3, err := Refresh(context.Background(), st, dir, Options{Model: fmErr, Tier: model.Small, Lang: "zh-Hans", Day: "2026-11-24", Now: now, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum3.Mode != ModeDeterministic || sum3.CoveredCount != 6 {
		t.Fatalf("expected deterministic fallback, got mode=%s covered=%d", sum3.Mode, sum3.CoveredCount)
	}
	if !slices.Contains(sum3.Sections[1].Subjects, "host-a") {
		t.Errorf("expected host-a in tools subjects: %v", sum3.Sections[1].Subjects)
	}
}

type mockOverviewProvider struct {
	items []CandidateItem
}

func (m mockOverviewProvider) Name() string                         { return "periodic_overview" }
func (m mockOverviewProvider) Items(_ *store.State) []CandidateItem { return m.items }

func TestBuildWithCustomProvider(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	prov := mockOverviewProvider{
		items: []CandidateItem{
			{ID: "pat-1", Facet: entry.Judgement, Text: "架构选型优先插件化解耦而非侵入主分支", Last: "2026-11-24", Priority: 0, Weight: 10},
			{ID: "pat-2", Facet: entry.Workflow, Text: "上线前先小样本实证再放量", Last: "2026-11-24", Priority: 0, Weight: 9},
			{ID: "pat-3", Facet: entry.Persona, Text: "关注长期可维护性", Last: "2026-11-24", Priority: 0, Weight: 8, Categories: []string{entry.CatHealth}, OriginSpace: "health"},
		},
	}
	sum := BuildWith(nil, prov, now, "zh-Hans")
	if sum.InputSource != "periodic_overview" {
		t.Fatalf("input_source = %q, want periodic_overview", sum.InputSource)
	}
	if sum.CoveredCount != 2 || len(sum.Sensitive) != 1 {
		t.Fatalf("covered=%d sensitive=%d", sum.CoveredCount, len(sum.Sensitive))
	}
	if !strings.Contains(sum.Overview, "插件化解耦") || strings.Contains(sum.Overview, "长期可维护性") {
		t.Fatalf("unexpected overview: %q", sum.Overview)
	}
}

// Recent highlights are the user's own doings: a world line (a colleague's meeting, someone's rollout, a host's
// status, even in the user's own words) never makes them; a ⑥ current-work entry does. A model answer replaces them with the ones it kept about the user.
func TestRecentOnlyOwn(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	st := sampleState()
	ev := func(who string) []entry.Evidence {
		return []entry.Evidence{{Source: "S1", Ref: "chat/c1", Day: "2026-11-23", Who: who, Role: "user", Quote: "q"}}
	}
	st.Entries["w-colleague"] = &entry.Entry{ID: "w-colleague", Facet: entry.World, Grain: entry.Action,
		Text: "colleague-a 约了周四的分工会", Status: entry.Stable, Count: 2, Kinds: []string{"S1"}, Last: "2026-11-23",
		Tagged: true, Origin: entry.Behavior, Evidence: ev("person:p2")}
	st.Entries["w-rel"] = &entry.Entry{ID: "w-rel", Facet: entry.World, Grain: entry.Action, Subject: "person:p3",
		Predicate: "owns", Text: "colleague-b 在推动扩量", Status: entry.Stable, Count: 2, Kinds: []string{"S1"},
		Last: "2026-11-23", Tagged: true, Origin: entry.Behavior, Evidence: ev(entry.WhoSelf)}
	st.Entries["w-status"] = &entry.Entry{ID: "w-status", Facet: entry.World, Grain: entry.Action, Type: "status",
		Text: "host-b 上没有仓库凭据", Status: entry.Stable, Count: 2, Kinds: []string{"S3"}, Last: "2026-11-23",
		Tagged: true, Origin: entry.Behavior, Evidence: ev(entry.WhoSelf)}
	st.Entries["p-own"] = &entry.Entry{ID: "p-own", Facet: entry.Persona, Grain: entry.Action, Type: "current_work",
		Text: "在给 project-y 写迁移方案", Status: entry.Stable, Count: 2, Kinds: []string{"S1"}, Last: "2026-11-23",
		Tagged: true, Origin: entry.Behavior, Evidence: ev(entry.WhoSelf)}
	sum := Build(st, now, "zh-Hans")
	joined := strings.Join(sum.Recent, "|")
	if strings.Contains(joined, "colleague") || strings.Contains(joined, "host-b") || !strings.Contains(joined, "project-y") || !strings.Contains(joined, "稳定性治理") {
		t.Fatalf("recent = %v", sum.Recent)
	}
	fm := &fakeModel{resp: `{"overview":"o","sections":{"judgement":"j"},"recent":["在写 project-y 的迁移方案"]}`}
	out, err := Refresh(context.Background(), st, t.TempDir(), Options{Model: fm, Lang: "zh-Hans", Now: func() time.Time { return now }})
	if err != nil || len(out.Recent) != 1 || out.Recent[0] != "在写 project-y 的迁移方案" {
		t.Fatalf("%v %+v", err, out.Recent)
	}
}

// The synthesis has room for six coherent facets, the overview and the recent highlights in Chinese: a live reply ran
// to 3,200 tokens and was cut off at the old bound of 1,600.
func TestSynthReplyBound(t *testing.T) {
	fm := &fakeModel{resp: `{"overview":"o","sections":{},"recent":[]}`}
	if _, err := Refresh(context.Background(), sampleState(), t.TempDir(), Options{Model: fm, Lang: "zh-Hans"}); err != nil {
		t.Fatal(err)
	}
	if fm.maxTok < 3500 {
		t.Fatalf("max tokens %d", fm.maxTok)
	}
}

// v0.4.0: profile summary excludes S3/S4-only persona hallucinations and S6 assistant notes, and strictly isolates
// work-methodology rules from sensitive sections (0 work rules in profile:cat:emotion or profile:cat:health).
func TestV04ProfileExcludesS3S6AndIsolatesWorkRulesFromSensitive(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		"s3-sup": {
			ID: "s3-sup", Facet: entry.Persona, Origin: entry.Behavior, Status: entry.Stable,
			Text: "用户担任某平台自动调度主管", Count: 5, Last: "2026-11-24", Tagged: true,
			Evidence: []entry.Evidence{{Source: entry.SrcAgent, Ref: "ag/1", Day: "2026-11-24", Who: entry.WhoSelf, Quote: "平台主管"}},
		},
		"s6-note": {
			ID: "s6-note", Facet: entry.Interaction, Origin: entry.OriginAssistantNote, Status: entry.Stable,
			Text: "助手反思笔记：切勿说我改了目标", Count: 5, Last: "2026-11-24", Tagged: true,
		},
		"work-emo-false": {
			ID: "work-emo-false", Facet: entry.Interaction, Origin: entry.Behavior, Status: entry.Stable,
			Text: "反复发问会让用户不满，嫌解释绕要求说人话", Count: 4, Last: "2026-11-24", Tagged: true,
			Categories: []entry.Category{{Name: entry.CatEmotion, By: entry.ByModel}},
		},
		"work-health-false": {
			ID: "work-health-false", Facet: entry.Interaction, Origin: entry.Behavior, Status: entry.Stable,
			Text: "要的是例行方案不要一次性导出", Count: 4, Last: "2026-11-24", Tagged: true,
			OriginSpace: "health",
			Categories:  []entry.Category{{Name: entry.CatHealth, By: entry.BySpaceDefault}},
		},
		"true-health": {
			ID: "true-health", Facet: entry.Persona, Origin: entry.Behavior, Status: entry.Stable,
			Text: "近期体检空腹血糖偏高，需要按医生建议复查", Count: 2, Last: "2026-11-24", Tagged: true,
			Evidence: []entry.Evidence{{Source: entry.SrcAssistant, Ref: "c/1", Day: "2026-11-24", Who: entry.WhoSelf, Quote: "体检血糖高"}},
		},
	}}

	sum := Build(st, time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC), "zh-Hans")
	// 1. S3 persona hallucination and S6 assistant note must be excluded everywhere.
	for _, sec := range sum.Sections {
		for _, pt := range sec.Points {
			if pt.ID == "s3-sup" || pt.ID == "s6-note" {
				t.Errorf("forbidden entry %s leaked into profile section %s", pt.ID, sec.Facet)
			}
		}
	}
	// 2. Work rules mis-tagged emotion/health must land in general interaction section, NEVER in Sensitive.
	inter, ok := sum.SectionByFacet(entry.Interaction)
	if !ok || len(inter.Points) != 2 {
		t.Fatalf("expected 2 general interaction points (mis-tagged work rules released), got %+v", inter.Points)
	}
	for _, sens := range sum.Sensitive {
		for _, pt := range sens.Points {
			if pt.ID == "work-emo-false" || pt.ID == "work-health-false" {
				t.Errorf("work rule %s must not enter sensitive section %s", pt.ID, sens.Category)
			}
		}
	}
	// 3. Genuine health payload must still be strictly isolated into Sensitive[health].
	foundHealth := false
	for _, sens := range sum.Sensitive {
		if sens.Category == entry.CatHealth {
			for _, pt := range sens.Points {
				if pt.ID == "true-health" {
					foundHealth = true
				}
			}
		}
	}
	if !foundHealth {
		t.Fatalf("genuine health disclosure missing from Sensitive[health]: %+v", sum.Sensitive)
	}
}
