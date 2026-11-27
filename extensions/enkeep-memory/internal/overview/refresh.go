package overview

import (
	"slices"
	"sort"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// Refresh is L3: on a new snapshot of the curated state, every pattern is counted again from what is still there, with
// no model. A used pattern whose evidence the user marked wrong, deleted or had forgotten no longer passes the gate:
// it leaves the exits at once (a candidate, the lifecycle it had kept in Withdrawn), and comes back if the evidence
// does (restored). A pattern left with no evidence is retired. It says whether anything changed; a snapshot that
// changes nothing for the patterns writes nothing.
func Refresh(st *store.State, s *State, now time.Time) bool {
	changed := false
	var dead []string
	for _, p := range s.Patterns {
		for _, id := range append(slices.Clone(p.EvidenceIDs), p.CounterIDs...) {
			if !usable(st, st.Entries[id]) && !slices.Contains(dead, id) {
				dead = append(dead, id)
			}
		}
	}
	sort.Strings(dead)
	for _, p := range s.Sorted(nil) {
		if p.Lifecycle == Retired {
			continue
		}
		before := *p
		before.EvidenceIDs = slices.Clone(p.EvidenceIDs)
		before.CounterIDs = slices.Clone(p.CounterIDs)
		recount(st, p, "")
		switch {
		case gone(st, p):
			p.Lifecycle, p.Reason, p.Withdrawn = Retired, "its evidence is gone", ""
		case p.Used() && p.Gate != "":
			p.Withdrawn, p.Lifecycle, p.Reason = p.Lifecycle, Candidate, "evidence withdrawn: "+p.Gate
		case p.Withdrawn != "" && p.Gate == "" && p.Lifecycle == Candidate:
			p.Lifecycle, p.Withdrawn, p.Reason = p.Withdrawn, "", "evidence restored"
		}
		if !samePattern(&before, p) {
			p.UpdatedAt = stamp(now)
			changed = true
		}
	}
	if !slices.Equal(dead, s.Dead) {
		s.Dead = dead
		changed = true
	}
	if changed {
		s.Refreshed = stamp(now)
	}
	return changed
}

// gone: every entry the pattern rested on is deleted or no longer there (a forgotten source); a mark of wrong is not
// gone, the user may take it back.
func gone(st *store.State, p *Pattern) bool {
	for _, id := range p.EvidenceIDs {
		if e := st.Entries[id]; e != nil && !e.Deleted && e.MergedInto == "" {
			return false
		}
	}
	return true
}

// samePattern compares what Refresh may change.
func samePattern(a, b *Pattern) bool {
	return a.Lifecycle == b.Lifecycle && a.Gate == b.Gate && a.ScenesCount == b.ScenesCount && a.SourcesCount == b.SourcesCount &&
		a.UniqueQuotes == b.UniqueQuotes && a.Kind == b.Kind && a.Dimension == b.Dimension && a.Polarity == b.Polarity &&
		a.Hard == b.Hard && a.ViolationCount == b.ViolationCount && a.Rooted == b.Rooted &&
		slices.Equal(a.EvidenceIDs, b.EvidenceIDs) && slices.Equal(a.CounterIDs, b.CounterIDs) &&
		slices.Equal(a.CoveredIDs, b.CoveredIDs) && slices.Equal(a.Categories, b.Categories) &&
		a.OriginSpace == b.OriginSpace && slices.Equal(a.ActiveWeeks, b.ActiveWeeks) && a.Withdrawn == b.Withdrawn &&
		slices.Equal(a.Subjects, b.Subjects) && a.LastEvidence == b.LastEvidence && a.Perspective == b.Perspective
}
