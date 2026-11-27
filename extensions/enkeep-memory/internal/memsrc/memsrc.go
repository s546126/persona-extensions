// Package memsrc answers the hub's memory_sources call memory.entries (Persona #13532, schema/extension/memory.schema.json,
// docs/extensions/extensions.md "Memory sources"): the curated entries one by one, read-only, for the hub to grade by
// scene. It reads a snapshot of the state and never changes it. Nothing is filtered by scene here: every entry the
// text outlets (the resident block, the project files, the Inbox context, enkeep_recall) could carry is answered,
// with what the hub needs to grade it.
package memsrc

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/export"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/profile"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/recall"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// The protocol's bounds (memory.schema.json).
const (
	MaxLimit    = 500
	MaxText     = 4096
	MaxCats     = 8
	MaxSubjects = 16
	MaxSubject  = 80
	MaxAfter    = 512
	// MaxBytes: a page ends before its answer passes this (the hub's limit is 1 MB; room for the envelope).
	MaxBytes = 900 << 10
)

// Params is memory.entries' parameters (wire.MemoryEntriesParams; local until the SDK copy carries the types).
type Params struct {
	Contribution string `json:"contribution"`
	Limit        int    `json:"limit"`
	After        string `json:"after,omitempty"`
	// ResidentBytes is the room the standing set is chosen for (the resident_bytes setting; 0: ResidentBytes).
	ResidentBytes int `json:"-"`
}

// Result is one page (wire.MemoryEntriesResult).
type Result struct {
	Entries []Entry `json:"entries"`
	Tagged  bool    `json:"tagged"`
	HasMore bool    `json:"has_more"`
	After   string  `json:"after,omitempty"`
}

// Entry is one entry (wire.MemoryEntry).
type Entry struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	Facet       string   `json:"facet,omitempty"`
	Status      string   `json:"status"`
	OriginSpace string   `json:"origin_space,omitempty"`
	Categories  []string `json:"categories,omitempty"`
	Standing    bool     `json:"standing,omitempty"`
	Rank        int      `json:"rank,omitempty"`
	Subjects    []string `json:"subjects,omitempty"`
}

// ErrBadAfter is an after this source did not give.
var ErrBadAfter = errors.New("after is not a cursor this source gave")

var (
	idRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	facetRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	spaceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

// item is a pooled entry with its order key: a curated entry (e), or one made elsewhere in the extension (x): the profile summary's or the overview's patterns.
type item struct {
	e        *entry.Entry
	x        *Entry
	standing bool
	rank     int
	last     string
	text     string // a curated entry's line as answered, when not its recall line (standing, or with its quote)
}

// Extra is an entry made elsewhere in the extension (the periodic overview's L1 principles and contracts,
// internal/overview), answered beside the curated ones in the same order: Last is its newest evidence day.
type Extra struct {
	Entry Entry
	Last  string
	// Score and Kind (entry.KindRules or KindUnderstanding) make it a candidate for the standing set; Kind "" never
	// stands. Entry.Standing is decided here, not by the maker.
	Score int
	Kind  string
	// Evidence are the curated entries it rests on: a pattern resting mostly on template lines never stands.
	Evidence []string
	// L1 metadata for three-bucket standing selection (unified design §2.1).
	L1Kind     string   // "contract" or "principle"
	Hard       bool     // contract: hard rule
	Polarity   string   // contract: act / check / communicate / route
	Dimension  string   // principle: decision / thinking / reaction / attention / communication
	Group      string   // normalized "<facet>:<rule_type>" (empty for generic types)
	Quotes     int      // distinct normalised user quotes (tie-breaker)
	Body       string   // core statement without prefix/suffix (clipped first when over budget)
	Suffix     string   // "（例外：…）" or "（两端：…）" (never clipped)
	CoveredIDs []string // L0 entries covered by this L1 contract (used to retire absorbed hot-slot entries)
}

func (it item) id() string {
	if it.x != nil {
		return it.x.ID
	}
	return it.e.ID
}

// key is the cursor: the last answered entry's place in the order.
type key struct {
	S bool   `json:"s"`
	R int    `json:"r"`
	L string `json:"l"`
	I string `json:"i"`
}

func (it item) key() key { return key{it.standing, it.rank, it.last, it.id()} }

// before says whether a comes before b: standing first, then rank, then newest, then id.
func before(a, b key) bool {
	if a.S != b.S {
		return a.S
	}
	if a.R != b.R {
		return a.R > b.R
	}
	if a.L != b.L {
		return a.L > b.L
	}
	return a.I < b.I
}

// Relation predicates, in three kinds (0.2.0, design l0-leverage §13; the collaboration gap analysis §4.4):
//   - the skeleton (assigned, depends_on, member_of, collaborates_with, with owns, role and reports_to): who is
//     responsible for what, which team, what depends on what, who works with whom. Never standing (a world entry has
//     no standing kind), but ranked by its score in recall and fed to the overview and the evaluations like any entry;
//   - noise (about, same_circle): "X is about Y", a shared circle; ranked last, kept out of Pool;
//   - passing edges (waiting_on, blocked_by, allocated, serves): what holds now; weak only once stale, when last seen
//     more than staleDays ago.
//
// Up to beta.52 all ten were weak alike. Nothing is deleted from state; user-pinned, user-edited or user-confirmed
// entries are never weak.
var noisePredicates = map[string]bool{
	"about":       true,
	"same_circle": true,
}

var passingPredicates = map[string]bool{
	"allocated":  true,
	"blocked_by": true,
	"serves":     true,
	"waiting_on": true,
}

// staleDays is how long a passing edge stays strong after it was last seen.
const staleDays = 14

// nowFn is the clock IsWeakRelation reads a passing edge's age by (tests set it).
var nowFn = time.Now

// substantiveWorldTypes are world-facet types that carry durable project/ownership/decision value over transient status.
var substantiveWorldTypes = map[string]bool{
	"committed":  true,
	"decided":    true,
	"decides":    true,
	"decision":   true,
	"means":      true,
	"mentors":    true,
	"owns":       true,
	"reports_to": true,
	"role":       true,
	"term":       true,
}

func userConfirmedRight(e *entry.Entry) bool {
	for i := len(e.Verdicts) - 1; i >= 0; i-- {
		if e.Verdicts[i].By == "user" {
			return e.Verdicts[i].Mark == "right"
		}
	}
	return false
}

// IsWeakRelation says whether e is a low-value relation triple: a noise edge, or a passing edge not seen for
// staleDays (see noisePredicates). Weak triples are answered by memory.entries ranked last and kept out of Pool. The
// skeleton edges are never weak.
func IsWeakRelation(e *entry.Entry) bool {
	if e == nil || e.Facet != entry.World || e.Predicate == "" {
		return false
	}
	p := strings.ToLower(strings.TrimSpace(e.Predicate))
	switch {
	case noisePredicates[p]:
	case passingPredicates[p]:
		last, err := time.Parse("2006-01-02", e.Last)
		if err == nil && nowFn().Sub(last) <= staleDays*24*time.Hour {
			return false
		}
	default:
		return false
	}
	if e.Pinned || (e.User != nil && strings.TrimSpace(e.User.Text) != "") || userConfirmedRight(e) {
		return false
	}
	return true
}

// Pool says whether an entry is answered: live, settled or on its way (not one the user deleted, merged, retired or
// marked wrong), never a card's evaluation or inference about a person, never a low-value weak relation triple, and
// not life content when the user chose "work only". The same gate as enkeep_recall's, without its category look-up:
// grading by category is the hub's.
func Pool(st *store.State, e *entry.Entry) bool { return pool(st, e) && !IsWeakRelation(e) }

// Pageable is what memory.entries answers: the pool, and the weak relation triples too, ranked last (0.2.0-beta.52:
// the hub reads every page now, nwverse/persona#15170, so they no longer crowd the first thousand; the record is kept
// whole and recalled on demand).
func Pageable(st *store.State, e *entry.Entry) bool { return pool(st, e) }

func pool(st *store.State, e *entry.Entry) bool {
	if !e.Live() {
		return false
	}
	switch e.Status {
	case entry.Stable, entry.Shifting, entry.Candidate, entry.Dormant:
	default:
		if !e.Pinned {
			return false
		}
	}
	if e.Facet == entry.People && (e.Part == entry.PartUserEval || e.Part == entry.PartInferred || e.Inferred) {
		return false
	}
	if e.Domain == entry.Life && st.Onboarding != nil && st.Onboarding.Use == entry.Work {
		return false
	}
	return true
}

// Status maps the plugin's states to the protocol's: active is what the exits use today (stable, shifting, pinned,
// written by the user); candidate, dormant and retired as they are.
func Status(e *entry.Entry) string {
	switch {
	case !e.Live() || e.Status == entry.Retired:
		return "retired"
	case e.Status == entry.Stable || e.Status == entry.Shifting || export.Stable(e):
		return "active"
	case e.Status == entry.Dormant:
		return "dormant"
	}
	return "candidate"
}

// Rank orders entries by how much they matter (0 to 940; pinned 1000), not by how new they are:
//   - Active ops_runbook entries keep high on-demand recall rank (840–940) modulated by entry.OpsRunbookWeight
//     (45d demote 0.5x, 60d dormant 0.25x) so operational rules stay out of 4 KB standing yet recall reliably when
//     relevant and naturally decay when stale (v0.4.0 §1.3/§1.7).
//   - Active genuinely user-owned entries keep rank >= 820; entity-anchored local rules >= 750.
//   - Weak relation triples rank last (<= 9).
//   - Standing entries receive their own ranks (995..989 down) in workStandingSet.
func Rank(e *entry.Entry) int {
	if e.Pinned {
		return 1000
	}
	if entry.IsOpsRunbookEntry(e) && Status(e) == "active" {
		base := 840 + min(100, entry.Score(e))
		today := nowFn().Format("2006-01-02")
		w := entry.OpsRunbookWeight(e, today)
		return max(0, min(940, int(float64(base)*w)))
	}
	r := entry.Score(e) * 9
	if IsWeakRelation(e) {
		r = min(r, 9)
	} else if IsUserOwned(e) && Status(e) == "active" && entry.KindOf(e) != "" {
		r = max(r, 820)
	} else if entityAnchoredRule(e) && Status(e) == "active" {
		r = max(r, 750)
	}
	return max(0, min(891, r))
}

// entityAnchoredRule says whether e is a rule or decision explicitly anchored to a person, project, machine or tool
// (non-global scope, impact.object, or object), so it stays out of global standing while ranking high in recall.
func entityAnchoredRule(e *entry.Entry) bool {
	if e == nil {
		return false
	}
	switch e.Facet {
	case entry.Interaction, entry.Tools, entry.Judgement, entry.Workflow:
	default:
		return false
	}
	if strings.TrimSpace(e.Impact.Object) != "" || strings.TrimSpace(e.Object) != "" {
		return true
	}
	for _, sc := range e.Scopes {
		if sc.Kind != entry.ScopeGlobal && strings.TrimSpace(sc.Name) != "" {
			return true
		}
	}
	return false
}

// LegacyRank is Rank before 0.2.0-beta.52 (facet, grain and count, ties broken by the newest), kept for the hard
// evaluation's baseline arm (hardeval): the order memory.entries answered in up to beta.49.
func LegacyRank(e *entry.Entry) int {
	if e.Pinned {
		return 1000
	}
	var score int
	switch e.Facet {
	case entry.Interaction, entry.Judgement, entry.Workflow, entry.Tools, entry.Persona:
		score = 400
	case entry.People:
		score = 200
	default:
		typ := strings.ToLower(strings.TrimSpace(e.Type))
		pred := strings.ToLower(strings.TrimSpace(e.Predicate))
		switch {
		case substantiveWorldTypes[typ] || substantiveWorldTypes[pred]:
			score = 220
		case typ == "status":
			score = 80
		default:
			score = 180
		}
	}
	switch e.Grain {
	case entry.Abstract:
		score += 160
	case entry.Class:
		score += 100
	default:
		if slices.Contains(e.Flags, "durable") {
			score += 60
		}
	}
	if (e.Origin != entry.Behavior && e.Origin != "" && e.Origin != entry.Seed) ||
		(e.User != nil && strings.TrimSpace(e.User.Text) != "") ||
		userConfirmedRight(e) {
		score += 120
	}
	score += min(120, max(0, e.Count)*max(1, len(e.Kinds))*12)
	switch Status(e) {
	case "dormant":
		score /= 2
	case "candidate":
		score = score * 3 / 4
	}
	return max(0, min(999, score))
}

// Tagged says whether the entry's categories and space were decided by the tagging rules: its own mark, or, for a
// class or an abstract entry, every member's.
func Tagged(st *store.State, e *entry.Entry) bool { return tagged(st, e, 0) }

func tagged(st *store.State, e *entry.Entry, depth int) bool {
	if len(e.Members) == 0 || depth > 2 {
		return e.Tagged
	}
	n := 0
	for _, id := range e.Members {
		m := st.Entries[id]
		if m == nil {
			continue
		}
		if !tagged(st, m, depth+1) {
			return false
		}
		n++
	}
	return n > 0 || e.Tagged
}

// Tags are the categories and the source space an entry is answered with, and whether they were decided now rather
// than when it was read. An entry tagged by the 0.2.0-beta.40 rules (Tagged) keeps its own. One read before them is
// tagged here by the same rules on what it carries (curate.CategoriesOf): its old model label, the money words of its
// text and quotes when it has no label, the identity-paper and legal words; a class or an abstract entry also takes
// every member's categories and, without a space of its own, the first member's space. The state is not changed
// (memory.entries is read-only). Space: before 0.2.0-beta.40 no unit carried its space, so such an entry is answered
// from the main space unless it or a member says otherwise; what it is about still tags it (a health matter is
// health wherever it was said), and the hub never carries a sensitive entry outside its track.
func Tags(st *store.State, e *entry.Entry) (cats []string, space string, backfilled bool) {
	if Tagged(st, e) {
		return e.Cats(), e.OriginSpace, false
	}
	cats, space = backfill(st, e, 0)
	return cats, space, true
}

// backfill classifies untagged legacy entries strictly by Payload-First (v0.4.0 §1.6):
//   - NEVER scans raw Evidence quotes (ev.Quote), only the entry's own PayloadTexts().
//   - Work-methodology facets (interaction, tools, workflow, judgement) do NOT inherit OriginSpace or member
//     conversation-topic sensitivity unless their own payload discloses personal privacy or was explicitly tagged ByUser.
func backfill(st *store.State, e *entry.Entry, depth int) ([]string, string) {
	workMethod := entry.IsWorkMethodFacet(e.Facet)
	var cats []string
	space := ""
	if !workMethod {
		space = e.OriginSpace
	}
	add := func(list []string) {
		for _, c := range list {
			if c != "" && !slices.Contains(cats, c) && !slices.Contains(e.CatsRemoved, c) {
				cats = append(cats, c)
			}
		}
	}
	// Preserve explicit user-authored categories and Payload-First supported categories.
	ownPayload := entry.PayloadCategories(e.Facet, e.PayloadTexts()...)
	add(ownPayload)
	for _, c := range e.Categories {
		cn := entry.CategoryOf(c.Name)
		if cn == "" {
			continue
		}
		if c.By == entry.ByUser || (!workMethod && (c.By == entry.BySpaceDefault || slices.Contains(ownPayload, cn))) {
			add([]string{cn})
		}
	}
	if !workMethod {
		if cn := entry.CategoryOf(e.Sensitive); cn != "" && slices.Contains(ownPayload, cn) {
			add([]string{cn})
		}
	}
	if !workMethod && depth < 2 {
		for _, id := range e.Members {
			m := st.Entries[id]
			if m == nil {
				continue
			}
			var mc []string
			var ms string
			if Tagged(st, m) {
				mc, ms = m.Cats(), m.OriginSpace
			} else {
				mc, ms = backfill(st, m, depth+1)
			}
			add(mc)
			if space == "" {
				space = ms
			}
		}
	}
	if !workMethod {
		for _, c := range entry.SpaceDefaults(space) {
			add([]string{c})
		}
	}
	return cats, space
}

// Subjects are the names whose mention brings the entry in: the person of a card, its object, the person or project
// its impact is for, its scopes' names, skeleton endpoints, occasion words, and 1-hop skeleton neighbors (up to MaxSubjects).
func Subjects(st *store.State, e *entry.Entry) []string {
	var out []string
	var entityNames []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || len(out) >= MaxSubjects || slices.Contains(out, s) {
			return
		}
		out = append(out, clip(s, MaxSubject))
	}
	addEntity := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if strings.HasPrefix(s, "person:") && st != nil {
			if k := st.Contacts[s]; k != nil && k.Manual != "removed" && k.Name != "" {
				s = k.Name
			} else {
				s = strings.TrimPrefix(s, "person:")
			}
		} else {
			for _, p := range []string{"project:", "system:", "object:"} {
				s = strings.TrimPrefix(s, p)
			}
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		add(s)
		if !slices.Contains(entityNames, s) {
			entityNames = append(entityNames, s)
		}
	}
	if st != nil && e.Person != "" {
		if k := st.Contacts[e.Person]; k != nil && k.Manual != "removed" {
			addEntity(k.Name)
		}
	}
	addEntity(e.Object)
	// The person or project a rule is for (impact.object, design l0-leverage §13: rules hang on entities): mentioning
	// them brings the rule in. A kind of task named there is a short phrase too and does no harm.
	if o := strings.TrimSpace(e.Impact.Object); utf8.RuneCountInString(o) <= 24 {
		addEntity(o)
	}
	for _, sc := range e.Scopes {
		if sc.Kind != entry.ScopeGlobal {
			addEntity(sc.Name)
		}
	}
	if e.Facet == entry.World && entry.IsSkeletonPredicate(e.Predicate) {
		addEntity(e.Subject)
		addEntity(e.Target)
	}
	// The occasion a rule is for (0.2.6): decisions_query matches its tag's words as substrings of the text, the
	// subjects, the facet and the categories, so "派活" or "汇报" in a tag brings the rules for that moment in.
	if entry.KindOf(e) == entry.KindRules || e.Facet == entry.Workflow {
		for _, w := range momentWords[e.Impact.Moment] {
			add(w)
		}
		if g := GroupOf(st, e); g != "" && !genericTypes[e.Type] {
			add(strings.TrimPrefix(g, string(e.Facet)+":"))
		}
	}
	// Enrich user-owned rules and ops_runbook entries with concrete tool/host/command identifiers and occasion cues
	// (v0.4.0 §1.7) so non-standing ops runbooks stay 100% reachable via decisions_query(tag) and contextual recall.
	if (IsUserOwned(e) || entry.IsOpsRunbookEntry(e)) && (entry.KindOf(e) != "" || e.Facet == entry.Tools) && len(out) < MaxSubjects {
		enrichUserOwnedSubjects(recall.Head(e)+" "+e.Exception+" "+e.Object+" "+e.Pitfall+" "+e.Fix, add)
	}
	// 1-hop skeleton neighbors (unified design §2.2): for entity-anchored rules and ops_runbooks, attach up to 4
	// structural 1-hop neighbor names from the entity graph so querying a related project/person/repo surfaces the rule.
	if st != nil && st.Graph != nil && len(entityNames) > 0 && len(out) < MaxSubjects &&
		(entityAnchoredRule(e) || entry.IsOpsRunbookEntry(e) || (e.Facet == entry.World && entry.IsSkeletonPredicate(e.Predicate))) {
		for _, nb := range recall.SkeletonNeighbors(st, entityNames, 4) {
			add(nb)
		}
	}
	return out
}

var (
	uoIdentRe   = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_.-]{2,}`)
	uoIdentStop = map[string]bool{
		"the": true, "and": true, "for": true, "not": true, "all": true, "true": true, "false": true,
		"ttl": true, "api": true, "cli": true, "url": true, "pdf": true, "json": true, "md": true,
	}
	uoOccasions = []struct {
		cues []string
		tags []string
	}{
		{cues: []string{"派活", "派发", "派 Agent", "安排 Agent", "主管", "task_plan", "agent_start", "agent_send", "conversation_send"}, tags: []string{"派活", "dispatch"}},
		{cues: []string{"汇报", "交付", "结论", "摘要", "进展", "耗时", "时间戳"}, tags: []string{"汇报", "report"}},
		{cues: []string{"提问", "追问", "请示", "确认", "待定", "选项", "大白话", "授权卡", "卡片"}, tags: []string{"提问", "ask"}},
		{cues: []string{"开工", "执行", "plan mode", "大纲", "框架", "方案"}, tags: []string{"开工", "task"}},
		{cues: []string{"验收", "核实", "核对", "源头", "门禁", "测试", "编译"}, tags: []string{"验收", "门禁"}},
		{cues: []string{"排期", "估时间", "工期", "估计交付"}, tags: []string{"排期", "估时间"}},
		{cues: []string{"密钥", "凭据", "token", "1Password", "op CLI", "secret_env"}, tags: []string{"密钥", "凭据"}},
		{cues: []string{"配置", "升级", "回滚", "镜像", "备份"}, tags: []string{"配置", "升级"}},
		{cues: []string{"待办", "todo_complete", "STATE.md"}, tags: []string{"待办"}},
		{cues: []string{"飞书", "云文档", "多维表格"}, tags: []string{"飞书"}},
		{cues: []string{"评审", "质疑", "分歧", "独立判断"}, tags: []string{"评审", "review"}},
	}
)

func enrichUserOwnedSubjects(body string, add func(string)) {
	for _, m := range uoIdentRe.FindAllString(body, -1) {
		m = strings.Trim(m, ".-_")
		if len(m) < 3 {
			continue
		}
		low := strings.ToLower(m)
		if uoIdentStop[low] {
			continue
		}
		hasLetter := false
		for i := 0; i < len(low); i++ {
			if low[i] >= 'a' && low[i] <= 'z' {
				hasLetter = true
				break
			}
		}
		if hasLetter {
			add(m)
		}
	}
	lowBody := strings.ToLower(body)
	for _, r := range uoOccasions {
		for _, cue := range r.cues {
			if strings.Contains(lowBody, strings.ToLower(cue)) {
				for _, t := range r.tags {
					add(t)
				}
				break
			}
		}
	}
}

// momentWords are the words a query for an occasion would use, in both languages.
var momentWords = map[string][]string{
	"ask_user":     {"提问", "问用户", "ask"},
	"report":       {"汇报", "report"},
	"dispatch":     {"派活", "dispatch"},
	"task_start":   {"开工", "task"},
	"reply_person": {"回复", "reply"},
	"use_object":   {"工具", "tool"},
	"review":       {"评审", "review"},
}

// genericTypes name no occasion: a correction can be of anything.
var genericTypes = map[string]bool{"": true, "correction": true, "preference": true, "纠正": true}

// Counts summarises a source's pool: entries, active, standing, and Backfilled: read before the 0.2.0-beta.40 tagging
// rules and tagged by them now (Tags).
type Counts struct{ Entries, Active, Standing, Backfilled int }

// Page answers one memory.entries page of st. The order is standing first, then rank, then newest; after is the
// last answered entry's place, so pages stay consistent while entries come and go between them.
func Page(st *store.State, p Params) (Result, Counts, error) { return PageWith(st, nil, nil, p) }

// PageWithProfile is Page with the user profile summary's entries.
func PageWithProfile(st *store.State, sum *profile.Summary, p Params) (Result, Counts, error) {
	return PageWith(st, sum, nil, p)
}

// PageWith is Page with the profile summary's entries and extras answered among the curated entries. An extra whose
// id, facet or space the protocol would refuse, or whose text is empty or taken, is left out; its text, categories and
// subjects are cut to the protocol's bounds.
func PageWith(st *store.State, sum *profile.Summary, extras []Extra, p Params) (Result, Counts, error) {
	var from *key
	if p.After != "" {
		b, err := base64.RawURLEncoding.DecodeString(p.After)
		var k key
		if err != nil || json.Unmarshal(b, &k) != nil || k.I == "" {
			return Result{}, Counts{}, ErrBadAfter
		}
		from = &k
	}
	limit := min(max(p.Limit, 1), MaxLimit)
	var all []item
	var n Counts
	stand := standingSet(st, extras, p.ResidentBytes)
	for _, e := range st.Entries {
		if !Pageable(st, e) || !idRe.MatchString(e.ID) || strings.TrimSpace(recall.Line(e)) == "" {
			continue
		}
		it := item{e: e, rank: Rank(e), last: e.Last}
		if c, ok := stand[e.ID]; ok {
			it.standing, it.rank, it.text = true, c.rank, c.text
		} else if q := Quote(e); q != "" {
			it.text = clip(strings.TrimSpace(recall.Line(e)), MaxText-utf8.RuneCountInString(q)-1) + " " + q
		}
		all = append(all, it)
		n.Entries++
		if Status(e) == "active" {
			n.Active++
		}
		if it.standing {
			n.Standing++
		}
		if !Tagged(st, e) {
			n.Backfilled++
		}
	}
	for _, pit := range profileItems(sum) {
		all = append(all, pit)
		n.Entries++
		if pit.x.Status == "active" {
			n.Active++
		}
		if pit.standing {
			n.Standing++
		}
	}
	for _, x := range extras {
		e := x.Entry
		if !idRe.MatchString(e.ID) || strings.TrimSpace(e.Text) == "" || st.Entries[e.ID] != nil || strings.HasPrefix(e.ID, "profile:") {
			continue
		}
		e.Text = clip(strings.TrimSpace(e.Text), MaxText)
		if !facetRe.MatchString(e.Facet) {
			e.Facet = ""
		}
		if !spaceRe.MatchString(e.OriginSpace) {
			e.OriginSpace = ""
		}
		e.Categories = e.Categories[:min(len(e.Categories), MaxCats)]
		var subj []string
		for _, s := range e.Subjects {
			if s = strings.TrimSpace(s); s != "" && len(subj) < MaxSubjects {
				subj = append(subj, clip(s, MaxSubject))
			}
		}
		e.Subjects = subj
		e.Rank = min(max(e.Rank, 0), 1000)
		e.Standing = false
		if c, ok := stand[e.ID]; ok {
			e.Standing, e.Rank, e.Text = true, c.rank, c.text
		}
		if e.Status == "" {
			e.Status = "active"
		}
		all = append(all, item{x: &e, standing: e.Standing, rank: e.Rank, last: x.Last})
		n.Entries++
		if e.Status == "active" {
			n.Active++
		}
		if e.Standing {
			n.Standing++
		}
	}
	sort.Slice(all, func(i, j int) bool { return before(all[i].key(), all[j].key()) })
	start := 0
	if from != nil {
		start = sort.Search(len(all), func(i int) bool { return before(*from, all[i].key()) })
	}
	// Every entry's categories and space are decided (Tags), so the page keeps the protocol's promise: tagged.
	res := Result{Entries: []Entry{}, Tagged: true}
	size := 64
	for i := start; i < len(all); i++ {
		if len(res.Entries) == limit {
			res.HasMore = true
			break
		}
		x := entryOf(st, all[i])
		b, _ := json.Marshal(x)
		if size+len(b)+1 > MaxBytes && len(res.Entries) > 0 {
			res.HasMore = true
			break
		}
		size += len(b) + 1
		res.Entries = append(res.Entries, x)
		k, _ := json.Marshal(all[i].key())
		if a := base64.RawURLEncoding.EncodeToString(k); len(a) <= MaxAfter {
			res.After = a
		}
	}
	if !res.HasMore {
		res.After = ""
	}
	return res, n, nil
}

// profileItems converts a profile summary into memory.entries items. Profile summaries (general overview, six-facet
// summaries, and sensitive category summaries) are never standing (unified design §2.1): they stay in the recallable
// pool for on-demand lookup and UI views, with sensitive summaries carrying their exact categories and origin_space.
func profileItems(sum *profile.Summary) []item {
	if sum == nil || sum.SourceCount == 0 {
		return nil
	}
	day := sum.Day
	if day == "" && len(sum.GeneratedAt) >= 10 {
		day = sum.GeneratedAt[:10]
	}
	var out []item
	if strings.TrimSpace(sum.Overview) != "" {
		text := "用户侧写汇总（我是怎样的人）：" + strings.TrimSpace(sum.Overview)
		if len(sum.Recent) > 0 {
			text += " 近况：" + strings.Join(sum.Recent[:min(len(sum.Recent), 3)], "；")
		}
		e := Entry{
			ID:       "profile:overview",
			Text:     clip(text, MaxText),
			Facet:    string(entry.Persona),
			Status:   "active",
			Standing: false, // longer than the hub's standing room (1536 bytes): recalled, never standing (beta.52)
			Rank:     995,
			Subjects: []string{"用户侧写", "我是怎样的人", "关于我", "profile", "summary"},
		}
		out = append(out, item{x: &e, standing: e.Standing, rank: e.Rank, last: day})
	}
	facetKeywords := map[entry.Facet][]string{
		entry.Interaction: {"用户侧写", "和AI交互", "偏好", "规矩", "interaction"},
		entry.Tools:       {"用户侧写", "工具", "环境", "机器", "tools"},
		entry.Judgement:   {"用户侧写", "默认判断", "决策", "取舍", "judgement"},
		entry.Workflow:    {"用户侧写", "做事方式", "处理流程", "workflow"},
		entry.World:       {"用户侧写", "项目", "实体", "联系人", "world"},
		entry.Persona:     {"用户侧写", "关于我", "近况", "个人风格", "persona"},
	}
	facetTitles := map[entry.Facet]string{
		entry.Interaction: "用户侧写·和AI交互",
		entry.Tools:       "用户侧写·AI与工具",
		entry.Judgement:   "用户侧写·默认判断",
		entry.Workflow:    "用户侧写·处理流程与方式",
		entry.World:       "用户侧写·实体与来往的人",
		entry.Persona:     "用户侧写·人物理解与近况",
	}
	for _, sec := range sum.Sections {
		if len(sec.Points) == 0 {
			continue
		}
		id := "profile:" + string(sec.Facet)
		if !idRe.MatchString(id) {
			continue
		}
		// One entry per facet, recalled on its own: the section's synthesized description; the points joined only when
		// there is none (no model yet).
		text := strings.TrimSpace(sec.Summary)
		if text == "" {
			var parts []string
			for i, pt := range sec.Points {
				if i >= 5 {
					break
				}
				parts = append(parts, strings.TrimRight(strings.TrimSpace(pt.Text), "。.;；"))
			}
			text = strings.Join(parts, "；") + "。"
		}
		var b strings.Builder
		b.WriteString(facetTitles[sec.Facet])
		b.WriteString("：")
		b.WriteString(text)
		subjs := append([]string(nil), facetKeywords[sec.Facet]...)
		for _, s := range sec.Subjects {
			if len(subjs) >= MaxSubjects {
				break
			}
			if !slices.Contains(subjs, s) {
				subjs = append(subjs, clip(s, MaxSubject))
			}
		}
		e := Entry{
			ID:       id,
			Text:     clip(b.String(), MaxText),
			Facet:    string(sec.Facet),
			Status:   "active",
			Standing: false, // the overview entry is the standing one; each facet comes in by its subjects
			Rank:     990,
			Subjects: subjs,
		}
		out = append(out, item{x: &e, standing: e.Standing, rank: e.Rank, last: day})
	}
	for _, sens := range sum.Sensitive {
		if len(sens.Points) == 0 || sens.Category == "" {
			continue
		}
		id := "profile:cat:" + sens.Category
		if !idRe.MatchString(id) {
			continue
		}
		var b strings.Builder
		b.WriteString("用户侧写（敏感分类 ")
		b.WriteString(sens.Category)
		b.WriteString("）：")
		if strings.TrimSpace(sens.Summary) != "" {
			b.WriteString(strings.TrimSpace(sens.Summary))
		}
		e := Entry{
			ID:         id,
			Text:       clip(b.String(), MaxText),
			Facet:      string(entry.Persona),
			Status:     "active",
			Categories: []string{sens.Category},
			Standing:   false,
			Rank:       900,
			Subjects:   []string{"用户侧写", sens.Category, "profile"},
		}
		if spaceRe.MatchString(sens.OriginSpace) {
			e.OriginSpace = sens.OriginSpace
		}
		out = append(out, item{x: &e, standing: e.Standing, rank: e.Rank, last: day})
	}
	return out
}

// entryOf is what the protocol carries of an entry, with its categories and space (Tags). A space slug the protocol
// would refuse is left out; the entry's categories still grade it.
func entryOf(st *store.State, it item) Entry {
	if it.x != nil {
		return *it.x
	}
	e := it.e
	text := it.text
	if text == "" {
		text = strings.TrimSpace(recall.Line(e))
	}
	x := Entry{ID: e.ID, Text: clip(text, MaxText), Status: Status(e), Standing: it.standing,
		Rank: it.rank, Subjects: Subjects(st, e)}
	if f := string(e.Facet); facetRe.MatchString(f) {
		x.Facet = f
	}
	cats, space, _ := Tags(st, e)
	if spaceRe.MatchString(space) {
		x.OriginSpace = space
	}
	x.Categories = cats[:min(len(cats), MaxCats)]
	return x
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// standing is one entry of the standing set: its rank and its line.
type standing struct {
	rank int
	text string
}

// standingCache keeps the last standing set: the hub reads memory.entries a page at a time (a hundred pages and
// more), all of one snapshot; the set is worked out once per snapshot and its extras. A new snapshot (after a run, a
// mark, a delete: the light refresh) is a new state and works it out again.
var standingCache struct {
	sync.Mutex
	st  *store.State
	key string
	set map[string]standing
}

// standingSet is the standing set of st and the overview's extras (Resident), by id.
func standingSet(st *store.State, extras []Extra, room int) map[string]standing {
	var kb strings.Builder
	kb.WriteString(strconv.Itoa(room) + "|")
	kb.WriteString(nowFn().Format("2006-01-02") + "|")
	for _, x := range extras {
		kb.WriteString(x.Entry.ID)
		kb.WriteString(strconv.Itoa(x.Score))
		kb.WriteString(x.L1Kind)
		kb.WriteString(x.Polarity)
		kb.WriteString(x.Dimension)
		kb.WriteString(x.Entry.Text)
		kb.WriteString(x.Body)
		kb.WriteString(x.Suffix)
	}
	key := strconv.Itoa(len(st.Entries)) + "|" + kb.String()
	standingCache.Lock()
	defer standingCache.Unlock()
	if standingCache.st == st && standingCache.key == key {
		return standingCache.set
	}
	set := workStandingSet(st, extras, room)
	standingCache.st, standingCache.key, standingCache.set = st, key, set
	return set
}

// ResidentMostFor is how many lines a room holds: ResidentMost for the default, one more per 120 bytes beyond it.
func ResidentMostFor(room int) int {
	if room <= ResidentBytes {
		return ResidentMost
	}
	return ResidentMost + (room-ResidentBytes)/120
}

func workStandingSet(st *store.State, extras []Extra, room int) map[string]standing {
	var l1Cands, hotCands, coldCands []Candidate
	tpl := entry.TemplateQuotes(st.Entries)
	now := nowFn()

	// Collect L1 candidates across the three main sections (Profile, Principle, Contract) and track which L0 entries
	// are absorbed by a live L1 contract.
	absorbed := map[string]bool{}
	var contractTexts []string
	hasMainProfile, hasMainPrinciple, hasMainContract := false, false, false
	l1LatestDay := ""
	for _, x := range extras {
		e := x.Entry
		if x.Kind == "" || len(e.Categories) > 0 || e.OriginSpace != "" || st.Entries[e.ID] != nil || !idRe.MatchString(e.ID) ||
			entry.TemplateBacked(st.Entries, x.Evidence, tpl) {
			continue
		}
		if e.Facet == string(entry.Tools) {
			continue // Class C ops_runbook never enters global 4 KB standing
		}
		if x.Last > l1LatestDay {
			l1LatestDay = x.Last
		}
		bucket := BucketPrinciple
		maxBytes := PrincipleTextMost
		switch x.L1Kind {
		case BucketProfile:
			bucket = BucketProfile
			maxBytes = ProfileTextMost
		case BucketContract:
			bucket = BucketContract
			maxBytes = ContractTextMost
		case BucketPrinciple:
			bucket = BucketPrinciple
			maxBytes = PrincipleTextMost
		default:
			if x.Kind == entry.KindRules {
				bucket = BucketContract
				maxBytes = ContractTextMost
			}
		}
		prefix := StandingPrefix(bucket, x.Hard, x.Polarity)
		text := ""
		if strings.TrimSpace(x.Body) != "" {
			text = FormatResidentLine(prefix+strings.TrimSpace(x.Body), x.Suffix, maxBytes)
		} else {
			text = FormatResidentLine(prefix+strings.TrimSpace(e.Text), "", maxBytes)
		}
		if text == "" {
			continue
		}
		quotes := x.Quotes
		if quotes <= 0 {
			quotes = len(x.Evidence)
		}
		l1Cands = append(l1Cands, Candidate{
			ID:        e.ID,
			Text:      text,
			Kind:      x.Kind,
			Bucket:    bucket,
			Facet:     e.Facet,
			L1:        true,
			Hard:      x.Hard,
			Score:     x.Score,
			Days:      quotes,
			Last:      x.Last,
			Polarity:  x.Polarity,
			Dimension: x.Dimension,
			Group:     x.Group,
		})
		switch bucket {
		case BucketProfile:
			hasMainProfile = true
		case BucketContract:
			hasMainContract = true
			for _, id := range x.Evidence {
				absorbed[id] = true
			}
			for _, id := range x.CoveredIDs {
				absorbed[id] = true
			}
			contractTexts = append(contractTexts, text)
		default:
			hasMainPrinciple = true
		}
	}

	// Collect Bucket 0 (Hot Slot: genuinely user-owned Pinned + active 7d non-ops Fast-Path) and cold-start L0
	// fallback candidates per section (0 reliance on S6 assistant notes, 0 ops_runbook, 0 ephemeral, 0 sensitive).
	for _, e := range st.Entries {
		if !idRe.MatchString(e.ID) {
			continue
		}
		hc, ok := HotSlotCandidate(st, e, tpl, now)
		if !ok && hasMainContract && l1LatestDay != "" {
			hc, ok = UnabsorbedRuleHotCandidate(st, e, tpl, l1LatestDay, now)
		}
		if ok {
			if !hc.Pinned {
				if absorbed[e.ID] || slices.ContainsFunc(contractTexts, func(t string) bool { return norm.Jaccard(t, hc.Text) >= sameRule }) {
					continue // absorbed by L1 contract
				}
			}
			hotCands = append(hotCands, hc)
			continue
		}
		// Cold-start L0 fallback (v0.4.0 §1.7): T0 SelfReport/FromOnboarding or high-confidence T1 stable entries
		// (Count >= 2 for Persona/Interaction, or entry.ColdStartEligible) for any section lacking L1.
		coldOK := entry.ColdStartEligible(e) ||
			(e.Facet == entry.Persona && (entry.IsTrueUserOwned(e) || (e.Status == entry.Stable && e.Count >= 2))) ||
			(e.Facet == entry.Interaction && (entry.IsTrueUserOwned(e) || (e.Status == entry.Stable && e.Count >= 2)) && !entry.IsOpsRunbookEntry(e))
		if !coldOK {
			continue
		}
		if c, ok := CuratedCandidate(st, e, tpl); ok {
			coldCands = append(coldCands, c)
		}
	}

	cands := make([]Candidate, 0, len(hotCands)+len(l1Cands)+len(coldCands))
	cands = append(cands, hotCands...)
	cands = append(cands, l1Cands...)
	for _, c := range coldCands {
		if c.Bucket == BucketProfile && hasMainProfile {
			continue
		}
		if c.Bucket == BucketPrinciple && hasMainPrinciple {
			continue
		}
		if c.Bucket == BucketContract && hasMainContract {
			continue
		}
		cands = append(cands, c)
	}

	out := map[string]standing{}
	hotIdx, standIdx := 0, 0
	for _, c := range ResidentWithin(cands, room, ResidentMostFor(room)) {
		if c.Bucket == BucketHot {
			out[c.ID] = standing{rank: rankHot - hotIdx, text: c.Text}
			hotIdx++
		} else {
			out[c.ID] = standing{rank: rankStand - standIdx, text: c.Text}
			standIdx++
		}
	}
	return out
}
