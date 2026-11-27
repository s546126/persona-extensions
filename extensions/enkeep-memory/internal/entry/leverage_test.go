package entry

import (
	"testing"
	"time"
)

// All texts are placeholders.
func TestLeverageNormalizeAndMerge(t *testing.T) {
	if (&Leverage{Reach: "galaxy", Horizon: "forever"}).Normalize() != nil {
		t.Error("unknown values only: nothing kept")
	}
	e := &Entry{}
	e.MergeLeverage(&Leverage{Reach: ReachProject, Horizon: HorizonWeeks, Principle: "keep a way back", PrincipleInferred: true})
	e.MergeLeverage(&Leverage{Reach: ReachTaskKind, Horizon: HorizonLasting, Principle: "always keep a rollback"})
	l := e.Leverage
	if l.Reach != ReachProject || l.Horizon != HorizonLasting || l.Principle != "always keep a rollback" || l.PrincipleInferred {
		t.Errorf("merged %+v", l)
	}
}

// What reaches further and holds longer scores higher than a one-off detail seen as often; a candidate less than the
// same stable entry; pinned is 100.
func TestScoreOrders(t *testing.T) {
	wide := &Entry{Facet: Judgement, Status: Stable, Count: 3, Leverage: &Leverage{Reach: ReachCrossProject, Horizon: HorizonLasting, Principle: "p"}}
	detail := &Entry{Facet: Tools, Status: Stable, Count: 3, Leverage: &Leverage{Reach: ReachOneOff, Horizon: HorizonDays}}
	if Score(wide) <= Score(detail) {
		t.Errorf("wide %d <= detail %d", Score(wide), Score(detail))
	}
	cand := *wide
	cand.Status = Candidate
	if Score(&cand) >= Score(wide) {
		t.Error("a candidate scores as much as the stable one")
	}
	if Score(&Entry{Pinned: true}) != 100 {
		t.Error("pinned")
	}
	// Without labels (read before daily/17) a class scores above one action of the same count.
	if Score(&Entry{Facet: Judgement, Grain: Class, Status: Stable, Count: 2}) <= Score(&Entry{Facet: Judgement, Grain: Action, Status: Stable, Count: 2}) {
		t.Error("fallback: class not above action")
	}
}

func TestKindOf(t *testing.T) {
	for _, c := range []struct {
		e    *Entry
		want string
	}{
		{&Entry{Facet: Interaction}, KindRules}, {&Entry{Facet: Tools}, KindRules},
		{&Entry{Facet: Judgement}, KindUnderstanding}, {&Entry{Facet: Persona}, KindUnderstanding},
		{&Entry{Facet: World, Type: "decision"}, KindUnderstanding}, {&Entry{Facet: World, Type: "status"}, ""},
		{&Entry{Facet: World, Type: "decision", Predicate: "owns"}, ""}, {&Entry{Facet: People}, ""},
	} {
		if got := KindOf(c.e); got != c.want {
			t.Errorf("%+v: %q, want %q", c.e, got, c.want)
		}
	}
}

// Rooted: four ISO weeks of evidence, or a lasting cross-project principle on two days.
func TestRooted(t *testing.T) {
	e := &Entry{Facet: Judgement}
	if !Rooted(e, []string{"2026-10-05", "2026-10-12", "2026-10-19", "2026-10-26"}) {
		t.Error("four weeks")
	}
	if Rooted(e, []string{"2026-10-05", "2026-10-06", "2026-10-07"}) {
		t.Error("one week")
	}
	e.Leverage = &Leverage{Reach: ReachCrossProject, Horizon: HorizonLasting, Principle: "p"}
	if !Rooted(e, []string{"2026-10-05", "2026-10-06"}) || Rooted(e, []string{"2026-10-05"}) {
		t.Error("principle on two days")
	}
}

// 0.4.0 §2: single-day anger/ban cue L0 auto-promotion is removed; Hard requires either an explicit user Pin
// or multi-day user corrections within 7 days, and hard rules still score by their own leverage without a 90+ floor.
func TestHardRulesScoreByLeverageWithoutFloor(t *testing.T) {
	ev := func(day, quote string) Evidence { return Evidence{Who: WhoSelf, Day: day, Quote: quote} }
	singleAnger := &Entry{Facet: Interaction, Type: "correction", Status: Stable, Grain: Action, Count: 1,
		Evidence: []Evidence{ev("2026-07-14", "以后别再这样，真烦人")}, Leverage: &Leverage{Reach: ReachOneOff, Horizon: HorizonDays}}
	if Hard(singleAnger) {
		t.Fatalf("single-day anger/ban cue must NOT auto-promote to Hard in 0.4.0")
	}
	narrow := &Entry{Facet: Interaction, Type: "correction", Status: Stable, Grain: Action, Count: 2,
		Evidence: []Evidence{ev("2026-07-14", "别这样"), ev("2026-07-16", "又这样了")}, Leverage: &Leverage{Reach: ReachOneOff, Horizon: HorizonDays}}
	wide := &Entry{Facet: Interaction, Type: "correction", Status: Stable, Grain: Action, Count: 6,
		Evidence: []Evidence{ev("2026-11-01", "先核验"), ev("2026-11-05", "再说一遍先核验"), ev("2026-11-20", "先查再答")}, Leverage: &Leverage{Reach: ReachCrossProject, Horizon: HorizonMonths}}
	if !Hard(narrow) || !Hard(wide) {
		t.Fatal("expected multi-day corrections within 7 days to be Hard")
	}
	if !Hard(&Entry{Facet: Interaction, Pinned: true}) {
		t.Fatal("expected Pinned entry to be Hard")
	}
	if Score(narrow) >= 90 {
		t.Errorf("narrow hard rule should not be bumped to 90+: got %d", Score(narrow))
	}
	if Score(wide) <= Score(narrow) {
		t.Errorf("hard scores: narrow %d, wide %d", Score(narrow), Score(wide))
	}
	// S6 assistant_note / legacy handwritten must not receive the +10 user-owned score bonus.
	s6 := &Entry{Facet: Interaction, Origin: OriginAssistantNote, Status: Stable, Count: 2}
	beh := &Entry{Facet: Interaction, Origin: Behavior, Status: Stable, Count: 2}
	selfRep := &Entry{Facet: Interaction, Origin: SelfReport, Status: Stable, Count: 2}
	if Score(s6) != Score(beh) || Score(selfRep) <= Score(s6) {
		t.Errorf("score authority: s6=%d beh=%d selfReport=%d", Score(s6), Score(beh), Score(selfRep))
	}
}

func TestFastHotAndColdStart(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	e := &Entry{Facet: Interaction, Type: "correction", Status: Candidate}
	SetFastHot(e, "2026-11-20")
	SetFastHot(e, "2026-11-24")
	if got := FastHotDay(e); got != "2026-11-24" {
		t.Fatalf("FastHotDay = %q, want 2026-11-24", got)
	}
	if !IsFastHot(e, now, 7) {
		t.Fatal("expected active within 7 days")
	}
	if IsFastHot(e, now.AddDate(0, 0, 10), 7) {
		t.Fatal("expected expired after 7 days")
	}
	// ColdStartEligible: cross_project + lasting + principle (understanding), or hard/wideLasting (rules).
	pOK := &Entry{Facet: Judgement, Leverage: &Leverage{Reach: ReachCrossProject, Horizon: HorizonLasting, Principle: "verify first"}}
	pNoPrin := &Entry{Facet: Judgement, Leverage: &Leverage{Reach: ReachCrossProject, Horizon: HorizonLasting}}
	if !ColdStartEligible(pOK) || ColdStartEligible(pNoPrin) {
		t.Fatalf("cold start understanding: ok=%v noPrin=%v", ColdStartEligible(pOK), ColdStartEligible(pNoPrin))
	}
	rHard := &Entry{Facet: Interaction, Type: "correction", Evidence: []Evidence{
		{Who: WhoSelf, Day: "2026-11-20", Quote: "先检查"},
		{Who: WhoSelf, Day: "2026-11-22", Quote: "记得先检查"},
	}}
	rPlain := &Entry{Facet: Interaction, Type: "correction"}
	if !ColdStartEligible(rHard) || ColdStartEligible(rPlain) {
		t.Fatalf("cold start rules: hard=%v plain=%v", ColdStartEligible(rHard), ColdStartEligible(rPlain))
	}
}
