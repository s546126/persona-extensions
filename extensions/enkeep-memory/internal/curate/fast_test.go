package curate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/export"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/kbsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/stub"
)

// The fast path reads today's assistant conversation once the user corrects the assistant in it, and only lines it
// has not looked at; other conversations and other sources are not read. Synthetic fixture.
func TestRunFast(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../testdata/sample")); err != nil {
		t.Fatal(err)
	}
	conv := filepath.Join(dir, "chats", "persona-1")
	if err := os.MkdirAll(filepath.Join(conv, "messages"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(conv, "meta.json"), `{"source":"persona","kind":"assistant","name":"助手","id":"persona-1"}`)
	day := filepath.Join(conv, "messages", "2026-11-09.md")
	write(day, "# 2026-11-09\n\n- 09:00 我: 把磁盘清理一下\n- 09:01 助手: 要开始吗？\n")
	fx, err := kbsrc.LoadFixture(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &stub.Model{}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	c := &Curator{Src: fx, Model: m, St: st, Cfg: Defaults(), Loc: time.UTC, Now: func() time.Time { return now }}
	ctx := context.Background()
	if n, err := c.RunFast(ctx); err != nil || n != 0 || m.Calls["daily"] != 0 {
		t.Fatalf("no correction yet: %d %v %v", n, err, m.Calls)
	}
	write(day, "# 2026-11-09\n\n- 09:00 我: 把磁盘清理一下\n- 09:01 助手: 要开始吗？\n- 09:02 我: 我说过了，直接做，别再问我\n")
	if fx, err = kbsrc.LoadFixture(dir); err != nil {
		t.Fatal(err)
	}
	c.Src = fx
	if n, err := c.RunFast(ctx); err != nil || n != 1 || m.Calls["daily"] == 0 {
		t.Fatalf("correction: %d %v %v", n, err, m.Calls)
	}
	calls := m.Calls["daily"]
	if n, _ := c.RunFast(ctx); n != 0 || m.Calls["daily"] != calls {
		t.Errorf("read again without new lines: %d", n)
	}
	if c.St.S.FastSeen["chats/persona-1@2026-11-09"] == 0 || len(c.St.S.ActiveDays) != 0 {
		t.Errorf("seen %v, active %v", c.St.S.FastSeen, c.St.S.ActiveDays)
	}
	// Any newly observed interaction/tools correction rule from RunFast is tagged fast_hot:<day> for the hot slot.
	for _, e := range c.St.S.Entries {
		if entry.KindOf(e) == entry.KindRules && (e.Type == "correction" || e.Rejected != "" || entry.Hard(e)) {
			if got := entry.FastHotDay(e); got != "2026-11-09" {
				t.Errorf("fast path correction %s missing fast_hot:2026-11-09, got %q", e.ID, got)
			}
		}
	}
}

// A correction made on two days within a week, or explicitly Pinned, is Hard (0.4.0 §2: single-day anger/ban cue
// no longer auto-promotes to Hard).
func TestHardRule(t *testing.T) {
	ev := func(day, q string) entry.Evidence { return entry.Evidence{Who: entry.WhoSelf, Day: day, Quote: q} }
	one := &entry.Entry{Facet: entry.Interaction, Type: "correction", Status: entry.Candidate, Evidence: []entry.Evidence{ev("2026-11-20", "please answer in Chinese")}}
	twice := &entry.Entry{Facet: entry.Interaction, Type: "correction", Status: entry.Candidate,
		Evidence: []entry.Evidence{ev("2026-11-20", "please answer in Chinese"), ev("2026-11-23", "answer in Chinese")}}
	ban := &entry.Entry{Facet: entry.Interaction, Type: "correction", Status: entry.Candidate, Evidence: []entry.Evidence{ev("2026-11-20", "以后一律用中文回我")}}
	pinned := &entry.Entry{Facet: entry.Interaction, Type: "correction", Status: entry.Candidate, Pinned: true, Evidence: []entry.Evidence{ev("2026-11-20", "answer in Chinese")}}
	if entry.Hard(one) || !entry.Hard(twice) || entry.Hard(ban) || !entry.Hard(pinned) {
		t.Fatalf("hard: one %v twice %v ban %v pinned %v", entry.Hard(one), entry.Hard(twice), entry.Hard(ban), entry.Hard(pinned))
	}
	if !export.Stable(twice) || export.Stable(one) || export.Stable(ban) {
		t.Error("multi-day hard rule stands at once; single-day candidate (even with ban cue) does not")
	}
	if entry.Score(twice) <= entry.Score(one) {
		t.Errorf("scores %d <= %d", entry.Score(twice), entry.Score(one))
	}
	far := &entry.Entry{Facet: entry.Interaction, Type: "correction", Evidence: []entry.Evidence{ev("2026-10-01", "a"), ev("2026-11-20", "b")}}
	if entry.Hard(far) {
		t.Error("two days seven weeks apart")
	}
}
