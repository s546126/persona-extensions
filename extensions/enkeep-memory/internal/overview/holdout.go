package overview

import (
	"context"
	"crypto/sha1"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/export"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

//go:embed predict.md
var predictPrompt string

//go:embed predict.schema.json
var predictSchema []byte

// PredictPrompt and PredictSchema are the forward check's question, for the hard evaluation (hardeval) to put its
// questions the same way.
var (
	PredictPrompt = predictPrompt
	PredictSchema = predictSchema
)

// The forward check (design 2.5, line 3): the user's real choices first seen in a week (a situation, what they chose
// and what they turned down) are filtered by code to remove obvious strawman distractors and then screened without
// memory in both option orders so easy common-sense questions are dropped. The surviving hard questions are put to the
// model in both option orders under three arms: the patterns kept before the week, scattered entries in the same
// character budget, and no memory (answered afresh, never reusing the filter pass). Scoring is purely by code (strict
// 2/2 across both orders to eliminate position bias, with trial rates also noted). Patterns or scattered entries
// resting on a question's entry, its class, or any of its ref@day units are excluded so no arm sees the answer.
const (
	HoldoutMost        = 30
	holdoutScanMost    = 120
	holdoutFilterRound = 30
	holdoutBatch       = 15
	holdoutMaxTok      = 1500
	memoryCharsMost    = 6000
	minOptionRunes     = 6
)

// Arms of the check.
const (
	ArmOverview = "overview"
	ArmScatter  = "scatter"
	ArmNone     = "none"
)

// question is one held-out real choice.
type question struct {
	e *entry.Entry
}

// strawmanCues are phrases that turn a rejected option into an obvious strawman any stranger would turn down without
// knowing the user. Real trade-offs have plausible alternatives.
var strawmanCues = []string{
	"盲目", "草率", "胡乱", "敷衍", "随意", "乱改", "瞎猜",
	"不验证", "不检查", "不核对", "不确认", "不测试", "跳过测试", "跳过验证", "跳过检查",
	"直接忽略", "忽略报错", "忽略错误", "无视错误", "掩盖错误",
	"伪造", "编造", "造假", "欺骗", "泄露", "硬编码", "不看直接",
	"blindly", "carelessly", "randomly", "ignore error", "ignore fail", "skip test", "skip check",
	"fabricate", "hardcode", "leak secret",
}

// strawmanRejected reports whether a choice/rejected pair is too trivial or unbalanced to test personal preference.
func strawmanRejected(choice, rejected string) bool {
	c, r := strings.TrimSpace(choice), strings.TrimSpace(rejected)
	if len([]rune(c)) < minOptionRunes || len([]rune(r)) < minOptionRunes {
		return true
	}
	if norm.Jaccard(c, r) > 0.7 {
		return true
	}
	low := strings.ToLower(r)
	for _, cue := range strawmanCues {
		if strings.Contains(low, cue) {
			return true
		}
	}
	return false
}

// questions draws real choice candidates first seen in the week (situation, choice, plausible rejected option) across
// the four choice facets, balanced round-robin in hash order (no cherry picking), up to holdoutScanMost.
func questions(st *store.State, days []string) []question {
	choiceFacets := []entry.Facet{entry.Judgement, entry.Workflow, entry.Tools, entry.Interaction}
	byFacet := map[entry.Facet][]question{}
	for _, e := range st.Entries {
		if !usable(st, e) || e.Grain != entry.Action || e.First < days[0] || e.First > days[6] {
			continue
		}
		if len(e.Cats()) > 0 || e.OriginSpace != "" {
			continue
		}
		if !slices.Contains(choiceFacets, e.Facet) {
			continue
		}
		if strings.TrimSpace(e.Situation) == "" || strawmanRejected(e.Choice, e.Rejected) {
			continue
		}
		byFacet[e.Facet] = append(byFacet[e.Facet], question{e: e})
	}
	h := func(s string) string { x := sha1.Sum([]byte(s)); return fmt.Sprintf("%x", x) }
	for _, f := range choiceFacets {
		qs := byFacet[f]
		sort.Slice(qs, func(i, j int) bool { return h(qs[i].e.ID) < h(qs[j].e.ID) })
		byFacet[f] = qs
	}
	var out []question
	for len(out) < holdoutScanMost {
		took := false
		for _, f := range choiceFacets {
			if qs := byFacet[f]; len(qs) > 0 && len(out) < holdoutScanMost {
				out, byFacet[f], took = append(out, qs[0]), qs[1:], true
			}
		}
		if !took {
			break
		}
	}
	return out
}

type (
	predictQuestion struct {
		ID        string `json:"id"`
		Situation string `json:"situation"`
		A         string `json:"A"`
		B         string `json:"B"`
	}
	predictInput struct {
		Memory    string            `json:"memory"`
		Questions []predictQuestion `json:"questions"`
	}
	predictOutput struct {
		Answers []struct {
			ID   string `json:"id"`
			Pick string `json:"pick"`
		} `json:"answers"`
	}
	askedTrial struct {
		id        string
		qIdx      int
		situation string
		a, b      string
		answer    string
	}
)

// twoOrderTrials builds both AB and BA trials for qs.
func twoOrderTrials(qs []question) []askedTrial {
	out := make([]askedTrial, 0, len(qs)*2)
	for i, q := range qs {
		out = append(out,
			askedTrial{id: fmt.Sprintf("q%d_1", i+1), qIdx: i, situation: q.e.Situation, a: q.e.Choice, b: q.e.Rejected, answer: "A"},
			askedTrial{id: fmt.Sprintf("q%d_2", i+1), qIdx: i, situation: q.e.Situation, a: q.e.Rejected, b: q.e.Choice, answer: "B"},
		)
	}
	return out
}

// askTrials asks m to answer trials given mem, returning per-question right count (0, 1 or 2) and total right trials.
func (r *Runner) askTrials(ctx context.Context, run *Run, mem string, nQ int, trials []askedTrial) ([]int, int, int, error) {
	perQ := make([]int, nQ)
	totalRight := 0
	declined := 0
	for b := 0; b < len(trials); b += holdoutBatch {
		batch := trials[b:min(len(trials), b+holdoutBatch)]
		in := predictInput{Memory: mem}
		for _, t := range batch {
			in.Questions = append(in.Questions, predictQuestion{ID: t.id, Situation: clip(t.situation, 160),
				A: clip(t.a, 120), B: clip(t.b, 120)})
		}
		var out predictOutput
		err := r.call(ctx, run, "predict", model.Small, predictPrompt, predictSchema, in, holdoutMaxTok, &out)
		if errors.Is(err, model.ErrRefused) {
			declined++
			continue
		}
		if err != nil {
			return perQ, totalRight, declined, err
		}
		picks := map[string]string{}
		for _, a := range out.Answers {
			picks[a.ID] = a.Pick
		}
		for _, t := range batch {
			if picks[t.id] == t.answer {
				perQ[t.qIdx]++
				totalRight++
			}
		}
	}
	return perQ, totalRight, declined, nil
}

// filterHard screens candidates without memory in both option orders and drops any question a stranger gets right both
// times, stopping once HoldoutMost hard questions are found.
func (r *Runner) filterHard(ctx context.Context, run *Run, cands []question) (hard []question, scanned, dropped int, err error) {
	for start := 0; start < len(cands) && len(hard) < HoldoutMost; start += holdoutFilterRound {
		chunk := cands[start:min(len(cands), start+holdoutFilterRound)]
		trials := twoOrderTrials(chunk)
		perQ, _, _, aerr := r.askTrials(ctx, run, "", len(chunk), trials)
		if aerr != nil {
			return hard, scanned, dropped, aerr
		}
		scanned += len(chunk)
		for i, q := range chunk {
			if perQ[i] == 2 {
				dropped++
				continue
			}
			if len(hard) < HoldoutMost {
				hard = append(hard, q)
			}
		}
	}
	return hard, scanned, dropped, nil
}

// entryUnits returns all ref@day keys under e (walking its evidence tree).
func entryUnits(st *store.State, e *entry.Entry) []string {
	var out []string
	for _, ev := range evidenceOf(st, e, "") {
		if !slices.Contains(out, ev.key) {
			out = append(out, ev.key)
		}
	}
	return out
}

// Holdout checks the patterns of s (kept before the week) against the week's hard real choices in st. backtest marks a
// check whose patterns were built with a cutoff at the week's start.
func (r *Runner) Holdout(ctx context.Context, st *store.State, s *State, week string, backtest bool) (*Holdout, error) {
	days, err := weekDays(week)
	if err != nil {
		return nil, err
	}
	cands := questions(st, days)
	h := &Holdout{Week: week, At: stamp(r.now()), Backtest: backtest, Hits: map[string]int{}, Rate: map[string]float64{},
		Chars: map[string]int{}}
	if len(cands) == 0 {
		h.Notes = append(h.Notes, "no real choice with a plausible turned-down option was first seen in the week")
		return h, nil
	}
	run := &Run{}
	qs, scanned, dropped, err := r.filterHard(ctx, run, cands)
	if err != nil {
		h.Calls = run.Calls
		return h, err
	}
	h.Candidates, h.DroppedEasy = scanned, dropped
	if len(qs) == 0 {
		// Fall back only if every candidate was easy, and record it plainly.
		qs = cands[:min(len(cands), HoldoutMost)]
		h.Notes = append(h.Notes, "all candidates were answered right without memory; fell back to unfiltered candidates")
	}
	h.N = len(qs)

	// Anti-leak set: every question's own entry, its parent class, and all entries sharing any of its ref@day units.
	leak := map[string]bool{}
	leakUnits := map[string]bool{}
	for _, q := range qs {
		leak[q.e.ID] = true
		if q.e.Parent != "" {
			leak[q.e.Parent] = true
		}
		for _, u := range entryUnits(st, q.e) {
			leakUnits[u] = true
		}
	}
	hasLeakUnit := func(e *entry.Entry) bool {
		if e == nil {
			return false
		}
		for _, u := range entryUnits(st, e) {
			if leakUnits[u] {
				return true
			}
		}
		return false
	}

	// The patterns' memory.
	var lines []string
	for _, p := range s.Sorted(func(p *Pattern) bool { return p.Used() && !private(p) }) {
		if slices.ContainsFunc(p.EvidenceIDs, func(id string) bool { return leak[id] || hasLeakUnit(st.Entries[id]) }) {
			h.Excluded++
			continue
		}
		lines = append(lines, "- "+Text(p))
	}
	ovMem := fit(lines, memoryCharsMost)
	h.Patterns = len(strings.Split(strings.TrimSpace(ovMem), "\n"))
	if strings.TrimSpace(ovMem) == "" {
		h.Patterns = 0
	}

	// The scattered memory: curated entries in memory.entries' order (standing, rank, newest), first seen before the
	// week, matching the exact character budget of the patterns (at least 1500 when overview is empty).
	budget := max(len([]rune(ovMem)), 1500)
	var pool []*entry.Entry
	for _, e := range st.Entries {
		if leak[e.ID] || hasLeakUnit(e) || !memsrc.Pool(st, e) || e.First >= days[0] || len(e.Cats()) > 0 || e.OriginSpace != "" {
			continue
		}
		pool = append(pool, e)
	}
	sort.Slice(pool, func(i, j int) bool {
		a, b := pool[i], pool[j]
		sa, sb := export.Usable(st, a), export.Usable(st, b)
		if sa != sb {
			return sa
		}
		if ra, rb := memsrc.Rank(a), memsrc.Rank(b); ra != rb {
			return ra > rb
		}
		if a.Last != b.Last {
			return a.Last > b.Last
		}
		return a.ID < b.ID
	})
	lines = lines[:0]
	for _, e := range pool {
		lines = append(lines, "- "+clip(e.Shown(), 200))
		if len([]rune(strings.Join(lines, "\n"))) > budget {
			break
		}
	}
	scMem := fit(lines, budget)

	arms := []struct{ name, mem string }{{ArmOverview, ovMem}, {ArmScatter, scMem}, {ArmNone, ""}}
	trials := twoOrderTrials(qs)
	trialRates := map[string]float64{}
	for _, arm := range arms {
		h.Chars[arm.name] = len([]rune(arm.mem))
		perQ, totalRight, declined, aerr := r.askTrials(ctx, run, arm.mem, len(qs), trials)
		if aerr != nil {
			h.Calls = run.Calls
			return h, aerr
		}
		if declined > 0 {
			h.Notes = append(h.Notes, fmt.Sprintf("%s: the model declined %d batch(es) (counted wrong)", arm.name, declined))
		}
		strictHits := 0
		for _, c := range perQ {
			if c == 2 {
				strictHits++
			}
		}
		h.Hits[arm.name] = strictHits
		h.Rate[arm.name] = float64(strictHits) / float64(len(qs))
		trialRates[arm.name] = float64(totalRight) / float64(len(trials))
	}
	h.Notes = append(h.Notes, fmt.Sprintf("hard filter: scanned %d, dropped %d easy; strict 2/2 (trial rates: overview %.0f%%, scatter %.0f%%, none %.0f%%)",
		scanned, dropped, trialRates[ArmOverview]*100, trialRates[ArmScatter]*100, trialRates[ArmNone]*100))
	h.Calls = run.Calls
	return h, nil
}

// fit joins lines up to n characters.
func fit(lines []string, n int) string {
	var b strings.Builder
	used := 0
	for _, l := range lines {
		k := len([]rune(l)) + 1
		if used+k > n {
			break
		}
		b.WriteString(l + "\n")
		used += k
	}
	return b.String()
}
