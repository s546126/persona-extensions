package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/overview"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// Synthetic fixtures only.

// Phase 1 demotes stable LOW_CONFIDENCE one-sighting persona misreads (never the user's own) and takes known legal_id
// false positives (a Visa card payment, "被告状") off non-work entries, keeping real visas and user labels; idempotent.
func TestMigrateV04Phase1LowConfidenceAndLegalIDFalsePositives(t *testing.T) {
	today := "2026-11-27"
	model := func(names ...string) []entry.Category {
		var out []entry.Category
		for _, n := range names {
			out = append(out, entry.Category{Name: n, By: entry.ByModel})
		}
		return out
	}
	ev := []entry.Evidence{{Source: entry.SrcChat, Ref: "chats/x", Day: "2026-09-21", Who: entry.WhoSelf, Quote: "q"}}
	st := &store.State{Entries: map[string]*entry.Entry{
		"u-low": {ID: "u-low", Facet: entry.Persona, Status: entry.Stable, Count: 1, Origin: entry.Behavior, Evidence: ev,
			Text: "用户在拒绝一个面试后遭到对方告状，感到烦躁，并自我回顾读博的时间。", Flags: []string{entry.FlagLowConfidence, "standalone"},
			Leverage: &entry.Leverage{Reach: entry.ReachOneOff}, Categories: model(entry.CatEmotion, entry.CatLegalID)},
		"u-self": {ID: "u-self", Facet: entry.Persona, Status: entry.Stable, Count: 1, Origin: entry.SelfReport, Evidence: ev,
			Text: "用户最近在准备一次分享", Flags: []string{entry.FlagLowConfidence}},
		"u-twice": {ID: "u-twice", Facet: entry.Persona, Status: entry.Stable, Count: 3, Origin: entry.Behavior, Evidence: ev,
			Text: "用户以凌晨为清醒窗口", Flags: []string{entry.FlagLowConfidence}},
		"p-visa": {ID: "p-visa", Facet: entry.People, Person: "person:x", Part: entry.PartFact, Status: entry.Stable, Count: 1,
			Origin: entry.Behavior, Evidence: ev, Text: "user-x 的第一个号用 Google Play 付款，用的是国内 visa 和日本账号。",
			Categories: model(entry.CatLegalID)},
		"u-visa": {ID: "u-visa", Facet: entry.Persona, Status: entry.Stable, Count: 1, Origin: entry.Behavior, Evidence: ev,
			Text: "用户目前签证仍在有效期内，回国安排受 10 月 H1B 影响的判断牵制。", Categories: model(entry.CatLegalID)},
		"u-user": {ID: "u-user", Facet: entry.Persona, Status: entry.Stable, Count: 2, Origin: entry.Behavior, Evidence: ev,
			Text:       "用户被同事告状后整理了一份说明",
			Categories: []entry.Category{{Name: entry.CatLegalID, By: entry.ByUser}}},
	}}
	rep, err := RunMigrateV04Phase1(context.Background(), st, today)
	if err != nil {
		t.Fatal(err)
	}
	if rep.LowConfidencePersonaDemoted != 1 || st.Entries["u-low"].Status != entry.Candidate {
		t.Errorf("low-confidence demoted = %d, u-low status %q", rep.LowConfidencePersonaDemoted, st.Entries["u-low"].Status)
	}
	if st.Entries["u-self"].Status != entry.Stable || st.Entries["u-twice"].Status != entry.Stable {
		t.Error("the user's own entry or a repeated one was demoted")
	}
	if rep.LegalIDFalsePositiveCleared != 2 {
		t.Errorf("legal_id false positives cleared = %d, want 2", rep.LegalIDFalsePositiveCleared)
	}
	if cats := st.Entries["u-low"].Cats(); !slices.Equal(cats, []string{entry.CatEmotion}) {
		t.Errorf("u-low cats = %v, want only emotion", cats)
	}
	if cats := st.Entries["p-visa"].Cats(); len(cats) != 0 {
		t.Errorf("visa card payment keeps %v", cats)
	}
	for _, id := range []string{"u-visa", "u-user"} {
		if !slices.Contains(st.Entries[id].Cats(), entry.CatLegalID) {
			t.Errorf("%s lost legal_id", id)
		}
	}
	rep2, err := RunMigrateV04Phase1(context.Background(), st, today)
	if err != nil || rep2.LowConfidencePersonaDemoted != 0 || rep2.LegalIDFalsePositiveCleared != 0 {
		t.Errorf("second run: %+v err=%v", rep2, err)
	}
}

// uniqText is n characters of the idx-th run of CJK characters, so no two fixture lines are alike.
func uniqText(idx, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(rune(0x4e00 + idx*30 + i%30))
	}
	return b.String()
}

// review-bundle renders the resident report for 4, 8 and 16 KB rooms within the room, with Chinese review notes and
// no English debug tags; an over-large room is bounded to 16 KB.
func TestReviewBundleResidentRooms(t *testing.T) {
	dir := t.TempDir()
	today := "2026-11-27"
	st := &store.State{Version: store.Version, ActiveDays: []string{today}, Entries: map[string]*entry.Entry{}}
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("i-%010d", i)
		st.Entries[id] = &entry.Entry{ID: id, Facet: entry.Interaction, Grain: entry.Class, Origin: entry.Behavior,
			Status: entry.Stable, Tagged: true, Count: 3, First: "2026-11-01", Last: today,
			Text:     uniqText(100+i, 20) + "。",
			Evidence: []entry.Evidence{{Source: entry.SrcAssistant, Ref: fmt.Sprintf("chat/%d", i), Day: today, Who: entry.WhoSelf, Quote: "先核对"}}}
	}
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ovH, err := overview.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ovH.Update(func(s *overview.State) (bool, error) {
		for i := 0; i < 6; i++ {
			id := fmt.Sprintf("ov:pc%02d", i)
			s.Patterns[id] = &overview.Pattern{ID: id, Facet: string(entry.Persona), Kind: overview.KindProfileCore,
				Lifecycle: overview.Core, Rooted: true, Text: uniqText(i, 40) + "。",
				EvidenceIDs: []string{"i-0000000000"}, ScenesCount: 3, SourcesCount: 4, UniqueQuotes: 4,
				ActiveWeeks: []string{"2026-W46", "2026-W47"}}
		}
		for i := 0; i < 12; i++ {
			id := fmt.Sprintf("ov:pr%02d", i)
			s.Patterns[id] = &overview.Pattern{ID: id, Facet: string(entry.Judgement), Kind: overview.KindPrinciple,
				Lifecycle: overview.Core, Rooted: true, Text: uniqText(10+i, 40) + "。",
				EvidenceIDs: []string{"i-0000000001"}, ScenesCount: 3, SourcesCount: 5, UniqueQuotes: 4,
				ActiveWeeks: []string{"2026-W46", "2026-W47"}}
		}
		for i := 0; i < 30; i++ {
			id := fmt.Sprintf("ov:ct%02d", i)
			s.Patterns[id] = &overview.Pattern{ID: id, Facet: string(entry.Interaction), Kind: overview.KindContract,
				Lifecycle: overview.Core, Hard: i < 4, Polarity: []string{overview.PolarityCheck, "act", "communicate", "route"}[i%4],
				Text:        uniqText(30+i, 30) + "。",
				EvidenceIDs: []string{fmt.Sprintf("i-%010d", i)}, ScenesCount: 3, SourcesCount: 6, UniqueQuotes: 5,
				ActiveWeeks: []string{"2026-W46", "2026-W47"}}
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ room, want int }{{4096, 4096}, {8192, 8192}, {16384, 16384}, {65536, 16384}} {
		out := filepath.Join(t.TempDir(), fmt.Sprint(tc.room))
		sum, err := GenerateReviewBundle(dir, out, "zh-Hans", tc.room, today)
		if err != nil {
			t.Fatalf("room %d: %v", tc.room, err)
		}
		if sum.ResidentBytesLimit != tc.want || sum.ResidentBytesActual > tc.want || sum.ResidentLines == 0 {
			t.Errorf("room %d: limit=%d actual=%d lines=%d", tc.room, sum.ResidentBytesLimit, sum.ResidentBytesActual, sum.ResidentLines)
		}
		if sum.ResidentProfileLines != 6 {
			t.Errorf("room %d: profile lines = %d, want 6", tc.room, sum.ResidentProfileLines)
		}
		md, _ := os.ReadFile(filepath.Join(out, "resident-v04.md"))
		s := string(md)
		if !strings.Contains(s, "不占用实际注入的常驻字节") || !strings.Contains(s, fmt.Sprintf("%d KB", tc.want/1024)) {
			t.Errorf("room %d: report header missing:\n%s", tc.room, s[:min(len(s), 400)])
		}
		for _, bad := range []string{"rank=", "facet=", "【画像】", "【原则】", "两端："} {
			if strings.Contains(s, bad) {
				t.Errorf("room %d: report still carries %q", tc.room, bad)
			}
		}
		if !strings.Contains(s, "[画]") || !strings.Contains(s, "[原]") {
			t.Errorf("room %d: short tags missing", tc.room)
		}
	}
}
