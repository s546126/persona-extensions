package overview

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

//go:embed merge.md
var mergePrompt string

//go:embed merge.schema.json
var mergeSchema []byte

// Bounds of the monthly pass (design 2.6): per facet one rescue synthesis, one merge call, and the counter-search
// again for every pattern through the gate.
const (
	rescueMinCount = 4   // a quiet class looked at again when it was seen at least this often
	rescueDays     = 365 // ... with evidence in the year before
	eraDays        = 90  // a project-bound pattern quiet this long is era-closed
	mergeMost      = 60
	mergeMaxTok    = 4000
	driftRetire    = 2 // monthly passes in a row drifting before it is retired
)

type (
	mergePattern struct {
		ID        string `json:"id"`
		Text      string `json:"text"`
		Situation string `json:"situation,omitempty"`
		Choice    string `json:"choice,omitempty"`
		Exception string `json:"exception,omitempty"`
	}
	mergeInput struct {
		Facet    string         `json:"facet"`
		Lang     string         `json:"language"`
		Patterns []mergePattern `json:"patterns"`
	}
	mergeGroup struct {
		Keep   string   `json:"keep"`
		Merge  []string `json:"merge"`
		Text   string   `json:"text"`
		Reason string   `json:"reason"`
	}
	mergeOutput struct {
		Groups []mergeGroup `json:"groups"`
	}
)

// Monthly is L2 for the month (YYYY-MM) ending before end (YYYY-MM-DD, exclusive): the quiet but often-seen classes
// looked at again (the 40-active-day dormancy no longer decides alone whether a principle is used), overlapping
// patterns merged, every pattern through the gate put to its counter-examples again over the ninety days before end,
// and lifecycles settled: drifting two months running → retired; project-bound and quiet for ninety days → era-closed.
func (r *Runner) Monthly(ctx context.Context, st *store.State, s *State, month, end string) (*Run, error) {
	run := &Run{Period: month, At: stamp(r.now()), Counts: map[string]int{}}
	now := stamp(r.now())
	var touched []string
	// 1. Rescue: quiet classes seen often (principle facets).
	for _, f := range PrincipleFacets {
		cands := candidates(st, f, addDays(end, -rescueDays), "", func(e *entry.Entry) bool {
			return e.Status == entry.Dormant && e.Grain == entry.Class && e.Count >= rescueMinCount
		})
		run.Counts["rescue_items_"+string(f)] = len(cands)
		if len(cands) < 3 {
			continue
		}
		ids, err := r.synthesize(ctx, st, s, run, f, cands[:min(len(cands), synthItems)], "", now)
		if err != nil {
			return run, err
		}
		touched = union(touched, ids...)
		run.Counts["rescued"] += len(ids)
	}
	// 2. Merge alike patterns, facet by facet (within the same L1 kind).
	for _, f := range Facets {
		ps := s.Sorted(func(p *Pattern) bool { return p.Facet == string(f) && p.Lifecycle != Retired })
		if len(ps) < 2 {
			continue
		}
		ps = ps[:min(len(ps), mergeMost)]
		in := mergeInput{Facet: string(f), Lang: r.lang()}
		local := map[string]*Pattern{}
		for i, p := range ps {
			id := fmt.Sprintf("p%d", i+1)
			local[id] = p
			in.Patterns = append(in.Patterns, mergePattern{ID: id, Text: clip(p.Text, 200), Situation: clip(p.Situation, 80),
				Choice: clip(p.Choice, 80), Exception: clip(p.Exception, 80)})
		}
		var out mergeOutput
		err := r.call(ctx, run, "merge", model.Medium, mergePrompt, mergeSchema, in, mergeMaxTok, &out)
		if errors.Is(err, model.ErrRefused) {
			run.Counts["merge_refused"]++
			continue
		}
		if err != nil {
			return run, err
		}
		for _, g := range out.Groups {
			keep := local[g.Keep]
			if keep == nil || keep.Lifecycle == Retired {
				continue
			}
			n := 0
			for _, m := range g.Merge {
				x := local[m]
				if x == nil || x == keep || x.Lifecycle == Retired || x.KindOf() != keep.KindOf() {
					continue
				}
				keep.EvidenceIDs = union(keep.EvidenceIDs, x.EvidenceIDs...)
				keep.CounterIDs = union(keep.CounterIDs, x.CounterIDs...)
				keep.CoveredIDs = union(keep.CoveredIDs, x.CoveredIDs...)
				if keep.Exception == "" {
					keep.Exception = x.Exception
				}
				if len(keep.Poles) == 0 && len(x.Poles) > 0 {
					keep.Poles = x.Poles
				}
				if keep.Polarity == "" {
					keep.Polarity = x.Polarity
				}
				if keep.Dimension == "" {
					keep.Dimension = x.Dimension
				}
				if x.Hard {
					keep.Hard = true
				}
				x.Lifecycle, x.Reason, x.UpdatedAt = Retired, "merged into "+keep.ID, now
				touched = union(touched, x.ID)
				n++
			}
			if n == 0 {
				continue
			}
			if t := strings.TrimSpace(g.Text); t != "" {
				keep.Text = t
			}
			keep.UpdatedAt = now
			recount(st, keep, "")
			touched = union(touched, keep.ID)
			run.Counts["merged"] += n
		}
	}
	// 3. Counter-search again for every pattern through the gate, then the month's lifecycle rules.
	var check []string
	for _, p := range s.Patterns {
		if p.Lifecycle == Retired {
			continue
		}
		recount(st, p, "")
		if p.Gate == "" {
			check = append(check, p.ID)
		} else if p.Used() {
			p.Lifecycle, p.Reason = Candidate, "no longer through the gate: "+p.Gate
			touched = union(touched, p.ID)
		}
	}
	slices.Sort(check)
	before := map[string]string{}
	for _, p := range s.Patterns {
		before[p.ID] = p.Lifecycle
	}
	if err := r.counterSearch(ctx, st, s, run, check, end, "", now); err != nil {
		return run, err
	}
	// 4. Global Reconciliation Pass at month end (v0.4.0 §1.4).
	if recIDs, err := r.Reconcile(ctx, st, s, run, "", now); err != nil {
		return run, err
	} else {
		touched = union(touched, recIDs...)
	}
	since := addDays(end, -eraDays)
	for _, p := range s.Patterns {
		if p.Lifecycle == Retired {
			continue
		}
		if p.Lifecycle == Drifting {
			p.DriftMonths++
			if p.DriftMonths >= driftRetire {
				p.Lifecycle, p.Reason = Retired, fmt.Sprintf("drifting %d months running", p.DriftMonths)
			}
		} else {
			p.DriftMonths = 0
		}
		if p.Used() && !p.Rooted && p.LastEvidence < since && projectBound(st, p) {
			p.Lifecycle, p.Reason = EraClosed, "its projects have been quiet for 90 days"
		}
		if before[p.ID] != p.Lifecycle {
			touched = union(touched, p.ID)
		}
	}
	for _, id := range touched {
		if p := s.Patterns[id]; p != nil {
			run.Counts["lifecycle_"+p.Lifecycle]++
		}
	}
	run.Changed = touched
	return run, nil
}

// projectBound: every line the pattern rests on was said about a project (a principle the user holds anywhere is
// not); an entry's own scopes or the entry the line is under count.
func projectBound(st *store.State, p *Pattern) bool {
	n := 0
	for _, id := range liveIDs(st, p.EvidenceIDs) {
		e := st.Entries[id]
		for _, ev := range evidenceOf(st, e, "") {
			n++
			if x := st.Entries[ev.entry]; len(projects(e)) == 0 && (x == nil || len(projects(x)) == 0) {
				return false
			}
		}
	}
	return n > 0
}
