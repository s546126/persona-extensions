package overview

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

//go:embed reconcile.md
var reconcilePrompt string

//go:embed reconcile.schema.json
var reconcileSchema []byte

const (
	reconcileMaxPatterns = 65
	reconcileMaxTokens   = 8000
)

type (
	reconcilePatternInput struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		Facet     string `json:"facet"`
		Polarity  string `json:"polarity,omitempty"`
		Dimension string `json:"dimension,omitempty"`
		Hard      bool   `json:"hard,omitempty"`
		Text      string `json:"text"`
		Situation string `json:"situation,omitempty"`
		Choice    string `json:"choice,omitempty"`
		Exception string `json:"exception,omitempty"`
		Poles     []Pole `json:"poles,omitempty"`
	}
	reconcileInput struct {
		Lang     string                  `json:"language"`
		Patterns []reconcilePatternInput `json:"patterns"`
	}
	reconcileMerge struct {
		Keep      string   `json:"keep"`
		Merge     []string `json:"merge"`
		Text      string   `json:"text"`
		Exception string   `json:"exception,omitempty"`
		Reason    string   `json:"reason"`
	}
	reconcileTension struct {
		Keep   string   `json:"keep"`
		Merge  []string `json:"merge"`
		Text   string   `json:"text"`
		Poles  []Pole   `json:"poles"`
		Reason string   `json:"reason"`
	}
	reconcileReclassify struct {
		ID         string `json:"id"`
		TargetKind string `json:"target_kind"` // ops_runbook | contract
		Reason     string `json:"reason"`
	}
	reconcileOutput struct {
		Merges     []reconcileMerge      `json:"merges"`
		Tensions   []reconcileTension    `json:"tensions"`
		Reclassify []reconcileReclassify `json:"reclassify"`
	}
)

// Reconcile runs one global Sonnet reconciliation pass over all used principles and universal contracts in s
// (v0.4.0 §1.4 Stage 2):
//  1. Deterministic proper-noun/ops guard routes any lingering ops-bound pattern to KindOpsRunbook;
//  2. Single Sonnet call merges synonymous duplicates, collapses opposing tensions into a single pattern with Poles,
//     and reclassifies misplaced ops/contract items.
//
// Returns the ids of patterns modified or retired.
func (r *Runner) Reconcile(ctx context.Context, st *store.State, s *State, run *Run, cutoff, now string) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	if run == nil {
		run = &Run{Counts: map[string]int{}}
	}
	if run.Counts == nil {
		run.Counts = map[string]int{}
	}
	var touched []string
	// Stage 0: deterministic proper-noun / ops-runbook diversion before calling the model.
	for _, p := range s.Sorted(func(p *Pattern) bool { return p.Used() && (p.IsPrinciple() || p.IsContract()) }) {
		if patternIsOpsRunbook(p) {
			p.Kind = KindOpsRunbook
			p.Hard = false
			p.Reason = "ops_runbook: proper-noun/tool guard"
			p.UpdatedAt = now
			recount(st, p, cutoff)
			touched = union(touched, p.ID)
			run.Counts["reconcile_ops_diverted"]++
		}
	}

	ps := s.Sorted(func(p *Pattern) bool {
		return p.Used() && (p.IsPrinciple() || p.IsContract()) && len(p.Categories) == 0 && p.OriginSpace == ""
	})
	if len(ps) < 2 {
		return touched, nil
	}
	if len(ps) > reconcileMaxPatterns {
		ps = ps[:reconcileMaxPatterns]
	}

	in := reconcileInput{Lang: r.lang()}
	local := map[string]*Pattern{}
	for i, p := range ps {
		pid := fmt.Sprintf("p%d", i+1)
		local[pid] = p
		in.Patterns = append(in.Patterns, reconcilePatternInput{
			ID:        pid,
			Kind:      p.KindOf(),
			Facet:     p.Facet,
			Polarity:  p.Polarity,
			Dimension: p.Dimension,
			Hard:      p.Hard,
			Text:      clip(p.Text, 200),
			Situation: clip(p.Situation, 80),
			Choice:    clip(p.Choice, 80),
			Exception: clip(p.Exception, 100),
			Poles:     p.Poles,
		})
	}

	var out reconcileOutput
	err := r.call(ctx, run, "reconcile", model.Medium, reconcilePrompt, reconcileSchema, in, reconcileMaxTokens, &out)
	if errors.Is(err, model.ErrRefused) {
		run.Counts["reconcile_refused"]++
		return touched, nil
	}
	if err != nil {
		return touched, err
	}

	mergeInto := func(keep, x *Pattern, reason string) {
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
		if x.Hard && keep.IsContract() {
			keep.Hard = true
		}
		x.Lifecycle, x.Reason, x.UpdatedAt = Retired, reason, now
		touched = union(touched, x.ID)
	}

	// 1. Synonymous merges.
	for _, g := range out.Merges {
		keep := local[g.Keep]
		if keep == nil || keep.Lifecycle == Retired {
			continue
		}
		n := 0
		for _, mid := range g.Merge {
			x := local[mid]
			if x == nil || x == keep || x.Lifecycle == Retired {
				continue
			}
			mergeInto(keep, x, "reconciled: merged into "+keep.ID)
			n++
		}
		if n == 0 {
			continue
		}
		if t := strings.TrimSpace(g.Text); t != "" {
			keep.Text = t
		}
		if ex := strings.TrimSpace(g.Exception); ex != "" {
			keep.Exception = ex
		}
		if rsn := strings.TrimSpace(g.Reason); rsn != "" {
			keep.Reason = rsn
		}
		if patternIsOpsRunbook(keep) {
			keep.Kind = KindOpsRunbook
			keep.Hard = false
		}
		keep.UpdatedAt = now
		recount(st, keep, cutoff)
		if keep.Gate == "" {
			if keep.Exception != "" || len(keep.Poles) >= 2 {
				keep.Lifecycle = Scoped
			} else {
				keep.Lifecycle = Core
			}
		}
		touched = union(touched, keep.ID)
		run.Counts["reconcile_merged"] += n
	}

	// 2. Opposing tension resolution into one structured pattern with Poles.
	// Supports both collapsing 2+ competing patterns (including cross-kind principle+contract pairs) and upgrading a
	// single principle that already carries a significant two-pole tension or one-sided exception into structured Poles.
	for _, g := range out.Tensions {
		keep := local[g.Keep]
		if keep == nil || keep.Lifecycle == Retired {
			continue
		}
		poles := cleanPoles(g.Poles)
		if len(poles) < 2 {
			continue
		}
		for _, mid := range g.Merge {
			x := local[mid]
			if x == nil || x == keep || x.Lifecycle == Retired {
				continue
			}
			mergeInto(keep, x, "reconciled: tension resolved into "+keep.ID)
		}
		if t := strings.TrimSpace(g.Text); t != "" {
			keep.Text = t
		}
		keep.Poles = poles
		keep.Exception = "" // Poles replace one-sided exception
		if rsn := strings.TrimSpace(g.Reason); rsn != "" {
			keep.Reason = rsn
		}
		if patternIsOpsRunbook(keep) {
			keep.Kind = KindOpsRunbook
			keep.Hard = false
		}
		keep.UpdatedAt = now
		recount(st, keep, cutoff)
		if keep.Gate == "" {
			keep.Lifecycle = Scoped
		}
		touched = union(touched, keep.ID)
		run.Counts["reconcile_tensions"]++
	}

	// 3. Reclassify misplaced ops/contract items.
	for _, rc := range out.Reclassify {
		p := local[rc.ID]
		if p == nil || p.Lifecycle == Retired {
			continue
		}
		switch rc.TargetKind {
		case KindOpsRunbook:
			p.Kind = KindOpsRunbook
			p.Hard = false
		case KindContract:
			if patternIsOpsRunbook(p) {
				p.Kind = KindOpsRunbook
				p.Hard = false
			} else {
				p.Kind = KindContract
			}
		default:
			continue
		}
		if rsn := strings.TrimSpace(rc.Reason); rsn != "" {
			p.Reason = "reconciled: " + rsn
		}
		p.UpdatedAt = now
		recount(st, p, cutoff)
		if p.Gate == "" {
			if p.Exception != "" || len(p.Poles) >= 2 {
				p.Lifecycle = Scoped
			} else {
				p.Lifecycle = Core
			}
		}
		touched = union(touched, p.ID)
		run.Counts["reconcile_reclassified"]++
	}

	_ = json.RawMessage(nil)
	return touched, nil
}
