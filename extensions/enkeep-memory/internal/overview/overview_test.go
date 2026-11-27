package overview

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/profile"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// Synthetic fixtures only: made-up projects, sources and quotes.

var now = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// action makes an action entry with one quoted line.
func action(id string, f entry.Facet, project, src, day, text string) *entry.Entry {
	e := &entry.Entry{ID: id, Facet: f, Grain: entry.Action, Text: text, Situation: "s-" + text, Choice: "c-" + text,
		Status: entry.Stable, Origin: entry.Behavior, Tagged: true, First: day, Last: day, Count: 1,
		Evidence: []entry.Evidence{{Source: src, Ref: "unit/" + id, Day: day, Quote: "quote " + text}}}
	if project != "" {
		e.Scopes = []entry.Scope{{Kind: entry.ScopeProject, Name: project}}
	}
	return e
}

// class makes a class over actions.
func class(id string, f entry.Facet, text string, members ...*entry.Entry) *entry.Entry {
	c := &entry.Entry{ID: id, Facet: f, Grain: entry.Class, Text: text, Situation: "situation " + text, Choice: "choice " + text,
		Status: entry.Stable, Origin: entry.Behavior, Tagged: true, Count: len(members)}
	for _, m := range members {
		c.Members = append(c.Members, m.ID)
		m.Parent = c.ID
		if c.First == "" || m.First < c.First {
			c.First = m.First
		}
		if m.Last > c.Last {
			c.Last = m.Last
		}
	}
	return c
}

func state(es ...*entry.Entry) *store.State {
	st := &store.State{Entries: map[string]*entry.Entry{}}
	for _, e := range es {
		st.Entries[e.ID] = e
	}
	return st
}

// fixture: a judgement made the same way in two projects, through chats and cards, over two weeks (a pattern); a
// one-off convention of one project, from one chat (not a pattern); and a health-tagged way across two projects.
func fixture() *store.State {
	a1 := action("a1", entry.Judgement, "alpha", entry.SrcAssistant, "2026-11-09", "pilot first")
	a2 := action("a2", entry.Judgement, "alpha", entry.SrcAsk, "2026-11-11", "pilot again")
	a3 := action("a3", entry.Judgement, "beta", entry.SrcAgent, "2026-11-17", "small sample before rollout")
	a4 := action("a4", entry.Judgement, "beta", entry.SrcAssistant, "2026-11-19", "sample first")
	b1 := action("b1", entry.Judgement, "gamma", entry.SrcAssistant, "2026-11-18", "tabs not spaces")
	b2 := action("b2", entry.Judgement, "gamma", entry.SrcAssistant, "2026-11-18", "tabs in gamma")
	h1 := action("h1", entry.Persona, "alpha", entry.SrcAssistant, "2026-11-10", "rest after deploys")
	h2 := action("h2", entry.Persona, "beta", entry.SrcAsk, "2026-11-12", "rest days")
	h3 := action("h3", entry.Persona, "beta", entry.SrcAgent, "2026-11-18", "rest again")
	for _, h := range []*entry.Entry{h1, h2, h3} {
		h.Categories = []entry.Category{{Name: entry.CatHealth, By: entry.ByModel}}
	}
	x1 := action("x1", entry.Persona, "", entry.SrcAssistant, "2026-11-18", "filler one")
	x2 := action("x2", entry.Persona, "", entry.SrcAssistant, "2026-11-18", "filler two")
	ca := class("ca", entry.Judgement, "pilot before rollout", a1, a2)
	cb := class("cb", entry.Judgement, "small sample first", a3, a4)
	cc := class("cc", entry.Judgement, "gamma indent", b1, b2)
	cd := class("cd", entry.Judgement, "tabs everywhere in gamma")
	cd.First, cd.Last, cd.Evidence = "2026-11-18", "2026-11-18", []entry.Evidence{{Source: entry.SrcAssistant, Ref: "unit/b1", Day: "2026-11-18", Quote: "q"}}
	ch := class("ch", entry.Persona, "rest", h1, h2, h3)
	return state(a1, a2, a3, a4, b1, b2, h1, h2, h3, x1, x2, ca, cb, cc, cd, ch)
}

// fakeModel answers the overview's calls from rules over its input.
type fakeModel struct {
	calls   map[string]int
	models  [][]string
	counter func(in counterInput) counterOutput
	predict func(in predictInput) predictOutput
}

func (m *fakeModel) Complete(_ context.Context, r model.Request) (model.Response, error) {
	if m.calls == nil {
		m.calls = map[string]int{}
	}
	m.calls[r.Step]++
	m.models = append(m.models, r.Models)
	var out any
	switch r.Step {
	case "overview_synth":
		var in synthInput
		_ = json.Unmarshal([]byte(r.User), &in)
		var o synthOutput
		pick := func(words ...string) []string {
			var ids []string
			for _, it := range in.Items {
				for _, w := range words {
					if strings.Contains(it.Text, w) {
						ids = append(ids, it.ID)
						break
					}
				}
			}
			return ids
		}
		if ids := pick("pilot", "sample"); len(ids) > 0 {
			o.Patterns = append(o.Patterns, synthProposal{Text: "tries a small pilot before any rollout", Situation: "rolling out a change",
				Choice: "pilot first", Rejected: "full rollout", DefaultContrast: "roll out once tests pass", Members: ids, Reason: "same choice"})
		}
		if ids := pick("gamma"); len(ids) > 0 {
			o.Patterns = append(o.Patterns, synthProposal{Text: "uses tabs", Situation: "indenting", Choice: "tabs",
				DefaultContrast: "spaces", Members: ids, Reason: "one project"})
		}
		if ids := pick("rest"); len(ids) > 0 {
			o.Patterns = append(o.Patterns, synthProposal{Text: "rests after heavy days", Situation: "after a deploy", Choice: "rest",
				DefaultContrast: "keeps working", Members: ids, Reason: "same"})
		}
		out = o
	case "overview_counter":
		var in counterInput
		_ = json.Unmarshal([]byte(r.User), &in)
		if m.counter != nil {
			out = m.counter(in)
		} else {
			var o counterOutput
			for _, p := range in.Patterns {
				o.Verdicts = append(o.Verdicts, counterVerdict{ID: p.ID, Counters: []string{}, Reason: "none against"})
			}
			out = o
		}
	case "overview_predict":
		var in predictInput
		_ = json.Unmarshal([]byte(r.User), &in)
		out = m.predict(in)
	case "overview_merge":
		out = mergeOutput{Groups: []mergeGroup{}}
	case "overview_profile_core":
		out = profileCoreOutput{}
	case "overview_reconcile":
		out = reconcileOutput{}
	case "overview_audit":
		var in auditInput
		_ = json.Unmarshal([]byte(r.User), &in)
		var o auditOutput
		for _, it := range in.Items {
			o.Items = append(o.Items, struct {
				ID      string `json:"id"`
				Verdict string `json:"verdict"`
				Reason  string `json:"reason"`
			}{it.ID, VerdictRight, fmt.Sprint(len(it.Quotes), " quotes")})
		}
		out = o
	default:
		return model.Response{}, fmt.Errorf("unexpected step %s", r.Step)
	}
	b, _ := json.Marshal(out)
	return model.Response{Text: string(b)}, nil
}

func runner(m model.Model) *Runner {
	return &Runner{M: m, Long: "claude-sonnet-5-5", Short: "claude-haiku-5-5", Lang: "en", Now: func() time.Time { return now }}
}

func byText(s *State, text string) *Pattern {
	for _, p := range s.Patterns {
		if p.Text == text {
			return p
		}
	}
	return nil
}

// A pattern across two projects, three independent sources and two weeks passes the gate and is used; a one-project
// convention seen on one day stays a candidate, whatever the model said.
func TestWeeklyGate(t *testing.T) {
	st := fixture()
	m := &fakeModel{}
	s := newState()
	run, err := runner(m).Weekly(context.Background(), st, s, "2026-W47", "")
	if err != nil {
		t.Fatal(err)
	}
	p := byText(s, "tries a small pilot before any rollout")
	if p == nil || p.Lifecycle != Core || p.Gate != "" || p.ScenesCount < 2 || p.SourcesCount < 3 || len(p.ActiveWeeks) < 2 {
		t.Fatalf("pattern %+v", p)
	}
	if p.Perspective != PerspCrossSource {
		t.Fatalf("perspective %s", p.Perspective)
	}
	one := byText(s, "uses tabs")
	if one == nil || one.Lifecycle != Candidate || one.Gate == "" {
		t.Fatalf("one-off %+v", one)
	}
	if run.Calls == 0 || m.calls["overview_synth"] == 0 {
		t.Fatalf("calls %v", m.calls)
	}
	// Claude only: every call named its models, none of another family.
	for _, ms := range m.models {
		if len(ms) == 0 {
			t.Fatal("a call without its own models")
		}
		for _, x := range ms {
			if !strings.HasPrefix(x, "claude-") {
				t.Fatalf("model %s", x)
			}
		}
	}
	// Run twice: the same pattern keeps its id.
	n := len(s.Patterns)
	if _, err := runner(m).Weekly(context.Background(), st, s, "2026-W47", ""); err != nil || len(s.Patterns) != n {
		t.Fatalf("second run: %d patterns, %v", len(s.Patterns), err)
	}
}

// A backtest's cutoff keeps the evidence of the week and after out.
func TestWeeklyCutoff(t *testing.T) {
	st := fixture()
	s := newState()
	if _, err := runner(&fakeModel{}).Weekly(context.Background(), st, s, "2026-W46", "2026-11-16"); err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Patterns {
		if p.LastEvidence >= "2026-11-16" {
			t.Fatalf("evidence after the cutoff: %+v", p)
		}
		if p.Used() {
			t.Fatalf("one week of evidence cannot pass: %+v", p)
		}
	}
}

// The counter-search: an exception bounds a pattern (scoped); recent counter-examples at a quarter or more make it
// drifting; overturned retires it.
func TestCounterSearch(t *testing.T) {
	st := fixture()
	// Two recent counter-examples in the same facet, alike in words.
	k1 := action("k1", entry.Judgement, "delta", entry.SrcAssistant, "2026-11-20", "pilot skipped full rollout")
	k2 := action("k2", entry.Judgement, "delta", entry.SrcAgent, "2026-11-21", "pilot skipped rollout directly")
	st.Entries["k1"], st.Entries["k2"] = k1, k2
	for name, tc := range map[string]struct {
		v    func(p counterPattern) counterVerdict
		want string
	}{
		"scoped": {func(p counterPattern) counterVerdict {
			return counterVerdict{ID: p.ID, Exception: "for a hotfix", Reason: "bounded"}
		}, Scoped},
		"drifting": {func(p counterPattern) counterVerdict {
			var ids []string
			for _, c := range p.Candidates {
				ids = append(ids, c.ID)
			}
			return counterVerdict{ID: p.ID, Counters: ids, Reason: "recently otherwise"}
		}, Drifting},
		"overturned": {func(p counterPattern) counterVerdict {
			return counterVerdict{ID: p.ID, Overturned: true, Reason: "wrong"}
		}, Retired},
	} {
		t.Run(name, func(t *testing.T) {
			m := &fakeModel{counter: func(in counterInput) counterOutput {
				var o counterOutput
				for _, p := range in.Patterns {
					o.Verdicts = append(o.Verdicts, tc.v(p))
				}
				return o
			}}
			s := newState()
			if _, err := runner(m).Weekly(context.Background(), st, s, "2026-W47", ""); err != nil {
				t.Fatal(err)
			}
			p := byText(s, "tries a small pilot before any rollout")
			if p == nil || p.Lifecycle != tc.want {
				t.Fatalf("%+v", p)
			}
			if tc.want == Drifting && (len(p.CounterIDs) == 0 || p.CounterRatio < driftRatio) {
				t.Fatalf("counters %+v", p)
			}
		})
	}
}

// An unanswered counter-search leaves the pattern a candidate: never used unchecked.
func TestCounterUnanswered(t *testing.T) {
	st := fixture()
	st.Entries["k1"] = action("k1", entry.Judgement, "delta", entry.SrcAssistant, "2026-11-20", "pilot skipped full rollout")
	m := &fakeModel{counter: func(counterInput) counterOutput { return counterOutput{} }}
	s := newState()
	if _, err := runner(m).Weekly(context.Background(), st, s, "2026-W47", ""); err != nil {
		t.Fatal(err)
	}
	if p := byText(s, "tries a small pilot before any rollout"); p == nil || p.Lifecycle != Candidate {
		t.Fatalf("%+v", p)
	}
}

// Privacy: a pattern resting on health-tagged evidence is answered as ov:cat:health:…, not standing, with its
// categories; a general one as ov:pattern:…, ranked between the scattered entries and the profile summary, with a
// score and a side for the standing set (memsrc decides whether it stands: never the maker, 0.2.0-beta.52).
func TestExitsPrivacy(t *testing.T) {
	st := fixture()
	s := newState()
	if _, err := runner(&fakeModel{}).Weekly(context.Background(), st, s, "2026-W47", ""); err != nil {
		t.Fatal(err)
	}
	ex := Exits(s)
	var general, health *memsrc.Extra
	for i := range ex {
		x := &ex[i]
		switch {
		case strings.HasPrefix(x.Entry.ID, "ov:pattern:"):
			general = x
		case strings.HasPrefix(x.Entry.ID, "ov:cat:health:"):
			health = x
		}
	}
	if general == nil || general.Entry.Standing || general.Entry.Rank < rankBottom || general.Entry.Rank > rankTop ||
		len(general.Entry.Categories) != 0 || general.Kind == "" || general.Score <= 0 {
		t.Fatalf("general %+v", general)
	}
	if health == nil || health.Entry.Standing || health.Kind != "" || health.Entry.Rank != rankCat ||
		!slices.Contains(health.Entry.Categories, entry.CatHealth) {
		t.Fatalf("health %+v", health)
	}
	// The report and the summary never quote it.
	run := &Run{Changed: []string{}}
	for id := range s.Patterns {
		run.Changed = append(run.Changed, id)
	}
	if sec := Section(s, run, "zh"); strings.Contains(sec, "rests after heavy days") {
		t.Fatal("a categorised pattern quoted in the report")
	}
	if sum := Summary(s, run, nil, "zh"); strings.Contains(sum, "rests after heavy days") {
		t.Fatal("a categorised pattern quoted in the summary")
	}
	// memory.entries answers them beside the curated entries, in order: the standing pattern before every scattered entry.
	res, n, err := memsrc.PageWith(st, nil, ex, memsrc.Params{Contribution: "memory", Limit: 500})
	if err != nil || n.Entries != len(ex)+countPool(st) {
		t.Fatalf("%v %+v", err, n)
	}
	if !strings.HasPrefix(res.Entries[0].ID, "ov:pattern:") {
		t.Fatalf("first %s", res.Entries[0].ID)
	}
}

func countPool(st *store.State) int {
	n := 0
	for _, e := range st.Entries {
		if memsrc.Pool(st, e) {
			n++
		}
	}
	return n
}

// L3: marking evidence wrong takes the pattern out of the exits at once; marking it right again brings it back; with
// nothing left it is retired. A snapshot that changes nothing writes nothing.
func TestRefresh(t *testing.T) {
	st := fixture()
	s := newState()
	if _, err := runner(&fakeModel{}).Weekly(context.Background(), st, s, "2026-W47", ""); err != nil {
		t.Fatal(err)
	}
	Refresh(st, s, now) // the first may record what it found; a second must be a no-op
	if Refresh(st, s, now) {
		t.Fatal("a refresh with nothing new changed something")
	}
	p := byText(s, "tries a small pilot before any rollout")
	st.Entries["cb"].Verdicts = append(st.Entries["cb"].Verdicts, entry.Verdict{Mark: "wrong", By: "user"})
	if !Refresh(st, s, now) || p.Used() || p.Withdrawn != Core {
		t.Fatalf("after wrong: %+v", p)
	}
	for _, x := range Exits(s) {
		if strings.HasSuffix(x.Entry.ID, strings.TrimPrefix(p.ID, "ov:")) {
			t.Fatal("withdrawn pattern still answered")
		}
	}
	st.Entries["cb"].Verdicts = append(st.Entries["cb"].Verdicts, entry.Verdict{Mark: "right", By: "user"})
	if !Refresh(st, s, now) || p.Lifecycle != Core {
		t.Fatalf("after right: %+v", p)
	}
	st.Entries["ca"].Deleted, st.Entries["cb"].Deleted = true, true
	Refresh(st, s, now)
	if p.Lifecycle != Retired {
		t.Fatalf("after delete: %+v", p)
	}
}

// The pushed summary stays within 300 characters however much there is, and names where the full text is.
func TestSummaryShort(t *testing.T) {
	s := newState()
	run := &Run{}
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("ov:%010d", i)
		s.Patterns[id] = &Pattern{ID: id, Facet: string(Facets[i%6]), Lifecycle: Core, Text: strings.Repeat("很长的模式描述", 20)}
		run.Changed = append(run.Changed, id)
	}
	sum := Summary(s, run, map[string]int{"effective": 201, "commitments_overdue": 33, "asks": 8}, "zh-Hans")
	if n := len([]rune(sum)); n > SummaryMost || n == 0 {
		t.Fatalf("%d characters", n)
	}
	if !strings.Contains(sum, "定期总览") || !strings.Contains(sum, "201") {
		t.Fatal(sum)
	}
	en := Summary(s, run, map[string]int{"effective": 201}, "en")
	if n := len([]rune(en)); n > SummaryMost || !strings.Contains(en, "Periodic overview") {
		t.Fatalf("%d %s", n, en)
	}
}

// L2: a project-bound pattern quiet for ninety days is era-closed; drifting two months running is retired.
func TestMonthly(t *testing.T) {
	st := fixture()
	s := newState()
	if _, err := runner(&fakeModel{}).Weekly(context.Background(), st, s, "2026-W47", ""); err != nil {
		t.Fatal(err)
	}
	p := byText(s, "tries a small pilot before any rollout")
	if p.Lifecycle != Core {
		t.Fatalf("%+v", p)
	}
	// Ninety days later, nothing new: the pattern rests only on project-scoped entries.
	r := runner(&fakeModel{})
	if _, err := r.Monthly(context.Background(), st, s, "2027-02", "2027-03-01"); err != nil {
		t.Fatal(err)
	}
	if p.Lifecycle != EraClosed {
		t.Fatalf("%+v", p)
	}
	// A pattern drifting in two monthly passes is retired.
	m := &fakeModel{counter: func(in counterInput) counterOutput {
		var o counterOutput
		for _, p := range in.Patterns {
			var ids []string
			for _, c := range p.Candidates {
				ids = append(ids, c.ID)
			}
			o.Verdicts = append(o.Verdicts, counterVerdict{ID: p.ID, Counters: ids, Reason: "otherwise"})
		}
		return o
	}}
	st.Entries["k1"] = action("k1", entry.Judgement, "delta", entry.SrcAssistant, "2026-11-25", "pilot skipped full rollout")
	st.Entries["k2"] = action("k2", entry.Judgement, "delta", entry.SrcAgent, "2026-11-26", "pilot skipped rollout directly")
	s2 := newState()
	if _, err := runner(m).Weekly(context.Background(), st, s2, "2026-W47", ""); err != nil {
		t.Fatal(err)
	}
	for i, month := range []string{"2026-11", "2026-12"} {
		if _, err := runner(m).Monthly(context.Background(), st, s2, month, "2026-12-01"); err != nil {
			t.Fatal(err)
		}
		q := byText(s2, "tries a small pilot before any rollout")
		want := Drifting
		if i == 1 {
			want = Retired
		}
		if q.Lifecycle != want {
			t.Fatalf("month %s: %+v", month, q)
		}
	}
}

// The forward check: a question's answer is scored by code; patterns resting on its entry or class are left out.
func TestHoldout(t *testing.T) {
	st := fixture()
	s := newState()
	if _, err := runner(&fakeModel{}).Weekly(context.Background(), st, s, "2026-W47", ""); err != nil {
		t.Fatal(err)
	}
	// Two real choices in W48, with what was turned down.
	q1 := action("q1", entry.Judgement, "alpha", entry.SrcAsk, "2026-11-24", "pilot the migration")
	q1.Situation, q1.Choice, q1.Rejected = "a migration", "pilot it first", "migrate at once"
	q2 := action("q2", entry.Workflow, "beta", entry.SrcAsk, "2026-11-25", "sample check")
	q2.Situation, q2.Choice, q2.Rejected = "a release", "check a sample", "release directly"
	st.Entries["q1"], st.Entries["q2"] = q1, q2
	// The fake answers right only when the memory mentions a pilot.
	m := &fakeModel{predict: func(in predictInput) predictOutput {
		var o predictOutput
		for _, q := range in.Questions {
			pick := "B"
			if strings.Contains(in.Memory, "pilot") {
				pick = "A"
				if !strings.Contains(q.A, "pilot") && !strings.Contains(q.A, "sample") {
					pick = "B"
				}
			}
			o.Answers = append(o.Answers, struct {
				ID   string `json:"id"`
				Pick string `json:"pick"`
			}{q.ID, pick})
		}
		return o
	}}
	h, err := runner(m).Holdout(context.Background(), st, s, "2026-W48", false)
	if err != nil {
		t.Fatal(err)
	}
	if h.N != 2 || h.Rate[ArmOverview] != 1 || h.Chars[ArmNone] != 0 || h.Patterns == 0 {
		t.Fatalf("%+v", h)
	}
	// A question filed under a class a pattern rests on: that pattern is left out.
	q1.Parent = "ca"
	h, _ = runner(m).Holdout(context.Background(), st, s, "2026-W48", false)
	if h.Excluded == 0 {
		t.Fatalf("leak not excluded: %+v", h)
	}
}

// The spot check samples used patterns across the facets and keeps the model's verdicts.
func TestAudit(t *testing.T) {
	st := fixture()
	s := newState()
	if _, err := runner(&fakeModel{}).Weekly(context.Background(), st, s, "2026-W47", ""); err != nil {
		t.Fatal(err)
	}
	a, err := runner(&fakeModel{}).Audit(context.Background(), st, s)
	if err != nil || len(a.Items) == 0 || a.Summary[VerdictRight] != len(a.Items) {
		t.Fatalf("%+v %v", a, err)
	}
	if !strings.Contains(a.Items[0].Reason, "quotes") || strings.HasPrefix(a.Items[0].Reason, "0 ") {
		t.Fatalf("no quotes given: %+v", a.Items[0])
	}
}

// The holder saves atomically and reads back; reports are kept by kind and period, and a period cannot escape.
func TestHolder(t *testing.T) {
	dir := t.TempDir()
	h, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Update(func(s *State) (bool, error) {
		s.Patterns["ov:1"] = &Pattern{ID: "ov:1", Facet: "tools", Lifecycle: Core}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	h2, err := Open(dir)
	if err != nil || h2.Snapshot().Patterns["ov:1"] == nil {
		t.Fatalf("%v", err)
	}
	if err := h.SaveReport("weekly", "2026-W47", "full"); err != nil || h.Report("weekly", "2026-W47") != "full" {
		t.Fatal(err)
	}
	if h.Report("weekly", "../state") != "" || h.Report("other", "2026-W47") != "" {
		t.Fatal("a path escaped")
	}
	if got := h.Reports("weekly"); len(got) != 1 || got[0] != "2026-W47" {
		t.Fatal(got)
	}
	if SaveScratch(dir, File, newState()) == nil || SaveScratch(dir, "../x.json", newState()) == nil {
		t.Fatal("scratch over the overview's file or outside")
	}
}

// The profile summary fed by the overview: a facet with a general used pattern shows the pattern instead of the
// scattered entries it was made from; a categorised pattern goes to its own sensitive section only; with no state the
// curated entries are used as before.
func TestOverviewBackedProvider(t *testing.T) {
	st := fixture()
	s := newState()
	if _, err := runner(&fakeModel{}).Weekly(context.Background(), st, s, "2026-W47", ""); err != nil {
		t.Fatal(err)
	}
	pilot := byText(s, "tries a small pilot before any rollout")
	if pilot == nil || !pilot.Used() {
		t.Fatalf("%+v", pilot)
	}
	prov := OverviewBackedProvider{Snapshot: func() *State { return s }}
	items := prov.Items(st)
	var judgement []string
	healthPattern := false
	for _, it := range items {
		if it.Facet == entry.Judgement && len(it.Categories) == 0 {
			judgement = append(judgement, it.ID)
		}
		if strings.HasPrefix(it.ID, "ov:") && slices.Contains(it.Categories, entry.CatHealth) {
			healthPattern = true
		}
	}
	if !slices.Contains(judgement, pilot.ID) || slices.Contains(judgement, "a1") || slices.Contains(judgement, "ca") {
		t.Fatalf("judgement items %v", judgement)
	}
	if !healthPattern {
		t.Fatal("the health pattern is not carried with its category")
	}
	sum := profile.BuildWith(st, prov, now, "zh-Hans")
	if sum.InputSource != "overview" {
		t.Fatal(sum.InputSource)
	}
	found := false
	for _, sec := range sum.Sections {
		for _, pt := range sec.Points {
			if strings.Contains(pt.Text, "rests after heavy days") {
				t.Fatal("a categorised pattern in a general section")
			}
			if sec.Facet == entry.Judgement && strings.Contains(pt.Text, "tries a small pilot") {
				found = true
			}
		}
	}
	if !found || strings.Contains(sum.Overview, "rests after heavy days") {
		t.Fatalf("judgement section %+v", sum.Sections)
	}
	// What the user is doing now stays, though its facet has a pattern: the recent highlights come from it.
	st.Entries["n1"] = &entry.Entry{ID: "n1", Facet: entry.Persona, Grain: entry.Action, Type: "current_work", Text: "writing the migration plan",
		Status: entry.Stable, Origin: entry.Behavior, Tagged: true, First: "2026-11-20", Last: "2026-11-20", Count: 2,
		Evidence: []entry.Evidence{{Source: entry.SrcAssistant, Ref: "unit/n1", Day: "2026-11-20", Who: entry.WhoSelf, Role: "user", Quote: "q"}}}
	s.Patterns["ov:persona0001"] = &Pattern{ID: "ov:persona0001", Facet: "persona", Lifecycle: Core, Text: "asks why"}
	if !slices.ContainsFunc(prov.Items(st), func(it profile.CandidateItem) bool { return it.ID == "n1" }) {
		t.Fatal("the current work was dropped with its facet's scattered entries")
	}
	if sum := profile.BuildWith(st, prov, now, "zh-Hans"); !slices.ContainsFunc(sum.Recent, func(r string) bool { return strings.Contains(r, "migration plan") }) {
		t.Fatalf("recent %v", sum.Recent)
	}
	delete(st.Entries, "n1")
	if base := (OverviewBackedProvider{}).Items(st); len(base) != len(profile.EntriesProvider{}.Items(st)) {
		t.Fatal("no overview state: not the curated entries")
	}
}

// A contradiction is kept as its two poles, each in its situation; a blank pole is dropped, at most two stay.
func TestPolesText(t *testing.T) {
	ps := cleanPoles([]Pole{{When: "a deadline is near", Choice: "ship the smaller cut"}, {When: " ", Choice: "x"},
		{When: "the design is open", Choice: "explore widely"}, {When: "third", Choice: "dropped"}})
	if len(ps) != 2 || ps[1].When != "the design is open" {
		t.Fatalf("poles %+v", ps)
	}
	txt := Text(&Pattern{Facet: "judgement", Text: "weighs speed against breadth", Poles: ps})
	if !strings.Contains(txt, "两端：a deadline is near时，ship the smaller cut；the design is open时，explore widely") {
		t.Errorf("text %q", txt)
	}
}

// A pattern scores by multi-dimensional leverage (unified design §2.1): cross-scene/user-owned/check hard contracts
// score highest with progressive CoveredIDs weight, single-scene non-owned contracts are capped at 70, regular
// contracts and principles reward spread/owned/decision-attention/judgement-workflow/poles, and unused patterns score 0.
func TestPatternScoreScale(t *testing.T) {
	hard := &Pattern{Facet: "interaction", Kind: KindContract, Lifecycle: Core, Hard: true, Polarity: PolarityCheck,
		ViolationCount: 5, ScenesCount: 3, UniqueQuotes: 6, CoveredIDs: []string{"i-hand"}}
	if s := Score(hard); s != 80+20+18+12+15+6 {
		t.Errorf("hard contract score = %d, want %d", s, 151)
	}
	multiOwned := &Pattern{Facet: "interaction", Kind: KindContract, Lifecycle: Core, Hard: true, Polarity: PolarityRoute,
		ViolationCount: 1, ScenesCount: 1, UniqueQuotes: 6, CoveredIDs: []string{"h1", "h2", "h3", "h4", "h5", "h6"}}
	if s := Score(multiOwned); s != 80+4+6+12+30 {
		t.Errorf("multi-owned hard contract score = %d, want %d", s, 132)
	}
	narrowHard := &Pattern{Facet: "interaction", Kind: KindContract, Lifecycle: Core, Hard: true, ViolationCount: 10, ScenesCount: 1, UniqueQuotes: 10}
	if s := Score(narrowHard); s != 70 {
		t.Errorf("single-scene non-owned hard contract score = %d, want 70 cap", s)
	}
	reg := &Pattern{Facet: "interaction", Kind: KindContract, Lifecycle: Core, ScenesCount: 3, UniqueQuotes: 4, ActiveWeeks: []string{"w1", "w2"}}
	if s := Score(reg); s != 30+18+8+8 {
		t.Errorf("regular contract score = %d, want %d", s, 64)
	}
	prin := &Pattern{Facet: "judgement", Kind: KindPrinciple, Lifecycle: Core, Dimension: "decision", ScenesCount: 5, UniqueQuotes: 10,
		ActiveWeeks: []string{"w1", "w2", "w3", "w4", "w5"}, Exception: "only for hotfixes", CoveredIDs: []string{"j-hand"}}
	if s := Score(prin); s != 30+25+20+15+10+18+8+6 {
		t.Errorf("principle score = %d, want %d", s, 132)
	}
	if Score(&Pattern{Facet: "judgement", Kind: KindPrinciple, Lifecycle: Core, Dimension: "decision", ScenesCount: 2}) <=
		Score(&Pattern{Facet: "persona", Kind: KindPrinciple, Lifecycle: Core, Dimension: "thinking", ScenesCount: 2}) {
		t.Error("actionable decision/judgement principle not above static persona/thinking observation")
	}
	if Score(&Pattern{Facet: "judgement", Kind: KindPrinciple, Lifecycle: Core, ScenesCount: 2}) <=
		Score(&Pattern{Facet: "judgement", Kind: KindPrinciple, Lifecycle: Scoped, ScenesCount: 2}) {
		t.Error("core not above scoped")
	}
	if Score(&Pattern{Facet: "judgement", Kind: KindPrinciple, Lifecycle: Candidate, ScenesCount: 9}) != 0 {
		t.Error("an unused pattern scored")
	}
}

// Audit quotes sample round-robin across EvidenceIDs so one large class cannot crowd out the others.
func TestAuditQuotesRoundRobin(t *testing.T) {
	var all []*entry.Entry
	var bigMembers []*entry.Entry
	for i := 0; i < 10; i++ {
		a := action(fmt.Sprintf("big%d", i), entry.Workflow, "alpha", entry.SrcAssistant, "2026-11-10", fmt.Sprintf("big quote %d", i))
		all = append(all, a)
		bigMembers = append(bigMembers, a)
	}
	big := class("cbig", entry.Workflow, "big class", bigMembers...)
	other := action("other1", entry.Workflow, "beta", entry.SrcAgent, "2026-11-18", "second evidence quote")
	all = append(all, big, other)
	st := state(all...)
	p := &Pattern{ID: "ov:rr", Facet: string(entry.Workflow), EvidenceIDs: []string{"cbig", "other1"}}
	qs := quotes(st, p, 4)
	if len(qs) != 4 {
		t.Fatalf("got %d quotes: %v", len(qs), qs)
	}
	if !slices.ContainsFunc(qs, func(s string) bool { return strings.Contains(s, "second evidence quote") }) {
		t.Fatalf("second evidence id crowded out: %v", qs)
	}
}

// Hard holdout drops strawman distractors and easy candidates a no-memory model gets right in both orders.
func TestHoldoutHardFilter(t *testing.T) {
	st := fixture()
	s := newState()
	if _, err := runner(&fakeModel{}).Weekly(context.Background(), st, s, "2026-W47", ""); err != nil {
		t.Fatal(err)
	}
	// Strawman distractor: filtered out before any model call.
	straw := action("qstraw", entry.Judgement, "alpha", entry.SrcAsk, "2026-11-24", "check logs")
	straw.Situation, straw.Choice, straw.Rejected = "a service fails", "inspect logs before restarting", "blindly ignore errors and restart"
	// Easy question: fake model answers right without memory in both orders -> dropped by filterHard.
	easy := action("qeasy", entry.Judgement, "alpha", entry.SrcAsk, "2026-11-24", "easy choice")
	easy.Situation, easy.Choice, easy.Rejected = "picking a colour", "choose obvious option", "choose odd option"
	// Hard question: fake model only gets it right when memory mentions "pilot".
	hard := action("qhard", entry.Judgement, "alpha", entry.SrcAsk, "2026-11-25", "pilot rollout")
	hard.Situation, hard.Choice, hard.Rejected = "rolling out a change", "pilot with a small batch first", "roll out to all users at once"
	st.Entries["qstraw"], st.Entries["qeasy"], st.Entries["qhard"] = straw, easy, hard

	m := &fakeModel{predict: func(in predictInput) predictOutput {
		var o predictOutput
		for _, q := range in.Questions {
			pick := "B"
			switch {
			case strings.Contains(q.A, "obvious"):
				pick = "A"
			case strings.Contains(q.B, "obvious"):
				pick = "B"
			case strings.Contains(in.Memory, "pilot"):
				if strings.Contains(q.A, "pilot") {
					pick = "A"
				} else {
					pick = "B"
				}
			}
			o.Answers = append(o.Answers, struct {
				ID   string `json:"id"`
				Pick string `json:"pick"`
			}{q.ID, pick})
		}
		return o
	}}
	h, err := runner(m).Holdout(context.Background(), st, s, "2026-W48", false)
	if err != nil {
		t.Fatal(err)
	}
	if h.N != 1 || h.DroppedEasy != 1 || h.Candidates != 2 {
		t.Fatalf("expected 1 hard question (1 dropped easy, 2 scanned, strawman excluded): %+v", h)
	}
	if h.Rate[ArmOverview] != 1 || h.Rate[ArmNone] != 0 {
		t.Fatalf("unexpected rates: %+v", h)
	}
}

func TestEnrichSubjects(t *testing.T) {
	p := &Pattern{
		ID: "ov:tp1", Facet: string(entry.Tools), Lifecycle: Core,
		Text:      "用 task_plan 派运维写操作时，带 supervisor 并设 user_requested=true，估时间不按人工期。",
		Exception: "涉及 1Password 时改用 secret_env",
		Subjects:  []string{"existing-tag"},
	}
	got := EnrichSubjects(p)
	if len(got) > memsrc.MaxSubjects {
		t.Fatalf("enriched subjects exceeded MaxSubjects: %d %v", len(got), got)
	}
	for _, want := range []string{"existing-tag", "task_plan", "1Password", "派活", "排期", "密钥", "工具"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing expected enriched subject %q in %v", want, got)
		}
	}
}
