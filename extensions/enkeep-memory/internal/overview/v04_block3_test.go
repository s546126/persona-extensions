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
