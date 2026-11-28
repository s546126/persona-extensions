// Package profile builds and persists the user's "who I am" profile summary across all six facets
// (interaction, tools, judgement, workflow, world+people facts, persona+recent).
//
// Design invariants:
//   - All six facets are first-class and represented in order; an empty facet stays empty instead of using generic defaults.
//   - Blank new users start directly from their own records or onboarding answers without external profile seeds.
//   - General summary sections and overview include ONLY non-sensitive general entries (no health/finance/relationships/emotion/legal_id,
//     no restricted domain spaces, no person card evaluations or inferences, and no life-domain entries when onboarding.use == "work").
//   - Sensitive stable entries are grouped separately per category with explicit category and origin_space metadata so the hub's
//     scene filter controls where they may appear.
//   - The input layer is pluggable (InputProvider): today EntriesProvider reads curated entries from store.State; future
//     periodic cross-event overview pattern entries can be plugged in via InputProvider without changing the UI or memory.entries export.
//   - Persisted in its own file (profile-summary.json) beside state.json; never mutates state.json.
package profile

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/export"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/people"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/recall"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

const (
	FileName           = "profile-summary.json"
	CurrentVersion     = 3 // 3: sensitive sections only the user's own, world led by the user's own facts, no one-off misreads
	ModeDeterministic  = "deterministic"
	ModeModel          = "model"
	MaxPointsPerFacet  = 8
	MaxRecent          = 6
	RecentDays         = 30
	MaxSensitivePoints = 6
	MaxSubjects        = 12
	// MaxWorldPeople caps how many people-card lines the world section may carry after the user's own projects,
	// working relations and world facts, at most one per person, so one colleague never fills the section.
	MaxWorldPeople = 2
)

// Facets are the six profile facets in display order.
var Facets = []entry.Facet{
	entry.Interaction,
	entry.Tools,
	entry.Judgement,
	entry.Workflow,
	entry.World,
	entry.Persona,
}

// CandidateItem is one normalized input item fed into the profile summary builder.
// Replacing InputProvider lets the summary switch from raw curated entries to periodic-overview
// pattern entries later without changing the subpage or memory.entries export.
type CandidateItem struct {
	ID          string
	Facet       entry.Facet
	Text        string
	Last        string
	Pinned      bool
	Priority    int      // lower is higher priority (e.g. 0=abstract/pattern, 1=self_report, 2=class, 3=action)
	Weight      int      // higher ranks first within the same priority
	Categories  []string // non-empty => strictly isolated into Summary.Sensitive
	OriginSpace string
	Subjects    []string
	IsFocus     bool // current_focus / active recent highlight
	// Own: about what the user themself is doing or keeping an eye on (a ⑥ entry, or a ⑤ project line in the user's
	// own words with no one else's). Only these make the recent highlights: a colleague's meeting or rollout does not.
	// A pattern is a way of doing things, not news: never Own.
	Own bool
	// Pattern: a cross-event pattern of the periodic overview (internal/overview), not a single entry.
	Pattern bool
	// Person is the contact a people-card item (entry.People) is about; "" for the user's own items. Such items
	// never enter the sensitive sections (someone else's health or money is not the user's profile) and fill the world
	// section only after the user's own lines, one per person, at most MaxWorldPeople.
	Person string
	// Others are the other people a world relation line names (its target, when a person): they count towards the
	// one-line-per-person cap like Person.
	Others []string
}

// InputProvider supplies normalized candidate items for building a profile Summary.
type InputProvider interface {
	Name() string
	Items(st *store.State) []CandidateItem
}

// EntriesProvider is the default InputProvider backed by curated entries in store.State.
type EntriesProvider struct{}

func (EntriesProvider) Name() string { return "entries" }

func (EntriesProvider) Items(st *store.State) []CandidateItem {
	if st == nil {
		return nil
	}
	contactScore := map[string]int{}
	for _, c := range people.Ranked(st.Contacts) {
		contactScore[c.Person] = c.Score
	}
	var out []CandidateItem
	for _, e := range st.Entries {
		if !eligible(st, e) {
			continue
		}
		cats := entryCategories(e)
		// Someone else's sensitive facts (a colleague's loan, a friend's diet) are not the user's profile: they stay in
		// the person's card and the sensitive review list, never in profile:cat:* nor in the general sections.
		if e.Facet == entry.People && len(cats) > 0 {
			continue
		}
		// Entries from a non-empty domain space outside interaction/persona stay out of general profile unless tagged sensitive.
		if len(cats) == 0 && e.OriginSpace != "" && e.Facet != entry.Interaction && e.Facet != entry.Persona {
			continue
		}
		targetFacet := e.Facet
		if targetFacet == entry.People {
			targetFacet = entry.World
		}
		if len(cats) == 0 && !slices.Contains(Facets, targetFacet) {
			continue
		}
		weight := e.Count * max(1, len(e.Kinds))
		person, others := "", []string(nil)
		if e.Facet == entry.People {
			// The contact's score orders people lines among themselves; they come after the user's own world lines.
			weight += contactScore[e.Person] / 10
			if person = strings.TrimSpace(e.Person); person == "" {
				person = e.ID
			}
		} else if e.Facet == entry.World && isPersonRef(e.Subject) {
			// A relation between other people ("A 和 B 合作", "A 负责 X") is about them, not the user's own world:
			// it is capped like a people card, counting both ends.
			person = strings.TrimSpace(e.Subject)
			if isPersonRef(e.Target) {
				others = []string{strings.TrimSpace(e.Target)}
			}
		}
		originSpace := e.OriginSpace
		if entry.IsOpsRunbookEntry(e) || (len(cats) == 0 && entry.IsWorkMethodFacet(e.Facet)) {
			originSpace = ""
		}
		out = append(out, CandidateItem{
			ID:          e.ID,
			Facet:       targetFacet,
			Text:        formatPointText(st, e),
			Last:        e.Last,
			Pinned:      e.Pinned,
			Priority:    grainRank(e),
			Weight:      weight,
			Categories:  cats,
			OriginSpace: originSpace,
			Subjects:    collectEntrySubjects(st, e),
			IsFocus:     e.Type == "current_focus",
			Own:         own(e),
			Person:      person,
			Others:      others,
		})
	}
	return out
}

// Summary is the persisted user profile summary.
type Summary struct {
	Version      int                `json:"version"`
	GeneratedAt  string             `json:"generated_at"`
	Day          string             `json:"day,omitempty"`
	Lang         string             `json:"lang,omitempty"`
	Mode         string             `json:"mode"`
	InputSource  string             `json:"input_source,omitempty"`
	BasisHash    string             `json:"basis_hash"`
	SourceCount  int                `json:"source_count"`
	CoveredCount int                `json:"covered_count"`
	Overview     string             `json:"overview,omitempty"`
	Recent       []string           `json:"recent,omitempty"`
	Sections     []Section          `json:"sections"`
	Sensitive    []SensitiveSection `json:"sensitive,omitempty"`
}

// Section is one of the six facets in the general (non-sensitive) profile summary.
type Section struct {
	Facet       entry.Facet `json:"facet"`
	Group       string      `json:"group"` // "smooth" | "understanding"
	Summary     string      `json:"summary,omitempty"`
	Points      []Point     `json:"points,omitempty"`
	SourceCount int         `json:"source_count"`
	Subjects    []string    `json:"subjects,omitempty"`
}

// Point is one grounded bullet inside a section.
type Point struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Last    string `json:"last,omitempty"`
	Pattern bool   `json:"pattern,omitempty"` // a cross-event pattern (CandidateItem.Pattern)
}

// SensitiveSection groups stable entries of one sensitive category apart from the general profile.
type SensitiveSection struct {
	Category    string   `json:"category"`
	OriginSpace string   `json:"origin_space,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Points      []Point  `json:"points,omitempty"`
	SourceCount int      `json:"source_count"`
	Subjects    []string `json:"subjects,omitempty"`
}

// Options configures Refresh.
type Options struct {
	Provider  InputProvider // nil defaults to EntriesProvider{}
	Model     model.Model
	Tier      model.Tier
	Lang      string
	Day       string
	Now       func() time.Time
	Logf      func(format string, args ...any)
	SkipModel bool
	Force     bool
}

func isChinese(lang string) bool {
	l := strings.ToLower(strings.TrimSpace(lang))
	return l == "" || strings.HasPrefix(l, "zh")
}

func groupOf(f entry.Facet) string {
	switch f {
	case entry.Interaction, entry.Tools, entry.Judgement, entry.Workflow:
		return "smooth"
	default:
		return "understanding"
	}
}

// entryCategories returns the sensitive categories of e using state.json's persisted categories as the single source
// of truth (v0.4.0 §1.6):
//   - Ops runbook entries (entry.IsOpsRunbookEntry) NEVER enter profile:cat:* sensitive sections.
//   - Never re-runs entry.PayloadCategories from scratch to resurrect categories already cleared by Phase 1/2.2.
//   - Work-methodology facets (interaction, tools, workflow, judgement) accept ONLY persisted ByUser categories or
//     persisted ByModel categories still corroborated by entry.PayloadCategories (never OriginSpace or zero-tag items).
//   - Non-work facets (persona, world, people) retain persisted Categories, legacy Sensitive, and SpaceDefaults.
func entryCategories(e *entry.Entry) []string {
	if e == nil || entry.IsOpsRunbookEntry(e) {
		return nil
	}
	var cats []string
	add := func(c string) {
		if cn := entry.CategoryOf(c); cn != "" && !slices.Contains(cats, cn) && !slices.Contains(e.CatsRemoved, cn) {
			cats = append(cats, cn)
		}
	}
	workMethod := entry.IsWorkMethodFacet(e.Facet)
	var ownPayload []string
	payloadChecked := false
	for _, c := range e.Categories {
		cn := entry.CategoryOf(c.Name)
		if cn == "" {
			continue
		}
		if c.By == entry.ByUser {
			add(cn)
		} else if !workMethod {
			add(cn)
		} else if c.By == entry.ByModel {
			if !payloadChecked {
				ownPayload = entry.PayloadCategories(e.Facet, e.PayloadTexts()...)
				payloadChecked = true
			}
			if slices.Contains(ownPayload, cn) {
				add(cn)
			}
		}
	}
	if !workMethod {
		if cn := entry.CategoryOf(e.Sensitive); cn != "" {
			add(cn)
		}
		for _, c := range entry.SpaceDefaults(e.OriginSpace) {
			add(c)
		}
	}
	sort.Strings(cats)
	return cats
}

// isS3S4Only reports whether e has evidence and every piece comes solely from S3/S4 agent/tool sessions.
func isS3S4Only(e *entry.Entry) bool {
	if e == nil || entry.IsTrueUserOwned(e) || len(e.Evidence) == 0 {
		return false
	}
	for _, ev := range e.Evidence {
		if ev.Source != entry.SrcAgent && ev.Source != entry.SrcTools {
			return false
		}
	}
	return true
}

// isPersonRef says whether a relation end is someone else's contact ("person:<id>"), not the user.
func isPersonRef(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "person:") && s != "person:me"
}

// ownTypes are the ⑥ types that say what the user is doing or keeping an eye on now (the prompt's keys and the
// class names they are filed under).
var ownTypes = []string{"current_work", "current_focus", "goal", "在做的事", "目标", "关注"}

// own says whether e is about what the user themself is doing or watching lately: a ⑥ entry of a doing or focus type,
// not a reading. A ⑤ line is never: on a live history it is a repository's status, a decision on a rule or someone
// else's commitment far more often than the user's own doing, even when it rests on the user's words.
func own(e *entry.Entry) bool {
	return e.Facet == entry.Persona && !e.Inferred && slices.Contains(ownTypes, strings.ToLower(strings.TrimSpace(e.Type)))
}

// eligible says whether an entry may enter the profile summary at all (general or sensitive).
// Excludes S6 assistant notes, ephemeral status/meeting chatter, and S3/S4-only persona hallucinations (v0.4.0 §1.2/§1.3).
func eligible(st *store.State, e *entry.Entry) bool {
	if e == nil || !e.Live() || !export.Stable(e) {
		return false
	}
	if entry.IsAssistantNote(e.Origin) || entry.IsEphemeral(e) {
		return false
	}
	if e.Facet == entry.Persona && isS3S4Only(e) {
		return false
	}
	// LOW_CONFIDENCE extractions and single one-off persona sightings (a vent read as a life fact) never describe the
	// user (entry.IsUnreliableOneOff).
	if entry.IsUnreliableOneOff(e) {
		return false
	}
	if e.Origin == entry.Seed && !entry.On(st.Switches, entry.K21Seeds) {
		return false
	}
	if e.Domain == entry.Life && st.Onboarding != nil && st.Onboarding.Use == entry.Work {
		return false
	}
	// Never include person card evaluations or inferences in the profile summary.
	if e.Facet == entry.People && (e.Part == entry.PartUserEval || e.Part == entry.PartInferred || e.Inferred) {
		return false
	}
	if strings.TrimSpace(recall.Line(e)) == "" {
		return false
	}
	return true
}

func grainRank(e *entry.Entry) int {
	if e.Origin == entry.SelfReport || e.Origin == entry.FromOnboarding {
		return 1
	}
	switch e.Grain {
	case entry.Abstract:
		return 0
	case entry.Class:
		return 2
	default:
		return 3
	}
}

func sortCandidates(items []CandidateItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Pinned != items[j].Pinned {
			return items[i].Pinned
		}
		if items[i].Priority != items[j].Priority {
			return items[i].Priority < items[j].Priority
		}
		if items[i].Weight != items[j].Weight {
			return items[i].Weight > items[j].Weight
		}
		if items[i].Last != items[j].Last {
			return items[i].Last > items[j].Last
		}
		return items[i].ID < items[j].ID
	})
}

func cleanLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func contactName(st *store.State, personID string) string {
	if personID == "" || st == nil {
		return ""
	}
	if c := st.Contacts[personID]; c != nil && c.Manual != "removed" && strings.TrimSpace(c.Name) != "" {
		return strings.TrimSpace(c.Name)
	}
	return ""
}

func formatPointText(st *store.State, e *entry.Entry) string {
	txt := cleanLine(recall.Line(e))
	if e.Facet == entry.People {
		if name := contactName(st, e.Person); name != "" && !strings.Contains(txt, name) {
			txt = name + "：" + txt
		}
	}
	return clipRunes(txt, 160)
}

func collectEntrySubjects(st *store.State, e *entry.Entry) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		for _, p := range []string{"project:", "person:", "system:", "object:"} {
			s = strings.TrimPrefix(s, p)
		}
		s = strings.TrimSpace(s)
		if s == "" || len(out) >= MaxSubjects || slices.Contains(out, s) {
			return
		}
		out = append(out, clipRunes(s, 40))
	}
	if name := contactName(st, e.Person); name != "" {
		add(name)
	}
	add(e.Object)
	for _, sc := range e.Scopes {
		if sc.Kind != entry.ScopeGlobal {
			add(sc.Name)
		}
	}
	return out
}

func mergeSubjects(items []CandidateItem) []string {
	var out []string
	for _, it := range items {
		for _, s := range it.Subjects {
			s = strings.TrimSpace(s)
			if s == "" || len(out) >= MaxSubjects || slices.Contains(out, s) {
				continue
			}
			out = append(out, clipRunes(s, 40))
		}
	}
	return out
}

func joinSummary(points []Point, zh bool, maxItems int) string {
	if len(points) == 0 {
		return ""
	}
	sep := "; "
	if zh {
		sep = "；"
	}
	n := min(len(points), maxItems)
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		p := strings.TrimRight(points[i].Text, "。.;；")
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	out := strings.Join(parts, sep)
	if zh {
		return out + "。"
	}
	return out + "."
}

func facetShortName(f entry.Facet, zh bool) string {
	if zh {
		switch f {
		case entry.Interaction:
			return "和 AI 协作"
		case entry.Tools:
			return "工具与环境"
		case entry.Judgement:
			return "默认判断"
		case entry.Workflow:
			return "做事方式"
		case entry.World:
			return "人和事"
		case entry.Persona:
			return "个人与近况"
		}
	}
	switch f {
	case entry.Interaction:
		return "Working with AI"
	case entry.Tools:
		return "Tools & setup"
	case entry.Judgement:
		return "Default judgements"
	case entry.Workflow:
		return "Ways of working"
	case entry.World:
		return "Projects & people"
	case entry.Persona:
		return "About you & recent"
	}
	return string(f)
}

// Build deterministically compiles a six-facet profile summary from st using the default EntriesProvider.
// It never calls a model and never mutates st.
func Build(st *store.State, now time.Time, lang string) *Summary {
	return BuildWith(st, EntriesProvider{}, now, lang)
}

// BuildWith deterministically compiles a six-facet profile summary from any InputProvider.
func BuildWith(st *store.State, src InputProvider, now time.Time, lang string) *Summary {
	if src == nil {
		src = EntriesProvider{}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	zh := isChinese(lang)
	sum := &Summary{
		Version:     CurrentVersion,
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Lang:        lang,
		Mode:        ModeDeterministic,
		InputSource: src.Name(),
		Sections:    make([]Section, 0, len(Facets)),
	}

	items := src.Items(st)
	if len(items) == 0 {
		for _, f := range Facets {
			sum.Sections = append(sum.Sections, Section{Facet: f, Group: groupOf(f)})
		}
		sum.BasisHash = computeBasisHash(lang, src.Name(), nil, nil)
		return sum
	}

	generalByFacet := map[entry.Facet][]CandidateItem{}
	sensitiveByCat := map[string][]CandidateItem{}
	var allUsed []CandidateItem

	for _, it := range items {
		it.Text = clipRunes(cleanLine(it.Text), 160)
		if it.Text == "" {
			continue
		}
		// Normalize categories: any sensitive category goes strictly to Sensitive, never into general sections or overview.
		var cats []string
		for _, c := range it.Categories {
			if cn := entry.CategoryOf(c); cn != "" && !slices.Contains(cats, cn) {
				cats = append(cats, cn)
			}
		}
		if !entry.IsWorkMethodFacet(it.Facet) {
			for _, c := range entry.SpaceDefaults(it.OriginSpace) {
				if !slices.Contains(cats, c) {
					cats = append(cats, c)
				}
			}
		}
		sort.Strings(cats)
		it.Categories = cats

		if len(cats) > 0 {
			if it.Person != "" {
				continue // someone else's sensitive facts never enter the user's sensitive sections
			}
			for _, cat := range cats {
				sensitiveByCat[cat] = append(sensitiveByCat[cat], it)
			}
			allUsed = append(allUsed, it)
			continue
		}
		if !slices.Contains(Facets, it.Facet) {
			continue
		}
		generalByFacet[it.Facet] = append(generalByFacet[it.Facet], it)
		allUsed = append(allUsed, it)
	}

	// Build each of the six sections in fixed order.
	for _, f := range Facets {
		es := generalByFacet[f]
		sortCandidates(es)
		if f == entry.World {
			// The user's own projects, working relations and world facts first; people cards only fill what is left.
			sort.SliceStable(es, func(i, j int) bool { return es[i].Person == "" && es[j].Person != "" })
		}

		seen := map[string]bool{}
		seenPerson := map[string]bool{}
		people := 0
		var pts []Point
		var picked []CandidateItem
		for _, it := range es {
			key := norm.Q(it.Text)
			if key == "" || seen[key] {
				continue
			}
			if it.Person != "" {
				names := append([]string{it.Person}, it.Others...)
				if people >= MaxWorldPeople || slices.ContainsFunc(names, func(n string) bool { return seenPerson[n] }) {
					continue
				}
				for _, n := range names {
					seenPerson[n] = true
				}
				people++
			}
			seen[key] = true
			pts = append(pts, Point{ID: it.ID, Text: it.Text, Last: it.Last, Pattern: it.Pattern})
			picked = append(picked, it)
			if len(pts) >= MaxPointsPerFacet {
				break
			}
		}

		sec := Section{
			Facet:       f,
			Group:       groupOf(f),
			Points:      pts,
			SourceCount: len(es),
			Subjects:    mergeSubjects(picked),
			Summary:     joinSummary(pts, zh, 3),
		}
		if len(pts) > 0 {
			sum.CoveredCount++
		}
		sum.Sections = append(sum.Sections, sec)
	}

	// Build recent highlights: the user's own current focus and doings active within RecentDays, never someone else's.
	cutoff := now.AddDate(0, 0, -RecentDays).Format("2006-01-02")
	var recentCandidates []CandidateItem
	for _, f := range []entry.Facet{entry.Persona, entry.World, entry.Workflow} {
		for _, it := range generalByFacet[f] {
			if (it.IsFocus || it.Own) && !it.Pattern && (it.IsFocus || it.Last != "" && it.Last >= cutoff) {
				recentCandidates = append(recentCandidates, it)
			}
		}
	}
	sort.SliceStable(recentCandidates, func(i, j int) bool {
		if recentCandidates[i].IsFocus != recentCandidates[j].IsFocus {
			return recentCandidates[i].IsFocus
		}
		if recentCandidates[i].Last != recentCandidates[j].Last {
			return recentCandidates[i].Last > recentCandidates[j].Last
		}
		return recentCandidates[i].ID < recentCandidates[j].ID
	})
	seenRecent := map[string]bool{}
	for _, it := range recentCandidates {
		key := norm.Q(it.Text)
		if key == "" || seenRecent[key] {
			continue
		}
		seenRecent[key] = true
		sum.Recent = append(sum.Recent, it.Text)
		if len(sum.Recent) >= MaxRecent {
			break
		}
	}

	// Build concise deterministic overview from non-empty general sections.
	var ovParts []string
	for _, sec := range sum.Sections {
		if len(sec.Points) == 0 {
			continue
		}
		lead := strings.TrimRight(sec.Points[0].Text, "。.;；")
		if zh {
			ovParts = append(ovParts, facetShortName(sec.Facet, true)+"："+lead)
		} else {
			ovParts = append(ovParts, facetShortName(sec.Facet, false)+": "+lead)
		}
	}
	if len(ovParts) > 0 {
		if zh {
			sum.Overview = strings.Join(ovParts, "；") + "。"
		} else {
			sum.Overview = strings.Join(ovParts, "; ") + "."
		}
	}

	// Build sensitive sections isolated by category.
	for _, cat := range entry.CategoryNames {
		es := sensitiveByCat[cat]
		if len(es) == 0 {
			continue
		}
		sortCandidates(es)
		seen := map[string]bool{}
		var pts []Point
		var picked []CandidateItem
		space := ""
		for _, it := range es {
			if space == "" && it.OriginSpace != "" {
				space = it.OriginSpace
			}
			key := norm.Q(it.Text)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			pts = append(pts, Point{ID: it.ID, Text: it.Text, Last: it.Last})
			picked = append(picked, it)
			if len(pts) >= MaxSensitivePoints {
				break
			}
		}
		if len(pts) > 0 {
			sum.Sensitive = append(sum.Sensitive, SensitiveSection{
				Category:    cat,
				OriginSpace: space,
				Summary:     joinSummary(pts, zh, 3),
				Points:      pts,
				SourceCount: len(es),
				Subjects:    mergeSubjects(picked),
			})
		}
	}

	sum.SourceCount = len(allUsed)
	sum.BasisHash = computeBasisHash(lang, src.Name(), allUsed, sum.Recent)
	return sum
}

func computeBasisHash(lang, srcName string, items []CandidateItem, recent []string) string {
	sorted := slices.Clone(items)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	h := sha1.New()
	fmt.Fprintf(h, "v%d|%s|%s\n", CurrentVersion, lang, srcName)
	for _, it := range sorted {
		fmt.Fprintf(h, "%s|%s|%s|%v|%s|%s\n", it.ID, it.Facet, it.Last, it.Pinned, strings.Join(it.Categories, ","), it.Text)
	}
	for _, r := range recent {
		fmt.Fprintf(h, "r|%s\n", r)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Load reads profile-summary.json from dir. Returns (nil, nil) if the file does not exist.
func Load(dir string) (*Summary, error) {
	if dir == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Summary
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Save atomically writes s to profile-summary.json in dir.
func Save(dir string, s *Summary) error {
	if dir == "" || s == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)
	tmp, err := os.CreateTemp(dir, ".profile-summary-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Refresh rebuilds the profile summary from st (using o.Provider or EntriesProvider), optionally polishing
// the general sections with a single model call when the input fingerprint changed, and saves profile-summary.json.
func Refresh(ctx context.Context, st *store.State, dir string, o Options) (*Summary, error) {
	nowFn := o.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	base := BuildWith(st, o.Provider, now, o.Lang)
	base.Day = o.Day

	if !o.Force && dir != "" {
		if prev, err := Load(dir); err == nil && prev != nil && prev.BasisHash == base.BasisHash {
			if prev.Mode == ModeModel || o.SkipModel || o.Model == nil {
				return prev, nil
			}
		}
	}

	out := base
	if !o.SkipModel && o.Model != nil && base.CoveredCount > 0 {
		if polished, err := synthesize(ctx, o.Model, o.Tier, base, o.Lang); err == nil && polished != nil {
			out = polished
		} else if err != nil && o.Logf != nil {
			o.Logf("profile summary model synth fell back to deterministic: %v", err)
		}
	}

	if err := Save(dir, out); err != nil {
		return out, err
	}
	return out, nil
}
