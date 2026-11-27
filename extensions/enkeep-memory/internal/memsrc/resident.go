package memsrc

import (
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/export"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/recall"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// The 0.4.0 three-section + dynamic hot-slot standing set (§1.7):
//
//   - Bucket 0 (Hot Slot): dynamic 0%–10% (at most HotSlotMaxBytes=400 B and HotSlotMax=2 entries) for genuinely
//     user-owned Pinned entries and active 7-day de-shelled non-ops Fast-Path corrections. Unused hot-slot bytes
//     return to the three main sections.
//   - Section 1 (Profile Core, ~20%): user identity, stage, constraints, and anti-misread boundaries (A1).
//   - Section 2 (Cognitive Principles, ~28%): reconciled cross-scene decision/thinking principles with Poles/Exception (A2).
//   - Section 3 (Universal Contracts, ~52%): cross-tool universal AI collaboration rules (B), enforcing polarity balance
//     (check <= 45%, act <= 35%, communicate <= 30%, route <= 30%) and a strict cap of HardContractLimit=3 【硬】 lines
//     (remaining contracts stay neutral).
//
// Hard invariants enforced in code:
//   - 0 ops_runbook entries (entry.IsOpsRunbookEntry) in global standing.
//   - 0 S6 assistant_note pass-through entries (entry.IsAssistantNote) in global standing.
//   - 0 ephemeral entries (entry.IsEphemeral) in global standing.
//   - 0 sensitive or restricted-space entries in global standing.
//   - Blank new-user cold-start fallback: any section with 0 L1 candidates falls back independently to qualified T0/T1
//     L0 entries (0 reliance on S6). L1 leftover room flows only between L1 sections, never into L0 fragments.
const (
	ResidentBytes = 1400
	ResidentMost  = 12

	// HotSlotShare, HotSlotMaxBytes, HotSlotMax, HotSlotTTLDays bound Bucket 0 (Hot Slot).
	HotSlotShare    = 0.10
	HotSlotMaxBytes = 400
	HotSlotMax      = 2
	HotSlotTTLDays  = 7

	// Main section shares of the room left after Bucket 0 (0.20 + 0.28 + 0.52 = 1.00).
	ProfileShare   = 0.20
	PrincipleShare = 0.28
	ContractShare  = 0.52

	// HardContractLimit caps how many contracts receive the 【硬】 prefix in Section 3 (v0.4.0 §1.7).
	HardContractLimit = 3

	// PolarityShare is the default cap for an unspecified contract polarity; specific polarities use polarityShareOf
	// (check <= 45%, act <= 35%, communicate <= 30%, route <= 30%).
	PolarityShare = 0.35
	// GroupShare caps any single non-generic occasion/type class ("<facet>:<type>") within a section's effective room.
	GroupShare = 0.20

	// Per-line maximum lengths in bytes by section.
	HotTextMost       = 260
	ProfileTextMost   = 240
	PrincipleTextMost = 280
	ContractTextMost  = 280
	residentTextMost  = 280

	// sameRule is the bigram Jaccard threshold above which two standing lines are treated as duplicates.
	sameRule = 0.45
	// rankHot is the first hot-slot rank (995..994); rankStand is the first main-section rank (989 down).
	rankHot   = 995
	rankStand = 989
	// residentOverhead is what the hub adds to each line ("- " and a line end).
	residentOverhead = 3

	hardTagPrefix = "【硬】"
)

// Bucket names for three-section + hot-slot standing selection.
const (
	BucketHot       = "hot"
	BucketProfile   = "profile"
	BucketPrinciple = "principle"
	BucketContract  = "contract"
)

// Candidate is one entry that may stand: a hot-slot entry (Pinned or Fast-Path), an L1 profile/principle/contract,
// or a cold-start L0 fallback entry.
type Candidate struct {
	ID    string
	Text  string
	Kind  string // entry.KindRules or entry.KindUnderstanding
	Score int
	// Days (independent quotes/evidence count) and Last (newest evidence day) break ties in score.
	Days int
	Last string
	// Bucket is BucketHot, BucketProfile, BucketPrinciple, or BucketContract (inferred from Kind/Facet when empty).
	Bucket string
	// L1 marks an L1 overview pattern (false for hot-slot and cold-start L0 entries).
	L1 bool
	// UserOwned marks a genuinely user-sovereign T0 entry (SelfReport, FromOnboarding, Pinned, or user-edited).
	UserOwned bool
	// Pinned marks a user-pinned entry (ranks first inside the hot slot).
	Pinned bool
	// Hard marks a hard contract candidate (at most HardContractLimit receive 【硬】 in Section 3).
	Hard bool
	// Polarity is a contract's action direction (act/check/communicate/route).
	Polarity string
	// Facet is the source facet ("persona", "judgement", "workflow", "interaction", ...).
	Facet string
	// Dimension is a principle's cognitive dimension (decision/thinking/reaction/attention/communication).
	Dimension string
	// Group is the non-generic class GroupShare caps ("<facet>:<type>"); "" is uncapped.
	Group string
}

// polarityShareOf returns the anti-monopoly polarity cap within Section 3 (v0.4.0 §1.7):
// check <= 45%, act <= 35%, communicate <= 30%, route <= 30%.
func polarityShareOf(pol string) float64 {
	switch pol {
	case "check":
		return 0.45
	case "act":
		return 0.35
	case "communicate":
		return 0.30
	case "route":
		return 0.30
	default:
		return PolarityShare
	}
}

// StandingPrefix returns the lightweight semantic tier tag prepended to standing lines:
//   - BucketProfile: "【画像】"
//   - BucketPrinciple: "【原则】"
//   - BucketContract: "" (the <=3 【硬】 tags are applied deterministically inside ResidentWithin)
func StandingPrefix(bucket string, hard bool, polarity string) string {
	switch bucket {
	case BucketProfile:
		return "【画像】"
	case BucketPrinciple:
		return "【原则】"
	default:
		return ""
	}
}

func (c Candidate) bucket() string {
	if c.Bucket != "" {
		return c.Bucket
	}
	if c.Facet == string(entry.Persona) {
		return BucketProfile
	}
	if c.Kind == entry.KindRules {
		return BucketContract
	}
	return BucketPrinciple
}

// Resident picks the standing set from cands in the default ResidentBytes room.
func Resident(cands []Candidate) []Candidate {
	return ResidentWithin(cands, ResidentBytes, ResidentMost)
}

// ResidentWithin picks the standing set from cands in total bytes, at most most entries (most <= 0 means no count cap
// beyond HotSlotMax for Bucket 0 and total bytes for the three main sections).
func ResidentWithin(cands []Candidate, total, most int) []Candidate {
	if total <= 0 {
		total = ResidentBytes
	}
	var hotPool, profL1, profL0, princL1, princL0, contL1, contL0 []Candidate
	for _, c := range cands {
		if strings.TrimSpace(c.Text) == "" {
			continue
		}
		switch c.bucket() {
		case BucketHot:
			c.Bucket = BucketHot
			hotPool = append(hotPool, c)
		case BucketProfile:
			c.Bucket = BucketProfile
			if c.L1 {
				profL1 = append(profL1, c)
			} else {
				profL0 = append(profL0, c)
			}
		case BucketContract:
			c.Bucket = BucketContract
			if c.L1 {
				contL1 = append(contL1, c)
			} else {
				contL0 = append(contL0, c)
			}
		default:
			c.Bucket = BucketPrinciple
			if c.L1 {
				princL1 = append(princL1, c)
			} else {
				princL0 = append(princL0, c)
			}
		}
	}

	// Each main section uses L1 exclusively once any qualified L1 candidate exists; only a section with 0 L1 candidates
	// falls back to cold-start L0 candidates.
	profIsL1 := len(profL1) > 0
	profPool := profL1
	if !profIsL1 {
		profPool = profL0
	}
	princIsL1 := len(princL1) > 0
	princPool := princL1
	if !princIsL1 {
		princPool = princL0
	}
	contIsL1 := len(contL1) > 0
	contPool := contL1
	if !contIsL1 {
		contPool = contL0
	}

	sortHot(hotPool)
	sortPrinciples(profPool)
	sortPrinciples(princPool)
	sortContracts(contPool)

	taken := map[string]bool{}
	var allChosen []Candidate
	alike := func(c Candidate) bool {
		for _, x := range allChosen {
			if norm.Jaccard(x.Text, c.Text) >= sameRule {
				return true
			}
		}
		return false
	}
	atMost := func() bool { return most > 0 && len(allChosen) >= most }

	// Step 1: Bucket 0 (Hot Slot): min(10% of total, HotSlotMaxBytes=400 B), at most HotSlotMax=2 entries.
	hotCap := min(int(float64(total)*HotSlotShare), HotSlotMaxBytes)
	var hotChosen []Candidate
	hotUsed := 0
	for _, c := range hotPool {
		if len(hotChosen) >= HotSlotMax || atMost() {
			break
		}
		if taken[c.ID] || alike(c) {
			continue
		}
		size := len(c.Text) + residentOverhead
		if hotUsed+size > hotCap || hotUsed+size > total {
			continue
		}
		hotUsed += size
		taken[c.ID] = true
		hotChosen = append(hotChosen, c)
		allChosen = append(allChosen, c)
	}

	// Step 2: Allocate remaining room across the three main sections (~20% Profile, ~28% Principles, ~52% Contracts).
	rem := total - hotUsed
	profBase := int(float64(rem) * ProfileShare)
	princBase := int(float64(rem) * PrincipleShare)
	contBase := rem - profBase - princBase

	var profChosen, princChosen, contChosen []Candidate
	profUsed, princUsed, contUsed := 0, 0, 0
	princGroups := map[string]int{}
	contPols, contGroups := map[string]int{}, map[string]int{}
	hardTagged := 0

	fillProfile := func(budget int) {
		for _, c := range profPool {
			if atMost() {
				return
			}
			if taken[c.ID] || alike(c) {
				continue
			}
			size := len(c.Text) + residentOverhead
			if profUsed+size > budget || hotUsed+profUsed+princUsed+contUsed+size > total {
				continue
			}
			profUsed += size
			taken[c.ID] = true
			profChosen = append(profChosen, c)
			allChosen = append(allChosen, c)
		}
	}

	fillPrinciples := func(budget int, relaxGroup bool) {
		grpCap := int(float64(budget) * GroupShare)
		for _, c := range princPool {
			if atMost() {
				return
			}
			if taken[c.ID] || alike(c) {
				continue
			}
			size := len(c.Text) + residentOverhead
			if princUsed+size > budget || hotUsed+profUsed+princUsed+contUsed+size > total {
				continue
			}
			if !relaxGroup && c.Group != "" && grpCap > 0 && princGroups[c.Group]+size > grpCap {
				continue
			}
			princUsed += size
			if c.Group != "" {
				princGroups[c.Group] += size
			}
			taken[c.ID] = true
			princChosen = append(princChosen, c)
			allChosen = append(allChosen, c)
		}
	}

	fillContracts := func(budget int) {
		grpCap := int(float64(budget) * GroupShare)
		for _, c := range contPool {
			if atMost() {
				return
			}
			if taken[c.ID] || alike(c) {
				continue
			}
			addHardPrefix := c.Hard && hardTagged < HardContractLimit && !strings.HasPrefix(c.Text, hardTagPrefix)
			txt := c.Text
			if addHardPrefix {
				txt = hardTagPrefix + txt
			}
			size := len(txt) + residentOverhead
			if contUsed+size > budget || hotUsed+profUsed+princUsed+contUsed+size > total {
				continue
			}
			if c.Polarity != "" {
				polCap := int(float64(budget) * polarityShareOf(c.Polarity))
				if polCap > 0 && contPols[c.Polarity]+size > polCap {
					continue
				}
			}
			if c.Group != "" && grpCap > 0 && contGroups[c.Group]+size > grpCap {
				continue
			}
			c.Text = txt
			if addHardPrefix {
				hardTagged++
			}
			contUsed += size
			if c.Polarity != "" {
				contPols[c.Polarity] += size
			}
			if c.Group != "" {
				contGroups[c.Group] += size
			}
			taken[c.ID] = true
			contChosen = append(contChosen, c)
			allChosen = append(allChosen, c)
		}
	}

	// Pass A: fill each section within its nominal budget.
	fillProfile(profBase)
	fillPrinciples(princBase, false)

	// Unused profile/principle room flows into contracts if contracts are L1 (or during pure cold-start when all
	// sections are L0). Never spill L1 leftover into L0 fragments.
	unusedProf := max(0, profBase-profUsed)
	unusedPrinc := max(0, princBase-princUsed)
	contEff := contBase
	if contIsL1 || (!profIsL1 && !princIsL1) {
		contEff += unusedProf + unusedPrinc
	}
	fillContracts(contEff)

	// Pass B: if room remains, spill back into L1 principles, then L1 profile, then L1 contracts (strictly preserving
	// polarity caps so no single polarity monopolizes the section).
	if remLeft := rem - profUsed - princUsed - contUsed; remLeft > 0 {
		if princIsL1 || (!profIsL1 && !contIsL1) {
			princEff := princUsed + remLeft
			fillPrinciples(princEff, false)
			if princUsed < princEff {
				fillPrinciples(princEff, true)
			}
		}
	}
	if remLeft := rem - profUsed - princUsed - contUsed; remLeft > 0 {
		if profIsL1 || (!princIsL1 && !contIsL1) {
			fillProfile(profUsed + remLeft)
		}
	}
	if remLeft := rem - profUsed - princUsed - contUsed; remLeft > 0 {
		if contIsL1 {
			fillContracts(contUsed + remLeft)
		}
	}

	// Output order: Hot Slot -> L1 sections (Profile -> Principles -> Contracts) -> Cold-start L0 fallback sections.
	out := make([]Candidate, 0, len(hotChosen)+len(profChosen)+len(princChosen)+len(contChosen))
	out = append(out, hotChosen...)
	for _, sec := range [][]Candidate{profChosen, princChosen, contChosen} {
		for _, c := range sec {
			if c.L1 {
				out = append(out, c)
			}
		}
	}
	for _, sec := range [][]Candidate{profChosen, princChosen, contChosen} {
		for _, c := range sec {
			if !c.L1 {
				out = append(out, c)
			}
		}
	}
	return out
}

func sortHot(cs []Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.Pinned != b.Pinned {
			return a.Pinned
		}
		if a.Last != b.Last {
			return a.Last > b.Last
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.ID < b.ID
	})
}

func sortContracts(cs []Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		// Prefer Hard check-polarity guardrails first for the <=3 【硬】 slots, then unified Score.
		aHardCheck := a.Hard && a.Polarity == "check"
		bHardCheck := b.Hard && b.Polarity == "check"
		if aHardCheck != bHardCheck {
			return aHardCheck
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.UserOwned != b.UserOwned {
			return a.UserOwned
		}
		if a.Hard != b.Hard {
			return a.Hard
		}
		if a.Last != b.Last {
			return a.Last > b.Last
		}
		if a.Days != b.Days {
			return a.Days > b.Days
		}
		return a.ID < b.ID
	})
}

func sortPrinciples(cs []Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.UserOwned != b.UserOwned {
			return a.UserOwned
		}
		if a.Last != b.Last {
			return a.Last > b.Last
		}
		if a.Days != b.Days {
			return a.Days > b.Days
		}
		return a.ID < b.ID
	})
}

// FormatResidentLine formats a standing line as body+suffix within maxBytes, always clipping body first and keeping
// suffix (the exception or poles boundary) whole. Only when suffix itself leaves fewer than 30 bytes for body is the
// suffix dropped to protect readability.
func FormatResidentLine(body, suffix string, maxBytes int) string {
	body = strings.TrimSpace(body)
	suffix = strings.TrimSpace(suffix)
	if maxBytes <= 0 {
		maxBytes = residentTextMost
	}
	if len(body)+len(suffix) <= maxBytes {
		return body + suffix
	}
	const minBodyBytes = 30
	if suffix != "" && maxBytes-len(suffix) < minBodyBytes {
		suffix = ""
	}
	return clipBytes(body, maxBytes-len(suffix)) + suffix
}

// ResidentText is the standing line for an L0 entry (hot slot or cold-start fallback): for understanding, its principle
// first then the choice it shows; for a rule, what the assistant does. At most HotTextMost bytes, keeping the exception
// whole.
func ResidentText(e *entry.Entry) string {
	line := strings.TrimSpace(recall.Head(e))
	if p := e.Principle(); p != "" && entry.KindOf(e) == entry.KindUnderstanding && e.User == nil && e.Facet != entry.Persona {
		ex := strings.TrimSpace(e.Choice)
		if s := strings.TrimSpace(e.Situation); s != "" && ex != "" {
			ex = s + "：" + ex
		}
		if ex == "" {
			ex = line
		}
		line = "原则：" + strings.TrimRight(p, "。.") + "（如 " + strings.TrimRight(ex, "。.") + "）"
	}
	return FormatResidentLine(line, recall.Exception(e), HotTextMost)
}

// clipBytes cuts s to at most n bytes at a UTF-8 rune boundary, reserving space for "…" (3 bytes) when clipped.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ellipsis = "…"
	if n <= len(ellipsis) {
		return ""
	}
	s = s[:n-len(ellipsis)]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + ellipsis
}

// standingForbidden enforces the v0.4.0 §1.7 hard bans for any entry entering global 4 KB standing:
// 0 ops_runbook, 0 S6 assistant_note, 0 ephemeral, 0 sensitive, 0 restricted-space non-work entries.
func standingForbidden(st *store.State, e *entry.Entry) bool {
	if e == nil || !e.Live() || !Pool(st, e) {
		return true
	}
	if entry.IsOpsRunbookEntry(e) || entry.IsAssistantNote(e.Origin) || entry.IsEphemeral(e) {
		return true
	}
	cats, space, _ := Tags(st, e)
	if len(cats) > 0 {
		return true
	}
	if space != "" && !entry.IsWorkMethodFacet(e.Facet) {
		return true
	}
	// For non-work facets (e.g. Persona class aggregating sensitive actions), also block if any live member is sensitive
	// or space-confined so sensitive child entries never leak into global standing via a parent class.
	if st != nil && !entry.IsWorkMethodFacet(e.Facet) {
		for _, mid := range e.Members {
			if m := st.Entries[mid]; m != nil && m.Live() {
				if mc, ms, _ := Tags(st, m); len(mc) > 0 || ms != "" {
					return true
				}
			}
		}
	}
	return false
}

// isS3S4OnlyEntry reports whether e has evidence and all of it comes solely from S3/S4 agent/tool sessions.
func isS3S4OnlyEntry(e *entry.Entry) bool {
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

// CuratedCandidate converts e into a cold-start L0 standing Candidate for one of the three sections (Profile,
// Principle, or Contract), or returns false when e is forbidden or ineligible for standing.
func CuratedCandidate(st *store.State, e *entry.Entry, tpl map[string]bool) (Candidate, bool) {
	if standingForbidden(st, e) {
		return Candidate{}, false
	}
	if e.Facet == entry.World || e.Facet == entry.People || e.Facet == entry.Tools {
		return Candidate{}, false
	}
	if e.Facet == entry.Persona && isS3S4OnlyEntry(e) {
		return Candidate{}, false
	}
	k := entry.KindOf(e)
	if k == "" && e.Pinned {
		if k = entry.KindOfFacet(string(e.Facet)); k == "" {
			k = entry.KindUnderstanding
		}
	}
	if k == "" || !export.Usable(st, e) || (!e.Pinned && entry.TemplateOnly(e, tpl)) ||
		(!e.Pinned && entry.CoveredBy(e) != "" && coverLive(st, e)) {
		return Candidate{}, false
	}
	days := map[string]bool{}
	for _, ev := range e.Evidence {
		if ev.Day != "" {
			days[ev.Day] = true
		}
	}
	bucket := BucketPrinciple
	switch {
	case e.Pinned && entry.IsTrueUserOwned(e):
		bucket = BucketHot
	case e.Facet == entry.Persona:
		bucket = BucketProfile
	case k == entry.KindRules:
		bucket = BucketContract
	}
	dim := ""
	if e.Leverage != nil {
		dim = e.Leverage.Dimension
	}
	grp := GroupOf(st, e)
	if genericTypes[strings.ToLower(strings.TrimSpace(e.Type))] {
		grp = ""
	}
	return Candidate{
		ID:        e.ID,
		Text:      ResidentText(e),
		Kind:      k,
		Bucket:    bucket,
		Facet:     string(e.Facet),
		UserOwned: entry.IsTrueUserOwned(e),
		Pinned:    e.Pinned,
		Hard:      entry.Hard(e),
		Score:     entry.Score(e),
		Days:      len(days),
		Last:      e.Last,
		Dimension: dim,
		Group:     grp,
	}, true
}

// HotSlotCandidate returns e as a Bucket 0 (Hot Slot) candidate when it is either genuinely user-owned Pinned or
// an active Fast-Path temporary hard contract within HotSlotTTLDays (v0.4.0 §1.7), enforcing all hard bans.
func HotSlotCandidate(st *store.State, e *entry.Entry, tpl map[string]bool, now time.Time) (Candidate, bool) {
	if standingForbidden(st, e) {
		return Candidate{}, false
	}
	if e.Pinned && entry.IsTrueUserOwned(e) {
		k := entry.KindOf(e)
		if k == "" {
			if k = entry.KindOfFacet(string(e.Facet)); k == "" {
				k = entry.KindUnderstanding
			}
		}
		return Candidate{
			ID:        e.ID,
			Text:      ResidentText(e),
			Kind:      k,
			Bucket:    BucketHot,
			Facet:     string(e.Facet),
			UserOwned: true,
			Pinned:    true,
			Score:     entry.Score(e),
			Last:      e.Last,
		}, true
	}
	if !entry.IsFastHot(e, now, HotSlotTTLDays) {
		return Candidate{}, false
	}
	if entry.TemplateOnly(e, tpl) || (entry.CoveredBy(e) != "" && coverLive(st, e)) {
		return Candidate{}, false
	}
	return Candidate{
		ID:     e.ID,
		Text:   ResidentText(e),
		Kind:   entry.KindRules,
		Bucket: BucketHot,
		Facet:  string(e.Facet),
		Hard:   true,
		Score:  entry.Score(e),
		Last:   entry.FastHotDay(e),
	}, true
}

// UnabsorbedRuleHotCandidate promotes a high-certainty universal rule (UserOwned, Hard, or cross-month Abstract with
// Count >= 3) first seen strictly after the latest L1 build day (l1LatestDay, YYYY-MM-DD) into Bucket 0 (Hot Slot)
// within HotSlotTTLDays of now, excluding all ops_runbook, assistant_note, ephemeral, and sensitive entries.
func UnabsorbedRuleHotCandidate(st *store.State, e *entry.Entry, tpl map[string]bool, l1LatestDay string, now time.Time) (Candidate, bool) {
	if e == nil || l1LatestDay == "" || standingForbidden(st, e) {
		return Candidate{}, false
	}
	if e.First == "" || e.First <= l1LatestDay {
		return Candidate{}, false
	}
	if firstTime, err := time.Parse("2006-01-02", e.First); err != nil || now.Sub(firstTime) > time.Duration(HotSlotTTLDays)*24*time.Hour {
		return Candidate{}, false
	}
	if !export.Usable(st, e) || entry.TemplateOnly(e, tpl) || (entry.CoveredBy(e) != "" && coverLive(st, e)) {
		return Candidate{}, false
	}
	isUserOwned := entry.IsTrueUserOwned(e)
	highAbstract := e.Grain == entry.Abstract && e.Count >= 3 && e.Facet == entry.Interaction
	if !isUserOwned && !entry.Hard(e) && !highAbstract {
		return Candidate{}, false
	}
	k := entry.KindOf(e)
	if k == "" {
		k = entry.KindOfFacet(string(e.Facet))
	}
	if k != entry.KindRules {
		return Candidate{}, false
	}
	return Candidate{
		ID:        e.ID,
		Text:      ResidentText(e),
		Kind:      k,
		Bucket:    BucketHot,
		Facet:     string(e.Facet),
		UserOwned: isUserOwned,
		Hard:      entry.Hard(e),
		Score:     entry.Score(e),
		Last:      e.Last,
	}, true
}

// IsUserOwned delegates to entry.IsTrueUserOwned (0.4.0 §1.2 single source of truth): only genuinely user-sovereign
// entries (SelfReport, FromOnboarding, Pinned, or non-empty User edit) qualify; S6 assistant notes never do.
func IsUserOwned(e *entry.Entry) bool {
	return entry.IsTrueUserOwned(e)
}

// coverLive: the rule that covers e is still there to stand in its place (when it went, e stands again).
func coverLive(st *store.State, e *entry.Entry) bool {
	x := st.Entries[entry.CoveredBy(e)]
	return x != nil && x.Live()
}

// GroupOf is the class an entry's line counts towards in the standing set: its facet and its type as the type table
// names it now (a renamed type under its new name); "" without a type.
func GroupOf(st *store.State, e *entry.Entry) string {
	name := strings.TrimSpace(e.Type)
	if name == "" {
		return ""
	}
	for i := 0; i < 8 && st != nil; i++ {
		j := slices.IndexFunc(st.Types[e.Facet], func(t entry.TypeDef) bool { return norm.Q(t.Name) == norm.Q(name) })
		if j < 0 || st.Types[e.Facet][j].Renamed == "" {
			break
		}
		name = st.Types[e.Facet][j].Renamed
	}
	return string(e.Facet) + ":" + norm.Q(name)
}

// Quote is a line of the user's own words behind e, for recall on demand ("原话：「…」（day）"); "" when it rests on
// no line of the user's.
func Quote(e *entry.Entry) string {
	for i := len(e.Evidence) - 1; i >= 0; i-- {
		ev := e.Evidence[i]
		if ev.Who != entry.WhoSelf || ev.Reported || strings.TrimSpace(ev.Quote) == "" {
			continue
		}
		q := clip(strings.Join(strings.Fields(ev.Quote), " "), quoteMost)
		return "原话：「" + q + "」（" + ev.Day + "）"
	}
	return ""
}

// quoteMost is the longest quote a recalled line carries, in characters.
const quoteMost = 80
