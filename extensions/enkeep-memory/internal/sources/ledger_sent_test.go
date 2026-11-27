package sources

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/hubapi"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/kbsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
	"github.com/nwverse/persona/sdk/go/wire"
)

type ledgerTestHub struct {
	hubapi.None
	entries []hubapi.LedgerEntry
	err     error
}

func (h *ledgerTestHub) Ledger(context.Context, hubapi.LedgerQuery) ([]hubapi.LedgerEntry, error) {
	if h.err != nil {
		return nil, h.err
	}
	return hubapi.LedgerCleanForTest(h.entries), nil
}

type sentTestKB struct {
	kb
	days map[string]wire.KBDayResult
}

func (k *sentTestKB) Day(_ context.Context, chat, day string) (wire.KBDayResult, error) {
	if r, ok := k.days[chat+"@"+day]; ok {
		return r, nil
	}
	return wire.KBDayResult{}, kbsrc.ErrNotFound
}

func TestLedgerReadDedupSpaceAndCursor(t *testing.T) {
	ctx := context.Background()
	h := &ledgerTestHub{
		entries: []hubapi.LedgerEntry{
			{
				ID:    "e-old",
				Type:  "decision",
				Text:  "old decision",
				At:    at(3, 9),
				By:    "user",
				Trust: "user_verbatim",
			},
			{
				ID:         "e-new",
				Type:       "decision",
				Text:       "use main branch for release",
				Quote:      "release from main only",
				At:         at(3, 10),
				By:         "user",
				Trust:      "user_verbatim",
				Supersedes: "e-old",
				Scope:      hubapi.LedgerScopeOf{Project: []string{"demo-proj"}},
				Decision: &hubapi.LedgerDecision{
					Selected: []string{"main"},
					Rejected: []string{"dev"},
					Reason:   "single release line",
				},
			},
			// Duplicate id in response should be ignored.
			{
				ID:    "e-new",
				Type:  "decision",
				Text:  "use main branch for release",
				At:    at(3, 10),
				By:    "user",
				Trust: "user_verbatim",
			},
			// Self-written by enkeep must be ignored to avoid feedback loops.
			{
				ID:   "e-self",
				Type: "rule",
				Text: "enkeep internal",
				At:   at(3, 11),
				By:   "enkeep",
			},
			// Agent entry in a separate space keeps its origin space and uses SpeakerLedger.
			{
				ID:          "e-health",
				Type:        "fact",
				Text:        "weekly sleep summary",
				At:          at(3, 12),
				By:          "agent",
				Trust:       "agent",
				OriginSpace: "health",
			},
			// Another day must not appear on day3.
			{
				ID:   "e-other-day",
				Type: "rule",
				Text: "next day rule",
				At:   at(4, 8),
				By:   "user",
			},
		},
	}

	s := With(&kb{}, h, time.UTC)
	st := &store.State{}
	s.Begin(st, "2026-11-03")
	got, err := s.Recent(ctx, day3, day3.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	var ledgerIDs []string
	for _, e := range got {
		if e.Kind == KindLedger {
			ledgerIDs = append(ledgerIDs, e.ID)
		}
	}
	if len(ledgerIDs) != 2 || ledgerIDs[0] != "health/2026-11-03" || ledgerIDs[1] != "2026-11-03" {
		t.Fatalf("ledger units = %v, want [health/2026-11-03 2026-11-03]", ledgerIDs)
	}

	// Main space unit: meta code, structured decision, quote, project, and SpeakerUser.
	metaMain, err := s.Get(ctx, KindLedger, "2026-11-03")
	if err != nil {
		t.Fatalf("Get main: %v", err)
	}
	if m := kbsrc.DecodeMeta(metaMain); m.Code != entry.SrcLedger || m.Space != "" {
		t.Fatalf("main meta = %+v", m)
	}
	dayMain, err := s.Day(ctx, KindLedger+"/2026-11-03", "2026-11-03")
	if err != nil {
		t.Fatalf("Day main: %v", err)
	}
	if len(dayMain.Messages) != 1 {
		t.Fatalf("main lines = %d, want 1: %+v", len(dayMain.Messages), dayMain.Messages)
	}
	line := dayMain.Messages[0].Text
	for _, want := range []string{"10:00 " + SpeakerUser + ":", "[决定]", "use main branch for release", "选定：main", "否决：dev", "理由：single release line", "原话：「release from main only」", "（项目 demo-proj）"} {
		if !strings.Contains(line, want) {
			t.Fatalf("main line %q missing %q", line, want)
		}
	}

	// Health space unit carries OriginSpace and SpeakerLedger.
	metaHealth, err := s.Get(ctx, KindLedger, "health/2026-11-03")
	if err != nil {
		t.Fatalf("Get health: %v", err)
	}
	if m := kbsrc.DecodeMeta(metaHealth); m.Code != entry.SrcLedger || m.Space != "health" {
		t.Fatalf("health meta = %+v", m)
	}
	dayHealth, err := s.Day(ctx, KindLedger+"/health/2026-11-03", "2026-11-03")
	if err != nil {
		t.Fatalf("Day health: %v", err)
	}
	if len(dayHealth.Messages) != 1 || !strings.Contains(dayHealth.Messages[0].Text, "12:00 "+SpeakerLedger+": [事实] weekly sleep summary") {
		t.Fatalf("health line = %+v", dayHealth.Messages)
	}

	s.End(st, "2026-11-03", 2)
	if st.Sources["S12/ledger"] == nil || !st.Sources["S12/ledger"].Available || st.Sources["S12/ledger"].Units != 2 || st.Sources["S12/ledger"].Lines != 2 {
		t.Fatalf("S12 status = %+v", st.Sources["S12/ledger"])
	}
	if st.Cursors[ledgerCursorKey("2026-11-03")] == "" || st.Cursors[ledgerCursorKey("health/2026-11-03")] == "" {
		t.Fatalf("missing ledger cursors: %+v", st.Cursors)
	}

	// Second pass with unchanged entries skips via cursor.
	s.Begin(st, "2026-11-03")
	got2, err := s.Recent(ctx, day3, day3.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("Recent 2: %v", err)
	}
	for _, e := range got2 {
		if e.Kind == KindLedger {
			t.Fatalf("expected unchanged ledger to be skipped by cursor, got %+v", e)
		}
	}

	// Adding a new entry on the same day invalidates that unit's cursor only.
	h.entries = append(h.entries, hubapi.LedgerEntry{
		ID:    "e-added",
		Type:  "rule",
		Text:  "keep responses concise",
		At:    at(3, 15),
		By:    "user",
		Trust: "user_verbatim",
	})
	s.Begin(st, "2026-11-03")
	got3, err := s.Recent(ctx, day3, day3.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("Recent 3: %v", err)
	}
	var updatedIDs []string
	for _, e := range got3 {
		if e.Kind == KindLedger {
			updatedIDs = append(updatedIDs, e.ID)
		}
	}
	if len(updatedIDs) != 1 || updatedIDs[0] != "2026-11-03" {
		t.Fatalf("updated ledger units = %v, want [2026-11-03]", updatedIDs)
	}
}

func TestLedgerMissingAndDeniedStates(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		err     error
		missing string
	}{
		{name: "denied", err: &hubapi.LedgerDenied{Scope: hubapi.LedgerScope}, missing: "no_scope"},
		{name: "waiting", err: &hubapi.WaitingHub{Change: "I1"}, missing: "waiting_hub"},
		{name: "empty", err: nil, missing: "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := With(&kb{}, &ledgerTestHub{err: tc.err}, time.UTC)
			st := &store.State{}
			s.Begin(st, "2026-11-03")
			_, _ = s.Recent(ctx, day3, day3.AddDate(0, 0, 1))
			s.End(st, "2026-11-03", 0)
			row := st.Sources["S12/ledger"]
			if row == nil || row.Missing != tc.missing {
				t.Fatalf("S12 status = %+v, want missing=%q", row, tc.missing)
			}
		})
	}
}

func TestSentOriginTrackingDedupAndCursor(t *testing.T) {
	ctx := context.Background()
	k := &sentTestKB{
		days: map[string]wire.KBDayResult{
			"chat-a@2026-11-03": {
				Day:        "2026-11-03",
				SentOrigin: "available",
				Messages: []wire.KBDayMessage{
					{N: 1, Text: "09:00 我: 收到"},
					{N: 2, Text: "09:05 我: 我来跟进", SentBy: "assistant", SentLedgerID: 101},
					{N: 3, Text: "09:10 我: 明天同步", SentBy: "assistant", SentLedgerID: 105},
				},
			},
			// Overlapping chat carrying the same SentLedgerID 101 must not double-count it.
			"chat-b@2026-11-03": {
				Day:        "2026-11-03",
				SentOrigin: "available",
				Messages: []wire.KBDayMessage{
					{N: 1, Text: "09:05 我: 我来跟进", SentBy: "assistant", SentLedgerID: 101},
				},
			},
		},
	}
	s := With(k, hubapi.None{}, time.UTC)
	st := &store.State{}
	s.Begin(st, "2026-11-03")
	if _, err := s.Day(ctx, "chat-a", "2026-11-03"); err != nil {
		t.Fatalf("Day chat-a: %v", err)
	}
	if _, err := s.Day(ctx, "chat-b", "2026-11-03"); err != nil {
		t.Fatalf("Day chat-b: %v", err)
	}
	s.End(st, "2026-11-03", 2)

	row := st.Sources["S13/sent"]
	if row == nil || !row.Available || row.Units != 1 || row.Lines != 2 {
		t.Fatalf("S13 status = %+v, want available=true units=1 lines=2", row)
	}
	if got := st.Cursors[sentCursorKey]; got != "105|2026-11-03" {
		t.Fatalf("S13 cursor = %q, want 105|2026-11-03", got)
	}

	// Missing scope records no_scope cleanly.
	k2 := &sentTestKB{
		days: map[string]wire.KBDayResult{
			"chat-c@2026-11-03": {
				Day:        "2026-11-03",
				SentOrigin: "missing_scope",
				Messages:   []wire.KBDayMessage{{N: 1, Text: "09:00 我: hello"}},
			},
		},
	}
	s2 := With(k2, hubapi.None{}, time.UTC)
	st2 := &store.State{}
	s2.Begin(st2, "2026-11-03")
	_, _ = s2.Day(ctx, "chat-c", "2026-11-03")
	s2.End(st2, "2026-11-03", 1)
	if r2 := st2.Sources["S13/sent"]; r2 == nil || r2.Missing != "no_scope" {
		t.Fatalf("S13 missing_scope status = %+v", r2)
	}
}
