package overview

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/memsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// The four-perspective projection: every curated entry the overview may use, with its evidence spread over time
// (ISO weeks), projects, people and source kinds, so a pattern's scenes, independent sources and weeks are counted by
// code, never taken from the model.

// The gate's thresholds (design 2.5, line 1). They do not relax with little data: a pattern that misses one stays a
// candidate.
const (
	MinScenes  = 2 // distinct projects or source kinds
	MinSources = 3 // distinct quoted unit·day pieces of evidence
	MinWeeks   = 2 // distinct ISO weeks
)

// Gate failures.
const (
	GateScenes  = "scenes"
	GateSources = "sources"
	GateWeeks   = "weeks"
	GateDefault = "no_default"
	GateGeneric = "holds_for_anyone"
	GateQuotes  = "quotes"
	GateEmpty   = "no_evidence"
)

// evid is one piece of evidence under an entry.
type evid struct {
	entry    string // the curated entry it is under (an action, or the class itself)
	key      string // ref@day: one unit on one day, the independent unit of evidence
	day      string
	group    string // source kind (sourceGroup)
	quoted   bool   // non-empty quote attributed to the user (not reported, not assistant/bot, not another person)
	self     bool   // explicitly WhoSelf && !Reported
	quoteKey string // normalised quote text, to reject identical quotes repeated across entries
}

// isUserQuote reports whether ev carries a usable quote by the user themselves (never reported speech, another person,
// or an assistant/bot turn). Unresolved Who ("") is accepted for legacy/test fixtures unless Role marks it non-user.
func isUserQuote(ev entry.Evidence) bool {
	if strings.TrimSpace(ev.Quote) == "" || ev.Reported {
		return false
	}
	if ev.Who != "" && ev.Who != entry.WhoSelf {
		return false
	}
	switch ev.Role {
	case "assistant", "agent_draft", "bot":
		return false
	}
	return true
}

// contractHardCue mirrors entry.hardCue for cross-entry L1 contract hard-rule detection.
var contractHardCue = regexp.MustCompile(`以后(都|一律|别|不要|不许)|一律|永远不|绝不|再也不|别再|不要再|不许|他妈|我操|傻逼|[？?]{3,}|(?i)\bnever (do|ask)|\bstop (doing|asking)`)

// properNounShellRe catches concrete single-incident hardware/software/external-product proper nouns that should
// not leak into de-shelled universal principles or universal collaboration contracts (v0.4.0 §1.4).
var properNounShellRe = regexp.MustCompile(`(?i)\b(owc|thunderbolt|sonoma|sequoia|ventura|whoop|feishu[-_]cli|discord|slack|telegram|whatsapp|notion|linear|jira|github\s+actions|docker\s+hub|homebrew)\b`)

// hasProperNounOrOps reports whether any of texts names a concrete host, tool, CLI command, API, repository,
// platform-specific workflow mechanism, or single-incident hardware/software shell (v0.4.0 §1.3/§1.4).
func hasProperNounOrOps(texts ...string) bool {
	joined := strings.TrimSpace(strings.Join(texts, " "))
	if joined == "" {
		return false
	}
	if entry.ClassifyOpsRunbook(&entry.Entry{Facet: entry.Interaction, Text: joined}) {
		return true
	}
	return properNounShellRe.MatchString(joined)
}

// patternIsOpsRunbook reports whether p belongs in KindOpsRunbook rather than KindPrinciple or KindContract.
func patternIsOpsRunbook(p *Pattern) bool {
	if p == nil {
		return false
	}
	if p.Kind == KindOpsRunbook || p.Facet == string(entry.Tools) {
		return true
	}
	if p.Kind == KindProfileCore {
		return false
	}
	return hasProperNounOrOps(p.Text, p.Situation, p.Choice, p.Exception)
}

// isS3S4Only reports whether e has evidence and every piece of evidence comes solely from S3/S4 agent/tool sessions
// (never T0 self-report/onboarding or T1 direct user chat/card/todo/message sources).
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

// sourceGroup is the kind of source a code belongs to: the user's chats with the assistant, cards and to-dos, agent
// sessions, connected message sources, the user's own notes, the knowledge base, recordings.
func sourceGroup(code string) string {
	switch code {
	case entry.SrcAssistant:
		return "chat"
	case entry.SrcAsk, entry.SrcTodo, entry.SrcLedger:
		return "card"
	case entry.SrcAgent, entry.SrcTools:
		return "agent"
	case entry.SrcChat, entry.SrcSent:
		return "messages"
	case entry.SrcMemory, entry.SrcAbout:
		return "self"
	case entry.SrcKB:
		return "kb"
	case entry.SrcRecording:
		return "recording"
	}
	return strings.ToLower(code)
}

// facetOf is the overview facet an entry feeds: its own, or World for a card's fact or commitment; "" for none.
func facetOf(e *entry.Entry) entry.Facet {
	if e.Facet == entry.People {
		if e.Part == entry.PartFact || e.Part == entry.PartCommitment {
			return entry.World
		}
		return ""
	}
	if slices.Contains(Facets, e.Facet) {
		return e.Facet
	}
	return ""
}

// usable says whether an entry may feed the overview: what memory.entries may answer (memsrc.Pool: live, never a
// card's evaluation or inference, no life content for a "work only" user), and of a facet the overview covers.
func usable(st *store.State, e *entry.Entry) bool {
	return e != nil && memsrc.Pool(st, e) && !e.Inferred && facetOf(e) != ""
}

// projects are the project names an entry is scoped to.
func projects(e *entry.Entry) []string {
	var out []string
	for _, sc := range e.Scopes {
		if sc.Kind == entry.ScopeProject && strings.TrimSpace(sc.Name) != "" && !slices.Contains(out, sc.Name) {
			out = append(out, sc.Name)
		}
	}
	return out
}

// evidenceOf is the evidence under e: its own lines and its live members' (two levels: an abstract's classes' actions),
// only days before cutoff when one is set.
func evidenceOf(st *store.State, e *entry.Entry, cutoff string) []evid {
	var out []evid
	seen := map[string]bool{}
	var walk func(x *entry.Entry, depth int)
	walk = func(x *entry.Entry, depth int) {
		if seen[x.ID] {
			return
		}
		seen[x.ID] = true
		for _, ev := range x.Evidence {
			if ev.Day == "" || cutoff != "" && ev.Day >= cutoff {
				continue
			}
			q := strings.TrimSpace(ev.Quote)
			uq := isUserQuote(ev)
			qk := ""
			if uq {
				qk = norm.Q(q)
			}
			out = append(out, evid{
				entry:    x.ID,
				key:      ev.Ref + "@" + ev.Day,
				day:      ev.Day,
				group:    sourceGroup(ev.Source),
				quoted:   uq,
				self:     uq && ev.Who == entry.WhoSelf,
				quoteKey: qk,
			})
		}
		if depth >= 2 {
			return
		}
		for _, id := range x.Members {
			if m := st.Entries[id]; m != nil && m.Live() {
				walk(m, depth+1)
			}
		}
	}
	walk(e, 0)
	return out
}

// tally is a set of evidence counted for the gate.
type tally struct {
	scenes       []string // "p:<project>" and "s:<source kind>"
	sources      []string // quoted ref@day
	uniqueQuotes []string // distinct normalised user quote texts
	weeks        []string
	groups       []string
	projects     []string
	people       []string
	last         string
	unquoted     int  // entries among the evidence without a quoted line
	hasUserOwned bool // at least one live evidence entry is user-edited, pinned or handwritten
}

// count tallies the evidence of the entries ids, before cutoff.
func count(st *store.State, ids []string, cutoff string) tally {
	var t tally
	add := func(list *[]string, s string) {
		if s != "" && !slices.Contains(*list, s) {
			*list = append(*list, s)
		}
	}
	for _, id := range ids {
		e := st.Entries[id]
		if e == nil || !e.Live() {
			continue
		}
		if isUserOwned(e) {
			t.hasUserOwned = true
		}
		evs := evidenceOf(st, e, cutoff)
		quoted := false
		for _, ev := range evs {
			x := st.Entries[ev.entry]
			if x != nil {
				for _, p := range projects(x) {
					add(&t.scenes, "p:"+p)
					add(&t.projects, p)
				}
				if x.Person != "" {
					add(&t.people, x.Person)
				}
			}
			if !ev.quoted {
				continue
			}
			quoted = true
			add(&t.scenes, "s:"+ev.group)
			add(&t.groups, ev.group)
			add(&t.sources, ev.key)
			add(&t.uniqueQuotes, ev.quoteKey)
			add(&t.weeks, isoWeek(ev.day))
			if ev.day > t.last {
				t.last = ev.day
			}
		}
		for _, p := range projects(e) {
			add(&t.scenes, "p:"+p)
			add(&t.projects, p)
		}
		if e.Person != "" {
			add(&t.people, e.Person)
		}
		if !quoted {
			t.unquoted++
		}
	}
	sort.Strings(t.weeks)
	return t
}

// crossScene is true only when the evidence spans at least MinScenes distinct projects OR at least MinScenes distinct
// source kinds. One project seen through one source kind is a single scene, never two.
func (t tally) crossScene() bool {
	return len(t.projects) >= MinScenes || len(t.groups) >= MinScenes
}

// sceneCount is the effective scene spread: 0 when empty, 1 when confined to a single project and single source kind,
// and len(projects)+len(groups) once it truly spans multiple projects or multiple source kinds.
func (t tally) sceneCount() int {
	if t.crossScene() {
		return len(t.projects) + len(t.groups)
	}
	if len(t.projects) > 0 || len(t.groups) > 0 {
		return 1
	}
	return 0
}

// contractHard computes whether a collaboration contract is hard and its violation_count (distinct days the user
// corrected it across all member entries), respecting cutoff.
// A contract is hard only when:
//   - the user corrected it on >=2 distinct days within a 7-day window; OR
//   - the user corrected it on >=3 distinct days overall; OR
//   - a ban/anger cue appears AND is backed by repeated sightings (>=2 days or count>=2) or explicit user ownership/confirmation.
//
// A single-day casual phrasing cue with violation_count==1 stays a regular (non-hard) contract.
func contractHard(st *store.State, ids []string, cutoff string) (hard bool, violations int) {
	seen := map[string]bool{}
	corrDays := map[string]bool{}
	cueHits := 0
	userConfirmed := false
	totalCount := 0
	var walk func(e *entry.Entry, depth int)
	walk = func(e *entry.Entry, depth int) {
		if e == nil || !e.Live() || seen[e.ID] || entry.IsAssistantNote(e.Origin) {
			return
		}
		seen[e.ID] = true
		totalCount += max(e.Count, 1)
		if isUserOwned(e) || lastUserMarkRight(e) {
			userConfirmed = true
		}
		isCorr := (e.Facet == entry.Interaction || e.Facet == entry.Tools) &&
			(strings.EqualFold(e.Type, "correction") || strings.TrimSpace(e.Rejected) != "")
		for _, ev := range e.Evidence {
			if ev.Day == "" || (cutoff != "" && ev.Day >= cutoff) {
				continue
			}
			if ev.Who != entry.WhoSelf || ev.Reported {
				continue
			}
			if isCorr {
				corrDays[ev.Day] = true
				if contractHardCue.MatchString(ev.Quote) {
					cueHits++
				}
			}
		}
		if depth >= 2 {
			return
		}
		for _, mid := range e.Members {
			walk(st.Entries[mid], depth+1)
		}
	}
	for _, id := range ids {
		walk(st.Entries[id], 0)
	}
	var days []string
	for d := range corrDays {
		days = append(days, d)
	}
	sort.Strings(days)
	if len(days) >= 3 {
		hard = true
	}
	for i := 1; !hard && i < len(days); i++ {
		if a, err1 := time.Parse("2006-01-02", days[i-1]); err1 == nil {
			if b, err2 := time.Parse("2006-01-02", days[i]); err2 == nil && b.Sub(a) <= 7*24*time.Hour {
				hard = true
			}
		}
	}
	if !hard && cueHits > 0 && (len(days) >= 2 || userConfirmed) {
		hard = true
	}
	_ = totalCount
	return hard, len(days)
}

func lastUserMarkRight(e *entry.Entry) bool {
	for i := len(e.Verdicts) - 1; i >= 0; i-- {
		if e.Verdicts[i].By == "user" {
			return e.Verdicts[i].Mark == "right"
		}
	}
	return false
}

// inferDimension returns the most frequent Leverage.Dimension among the live entries under ids, or "".
func inferDimension(st *store.State, ids []string) string {
	counts := map[string]int{}
	for _, id := range ids {
		e := st.Entries[id]
		if e == nil || !e.Live() || e.Leverage == nil || e.Leverage.Dimension == "" {
			continue
		}
		counts[e.Leverage.Dimension]++
	}
	best, bestN := "", 0
	for d, n := range counts {
		if n > bestN || (n == bestN && d < best) {
			best, bestN = d, n
		}
	}
	return best
}

// perspective is where the evidence differs most: sources first (what was said in one place and done in another),
// then projects, people, and time.
func (t tally) perspective() string {
	switch {
	case len(t.groups) >= 2:
		return PerspCrossSource
	case len(t.projects) >= 2:
		return PerspProject
	case len(t.people) >= 2:
		return PerspPeople
	}
	return PerspTemporal
}

// gate checks a pattern against the code gate from its tally; "" when it passes.
// Profile core patterns pass once backed by any qualified T0/T1 evidence (enforced at candidate selection).
// Ops runbooks pass with >=1 live evidence entry for on-demand entity recall (never global 4 KB standing).
// Principles enforce strict cross-scene, independent unique-quote, multi-week and distinctiveness gates.
// Collaboration contracts pass immediately when hard or user-owned; otherwise require >=2 independent quotes and
// either cross-scene spread, >=2 weeks, or >=2 merged rule entries.
func gate(p *Pattern, t tally) string {
	if p.IsProfileCore() {
		if len(t.sources) == 0 && !t.hasUserOwned && len(p.EvidenceIDs) == 0 {
			return GateEmpty
		}
		return ""
	}
	if p.IsOpsRunbook() {
		if len(t.sources) == 0 && !t.hasUserOwned && len(p.EvidenceIDs) == 0 {
			return GateEmpty
		}
		return ""
	}
	if p.IsContract() {
		if len(t.sources) == 0 && !t.hasUserOwned {
			return GateEmpty
		}
		if p.Hard || t.hasUserOwned {
			return ""
		}
		if len(t.sources) < 2 || len(t.uniqueQuotes) < 2 {
			return GateSources
		}
		if !t.crossScene() && len(t.weeks) < MinWeeks && len(p.EvidenceIDs) < 2 {
			return GateScenes
		}
		return ""
	}
	if t.hasUserOwned && len(t.sources) >= 2 && !p.HoldsForAnyone {
		return ""
	}
	switch {
	case len(t.sources) == 0:
		return GateEmpty
	case strings.TrimSpace(p.DefaultContrast) == "" || norm.Q(p.DefaultContrast) == norm.Q(p.Text):
		return GateDefault
	case p.HoldsForAnyone:
		return GateGeneric
	case t.unquoted > 0:
		return GateQuotes
	case !t.crossScene():
		return GateScenes
	case len(t.sources) < MinSources || len(t.uniqueQuotes) < MinSources:
		return GateSources
	case len(t.weeks) < MinWeeks:
		return GateWeeks
	}
	return ""
}

// apply writes a tally onto a pattern: the counts, the weeks, the perspective, rooted status and the gate.
func apply(p *Pattern, t tally) {
	p.ScenesCount, p.SourcesCount, p.UniqueQuotes = t.sceneCount(), len(t.sources), len(t.uniqueQuotes)
	p.ActiveWeeks, p.LastEvidence = t.weeks, t.last
	p.Perspective = t.perspective()
	p.Rooted = p.IsProfileCore() || len(t.weeks) >= 4 || (p.IsContract() && p.Hard)
	p.Gate = gate(p, t)
}

// tags are the categories and the source space a pattern carries (v0.4.0 §1.6 Payload-First):
//   - A pattern's own payload (Text/Situation/Choice/Exception) is checked via entry.PayloadCategories.
//   - Only top-level evidence ids are inspected (never recursively walking 2 levels of e.Members), preventing a single
//     sensitive child action under a broad Class from contaminating a universal L1 principle or profile_core.
//   - Work-methodology facets (interaction, tools, workflow, judgement) and ops_runbook patterns inherit a category ONLY
//     when >=50% of valid top-level ids carry an explicit ByUser label for that category, and NEVER inherit OriginSpace.
//   - Non-work facets (persona, world) inherit a category ONLY when >=50% of valid top-level ids carry a ByUser label or
//     their own PayloadCategories match for that category; OriginSpace is inherited ONLY when 100% of valid top-level
//     ids share the same non-empty OriginSpace.
func tags(st *store.State, p *Pattern, ids []string) (cats []string, space string) {
	if p != nil && p.IsOpsRunbook() {
		return nil, ""
	}
	addCat := func(c string) {
		if c != "" && !slices.Contains(cats, c) {
			cats = append(cats, c)
		}
	}
	facet := entry.Facet("")
	isOps := false
	if p != nil {
		facet = entry.Facet(p.Facet)
		isOps = p.IsOpsRunbook()
		for _, c := range entry.PayloadCategories(facet, p.Text, p.Situation, p.Choice, p.Exception) {
			addCat(c)
		}
	}
	workMethod := entry.IsWorkMethodFacet(facet) || isOps
	catCounts := map[string]int{}
	totalTop := 0
	allSameSpace := true
	commonSpace := ""
	for _, rootID := range ids {
		e := st.Entries[rootID]
		if e == nil {
			continue
		}
		totalTop++
		if e.OriginSpace == "" {
			allSameSpace = false
		} else if commonSpace == "" {
			commonSpace = e.OriginSpace
		} else if e.OriginSpace != commonSpace {
			allSameSpace = false
		}
		topCats := map[string]bool{}
		for _, c := range e.Categories {
			cn := entry.CategoryOf(c.Name)
			if cn == "" || slices.Contains(e.CatsRemoved, cn) {
				continue
			}
			if c.By == entry.ByUser {
				topCats[cn] = true
			}
		}
		if !workMethod {
			for _, c := range entry.PayloadCategories(e.Facet, e.PayloadTexts()...) {
				if !slices.Contains(e.CatsRemoved, c) {
					topCats[c] = true
				}
			}
		}
		for c := range topCats {
			catCounts[c]++
		}
	}
	if totalTop > 0 {
		for c, n := range catCounts {
			if n*2 >= totalTop {
				addCat(c)
			}
		}
		if !workMethod && allSameSpace && commonSpace != "" {
			space = commonSpace
			for _, c := range entry.SpaceDefaults(space) {
				addCat(c)
			}
		}
	}
	sort.Strings(cats)
	return cats, space
}

// effectiveCategories are the categories a pattern still carries under today's Payload-First rules: a
// work-methodology pattern (interaction, tools, workflow, judgement) or a contract keeps a stored category only while
// tags still finds it on the pattern's own payload or its user-labelled evidence, so a label an older detector put on
// a general working rule (bookkeeping by the user's own conventions read as personal finance) is not carried into the
// profile's sensitive sections. Never adds a category; a persona or world pattern keeps what it has.
func effectiveCategories(st *store.State, p *Pattern) []string {
	if p == nil || len(p.Categories) == 0 {
		return nil
	}
	if st == nil || (!entry.IsWorkMethodFacet(entry.Facet(p.Facet)) && !p.IsContract()) {
		return slices.Clone(p.Categories)
	}
	ids := followMerges(st, p.EvidenceIDs)
	live := liveIDs(st, ids)
	if p.IsProfileCore() || p.IsOpsRunbook() || p.IsContract() {
		// As recount: live user-owned evidence counts even when the pool filters it out.
		for _, id := range ids {
			if e := st.Entries[id]; e != nil && e.Live() && isUserOwned(e) && !slices.Contains(live, id) {
				live = append(live, id)
			}
		}
	}
	now, _ := tags(st, p, append(live, liveIDs(st, followMerges(st, p.CounterIDs))...))
	var out []string
	for _, c := range p.Categories {
		if slices.Contains(now, c) {
			out = append(out, c)
		}
	}
	return out
}

// subjects are the names that bring a pattern in: its evidence's (memsrc.Subjects), at most MaxSubjects.
func subjects(st *store.State, ids []string) []string {
	var out []string
	for _, id := range ids {
		e := st.Entries[id]
		if e == nil {
			continue
		}
		for _, s := range memsrc.Subjects(st, e) {
			if len(out) < memsrc.MaxSubjects && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

func isoWeek(day string) string {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return ""
	}
	y, w := t.ISOWeek()
	return weekName(y, w)
}

func weekName(y, w int) string {
	b := []byte("0000-W00")
	for i := 3; i >= 0; i-- {
		b[i] = byte('0' + y%10)
		y /= 10
	}
	b[6], b[7] = byte('0'+w/10), byte('0'+w%10)
	return string(b)
}

// patternID is a pattern's id, from its facet and what it is about, so the same pattern found again keeps its id.
func patternID(facet, situation, choice, text string) string {
	key := norm.Q(situation) + "|" + norm.Q(choice)
	if strings.Trim(key, "|") == "" {
		key = norm.Q(text)
	}
	h := sha1.Sum([]byte(facet + "|" + key))
	return "ov:" + hex.EncodeToString(h[:])[:10]
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func union(a []string, b ...string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if x != "" && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}
