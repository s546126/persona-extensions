package curate

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/hubapi"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/kbsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	srcs "github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/sources"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/triage"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/voice"
	"github.com/nwverse/persona/sdk/go/wire"
)

type sentUnitKB struct {
	day wire.KBDayResult
}

func (k sentUnitKB) List(context.Context, string) ([]wire.KBEntry, error) { return nil, nil }
func (k sentUnitKB) Get(_ context.Context, kind, id string) (wire.KBDetail, error) {
	raw, _ := json.Marshal(map[string]any{"id": id, "kind": "dm", "source": "lark", "name": "Demo DM"})
	return wire.KBDetail{Kind: kind, ID: id, Meta: raw}, nil
}
func (k sentUnitKB) Day(context.Context, string, string) (wire.KBDayResult, error) {
	return k.day, nil
}
func (k sentUnitKB) Recent(context.Context, time.Time, time.Time) ([]wire.KBEntry, error) {
	return []wire.KBEntry{{Kind: "chats", ID: "dm-1", Name: "Demo DM", Source: "lark", Updated: time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)}}, nil
}
func (k sentUnitKB) Update(context.Context, wire.KBUpdateParams) (map[string]json.RawMessage, error) {
	return nil, nil
}
func (k sentUnitKB) Owner(context.Context) (wire.KBOwner, error) { return wire.KBOwner{}, nil }

type ledgerUnitHub struct {
	hubapi.None
	entries []hubapi.LedgerEntry
}

func (h ledgerUnitHub) Ledger(context.Context, hubapi.LedgerQuery) ([]hubapi.LedgerEntry, error) {
	return hubapi.LedgerCleanForTest(h.entries), nil
}

func TestUnitSentProvenanceAndRecall(t *testing.T) {
	recalled := time.Date(2026, 11, 3, 9, 12, 0, 0, time.UTC)
	k := sentUnitKB{
		day: wire.KBDayResult{
			Day:        "2026-11-03",
			SentOrigin: "available",
			Messages: []wire.KBDayMessage{
				{N: 1, Text: "09:00 我: 亲口确认的方案"},
				{N: 2, Text: "09:05 我: 助手代发的回复", SentBy: "assistant", SentLedgerID: 11},
				{N: 3, Text: "09:10 我: 后来撤回的代发", SentBy: "assistant", SentLedgerID: 12, RecalledAt: recalled},
				{N: 4, Text: "09:15 Alice: 好的"},
			},
		},
	}
	c := &Curator{Src: k, Loc: time.UTC}
	u, ok, err := c.unit(context.Background(), "chats", wire.KBEntry{Kind: "chats", ID: "dm-1", Name: "Demo DM", Source: "lark"}, "2026-11-03")
	if err != nil || !ok {
		t.Fatalf("unit: ok=%v err=%v", ok, err)
	}
	if len(u.Lines) != 3 {
		t.Fatalf("lines = %d, want 3 (recalled dropped): %+v", len(u.Lines), u.Lines)
	}
	if u.Lines[0].Speaker != norm.Me {
		t.Fatalf("line 1 speaker = %q, want %q", u.Lines[0].Speaker, norm.Me)
	}
	if u.Lines[1].Speaker != voice.Ghost {
		t.Fatalf("line 2 (SentBy=assistant) speaker = %q, want %q", u.Lines[1].Speaker, voice.Ghost)
	}
	if u.Lines[2].Speaker != "Alice" {
		t.Fatalf("line 3 speaker = %q, want Alice", u.Lines[2].Speaker)
	}
}

func TestLedgerUnitCodeAndTriage(t *testing.T) {
	h := ledgerUnitHub{
		entries: []hubapi.LedgerEntry{
			{
				ID:    "l1",
				Type:  "rule",
				Text:  "always keep backups",
				At:    time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC),
				By:    "user",
				Trust: "user_verbatim",
			},
		},
	}
	src := srcs.With(&kbsrc.DryRun{Source: sentUnitKB{}}, h, time.UTC)
	cfg, err := ParseConfig(nil)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	c := &Curator{Src: src, Hub: h, Loc: time.UTC, Cfg: cfg}
	units, err := c.units(context.Background(), "2026-11-03")
	if err != nil {
		t.Fatalf("units: %v", err)
	}
	var ledgerUnit *Unit
	for i := range units {
		if units[i].Entity == srcs.KindLedger {
			ledgerUnit = &units[i]
			break
		}
	}
	if ledgerUnit == nil {
		t.Fatalf("expected ledger unit in %+v", units)
	}
	if got := unitCode(ledgerUnit); got != entry.SrcLedger {
		t.Fatalf("unitCode = %q, want %q", got, entry.SrcLedger)
	}
	if !aiUnit(ledgerUnit) {
		t.Fatalf("expected ledger unit to satisfy aiUnit")
	}
	d := triage.Decide(triage.Chat{Entity: ledgerUnit.Entity, Meta: ledgerUnit.Meta, Lines: ledgerUnit.Lines, Who: ledgerUnit.Who}, c.Cfg.triage())
	if d.Action != triage.Include || d.Tier != triage.TierOwn {
		t.Fatalf("triage decision = %+v, want Include/TierOwn", d)
	}
}
