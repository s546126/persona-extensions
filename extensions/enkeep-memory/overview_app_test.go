package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/overview"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/profile"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
	"github.com/nwverse/persona/sdk/go/wire"
)

// overviewApp is v4App with the overview's state open and one used pattern resting on a seeded entry.
func overviewApp(t *testing.T) (*app, string, string) {
	t.Helper()
	a := v4App(t)
	ids := seed(a)
	a.openOverview(a.c.St.Dir)
	if a.ov == nil {
		t.Fatal("overview not opened")
	}
	var ev string
	for _, id := range ids {
		if e := a.c.St.S.Entries[id]; e != nil && e.Live() && len(e.Evidence) > 0 && e.Facet != entry.People {
			ev = id
			break
		}
	}
	pid := "ov:0123456789"
	err := a.ov.Update(func(s *overview.State) (bool, error) {
		s.Patterns[pid] = &overview.Pattern{ID: pid, Facet: "judgement", Lifecycle: overview.Core, Text: "试点后再全量",
			EvidenceIDs: []string{ev}, ScenesCount: 2, SourcesCount: 3, ActiveWeeks: []string{"2026-W46", "2026-W47"}, Created: "x"}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, pid, ev
}

// The overview's patterns reach memory.entries beside the curated entries; the subpage and a report page render and
// pass the hub's view check.
func TestOverviewExitAndPages(t *testing.T) {
	a, pid, _ := overviewApp(t)
	answered := func() bool {
		res, _, err := a.memoryPage(memsrc.Params{Contribution: memorySourceID, Limit: 500})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range res.Entries {
			if strings.HasPrefix(e.ID, "ov:pattern:") && strings.Contains(e.Text, "试点后再全量") && e.Standing {
				return true
			}
		}
		return false
	}
	hook := a.c.OnPublish // a snapshot to read, before the light refresh is hooked
	a.c.OnPublish = nil
	a.c.St.Lock()
	a.c.Publish()
	a.c.St.Unlock()
	a.c.OnPublish = hook
	if !answered() {
		t.Fatal("a used pattern is not answered by memory.entries")
	}
	// The light refresh on the next publish recounts the pattern from the seeded entry: one entry cannot pass the gate,
	// so it is withdrawn at once and no longer answered.
	a.c.St.Lock()
	a.c.Publish()
	a.c.St.Unlock()
	if p := a.ov.Snapshot().Patterns[pid]; p.Used() || p.Withdrawn != overview.Core {
		t.Fatalf("a pattern resting on one entry stayed used: %+v", p)
	}
	if answered() {
		t.Fatal("a withdrawn pattern is still answered")
	}
	_, v := render(t, a, page{Tab: tabStart}, "zh-Hans")
	if !strings.Contains(v, "定期总览") {
		t.Fatal("no overview row on the start tab")
	}
	_, v = render(t, a, page{Tab: tabStart, Open: "overview"}, "zh-Hans")
	if !strings.Contains(v, "试点后再全量") || !strings.Contains(v, "观察中") {
		t.Fatalf("the subpage: %s", v)
	}
	if err := overview.SaveReportIn(a.c.St.Dir, "monthly", "2026-11", strings.Repeat("一行很长的月报内容\n", 600)); err != nil {
		t.Fatal(err)
	}
	vv, _ := render(t, a, page{Tab: tabStart, Open: "ovr:monthly:2026-11"}, "zh-Hans")
	if len(vv.Sections) == 0 || len(vv.Sections[0].Items) < 2 {
		t.Fatal("a long report is not cut into parts")
	}
	_, v = render(t, a, page{Tab: tabStart, Open: "ovr:weekly:../../state"}, "zh-Hans")
	if !strings.Contains(v, "不见了") {
		t.Fatal("a bad period read something")
	}
}

// The tools: the status reads the overview; the run checks its arguments and describes itself before the user's gate.
func TestOverviewTools(t *testing.T) {
	a, _, _ := overviewApp(t)
	text, err := call(t, a, "enkeep_overview_status", map[string]any{})
	if err != nil || !strings.Contains(text, "Overview: 1 pattern(s)") || !strings.Contains(text, "memory.entries:") {
		t.Fatalf("%q %v", text, err)
	}
	for _, bad := range []map[string]any{{"kind": "weekly", "period": "2026-11"}, {"kind": "monthly", "period": "2026-W47"},
		{"kind": "nope"}, {"kind": "report", "period": "x"}} {
		if _, _, err := overviewArgs(mustJSON(bad)); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	for args, want := range map[string]string{
		`{"kind":"weekly","period":"2026-W47"}`:   "周级跨事总览",
		`{"kind":"monthly","period":"2026-11"}`:   "月度巩固",
		`{"kind":"backtest","period":"2026-W47"}`: "预测",
		`{"kind":"audit"}`:                        "盲判",
		`{"kind":"report","period":"2026-W47"}`:   "重新推送",
	} {
		d, err := a.describe("enkeep_overview_run", []byte(args))
		if err != nil || !strings.Contains(d, want) {
			t.Errorf("%s: %q %v", args, d, err)
		}
	}
	// report sends the week's summary again, short, even when the ledger says it failed.
	h := &notifyHub{}
	a.hubAPI = h
	if err := saveLedger(a.c.St.Dir, &weeklyLedger{Week: "2026-W47", Err: "hub 400 bad_request: body is too long"}); err != nil {
		t.Fatal(err)
	}
	text, err = call(t, a, "enkeep_overview_run", map[string]any{"kind": "report", "period": "2026-W47"})
	if err != nil || len(h.sent) != 1 {
		t.Fatalf("%q %v %d", text, err, len(h.sent))
	}
	if _, body, _ := strings.Cut(h.sent[0], "\n"); len([]rune(body)) > overview.SummaryMost {
		t.Fatalf("pushed %d characters", len([]rune(body)))
	}
	if led := loadLedger(a.c.St.Dir); !led.Pushed || led.Err != "" {
		t.Fatalf("ledger %+v", led)
	}
}

// The weekly report waits for the week's overview, then goes; a failed overview does not hold it.
func TestWeeklyReportWaitsForOverview(t *testing.T) {
	a, _, _ := overviewApp(t)
	h := &notifyHub{}
	a.hubAPI = h
	a.hubLoc = time.UTC
	a.c.St.S.Runs = append(a.c.St.S.Runs, weeklyRun("2026-W47"))
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	if err := a.weeklyTick(context.Background(), now); err != nil || loadLedger(a.c.St.Dir).Week != "" {
		t.Fatalf("did not wait: %v", err)
	}
	a.mu.Lock()
	a.tried = append(a.tried, kindOverview+"@2026-W47") // started: recorded as tried while it runs
	a.running = kindOverview + " 2026-W47"
	a.mu.Unlock()
	if err := a.weeklyTick(context.Background(), now); err != nil || loadLedger(a.c.St.Dir).Week != "" {
		t.Fatalf("did not wait for the running overview: %v", err)
	}
	a.mu.Lock()
	a.running = "" // it failed
	a.mu.Unlock()
	if err := a.weeklyTick(context.Background(), now); err != nil || loadLedger(a.c.St.Dir).Week != "2026-W47" {
		t.Fatalf("held by a failed overview: %v", err)
	}
	if overview.ReportIn(a.c.St.Dir, "weekly", "2026-W47") == "" {
		t.Fatal("full report not saved")
	}
}

type failingHub struct{ notifyHub }

func (h *failingHub) Notify(context.Context, string, string) error {
	return errors.New("hub 400 bad_request: body is too long")
}

// A push the hub refused is tried once more by a new process, never every minute.
func TestWeeklyReportRetriesOnce(t *testing.T) {
	a, pid, _ := overviewApp(t)
	// The week's overview moved a pattern: there is something to send whatever the curation's week holds.
	_ = a.ov.Update(func(s *overview.State) (bool, error) {
		s.Weeks["2026-W47"] = &overview.Run{Period: "2026-W47", Changed: []string{pid}}
		return true, nil
	})
	a.hubAPI = &failingHub{}
	a.hubLoc = time.UTC
	a.c.St.S.Runs = append(a.c.St.S.Runs, weeklyRun("2026-W47"))
	a.tried = append(a.tried, kindOverview+"@2026-W47")
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	_ = a.weeklyTick(context.Background(), now)
	if led := loadLedger(a.c.St.Dir); led.Pushed || led.Err == "" {
		t.Fatalf("%+v", led)
	}
	h := &notifyHub{}
	a.hubAPI = h
	_ = a.weeklyTick(context.Background(), now.Add(time.Minute))
	if len(h.sent) != 0 {
		t.Fatal("retried within the same process")
	}
	a.tried = []string{kindOverview + "@2026-W47"} // a new process
	if err := a.weeklyTick(context.Background(), now.Add(2*time.Minute)); err != nil || len(h.sent) != 1 {
		t.Fatalf("not retried once: %v %d", err, len(h.sent))
	}
	if led := loadLedger(a.c.St.Dir); !led.Pushed || led.Err != "" {
		t.Fatalf("%+v", led)
	}
	_ = a.weeklyTick(context.Background(), now.Add(3*time.Minute))
	if len(h.sent) != 1 {
		t.Fatal("sent twice")
	}
}

// With no month consolidated yet, the last complete month is caught up once; then only the first Monday of a month.
func TestMonthDue(t *testing.T) {
	a, _, _ := overviewApp(t)
	monday := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	if m := a.monthDue(monday, "2026-W47"); m != "" {
		t.Fatalf("before any week: %q", m)
	}
	_ = a.ov.Update(func(s *overview.State) (bool, error) { s.Weeks["2026-W47"] = &overview.Run{}; return true, nil })
	if m := a.monthDue(monday, "2026-W47"); m != "2026-10" {
		t.Fatalf("catch-up: %q", m)
	}
	_ = a.ov.Update(func(s *overview.State) (bool, error) { s.Months["2026-10"] = &overview.Run{}; return true, nil })
	if m := a.monthDue(monday, "2026-W47"); m != "" {
		t.Fatalf("again: %q", m)
	}
	if m := a.monthDue(time.Date(2026, 12, 7, 0, 0, 0, 0, time.UTC), "2026-W49"); m != "" {
		t.Fatalf("week not settled: %q", m)
	}
	_ = a.ov.Update(func(s *overview.State) (bool, error) { s.Weeks["2026-W49"] = &overview.Run{}; return true, nil })
	if m := a.monthDue(time.Date(2026, 12, 7, 0, 0, 0, 0, time.UTC), "2026-W49"); m != "2026-11" {
		t.Fatalf("first Monday: %q", m)
	}
}

// Claude only: a configured model of another family is not used for the overview.
func TestOverviewModels(t *testing.T) {
	a := v4App(t)
	a.c.Cfg.ModelLong, a.c.Cfg.ModelShort = "gpt-x", "gemini-y"
	r := a.overviewRunner()
	if r.Long != overviewLong || r.Short != overviewShort {
		t.Fatalf("%s %s", r.Long, r.Short)
	}
	a.c.Cfg.ModelLong = "claude-opus-5-5"
	if r := a.overviewRunner(); r.Long != "claude-opus-5-5" {
		t.Fatal(r.Long)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func weeklyRun(week string) *store.Run {
	return &store.Run{ID: "w-" + week, Kind: "weekly", Period: week, Finished: "x"}
}

// The profile page on a phone: a facet's sources are short one-line titles, at most four; a pattern is marked and opens
// the periodic overview, not an entry page that does not exist.
func TestProfilePagePatternPoints(t *testing.T) {
	a := v4App(t)
	long := strings.Repeat("在改动现有系统之前先备好变更、验证和回滚办法", 4)
	sum := &profile.Summary{Version: profile.CurrentVersion, CoveredCount: 1, SourceCount: 6, Sections: []profile.Section{{
		Facet: entry.Workflow, Summary: "改动前先备好回滚。", SourceCount: 6,
		Points: []profile.Point{{ID: "ov:0123456789", Text: long, Last: "2026-11-23", Pattern: true},
			{ID: "w-1", Text: "b"}, {ID: "w-2", Text: "c"}, {ID: "w-3", Text: "d"}, {ID: "w-4", Text: "e"}},
	}}}
	st, done := a.read()
	v := a.profilePage(trOf("zh-Hans"), st, sum)
	done()
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	var rows []wire.Component
	for _, sec := range v.Sections {
		for _, it := range sec.Items {
			if it.Type == wire.ComponentRow && it.Action != nil && strings.HasPrefix(it.Action.ID, "open:") && !strings.HasPrefix(it.Action.ID, "open:facet:") {
				rows = append(rows, it)
			}
		}
	}
	if len(rows) != 4 {
		t.Fatalf("%d source rows", len(rows))
	}
	p := rows[0]
	if len([]rune(p.Title)) > 26 || !strings.HasSuffix(p.Title, "…") || !strings.Contains(p.Subtitle, "跨事模式") ||
		!strings.Contains(string(mustJSON(p.Action)), "overview") {
		t.Fatalf("%+v %+v", p, p.Action)
	}
}

func TestReportPageCheckViewFallbackAndCap(t *testing.T) {
	a, _, _ := overviewApp(t)
	// Report with disallowed Markdown (http:// link and image) must fall back cleanly and pass CheckView.
	disallowed := "# Weekly Report\n\nSee [local server](http://localhost:8080/status) and ![chart](https://example.com/chart.png).\n"
	if err := overview.SaveReportIn(a.c.St.Dir, "weekly", "2026-W12", disallowed); err != nil {
		t.Fatal(err)
	}
	v := a.reportPage(trOf("en"), "ovr:weekly:2026-W12")
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("reportPage with disallowed markdown failed CheckView: %v", err)
	}
	if len(v.Sections) == 0 || len(v.Sections[0].Items) == 0 || !strings.Contains(v.Sections[0].Items[0].Text, "http://localhost:8080/status") {
		t.Fatalf("reportPage lost content during fallback: %+v", v)
	}

	// Report whose raw chunk count exceeds CheckView's 300-component limit must be capped and pass CheckView.
	huge := strings.Repeat(strings.Repeat("a", 1790)+"\n", 320)
	if err := overview.SaveReportIn(a.c.St.Dir, "monthly", "2026-12", huge); err != nil {
		t.Fatal(err)
	}
	vh := a.reportPage(trOf("en"), "ovr:monthly:2026-12")
	if err := wire.CheckView(vh); err != nil {
		t.Fatalf("huge reportPage failed CheckView: %v", err)
	}
	last := vh.Sections[0].Items[len(vh.Sections[0].Items)-1]
	if !strings.Contains(last.Text, "omitted") {
		t.Fatalf("huge reportPage missing truncation note: %+v", last)
	}

	// Overlong period parameter in open state must be truncated so v.Title stays within CheckView's 2000-char limit.
	vLongTitle := a.reportPage(trOf("en"), "ovr:weekly:"+strings.Repeat("x", 2005))
	if err := wire.CheckView(vLongTitle); err != nil {
		t.Fatalf("reportPage with overlong period failed CheckView: %v", err)
	}
}
