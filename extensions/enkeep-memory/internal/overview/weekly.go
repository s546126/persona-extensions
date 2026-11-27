package overview

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

//go:embed synth.md
var synthPrompt string

//go:embed synth.schema.json
var synthSchema []byte

//go:embed counter.md
var counterPrompt string

//go:embed counter.schema.json
var counterSchema []byte

// Bounds of the weekly pass (unified design §1.3, §5.2): principle facets are clustered into compact 3–15 item groups
// packed into at most synthBatches calls of synthBatchItems items each (avoiding 90-item grab-bags); interaction and
// tools rules go through SyncContracts; every passing pattern is checked by counterSearch.
const (
	windowDays      = 28 // the weekly pass looks at entries with evidence in the four weeks before its end
	bootstrapDays   = 84 // ... twelve weeks on a first run (no week yet): the history the user already has
	synthClusterMin = 3
	synthClusterMax = 15
	synthBatchItems = 30
	synthItems      = 90 // upper cap for monthly rescue synthesis
	synthBatches    = 3
	synthExisting   = 40
	synthMaxTokens  = 8000
	counterPatterns = 12
	counterCands    = 8
	counterDays     = 60
	counterMaxTok   = 4000
	driftRatio      = 0.25 // counter-examples at or above this share make a pattern drifting
	recentDays      = 30
)

// Runner runs the overview's passes. Long and Short are the models its calls name (Medium tier on Long, Small on
// Short then Long): the overview never goes through the route's primary models.
type Runner struct {
	M     model.Model
	Long  string
	Short string
	Lang  string
	Now   func() time.Time
	Logf  func(format string, args ...any)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) logf(f string, args ...any) {
	if r.Logf != nil {
		r.Logf(f, args...)
	}
}

func (r *Runner) lang() string {
	if r.Lang == "" {
		return "zh-CN"
	}
	return r.Lang
}

// call asks one question on the named models and decodes the answer into out.
func (r *Runner) call(ctx context.Context, run *Run, step string, tier model.Tier, system string, schema []byte, in any, maxTok int, out any) error {
	body, _ := json.Marshal(in)
	var models []string
	if tier == model.Medium {
		models = union(nil, r.Long, r.Short)
	} else {
		models = union(nil, r.Short, r.Long)
	}
	resp, err := r.M.Complete(ctx, model.Request{Step: "overview_" + step, Tier: tier, System: system, User: string(body),
		MaxTokens: maxTok, Schema: schema, Models: models})
	run.Calls++
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(jsonOf(resp.Text)), out); err != nil {
		return fmt.Errorf("%s: answer not readable: %w", step, err)
	}
	return nil
}

func jsonOf(t string) string {
	i, j := strings.Index(t, "{"), strings.LastIndex(t, "}")
	if i < 0 || j < i {
		return t
	}
	return t[i : j+1]
}

// facetMeaning tells the model which side of the user a facet is.
var facetMeaning = map[entry.Facet]string{
	entry.Interaction: "how the user and the AI work together: when the AI acts on its own, when it asks, how it reports",
	entry.Tools:       "how the AI should work with the user's machines, tools, runtimes, models and environments",
	entry.Judgement:   "the user's default judgements: at a fork of the same kind, what they choose and what they turn down",
	entry.Workflow:    "how the user handles things end to end: the steps, the order, what is checked before what",
	entry.World:       "the user's world: their projects and how they relate, the systems they depend on, the people they work with and the commitments either way (facts only)",
	entry.Persona:     "the user as a person: role, what they care about, how they think and decide, what they focus on lately",
}

type (
	synthItem struct {
		ID        string   `json:"id"`
		Cluster   string   `json:"cluster,omitempty"`
		Text      string   `json:"text"`
		Situation string   `json:"situation,omitempty"`
		Choice    string   `json:"choice,omitempty"`
		Rejected  string   `json:"rejected,omitempty"`
		Dimension string   `json:"dimension,omitempty"`
		Hint      string   `json:"hint,omitempty"`
		Principle string   `json:"principle,omitempty"`
		Quotes    []string `json:"quotes,omitempty"`
		Projects  []string `json:"projects,omitempty"`
		Sources   []string `json:"source_kinds,omitempty"`
		Weeks     int      `json:"weeks"`
		Evidence  int      `json:"independent_sources"`
		People    int      `json:"people,omitempty"`
	}
	synthExistingPattern struct {
		ID        string `json:"id"`
		Text      string `json:"text"`
		Situation string `json:"situation,omitempty"`
		Choice    string `json:"choice,omitempty"`
		Exception string `json:"exception,omitempty"`
	}
	synthInput struct {
		Facet    string                 `json:"facet"`
		Meaning  string                 `json:"facet_meaning"`
		Lang     string                 `json:"language"`
		Items    []synthItem            `json:"items"`
		Existing []synthExistingPattern `json:"existing"`
	}
	synthProposal struct {
		Extends         string   `json:"extends"`
		Text            string   `json:"text"`
		Situation       string   `json:"situation"`
		Choice          string   `json:"choice"`
		Rejected        string   `json:"rejected"`
		Exception       string   `json:"exception"`
		Dimension       string   `json:"dimension"`
		DefaultContrast string   `json:"default_contrast"`
		HoldsForAnyone  bool     `json:"holds_for_anyone"`
		Members         []string `json:"members"`
		Reason          string   `json:"reason"`
	}
	synthOutput struct {
		Patterns []synthProposal `json:"patterns"`
	}
)

// candidate is an entry offered to the synthesis.
type candidate struct {
	e *entry.Entry
	t tally
}

// candidates are the entries of facet f with evidence in [from, cutoff), most spread first. Classes and abstracts
// stand for their actions; an action is offered on its own only when it has no live class.
func candidates(st *store.State, f entry.Facet, from, cutoff string, keep func(*entry.Entry) bool) []candidate {
	var out []candidate
	for _, e := range st.Entries {
		if !usable(st, e) || facetOf(e) != f || (keep != nil && !keep(e)) {
			continue
		}
		if entry.IsAssistantNote(e.Origin) || entry.IsEphemeral(e) {
			continue
		}
		if f == entry.Persona && isS3S4Only(e) {
			continue
		}
		if cutoff != "" && e.First >= cutoff {
			continue
		}
		if e.Grain == entry.Action {
			if p := st.Entries[e.Parent]; p != nil && p.Live() {
				continue
			}
		}
		t := count(st, []string{e.ID}, cutoff)
		if len(t.sources) == 0 || (t.last < from && !isUserOwned(e)) {
			continue
		}
		out = append(out, candidate{e, t})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if sa, sb := a.t.sceneCount(), b.t.sceneCount(); sa != sb {
			return sa > sb
		}
		if len(a.t.sources) != len(b.t.sources) {
			return len(a.t.sources) > len(b.t.sources)
		}
		if a.t.last != b.t.last {
			return a.t.last > b.t.last
		}
		return a.e.ID < b.e.ID
	})
	return out
}

type candCluster struct {
	label string
	items []candidate
}

// clusterCandidates projects a facet's candidates into compact 3–15 item clusters by cognitive clues
// (Leverage.Dimension buckets, cross-project Principle clues, and spread-ordered general history) and packs them
// round-robin across buckets into synthesis batches of at most synthBatchItems items (at most synthBatches calls per
// facet), so no single dimension starves other dimensions or long-lived high-spread entries.
func clusterCandidates(cands []candidate) [][]candCluster {
	byDim := map[string][]candidate{}
	var coreOwned, princCross, rest []candidate
	for _, c := range cands {
		if isUserOwned(c.e) || c.e.Grain == entry.Abstract {
			coreOwned = append(coreOwned, c)
		} else if c.e.Leverage != nil && c.e.Leverage.Dimension != "" {
			byDim[c.e.Leverage.Dimension] = append(byDim[c.e.Leverage.Dimension], c)
		} else if c.e.Principle() != "" && len(c.t.projects) >= MinScenes {
			princCross = append(princCross, c)
		} else {
			rest = append(rest, c)
		}
	}
	toChunks := func(label string, items []candidate) []candCluster {
		var out []candCluster
		for len(items) > 0 {
			n := min(len(items), synthClusterMax)
			if len(items)-n > 0 && len(items)-n < synthClusterMin {
				n = len(items)
			}
			out = append(out, candCluster{label: label, items: items[:n]})
			items = items[n:]
		}
		return out
	}
	var queues [][]candCluster
	if len(coreOwned) > 0 {
		queues = append(queues, toChunks("core_owned", coreOwned))
	}
	for _, dim := range entry.Dimensions {
		if len(byDim[dim]) >= synthClusterMin {
			queues = append(queues, toChunks("dim:"+dim, byDim[dim]))
		} else {
			rest = append(rest, byDim[dim]...)
		}
	}
	if len(princCross) >= synthClusterMin {
		queues = append(queues, toChunks("principle", princCross))
	} else {
		rest = append(rest, princCross...)
	}
	if len(rest) > 0 {
		// Re-sort rest by spread so high-spread historical/handwritten entries surface first in general chunks.
		sort.SliceStable(rest, func(i, j int) bool {
			a, b := rest[i], rest[j]
			if sa, sb := a.t.sceneCount(), b.t.sceneCount(); sa != sb {
				return sa > sb
			}
			if len(a.t.sources) != len(b.t.sources) {
				return len(a.t.sources) > len(b.t.sources)
			}
			if a.t.last != b.t.last {
				return a.t.last > b.t.last
			}
			return a.e.ID < b.e.ID
		})
		queues = append(queues, toChunks("general", rest))
	}
	// Round-robin across bucket queues so every dimension and general history gets representation in the first batches.
	var clusters []candCluster
	for {
		took := false
		for i := range queues {
			if len(queues[i]) > 0 {
				clusters = append(clusters, queues[i][0])
				queues[i] = queues[i][1:]
				took = true
			}
		}
		if !took {
			break
		}
	}
	// Pack clusters into calls of at most synthBatchItems items, capped at synthBatches calls.
	var batches [][]candCluster
	var cur []candCluster
	curN := 0
	for _, cl := range clusters {
		if curN > 0 && curN+len(cl.items) > synthBatchItems {
			batches = append(batches, cur)
			if len(batches) >= synthBatches {
				return batches
			}
			cur, curN = nil, 0
		}
		cur = append(cur, cl)
		curN += len(cl.items)
	}
	if len(cur) > 0 && len(batches) < synthBatches {
		batches = append(batches, cur)
	}
	return batches
}

// Weekly is L1 for the week (ISO, "2026-W47") on st, a snapshot of the curated state. cutoff, when set, is a
// backtest's: only evidence before that day is used, and the patterns go into a scratch state (s), not the holder's.
// Principle facets (③④⑥⑤) run clustered quote-grounded synthesis; contract facets (①②) run SyncContracts; all
// passing patterns then go through counterSearch.
func (r *Runner) Weekly(ctx context.Context, st *store.State, s *State, week, cutoff string) (*Run, error) {
	days, err := weekDays(week)
	if err != nil {
		return nil, err
	}
	end := cutoff
	if end == "" {
		end = addDays(days[6], 1)
	}
	window := windowDays
	if len(s.Weeks) == 0 {
		window = bootstrapDays
	}
	from := addDays(end, -window)
	run := &Run{Period: week, At: stamp(r.now()), Cutoff: cutoff, Counts: map[string]int{"window_days": window}}
	now := stamp(r.now())
	var touched []string
	// 0. A1 Core Profile & Anti-Misread Boundaries (T0 + multi-week cross-scene T1 Persona).
	pcIDs, err := r.SyncProfileCore(ctx, st, s, run, cutoff, now)
	if err != nil {
		return run, err
	}
	touched = union(touched, pcIDs...)
	// 1. L1 Principles (③ Judgement, ④ Workflow, ⑤ World, ⑥ Persona).
	for _, f := range PrincipleFacets {
		cands := candidates(st, f, from, cutoff, nil)
		run.Counts["items_"+string(f)] = len(cands)
		if len(cands) < 3 {
			continue
		}
		ids, err := r.synthesize(ctx, st, s, run, f, cands, cutoff, now)
		if err != nil {
			return run, err
		}
		touched = union(touched, ids...)
	}
	// 2. L1 Collaboration Contracts & Ops Runbooks (① Interaction, ② Tools).
	cIDs, err := r.SyncContracts(ctx, st, s, run, cutoff, now)
	if err != nil {
		return run, err
	}
	touched = union(touched, cIDs...)
	// 3. Counter-example search for all passing principles and universal contracts.
	if err := r.counterSearch(ctx, st, s, run, touched, end, cutoff, now); err != nil {
		return run, err
	}
	// 4. Global Reconciliation Pass (synonym merge, tension resolution into Poles, ops diversion).
	recIDs, err := r.Reconcile(ctx, st, s, run, cutoff, now)
	if err != nil {
		return run, err
	}
	touched = union(touched, recIDs...)
	for _, id := range touched {
		p := s.Patterns[id]
		if p == nil {
			continue
		}
		run.Counts["lifecycle_"+p.Lifecycle]++
		if p.Gate != "" {
			run.Counts["gate_"+p.Gate]++
		}
	}
	run.Changed = touched
	return run, nil
}

// synthesize asks for the principles behind one facet's candidates, grouped into compact cognitive clusters and
// grounded in user quotes, and writes them into s: a proposal extending an existing principle adds its members there.
func (r *Runner) synthesize(ctx context.Context, st *store.State, s *State, run *Run, f entry.Facet, cands []candidate, cutoff, now string) ([]string, error) {
	var touched []string
	batches := clusterCandidates(cands)
	for _, batch := range batches {
		total := 0
		for _, cl := range batch {
			total += len(cl.items)
		}
		if total < synthClusterMin {
			continue
		}
		in := synthInput{Facet: string(f), Meaning: facetMeaning[f], Lang: r.lang()}
		local := map[string]string{}
		idx := 0
		for _, cl := range batch {
			for _, c := range cl.items {
				idx++
				id := fmt.Sprintf("i%d", idx)
				local[id] = c.e.ID
				var dim, hint, princ string
				if c.e.Leverage != nil {
					dim, hint, princ = c.e.Leverage.Dimension, c.e.Leverage.Hint, clip(c.e.Leverage.Principle, 100)
				}
				in.Items = append(in.Items, synthItem{
					ID: id, Cluster: cl.label, Text: clip(c.e.Shown(), 160), Situation: clip(c.e.Situation, 80),
					Choice: clip(c.e.Choice, 80), Rejected: clip(c.e.Rejected, 60),
					Dimension: dim, Hint: hint, Principle: princ,
					Quotes:   itemQuotes(st, c.e, cutoff, 2),
					Projects: c.t.projects, Sources: c.t.groups,
					Weeks: len(c.t.weeks), Evidence: len(c.t.sources), People: len(c.t.people),
				})
			}
		}
		existing := s.Sorted(func(p *Pattern) bool { return p.Facet == string(f) && p.Lifecycle != Retired })
		for i, p := range existing {
			if i >= synthExisting {
				break
			}
			id := fmt.Sprintf("p%d", i+1)
			local[id] = p.ID
			in.Existing = append(in.Existing, synthExistingPattern{ID: id, Text: clip(p.Text, 200), Situation: clip(p.Situation, 80),
				Choice: clip(p.Choice, 80), Exception: clip(p.Exception, 80)})
		}
		var out synthOutput
		err := r.call(ctx, run, "synth", model.Medium, synthPrompt, synthSchema, in, synthMaxTokens, &out)
		if errors.Is(err, model.ErrRefused) {
			run.Counts["synth_refused"]++
			run.Notes = append(run.Notes, fmt.Sprintf("%s: the model declined a batch", f))
			continue
		}
		if err != nil {
			return touched, err
		}
		run.Counts["proposed"] += len(out.Patterns)
		for _, prop := range out.Patterns {
			var members []string
			for _, m := range prop.Members {
				if id, ok := local[m]; ok && strings.HasPrefix(m, "i") {
					members = union(members, id)
				}
			}
			text := strings.TrimSpace(prop.Text)
			if text == "" || len(members) == 0 {
				run.Counts["proposal_dropped"]++
				continue
			}
			var p *Pattern
			if ext, ok := local[prop.Extends]; ok && strings.HasPrefix(prop.Extends, "p") {
				p = s.Patterns[ext]
			}
			if p == nil {
				id := patternID(string(f), prop.Situation, prop.Choice, text)
				p = s.Patterns[id]
				if p == nil {
					p = &Pattern{ID: id, Facet: string(f), Kind: KindPrinciple, Lifecycle: Candidate, Created: now}
					s.Patterns[id] = p
					run.Counts["new"]++
				}
			}
			if p.Lifecycle == Retired || p.Lifecycle == EraClosed {
				p.Lifecycle = Candidate // seen again: back for the gate
			}
			if p.Kind == "" {
				p.Kind = KindPrinciple
			}
			p.Text, p.Situation, p.Choice = text, strings.TrimSpace(prop.Situation), strings.TrimSpace(prop.Choice)
			p.Rejected, p.DefaultContrast = strings.TrimSpace(prop.Rejected), strings.TrimSpace(prop.DefaultContrast)
			p.HoldsForAnyone = prop.HoldsForAnyone
			if d := strings.TrimSpace(prop.Dimension); slices.Contains(entry.Dimensions, d) {
				p.Dimension = d
			}
			if x := strings.TrimSpace(prop.Exception); x != "" {
				p.Exception = x
			}
			p.EvidenceIDs = union(p.EvidenceIDs, members...)
			for _, mid := range members {
				if e := st.Entries[mid]; isUserOwned(e) {
					p.CoveredIDs = union(p.CoveredIDs, mid)
				}
			}
			sort.Strings(p.CoveredIDs)
			p.Reason = strings.TrimSpace(prop.Reason)
			p.UpdatedAt = now
			if patternIsOpsRunbook(p) {
				p.Kind = KindOpsRunbook
				p.Hard = false
				run.Counts["principle_diverted_ops"]++
			}
			recount(st, p, cutoff)
			touched = union(touched, p.ID)
		}
	}
	return touched, nil
}

// recount tallies a pattern's evidence again and sets its counts, gate, tags, subjects and contract/principle metadata.
// Evidence the user withdrew (marked wrong, deleted, a forgotten source) is not counted but kept in EvidenceIDs, so
// marking it right again brings it back.
func recount(st *store.State, p *Pattern, cutoff string) {
	p.EvidenceIDs = followMerges(st, p.EvidenceIDs)
	p.CounterIDs = followMerges(st, p.CounterIDs)
	sort.Strings(p.EvidenceIDs)
	live := liveIDs(st, p.EvidenceIDs)
	if p.IsProfileCore() || p.IsOpsRunbook() || p.IsContract() {
		// Also include live user-owned entries (T0) that may not pass memsrc.Pool filters.
		for _, id := range p.EvidenceIDs {
			if e := st.Entries[id]; e != nil && e.Live() && isUserOwned(e) && !slices.Contains(live, id) {
				live = append(live, id)
			}
		}
	}
	switch {
	case p.IsProfileCore(), p.IsOpsRunbook():
		p.Hard = false
	case p.IsContract():
		if patternIsOpsRunbook(p) {
			p.Kind = KindOpsRunbook
			p.Hard = false
		} else {
			p.Hard, p.ViolationCount = contractHard(st, live, cutoff)
		}
	default:
		if evDim := inferDimension(st, live); evDim != "" {
			// Evidence majority dimension takes precedence over a model guess (prevents defaulting everything to decision).
			p.Dimension = evDim
		}
	}
	apply(p, count(st, live, cutoff))
	p.Categories, p.OriginSpace = tags(st, p, append(live, liveIDs(st, p.CounterIDs)...))
	p.Subjects = subjects(st, live)
}

// followMerges replaces an entry the curation merged into another by that one (the weekly class merge), so a pattern
// keeps its evidence across merges.
func followMerges(st *store.State, ids []string) []string {
	var out []string
	for _, id := range ids {
		for i := 0; i < 5; i++ {
			e := st.Entries[id]
			if e == nil || e.MergedInto == "" || st.Entries[e.MergedInto] == nil {
				break
			}
			id = e.MergedInto
		}
		out = union(out, id)
	}
	return out
}

// liveIDs are the ids of entries the overview may still use.
func liveIDs(st *store.State, ids []string) []string {
	var out []string
	for _, id := range ids {
		if usable(st, st.Entries[id]) {
			out = append(out, id)
		}
	}
	return out
}

type (
	counterCand struct {
		ID        string `json:"id"`
		Text      string `json:"text"`
		Situation string `json:"situation,omitempty"`
		Choice    string `json:"choice,omitempty"`
		Day       string `json:"day,omitempty"`
	}
	counterPattern struct {
		ID         string        `json:"id"`
		Text       string        `json:"text"`
		Situation  string        `json:"situation,omitempty"`
		Choice     string        `json:"choice,omitempty"`
		Candidates []counterCand `json:"candidates"`
	}
	counterInput struct {
		Lang     string           `json:"language"`
		Patterns []counterPattern `json:"patterns"`
	}
	counterVerdict struct {
		ID         string   `json:"id"`
		Counters   []string `json:"counters"`
		Exception  string   `json:"exception"`
		Poles      []Pole   `json:"poles,omitempty"`
		Overturned bool     `json:"overturned"`
		Reason     string   `json:"reason"`
	}
	counterOutput struct {
		Verdicts []counterVerdict `json:"verdicts"`
	}
)

// itemQuotes returns up to n distinct representative user quotes under e (before cutoff), preferring explicit WhoSelf
// lines, so synthesis and contract aggregation stay grounded in the user's actual words.
func itemQuotes(st *store.State, e *entry.Entry, cutoff string, n int) []string {
	evs := evidenceOf(st, e, cutoff)
	// Prefer explicit WhoSelf quotes before unresolved-Who legacy quotes.
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].self && !evs[j].self })
	var out []string
	seenUnit := map[string]bool{}
	seenText := map[string]bool{}
	for _, ev := range evs {
		if !ev.quoted || seenUnit[ev.key] || seenText[ev.quoteKey] {
			continue
		}
		x := st.Entries[ev.entry]
		if x == nil {
			continue
		}
		for _, line := range x.Evidence {
			if line.Ref+"@"+line.Day == ev.key && isUserQuote(line) {
				out = append(out, clip(line.Quote, 100))
				seenUnit[ev.key] = true
				seenText[ev.quoteKey] = true
				break
			}
		}
		if len(out) >= n {
			break
		}
	}
	return out
}

// counterCandidates are the entries that might contradict p: its classes' recorded counters first, then the entries of
// its facet over the counterDays before end that share a similar situation while differing in choice.
func counterCandidates(st *store.State, p *Pattern, end, cutoff string) []*entry.Entry {
	from := addDays(end, -counterDays)
	in := map[string]bool{}
	for _, id := range p.EvidenceIDs {
		in[id] = true
		if e := st.Entries[id]; e != nil {
			for _, m := range e.Members {
				in[m] = true
			}
		}
	}
	var out []*entry.Entry
	for _, id := range p.EvidenceIDs {
		e := st.Entries[id]
		if e == nil {
			continue
		}
		for _, c := range e.Counters {
			if x := st.Entries[c]; x != nil && usable(st, x) && !in[c] && len(out) < counterCands/2 && !slices.Contains(out, x) {
				out = append(out, x)
			}
		}
	}
	sitProbe := strings.TrimSpace(p.Situation)
	if sitProbe == "" {
		sitProbe = p.Text
	}
	choiceProbe := strings.TrimSpace(p.Choice)
	type scored struct {
		e *entry.Entry
		s float64
	}
	var pool []scored
	for _, e := range st.Entries {
		if in[e.ID] || !usable(st, e) || string(facetOf(e)) != p.Facet || e.Grain == entry.Abstract || slices.Contains(out, e) {
			continue
		}
		if e.Last < from || cutoff != "" && e.First >= cutoff {
			continue
		}
		sitSim := norm.Jaccard(sitProbe, e.Situation+" "+e.Shown())
		if sitSim < 0.06 {
			continue
		}
		// Down-rank entries that appear to make the same choice, so genuine counter-examples rise first.
		score := sitSim
		if choiceProbe != "" {
			choiceSim := norm.Jaccard(choiceProbe, e.Choice+" "+e.Shown())
			score = sitSim * (1 - 0.5*choiceSim)
		}
		pool = append(pool, scored{e, score})
	}
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].s != pool[j].s {
			return pool[i].s > pool[j].s
		}
		return pool[i].e.ID < pool[j].e.ID
	})
	for _, x := range pool {
		if len(out) >= counterCands {
			break
		}
		out = append(out, x.e)
	}
	return out
}

// counterSearch puts the patterns that pass the gate to their likely counter-examples, facet by facet, and settles
// their lifecycle: overturned → retired; recent counters ≥ 25% → drifting; an exception → scoped; else core.
// Patterns that fail the gate stay candidates without a call.
func (r *Runner) counterSearch(ctx context.Context, st *store.State, s *State, run *Run, ids []string, end, cutoff, now string) error {
	byFacet := map[string][]*Pattern{}
	for _, id := range ids {
		p := s.Patterns[id]
		if p == nil || p.IsProfileCore() || p.IsOpsRunbook() {
			continue
		}
		if p.Gate != "" {
			p.Lifecycle = Candidate
			continue
		}
		byFacet[p.Facet] = append(byFacet[p.Facet], p)
	}
	for _, f := range Facets {
		ps := byFacet[string(f)]
		if len(ps) == 0 {
			continue
		}
		sort.Slice(ps, func(i, j int) bool {
			return ps[i].SourcesCount*ps[i].ScenesCount > ps[j].SourcesCount*ps[j].ScenesCount
		})
		for len(ps) > 0 {
			batch := ps[:min(len(ps), counterPatterns)]
			ps = ps[len(batch):]
			if err := r.counterBatch(ctx, st, run, batch, end, cutoff, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runner) counterBatch(ctx context.Context, st *store.State, run *Run, ps []*Pattern, end, cutoff, now string) error {
	in := counterInput{Lang: r.lang()}
	local := map[string]map[string]string{}
	byID := map[string]*Pattern{}
	for i, p := range ps {
		pid := fmt.Sprintf("p%d", i+1)
		byID[pid] = p
		local[pid] = map[string]string{}
		cp := counterPattern{ID: pid, Text: clip(p.Text, 200), Situation: clip(p.Situation, 80), Choice: clip(p.Choice, 80)}
		for j, e := range counterCandidates(st, p, end, cutoff) {
			cid := fmt.Sprintf("c%d", j+1)
			local[pid][cid] = e.ID
			cp.Candidates = append(cp.Candidates, counterCand{ID: cid, Text: clip(e.Shown(), 140), Situation: clip(e.Situation, 60),
				Choice: clip(e.Choice, 60), Day: e.Last})
		}
		in.Patterns = append(in.Patterns, cp)
	}
	var out counterOutput
	if len(in.Patterns) > 0 && slices.ContainsFunc(in.Patterns, func(p counterPattern) bool { return len(p.Candidates) > 0 }) {
		err := r.call(ctx, run, "counter", model.Small, counterPrompt, counterSchema, in, counterMaxTok, &out)
		if errors.Is(err, model.ErrRefused) {
			run.Counts["counter_refused"]++
			out = counterOutput{}
		} else if err != nil {
			return err
		}
	}
	verdicts := map[string]counterVerdict{}
	for _, v := range out.Verdicts {
		verdicts[v.ID] = v
	}
	for pid, p := range byID {
		v, answered := verdicts[pid]
		var counters []string
		for _, c := range v.Counters {
			if id, ok := local[pid][c]; ok {
				counters = union(counters, id)
			}
		}
		settle(st, p, counters, v.Exception, v.Overturned, v.Reason, end, cutoff, answered || len(local[pid]) == 0)
		if ends := cleanPoles(v.Poles); len(ends) == 2 && !v.Overturned && len(counters) > 0 {
			p.Poles = ends // both ends, each with when it holds (0.2.0-beta.52)
		}
		p.UpdatedAt = now
	}
	return nil
}

// settle sets a pattern's lifecycle from its counter-search. checked is false when its counter-search could not be
// answered: it then stays a candidate (never used unchecked).
func settle(st *store.State, p *Pattern, counters []string, exception string, overturned bool, reason, end, cutoff string, checked bool) {
	p.CounterIDs = counters
	recount(st, p, cutoff)
	if p.Gate != "" {
		p.Lifecycle = Candidate
		return
	}
	if !checked {
		p.Lifecycle, p.Reason = Candidate, "counter-search not answered"
		return
	}
	if reason != "" {
		p.Reason = strings.TrimSpace(reason)
	}
	if overturned {
		p.Lifecycle = Retired
		return
	}
	p.CounterRatio = 0
	if n := len(counters); n > 0 {
		p.CounterRatio = float64(n) / float64(n+p.SourcesCount)
	}
	recent := 0
	since := addDays(end, -recentDays)
	for _, id := range counters {
		if e := st.Entries[id]; e != nil && e.Last >= since {
			recent++
		}
	}
	if x := strings.TrimSpace(exception); x != "" {
		p.Exception = x
	}
	switch {
	case p.CounterRatio >= driftRatio && recent*2 >= len(counters):
		p.Lifecycle = Drifting
	case p.Exception != "":
		p.Lifecycle = Scoped
	default:
		p.Lifecycle = Core
	}
}

func weekDays(week string) ([]string, error) {
	var y, w int
	if _, err := fmt.Sscanf(week, "%d-W%d", &y, &w); err != nil || w < 1 || w > 53 {
		return nil, fmt.Errorf("week %q: want YYYY-Www", week)
	}
	jan4 := time.Date(y, 1, 4, 0, 0, 0, 0, time.UTC)
	mon := jan4.AddDate(0, 0, -int((jan4.Weekday()+6)%7)+(w-1)*7)
	out := make([]string, 7)
	for i := range out {
		out[i] = mon.AddDate(0, 0, i).Format("2006-01-02")
	}
	return out, nil
}

func addDays(day string, n int) string {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

// cleanPoles keeps the ends that name both a situation and a choice, clipped; at most two.
func cleanPoles(ps []Pole) []Pole {
	var out []Pole
	for _, p := range ps {
		w, c := strings.TrimSpace(p.When), strings.TrimSpace(p.Choice)
		if w == "" || c == "" || len(out) == 2 {
			continue
		}
		out = append(out, Pole{When: clip(w, 80), Choice: clip(c, 80)})
	}
	return out
}
