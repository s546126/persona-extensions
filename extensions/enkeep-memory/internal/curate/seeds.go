package curate

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/verify"
)

// Seeds (unified memory design v2 §11.2; seed-import design): the user's own profile and rules confirmed elsewhere
// (another assistant's memory, a notes file, a profile platform) imported as a source of their own. A user with none
// does nothing and has none: this source is then simply empty. The source's switch (K21) is on by default, the
// user's call.
//
// What an import does:
//   - every item goes through three category filters (its source label and category, keywords in its text, and a
//     model's reading); what they find tags the entry (health, finance, relationships, emotion, legal_id) and the
//     exits use it by category and scene (0.2.0-beta.40, E9): nothing is left out for its category. A credential in
//     its text leaves it out, as does an item its source does not mark confirmed;
//   - an item that says what a live entry of the same facet already says (the same text, or nearly: sameSeedText) is
//     merged: the entry is marked as one the user confirmed elsewhere (flag "seed:<source>/<id>") and keeps its own
//     status and evidence;
//   - every other item becomes a candidate entry of origin Seed. It goes into the exits only once the user's own
//     records bear it out (an action filed under it on an active day, thresholds.go), never on the import alone;
//   - importing the same source again updates an item whose text changed there, and counts the rest as the same.
//
// enkeep_seed_import previews by default (nothing written, the model's sensitive check included) and writes only with
// apply; enkeep_seed_status says what the seeds are now.

// SeedFormat is the general import format: any source converted to it can be imported.
type SeedFormat struct {
	Source string     `json:"source"` // where the items come from: "enkeep", "notes", another memory's name
	Items  []SeedItem `json:"items"`
}

// SeedItem is one item of the general format.
type SeedItem struct {
	ID   string `json:"id"`   // the item's id at the source (an import of the same source again updates it)
	Kind string `json:"kind"` // rule | guardrail | preference | fact
	Text string `json:"text"`
	// Facet: interaction | tools | judgement | workflow | world | persona; empty: by kind (rule and guardrail
	// interaction, preference judgement, fact persona).
	Facet     string `json:"facet,omitempty"`
	Hard      bool   `json:"hard,omitempty"`     // a hard rule the user confirmed at the source
	Category  string `json:"category,omitempty"` // the source's own category
	Scope     string `json:"scope,omitempty"`    // "global" (default) or "project:<name>"
	Domain    string `json:"domain,omitempty"`   // work (default) | life
	Confirmed string `json:"confirmed"`          // YYYY-MM-DD the user confirmed it at the source; none: left out
	Effective string `json:"effective,omitempty"`
	Expiring  string `json:"expiring,omitempty"`  // recent | role | "" (see entry.SeedInfo)
	Sensitive string `json:"sensitive,omitempty"` // the source's own label, when it has one
}

// SeedReport is what an import did or would do (counts and per item an outcome, never the text).
type SeedReport struct {
	Source  string
	Applied bool
	New     int
	Merged  int
	Updated int
	Same    int
	Skipped map[string]int // reason → count
	Items   []SeedOutcome
}

// SeedOutcome is one item's outcome.
type SeedOutcome struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"` // new | merged | updated | same | skipped
	Reason  string `json:"reason,omitempty"`
	Facet   string `json:"facet,omitempty"`
	Entry   string `json:"entry,omitempty"`
	// Categories are the item's categories by the three filters (it is tagged with them, not left out).
	Categories []string `json:"categories,omitempty"`
}

// Reasons an item is left out.
const (
	SeedSensitive   = "sensitive"   // before 0.2.0-beta.40: an item in a K13 category (they are tagged now)
	SeedSecret      = "secret"      // a credential in the text
	SeedUnconfirmed = "unconfirmed" // the source does not say the user confirmed it
	SeedEmpty       = "empty"
	SeedBad         = "bad" // no id, an unknown kind, facet or scope
	SeedDuplicate   = "duplicate"
)

//go:embed seed_sensitive.md
var seedSensitivePrompt string

//go:embed seed_sensitive.schema.json
var seedSensitiveSchema []byte

// SeedSensitivePrompt is the version of the seeds' sensitive check.
const SeedSensitivePrompt = "seedsens/1"

// seedItemsMost bounds one import; seedBatch is the items per sensitive check.
const (
	seedItemsMost = 500
	seedBatch     = 40
	sameSeedText  = 0.85
)

// seedLabelRules maps a seed's explicit source category label for non-work facets (persona/world).
// Payload sensitivity is handled uniformly by entry.PayloadCategories (0.4.0 §1.6).
var seedLabelRules = []struct {
	cat   string
	label *regexp.Regexp
}{
	{entry.CatEmotion, regexp.MustCompile(`(?i)情感|感情|心理|emotion|mental`)},
	{entry.CatRelationships, regexp.MustCompile(`(?i)恋爱|婚姻|伴侣|家庭|家人|亲属|亲子|relationship|romance|dating|family|parent`)},
	{entry.CatFinance, regexp.MustCompile(`(?i)财务|财产|理财|收入|薪|finance|money|salary|income`)},
	{entry.CatHealth, regexp.MustCompile(`(?i)健康|医疗|疾病|身体|health|medical`)},
	{entry.CatLegalID, regexp.MustCompile(`(?i)证件|法律|legal|identity`)},
}

// seedCats computes an imported seed's categories under Payload-First (0.4.0 §1.6): always applies
// entry.PayloadCategories(facet, it.Text); source sensitive/category labels are added only for non-work facets.
func seedCats(facet entry.Facet, it SeedItem) []string {
	var out []string
	add := func(n string) {
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	for _, n := range entry.PayloadCategories(facet, it.Text) {
		add(n)
	}
	if !entry.IsWorkMethodFacet(facet) {
		add(entry.CategoryOf(it.Sensitive))
		for _, r := range seedLabelRules {
			if r.label.MatchString(it.Category) {
				add(r.cat)
			}
		}
	}
	return out
}

// seedFacet is an item's facet.
func seedFacet(it SeedItem) (entry.Facet, bool) {
	if f := entry.Facet(strings.TrimSpace(it.Facet)); f != "" {
		return f, f.Valid() && f != entry.People
	}
	switch it.Kind {
	case "rule", "guardrail":
		return entry.Interaction, true
	case "preference":
		return entry.Judgement, true
	case "fact":
		return entry.Persona, true
	}
	return "", false
}

// seedScopes is an item's scope.
func seedScopes(scope string) ([]entry.Scope, bool) {
	scope = strings.TrimSpace(scope)
	if scope == "" || scope == "global" {
		return []entry.Scope{{Kind: entry.ScopeGlobal}}, true
	}
	if name, ok := strings.CutPrefix(scope, "project:"); ok && strings.TrimSpace(name) != "" {
		return []entry.Scope{{Kind: entry.ScopeProject, Name: strings.TrimSpace(name)}}, true
	}
	return nil, false
}

// seedMark is the flag an entry merged with a seed carries.
func seedMark(source, id string) string { return "seed:" + source + "/" + id }

// ParseSeeds reads the general format.
func ParseSeeds(raw []byte) (SeedFormat, error) {
	var f SeedFormat
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("seed format: %w", err)
	}
	return f, CheckSeeds(&f)
}

// CheckSeeds checks the format's frame: a source name, not too many items.
func CheckSeeds(f *SeedFormat) error {
	f.Source = strings.TrimSpace(f.Source)
	if f.Source == "" || strings.ContainsAny(f.Source, "/ ") || len(f.Source) > 40 {
		return errors.New("seed format: source is a short name without spaces or slashes")
	}
	if len(f.Items) == 0 {
		return errors.New("seed format: no items")
	}
	if len(f.Items) > seedItemsMost {
		return fmt.Errorf("seed format: at most %d items at a time (%d given)", seedItemsMost, len(f.Items))
	}
	return nil
}

// ImportSeeds previews (apply false) or writes the items of f. The model reads every item that passed the other
// filters for the sensitive check, before the store's lock is taken; without a model nothing is imported. wait bounds
// the wait for the lock (0: as long as it takes).
func (c *Curator) ImportSeeds(ctx context.Context, f SeedFormat, apply bool, wait time.Duration) (SeedReport, error) {
	r := SeedReport{Source: f.Source, Applied: apply, Skipped: map[string]int{}}
	type cand struct {
		i     int
		it    SeedItem
		facet entry.Facet
		sc    []entry.Scope
		cats  []string
	}
	var cands []cand
	seen := map[string]bool{}
	skip := func(it SeedItem, why string) {
		r.Skipped[why]++
		r.Items = append(r.Items, SeedOutcome{ID: it.ID, Outcome: "skipped", Reason: why})
	}
	for i, it := range f.Items {
		it.ID, it.Text, it.Kind = strings.TrimSpace(it.ID), strings.TrimSpace(it.Text), strings.TrimSpace(it.Kind)
		facet, okFacet := seedFacet(it)
		sc, okScope := seedScopes(it.Scope)
		switch {
		case it.Text == "":
			skip(it, SeedEmpty)
		case it.ID == "" || !okFacet || !okScope || !slices.Contains([]string{"rule", "guardrail", "preference", "fact"}, it.Kind):
			skip(it, SeedBad)
		case seen[it.ID]:
			skip(it, SeedDuplicate)
		case strings.TrimSpace(it.Confirmed) == "":
			skip(it, SeedUnconfirmed)
		case verify.Secret(it.Text):
			skip(it, SeedSecret)
		default:
			cands = append(cands, cand{i, it, facet, sc, seedCats(facet, it)})
		}
		seen[it.ID] = true
	}
	// The third filter: the model reads what is left, and its category tags non-work facets (work-methodology facets
	// stay Payload-First to avoid topic/tone mislabeling).
	if len(cands) > 0 {
		if c.Model == nil {
			return r, errors.New("the sensitive check needs the model, which is not available: nothing imported")
		}
		texts := make([]string, len(cands))
		for i, x := range cands {
			texts[i] = x.it.Text
		}
		cats, err := c.seedSensitive(ctx, texts)
		if err != nil {
			return r, fmt.Errorf("the sensitive check: %w: nothing imported", err)
		}
		for i := range cands {
			if entry.IsWorkMethodFacet(cands[i].facet) {
				continue
			}
			if n := entry.CategoryOf(cats[i]); n != "" && !slices.Contains(cands[i].cats, n) {
				cands[i].cats = append(cands[i].cats, n)
			}
		}
	}
	if wait <= 0 {
		c.St.Lock()
	} else if !c.St.LockWithin(wait) {
		return r, ErrBusy
	}
	defer c.St.Unlock()
	s := c.St.S
	now := c.stamp()
	for _, x := range cands {
		it := x.it
		// The same item imported before: updated when its text changed at the source.
		if old := seedEntry(s, f.Source, it.ID); old != nil {
			if old.Seed.Text == it.Text {
				r.Same++
				r.Items = append(r.Items, SeedOutcome{ID: it.ID, Outcome: "same", Facet: string(old.Facet), Entry: old.ID, Categories: x.cats})
				if apply {
					tagSeed(old, x.cats)
				}
				continue
			}
			r.Updated++
			r.Items = append(r.Items, SeedOutcome{ID: it.ID, Outcome: "updated", Facet: string(old.Facet), Entry: old.ID, Categories: x.cats})
			if apply {
				c.updateSeed(old, it, x.sc, now)
				tagSeed(old, x.cats)
			}
			continue
		}
		if t := seedTwin(s, x.facet, it.Text); t != nil {
			r.Merged++
			r.Items = append(r.Items, SeedOutcome{ID: it.ID, Outcome: "merged", Facet: string(x.facet), Entry: t.ID, Categories: x.cats})
			if apply {
				tagSeed(t, x.cats)
			}
			if apply && !slices.Contains(t.Flags, seedMark(f.Source, it.ID)) {
				t.Flags = appendFlag(t.Flags, seedMark(f.Source, it.ID))
				t.Updated = now
				c.commit(entry.EvUser, t, "", "seed confirmed: "+f.Source, "")
			}
			continue
		}
		r.New++
		e := seedNew(f.Source, it, x.facet, x.sc, now)
		tagSeed(e, x.cats)
		r.Items = append(r.Items, SeedOutcome{ID: it.ID, Outcome: "new", Facet: string(x.facet), Entry: e.ID, Categories: x.cats})
		if apply {
			s.Entries[e.ID] = e
			c.commit(entry.EvCreate, e, "", "seed: "+f.Source, "")
		}
	}
	if apply {
		s.SeedImports = append(s.SeedImports, store.SeedImport{At: now, Source: f.Source, New: r.New, Merged: r.Merged,
			Updated: r.Updated, Same: r.Same, Skipped: r.Skipped})
		if err := c.St.Save(); err != nil {
			return r, err
		}
		c.Publish()
	}
	return r, nil
}

// seedEntry is the entry made from source's item id before, if any (deleted ones included: the user threw it away,
// and an import does not bring it back).
func seedEntry(s *store.State, source, id string) *entry.Entry {
	for _, k := range sortedKeys(s.Entries) {
		if e := s.Entries[k]; e.Origin == entry.Seed && e.Seed != nil && e.Seed.Source == source && e.Seed.ID == id {
			return e
		}
	}
	return nil
}

// seedTwin is a live entry of the facet saying the same as text: the same canonical text, or nearly the same.
func seedTwin(s *store.State, f entry.Facet, text string) *entry.Entry {
	q := norm.Q(text)
	var best *entry.Entry
	top := sameSeedText
	for _, k := range sortedKeys(s.Entries) {
		e := s.Entries[k]
		if e.Facet != f || !e.Live() || e.Origin == entry.Seed || e.Grain == entry.Action && e.Origin == entry.Behavior {
			continue
		}
		if norm.Q(e.Shown()) == q {
			return e
		}
		if j := norm.Jaccard(e.Shown(), text); j >= top {
			best, top = e, j
		}
	}
	return best
}

// seedNew is the candidate entry of an item.
func seedNew(source string, it SeedItem, f entry.Facet, sc []entry.Scope, now string) *entry.Entry {
	domain := entry.Work
	if it.Domain == entry.Life {
		domain = entry.Life
	}
	moment := "every_turn"
	switch {
	case sc[0].Kind == entry.ScopeProject:
		moment = "dispatch"
	case it.Kind == "fact" && f != entry.Persona:
		moment = "kb_lookup"
	}
	typ := it.Category
	if typ == "" {
		typ = it.Kind
	}
	e := &entry.Entry{Facet: f, Grain: entry.Class, Type: typ, Text: it.Text, Domain: domain, Scopes: sc,
		Impact: entry.Impact{Moment: moment, Action: it.Text}, Status: entry.Candidate, StatusAt: now, Origin: entry.Seed,
		OriginVersion: SeedSensitivePrompt, Updated: now, Flags: []string{seedMark(source, it.ID)},
		Seed: &entry.SeedInfo{Source: source, ID: it.ID, Kind: it.Kind, Hard: it.Hard, Category: it.Category,
			Confirmed: it.Confirmed, Effective: it.Effective, Expiring: it.Expiring, Imported: now, Text: it.Text}}
	e.ID = EntryID(f, entry.Class, it.Text, "")
	return e
}

// updateSeed takes the source's new text of an item imported before. An entry the user edited keeps the user's text
// (Shown); the seed's own text follows the source.
func (c *Curator) updateSeed(e *entry.Entry, it SeedItem, sc []entry.Scope, now string) {
	e.Text, e.Impact.Action = it.Text, it.Text
	e.Scopes = sc
	e.Seed.Text, e.Seed.Hard, e.Seed.Category, e.Seed.Confirmed = it.Text, it.Hard, it.Category, it.Confirmed
	e.Seed.Effective, e.Seed.Expiring, e.Seed.Imported = it.Effective, it.Expiring, now
	e.Updated = now
	c.commit(entry.EvUser, e, "", "seed updated: "+e.Seed.Source, "")
}

// tagSeed tags a seed's entry with the item's categories (by the plugin's filters: the model's reading and rules).
func tagSeed(e *entry.Entry, cats []string) {
	e.Tagged = true
	for _, n := range cats {
		e.AddCategory(n, entry.ByModel)
	}
}

// seedSensitive asks the model which texts are in one of the four categories: "" for none.
func (c *Curator) seedSensitive(ctx context.Context, texts []string) ([]string, error) {
	out := make([]string, len(texts))
	for from := 0; from < len(texts); from += seedBatch {
		to := min(from+seedBatch, len(texts))
		type item struct {
			I    int    `json:"i"`
			Text string `json:"text"`
		}
		var in []item
		for i := from; i < to; i++ {
			in = append(in, item{i - from, texts[i]})
		}
		user, _ := json.Marshal(map[string]any{"items": in})
		resp, err := c.Model.Complete(ctx, model.Request{Step: "seed_sensitive", Tier: model.Small, System: seedSensitivePrompt,
			User: string(user), MaxTokens: 2000, Schema: seedSensitiveSchema})
		if err != nil {
			return nil, err
		}
		var ans struct {
			Items []struct {
				I        int    `json:"i"`
				Category string `json:"category"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(extractJSON(resp.Text)), &ans); err != nil {
			return nil, fmt.Errorf("the answer is not JSON: %w", err)
		}
		answered := map[int]bool{}
		for _, a := range ans.Items {
			if a.I < 0 || a.I >= to-from {
				continue
			}
			answered[a.I] = true
			if cat := strings.ToLower(strings.TrimSpace(a.Category)); cat != "" {
				out[from+a.I] = cat
			}
		}
		for i := 0; i < to-from; i++ {
			if !answered[i] {
				out[from+i] = "unanswered" // not read: taken as sensitive, never imported unchecked
			}
		}
	}
	return out, nil
}

// SeedStatus is what the seeds are now.
type SeedStatus struct {
	On       bool                      // K21
	BySource map[string]map[string]int // source → status → entries (deleted counted as "deleted")
	ByFacet  map[string]int            // live seed entries per facet
	Merged   int                       // entries of the plugin's own marked as confirmed by a seed
	Usable   int                       // seed entries the exits use now
	Imports  []store.SeedImport
}

// Seeds reads what the seeds are now, from the pages' snapshot (never waiting for a run).
func (c *Curator) Seeds(usable func(*store.State, *entry.Entry) bool) SeedStatus {
	st, _, done := c.Read()
	defer done()
	out := SeedStatus{On: entry.On(st.Switches, entry.K21Seeds), BySource: map[string]map[string]int{}, ByFacet: map[string]int{},
		Imports: slices.Clone(st.SeedImports)}
	for _, e := range st.Entries {
		if e.Origin != entry.Seed {
			if slices.ContainsFunc(e.Flags, func(f string) bool { return strings.HasPrefix(f, "seed:") }) {
				out.Merged++
			}
			continue
		}
		src := "?"
		if e.Seed != nil {
			src = e.Seed.Source
		}
		if out.BySource[src] == nil {
			out.BySource[src] = map[string]int{}
		}
		status := string(e.Status)
		if e.Deleted {
			status = "deleted"
		} else if !e.Live() {
			status = "withdrawn"
		}
		out.BySource[src][status]++
		if e.Live() {
			out.ByFacet[string(e.Facet)]++
		}
		if usable != nil && usable(st, e) {
			out.Usable++
		}
	}
	return out
}

// Text says a status in a few lines.
func (s SeedStatus) Text() string {
	var b strings.Builder
	on := "on"
	if !s.On {
		on = "off (seed entries stay, none reaches the exits)"
	}
	fmt.Fprintf(&b, "Seeds as a source (K21): %s.", on)
	srcs := make([]string, 0, len(s.BySource))
	for k := range s.BySource {
		srcs = append(srcs, k)
	}
	sort.Strings(srcs)
	if len(srcs) == 0 {
		b.WriteString(" No seeds imported.")
	}
	for _, src := range srcs {
		var parts []string
		for _, st := range []string{"stable", "candidate", "shifting", "dormant", "withdrawn", "deleted", "retired"} {
			if n := s.BySource[src][st]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", st, n))
			}
		}
		fmt.Fprintf(&b, "\nSource %s: %s.", src, strings.Join(parts, ", "))
	}
	if len(s.ByFacet) > 0 {
		var parts []string
		for _, f := range entry.Facets {
			if n := s.ByFacet[string(f)]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", f, n))
			}
		}
		fmt.Fprintf(&b, "\nLive seed entries by facet: %s.", strings.Join(parts, ", "))
	}
	fmt.Fprintf(&b, "\nIn the exits now: %d (a seed goes out once the user's own records bear it out). Entries of the curator's own confirmed by a seed: %d.", s.Usable, s.Merged)
	for _, im := range s.Imports {
		fmt.Fprintf(&b, "\nImport %s from %s: new %d, merged %d, updated %d, same %d, skipped %s.", im.At, im.Source, im.New, im.Merged, im.Updated, im.Same, skippedText(im.Skipped))
	}
	return b.String()
}

func skippedText(m map[string]int) string {
	if len(m) == 0 {
		return "0"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// Text says a report: the counts, then each item's outcome by id.
func (r SeedReport) Text() string {
	var b strings.Builder
	verb := "Preview (nothing written; call again with apply true to write)"
	if r.Applied {
		verb = "Imported"
	}
	fmt.Fprintf(&b, "%s, source %s: new %d, merged %d, updated %d, same %d, skipped %s.", verb, r.Source, r.New, r.Merged, r.Updated, r.Same, skippedText(r.Skipped))
	b.WriteString(" New seed entries start as candidates: they reach the exits once the user's own records bear them out.")
	for _, it := range r.Items {
		line := fmt.Sprintf("\n- %s: %s", it.ID, it.Outcome)
		if it.Reason != "" {
			line += " (" + it.Reason + ")"
		}
		if it.Facet != "" {
			line += " " + it.Facet
		}
		if it.Entry != "" {
			line += " " + it.Entry
		}
		if len(it.Categories) > 0 {
			line += " [" + strings.Join(it.Categories, ",") + "]"
		}
		b.WriteString(line)
	}
	return b.String()
}
