package entry

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Leverage is the daily pass's reading of how much an entry matters (daily/17): how far it reaches (Reach), how long
// it holds (Horizon), and the principle or motive behind the choice that would decide other situations too. The score
// itself is computed (Score), never stored, so a change of formula needs no migration.
type Leverage struct {
	Reach     string `json:"reach,omitempty"`   // one of Reaches
	Horizon   string `json:"horizon,omitempty"` // one of Horizons
	Principle string `json:"principle,omitempty"`
	// PrincipleInferred: the user did not say the principle; it was read from the same lines.
	PrincipleInferred bool `json:"principle_inferred,omitempty"`
	// Dimension is how the user thinks (decision, thinking, reaction, attention, communication), for judgement and
	// persona entries that describe a way of thinking rather than one choice. Hint is the behaviour in a few words.
	Dimension string `json:"dimension,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

// Reach values, narrowest first.
const (
	ReachOneOff       = "one_off"
	ReachTaskKind     = "task_kind"
	ReachProject      = "project"
	ReachCrossProject = "cross_project"
	ReachEveryTurn    = "every_turn"
)

// Horizon values, shortest first.
const (
	HorizonDays    = "days"
	HorizonWeeks   = "weeks"
	HorizonMonths  = "months"
	HorizonLasting = "lasting"
)

// Reaches and Horizons are the allowed values, in order.
var (
	Reaches     = []string{ReachOneOff, ReachTaskKind, ReachProject, ReachCrossProject, ReachEveryTurn}
	Horizons    = []string{HorizonDays, HorizonWeeks, HorizonMonths, HorizonLasting}
	Dimensions  = []string{"decision", "thinking", "reaction", "attention", "communication"}
	maxLevField = 160
)

// Kind of an entry for the standing quotas: rules for how the assistant works (① and ②), understanding of the user
// (③ ④ ⑥ and ⑤'s decisions and evaluations).
const (
	KindRules         = "rules"
	KindUnderstanding = "understanding"
)

// KindOfFacet is the quota a facet's entries count against; "" for what never stands (people cards, relations).
func KindOfFacet(f string) string {
	switch Facet(f) {
	case Interaction, Tools:
		return KindRules
	case Judgement, Workflow, Persona, World:
		return KindUnderstanding
	}
	return ""
}

// KindOf is e's quota, "" when it never stands: a people card, a relation triple, a world fact other than a decision,
// a commitment or an evaluation.
func KindOf(e *Entry) string {
	if e.Facet == World {
		if e.Predicate != "" {
			return ""
		}
		switch strings.ToLower(strings.TrimSpace(e.Type)) {
		case "decision", "evaluation", "commitment":
			return KindUnderstanding
		}
		return ""
	}
	return KindOfFacet(string(e.Facet))
}

// Normalize keeps only allowed values and clips the texts; nil when nothing is left.
func (l *Leverage) Normalize() *Leverage {
	if l == nil {
		return nil
	}
	out := *l
	if !slices.Contains(Reaches, out.Reach) {
		out.Reach = ""
	}
	if !slices.Contains(Horizons, out.Horizon) {
		out.Horizon = ""
	}
	if !slices.Contains(Dimensions, out.Dimension) {
		out.Dimension = ""
	}
	out.Principle = clipRunes(strings.TrimSpace(out.Principle), maxLevField)
	out.Hint = clipRunes(strings.TrimSpace(out.Hint), 40)
	if out.Principle == "" {
		out.PrincipleInferred = false
	}
	if out == (Leverage{}) {
		return nil
	}
	return &out
}

// MergeLeverage folds a new sighting's reading into e's: the wider reach and the longer horizon win; a principle, a
// dimension and a hint are kept once set (a stated principle replaces an inferred one).
func (e *Entry) MergeLeverage(n *Leverage) {
	n = n.Normalize()
	if n == nil {
		return
	}
	if e.Leverage == nil {
		c := *n
		e.Leverage = &c
		return
	}
	l := e.Leverage
	if slices.Index(Reaches, n.Reach) > slices.Index(Reaches, l.Reach) {
		l.Reach = n.Reach
	}
	if slices.Index(Horizons, n.Horizon) > slices.Index(Horizons, l.Horizon) {
		l.Horizon = n.Horizon
	}
	if n.Principle != "" && (l.Principle == "" || (l.PrincipleInferred && !n.PrincipleInferred)) {
		l.Principle, l.PrincipleInferred = n.Principle, n.PrincipleInferred
	}
	if l.Dimension == "" {
		l.Dimension = n.Dimension
	}
	if l.Hint == "" {
		l.Hint = n.Hint
	}
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// reachOf and horizonOf are the labels, or an estimate from the other fields for an entry read before daily/17 (and a
// seed): a global scope reaches further than a project's, a class or an abstract entry further than one action, and
// what the user said holds from now on lasts.
func reachOf(e *Entry) int {
	if e.Leverage != nil {
		if i := slices.Index(Reaches, e.Leverage.Reach); i >= 0 {
			return i
		}
	}
	r := 1
	if global(e) {
		r = 2
	}
	switch e.Grain {
	case Class:
		r++
	case Abstract:
		r += 2
	}
	return min(r, len(Reaches)-1)
}

func horizonOf(e *Entry) int {
	if e.Leverage != nil {
		if i := slices.Index(Horizons, e.Leverage.Horizon); i >= 0 {
			return i
		}
	}
	switch {
	case slices.Contains(e.Flags, "durable"):
		return 3
	case e.Grain == Abstract:
		return 2
	}
	return 1
}

// global: no scope, or a global one among them.
func global(e *Entry) bool {
	if len(e.Scopes) == 0 {
		return true
	}
	return slices.ContainsFunc(e.Scopes, func(s Scope) bool { return s.Kind == "global" })
}

// Principle is the principle behind e, "" when none was read.
func (e *Entry) Principle() string {
	if e.Leverage == nil {
		return ""
	}
	return e.Leverage.Principle
}

// Score is how much the entry matters, 0 to 100 (pinned 100): the model's labels (how far, how long; at most 50), the
// evidence (independent sightings, source kinds, scopes; at most 30) and the user's own signals (said it holds from
// now on, corrected the assistant, more each day it had to again, wrote or confirmed it, a principle; at most 28), less for a candidate, a dormant or
// a low-confidence entry. Every part is a field of the entry: no learnt weights.
const countMost = 15.0

func Score(e *Entry) int {
	if e.Pinned {
		return 100
	}
	s := float64(reachOf(e))*6.25 + float64(horizonOf(e))*25.0/3
	s += math.Min(countMost, 5*math.Log2(1+float64(max(e.Count, 1))))
	s += math.Min(8, 4*float64(max(len(e.Kinds)-1, 0)))
	s += math.Min(7, 3.5*float64(max(len(e.Scopes)-1, 0)))
	u := 0.0
	if slices.Contains(e.Flags, "durable") {
		u += 8
	}
	if e.Facet == Interaction && (strings.EqualFold(e.Type, "correction") || e.Rejected != "") {
		// Each day the user had to correct it again is a violation (cogpipe's violation_count): 6 the first, up to 20.
		u += math.Min(20, 6+4*float64(max(correctionDays(e)-1, 0)))
	}
	if IsTrueUserOwned(e) || lastUserMark(e) == "right" {
		u += 10
	}
	if e.Principle() != "" && KindOf(e) == KindUnderstanding {
		u += 4
	}
	s += math.Min(28, u)
	if slices.Contains(e.Flags, FlagLowConfidence) {
		s -= 10
	}
	if e.Facet == World && strings.EqualFold(strings.TrimSpace(e.Type), "status") {
		s -= 20
	}
	switch e.Status {
	case Candidate:
		s *= 0.8
	case Dormant:
		s *= 0.5
	}
	return int(math.Round(math.Max(0, math.Min(99, s))))
}

// correctionDays are the days the user's own lines behind e fall on.
func correctionDays(e *Entry) int {
	days := map[string]bool{}
	for _, ev := range e.Evidence {
		if selfLine(ev) && ev.Day != "" {
			days[ev.Day] = true
		}
	}
	return len(days)
}

func lastUserMark(e *Entry) string {
	for i := len(e.Verdicts) - 1; i >= 0; i-- {
		if e.Verdicts[i].By == "user" {
			return e.Verdicts[i].Mark
		}
	}
	return ""
}

// Rooted says whether e is a core principle that holds whether or not it comes up (design l0-leverage 10.3, after
// cogpipe's core patterns): its evidence falls in at least four ISO weeks, or it reaches across projects (or every
// turn), lasts, states a principle and rests on at least two days. A rooted entry is not put to sleep by quiet days
// alone: the user is asked once instead.
func Rooted(e *Entry, days []string) bool {
	weeks := map[string]bool{}
	for _, d := range days {
		if w := isoWeek(d); w != "" {
			weeks[w] = true
		}
	}
	if len(weeks) >= 4 {
		return true
	}
	distinct := map[string]bool{}
	for _, d := range days {
		distinct[d] = true
	}
	return reachOf(e) >= 3 && horizonOf(e) == 3 && e.Principle() != "" && len(distinct) >= 2
}

// isoWeek is a day's ISO week ("2026-W47"), "" for a day that does not parse.
func isoWeek(day string) string {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return ""
	}
	y, w := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// Hard reports whether e carries a multi-day correction signal or explicit user pin for L1 reference (0.4.0 §2).
// Single-day L0 anger/ban cue auto-promotion has been removed: an entry qualifies here only if explicitly Pinned or
// corrected by the user's own lines on at least two distinct days within 7 days.
func Hard(e *Entry) bool {
	if e == nil {
		return false
	}
	if e.Pinned {
		return true
	}
	if e.Facet != Interaction && e.Facet != Tools {
		return false
	}
	if !strings.EqualFold(e.Type, "correction") && strings.TrimSpace(e.Rejected) == "" {
		return false
	}
	var days []string
	for _, ev := range e.Evidence {
		if !selfLine(ev) {
			continue
		}
		if ev.Day != "" && !slices.Contains(days, ev.Day) {
			days = append(days, ev.Day)
		}
	}
	slices.Sort(days)
	for i := 1; i < len(days); i++ {
		if a, err1 := time.Parse("2006-01-02", days[i-1]); err1 == nil {
			if b, err2 := time.Parse("2006-01-02", days[i]); err2 == nil && b.Sub(a) <= 7*24*time.Hour {
				return true
			}
		}
	}
	return false
}

// FlagCoveredBy marks a rule the weekly or daily rule merge found to say what another one says, when it could not be
// merged away (the assistant's hand-written memory, or the user's own): it stays and is recalled, but does not stand
// beside the one that covers it (0.2.6). The covering entry's id follows the colon.
const FlagCoveredBy = "covered_by:"

// CoveredBy is the id of the rule that covers e, "" when none does.
func CoveredBy(e *Entry) string {
	for _, f := range e.Flags {
		if strings.HasPrefix(f, FlagCoveredBy) {
			return strings.TrimPrefix(f, FlagCoveredBy)
		}
	}
	return ""
}

// FlagFastHot marks a temporary hard contract extracted by the correction fast path (unified design §2.1):
// "fast_hot:YYYY-MM-DD". It enters the standing hot slot (TTL 7 days) until L1 contract aggregation absorbs it.
const FlagFastHot = "fast_hot:"

// FastHotDay returns the latest YYYY-MM-DD day marked by FlagFastHot on e, or "" if none.
func FastHotDay(e *Entry) string {
	if e == nil {
		return ""
	}
	best := ""
	for _, f := range e.Flags {
		if d, ok := strings.CutPrefix(f, FlagFastHot); ok && d > best {
			best = d
		}
	}
	return best
}

// SetFastHot records (or updates) FlagFastHot on e to day (YYYY-MM-DD), removing any older fast_hot flag.
func SetFastHot(e *Entry, day string) {
	if e == nil || day == "" {
		return
	}
	e.Flags = slices.DeleteFunc(e.Flags, func(f string) bool { return strings.HasPrefix(f, FlagFastHot) })
	e.Flags = append(e.Flags, FlagFastHot+day)
}

// IsFastHot reports whether e is an active (unexpired) fast-path temporary hard contract within ttlDays of now.
func IsFastHot(e *Entry, now time.Time, ttlDays int) bool {
	if e == nil || !e.Live() || KindOf(e) != KindRules {
		return false
	}
	day := FastHotDay(e)
	if day == "" {
		return false
	}
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return false
	}
	if ttlDays <= 0 {
		ttlDays = 7
	}
	return now.Sub(t) <= time.Duration(ttlDays)*24*time.Hour
}

// ColdStartEligible reports whether e qualifies for L0 cold-start fallback when its standing bucket has no L1 entries
// yet (unified design §2.1): reach >= cross_project and horizon == lasting with a principle (understanding) or hard/lasting
// cross-project rule (rules).
func ColdStartEligible(e *Entry) bool {
	if e == nil {
		return false
	}
	k := KindOf(e)
	if k == "" {
		return false
	}
	wideLasting := e.Leverage != nil &&
		(e.Leverage.Reach == ReachCrossProject || e.Leverage.Reach == ReachEveryTurn) &&
		e.Leverage.Horizon == HorizonLasting
	if k == KindRules {
		return Hard(e) || wideLasting
	}
	return wideLasting && e.Principle() != ""
}

// FlagLowConfidence is the verifier's flag for an entry the extraction itself was unsure of (verify.LowConfidence).
const FlagLowConfidence = "LOW_CONFIDENCE"

// IsUnreliableOneOff says whether e is too shaky to describe the user: an entry the extraction flagged
// LOW_CONFIDENCE, or a persona entry read from a single sighting whose reach is one_off (one vent about one day, not
// who the user is). The user's own entries (IsTrueUserOwned) never are. Such entries stay for on-demand recall but are
// kept out of the profile summary (and Phase 1 of migrate-v04 demotes the stable LOW_CONFIDENCE persona ones).
func IsUnreliableOneOff(e *Entry) bool {
	if e == nil || IsTrueUserOwned(e) {
		return false
	}
	if slices.Contains(e.Flags, FlagLowConfidence) {
		return true
	}
	return e.Facet == Persona && e.Count <= 1 && e.Leverage != nil && e.Leverage.Reach == ReachOneOff
}
