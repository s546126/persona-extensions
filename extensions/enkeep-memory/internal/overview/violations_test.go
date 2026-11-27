package overview

import (
	"testing"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
)

func TestContractViolationsWindowAndRate(t *testing.T) {
	e1 := &entry.Entry{
		ID: "e-no-reconfirm", Facet: entry.Interaction, Grain: entry.Action, Type: "correction",
		Text: "用户说过的事别再问一遍", Rejected: "反复弹卡确认", Status: entry.Stable, Origin: entry.Behavior,
		First: "2026-11-04", Last: "2026-12-03", Count: 3, Tagged: true,
		Evidence: []entry.Evidence{
			{Source: entry.SrcAssistant, Ref: "chat/old", Day: "2026-11-06", Who: entry.WhoSelf, Quote: "早就说过了"},
			{Source: entry.SrcAsk, Ref: "asks/1128", Day: "2026-11-28", Who: entry.WhoSelf, Quote: "说过别再问了"},
			{Source: entry.SrcAsk, Ref: "asks/1128", Day: "2026-11-28", Who: entry.WhoSelf, Quote: "重复同一张卡"},
			{Source: entry.SrcAssistant, Ref: "chat/1203", Day: "2026-12-03", Who: entry.WhoSelf, Quote: "又来问一遍"},
		},
	}
	eHot := &entry.Entry{
		ID: "e-hot-ctx", Facet: entry.Interaction, Grain: entry.Action, Type: "correction",
		Text: "待定项要带背景和建议", Rejected: "只丢术语让用户选", Status: entry.Stable, Origin: entry.Behavior,
		First: "2026-12-01", Last: "2026-12-01", Count: 1, Tagged: true,
		Flags: []string{"fast_hot:2026-12-01"},
		Evidence: []entry.Evidence{
			{Source: entry.SrcAssistant, Ref: "chat/1201", Day: "2026-12-01", Who: entry.WhoSelf, Quote: "背景都没讲清"},
		},
	}
	st := state(e1, eHot)
	s := newState()
	s.Patterns["ov:c1"] = &Pattern{
		ID: "ov:c1", Facet: string(entry.Interaction), Kind: KindContract, Lifecycle: Core,
		Polarity: PolarityAct, Hard: true, ViolationCount: 3, Text: "用户说过的事直接执行，不再反复确认",
		EvidenceIDs: []string{"e-no-reconfirm"},
	}
	s.Patterns["ov:sens"] = &Pattern{
		ID: "ov:sens", Facet: string(entry.Interaction), Kind: KindContract, Lifecycle: Core,
		Categories: []string{entry.CatFinance}, Text: "敏感契约", EvidenceIDs: []string{"e-no-reconfirm"},
	}

	got := ContractViolations(st, s, "2026-11-26", "2026-12-10", 50)
	if len(got) != 2 {
		t.Fatalf("expected 2 stats (1 contract + 1 unabsorbed fast_hot, excluding sensitive), got %+v", got)
	}
	if got[0].ID != "ov:c1" || got[0].Recurrences != 2 || len(got[0].Days) != 2 || got[0].Per100Cards != 4.0 {
		t.Fatalf("unexpected contract stat: %+v", got[0])
	}
	if got[1].ID != "e-hot-ctx" || got[1].Kind != "fast_hot" || got[1].Recurrences != 1 || got[1].Per100Cards != 2.0 {
		t.Fatalf("unexpected fast_hot stat: %+v", got[1])
	}
}
