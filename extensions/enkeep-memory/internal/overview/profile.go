package overview

import (
	"slices"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/profile"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// OverviewBackedProvider feeds the profile summary (internal/profile) from the overview: the used patterns of a facet
// come first, and for a facet with a general pattern they stand in for the scattered actions, classes and abstracts
// they were made from. A pinned entry, a current focus, what the user is doing now (the recent highlights' source), a categorised entry (it goes to its own sensitive section)
// and the world facet's people and projects are kept as they are; a facet with no used pattern falls back to the
// curated entries (profile.EntriesProvider).
type OverviewBackedProvider struct {
	// Snapshot is the overview's state now (Holder.Snapshot); nil, or a nil state, means curated entries only.
	Snapshot func() *State
}

// Name changes the summary's basis when the patterns are what it is built from.
func (OverviewBackedProvider) Name() string { return "overview" }

// Pattern items rank before every entry (priority 0) and among themselves by spread.
const patternWeight = 1000

func (o OverviewBackedProvider) Items(st *store.State) []profile.CandidateItem {
	base := profile.EntriesProvider{}.Items(st)
	var s *State
	if o.Snapshot != nil {
		s = o.Snapshot()
	}
	if s == nil {
		return base
	}
	var out []profile.CandidateItem
	covered := map[entry.Facet]bool{}
	for _, p := range s.Sorted(func(p *Pattern) bool { return p.Used() }) {
		// A work-method pattern keeps a category only while today's Payload-First rules still find it (an older
		// label on a bookkeeping rule is not the user's finance); ops runbooks are stripped below as before.
		pcats := p.Categories
		if !p.IsOpsRunbook() {
			pcats = effectiveCategories(st, p)
		}
		f := entry.Facet(p.Facet)
		if p.IsProfileCore() {
			f = entry.Persona
		} else if p.IsOpsRunbook() {
			f = entry.Tools
		}
		if !slices.Contains(profile.Facets, f) {
			continue
		}
		// As the entries provider: something said in a space, with no category, stays out of the general profile
		// unless it is about working with the AI or the user.
		if len(pcats) == 0 && p.OriginSpace != "" && f != entry.Interaction && f != entry.Persona {
			continue
		}
		text := strings.TrimSpace(p.Text) + suffixOf(p)
		weight := patternWeight + Score(p)*10 + p.ScenesCount*p.SourcesCount
		if p.IsProfileCore() {
			weight += 2000
		}
		var itemCats []string
		itemSpace := p.OriginSpace
		if p.IsOpsRunbook() {
			// ops_runbook never enters profile:cat:* sensitive sections; it stays in Tools / on-demand recall.
			itemCats = nil
			itemSpace = ""
		} else {
			itemCats = pcats
		}
		out = append(out, profile.CandidateItem{
			ID: p.ID, Facet: f, Text: text, Last: p.LastEvidence, Priority: 0, Pattern: true,
			Weight:     weight,
			Categories: itemCats, OriginSpace: itemSpace, Subjects: slices.Clone(p.Subjects),
		})
		if len(pcats) == 0 && !p.IsOpsRunbook() {
			covered[f] = true
		}
	}
	for _, it := range base {
		if covered[it.Facet] && it.Facet != entry.World && !it.Pinned && !it.IsFocus && !it.Own && len(it.Categories) == 0 {
			continue
		}
		out = append(out, it)
	}
	return out
}
