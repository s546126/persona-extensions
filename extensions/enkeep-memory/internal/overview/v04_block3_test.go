package overview

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
)

// Synthetic fixtures only: de-identified projects, sources, and quotes.

// SyncProfileCore consumes only T0 and multi-week cross-scene T1 Persona entries, strictly excluding S3/S4-only
// agent hallucinations and S6 assistant notes.
func TestSyncProfileCoreFiltersS3S4AndAssistantNote(t *testing.T) {
	t0 := &entry.Entry{
		ID: "p-t0", Facet: entry.Persona, Grain: entry.Action, Type: "boundary",
		Text: "不要把理性分析风格等同于冷漠", Status: entry.Stable, Origin: entry.SelfReport, Tagged: true,
		First: "2026-11-01", Last: "2026-11-01", Updated: "2026-11-01T00:00:00Z", Count: 1,
		Evidence: []entry.Evidence{{Source: entry.SrcAbout, Ref: "about/1", Day: "2026-11-01", Who: entry.WhoSelf, Quote: "理性分析不代表不在乎"}},
	}
	t1Multi := &entry.Entry{
		ID: "p-t1", Facet: entry.Persona, Grain: entry.Class, Type: "constraint",
		Text: "注意力与决策容量有限，偏好先给结论再展开", Status: entry.Stable, Origin: entry.Behavior, Tagged: true,
		First: "2026-11-02", Last: "2026-11-18", Updated: "2026-11-18T00:00:00Z", Count: 4,
		Scopes: []entry.Scope{{Kind: entry.ScopeProject, Name: "projA"}, {Kind: entry.ScopeProject, Name: "projB"}},
		Evidence: []entry.Evidence{
			{Source: entry.SrcAssistant, Ref: "c/1", Day: "2026-11-02", Who: entry.WhoSelf, Quote: "直接先给结论"},
			{Source: entry.SrcAsk, Ref: "a/2", Day: "2026-11-18", Who: entry.WhoSelf, Quote: "别让我读长篇"},
		},
	}
	s3Hallucination := &entry.Entry{
		ID: "p-s3", Facet: entry.Persona, Grain: entry.Action, Type: "role",
		Text: "用户担任某平台自动调度主管", Status: entry.Stable, Origin: entry.Behavior, Tagged: true,
		First: "2026-11-01", Last: "2026-11-20", Updated: "2026-11-20T00:00:00Z", Count: 5,
		Scopes: []entry.Scope{{Kind: entry.ScopeProject, Name: "projA"}, {Kind: entry.ScopeProject, Name: "projB"}},
		Evidence: []entry.Evidence{
			{Source: entry.SrcAgent, Ref: "ag/1", Day: "2026-11-01", Who: entry.WhoSelf, Quote: "我是平台主管"},
			{Source: entry.SrcTools, Ref: "ag/2", Day: "2026-11-20", Who: entry.WhoSelf, Quote: "统筹调度"},
		},
	}
	s6Note := &entry.Entry{
		ID: "p-s6", Facet: entry.Persona, Grain: entry.Action, Type: "note",
		Text: "助手自记：不要说我改了目标", Status: entry.Stable, Origin: entry.OriginAssistantNote, Tagged: true,
		First: "2026-11-01", Last: "2026-11-20", Updated: "2026-11-20T00:00:00Z", Count: 5,
		Evidence: []entry.Evidence{{Source: entry.SrcMemory, Ref: "mem/1", Day: "2026-11-10", Who: entry.WhoSelf, Quote: "note"}},
	}

	st := state(t0, t1Multi, s3Hallucination, s6Note)
	var seenInput profileCoreInput
	m := &funcModel{fn: func(r model.Request) (any, error) {
		if r.Step != "overview_profile_core" {
			return nil, fmt.Errorf("unexpected step %s", r.Step)
		}
		_ = json.Unmarshal([]byte(r.User), &seenInput)
		var ids []string
		for _, it := range seenInput.Items {
			ids = append(ids, it.ID)
		}
		return profileCoreOutput{Items: []profileCoreProposal{
			{Kind: "boundary", Text: "不要把用户的理性分析等同于冷漠。", Members: ids[:1], Reason: "T0 self report"},
			{Kind: "constraint", Text: "用户注意力与决策容量有限，沟通先给结论再展开细节。", Members: ids[1:], Reason: "T1 cross-week"},
		}}, nil
	}}

	s := newState()
	run := &Run{Counts: map[string]int{}}
	ids, err := runner(m).SyncProfileCore(context.Background(), st, s, run, "", stamp(now))
	if err != nil {
		t.Fatal(err)
	}
	if len(seenInput.Items) != 2 {
		t.Fatalf("expected only T0 and T1-multi-week items (excluding S3 and S6), got %+v", seenInput.Items)
	}
	for _, it := range seenInput.Items {
		if strings.Contains(it.Text, "调度主管") || strings.Contains(it.Text, "助手自记") {
			t.Fatalf("forbidden S3/S6 item leaked into SyncProfileCore: %+v", it)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 profile_core patterns, got %d", len(ids))
	}
	for _, id := range ids {
		p := s.Patterns[id]
		if p == nil || !p.IsProfileCore() || !p.Used() || !p.Rooted {
			t.Fatalf("unexpected profile_core pattern: %+v", p)
		}
	}
	// Verify Exits routes KindProfileCore to BucketProfile.
	exits := Exits(s)
	profExits := 0
	for _, x := range exits {
		if x.L1Kind == memsrc.BucketProfile && strings.HasPrefix(x.Entry.ID, "ov:profile:") {
			profExits++
		}
	}
	if profExits != 2 {
		t.Fatalf("expected 2 profile exits in BucketProfile, got %d", profExits)
	}
}

// SyncContracts with 120 candidates (40 ops_runbook + 80 universal interaction rules) must:
//  1. Deterministically divert all 40 ops_runbook entries to KindOpsRunbook (0 LLM calls for them);
//  2. Chunk the remaining 80 universal rules into <= contractBatchSize (35) per call with MaxTokens >= 12000;
//  3. Divert any model output that still retains a single-incident/host proper noun (e.g. OWC/sj-wsl) to
//     KindOpsRunbook with Hard=false, preventing it from entering global standing.
func TestSyncContractsLargeBatch120DivertsOpsAndChunks(t *testing.T) {
	var all []*entry.Entry
	// 40 ops_runbook entries (20 tools + 20 interaction mentioning host/CLI/API).
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("ops-t-%02d", i)
		all = append(all, &entry.Entry{
			ID: id, Facet: entry.Tools, Grain: entry.Action, Type: "tool_rule",
			Text:   fmt.Sprintf("在 build-box-%02d 上跑 go test 前先清理缓存", i),
			Status: entry.Stable, Origin: entry.Behavior, Tagged: true,
			First: "2026-11-01", Last: "2026-11-20", Updated: "2026-11-20T00:00:00Z", Count: 5,
			Leverage: &entry.Leverage{Reach: entry.ReachCrossProject, Horizon: entry.HorizonLasting},
			Evidence: []entry.Evidence{{Source: entry.SrcAssistant, Ref: "c/" + id, Day: "2026-11-10", Who: entry.WhoSelf, Quote: "用专用机跑测试"}},
		})
	}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("ops-i-%02d", i)
		all = append(all, &entry.Entry{
			ID: id, Facet: entry.Interaction, Grain: entry.Action, Type: "correction",
			Text:   fmt.Sprintf("涉及 cloudflare 与 docker compose 部署步骤 %02d 先检查端口", i),
			Status: entry.Stable, Origin: entry.Behavior, Tagged: true,
			First: "2026-11-01", Last: "2026-11-20", Updated: "2026-11-20T00:00:00Z", Count: 5,
			Leverage: &entry.Leverage{Reach: entry.ReachCrossProject, Horizon: entry.HorizonLasting},
			Evidence: []entry.Evidence{{Source: entry.SrcAssistant, Ref: "c/" + id, Day: "2026-11-10", Who: entry.WhoSelf, Quote: "先查端口"}},
		})
	}
	// 80 universal collaboration rules (no ops/tool/host names).
	for i := 0; i < 80; i++ {
		id := fmt.Sprintf("uni-%02d", i)
		all = append(all, &entry.Entry{
			ID: id, Facet: entry.Interaction, Grain: entry.Action, Type: "correction",
			Text:     fmt.Sprintf("在协作沟通场景 %02d 中先给简明结论再补充依据", i),
			Rejected: "冗长铺垫", Status: entry.Stable, Origin: entry.Behavior, Tagged: true,
			First: "2026-11-01", Last: "2026-11-20", Updated: "2026-11-20T00:00:00Z", Count: 4,
			Leverage: &entry.Leverage{Reach: entry.ReachCrossProject, Horizon: entry.HorizonLasting},
			Evidence: []entry.Evidence{
				{Source: entry.SrcAssistant, Ref: "c1/" + id, Day: "2026-11-10", Who: entry.WhoSelf, Quote: fmt.Sprintf("先说结论 %02d", i)},
				{Source: entry.SrcAsk, Ref: "c2/" + id, Day: "2026-11-12", Who: entry.WhoSelf, Quote: fmt.Sprintf("别绕弯子 %02d", i)},
			},
		})
	}

	st := state(all...)
	batchSizes := []int{}
	maxTokensSeen := []int{}
	m := &funcModel{fn: func(r model.Request) (any, error) {
		if r.Step != "overview_contracts" {
			return nil, fmt.Errorf("unexpected step %s", r.Step)
		}
		var in contractInput
		_ = json.Unmarshal([]byte(r.User), &in)
		batchSizes = append(batchSizes, len(in.Rules))
		maxTokensSeen = append(maxTokensSeen, r.MaxTokens)
		// Ensure no ops_runbook leaked into LLM batch.
		for _, rl := range in.Rules {
			if rl.Facet == string(entry.Tools) || strings.Contains(rl.Text, "cloudflare") || strings.Contains(rl.Text, "docker") {
				t.Errorf("ops_runbook rule leaked into LLM batch: %+v", rl)
			}
		}
		// Emit 1 clean universal contract per batch + on batch 1 also emit one with an OWC proper-noun shell
		// to verify post-synthesis code-level diversion + Hard=false.
		var props []contractProposal
		if len(batchSizes) == 1 && len(in.Rules) >= 2 {
			props = append(props, contractProposal{
				Rules:    []string{in.Rules[0].ID},
				Polarity: PolarityCheck,
				Text:     "用户已说明 OWC 外接盘在 Linux 上验证成功时不要再质疑",
				Reason:   "incident shell test",
			})
			var rest []string
			for _, rl := range in.Rules[1:] {
				rest = append(rest, rl.ID)
			}
			props = append(props, contractProposal{
				Rules:    rest,
				Polarity: PolarityCommunicate,
				Text:     "沟通汇报时先给核心结论，再按需补充关键依据。",
				Reason:   "batch 1 universal",
			})
		} else {
			var ids []string
			for _, rl := range in.Rules {
				ids = append(ids, rl.ID)
			}
			ext := ""
			if len(in.Existing) > 0 {
				ext = in.Existing[0].ID
			}
			props = append(props, contractProposal{
				Extends:  ext,
				Rules:    ids,
				Polarity: PolarityCommunicate,
				Text:     "沟通汇报时先给核心结论，再按需补充关键依据。",
				Reason:   "merged across batch",
			})
		}
		return contractOutput{Contracts: props}, nil
	}}

	s := newState()
	run := &Run{Counts: map[string]int{}}
	_, err := runner(m).SyncContracts(context.Background(), st, s, run, "", stamp(now))
	if err != nil {
		t.Fatal(err)
	}
	if run.Counts["contract_candidates"] != 120 {
		t.Fatalf("expected 120 candidates, got %d", run.Counts["contract_candidates"])
	}
	if run.Counts["contract_ops_diverted"] != 40 {
		t.Fatalf("expected 40 ops_runbook diverted before LLM, got %d", run.Counts["contract_ops_diverted"])
	}
	if len(batchSizes) != 3 {
		t.Fatalf("expected 80 universal rules split into 3 batches (35+35+10), got %v", batchSizes)
	}
	for i, sz := range batchSizes {
		if sz > contractBatchSize {
			t.Errorf("batch %d exceeded contractBatchSize (%d): %d", i, contractBatchSize, sz)
		}
		if maxTokensSeen[i] < 12000 {
			t.Errorf("batch %d MaxTokens = %d, want >= 12000", i, maxTokensSeen[i])
		}
	}
	// Verify OWC shell proposal was diverted to KindOpsRunbook with Hard=false, and Exits keeps it non-standing.
	owcFound := false
	for _, p := range s.Patterns {
		if strings.Contains(p.Text, "OWC") {
			owcFound = true
			if !p.IsOpsRunbook() || p.Hard {
				t.Fatalf("OWC incident shell must be diverted to KindOpsRunbook with Hard=false, got %+v", p)
			}
		}
	}
	if !owcFound {
		t.Fatal("expected OWC test pattern to exist as KindOpsRunbook")
	}
	for _, x := range Exits(s) {
		if strings.Contains(x.Entry.Text, "OWC") || strings.Contains(x.Entry.Text, "cloudflare") {
			if x.L1Kind != "" || x.Kind != "" {
				t.Errorf("ops_runbook exit must never be standing-eligible: %+v", x)
			}
			if x.Entry.Rank < rankRunbookBottom || x.Entry.Rank > rankRunbookTop {
				t.Errorf("ops_runbook rank = %d, want in [%d, %d]", x.Entry.Rank, rankRunbookBottom, rankRunbookTop)
			}
		}
	}
}

// Reconcile resolves opposing tensions into 1 structured principle with Poles, merges synonymous duplicates, and
// diverts concrete ops rules out of principles into KindOpsRunbook.
func TestReconcileTensionMergeAndOpsDiversion(t *testing.T) {
	e1 := action("rc1", entry.Judgement, "alpha", entry.SrcAssistant, "2026-11-02", "auto execute")
	e2 := action("rc2", entry.Judgement, "beta", entry.SrcAsk, "2026-11-12", "plan first")
	e3 := action("rc3", entry.Judgement, "gamma", entry.SrcAssistant, "2026-11-18", "model cost")
	e4 := action("rc4", entry.Judgement, "delta", entry.SrcAsk, "2026-11-19", "model tiering")
	e5 := action("rc5", entry.Workflow, "alpha", entry.SrcAssistant, "2026-11-15", "backup config")
	st := state(e1, e2, e3, e4, e5)

	s := newState()
	s.Patterns["ov:auto"] = &Pattern{
		ID: "ov:auto", Facet: string(entry.Judgement), Kind: KindPrinciple, Lifecycle: Core,
		Text: "已授权任务直接全放手自主推进", DefaultContrast: "每步停下来问",
		EvidenceIDs: []string{"rc1"}, ScenesCount: 2, SourcesCount: 3, ActiveWeeks: []string{"2026-W45", "2026-W46"},
	}
	s.Patterns["ov:plan"] = &Pattern{
		ID: "ov:plan", Facet: string(entry.Judgement), Kind: KindPrinciple, Lifecycle: Core,
		Text: "动手前必须先出方案大纲给用户审阅", DefaultContrast: "直接开工",
		EvidenceIDs: []string{"rc2"}, ScenesCount: 2, SourcesCount: 3, ActiveWeeks: []string{"2026-W45", "2026-W46"},
	}
	s.Patterns["ov:cost1"] = &Pattern{
		ID: "ov:cost1", Facet: string(entry.Judgement), Kind: KindPrinciple, Lifecycle: Core,
		Text: "按任务难度分级选模型，避免无脑上最贵模型", DefaultContrast: "全部用旗舰模型",
		EvidenceIDs: []string{"rc3"}, ScenesCount: 2, SourcesCount: 3, ActiveWeeks: []string{"2026-W46", "2026-W47"},
	}
	s.Patterns["ov:cost2"] = &Pattern{
		ID: "ov:cost2", Facet: string(entry.Judgement), Kind: KindPrinciple, Lifecycle: Core,
		Text: "控制模型调用成本，简单任务不浪费高端额度", DefaultContrast: "忽视额度成本",
		EvidenceIDs: []string{"rc4"}, ScenesCount: 2, SourcesCount: 3, ActiveWeeks: []string{"2026-W46", "2026-W47"},
	}
	s.Patterns["ov:ops"] = &Pattern{
		ID: "ov:ops", Facet: string(entry.Workflow), Kind: KindPrinciple, Lifecycle: Core,
		Text: "修改线上配置前先在 sj-wsl 上备份并跑门禁", DefaultContrast: "直接修改",
		EvidenceIDs: []string{"rc5"}, ScenesCount: 2, SourcesCount: 3, ActiveWeeks: []string{"2026-W46", "2026-W47"},
	}

	m := &funcModel{fn: func(r model.Request) (any, error) {
		if r.Step != "overview_reconcile" {
			return nil, fmt.Errorf("unexpected step %s", r.Step)
		}
		var in reconcileInput
		_ = json.Unmarshal([]byte(r.User), &in)
		idBySub := func(sub string) string {
			for _, p := range in.Patterns {
				if strings.Contains(p.Text, sub) {
					return p.ID
				}
			}
			return ""
		}
		// ov:ops must already be diverted by deterministic Stage 0 before calling the model.
		if idBySub("sj-wsl") != "" {
			t.Errorf("ops pattern should have been deterministically diverted before LLM call")
		}
		return reconcileOutput{
			Merges: []reconcileMerge{{
				Keep:   idBySub("分级选模型"),
				Merge:  []string{idBySub("控制模型调用成本")},
				Text:   "按任务性质与难度分级分配模型，避免无脑上最贵模型浪费额度。",
				Reason: "synonymous model cost principles",
			}},
			Tensions: []reconcileTension{{
				Keep:  idBySub("全放手自主推进"),
				Merge: []string{idBySub("先出方案大纲")},
				Text:  "按语境分流授权：执行语境全放手推进，设计或开放写作先对齐方案大纲。",
				Poles: []Pole{
					{When: "方向已定或常规执行时", Choice: "全放手自主推进到底"},
					{When: "架构设计或开放式写作时", Choice: "先出方案或大纲对齐再动手"},
				},
				Reason: "resolve autonomy vs alignment tension",
			}},
		}, nil
	}}

	run := &Run{Counts: map[string]int{}}
	if _, err := runner(m).Reconcile(context.Background(), st, s, run, "", stamp(now)); err != nil {
		t.Fatal(err)
	}
	if !s.Patterns["ov:ops"].IsOpsRunbook() {
		t.Fatalf("ov:ops must be diverted to KindOpsRunbook, got %+v", s.Patterns["ov:ops"])
	}
	if s.Patterns["ov:plan"].Lifecycle != Retired {
		t.Fatalf("absorbed tension pattern ov:plan must be Retired, got %+v", s.Patterns["ov:plan"])
	}
	if len(s.Patterns["ov:auto"].Poles) != 2 {
		t.Fatalf("resolved tension pattern ov:auto must have 2 Poles, got %+v", s.Patterns["ov:auto"].Poles)
	}
	if s.Patterns["ov:cost2"].Lifecycle != Retired {
		t.Fatalf("duplicate cost pattern ov:cost2 must be Retired, got %+v", s.Patterns["ov:cost2"])
	}
	if run.Counts["reconcile_tensions"] != 1 || run.Counts["reconcile_merged"] != 1 || run.Counts["reconcile_ops_diverted"] != 1 {
		t.Fatalf("unexpected reconcile counts: %+v", run.Counts)
	}
}

// Bug 2 regression: tags() never recursively walks child Members and never lets a single OriginSpace entry
// contaminate a 10-scene universal principle (judgement or persona); ops_runbook never carries Categories into
// profile:cat:* sensitive sections.
func TestTagsNoChildOrSingleSpaceContagionAndOpsNotSensitive(t *testing.T) {
	healthChild := &entry.Entry{
		ID: "child-health", Facet: entry.Persona, Grain: entry.Action,
		Text: "体检血糖偏高需要复查", Status: entry.Stable, Origin: entry.Behavior,
		OriginSpace: "health",
		Categories:  []entry.Category{{Name: entry.CatHealth, By: entry.BySpaceDefault}},
	}
	entries := []*entry.Entry{healthChild}
	var ids []string
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("cls-%02d", i)
		e := &entry.Entry{
			ID: id, Facet: entry.Judgement, Grain: entry.Class,
			Text:   fmt.Sprintf("跨场景原则样本 %02d：先抽小样本实证再决定放大投入", i),
			Status: entry.Stable, Origin: entry.Behavior,
			First: "2026-10-01", Last: "2026-11-20", Count: 3,
		}
		if i == 0 {
			e.Members = []string{"child-health"}
		}
		if i == 1 {
			e.OriginSpace = "health"
			e.Categories = []entry.Category{{Name: entry.CatHealth, By: entry.BySpaceDefault}}
		}
		entries = append(entries, e)
		ids = append(ids, id)
	}
	st := state(entries...)

	// 10-scene judgement principle must remain completely clean.
	pj := &Pattern{ID: "ov:j10", Facet: string(entry.Judgement), Kind: KindPrinciple, Text: "先抽小样本实证再决定是否放大投入", EvidenceIDs: ids}
	if cats, space := tags(st, pj, ids); len(cats) != 0 || space != "" {
		t.Fatalf("judgement 10-scene principle contaminated: cats=%v space=%q", cats, space)
	}

	// 10-scene persona principle with 1 health child + 1 health-space top entry (1/10 < 100%, < 50%) must remain clean.
	pp := &Pattern{ID: "ov:p10", Facet: string(entry.Persona), Kind: KindPrinciple, Text: "长程系统习惯自己搭底座再逐步演进", EvidenceIDs: ids}
	if cats, space := tags(st, pp, ids); len(cats) != 0 || space != "" {
		t.Fatalf("persona 10-scene principle contaminated: cats=%v space=%q", cats, space)
	}

	// KindOpsRunbook must always return cats=nil, space="" from tags() regardless of text or member tags.
	pOps := &Pattern{ID: "ov:ops-clean", Facet: string(entry.Tools), Kind: KindOpsRunbook, Text: "运行心率回补脚本 run.sh", EvidenceIDs: ids}
	if cats, space := tags(st, pOps, ids); len(cats) != 0 || space != "" {
		t.Fatalf("ops_runbook tags() must return nil, \"\", got cats=%v space=%q", cats, space)
	}

	// OverviewBackedProvider.Items must strip Categories/OriginSpace from KindOpsRunbook so it never enters profile:cat:*.
	s := newState()
	s.Patterns["ov:rb1"] = &Pattern{
		ID: "ov:rb1", Facet: string(entry.Tools), Kind: KindOpsRunbook, Lifecycle: Core,
		Text: "在专用门禁机上运行 fast 测试", Categories: []string{entry.CatHealth}, OriginSpace: "health",
		ScenesCount: 2, SourcesCount: 3, ActiveWeeks: []string{"2026-W46", "2026-W47"},
	}
	items := OverviewBackedProvider{Snapshot: func() *State { return s }}.Items(st)
	found := false
	for _, it := range items {
		if it.ID == "ov:rb1" {
			found = true
			if len(it.Categories) != 0 || it.OriginSpace != "" {
				t.Fatalf("ops_runbook must not carry Categories/OriginSpace into profile: %+v", it)
			}
		}
	}
	if !found {
		t.Fatal("expected ov:rb1 in OverviewBackedProvider.Items")
	}
}

// Bug 3 regression: 6 profile_core proposals (2 identity, 2 constraint, 2 boundary) must hash to 6 distinct IDs and all survive.
func TestSyncProfileCoreSixDistinctIDs(t *testing.T) {
	var all []*entry.Entry
	for i := 0; i < 6; i++ {
		all = append(all, &entry.Entry{
			ID: fmt.Sprintf("pc-src-%d", i), Facet: entry.Persona, Grain: entry.Action, Type: "self",
			Text:   fmt.Sprintf("用户画像事实来源条目 %d", i),
			Status: entry.Stable, Origin: entry.SelfReport, Tagged: true,
			First: "2026-11-01", Last: "2026-11-20", Updated: "2026-11-20T00:00:00Z", Count: 2,
			Evidence: []entry.Evidence{{Source: entry.SrcAbout, Ref: fmt.Sprintf("about/%d", i), Day: "2026-11-10", Who: entry.WhoSelf, Quote: fmt.Sprintf("原话 %d", i)}},
		})
	}
	st := state(all...)
	m := &funcModel{fn: func(r model.Request) (any, error) {
		var in profileCoreInput
		_ = json.Unmarshal([]byte(r.User), &in)
		return profileCoreOutput{Items: []profileCoreProposal{
			{Kind: "identity", Text: "用户目前主导跨团队技术产品演进与系统架构落地。", Members: []string{in.Items[0].ID}, Reason: "id1"},
			{Kind: "identity", Text: "用户长期在多时区协作环境下推进核心研发项目。", Members: []string{in.Items[1].ID}, Reason: "id2"},
			{Kind: "constraint", Text: "用户经常不在电脑前，交付物需要直接给出可读摘要。", Members: []string{in.Items[2].ID}, Reason: "c1"},
			{Kind: "constraint", Text: "用户注意力与决策容量宝贵，沟通必须先结论后细节。", Members: []string{in.Items[3].ID}, Reason: "c2"},
			{Kind: "boundary", Text: "不要把用户关注组织管理方法论等同于已经是全职管理者。", Members: []string{in.Items[4].ID}, Reason: "b1"},
			{Kind: "boundary", Text: "不要把用户的理性拆解风格等同于冷漠或否定合作关系。", Members: []string{in.Items[5].ID}, Reason: "b2"},
		}}, nil
	}}

	s := newState()
	run := &Run{Counts: map[string]int{}}
	ids, err := runner(m).SyncProfileCore(context.Background(), st, s, run, "", stamp(now))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 6 {
		t.Fatalf("expected 6 distinct profile_core IDs, got %d: %v", len(ids), ids)
	}
	stored := 0
	for _, p := range s.Patterns {
		if p.IsProfileCore() && p.Used() {
			stored++
		}
	}
	if stored != 6 {
		t.Fatalf("expected 6 stored profile_core patterns without hash collision, got %d", stored)
	}
}

// Bug 5 regression: Reconcile supports upgrading a single principle with built-in tension into Poles (empty merge)
// and resolving cross-kind principle + contract tensions.
func TestReconcileSinglePrincipleUpgradeAndCrossKindTension(t *testing.T) {
	e1 := action("t1", entry.Judgement, "alpha", entry.SrcAssistant, "2026-11-02", "single tension quote 1")
	e2 := action("t2", entry.Judgement, "beta", entry.SrcAsk, "2026-11-12", "single tension quote 2")
	e3 := action("t3", entry.Judgement, "gamma", entry.SrcChat, "2026-11-19", "single tension quote 3")
	e4 := action("t4", entry.Judgement, "alpha", entry.SrcAssistant, "2026-11-03", "cross principle quote 1")
	e5 := action("t5", entry.Judgement, "beta", entry.SrcAsk, "2026-11-14", "cross principle quote 2")
	e6 := action("t6", entry.Judgement, "gamma", entry.SrcChat, "2026-11-21", "cross principle quote 3")
	e7 := action("t7", entry.Interaction, "delta", entry.SrcAssistant, "2026-11-15", "cross contract quote")
	st := state(e1, e2, e3, e4, e5, e6, e7)

	s := newState()
	s.Patterns["ov:single"] = &Pattern{
		ID: "ov:single", Facet: string(entry.Judgement), Kind: KindPrinciple, Lifecycle: Core,
		Text:            "单次交付优先收窄核心范围，除非根因分散必须整体设计",
		Exception:       "反复偏离或散点根因时推倒重做整体设计",
		DefaultContrast: "每次都全量重构",
		EvidenceIDs:     []string{"t1", "t2", "t3"}, ScenesCount: 3, SourcesCount: 3, UniqueQuotes: 3, ActiveWeeks: []string{"2026-W45", "2026-W46", "2026-W47"},
	}
	s.Patterns["ov:princ"] = &Pattern{
		ID: "ov:princ", Facet: string(entry.Judgement), Kind: KindPrinciple, Lifecycle: Core,
		Text:            "方向已定时自主连续推进到底",
		DefaultContrast: "每步请示",
		EvidenceIDs:     []string{"t4", "t5", "t6"}, ScenesCount: 3, SourcesCount: 3, UniqueQuotes: 3, ActiveWeeks: []string{"2026-W45", "2026-W46", "2026-W47"},
	}
	s.Patterns["ov:cont"] = &Pattern{
		ID: "ov:cont", Facet: string(entry.Interaction), Kind: KindContract, Lifecycle: Core, Polarity: PolarityCheck,
		Text:        "方案未定前先出大纲与用户对齐再动手",
		EvidenceIDs: []string{"t7"}, ScenesCount: 2, SourcesCount: 2, UniqueQuotes: 2, ActiveWeeks: []string{"2026-W46", "2026-W47"},
	}

	m := &funcModel{fn: func(r model.Request) (any, error) {
		var in reconcileInput
		_ = json.Unmarshal([]byte(r.User), &in)
		idBySub := func(sub string) string {
			for _, p := range in.Patterns {
				if strings.Contains(p.Text, sub) {
					return p.ID
				}
			}
			return ""
		}
		return reconcileOutput{
			Tensions: []reconcileTension{
				{
					Keep:  idBySub("收窄核心范围"),
					Merge: []string{},
					Text:  "按问题性质切换交付尺度：单次局部任务收窄核心，散点根因整体设计。",
					Poles: []Pole{
						{When: "单次局部交付时", Choice: "收窄到最小核心闭环"},
						{When: "反复偏离或根因分散时", Choice: "从底座整体重新设计"},
					},
					Reason: "upgrade single principle into structured poles",
				},
				{
					Keep:  idBySub("自主连续推进到底"),
					Merge: []string{idBySub("先出大纲与用户对齐")},
					Text:  "按方案确定度分流推进方式：未定先对齐，已定自主推进到底。",
					Poles: []Pole{
						{When: "方案或方向未定时", Choice: "先出方案大纲对齐再动手"},
						{When: "方向已定且授权明确时", Choice: "自主连续推进直到交付"},
					},
					Reason: "cross-kind principle+contract tension",
				},
			},
		}, nil
	}}

	run := &Run{Counts: map[string]int{}}
	if _, err := runner(m).Reconcile(context.Background(), st, s, run, "", stamp(now)); err != nil {
		t.Fatal(err)
	}
	if len(s.Patterns["ov:single"].Poles) != 2 || s.Patterns["ov:single"].Exception != "" || s.Patterns["ov:single"].Lifecycle != Scoped {
		t.Fatalf("single principle upgrade failed: %+v", s.Patterns["ov:single"])
	}
	if s.Patterns["ov:cont"].Lifecycle != Retired {
		t.Fatalf("absorbed cross-kind contract must be Retired: %+v", s.Patterns["ov:cont"])
	}
	if !s.Patterns["ov:princ"].IsPrinciple() || len(s.Patterns["ov:princ"].Poles) != 2 || s.Patterns["ov:princ"].Lifecycle != Scoped {
		t.Fatalf("cross-kind tension keep principle failed: %+v", s.Patterns["ov:princ"])
	}
	if run.Counts["reconcile_tensions"] != 2 {
		t.Fatalf("reconcile_tensions = %d, want 2", run.Counts["reconcile_tensions"])
	}
}
