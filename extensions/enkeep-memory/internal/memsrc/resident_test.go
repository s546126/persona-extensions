package memsrc

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/profile"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// All texts are placeholders.
func cand(id, kind string, score, n int) Candidate {
	// Each id its own run of characters (n bytes, three a character), so no two lines are alike.
	base := rune(0x4e00 + 300*int(id[0]%7) + 40*int(id[1]-'0'))
	var b strings.Builder
	for i := 0; b.Len()+3 <= n; i++ {
		b.WriteRune(base + rune(i%40))
	}
	bucket := BucketPrinciple
	if kind == entry.KindRules {
		bucket = BucketContract
	}
	return Candidate{ID: id, Kind: kind, Bucket: bucket, L1: true, Score: score, Text: b.String()}
}

// Three-section quotas: principles never get starved by contracts; unused room spills across L1 sections; alike lines
// count once; output order is Profile -> Principle -> Contract.
func TestResidentQuotas(t *testing.T) {
	var cs []Candidate
	for i, id := range []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7"} {
		cs = append(cs, cand(id, entry.KindRules, 90-i, 150))
	}
	cs = append(cs, cand("u1", entry.KindUnderstanding, 50, 150), cand("u2", entry.KindUnderstanding, 40, 150))
	got := Resident(cs)
	size, rules, under := 0, 0, 0
	for _, c := range got {
		size += len(c.Text) + residentOverhead
		if c.Bucket == BucketContract {
			rules++
		} else {
			under++
		}
	}
	if size > ResidentBytes || under != 2 || rules == 0 {
		t.Fatalf("%d bytes, %d contracts, %d principles: %v", size, rules, under, got)
	}
	// Section order: principles (u1, u2) precede contracts (r1, ...).
	if got[0].ID != "u1" || got[1].ID != "u2" || got[2].ID != "r1" {
		t.Errorf("section order %v", got)
	}
	// Alike lines: one.
	dup := Resident([]Candidate{
		{ID: "a", Kind: entry.KindRules, Bucket: BucketContract, L1: true, Score: 9, Text: "report the conclusion first, then the details"},
		{ID: "b", Kind: entry.KindRules, Bucket: BucketContract, L1: true, Score: 8, Text: "report the conclusion first, then details"},
	})
	if len(dup) != 1 || dup[0].ID != "a" {
		t.Errorf("dedupe %v", dup)
	}
}

// Only the standing set stands; it starts at rankStand; the rest carry a line of the user's own words for recall.
func TestPageStandingSetAndQuote(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		"j-wide": {ID: "j-wide", Facet: entry.Judgement, Text: "pilot before any rollout", Situation: "a new rollout", Choice: "pilot first",
			Status: entry.Stable, Count: 4, Tagged: true, Origin: entry.Behavior,
			Leverage: &entry.Leverage{Reach: entry.ReachCrossProject, Horizon: entry.HorizonLasting, Principle: "learn cheaply before betting"}},
		"w-cand": {ID: "w-cand", Facet: entry.Workflow, Text: "write the notes after the call", Status: entry.Candidate, Count: 1, Tagged: true,
			Origin: entry.Behavior, Evidence: []entry.Evidence{{Ref: "chats/a", Day: "2026-11-02", Who: entry.WhoSelf, Quote: "call notes right after"}}},
	}}
	r, n, err := Page(st, Params{Contribution: "memory", Limit: 10})
	if err != nil || n.Standing != 1 {
		t.Fatalf("%v %+v", err, n)
	}
	w := r.Entries[0]
	if w.ID != "j-wide" || !w.Standing || w.Rank != rankStand || !strings.HasPrefix(w.Text, "原则：learn cheaply before betting") {
		t.Errorf("standing %+v", w)
	}
	c := r.Entries[1]
	if c.Standing || !strings.Contains(c.Text, "原话：「call notes right after」（2026-11-02）") {
		t.Errorf("recalled %+v", c)
	}
}

// Alike scores break ties by latest verification day first, then independent quotes (Days), then id.
func TestResidentTieBreak(t *testing.T) {
	got := Resident([]Candidate{
		{ID: "a", Kind: entry.KindRules, Bucket: BucketContract, L1: true, Score: 50, Days: 5, Last: "2026-11-23", Text: "一二三四五"},
		{ID: "b", Kind: entry.KindRules, Bucket: BucketContract, L1: true, Score: 50, Days: 2, Last: "2026-11-24", Text: "六七八九十"},
		{ID: "c", Kind: entry.KindRules, Bucket: BucketContract, L1: true, Score: 50, Days: 6, Last: "2026-11-24", Text: "甲乙丙丁戊"},
	})
	if len(got) != 3 || got[0].ID != "c" || got[1].ID != "b" || got[2].ID != "a" {
		t.Errorf("order %v", got)
	}
}

// A wider room holds more lines.
func TestResidentRoom(t *testing.T) {
	var cs []Candidate
	for i := 0; i < 40; i++ {
		var b strings.Builder
		for k := 0; k < 30; k++ {
			b.WriteRune(rune(0x4e00 + 40*i + k))
		}
		cs = append(cs, Candidate{ID: fmt.Sprint("r", i), Kind: entry.KindRules, Bucket: BucketContract, L1: true, Score: 90 - i, Text: b.String()})
	}
	small, wide := len(ResidentWithin(cs, ResidentBytes, ResidentMostFor(ResidentBytes))), len(ResidentWithin(cs, 4096, ResidentMostFor(4096)))
	if wide <= small {
		t.Errorf("room 1400: %d lines, 4096: %d", small, wide)
	}
}

// Bucket 0 (Hot Slot): capped at min(10%, 400 B) and at most 2 entries; Pinned ranks ahead of Fast-Path; unused hot-slot
// bytes return to main sections; ranks start at 995 for hot slot and 989 for Profile -> Principle -> Contract.
func TestResidentHotSlotAndRanks(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	defer func() { nowFn = prevNow }()

	st := &store.State{Entries: map[string]*entry.Entry{
		"pin-1":  {ID: "pin-1", Facet: entry.Judgement, Text: "用户置顶的核心原则", Status: entry.Stable, Pinned: true, Last: "2026-11-20", Tagged: true, Origin: entry.SelfReport},
		"fh-1":   {ID: "fh-1", Facet: entry.Interaction, Type: "correction", Text: "热槽当日纠正甲，遇到线上变更先确认", Exception: "回滚操作除外", Status: entry.Candidate, Last: "2026-11-26", Tagged: true, Origin: entry.Behavior, Flags: []string{entry.FlagFastHot + "2026-11-26"}},
		"fh-2":   {ID: "fh-2", Facet: entry.Interaction, Type: "correction", Text: "热槽超限第三条不应入选", Status: entry.Candidate, Last: "2026-11-25", Tagged: true, Origin: entry.Behavior, Flags: []string{entry.FlagFastHot + "2026-11-25"}},
		"fh-old": {ID: "fh-old", Facet: entry.Interaction, Type: "correction", Text: "超过七天的旧热槽应过期退出", Status: entry.Candidate, Last: "2026-11-10", Tagged: true, Origin: entry.Behavior, Flags: []string{entry.FlagFastHot + "2026-11-10"}},
		"fh-abs": {ID: "fh-abs", Facet: entry.Interaction, Type: "correction", Text: "已被契约吸收的纠正应退出热槽", Status: entry.Candidate, Last: "2026-11-26", Tagged: true, Origin: entry.Behavior, Flags: []string{entry.FlagFastHot + "2026-11-26"}},
	}}
	extras := []Extra{
		{
			Entry:    Entry{ID: "ov:pattern:c1", Facet: "interaction", Text: "正式契约一"},
			Kind:     entry.KindRules,
			L1Kind:   BucketContract,
			Score:    120,
			Hard:     true,
			Polarity: "check",
			Body:     "正式契约一：变更前先核对环境",
			Evidence: []string{"fh-abs"},
		},
		{
			Entry:     Entry{ID: "ov:pattern:p1", Facet: "judgement", Text: "正式原理一"},
			Kind:      entry.KindUnderstanding,
			L1Kind:    BucketPrinciple,
			Score:     90,
			Dimension: "decision",
			Body:      "正式原理一：小步验证再推全量",
		},
	}
	res, _, err := PageWith(st, nil, extras, Params{Contribution: "memory", Limit: 50, ResidentBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	standingByID := map[string]Entry{}
	var hotIDs []string
	for _, e := range res.Entries {
		if e.Standing {
			standingByID[e.ID] = e
			if e.Rank >= 990 {
				hotIDs = append(hotIDs, e.ID)
			}
		}
	}
	// Hot slot holds at most 2: pin-1 (995), fh-1 (994). fh-2 (3rd), fh-old (expired), fh-abs (absorbed) excluded.
	wantHot := []string{"pin-1", "fh-1"}
	if !slices.Equal(hotIDs, wantHot) {
		t.Fatalf("hot slot = %v, want %v", hotIDs, wantHot)
	}
	if standingByID["pin-1"].Rank != rankHot || standingByID["fh-1"].Rank != rankHot-1 {
		t.Errorf("hot ranks: pin=%d fh1=%d", standingByID["pin-1"].Rank, standingByID["fh-1"].Rank)
	}
	if !strings.Contains(standingByID["fh-1"].Text, "（例外：回滚操作除外）") {
		t.Errorf("hot slot lost exception: %q", standingByID["fh-1"].Text)
	}
	if _, ok := standingByID["fh-old"]; ok {
		t.Error("expired fast_hot should not stand")
	}
	if _, ok := standingByID["fh-abs"]; ok {
		t.Error("L1-absorbed fast_hot should exit hot slot")
	}
	// Section order: Principle (p1) precedes Contract (c1).
	if standingByID["ov:pattern:p1"].Rank != rankStand || standingByID["ov:pattern:c1"].Rank != rankStand-1 {
		t.Errorf("section ranks: p1=%d c1=%d", standingByID["ov:pattern:p1"].Rank, standingByID["ov:pattern:c1"].Rank)
	}
}

// Contract section strictly enforces polarity caps (check <= 45%, act <= 35%, communicate <= 30%, route <= 30%),
// non-generic group cap (<= 20%), and HardContractLimit (<= 3 【硬】 lines).
func TestResidentContractPolarityGroupAndHardCap(t *testing.T) {
	var cs []Candidate
	for i := 0; i < 10; i++ {
		c := cand(fmt.Sprint("a", i), entry.KindRules, 130-i, 150)
		c.Hard = true
		c.Polarity = "act"
		c.Group = "interaction:autonomy"
		cs = append(cs, c)
	}
	for i := 0; i < 8; i++ {
		c := cand(fmt.Sprint("b", i), entry.KindRules, 110-i, 150)
		c.Hard = true
		c.Polarity = "check"
		cs = append(cs, c)
	}
	for i := 0; i < 8; i++ {
		c := cand(fmt.Sprint("c", i), entry.KindRules, 90-i, 150)
		c.Polarity = "communicate"
		cs = append(cs, c)
	}
	for i := 0; i < 8; i++ {
		c := cand(fmt.Sprint("d", i), entry.KindRules, 80-i, 150)
		c.Polarity = "route"
		cs = append(cs, c)
	}
	cs = append(cs,
		cand("p1", entry.KindUnderstanding, 85, 120),
		cand("p2", entry.KindUnderstanding, 80, 120),
	)
	room := 4096
	got := ResidentWithin(cs, room, ResidentMostFor(room))
	pBytes := 0
	polBytes := map[string]int{}
	grpBytes := map[string]int{}
	hardTagged := 0
	for _, c := range got {
		sz := len(c.Text) + residentOverhead
		if c.Bucket == BucketPrinciple {
			pBytes += sz
		} else if c.Bucket == BucketContract {
			polBytes[c.Polarity] += sz
			if c.Group != "" {
				grpBytes[c.Group] += sz
			}
			if strings.HasPrefix(c.Text, hardTagPrefix) {
				hardTagged++
			}
		}
	}
	cEff := room - pBytes
	if polBytes["act"] > int(float64(cEff)*0.35) {
		t.Errorf("act polarity bytes %d exceeded 35%% of effective contract budget %d", polBytes["act"], cEff)
	}
	if polBytes["check"] > int(float64(cEff)*0.45) {
		t.Errorf("check polarity bytes %d exceeded 45%% of effective contract budget %d", polBytes["check"], cEff)
	}
	if polBytes["communicate"] > int(float64(cEff)*0.30) {
		t.Errorf("communicate polarity bytes %d exceeded 30%% of effective contract budget %d", polBytes["communicate"], cEff)
	}
	if hardTagged > HardContractLimit {
		t.Errorf("hard-tagged contracts = %d, want <= %d", hardTagged, HardContractLimit)
	}
	if grpBytes["interaction:autonomy"] > int(float64(cEff)*GroupShare) {
		t.Errorf("group autonomy bytes %d exceeded 20%% of effective contract budget %d", grpBytes["interaction:autonomy"], cEff)
	}
	if polBytes["check"] == 0 || polBytes["communicate"] == 0 || polBytes["route"] == 0 {
		t.Errorf("expected all four polarities represented, got %v", polBytes)
	}
}

// Post-L1 unabsorbed universal interaction rules enter Bucket 0 (Hot Slot), while tools/ops_runbook rules are hard-banned.
func TestUnabsorbedRuleHotSlotTransition(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	st := &store.State{Entries: map[string]*entry.Entry{
		"i-new": {ID: "i-new", Facet: entry.Interaction, Grain: entry.Abstract, Text: "汇报结论前先核对原始事实再回答", Status: entry.Stable, Count: 12, First: "2026-11-17", Last: "2026-11-17", Tagged: true, Origin: entry.Behavior},
		"i-old": {ID: "i-old", Facet: entry.Interaction, Grain: entry.Abstract, Text: "早于 L1 检查点的旧规则不应进热槽", Status: entry.Stable, Count: 12, First: "2026-11-10", Last: "2026-11-15", Tagged: true, Origin: entry.Behavior},
		"t-ops": {ID: "t-ops", Facet: entry.Tools, Grain: entry.Abstract, Text: "共用机负载紧张时把编译门禁转到专用门禁机", Status: entry.Stable, Count: 55, First: "2026-11-17", Last: "2026-11-17", Tagged: true, Origin: entry.Behavior},
	}}
	if _, ok := UnabsorbedRuleHotCandidate(st, st.Entries["i-new"], nil, "2026-11-15", now); !ok {
		t.Fatal("new high-count universal interaction rule after l1LatestDay should enter hot slot")
	}
	if _, ok := UnabsorbedRuleHotCandidate(st, st.Entries["i-old"], nil, "2026-11-15", now); ok {
		t.Fatal("rule first seen on or before l1LatestDay must not enter transitional hot slot")
	}
	if _, ok := UnabsorbedRuleHotCandidate(st, st.Entries["t-ops"], nil, "2026-11-15", now); ok {
		t.Fatal("ops_runbook/tools rule must never enter hot slot")
	}
}

// Principle bucket selects strictly by unified leverage Score without forcing low-scoring dimensions into standing;
// L0 fragments never enter once L1 principles exist.
func TestResidentPrincipleScoreOrderAndNoL0Spill(t *testing.T) {
	var cs []Candidate
	for i := 0; i < 5; i++ {
		c := cand(fmt.Sprint("d", i), entry.KindUnderstanding, 110-i, 180)
		c.Dimension = "decision"
		cs = append(cs, c)
	}
	t1 := cand("t1", entry.KindUnderstanding, 70, 180)
	t1.Dimension = "thinking"
	cs = append(cs, t1)
	l0 := cand("l0", entry.KindUnderstanding, 150, 100)
	l0.L1 = false
	cs = append(cs, l0)

	got := ResidentWithin(cs, 950, ResidentMostFor(950))
	var ids []string
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	want := []string{"d0", "d1", "d2", "d3", "d4"}
	if !slices.Equal(ids, want) {
		t.Fatalf("principles should rank by Score without forced dimension padding: got %v, want %v", ids, want)
	}
	if slices.Contains(ids, "l0") {
		t.Fatalf("L0 fragment must never enter once L1 principles exist: %v", ids)
	}
}

// Profile summaries never stand, even when profile-summary is passed to PageWith.
func TestProfileSummaryNeverStands(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		"i-1": {ID: "i-1", Facet: entry.Interaction, Text: "concise replies", Status: entry.Stable, Count: 4, Last: "2026-11-20", Tagged: true, Origin: entry.Behavior},
	}}
	sum := profile.Build(st, time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC), "zh-Hans")
	res, _, err := PageWith(st, sum, nil, Params{Contribution: "memory", Limit: 50, ResidentBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Entries {
		if strings.HasPrefix(e.ID, "profile:") && e.Standing {
			t.Errorf("profile entry %s must never stand", e.ID)
		}
	}
}

// A rule's exception or principle's poles stay whole when the line is clipped.
func TestResidentTextKeepsExceptionAndPoles(t *testing.T) {
	e := &entry.Entry{Facet: entry.Interaction, Text: strings.Repeat("做完一轮不要停下等批准，", 12), Exception: "只有涉及线上、对外可见、删数据时才停"}
	line := ResidentText(e)
	if !strings.HasSuffix(line, "（例外：只有涉及线上、对外可见、删数据时才停）") || len(line) > HotTextMost {
		t.Errorf("line %q (%d bytes, max %d)", line, len(line), HotTextMost)
	}
	e.Text = "做完一轮不要停下等批准，只有涉及线上、对外可见、删数据时才停。"
	if strings.Contains(ResidentText(e), "例外") {
		t.Error("an exception already in the text is not said twice")
	}
	poles := "（两端：探索阶段时，放开验证；上线阶段时，严格收窄范围）"
	pl := FormatResidentLine(strings.Repeat("规划和执行任务时持续权衡收益与成本，", 10), poles, PrincipleTextMost)
	if !strings.HasSuffix(pl, poles) || len(pl) > PrincipleTextMost {
		t.Errorf("poles line %q (%d bytes, max %d)", pl, len(pl), PrincipleTextMost)
	}
}

// v0.4.0 §1.7: 4 KB three-section standing enforces balanced section budgets, <=3 【硬】 contracts,
// 0 ops_runbook, 0 S6 assistant_note pass-through, 0 ephemeral, 0 sensitive; ops_runbooks stay in
// on-demand recall with rank 840-940 (modulating by OpsRunbookWeight) and enriched tool/host Subjects;
// backfill never scans raw ev.Quote.
func TestV04ThreeSectionStandingAndHardBans(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	defer func() { nowFn = prevNow }()

	st := &store.State{Entries: map[string]*entry.Entry{
		"s6-note": {
			ID: "s6-note", Facet: entry.Interaction, Origin: entry.OriginAssistantNote, Status: entry.Stable,
			Text: "助手自记：切勿说我改了目标", Count: 20, Last: "2026-11-25", Tagged: true,
		},
		"s6-legacy": {
			ID: "s6-legacy", Facet: entry.Interaction, Origin: entry.Handwritten, Status: entry.Stable,
			Text: "旧版手写助手反思笔记不可直通", Count: 20, Last: "2026-11-25", Tagged: true,
		},
		"ops-fresh": {
			ID: "ops-fresh", Facet: entry.Tools, Origin: entry.Behavior, Status: entry.Stable,
			Text: "在 build-host-01 上跑 go test 门禁并检查 docker 状态", Count: 15, Last: "2026-11-20", Tagged: true,
			MemoryClass: entry.ClassOpsRunbook,
		},
		"ops-stale": {
			ID: "ops-stale", Facet: entry.Tools, Origin: entry.Behavior, Status: entry.Stable,
			Text: "旧版 cloudflare 额度 90% 分流策略", Count: 15, Last: "2026-09-10", Tagged: true,
			MemoryClass: entry.ClassOpsRunbook,
		},
		"eph-1": {
			ID: "eph-1", Facet: entry.World, Type: "status", Origin: entry.Behavior, Status: entry.Stable,
			Text: "2026-11-26 17:30 约了同步 call 跟进 PR #1234", Count: 5, Last: "2026-11-26", Tagged: true,
		},
		"sens-1": {
			ID: "sens-1", Facet: entry.Persona, Origin: entry.SelfReport, Status: entry.Stable,
			Text: "最近体检血糖偏高需要复查", Count: 3, Last: "2026-11-20", Tagged: true,
			Categories: []entry.Category{{Name: entry.CatHealth, By: entry.ByModel}},
		},
		"quote-not-tainted": {
			ID: "quote-not-tainted", Facet: entry.Interaction, Origin: entry.Behavior, Status: entry.Stable,
			Text: "汇报定期数据时直接给例行方案，不要只给一次性导出", Count: 4, Last: "2026-11-22", Tagged: false,
			Evidence: []entry.Evidence{{Source: entry.SrcAssistant, Ref: "c/1", Day: "2026-11-22", Who: entry.WhoSelf, Quote: "看我体检血压数据时要例行方案"}},
		},
	}}

	// Verify backfill does NOT scan ev.Quote: quote-not-tainted must stay non-sensitive.
	cats, space, _ := Tags(st, st.Entries["quote-not-tainted"])
	if len(cats) != 0 || space != "" {
		t.Fatalf("backfill must not scan raw ev.Quote for work rules: cats=%v space=%q", cats, space)
	}

	// Verify ops_runbook recall rank (840-940 fresh, decayed when stale) and enriched Subjects.
	if r := Rank(st.Entries["ops-fresh"]); r < 840 || r > 940 {
		t.Fatalf("fresh ops_runbook rank = %d, want in [840, 940]", r)
	}
	if rStale := Rank(st.Entries["ops-stale"]); rStale >= 400 {
		t.Fatalf("stale >=60d ops_runbook should decay strongly via OpsRunbookWeight, got rank=%d", rStale)
	}
	if subjs := Subjects(st, st.Entries["ops-fresh"]); !slices.Contains(subjs, "docker") {
		t.Fatalf("ops_runbook should enrich tool/host subjects, got %v", subjs)
	}

	// Build L1 extras across all 3 sections + 5 hard contracts to test <=3 【硬】 cap.
	var extras []Extra
	for i := 0; i < 4; i++ {
		extras = append(extras, Extra{
			Entry:  Entry{ID: fmt.Sprintf("ov:profile:p%d", i), Facet: "persona", Text: fmt.Sprintf("画像条目 %d", i)},
			Kind:   entry.KindUnderstanding,
			L1Kind: BucketProfile,
			Score:  125 - i,
			Body:   fmt.Sprintf("用户处于核心成长阶段条目 %d，不要把理性分析风格等同于冷漠。", i),
			Last:   "2026-11-25",
		})
	}
	for i := 0; i < 6; i++ {
		extras = append(extras, Extra{
			Entry:     Entry{ID: fmt.Sprintf("ov:pattern:pr%d", i), Facet: "judgement", Text: fmt.Sprintf("原理 %d", i)},
			Kind:      entry.KindUnderstanding,
			L1Kind:    BucketPrinciple,
			Score:     115 - i,
			Dimension: "decision",
			Body:      fmt.Sprintf("跨场景核心决策原理 %d：先做小样本实证验证，再决定是否放大投入。", i),
			Last:      "2026-11-25",
		})
	}
	pols := []string{"check", "check", "check", "check", "act", "act", "communicate", "communicate", "route"}
	for i, pol := range pols {
		extras = append(extras, Extra{
			Entry:    Entry{ID: fmt.Sprintf("ov:pattern:c%d", i), Facet: "interaction", Text: fmt.Sprintf("契约 %d", i)},
			Kind:     entry.KindRules,
			L1Kind:   BucketContract,
			Score:    130 - i,
			Hard:     i < 5, // 5 hard contracts -> at most 3 may carry 【硬】
			Polarity: pol,
			Body:     fmt.Sprintf("通用协作契约 %d（%s）：在对应场景下保持清晰边界与可追溯证据。", i, pol),
			Last:     "2026-11-25",
		})
	}

	res, _, err := PageWith(st, nil, extras, Params{Contribution: "memory", Limit: 100, ResidentBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	var profN, princN, contN, hardN, totalBytes int
	for _, e := range res.Entries {
		if !e.Standing {
			continue
		}
		totalBytes += len(e.Text) + residentOverhead
		for _, bad := range []string{"s6-note", "s6-legacy", "ops-fresh", "ops-stale", "eph-1", "sens-1"} {
			if e.ID == bad {
				t.Errorf("forbidden entry %s leaked into standing: %+v", bad, e)
			}
		}
		switch {
		case strings.HasPrefix(e.ID, "ov:profile:"):
			profN++
		case strings.HasPrefix(e.ID, "ov:pattern:pr"):
			princN++
		case strings.HasPrefix(e.ID, "ov:pattern:c"):
			contN++
			if strings.HasPrefix(e.Text, hardTagPrefix) {
				hardN++
			}
		}
	}
	if totalBytes > 4096 {
		t.Fatalf("standing exceeded 4096 B: %d", totalBytes)
	}
	if profN == 0 || princN == 0 || contN == 0 {
		t.Fatalf("all three sections must be populated: profile=%d principle=%d contract=%d", profN, princN, contN)
	}
	if hardN > HardContractLimit {
		t.Fatalf("hard-tagged contracts = %d, want <= %d", hardN, HardContractLimit)
	}
}

// v0.4.0 §1.7 Blank new-user cold start: 0 S6 handwritten notes, 0 L1 patterns, ONLY S1/S2 T1 behavior entries ->
// all three standing sections (Profile, Principle, Contract) must populate non-empty.
func TestV04BlankUserColdStartThreeSections(t *testing.T) {
	st := &store.State{Entries: map[string]*entry.Entry{
		"s1-prof": {
			ID: "s1-prof", Facet: entry.Persona, Origin: entry.Behavior, Status: entry.Stable,
			Text: "用户目前常不在电脑前，交付物需要直接发可读文档。", Count: 3, Last: "2026-11-25", Tagged: true,
			Evidence: []entry.Evidence{{Source: entry.SrcAssistant, Ref: "c/1", Day: "2026-11-25", Who: entry.WhoSelf, Quote: "我经常不在电脑前"}},
		},
		"s1-princ": {
			ID: "s1-princ", Facet: entry.Judgement, Origin: entry.Behavior, Status: entry.Stable,
			Text: "评估方案时优先先小样本实证再放大。", Situation: "新方案评估", Choice: "先小样本验证", Count: 4, Last: "2026-11-25", Tagged: true,
			Leverage: &entry.Leverage{Reach: entry.ReachCrossProject, Horizon: entry.HorizonLasting, Principle: "先小样本实证再放大"},
			Evidence: []entry.Evidence{{Source: entry.SrcAsk, Ref: "a/1", Day: "2026-11-25", Who: entry.WhoSelf, Quote: "先小样本试试"}},
		},
		"s2-cont": {
			ID: "s2-cont", Facet: entry.Interaction, Origin: entry.Behavior, Status: entry.Stable,
			Text: "结论先说人话并附直接建议，不抛开放选择题。", Count: 3, Last: "2026-11-25", Tagged: true,
			Leverage: &entry.Leverage{Reach: entry.ReachCrossProject, Horizon: entry.HorizonLasting},
			Evidence: []entry.Evidence{{Source: entry.SrcAsk, Ref: "a/2", Day: "2026-11-25", Who: entry.WhoSelf, Quote: "说人话直接给建议"}},
		},
	}}

	res, _, err := PageWith(st, nil, nil, Params{Contribution: "memory", Limit: 50, ResidentBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range res.Entries {
		if e.Standing {
			got[e.ID] = true
		}
	}
	for _, want := range []string{"s1-prof", "s1-princ", "s2-cont"} {
		if !got[want] {
			t.Errorf("blank-user cold start missing section entry %s; standing=%v", want, got)
		}
	}
}

// Bug 4 regression: standing lines must never contain mid-word "…" truncation and must deduplicate redundant
// exception suffixes that merely repeat the body.
func TestFormatResidentLineNoEllipsisAndDedupException(t *testing.T) {
	// 1. Long Chinese contract exceeding ContractTextMost trims at a complete sentence boundary without "…".
	longBody := "在多步骤协作任务中，先对齐核心目标与边界条件，再按既定计划自主连续推进全部子任务。" +
		"执行期间如果遇到非阻塞的小决策，直接按最优工程实践落地并记录依据，不在中途停下来反复请示。" +
		"每完成一个阶段后用两句话汇报已验证的结果和下一步安排，附上真实命令输出或文件链接作为证据。" +
		"若发现前置假设不成立或存在不可逆风险，立即停下说明现状和可选方案，等待用户拍板后再继续执行。"
	suffix := "（例外：涉及线上变更或删数据时必须停下确认）"
	line := FormatResidentLine(longBody, suffix, ContractTextMost)
	if line == "" || len(line) > ContractTextMost {
		t.Fatalf("expected non-empty line <= %dB, got len=%d: %q", ContractTextMost, len(line), line)
	}
	if strings.Contains(line, "…") {
		t.Fatalf("standing line must never contain mid-word ellipsis truncation: %q", line)
	}
	if !strings.HasSuffix(line, suffix) {
		t.Fatalf("expected exception suffix preserved after sentence-boundary trim: %q", line)
	}

	// 2. Redundant exception suffix overlapping body (Jaccard >= 0.45 or core phrase repeat) is omitted.
	dupBody := "在需求或技术分析尚未完备时，助手只做现状梳理和问题拆解，不直接给出最终拍板结论。"
	dupSuffix := "（例外：分析尚未完备时只做现状梳理，不直接给结论）"
	dedup := FormatResidentLine(dupBody, dupSuffix, ContractTextMost)
	if strings.Contains(dedup, "例外") {
		t.Fatalf("redundant exception suffix should be omitted, got %q", dedup)
	}

	// 3. Unpunctuated over-budget line returns "" (skipped) rather than emitting "…".
	unpunct := strings.Repeat("无标点超长连续中文契约内容测试", 20)
	if got := FormatResidentLine(unpunct, "", ContractTextMost); got != "" {
		t.Fatalf("unpunctuated over-budget line should be skipped cleanly, got %q", got)
	}

	// 4. End-to-end ResidentWithin produces 0 lines containing "…".
	got := ResidentWithin([]Candidate{
		{ID: "c1", Kind: entry.KindRules, Bucket: BucketContract, L1: true, Score: 100, Text: line},
		{ID: "c2", Kind: entry.KindRules, Bucket: BucketContract, L1: true, Score: 90, Text: dedup},
	}, 4096, 20)
	for _, c := range got {
		if strings.Contains(c.Text, "…") {
			t.Fatalf("ResidentWithin emitted truncated line with ellipsis: %q", c.Text)
		}
	}
}
