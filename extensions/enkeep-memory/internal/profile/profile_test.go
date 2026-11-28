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
			Categories: []entry.Category{{Name: entry.CatHealth, By: entry.ByModel}},
			Evidence:   []entry.Evidence{{Source: entry.SrcAssistant, Ref: "c/1", Day: "2026-11-24", Who: entry.WhoSelf, Quote: "体检血糖高"}},
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

// v0.4.0 rehearsal round 2: state.json persisted categories are the single source of truth for profile sensitive
// sections — profile.Build must NEVER resurrect categories already cleared by Phase 2.2, and ops_runbook entries must
// never enter profile:cat:*.
func TestV04ProfileSensitiveSSOTNoResurrectionAndNoOpsRunbook(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		// Cleared by Phase 2.2 (Categories == nil): even if text contains a health/finance/legal keyword,
		// profile.Build must NOT re-run PayloadCategories to resurrect it into Sensitive.
		"released-fp-persona": {
			ID: "released-fp-persona", Facet: entry.Persona, Origin: entry.Behavior, Status: entry.Stable,
			Text: "关注系统心率回补任务的自动化监控指标", Count: 3, Last: "2026-11-24", Tagged: true,
		},
		// Work-methodology rule with cleared Categories + leftover OriginSpace: must stay general, never sensitive.
		"released-fp-work": {
			ID: "released-fp-work", Facet: entry.Workflow, Origin: entry.Behavior, Status: entry.Stable,
			Text: "体检数据汇总表按月归档输出", Count: 3, Last: "2026-11-24", Tagged: true,
			OriginSpace: "health",
		},
		// Ops runbook (tools / script / ClassOpsRunbook) even with persisted sensitive tags must NEVER enter profile:cat:*.
		"ops-tools-script": {
			ID: "ops-tools-script", Facet: entry.Tools, Origin: entry.Behavior, Status: entry.Stable,
			Text: "通过 run.sh 回补心率数据配额并检查 API 限流", Count: 3, Last: "2026-11-24", Tagged: true,
			OriginSpace: "health",
			Categories:  []entry.Category{{Name: entry.CatHealth, By: entry.ByModel}},
		},
		"ops-class-explicit": {
			ID: "ops-class-explicit", Facet: entry.Workflow, Origin: entry.Behavior, Status: entry.Stable,
			MemoryClass: entry.ClassOpsRunbook,
			Text:        "执行合规审查脚本前先备份配置", Count: 2, Last: "2026-11-24", Tagged: true,
			Categories: []entry.Category{{Name: entry.CatLegalID, By: entry.ByModel}},
		},
		// Genuine persisted sensitive entry (ByModel on non-work facet) stays isolated in Sensitive[finance].
		"persisted-finance": {
			ID: "persisted-finance", Facet: entry.Persona, Origin: entry.Behavior, Status: entry.Stable,
			Text: "每月定投指数基金作为长期储蓄", Count: 2, Last: "2026-11-24", Tagged: true,
			Categories: []entry.Category{{Name: entry.CatFinance, By: entry.ByModel}},
		},
		// Work-methodology entry with explicit ByUser sensitive tag stays isolated in Sensitive[relationships].
		"work-by-user": {
			ID: "work-by-user", Facet: entry.Interaction, Origin: entry.Behavior, Status: entry.Stable,
			Text: "涉及家人安排时先私聊确认", Count: 2, Last: "2026-11-24", Tagged: true,
			Categories: []entry.Category{{Name: entry.CatRelationships, By: entry.ByUser}},
		},
	}}

	sum := Build(st, time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC), "zh-Hans")

	sensIDs := map[string]string{}
	for _, sec := range sum.Sensitive {
		for _, pt := range sec.Points {
			sensIDs[pt.ID] = sec.Category
		}
	}
	for _, forbidden := range []string{"released-fp-persona", "released-fp-work", "ops-tools-script", "ops-class-explicit"} {
		if cat, leaked := sensIDs[forbidden]; leaked {
			t.Errorf("entry %s must not enter profile:cat:%s (SSOT / ops_runbook violation)", forbidden, cat)
		}
	}
	if sensIDs["persisted-finance"] != entry.CatFinance {
		t.Errorf("persisted-finance expected in finance, got %+v", sensIDs)
	}
	if sensIDs["work-by-user"] != entry.CatRelationships {
		t.Errorf("work-by-user expected in relationships, got %+v", sensIDs)
	}
}

// v0.4.1: the sensitive sections hold only the user's own records (someone else's money or diet stays on their card),
// the world section leads with the user's own facts and carries at most MaxWorldPeople people lines, one per person,
// and LOW_CONFIDENCE / single one-off persona misreads never reach the profile.
func TestProfileIsolatesOthersAndMisreads(t *testing.T) {
	stable := func(e *entry.Entry) *entry.Entry {
		e.Status, e.Tagged, e.Origin = entry.Stable, true, entry.Behavior
		if e.Count == 0 {
			e.Count = 1
		}
		if e.Last == "" {
			e.Last = "2026-11-20"
		}
		if e.Kinds == nil {
			e.Kinds = []string{"S8"}
		}
		return e
	}
	fin := []entry.Category{{Name: entry.CatFinance, By: entry.ByModel}}
	st := &store.State{
		Entries: map[string]*entry.Entry{},
		Contacts: map[string]*entry.Contact{
			"person:a": {Person: "person:a", Name: "user-a", Score: 900},
			"person:b": {Person: "person:b", Name: "user-b", Score: 500},
			"person:c": {Person: "person:c", Name: "user-c", Score: 400},
			"person:d": {Person: "person:d", Name: "user-d", Score: 300},
		},
	}
	add := func(e *entry.Entry) { st.Entries[e.ID] = stable(e) }
	add(&entry.Entry{ID: "u-fin", Facet: entry.Persona, Grain: entry.Action, Text: "用户把财务自由定为攒够一笔净资产后退休", Categories: fin})
	for i, p := range []string{"person:a", "person:b", "person:c", "person:d", "person:a", "person:b", "person:c"} {
		add(&entry.Entry{ID: "p-fin-" + string(rune('a'+i)), Facet: entry.People, Person: p, Part: entry.PartFact, Count: 5,
			Text: "群友自述贷款买房的情况 " + string(rune('a'+i)), Categories: fin})
	}
	add(&entry.Entry{ID: "p-health", Facet: entry.People, Person: "person:b", Part: entry.PartFact, Count: 3, Text: "群友在减肥控制饮食",
		Categories: []entry.Category{{Name: entry.CatHealth, By: entry.ByModel}}})
	for i := 0; i < 6; i++ {
		add(&entry.Entry{ID: "p-a-" + string(rune('0'+i)), Facet: entry.People, Person: "person:a", Part: entry.PartFact, Count: 6,
			Text: "负责工程链路第 " + string(rune('0'+i)) + " 段的落地"})
	}
	add(&entry.Entry{ID: "p-b-0", Facet: entry.People, Person: "person:b", Part: entry.PartFact, Count: 4, Text: "负责评测数据维护"})
	add(&entry.Entry{ID: "p-c-0", Facet: entry.People, Person: "person:c", Part: entry.PartFact, Count: 4, Text: "负责值班告警"})
	for i, pair := range [][2]string{{"person:a", "person:b"}, {"person:b", "person:a"}, {"person:a", "person:c"}, {"person:c", "person:a"}} {
		add(&entry.Entry{ID: "w-rel-" + string(rune('0'+i)), Facet: entry.World, Grain: entry.Class, Type: "collaborates_with",
			Subject: pair[0], Predicate: "collaborates_with", Target: pair[1], Count: 15, Text: pair[0] + " 和 " + pair[1] + " 合作"})
	}
	add(&entry.Entry{ID: "w-own-1", Facet: entry.World, Grain: entry.Class, Text: "用户拆解方案时把每个环节点名到负责人"})
	add(&entry.Entry{ID: "w-own-2", Facet: entry.World, Grain: entry.Class, Text: "用户把协作与依赖关系明确记录下来"})
	add(&entry.Entry{ID: "u-low", Facet: entry.Persona, Grain: entry.Action, Text: "用户拒了个面试被告状，回顾读博的时间",
		Flags: []string{entry.FlagLowConfidence, "standalone"}, Leverage: &entry.Leverage{Reach: entry.ReachOneOff},
		Categories: []entry.Category{{Name: entry.CatEmotion, By: entry.ByModel}}})
	add(&entry.Entry{ID: "u-oneoff", Facet: entry.Persona, Grain: entry.Action, Text: "用户那天对一次排期临时吐槽",
		Leverage: &entry.Leverage{Reach: entry.ReachOneOff}})
	add(&entry.Entry{ID: "u-own", Facet: entry.Persona, Grain: entry.Action, Text: "用户以凌晨三点到上午八点为清醒窗口", Count: 4})

	sum := Build(st, time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), "zh-Hans")

	for _, ssec := range sum.Sensitive {
		for _, pt := range ssec.Points {
			if strings.HasPrefix(pt.ID, "p-") {
				t.Errorf("someone else's record %s in profile:cat:%s", pt.ID, ssec.Category)
			}
		}
	}
	var finance SensitiveSection
	ok := false
	for _, ssec := range sum.Sensitive {
		if ssec.Category == entry.CatFinance {
			finance, ok = ssec, true
		}
	}
	if !ok || len(finance.Points) != 1 || finance.Points[0].ID != "u-fin" {
		t.Fatalf("finance section should hold the user's own record only: %+v", sum.Sensitive)
	}
	world, ok := sum.SectionByFacet(entry.World)
	if !ok || len(world.Points) < 2 || !strings.HasPrefix(world.Points[0].ID, "w-own-") || !strings.HasPrefix(world.Points[1].ID, "w-own-") {
		t.Fatalf("world section should lead with the user's own facts: %+v", world.Points)
	}
	perPerson := map[string]int{}
	people := 0
	for _, pt := range world.Points {
		e := st.Entries[pt.ID]
		if e == nil {
			continue
		}
		var names []string
		switch {
		case e.Facet == entry.People:
			names = []string{e.Person}
		case strings.HasPrefix(e.Subject, "person:"):
			names = []string{e.Subject, e.Target}
		default:
			continue
		}
		people++
		for _, n := range names {
			if perPerson[n]++; perPerson[n] > 1 {
				t.Errorf("person %s holds %d world lines: %+v", n, perPerson[n], world.Points)
			}
		}
	}
	if people > MaxWorldPeople || people == 0 {
		t.Errorf("world people lines = %d, want 1..%d: %+v", people, MaxWorldPeople, world.Points)
	}
	all := sum.Overview + strings.Join(sum.Recent, "")
	for _, sec := range sum.Sections {
		for _, pt := range sec.Points {
			all += pt.ID + pt.Text
		}
	}
	for _, ssec := range sum.Sensitive {
		for _, pt := range ssec.Points {
			all += pt.ID + pt.Text
		}
	}
	for _, bad := range []string{"读博", "u-low", "u-oneoff", "临时吐槽"} {
		if strings.Contains(all, bad) {
			t.Errorf("misread %q reached the profile", bad)
		}
	}
	if !strings.Contains(all, "u-own") {
		t.Error("the user's own persona entry is missing")
	}
}
