package main

// The periodic cross-event overview in the app (internal/overview): when its passes run, the light refresh on every
// published snapshot, its entries in memory.entries, and the operations the tools start.
//
//   - weekly (L1): after the week's curation pass, before the weekly report, which waits for it;
//   - monthly (L2): on the first Monday of a month, after that week's overview, for the month before;
//   - L3: every snapshot the curator publishes (after a run, a mark, a delete, a forgotten source);
//   - a backtest (patterns built only from what came before a week, checked against the week's real choices) and a
//     blind spot check, when an operator asks for them (enkeep_overview_run).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/curate"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/overview"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/profile"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/signals"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// Kinds of the overview's passes (app.start).
const (
	kindOverview      = "overview"
	kindOverviewMonth = "overview-month"
	kindBacktest      = "overview-backtest"
	kindAudit         = "overview-audit"
)

// The models the overview names: Claude only (design 2.6). A configured long or short model of another family is not
// used for the overview; these are.
const (
	overviewLong  = "claude-sonnet-5-5"
	overviewShort = "claude-haiku-5-5"
)

func claudeOr(m, def string) string {
	if strings.HasPrefix(strings.TrimSpace(m), "claude-") {
		return strings.TrimSpace(m)
	}
	return def
}

// openOverview opens the overview's state beside the store and hooks the light refresh on the curator's snapshots. A
// file that cannot be read leaves the overview off (logged) rather than the extension down; the file is not touched.
func (a *app) openOverview(dir string) {
	h, err := overview.Open(dir)
	if err != nil {
		log.Printf("enkeep-memory: overview state not read, overview off until it is fixed: %v", err)
		return
	}
	a.ov = h
	a.c.OnPublish = a.overviewRefresh
	a.c.ProfileInput = overview.OverviewBackedProvider{Snapshot: h.Snapshot}
}

// overviewRefresh is L3 on a published snapshot.
func (a *app) overviewRefresh(st *store.State) {
	if a.readOnly || a.ov == nil || st == nil {
		return
	}
	err := a.ov.Update(func(s *overview.State) (bool, error) { return overview.Refresh(st, s, time.Now()), nil })
	if err != nil {
		log.Printf("enkeep-memory: overview refresh: %v", err)
	}
}

func (a *app) overviewRunner() *overview.Runner {
	lang := a.c.Cfg.Language
	if lang == "" {
		lang = a.c.Lang
	}
	return &overview.Runner{M: a.c.Model, Long: claudeOr(a.c.Cfg.ModelLong, overviewLong), Short: claudeOr(a.c.Cfg.ModelShort, overviewShort),
		Lang: lang, Now: a.c.Now, Logf: a.logf}
}

// overviewExtras are the used patterns for memory.entries.
func (a *app) overviewExtras() []memsrc.Extra {
	if a.ov == nil {
		return nil
	}
	return overview.Exits(a.ov.Snapshot())
}

// snapshotState is the last published state, read-only; an error before the first.
func (a *app) snapshotState() (*store.State, error) {
	snap := a.c.Live.Snapshot()
	if snap == nil || snap.State == nil {
		return nil, errors.New("no snapshot of the state yet")
	}
	return snap.State, nil
}

// overviewDue says whether the week's overview is still to run (and was not tried since this process started).
func (a *app) overviewDue(week string) bool {
	return a.ov != nil && a.ov.Snapshot().Weeks[week] == nil && !a.attempted(kindOverview+"@"+week)
}

// overviewSettled: the week's overview ran, or was tried and is no longer running (it failed), or is off: the weekly
// report need not wait. A try is recorded when the pass starts, so a running pass is not settled.
func (a *app) overviewSettled(week string) bool {
	if a.ov == nil || a.ov.Snapshot().Weeks[week] != nil {
		return true
	}
	a.mu.Lock()
	running := a.running == kindOverview+" "+week
	a.mu.Unlock()
	return !running && a.attempted(kindOverview+"@"+week)
}

// monthDue is the month to consolidate on the week starting monday, once the week's overview is settled: on the
// first Monday of a month, the month before; and, when no month was ever consolidated (a first install, an upgrade),
// the last complete month once, as the passes catch up their most recent missed run. "" otherwise.
func (a *app) monthDue(monday time.Time, lastWeek string) string {
	if a.ov == nil || !a.overviewSettled(lastWeek) {
		return ""
	}
	s := a.ov.Snapshot()
	if monday.Day() > 7 && (len(s.Months) > 0 || len(s.Weeks) == 0) {
		return ""
	}
	month := monday.AddDate(0, 0, -monday.Day()).Format("2006-01") // the last complete month
	if s.Months[month] != nil || a.attempted(kindOverviewMonth+"@"+month) {
		return ""
	}
	return month
}

// runOverview runs one overview pass of kind for period on the last snapshot.
func (a *app) runOverview(ctx context.Context, kind, period string) error {
	if a.ov == nil {
		return errors.New("the overview is off (its state could not be read)")
	}
	st, err := a.snapshotState()
	if err != nil {
		return err
	}
	r := a.overviewRunner()
	switch kind {
	case kindOverview:
		return a.overviewWeek(ctx, r, st, period)
	case kindOverviewMonth:
		return a.overviewMonth(ctx, r, st, period)
	case kindBacktest:
		return a.overviewBacktest(ctx, r, st, period)
	case kindAudit:
		au, err := r.Audit(ctx, st, a.ov.Snapshot())
		if au != nil {
			if uerr := a.ov.Update(func(s *overview.State) (bool, error) {
				s.Audits = append(s.Audits, *au)
				return true, nil
			}); uerr != nil && err == nil {
				err = uerr
			}
		}
		return err
	}
	return fmt.Errorf("unknown overview pass %q", kind)
}

// commit writes a pass's result and settles it against the newest snapshot at once (marks made while it ran).
func (a *app) commit(ctx context.Context, n *overview.State) error {
	err := a.ov.Update(func(s *overview.State) (bool, error) {
		*s = *n
		return true, nil
	})
	if err != nil {
		return err
	}
	if st, err := a.snapshotState(); err == nil {
		a.overviewRefresh(st)
		a.overviewProfile(ctx, st)
	}
	return nil
}

// overviewProfile writes the profile summary again from the new patterns when its basis changed, with the same one
// synthesis call the daily pass makes (a deterministic summary strings the patterns together; it is kept only when the
// model fails). It reads the snapshot, never the curated state.
func (a *app) overviewProfile(ctx context.Context, st *store.State) {
	if prev, err := profile.Load(a.c.St.Dir); err != nil || prev == nil {
		return
	}
	lang := a.c.Cfg.Language
	if lang == "" {
		lang = a.c.Lang
	}
	o := profile.Options{Provider: a.c.ProfileInput, Model: a.c.Model, Tier: model.Small, Lang: lang, Now: a.c.Now, Logf: a.logf}
	if _, err := profile.Refresh(ctx, st, a.c.St.Dir, o); err != nil {
		a.logf("enkeep-memory: overview: profile summary: %v", err)
	}
}

// overviewWeek: the forward check of the patterns kept so far against the week, then L1. With no pattern used yet and
// no forward check ever (a first run on a history) the forward check is a backtest, once: patterns built from what came
// before the week only.
func (a *app) overviewWeek(ctx context.Context, r *overview.Runner, st *store.State, week string) error {
	n := a.ov.Snapshot().Clone()
	used := slices.ContainsFunc(n.Sorted(nil), func(p *overview.Pattern) bool { return p.Used() })
	if !used && len(n.Holdouts) == 0 {
		if err := a.overviewBacktest(ctx, r, st, week); err != nil {
			a.logf("enkeep-memory: overview %s: backtest: %v", week, err)
		}
		n = a.ov.Snapshot().Clone() // with the backtest's reading
	} else if used {
		h, err := r.Holdout(ctx, st, n, week, false)
		if err != nil {
			a.logf("enkeep-memory: overview %s: forward check: %v", week, err)
		}
		if h != nil && h.N > 0 {
			n.Holdouts = append(n.Holdouts, *h)
		}
	}
	run, err := r.Weekly(ctx, st, n, week, "")
	if run == nil {
		return err
	}
	if err != nil {
		run.Error = err.Error()
	}
	n.Weeks[week] = run
	if cerr := a.commit(ctx, n); cerr != nil {
		return cerr
	}
	a.logf("enkeep-memory: overview %s: %d call(s), %d pattern(s) made or moved, %v", week, run.Calls, len(run.Changed), run.Counts)
	return err
}

func (a *app) overviewMonth(ctx context.Context, r *overview.Runner, st *store.State, month string) error {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return fmt.Errorf("month %q: want YYYY-MM", month)
	}
	end := t.AddDate(0, 1, 0).Format("2006-01-02")
	n := a.ov.Snapshot().Clone()
	run, err := r.Monthly(ctx, st, n, month, end)
	if run == nil {
		return err
	}
	if err != nil {
		run.Error = err.Error()
	}
	n.Months[month] = run
	if cerr := a.commit(ctx, n); cerr != nil {
		return cerr
	}
	lang := a.c.Cfg.Language
	if lang == "" {
		lang = a.c.Lang
	}
	title := "# enkeep memory · " + month + " consolidation\n\n"
	if overview.Chinese(lang) {
		title = "# enkeep 记忆 · " + month + " 月度巩固\n\n"
	}
	// The month's spot check: a model's blind verdicts on a sample of the used patterns, for the user to go over.
	if au, aerr := r.Audit(ctx, a.snapshotOr(st), a.ov.Snapshot()); aerr != nil {
		a.logf("enkeep-memory: spot check %s: %v", month, aerr)
	} else if len(au.Items) > 0 {
		if uerr := a.ov.Update(func(s *overview.State) (bool, error) {
			s.Audits = append(s.Audits, *au)
			return true, nil
		}); uerr != nil {
			a.logf("enkeep-memory: spot check %s: %v", month, uerr)
		}
	}
	full := title + overview.Section(a.ov.Snapshot(), run, lang)
	if serr := a.ov.SaveReport("monthly", month, full); serr != nil {
		a.logf("enkeep-memory: monthly report %s: %v", month, serr)
	}
	a.logf("enkeep-memory: overview month %s: %d call(s), %v", month, run.Calls, run.Counts)
	return err
}

// overviewBacktest builds the patterns from what came before the week only, in a scratch state (the kept patterns are
// not touched), and checks them against the week's real choices: the forward check without waiting a week. The
// scratch patterns are saved apart (overview-backtest.json) for review.
func (a *app) overviewBacktest(ctx context.Context, r *overview.Runner, st *store.State, week string) error {
	days, err := curate.WeekDays(week)
	if err != nil {
		return err
	}
	scratch := (&overview.State{}).Clone()
	prev := curate.ISOWeek(addDay(days[0], -1))
	run, err := r.Weekly(ctx, st, scratch, prev, days[0])
	if err != nil {
		return err
	}
	scratch.Weeks[prev] = run
	h, err := r.Holdout(ctx, st, scratch, week, true)
	if h != nil {
		h.Calls += run.Calls
		h.Notes = append(h.Notes, fmt.Sprintf("backtest: %d pattern(s) built from evidence before %s, %d used", len(scratch.Patterns), days[0],
			len(scratch.Sorted(func(p *overview.Pattern) bool { return p.Used() }))))
		scratch.Holdouts = append(scratch.Holdouts, *h)
		if uerr := a.ov.Update(func(s *overview.State) (bool, error) {
			s.Holdouts = append(s.Holdouts, *h)
			return true, nil
		}); uerr != nil && err == nil {
			err = uerr
		}
	}
	if serr := overview.SaveScratch(a.ov.Dir(), "overview-backtest.json", scratch); serr != nil && err == nil {
		err = serr
	}
	return err
}

// snapshotOr is the newest snapshot, or st when there is none.
func (a *app) snapshotOr(st *store.State) *store.State {
	if n, err := a.snapshotState(); err == nil {
		return n
	}
	return st
}

func addDay(day string, n int) string {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

// overviewStatus is enkeep_overview_status's text without a violation window.
func (a *app) overviewStatus() string {
	return a.overviewStatusRange("", "", "")
}

// overviewStatusRange is enkeep_overview_status's text; when from, to, or query is set, it appends per-contract
// violation recurrences and rates per 100 ask/decision cards for [from, to].
func (a *app) overviewStatusRange(from, to, query string) string {
	if a.ov == nil {
		return "The overview is off: its state could not be read (see the hub's log)."
	}
	s := a.ov.Snapshot()
	var b strings.Builder
	covered, by := overview.Coverage(s)
	fmt.Fprintf(&b, "Overview: %d pattern(s); %d of 6 facets with a used pattern.", len(s.Patterns), covered)
	for _, f := range overview.Facets {
		m := by[string(f)]
		fmt.Fprintf(&b, "\n- %s: core_stable %d, scoped %d, candidate %d, drifting %d, era_closed %d, retired %d", f, m[overview.Core],
			m[overview.Scoped], m[overview.Candidate], m[overview.Drifting], m[overview.EraClosed], m[overview.Retired])
	}
	for _, kind := range []string{"weeks", "months"} {
		runs := s.Weeks
		if kind == "months" {
			runs = s.Months
		}
		var keys []string
		for k := range runs {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys[max(0, len(keys)-3):] {
			r := runs[k]
			fmt.Fprintf(&b, "\nRun %s at %s: %d call(s), %d made or moved, counts %v", k, r.At, r.Calls, len(r.Changed), r.Counts)
			if r.Error != "" {
				fmt.Fprintf(&b, ", error: %s", r.Error)
			}
		}
	}
	for _, h := range s.Holdouts[max(0, len(s.Holdouts)-3):] {
		fmt.Fprintf(&b, "\nForward check %s (backtest %t) at %s: %d question(s), overview %d (%.0f%%, %d chars, %d pattern(s), %d excluded), scatter %d (%.0f%%, %d chars), none %d (%.0f%%); %d call(s)",
			h.Week, h.Backtest, h.At, h.N, h.Hits[overview.ArmOverview], h.Rate[overview.ArmOverview]*100, h.Chars[overview.ArmOverview], h.Patterns, h.Excluded,
			h.Hits[overview.ArmScatter], h.Rate[overview.ArmScatter]*100, h.Chars[overview.ArmScatter], h.Hits[overview.ArmNone], h.Rate[overview.ArmNone]*100, h.Calls)
		for _, n := range h.Notes {
			fmt.Fprintf(&b, "; %s", n)
		}
	}
	if n := len(s.Audits); n > 0 {
		au := s.Audits[n-1]
		fmt.Fprintf(&b, "\nSpot check at %s: %d pattern(s), verdicts %v (a model's blind verdicts, for the user to go over)", au.At, len(au.Items), au.Summary)
	}
	if s.Refreshed != "" {
		fmt.Fprintf(&b, "\nLast light refresh that changed anything: %s; %d evidence entr(ies) withdrawn.", s.Refreshed, len(s.Dead))
	}
	led := loadLedger(a.c.St.Dir)
	fmt.Fprintf(&b, "\nWeekly report: week %s pushed %t", led.Week, led.Pushed)
	if led.Err != "" {
		fmt.Fprintf(&b, " (%s)", led.Err)
	}
	b.WriteString(".")
	ex := overview.Exits(s)
	standing := 0
	for _, x := range ex {
		if x.Entry.Standing {
			standing++
		}
	}
	fmt.Fprintf(&b, "\nmemory.entries: %d overview entr(ies), %d standing.", len(ex), standing)
	if from != "" || to != "" || query != "" {
		cards, asks, days, _ := signals.AsksInRange(a.c.St.Dir, from, to)
		st, _ := a.snapshotState()
		stats := overview.ContractViolations(st, s, from, to, cards)
		q := strings.ToLower(query)
		fmt.Fprintf(&b, "\nContract violations (%s..%s, %d day(s) in signals, %d ask/decision card(s), %d total ask(s)):", from, to, days, cards, asks)
		shown := 0
		for _, v := range stats {
			if q != "" && !strings.Contains(strings.ToLower(v.ID+" "+v.Text), q) {
				continue
			}
			if q == "" && v.Recurrences == 0 {
				continue
			}
			shown++
			if cards > 0 {
				fmt.Fprintf(&b, "\n- [%s] (%s, pol=%s, hard=%t) recurrences=%d (%.2f/100 cards, days=%v, all_time=%d): %s",
					v.ID, v.Kind, v.Polarity, v.Hard, v.Recurrences, v.Per100Cards, v.Days, v.TotalAllTime, v.Text)
			} else {
				fmt.Fprintf(&b, "\n- [%s] (%s, pol=%s, hard=%t) recurrences=%d (cards=0, days=%v, all_time=%d): %s",
					v.ID, v.Kind, v.Polarity, v.Hard, v.Recurrences, v.Days, v.TotalAllTime, v.Text)
			}
		}
		if shown == 0 {
			fmt.Fprintf(&b, "\n- (no matching contract recurrences in window; %d active non-sensitive contract(s) checked)", len(stats))
		}
	}
	return b.String()
}
