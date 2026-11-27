package export

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

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/hubapi"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

// Options is what a compile needs. Model nil: no rewriting (an entry's own text goes out when it passes the check;
// people tips are left out). Hub nil: every exit is waiting for its hub change.
type Options struct {
	Dir   string // the data directory
	Model model.Model
	Tier  model.Tier
	Lang  string
	Loc   *time.Location
	Now   func() time.Time
	Hub   hubapi.Hub
	Run   string
	Event func(entry.Event) // the curator's event log: an entry going into an exit for the first time
	Logf  func(format string, args ...any)
	// Projects are the knowledge base's projects (kb.list "projects": the id is the slug the hub looks exit D files
	// up by). An entry's project names are matched to them by name, alias or id; a name that matches none gets no
	// project file (it stays in memory.entries). Nil: the list could not be read, and files are named by kb.Slug's
	// rule from the entry's own project name.
	Projects []entry.KBProject
}

// Report is what a compile did, for the run's notes, the page and the replay.
type Report struct {
	Files     map[string]FileReport `json:"files"`    // path → what is in it
	Sections  map[string]int        `json:"sections"` // exit A section → bytes
	SecLines  map[string]int        `json:"section_lines"`
	Rewritten int                   `json:"rewritten"`
	Reused    int                   `json:"reused"`
	Rejected  []string              `json:"rejected,omitempty"` // "<id>: <reasons>" of rewrites that failed the check
	Dropped   []string              `json:"dropped,omitempty"`  // entries left out for want of a line that passes
	Deferred  bool                  `json:"deferred,omitempty"` // exit A changed but was written today already
	Backfill  bool                  `json:"backfill,omitempty"` // the first-run replay is not done: nothing written
	Effective []string              `json:"effective,omitempty"`
}

// FileReport is one exit file.
type FileReport struct {
	Exit    string `json:"exit"`
	Bytes   int    `json:"bytes"`
	Entries int    `json:"entries"`
	Written bool   `json:"written"` // written by this compile (not unchanged, not deferred)
}

type kbEntry struct {
	slug    string
	name    string
	aliases []string
	keys    []string // normalized id, name, display-base, aliases, repo suffixes
}

type compiler struct {
	o      Options
	st     *store.State
	rep    *Report
	never  []string
	fw     words
	held   error               // the hub held the model (model.ErrPaused): nothing is written this time
	kb     map[string]*kbEntry // norm.Q of a project's id, name, slug and aliases → the project; nil when ambiguous
	kbList []*kbEntry          // all indexed projects, for unique-substring fallback
}

func (c *compiler) now() time.Time {
	if c.o.Now != nil {
		return c.o.Now()
	}
	return time.Now()
}

func (c *compiler) loc() *time.Location {
	if c.o.Loc != nil {
		return c.o.Loc
	}
	return time.UTC
}

func (c *compiler) logf(format string, args ...any) {
	if c.o.Logf != nil {
		c.o.Logf(format, args...)
	}
}

// Compile turns the entries into the exit files (§6), writes what changed, records State.Exits, Entry.Export and
// Entry.Effective, and reads back what the hub took. Call it with the store's lock held.
func Compile(ctx context.Context, st *store.State, o Options) (*Report, error) {
	c := &compiler{o: o, st: st, fw: fixed(o.Lang),
		rep: &Report{Files: map[string]FileReport{}, Sections: map[string]int{}, SecLines: map[string]int{}}}
	c.indexProjects()
	if st.Exits == nil {
		st.Exits = &entry.Exits{}
	}
	if st.Exits.Files == nil {
		st.Exits.Files = map[string]*entry.ExitFile{}
	}
	if st.Backfill != nil && st.Backfill.Finished == "" {
		// The first-run replay is still going: nothing is written to the assistant before it is done (4.1).
		c.rep.Backfill = true
		c.readback(ctx)
		return c.rep, nil
	}
	c.never = c.neverLines()
	p := c.plan()
	texts := c.texts(ctx, p.wants)
	if c.held != nil {
		// The hub held the model's connection past the call's wait (model_paused): the files stay as the last
		// compile wrote them, rather than go out with the lines that could not be rewritten left out; the rewrites
		// already made are kept on their entries and reused next time (0.2.0-beta.34).
		return c.rep, c.held
	}
	files := c.render(p, texts)
	if err := c.write(files); err != nil {
		return c.rep, err
	}
	c.readback(ctx)
	return c.rep, nil
}

// ---- which entries may go where -----------------------------------------------------------------------------------

// stable: only stable entries go out (a candidate never does). What the user wrote about themself, the first-run
// answers and the hand-written memory are stable from the start; a pinned entry counts as stable.
func stable(e *entry.Entry) bool {
	return e.Status == entry.Stable || e.Pinned || (entry.Hard(e) && e.Status == entry.Candidate) ||
		(e.Origin != entry.Behavior && e.Origin != "" && e.Origin != entry.Seed && e.Status == entry.Candidate)
}

// Stable says whether an entry counts as settled for the exits (stable, pinned, or written by the user).
func Stable(e *entry.Entry) bool { return stable(e) }

// usable is the gate every exit A–E shares: live (not deleted, retired, merged, invalidated or marked wrong), allowed
// in the resident exits by its categories and source space (entry.Resident: a category only once the user turned
// K22.<category> on; a domain space's content only as interaction or persona), stable, and not life content when
// the user chose "work only" at the first-run questions.
func (c *compiler) usable(e *entry.Entry) bool { return Usable(c.st, e) }

// Usable is the exits' shared gate for an entry of st (the overview card counts with it too).
func Usable(st *store.State, e *entry.Entry) bool {
	if !e.Live() || !e.Resident(st.Switches) || !stable(e) {
		return false
	}
	if e.Origin == entry.Seed && !entry.On(st.Switches, entry.K21Seeds) { // the seeds' source switched off
		return false
	}
	if e.Domain == entry.Life && st.Onboarding != nil && st.Onboarding.Use == entry.Work {
		return false
	}
	return true
}

func (c *compiler) on(key string) bool { return entry.On(c.st.Switches, key) }

func (c *compiler) entries(f entry.Facet, keep func(*entry.Entry) bool) []*entry.Entry {
	var out []*entry.Entry
	for _, e := range c.st.Entries {
		if e.Facet == f && c.usable(e) && (keep == nil || keep(e)) {
			out = append(out, e)
		}
	}
	sortEntries(out)
	return out
}

// sortEntries orders entries as every view does: count × source kinds, then last seen, then id.
func sortEntries(es []*entry.Entry) {
	sort.SliceStable(es, func(i, j int) bool {
		wi, wj := es[i].Count*max(1, len(es[i].Kinds)), es[j].Count*max(1, len(es[j].Kinds))
		if es[i].Pinned != es[j].Pinned {
			return es[i].Pinned
		}
		if wi != wj {
			return wi > wj
		}
		if es[i].Last != es[j].Last {
			return es[i].Last > es[j].Last
		}
		return es[i].ID < es[j].ID
	})
}

// lifted: a class or abstract entry, or one the user stated (③④⑥ action entries from behaviour are evidence).
func (c *compiler) lifted(e *entry.Entry) bool {
	if e.Grain == entry.Abstract {
		return c.on(entry.K3Abstract)
	}
	return e.Grain == entry.Class || (e.Origin != entry.Behavior && e.Origin != "")
}

func notHandwritten(e *entry.Entry) bool { return e.Origin != entry.Handwritten }

// underExported drops an action entry whose class is in the same list: the class says it already, and the member's
// line, written from one or two moments, tends to be the class again in vaguer words. What the user stated or
// pinned stays.
func underExported(es []*entry.Entry) []*entry.Entry {
	in := map[string]bool{}
	for _, e := range es {
		in[e.ID] = true
	}
	var out []*entry.Entry
	for _, e := range es {
		if e.Grain == entry.Action && e.Parent != "" && in[e.Parent] && !e.Pinned && (e.Origin == entry.Behavior || e.Origin == "") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func openCommitment(e *entry.Entry) bool { return e.Commitment != nil && e.Commitment.Done == "" }

type project struct {
	name     string
	aliases  []string
	parts    map[string][]*entry.Entry
	skeleton []string // 1-hop structural skeleton lines (owners, assignees, upstream/downstream dependencies)
}

type plan struct {
	a        map[string][]*entry.Entry // exit A section → entries
	objects  []string                  // ② objects, for the index line and the skill's description
	tools    []*entry.Entry            // exit B machines-and-tools
	projects map[string]*project       // exit D, by slug
	inbox    map[string][]*entry.Entry // exit E parts
	contacts []string                  // exit E: the people who matter most
	wants    []want
}

func (c *compiler) plan() *plan {
	p := &plan{a: map[string][]*entry.Entry{}, projects: map[string]*project{}, inbox: map[string][]*entry.Entry{}}
	add := func(e *entry.Entry, mode, exit string) { p.wants = append(p.wants, want{e: e, mode: mode, exit: exit}) }

	// Exit A (K1). People (⑤′) never; the hand-written memory is in the assistant's context already.
	// What holds only in a knowledge-base project goes to that project's block (exit D), not to every turn: it would be
	// applied everywhere else (0.2.0-beta.37). Without the project list it stays in exit A as before.
	general := func(e *entry.Entry) bool {
		return notHandwritten(e) && !(c.on(entry.K6WorkflowSkill) && c.inProject(e))
	}
	if c.on(entry.K1PerTurnBlock) {
		p.a["interaction"] = underExported(c.entries(entry.Interaction, general))
		p.a["judgement"] = underExported(c.entries(entry.Judgement, func(e *entry.Entry) bool { return general(e) && (c.lifted(e) || forkReady(e)) }))
		// ④: every stable way of working that holds across projects (the ways-of-working skills are gone); an
		// abstract entry only while K3 is on.
		p.a["workflow"] = underExported(c.entries(entry.Workflow, func(e *entry.Entry) bool {
			return general(e) && (e.Grain != entry.Abstract || c.on(entry.K3Abstract))
		}))
		p.a["world"] = c.entries(entry.World, openCommitment)
		if c.on(entry.K4Persona) {
			p.a["persona"] = underExported(c.entries(entry.Persona, func(e *entry.Entry) bool {
				return notHandwritten(e) && (c.lifted(e) || entry.SelfFact(e))
			}))
		}
		for _, s := range []string{"interaction", "judgement", "workflow", "persona"} {
			for _, e := range p.a[s] {
				add(e, ModePlain, "A")
			}
		}
	}
	// Exit B (K5, K6): every stable ② entry by object; every stable ④ entry by kind of task.
	if c.on(entry.K5ToolsSkill) {
		p.tools = c.entries(entry.Tools, notHandwritten)
		seen := map[string]bool{}
		for _, e := range p.tools {
			if o := objectOf(e); o != "" && !seen[strings.ToLower(o)] {
				seen[strings.ToLower(o)] = true
				p.objects = append(p.objects, o)
			}
			add(e, ModePlain, "B")
		}
	}
	// Exit D (K6): per knowledge-base project, its goals, project-specific rules (①②③④), facts (⑤) and 1-hop
	// structural skeleton (owners and upstream/downstream dependencies). Nothing of people cards or ⑥.
	if c.on(entry.K6WorkflowSkill) {
		for _, f := range []entry.Facet{entry.Interaction, entry.Tools, entry.Judgement, entry.Workflow, entry.World} {
			for _, e := range c.entries(f, nil) {
				for _, r := range c.projectsFor(e) {
					pr := p.projects[r.slug]
					if pr == nil {
						pr = &project{name: r.name, aliases: r.aliases, parts: map[string][]*entry.Entry{}}
						p.projects[r.slug] = pr
					}
					pr.parts[string(f)] = append(pr.parts[string(f)], e)
					if !openCommitment(e) {
						add(e, ModePlain, "D")
					}
				}
			}
		}
		for slug, pr := range p.projects {
			pr.skeleton = c.projectSkeleton(slug, pr)
		}
	}
	// Exit E: who matters and open commitments (⑤, ⑤′ commitments), how to communicate with them (⑤′ tips, K14; inferred
	// ones K15), the user's identity and way of communicating (⑥, K4), ①'s communication requirements.
	p.inbox["world"] = c.entries(entry.World, openCommitment)
	if c.on(entry.K14People) {
		p.contacts = c.contactNames(5)
		p.inbox["world"] = append(p.inbox["world"], c.entries(entry.People, func(e *entry.Entry) bool {
			return e.Part == entry.PartCommitment && openCommitment(e)
		})...)
		for _, e := range c.entries(entry.People, func(e *entry.Entry) bool {
			return e.Part != entry.PartCommitment && e.Impact.Moment == "reply_person" && c.name(e.Person) != "" &&
				(e.Part != entry.PartInferred || c.on(entry.K15Inferred)) && !(e.Inferred && !c.on(entry.K15Inferred))
		}) {
			p.inbox["people"] = append(p.inbox["people"], e)
			add(e, ModeTip, "E")
		}
	}
	if c.on(entry.K4Persona) {
		ps := c.entries(entry.Persona, func(e *entry.Entry) bool { return c.lifted(e) || e.Impact.Moment == "reply_person" })
		sort.SliceStable(ps, func(i, j int) bool {
			return ps[i].Impact.Moment == "reply_person" && ps[j].Impact.Moment != "reply_person"
		})
		p.inbox["persona"] = ps
		for _, e := range ps {
			add(e, ModePlain, "E")
		}
	}
	p.inbox["interaction"] = c.entries(entry.Interaction, func(e *entry.Entry) bool { return e.Impact.Moment == "reply_person" })
	for _, e := range p.inbox["interaction"] {
		add(e, ModePlain, "E")
	}
	return p
}

// forkReady: an action-grain judgement that says its fork, the choice and what was not chosen or why may go into
// exit A (0.2.0-beta.37): the grain gate alone left most judgements out.
func forkReady(e *entry.Entry) bool {
	return strings.TrimSpace(e.Situation) != "" && strings.TrimSpace(e.Choice) != "" &&
		(strings.TrimSpace(e.Rejected) != "" || strings.TrimSpace(e.Why) != "")
}

// indexProjects indexes the knowledge base's projects by id, normalized slug, name, display-base, aliases and repo
// suffix (norm.Q); a key two projects share leads to neither. Projects whose raw ID is not a valid hub slug fall back
// to KBSlug(id) or KBSlug(name) so Chinese/Unicode project names are never silently dropped.
func (c *compiler) indexProjects() {
	if c.o.Projects == nil {
		return
	}
	c.kb = map[string]*kbEntry{}
	c.kbList = nil
	for i := range c.o.Projects {
		pr := &c.o.Projects[i]
		slug := strings.TrimSpace(pr.ID)
		if !projectSlug(slug) {
			if s := KBSlug(slug); projectSlug(s) {
				slug = s
			} else if s := KBSlug(pr.Name); projectSlug(s) {
				slug = s
			} else {
				continue
			}
		}
		ke := &kbEntry{slug: slug, name: firstNonEmpty(pr.Name, slug), aliases: pr.Aliases}
		c.kbList = append(c.kbList, ke)
		seen := map[string]bool{}
		addKey := func(raw string) {
			k := norm.Q(raw)
			if k == "" || seen[k] {
				return
			}
			seen[k] = true
			ke.keys = append(ke.keys, k)
			if old, ok := c.kb[k]; ok && old != ke {
				c.kb[k] = nil
				return
			}
			c.kb[k] = ke
		}
		raws := append([]string{pr.ID, slug, pr.Name, KBSlug(pr.Name)}, pr.Aliases...)
		for _, n := range raws {
			addKey(n)
			if base := stripProjectDisplay(n); base != "" && base != n {
				addKey(base)
			}
			if _, tail, ok := strings.Cut(strings.TrimSpace(n), "/"); ok && tail != "" {
				addKey(tail)
			}
		}
	}
}

type projRef struct {
	slug, name string
	aliases    []string
}

func (c *compiler) matchKBProject(name string) *kbEntry {
	name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "project:"))
	if name == "" {
		return nil
	}
	candidates := []string{name}
	if base := stripProjectDisplay(name); base != "" && base != name {
		candidates = append(candidates, base)
	}
	if sl := KBSlug(name); sl != "" {
		candidates = append(candidates, sl)
	}
	if _, tail, ok := strings.Cut(name, "/"); ok && tail != "" {
		candidates = append(candidates, tail)
	}
	for _, cand := range candidates {
		k := norm.Q(cand)
		if k == "" {
			continue
		}
		if ke, ok := c.kb[k]; ok {
			return ke // nil if ambiguous across two KB projects
		}
	}
	// Unique substring match for Chinese/Unicode or descriptive project names (>= 2 runes):
	// only accepted when exactly one KB project matches, never when ambiguous.
	q := norm.Q(stripProjectDisplay(name))
	if len([]rune(q)) < 2 {
		return nil
	}
	var hit *kbEntry
	for _, ke := range c.kbList {
		matched := false
		for _, k := range ke.keys {
			if len([]rune(k)) >= 2 && (strings.Contains(q, k) || strings.Contains(k, q)) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if hit != nil && hit != ke {
			return nil // ambiguous
		}
		hit = ke
	}
	return hit
}

// projectsFor are the project files an entry goes into: its project names matched to knowledge-base projects, or,
// without the project list, named by kb.Slug's rule.
func (c *compiler) projectsFor(e *entry.Entry) []projRef {
	var out []projRef
	names := projectsOf(e)
	if c.o.Projects != nil {
		if o := strings.TrimSpace(e.Impact.Object); o != "" && len([]rune(o)) <= 24 && !slices.Contains(names, o) {
			names = append(names, o)
		}
		if o := strings.TrimSpace(e.Object); o != "" && !slices.Contains(names, o) {
			names = append(names, o)
		}
	}
	for _, name := range names {
		var r projRef
		if c.o.Projects != nil {
			ke := c.matchKBProject(name)
			if ke == nil {
				continue
			}
			r = projRef{slug: ke.slug, name: ke.name, aliases: ke.aliases}
		} else if s := KBSlug(name); projectSlug(s) {
			r = projRef{slug: s, name: name}
		} else {
			continue
		}
		if !slices.ContainsFunc(out, func(x projRef) bool { return x.slug == r.slug }) {
			out = append(out, r)
		}
	}
	return out
}

// projectSkeleton collects 1-hop structural skeleton lines (owners, assignees, upstream/downstream dependencies)
// for a project from st.Graph and usable World skeleton entries.
func (c *compiler) projectSkeleton(slug string, pr *project) []string {
	norms := map[string]bool{}
	for _, n := range append([]string{slug, pr.name}, pr.aliases...) {
		if q := norm.Q(n); q != "" {
			norms[q] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = oneLine(s)
		if s == "" || seen[s] || len(out) >= 8 {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	zh := norm.Base(c.o.Lang) == "zh"
	formatEdge := func(from, pred, to string) string {
		if from == "" || to == "" || from == to {
			return ""
		}
		if zh {
			switch pred {
			case "owns":
				return fmt.Sprintf("负责人：%s 负责 %s", from, to)
			case "assigned":
				return fmt.Sprintf("分配：%s 承接 %s", from, to)
			case "depends_on":
				return fmt.Sprintf("依赖：%s 依赖 %s", from, to)
			case "member_of":
				return fmt.Sprintf("归属：%s 属于 %s", from, to)
			case "collaborates_with":
				return fmt.Sprintf("协作：%s 与 %s 协作", from, to)
			case "role":
				return fmt.Sprintf("角色：%s（%s）", from, to)
			case "reports_to":
				return fmt.Sprintf("汇报：%s 汇报给 %s", from, to)
			}
		}
		return fmt.Sprintf("%s %s %s", from, pred, to)
	}
	if c.st.Graph != nil {
		g := c.st.Graph
		var nodeIDs []string
		for id, n := range g.Nodes {
			if n == nil || n.MergedInto != "" || n.Status == "not_entity" {
				continue
			}
			matched := norms[norm.Q(n.Name)]
			if !matched {
				for _, a := range n.Aliases {
					if norms[norm.Q(a.Name)] {
						matched = true
						break
					}
				}
			}
			if matched {
				nodeIDs = append(nodeIDs, id)
			}
		}
		sort.Strings(nodeIDs)
		for _, id := range nodeIDs {
			for _, a := range g.About(id, func(a *store.Assertion) bool {
				return (a.Status == "active" || a.Status == "candidate") && entry.IsSkeletonPredicate(a.P)
			}) {
				sn, on := g.Nodes[g.Resolve(a.S)], g.Nodes[g.Resolve(a.O)]
				if sn == nil || on == nil || sn.ID == "me" || on.ID == "me" {
					continue
				}
				add(formatEdge(strings.TrimSpace(sn.Name), strings.ToLower(strings.TrimSpace(a.P)), strings.TrimSpace(on.Name)))
			}
		}
	}
	for _, e := range pr.parts["world"] {
		if !entry.IsSkeletonPredicate(e.Predicate) {
			continue
		}
		from := c.cleanRef(e.Subject)
		to := c.cleanRef(e.Target)
		add(formatEdge(from, strings.ToLower(strings.TrimSpace(e.Predicate)), to))
	}
	return out
}

func (c *compiler) cleanRef(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "person:") {
		if n := c.name(raw); n != "" {
			return n
		}
	}
	for _, p := range []string{"project:", "person:", "system:", "object:"} {
		raw = strings.TrimPrefix(raw, p)
	}
	return strings.TrimSpace(raw)
}

// inProject: the entry holds in a project the knowledge base knows, so its project block carries it.
func (c *compiler) inProject(e *entry.Entry) bool {
	return c.o.Projects != nil && len(c.projectsFor(e)) > 0
}

// objectOf is the machine or tool a ② entry is about.
func objectOf(e *entry.Entry) string {
	if o := strings.TrimSpace(e.Object); o != "" {
		return o
	}
	for _, s := range e.Scopes {
		if s.Kind == entry.ScopeObject && s.Name != "" {
			return s.Name
		}
	}
	return ""
}

// projectsOf lists the projects an entry holds in: project scopes, a ⑤ relation's project, ④⑤'s Object.
func projectsOf(e *entry.Entry) []string {
	var out []string
	addName := func(n string) {
		n = strings.TrimSpace(strings.TrimPrefix(n, "project:"))
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	for _, s := range e.Scopes {
		if s.Kind == entry.ScopeProject {
			addName(s.Name)
		}
	}
	for _, x := range []string{e.Subject, e.Target} {
		if strings.HasPrefix(x, "project:") {
			addName(x)
		}
	}
	if (e.Facet == entry.Workflow || e.Facet == entry.World) && e.Object != "" && len(e.Scopes) == 0 {
		addName(e.Object)
	}
	return out
}

func (c *compiler) name(person string) string {
	if k := c.st.Contacts[person]; k != nil && k.Manual != "removed" {
		return k.Name
	}
	return ""
}

func (c *compiler) contactNames(n int) []string {
	var ks []*entry.Contact
	for _, k := range c.st.Contacts {
		if k.Manual != "removed" && k.Name != "" {
			ks = append(ks, k)
		}
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].Score != ks[j].Score {
			return ks[i].Score > ks[j].Score
		}
		return ks[i].Person < ks[j].Person
	})
	var out []string
	for _, k := range ks {
		if len(out) == n {
			break
		}
		out = append(out, k.Name)
	}
	return out
}

// neverLines are the cards' evaluation and inferred lines, as written and as the user edited them, and the user's
// own words they rest on: never in a draft (exit E's never_quote.txt, checked by the hub), never in exits A, B, D.
// Lines in a category the resident exits keep out are not listed either (no exit file carries them; no draft can
// copy what it never gets).
func (c *compiler) neverLines() []string {
	var es []*entry.Entry
	for _, e := range c.st.Entries {
		if e.Facet == entry.People && (e.Part == entry.PartUserEval || e.Part == entry.PartInferred) && e.Live() && e.Resident(c.st.Switches) {
			es = append(es, e)
		}
	}
	sortEntries(es)
	var out []string
	seen := map[string]bool{}
	addLine := func(s string) {
		for _, l := range strings.Split(s, "\n") {
			if l = strings.TrimSpace(l); l != "" && !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	for _, e := range es {
		addLine(e.Text)
		addLine(e.Shown())
		if e.Part == entry.PartUserEval {
			for _, ev := range e.Evidence {
				if ev.Who == entry.WhoSelf {
					addLine(ev.Quote)
				}
			}
		}
	}
	return out
}

// ---- writing ------------------------------------------------------------------------------------------------------

type file struct {
	path, exit string
	content    string
	ids        []string
}

// manifest is exits.json: what each exit file holds, for the page and the readback.
type manifest struct {
	Compiled string               `json:"compiled"`
	Exits    map[string]exitSum   `json:"exits"`
	Files    map[string]*manEntry `json:"files"`
}

type exitSum struct {
	Hash    string   `json:"hash"`
	Bytes   int      `json:"bytes"`
	Files   []string `json:"files"`
	Entries []string `json:"entries"`
}

type manEntry struct {
	Exit    string   `json:"exit"`
	Hash    string   `json:"hash"`
	Bytes   int      `json:"bytes"`
	Written string   `json:"written"`
	Entries []string `json:"entries"`
}

func sum(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func (c *compiler) readManifest() *manifest {
	m := &manifest{}
	if b, err := os.ReadFile(filepath.Join(c.o.Dir, ManifestPath)); err == nil {
		_ = json.Unmarshal(b, m)
	}
	if m.Files == nil {
		m.Files = map[string]*manEntry{}
	}
	return m
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// write puts the files on disk: only those whose hash changed; exit A at most once a day unless the change only
// takes entries out, or the file out there still carries an entry that may no longer go out (a withdrawal goes out
// at once, even when the same compile also adds lines); files no longer made are removed.
func (c *compiler) write(files []file) error {
	old := c.readManifest()
	now := c.now()
	stamp := now.UTC().Format(time.RFC3339)
	today := now.In(c.loc()).Format("2006-01-02")
	next := &manifest{Compiled: stamp, Exits: map[string]exitSum{}, Files: map[string]*manEntry{}}
	made := map[string]bool{}
	for _, f := range files {
		made[f.path] = true
		h := sum(f.content)
		prev := old.Files[f.path]
		full := filepath.Join(c.o.Dir, f.path)
		info, statErr := os.Stat(full)
		onDisk := statErr == nil && prev != nil && int(info.Size()) == prev.Bytes
		if prev != nil && prev.Hash == h && onDisk {
			next.Files[f.path] = prev
			c.rep.Files[f.path] = FileReport{Exit: f.exit, Bytes: prev.Bytes, Entries: len(prev.Entries)}
			continue
		}
		if f.path == InstructionsPath && prev != nil && onDisk && sameDay(prev.Written, today, c.loc()) && !subset(f.ids, prev.Entries) &&
			!c.stale(prev.Entries) {
			next.Files[f.path] = prev // once a day (H1 restarts the assistant on a change); withdrawals are not held back
			c.rep.Deferred = true
			c.rep.Files[f.path] = FileReport{Exit: f.exit, Bytes: prev.Bytes, Entries: len(prev.Entries)}
			continue
		}
		if err := writeFile(full, []byte(f.content)); err != nil {
			return err
		}
		next.Files[f.path] = &manEntry{Exit: f.exit, Hash: h, Bytes: len(f.content), Written: stamp, Entries: f.ids}
		c.rep.Files[f.path] = FileReport{Exit: f.exit, Bytes: len(f.content), Entries: len(f.ids), Written: true}
	}
	for path, prev := range old.Files {
		if made[path] {
			continue
		}
		full := filepath.Join(c.o.Dir, path)
		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if prev.Exit == "B" {
			_ = os.Remove(filepath.Dir(full)) // the skill's directory, when empty
		}
	}
	// What is out now: entries take effect the first time they are in a file (5.1 counts recurrences from here).
	paths := make([]string, 0, len(next.Files))
	for p := range next.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	c.st.Exits.Files = map[string]*entry.ExitFile{}
	for _, path := range paths {
		m := next.Files[path]
		c.st.Exits.Files[path] = &entry.ExitFile{Path: path, Hash: m.Hash, Bytes: m.Bytes, Written: m.Written, Entries: len(m.Entries)}
		sumx := next.Exits[m.Exit]
		sumx.Files = append(sumx.Files, path)
		sumx.Bytes += m.Bytes
		sumx.Entries = append(sumx.Entries, m.Entries...)
		next.Exits[m.Exit] = sumx
		var before []string
		if p := old.Files[path]; p != nil {
			before = p.Entries
		}
		for _, id := range m.Entries {
			e := c.st.Entries[id]
			if e == nil {
				continue
			}
			if e.Effective == "" {
				e.Effective = stamp
				c.rep.Effective = append(c.rep.Effective, id)
			}
			if !slices.Contains(before, id) && c.o.Event != nil {
				c.o.Event(entry.Event{At: stamp, Run: c.o.Run, Kind: entry.EvExport, Entry: id, Detail: m.Exit + ":" + path})
			}
		}
	}
	for x, s := range next.Exits {
		var parts []string
		for _, p := range s.Files {
			parts = append(parts, p+"="+next.Files[p].Hash)
		}
		s.Hash = sum(strings.Join(parts, "\n"))
		s.Entries = uniqSorted(s.Entries)
		next.Exits[x] = s
	}
	b, _ := json.MarshalIndent(next, "", " ")
	return writeFile(filepath.Join(c.o.Dir, ManifestPath), b)
}

// stale: one of ids (what a file out there carries) is gone or may no longer go out (wrong, deleted, sensitive, not
// stable any more). Before 0.2.0-beta.38 such a file waited for the next day when the same compile also added a line.
func (c *compiler) stale(ids []string) bool {
	for _, id := range ids {
		if e := c.st.Entries[id]; e == nil || !c.usable(e) {
			return true
		}
	}
	return false
}

func sameDay(written, today string, loc *time.Location) bool {
	t, err := time.Parse(time.RFC3339, written)
	return err == nil && t.In(loc).Format("2006-01-02") == today
}

func subset(xs, of []string) bool {
	for _, x := range xs {
		if !slices.Contains(of, x) {
			return false
		}
	}
	return true
}

func uniqSorted(xs []string) []string {
	xs = append([]string(nil), xs...)
	sort.Strings(xs)
	return slices.Compact(xs)
}

// ---- readback -----------------------------------------------------------------------------------------------------

// exitOf maps the hub's readback names to exits.
var exitOf = map[string]string{"instructions": "A", "skills": "B", "project_context": "D", "inbox_context": "E"}

var exitLabel = map[string]string{"A": "每轮说明块", "B": "技能", "D": "Agent 项目背景", "E": "收件箱"}

// readback asks the hub what it took (H1/H4/H5). Until it can say, every exit waits for its hub change; once it can,
// a version not taken 24 hours after it was written, a cut version, or a refusal is a warning on the page and in the
// weekly report (6.4).
func (c *compiler) readback(ctx context.Context) {
	ex := c.st.Exits
	ex.Warnings = nil
	waitAll := func() {
		ex.Waiting = map[string]string{}
		for _, x := range []string{"A", "B", "D", "E", "F"} {
			ex.Waiting[x] = Needs[x]
		}
	}
	if c.o.Hub == nil {
		waitAll()
		return
	}
	ads, err := c.o.Hub.Adoptions(ctx)
	if errors.Is(err, hubapi.ErrWaitingHub) {
		waitAll()
		return
	}
	if err != nil {
		ex.Warnings = append(ex.Warnings, "读不到 hub 的采纳情况："+err.Error())
		return
	}
	ex.Waiting = nil
	if ex.Adopted == nil {
		ex.Adopted = map[string]entry.Adoption{}
	}
	for _, a := range ads {
		x := exitOf[a.Exit]
		if x == "" {
			continue
		}
		ad := entry.Adoption{Hash: a.Hash, Truncated: a.Truncated, Reason: a.Reason, State: a.State, Current: a.Current}
		if !a.At.IsZero() {
			ad.At = a.At.UTC().Format(time.RFC3339)
		}
		if !a.Earliest.IsZero() {
			ad.Earliest = a.Earliest.UTC().Format(time.RFC3339)
		}
		ex.Adopted[x] = ad
	}
	m := c.readManifest()
	now := c.now()
	for _, x := range []string{"A", "B", "D", "E"} {
		s, ok := m.Exits[x]
		if !ok || s.Bytes == 0 {
			continue
		}
		latest := ""
		for _, p := range s.Files {
			if w := m.Files[p].Written; w > latest {
				latest = w
			}
		}
		ad, took := ex.Adopted[x]
		label := exitLabel[x]
		if took && ad.Truncated {
			ex.Warnings = append(ex.Warnings, fmt.Sprintf("hub 截断了%s（出口 %s）", label, x))
		}
		if took && (ad.Current || ad.Hash == s.Hash) {
			continue
		}
		if took && ad.Reason == "rate_limited" {
			// The hub's 24-hour window for instructions: expected, not a failure; only a version that has waited past a
			// day is warned about below.
		} else if took && ad.Reason != "" {
			ex.Warnings = append(ex.Warnings, fmt.Sprintf("hub 没有采纳%s（出口 %s）：%s", label, x, ad.Reason))
			continue
		}
		if t, err := time.Parse(time.RFC3339, latest); err == nil && now.Sub(t) > 24*time.Hour {
			if took {
				ex.Warnings = append(ex.Warnings, fmt.Sprintf("%s（出口 %s）写出 24 小时后，hub 采纳的还是旧版本", label, x))
			} else {
				ex.Warnings = append(ex.Warnings, fmt.Sprintf("%s（出口 %s）写出 24 小时后，hub 还没有采纳", label, x))
			}
		}
	}
}
