package overview

import (
	"slices"
	"sort"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// ViolationStat summarizes one L1 collaboration contract's (or unabsorbed Fast-Path hot-slot entry's) correction
// recurrences within a date window [from, to], alongside the rate per 100 ask/decision cards (unified design §3.2).
type ViolationStat struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"` // "contract" or "fast_hot"
	Polarity     string   `json:"polarity,omitempty"`
	Hard         bool     `json:"hard,omitempty"`
	Text         string   `json:"text"`
	Recurrences  int      `json:"recurrences"`             // distinct self-correction ref@day events in [from, to]
	Days         []string `json:"days,omitempty"`          // sorted distinct days in [from, to]
	TotalAllTime int      `json:"total_all_time"`          // contract's all-time ViolationCount (distinct days)
	Per100Cards  float64  `json:"per_100_cards,omitempty"` // Recurrences * 100 / cards when cards > 0
}

// ContractViolations computes per-contract correction recurrences in [from, to] (YYYY-MM-DD, inclusive) across all
// non-sensitive used L1 contracts (including their EvidenceIDs, CoveredIDs and member entries) plus any unabsorbed
// Fast-Path hot-slot entries whose fast_hot day falls in [from, to]. Sensitive or restricted-space patterns are
// excluded so no private wording leaks into status reports.
func ContractViolations(st *store.State, s *State, from, to string, cards int) []ViolationStat {
	if st == nil {
		return nil
	}
	absorbed := map[string]bool{}
	var out []ViolationStat

	if s != nil {
		for _, p := range s.Sorted(func(p *Pattern) bool {
			return p.IsContract() && p.Used() && len(p.Categories) == 0 && p.OriginSpace == ""
		}) {
			ids := union(p.EvidenceIDs, p.CoveredIDs...)
			for _, id := range ids {
				absorbed[id] = true
			}
			refs, days := countCorrectionsInRange(st, ids, from, to)
			stat := ViolationStat{
				ID:           p.ID,
				Kind:         KindContract,
				Polarity:     p.Polarity,
				Hard:         p.Hard,
				Text:         p.Text,
				Recurrences:  len(refs),
				Days:         days,
				TotalAllTime: p.ViolationCount,
			}
			if cards > 0 {
				stat.Per100Cards = float64(stat.Recurrences) * 100 / float64(cards)
			}
			out = append(out, stat)
		}
	}

	// Include unabsorbed live Fast-Path hot-slot entries in [from, to] so recent corrections before the next weekly
	// L1 sync are still counted.
	for _, e := range st.Entries {
		if e == nil || !e.Live() || absorbed[e.ID] || len(e.Cats()) > 0 || e.OriginSpace != "" {
			continue
		}
		hotDay := entry.FastHotDay(e)
		if hotDay == "" {
			continue
		}
		if (from != "" && hotDay < from) || (to != "" && hotDay > to) {
			continue
		}
		refs, days := countCorrectionsInRange(st, []string{e.ID}, from, to)
		rec := len(refs)
		if rec == 0 {
			rec = 1
			days = []string{hotDay}
		}
		stat := ViolationStat{
			ID:           e.ID,
			Kind:         "fast_hot",
			Hard:         true,
			Text:         e.Shown(),
			Recurrences:  rec,
			Days:         days,
			TotalAllTime: len(days),
		}
		if cards > 0 {
			stat.Per100Cards = float64(stat.Recurrences) * 100 / float64(cards)
		}
		out = append(out, stat)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Recurrences != out[j].Recurrences {
			return out[i].Recurrences > out[j].Recurrences
		}
		if out[i].TotalAllTime != out[j].TotalAllTime {
			return out[i].TotalAllTime > out[j].TotalAllTime
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// countCorrectionsInRange returns distinct self-correction ref@day keys and sorted distinct days within [from, to].
func countCorrectionsInRange(st *store.State, ids []string, from, to string) (refs []string, days []string) {
	seenEntry := map[string]bool{}
	seenRef := map[string]bool{}
	seenDay := map[string]bool{}
	var walk func(e *entry.Entry, depth int)
	walk = func(e *entry.Entry, depth int) {
		if e == nil || !e.Live() || seenEntry[e.ID] {
			return
		}
		seenEntry[e.ID] = true
		isCorr := (e.Facet == entry.Interaction || e.Facet == entry.Tools) &&
			(strings.EqualFold(e.Type, "correction") || strings.TrimSpace(e.Rejected) != "" || entry.FastHotDay(e) != "")
		if isCorr {
			for _, ev := range e.Evidence {
				if ev.Day == "" || (from != "" && ev.Day < from) || (to != "" && ev.Day > to) {
					continue
				}
				if ev.Reported || (ev.Who != "" && ev.Who != entry.WhoSelf) {
					continue
				}
				switch ev.Role {
				case "assistant", "agent_draft", "bot":
					continue
				}
				key := ev.Ref + "@" + ev.Day
				if !seenRef[key] {
					seenRef[key] = true
					refs = append(refs, key)
				}
				if !seenDay[ev.Day] {
					seenDay[ev.Day] = true
					days = append(days, ev.Day)
				}
			}
		}
		if depth >= 2 {
			return
		}
		for _, mid := range e.Members {
			if m := st.Entries[mid]; m != nil && m.Live() && !slices.Contains(ids, mid) {
				walk(m, depth+1)
			}
		}
	}
	for _, id := range ids {
		walk(st.Entries[id], 0)
	}
	sort.Strings(refs)
	sort.Strings(days)
	return refs, days
}
