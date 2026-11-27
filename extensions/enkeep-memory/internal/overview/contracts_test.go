package overview

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
)

// Synthetic fixtures only: made-up projects, sources and quotes.

// Single-project + single-source evidence must fail the scene gate even with multiple weeks and sources;
// repeated identical quotes must fail the source gate; reported/assistant/other quotes do not count as user quotes.
func TestStrictSceneAndQuoteGates(t *testing.T) {
	a1 := action("sg1", entry.Judgement, "solo", entry.SrcAssistant, "2026-11-03", "first quote")
	a2 := action("sg2", entry.Judgement, "solo", entry.SrcAssistant, "2026-11-11", "second quote")
	a3 := action("sg3", entry.Judgement, "solo", entry.SrcAssistant, "2026-11-18", "third quote")
	st := state(a1, a2, a3)
	p := &Pattern{ID: "ov:scene", Facet: string(entry.Judgement), Kind: KindPrinciple, Text: "strict choice",
		DefaultContrast: "the usual way", EvidenceIDs: []string{"sg1", "sg2", "sg3"}}
	recount(st, p, "")
	if p.ScenesCount != 1 || p.Gate != GateScenes {
		t.Fatalf("single project + single source passed scene gate: scenes=%d gate=%q", p.ScenesCount, p.Gate)
	}

	// Duplicate identical quotes across two projects and two weeks must fail GateSources.
	d1 := action("dq1", entry.Judgement, "alpha", entry.SrcAssistant, "2026-11-03", "same")
	d2 := action("dq2", entry.Judgement, "beta", entry.SrcAsk, "2026-11-11", "same")
	d3 := action("dq3", entry.Judgement, "gamma", entry.SrcAgent, "2026-11-18", "same")
	for _, d := range []*entry.Entry{d1, d2, d3} {
		d.Evidence[0].Quote = "Always keep it simple!"
	}
	stDup := state(d1, d2, d3)
	pDup := &Pattern{ID: "ov:dup", Facet: string(entry.Judgement), Kind: KindPrinciple, Text: "keep it simple",
		DefaultContrast: "add layers", EvidenceIDs: []string{"dq1", "dq2", "dq3"}}
	recount(stDup, pDup, "")
	if pDup.UniqueQuotes != 1 || pDup.Gate != GateSources {
		t.Fatalf("duplicate quotes passed source gate: unique=%d gate=%q", pDup.UniqueQuotes, pDup.Gate)
	}

	// Reported or non-user quotes must not count as user quotes.
	r1 := action("rq1", entry.Judgement, "alpha", entry.SrcAssistant, "2026-11-03", "rep")
	r1.Evidence[0].Reported = true
	r2 := action("rq2", entry.Judgement, "beta", entry.SrcAsk, "2026-11-11", "other")
	r2.Evidence[0].Who = "person:other-1"
	r3 := action("rq3", entry.Judgement, "gamma", entry.SrcAgent, "2026-11-18", "bot")
	r3.Evidence[0].Role = "assistant"
	stRep := state(r1, r2, r3)
	pRep := &Pattern{ID: "ov:rep", Facet: string(entry.Judgement), Kind: KindPrinciple, Text: "reported only",
		DefaultContrast: "other way", EvidenceIDs: []string{"rq1", "rq2", "rq3"}}
	recount(stRep, pRep, "")
	if pRep.Gate != GateEmpty {
		t.Fatalf("non-user quotes counted: sources=%d gate=%q", pRep.SourcesCount, pRep.Gate)
	}
}

// counterCandidates must rank a same-situation / different-choice entry above a same-situation / same-choice entry.
func TestCounterCandidatesDownRankSameChoice(t *testing.T) {
	p := &Pattern{
		ID:        "ov:cc",
		Facet:     string(entry.Judgement),
		Kind:      KindPrinciple,
		Text:      "when shipping a release, run a small canary before full rollout",
		Situation: "shipping a production release",
		Choice:    "run a small canary first",
	}
	same := action("same", entry.Judgement, "alpha", entry.SrcAssistant, "2026-11-20", "shipping a production release: run a small canary first")
	same.Situation = "shipping a production release"
	same.Choice = "run a small canary first"
	opp := action("opp", entry.Judgement, "beta", entry.SrcAssistant, "2026-11-20", "shipping a production release: push straight to all hosts")
	opp.Situation = "shipping a production release"
	opp.Choice = "push straight to all hosts"

	cands := counterCandidates(state(same, opp), p, "2026-11-23", "")
	if len(cands) != 2 || cands[0].ID != "opp" {
		ids := make([]string, len(cands))
		for i, c := range cands {
			ids[i] = c.ID
		}
		t.Fatalf("expected opposite-choice candidate first, got %v", ids)
	}
}

// itemQuotes prefers WhoSelf quotes, deduplicates by normalised text, and caps at n.
// clusterCandidates groups candidates by Leverage.Dimension into compact clusters and passes quotes+dimension to synth.
func TestClusterCandidatesAndItemQuotes(t *testing.T) {
	makeItem := func(id, proj, src, day, dim, hint, text, quote string) *entry.Entry {
		e := action(id, entry.Judgement, proj, src, day, text)
		e.Evidence[0].Who = entry.WhoSelf
		e.Evidence[0].Quote = quote
		e.Leverage = &entry.Leverage{Dimension: dim, Hint: hint, Principle: "verify before claiming"}
		return e
	}
	e1 := makeItem("c1", "alpha", entry.SrcAssistant, "2026-11-09", "thinking", "verify-first", "check logs first", "先看日志再下结论")
	e1.Evidence = append(e1.Evidence,
		entry.Evidence{Source: entry.SrcAsk, Ref: "u/dup", Day: "2026-11-10", Who: entry.WhoSelf, Quote: "先看日志再下结论"},
		entry.Evidence{Source: entry.SrcAsk, Ref: "u/rep", Day: "2026-11-11", Who: entry.WhoSelf, Reported: true, Quote: "别人说他看日志"},
		entry.Evidence{Source: entry.SrcAsk, Ref: "u/second", Day: "2026-11-12", Who: entry.WhoSelf, Quote: "拿真实输出说话"},
	)
	e2 := makeItem("c2", "beta", entry.SrcAsk, "2026-11-12", "thinking", "verify-first", "inspect live state", "查一下线上实际状态")
	e3 := makeItem("c3", "gamma", entry.SrcAgent, "2026-11-18", "thinking", "verify-first", "reproduce before fix", "先复现再改")
	st := state(e1, e2, e3)

	qs := itemQuotes(st, e1, "", 2)
	if len(qs) != 2 || qs[0] != "先看日志再下结论" || qs[1] != "拿真实输出说话" {
		t.Fatalf("unexpected quotes %v", qs)
	}

	var seenInput synthInput
	m := &funcModel{fn: func(r model.Request) (any, error) {
		switch r.Step {
		case "overview_synth":
			_ = json.Unmarshal([]byte(r.User), &seenInput)
			var ids []string
			for _, it := range seenInput.Items {
				ids = append(ids, it.ID)
			}
			return synthOutput{Patterns: []synthProposal{{
				Text: "用户在做判断前先核验一手状态", Situation: "下结论或排查问题", Choice: "先核验一手状态",
				DefaultContrast: "凭印象直接判断", Dimension: "thinking", Members: ids, Reason: "same",
			}}}, nil
		case "overview_counter":
			var in counterInput
			_ = json.Unmarshal([]byte(r.User), &in)
			var o counterOutput
			for _, p := range in.Patterns {
				o.Verdicts = append(o.Verdicts, counterVerdict{ID: p.ID, Counters: []string{}, Reason: "none"})
			}
			return o, nil
		default:
			return map[string]any{}, nil
		}
	}}
	s := newState()
	if _, err := runner(m).Weekly(context.Background(), st, s, "2026-W47", ""); err != nil {
		t.Fatal(err)
	}
	if len(seenInput.Items) != 3 || seenInput.Items[0].Cluster != "dim:thinking" || len(seenInput.Items[0].Quotes) == 0 {
		t.Fatalf("expected dim:thinking cluster with quotes, got %+v", seenInput.Items)
	}
	p := byText(s, "用户在做判断前先核验一手状态")
	if p == nil || p.Kind != KindPrinciple || p.Dimension != "thinking" || p.Lifecycle != Core {
		t.Fatalf("unexpected principle %+v", p)
	}
}

// SyncContracts folds rules_merge into L1: merges synonyms, sets polarity, computes hard and violation_count across
// merged members, preserves all exceptions, leaves conflicting rules apart, records CoveredIDs for handwritten rules,
// promotes standalone hard rules, and skips redundant calls when the rule set signature has not changed.
func TestSyncContracts(t *testing.T) {
	makeRule := func(id, typ, text, exc, day, quote string, origin entry.Origin, count int) *entry.Entry {
		return &entry.Entry{
			ID: id, Facet: entry.Interaction, Grain: entry.Action, Type: typ,
			Text: text, Exception: exc, Rejected: "stop and ask",
			Status: entry.Stable, Origin: origin, Tagged: true,
			First: day, Last: day, Updated: day + "T00:00:00Z", Count: count,
			Leverage: &entry.Leverage{Reach: entry.ReachCrossProject, Horizon: entry.HorizonLasting},
			Evidence: []entry.Evidence{{Source: entry.SrcAssistant, Ref: "chat/" + id, Day: day, Who: entry.WhoSelf, Role: "user", Quote: quote}},
		}
	}
	// Two synonymous rules corrected on two days within 7 days: neither alone is Hard (1 day each, no hardCue),
	// but merged into one L1 contract they span 2 days within 7 days -> Hard=true, ViolationCount=2.
	r1 := makeRule("r-act-1", "correction", "已放行的事直接做", "", "2026-11-18", "放行了就直接做", entry.Behavior, 3)
	r2 := makeRule("r-act-2", "correction", "不要反复弹确认卡", "涉及线上或删数据时停下问", "2026-11-20", "别再弹确认卡了", entry.Behavior, 2)
	// User-owned (SelfReport) entry saying the same thing: must be recorded in CoveredIDs, never deleted.
	rHand := makeRule("r-hand", "correction", "已授权任务直接推进", "", "2026-11-19", "授权过的直接推进", entry.SelfReport, 2)
	// Conflicting rule: must not be merged into the act contract.
	rConf := makeRule("r-conf", "correction", "每次行动前都要先问", "", "2026-11-20", "动手前先问我", entry.Behavior, 2)
	// Standalone hard rule (explicit ban cue + user confirmation, single source, single week): must still become a used hard contract.
	rCheck := makeRule("r-check", "correction", "断言前先查真实状态", "", "2026-11-21", "以后一律先查真实状态再说", entry.Behavior, 2)
	rCheck.Verdicts = []entry.Verdict{{Mark: "right", By: "user"}}
	// Single-day casual cue without user confirmation or repeat days: matches entry.Hard's cue so it enters
	// contractCandidates, but L1 contractHard keeps it non-hard (violation_count==1).
	rSoftCue := makeRule("r-soft-cue", "correction", "汇报简短一点", "", "2026-11-21", "以后不要写太长", entry.Behavior, 1)

	st := state(r1, r2, rHand, rConf, rCheck, rSoftCue)
	calls := 0
	m := &funcModel{fn: func(r model.Request) (any, error) {
		switch r.Step {
		case "overview_contracts":
			calls++
			var in contractInput
			_ = json.Unmarshal([]byte(r.User), &in)
			idByText := map[string]string{}
			for _, rl := range in.Rules {
				idByText[rl.Text] = rl.ID
			}
			return contractOutput{Contracts: []contractProposal{
				{
					Rules:       []string{idByText[r1.Text], idByText[r2.Text], idByText[rHand.Text], idByText[rConf.Text]},
					Conflicting: []string{idByText[rConf.Text]},
					Polarity:    PolarityAct,
					Text:        "在已放行的任务上，助手直接推进不停下来问",
					Situation:   "已放行的任务",
					Choice:      "直接推进",
					Exception:   "涉及线上或删数据时停下问",
					Reason:      "same act rule",
				},
				{
					Rules:     []string{idByText[rCheck.Text]},
					Polarity:  PolarityCheck,
					Text:      "下结论前先核查真实状态再汇报",
					Situation: "汇报状态或下结论",
					Choice:    "先核查真实状态",
					Exception: "",
					Reason:    "standalone hard check rule",
				},
				{
					Rules:     []string{idByText[rSoftCue.Text]},
					Polarity:  PolarityCommunicate,
					Text:      "汇报时保持简短",
					Situation: "汇报进度",
					Choice:    "简短汇报",
					Exception: "",
					Reason:    "single-day casual cue",
				},
			}}, nil
		case "overview_counter":
			var in counterInput
			_ = json.Unmarshal([]byte(r.User), &in)
			var o counterOutput
			for _, p := range in.Patterns {
				o.Verdicts = append(o.Verdicts, counterVerdict{ID: p.ID, Counters: []string{}, Reason: "none"})
			}
			return o, nil
		default:
			return map[string]any{}, nil
		}
	}}

	s := newState()
	run, err := runner(m).Weekly(context.Background(), st, s, "2026-W47", "")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 contracts call, got %d", calls)
	}

	actP := byText(s, "在已放行的任务上，助手直接推进不停下来问")
	if actP == nil {
		t.Fatalf("act contract missing: %+v", s.Patterns)
	}
	if actP.Kind != KindContract || actP.Polarity != PolarityAct || !actP.Hard || actP.ViolationCount != 3 || !actP.Rooted {
		t.Fatalf("act contract fields wrong: %+v", actP)
	}
	if actP.Lifecycle != Scoped || actP.Exception != "涉及线上或删数据时停下问" {
		t.Fatalf("act contract lifecycle/exception wrong: %+v", actP)
	}
	if !slices.Contains(actP.CoveredIDs, "r-hand") {
		t.Fatalf("handwritten rule not recorded in CoveredIDs: %v", actP.CoveredIDs)
	}
	if slices.Contains(actP.EvidenceIDs, "r-conf") {
		t.Fatalf("conflicting rule was merged into act contract: %v", actP.EvidenceIDs)
	}
	if !rHand.Live() || rHand.MergedInto != "" {
		t.Fatalf("handwritten entry was mutated: %+v", rHand)
	}

	checkP := byText(s, "下结论前先核查真实状态再汇报")
	if checkP == nil || checkP.Polarity != PolarityCheck || !checkP.Hard || checkP.Lifecycle != Core {
		t.Fatalf("standalone hard check contract wrong: %+v", checkP)
	}
	softP := byText(s, "汇报时保持简短")
	if softP == nil || softP.Hard || softP.ViolationCount != 1 {
		t.Fatalf("single-day casual cue should NOT be hard: %+v", softP)
	}

	// Second call with unchanged rules must skip the model call.
	run2 := &Run{Counts: map[string]int{}}
	if _, err := runner(m).SyncContracts(context.Background(), st, s, run2, "", stamp(now)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || run2.Counts["contracts_skipped_unchanged"] != 1 {
		t.Fatalf("expected unchanged signature skip: calls=%d counts=%v (run1=%v)", calls, run2.Counts, run.Counts)
	}

	// L3 Refresh: marking the standalone hard contract's evidence wrong withdraws it; marking right restores it.
	rCheck.Verdicts = append(rCheck.Verdicts, entry.Verdict{Mark: "wrong", By: "user"})
	if !Refresh(st, s, now) || checkP.Used() || checkP.Withdrawn != Core {
		t.Fatalf("L3 did not withdraw hard contract after wrong mark: %+v", checkP)
	}
	rCheck.Verdicts = append(rCheck.Verdicts, entry.Verdict{Mark: "right", By: "user"})
	if !Refresh(st, s, now) || checkP.Lifecycle != Core {
		t.Fatalf("L3 did not restore hard contract after right mark: %+v", checkP)
	}
}

// Rooted patterns (core principles spanning >=4 ISO weeks and hard contracts) must NOT be era-closed by 90d silence.
func TestRootedNeverAutoClosedInMonthly(t *testing.T) {
	// 4 distinct ISO weeks for a project-bound principle.
	w1 := action("w1", entry.Judgement, "alpha", entry.SrcAssistant, "2026-10-05", "week 1")
	w2 := action("w2", entry.Judgement, "beta", entry.SrcAsk, "2026-10-12", "week 2")
	w3 := action("w3", entry.Judgement, "alpha", entry.SrcAgent, "2026-10-19", "week 3")
	w4 := action("w4", entry.Judgement, "beta", entry.SrcAssistant, "2026-10-26", "week 4")
	// Hard contract bound to a project (2 days within 7d).
	hc := &entry.Entry{
		ID: "hc1", Facet: entry.Interaction, Grain: entry.Action, Type: "correction",
		Text: "never guess state", Rejected: "guess", Status: entry.Stable, Origin: entry.Behavior, Tagged: true,
		First: "2026-10-10", Last: "2026-10-12", Count: 2,
		Scopes: []entry.Scope{{Kind: entry.ScopeProject, Name: "alpha"}},
		Evidence: []entry.Evidence{
			{Source: entry.SrcAssistant, Ref: "chat/hc1", Day: "2026-10-10", Who: entry.WhoSelf, Quote: "以后一律不要猜状态"},
			{Source: entry.SrcAssistant, Ref: "chat/hc2", Day: "2026-10-12", Who: entry.WhoSelf, Quote: "说了别猜状态"},
		},
	}
	st := state(w1, w2, w3, w4, hc)
	s := newState()
	p := &Pattern{ID: "ov:rootedp", Facet: string(entry.Judgement), Kind: KindPrinciple, Lifecycle: Core,
		Text: "always verifies", DefaultContrast: "assumes", EvidenceIDs: []string{"w1", "w2", "w3", "w4"}}
	c := &Pattern{ID: "ov:rootedhc", Facet: string(entry.Interaction), Kind: KindContract, Polarity: PolarityCheck,
		Lifecycle: Core, Text: "never guess state", EvidenceIDs: []string{"hc1"}}
	recount(st, p, "")
	recount(st, c, "")
	s.Patterns[p.ID], s.Patterns[c.ID] = p, c
	if !p.Rooted || !c.Rooted {
		t.Fatalf("expected both rooted: p=%+v c=%+v", p, c)
	}
	// Run monthly >90 days later.
	if _, err := runner(&fakeModel{}).Monthly(context.Background(), st, s, "2027-02", "2027-03-01"); err != nil {
		t.Fatal(err)
	}
	if p.Lifecycle != Core || c.Lifecycle != Core {
		t.Fatalf("rooted patterns were auto-closed: p=%s c=%s", p.Lifecycle, c.Lifecycle)
	}
}

type funcModel struct {
	fn func(model.Request) (any, error)
}

func (m *funcModel) Complete(_ context.Context, r model.Request) (model.Response, error) {
	out, err := m.fn(r)
	if err != nil {
		return model.Response{}, err
	}
	b, _ := json.Marshal(out)
	return model.Response{Text: string(b)}, nil
}

// Verify that existing Pattern JSON without Kind still infers KindOf() from Facet cleanly.
func TestPatternKindBackwardCompat(t *testing.T) {
	if got := (&Pattern{Facet: string(entry.Interaction)}).KindOf(); got != KindContract {
		t.Fatalf("interaction inferred %q", got)
	}
	if got := (&Pattern{Facet: string(entry.Tools)}).KindOf(); got != KindOpsRunbook {
		t.Fatalf("tools inferred %q, want %q", got, KindOpsRunbook)
	}
	if got := (&Pattern{Facet: string(entry.Judgement)}).KindOf(); got != KindPrinciple {
		t.Fatalf("judgement inferred %q", got)
	}
	if got := (&Pattern{Facet: string(entry.Persona)}).KindOf(); got != KindPrinciple {
		t.Fatalf("persona inferred %q", got)
	}
	// Verify time import is used via stamp.
	if !strings.Contains(stamp(time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)), "2026-11-26") {
		t.Fatal("stamp failed")
	}
}

// contractCandidates reserves a dedicated quota for tools and at least 40% of each facet for non-hard rules,
// so >100 hard interaction rules never starve tools or non-hard stable rules.
func TestContractCandidatesBalancedQuota(t *testing.T) {
	var all []*entry.Entry
	for i := 0; i < 120; i++ {
		id := "ih-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		all = append(all, &entry.Entry{
			ID: id, Facet: entry.Interaction, Grain: entry.Action, Type: "correction", Text: "hard " + id,
			Rejected: "wrong", Status: entry.Stable, Origin: entry.Behavior, Tagged: true,
			First: "2026-11-10", Last: "2026-11-12", Count: 5,
			Leverage: &entry.Leverage{Reach: entry.ReachCrossProject, Horizon: entry.HorizonLasting},
			Evidence: []entry.Evidence{{Source: entry.SrcAssistant, Ref: "c/" + id, Day: "2026-11-10", Who: entry.WhoSelf, Quote: "以后一律" + id}},
		})
	}
	for i := 0; i < 40; i++ {
		id := "ir-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		all = append(all, &entry.Entry{
			ID: id, Facet: entry.Interaction, Grain: entry.Action, Type: "style", Text: "reg " + id,
			Status: entry.Stable, Origin: entry.Behavior, Tagged: true,
			First: "2026-11-10", Last: "2026-11-12", Count: 6,
			Leverage: &entry.Leverage{Reach: entry.ReachCrossProject, Horizon: entry.HorizonLasting},
			Evidence: []entry.Evidence{{Source: entry.SrcAssistant, Ref: "c/" + id, Day: "2026-11-10", Who: entry.WhoSelf, Quote: "普通规矩" + id}},
		})
	}
	for i := 0; i < 30; i++ {
		id := "tr-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		all = append(all, &entry.Entry{
			ID: id, Facet: entry.Tools, Grain: entry.Action, Type: "tool_rule", Text: "tool " + id,
			Status: entry.Stable, Origin: entry.Behavior, Tagged: true,
			First: "2026-11-10", Last: "2026-11-12", Count: 4,
			Leverage: &entry.Leverage{Reach: entry.ReachCrossProject, Horizon: entry.HorizonLasting},
			Evidence: []entry.Evidence{{Source: entry.SrcAssistant, Ref: "c/" + id, Day: "2026-11-10", Who: entry.WhoSelf, Quote: "工具规矩" + id}},
		})
	}
	picked := contractCandidates(state(all...), "")
	if len(picked) != contractCandMost {
		t.Fatalf("expected %d candidates, got %d", contractCandMost, len(picked))
	}
	toolsN, nonHardInter := 0, 0
	for _, e := range picked {
		if e.Facet == entry.Tools {
			toolsN++
		} else if !entry.Hard(e) {
			nonHardInter++
		}
	}
	if toolsN != 30 {
		t.Fatalf("expected all 30 tools rules picked, got %d", toolsN)
	}
	if nonHardInter < 30 {
		t.Fatalf("expected >=30 non-hard interaction rules picked, got %d", nonHardInter)
	}
}

// Evidence majority Leverage.Dimension must win over a model's default "decision" guess on recount.
func TestEvidenceDimensionWinsOverModelGuess(t *testing.T) {
	e1 := action("ed1", entry.Judgement, "alpha", entry.SrcAssistant, "2026-11-09", "verify first")
	e2 := action("ed2", entry.Judgement, "beta", entry.SrcAsk, "2026-11-12", "check logs")
	e3 := action("ed3", entry.Judgement, "gamma", entry.SrcAgent, "2026-11-18", "probe root cause")
	for _, e := range []*entry.Entry{e1, e2, e3} {
		e.Leverage = &entry.Leverage{Dimension: "thinking"}
	}
	p := &Pattern{ID: "ov:evdim", Facet: string(entry.Judgement), Kind: KindPrinciple, Dimension: "decision",
		Text: "verifies before concluding", DefaultContrast: "guesses", EvidenceIDs: []string{"ed1", "ed2", "ed3"}}
	recount(state(e1, e2, e3), p, "")
	if p.Dimension != "thinking" {
		t.Fatalf("expected evidence majority dimension 'thinking', got %q", p.Dimension)
	}
}

// Tags follow v0.4.0 §1.6 Payload-First: work-methodology facets ignore non-payload ByModel/OriginSpace contagion
// while honoring explicit ByUser labels and genuine payload disclosures; non-work facets (persona/world) still confine
// restricted OriginSpace strictly.
func TestTagsSubjectOwnership(t *testing.T) {
	e1 := action("ts1", entry.Interaction, "alpha", entry.SrcAssistant, "2026-11-05", "stay within asked scope")
	e2 := action("ts2", entry.Interaction, "beta", entry.SrcAsk, "2026-11-10", "do not expand scope unasked")
	e3 := action("ts3", entry.Interaction, "gamma", entry.SrcAgent, "2026-11-15", "keep to the user's question")
	e3.Categories = []entry.Category{{Name: entry.CatEmotion, By: entry.ByModel}}
	p := &Pattern{ID: "ov:gen", Facet: string(entry.Interaction), Kind: KindContract, Text: "只做用户所问范围",
		EvidenceIDs: []string{"ts1", "ts2", "ts3"}}
	cats, space := tags(state(e1, e2, e3), p, p.EvidenceIDs)
	if len(cats) != 0 || space != "" {
		t.Fatalf("model emotion tag on work rule must not taint general contract: cats=%v space=%q", cats, space)
	}

	// Majority (2/3) explicit ByUser sensitive categories must taint even on work-methodology facets.
	e2.Categories = []entry.Category{{Name: entry.CatFinance, By: entry.ByUser}}
	e3.Categories = []entry.Category{{Name: entry.CatFinance, By: entry.ByUser}}
	cats, _ = tags(state(e1, e2, e3), p, p.EvidenceIDs)
	if !slices.Contains(cats, entry.CatFinance) {
		t.Fatalf("majority ByUser sensitive evidence must taint: cats=%v", cats)
	}

	// De-shelled general work contract does NOT inherit OriginSpace confinement; non-work persona pattern DOES.
	e1.OriginSpace = "private-space"
	e2.Categories, e3.Categories = nil, nil
	_, space = tags(state(e1, e2, e3), p, p.EvidenceIDs)
	if space != "" {
		t.Fatalf("work-methodology contract should not inherit OriginSpace, got %q", space)
	}
	pe1 := action("ps1", entry.Persona, "alpha", entry.SrcAssistant, "2026-11-05", "personal reflection")
	pe1.OriginSpace = "private-space"
	pp := &Pattern{ID: "ov:pers", Facet: string(entry.Persona), Kind: KindPrinciple, Text: "个人阶段反思",
		EvidenceIDs: []string{"ps1"}}
	_, space = tags(state(pe1), pp, pp.EvidenceIDs)
	if space != "private-space" {
		t.Fatalf("persona pattern must inherit OriginSpace, got %q", space)
	}
}

// Single-day repeated ban/anger cues without user confirmation must not promote a contract to Hard.
func TestContractHardSingleDayMultiCueNotHard(t *testing.T) {
	e := &entry.Entry{
		ID: "c-1day", Facet: entry.Interaction, Grain: entry.Action, Type: "correction",
		Text: "接受用户已给出的验证前提", Rejected: "反复质疑", Status: entry.Stable, Origin: entry.Behavior,
		First: "2026-11-17", Last: "2026-11-17", Count: 2, Tagged: true,
		Evidence: []entry.Evidence{
			{Source: entry.SrcAssistant, Ref: "u1", Day: "2026-11-17", Who: entry.WhoSelf, Quote: "以后不要再说没人验证过"},
			{Source: entry.SrcAssistant, Ref: "u2", Day: "2026-11-17", Who: entry.WhoSelf, Quote: "别再重复质疑这个前提"},
		},
	}
	hard, viol := contractHard(state(e), []string{"c-1day"}, "")
	if hard || viol != 1 {
		t.Fatalf("single-day multi-cue should stay non-hard with viol=1, got hard=%v viol=%d", hard, viol)
	}
}

// clusterCandidates must round-robin across dimension and general buckets so a large decision bucket cannot starve
// other dimensions or high-spread general history out of the first 3 batches.
func TestClusterCandidatesRoundRobin(t *testing.T) {
	var cands []candidate
	for i := 0; i < 100; i++ {
		e := action("dec-"+strings.Repeat("a", i%5+1)+string(rune('0'+i%10)), entry.Judgement, "alpha", entry.SrcAssistant, "2026-11-10", "dec")
		e.ID = "dec-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		e.Leverage = &entry.Leverage{Dimension: "decision"}
		cands = append(cands, candidate{e: e, t: tally{projects: []string{"alpha", "beta"}, sources: []string{"1", "2", "3"}}})
	}
	for i := 0; i < 6; i++ {
		e := action("gen", entry.Judgement, "alpha", entry.SrcAssistant, "2026-11-10", "gen")
		e.ID = "gen-" + string(rune('a'+i))
		cands = append(cands, candidate{e: e, t: tally{projects: []string{"alpha", "beta"}, sources: []string{"1", "2", "3"}}})
	}
	batches := clusterCandidates(cands)
	foundGeneral := false
	for _, b := range batches {
		for _, cl := range b {
			if cl.label == "general" {
				foundGeneral = true
			}
		}
	}
	if !foundGeneral {
		t.Fatalf("general bucket was starved out of batches by large decision bucket")
	}
}

// Abstract tools rules and user-owned rules must enter contractCandidates, and user-owned judgement entries must enter
// the core_owned priority cluster in clusterCandidates.
func TestAbstractAndOwnedCandidatesIncluded(t *testing.T) {
	tAbs := &entry.Entry{
		ID: "t-abs", Facet: entry.Tools, Grain: entry.Abstract, Text: "offload heavy builds to dedicated gate host",
		Status: entry.Stable, Origin: entry.Behavior, First: "2026-08-01", Last: "2026-11-20", Count: 50, Tagged: true,
		Evidence: []entry.Evidence{{Source: entry.SrcAgent, Ref: "a/1", Day: "2026-11-10", Who: entry.WhoSelf, Quote: "重活上专用门禁机"}},
	}
	jOwned := action("j-owned", entry.Judgement, "alpha", entry.SrcAbout, "2026-10-01", "design from first principles")
	jOwned.Origin = entry.SelfReport
	if isUserOwned(&entry.Entry{Origin: entry.OriginAssistantNote}) || isUserOwned(&entry.Entry{Origin: entry.Handwritten}) {
		t.Fatalf("S6 assistant_note/handwritten must not satisfy isUserOwned")
	}
	st := state(tAbs, jOwned)

	picked := contractCandidates(st, "")
	if len(picked) != 1 || picked[0].ID != "t-abs" {
		t.Fatalf("expected abstract tools rule in contractCandidates, got %+v", picked)
	}
	batches := clusterCandidates([]candidate{{e: jOwned, t: tally{sources: []string{"a", "b"}}}})
	if len(batches) == 0 || batches[0][0].label != "core_owned" {
		t.Fatalf("expected user-owned principle in core_owned cluster, got %+v", batches)
	}
}
