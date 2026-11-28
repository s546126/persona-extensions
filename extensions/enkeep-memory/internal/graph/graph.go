// Package graph builds the entity and relation graph out of verified daily assertions: it resolves names to
// nodes (deterministically first), keeps every relation as a dated, cited assertion, supersedes instead of
// overwriting, and ages what nobody mentions any more.
package graph

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/schema"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/voice"
)

// Types are the built-in persistent node types allowed unconditionally in v0.4.0; the configuration may add custom
// non-disallowed types.
var Types = []string{"person", "org", "project", "system", "repo"}

// ConditionalTypes may become persistent nodes only when observed across >= MinConditionalSessions sessions with
// at least one structural skeleton edge.
var ConditionalTypes = []string{"topic", "resource"}

// MinConditionalSessions is the minimum distinct sessions required before a topic or resource node is created.
const MinConditionalSessions = 3

// DisallowedTypes are ephemeral/noisy entity types that must never become persistent graph nodes in v0.4.0.
var DisallowedTypes = []string{"task", "meeting", "chat", "doc", "decision"}

// IsPersistentNodeType reports whether typ is one of the 5 core persistent entity types.
func IsPersistentNodeType(typ string) bool {
	return slices.Contains(Types, strings.ToLower(strings.TrimSpace(typ)))
}

// IsConditionalNodeType reports whether typ requires cross-session structural evidence before node creation.
func IsConditionalNodeType(typ string) bool {
	return slices.Contains(ConditionalTypes, strings.ToLower(strings.TrimSpace(typ)))
}

// IsDisallowedNodeType reports whether typ is forbidden from creating persistent graph nodes.
func IsDisallowedNodeType(typ string) bool {
	return slices.Contains(DisallowedTypes, strings.ToLower(strings.TrimSpace(typ)))
}

// AllowConditionalNode reports whether a conditional node type (topic/resource) satisfies the v0.4.0 admission bar
// (seen across >= 3 sessions and connected by at least one structural edge).
func AllowConditionalNode(typ string, sessions int, hasStructuralEdge bool) bool {
	return IsConditionalNodeType(typ) && sessions >= MinConditionalSessions && hasStructuralEdge
}

// typePrefixes are leading "type:" prefixes stripped by CanonicalName. Never strip URL schemes like http:/https:.
var typePrefixes = []string{
	"person:", "people:", "project:", "projects:", "system:", "repo:", "task:", "org:",
	"topic:", "resource:", "doc:", "docs:", "decision:", "meeting:", "chat:", "object:", "service:",
	"person：", "people：", "project：", "projects：", "system：", "repo：", "task：", "org：",
	"topic：", "resource：", "doc：", "docs：", "decision：", "meeting：", "chat：", "object：", "service：",
	"系统:", "系统：", "项目:", "项目：", "人物:", "人物：", "任务:", "任务：", "仓库:", "仓库：",
}

// CanonicalName normalizes a raw entity surface before graph resolution or write: strips leading "type:" prefixes
// (person:/project:/system:/repo:/task:/...), trims surrounding whitespace, and collapses internal whitespace.
func CanonicalName(raw, typ string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	for {
		lower := strings.ToLower(s)
		stripped := false
		for _, p := range typePrefixes {
			if strings.HasPrefix(lower, p) {
				rest := strings.TrimSpace(s[len(p):])
				if rest != "" {
					s = rest
					stripped = true
				}
				break
			}
		}
		if !stripped {
			break
		}
	}
	return strings.Join(strings.Fields(s), " ")
}

// nonPersonSuffixRe matches a name that ends in a word saying it is software or a service, not a person
// ("enkeep-memory 插件", "persona-cli", "AsterGate 网关"). An ASCII suffix needs a separator so a name merely ending in
// those letters is left alone.
var nonPersonSuffixRe = regexp.MustCompile(`(?i)^(.*?\S)(?:\s*(插件|扩展|系统|平台|网关|服务|仓库|代码库)|[\s_-]+(plugin|extension|system|platform|gateway|service|server|cli|repo|sdk))$`)

// MisfiledPersonType says what a name typed as a person really is when its suffix says software or a service:
// "system" for a system, platform, gateway or service, "repo" for a repository, "project" for a plugin, extension,
// CLI or SDK; base is the name without that suffix. ("", "") when the name may well be a person's.
func MisfiledPersonType(name string) (typ, base string) {
	m := nonPersonSuffixRe.FindStringSubmatch(strings.TrimSpace(CanonicalName(name, "person")))
	if m == nil {
		return "", ""
	}
	base = strings.TrimSpace(m[1])
	if len([]rune(norm.Q(base))) < 2 {
		return "", ""
	}
	switch strings.ToLower(m[2] + m[3]) {
	case "系统", "平台", "网关", "服务", "system", "platform", "gateway", "service", "server":
		return "system", base
	case "仓库", "代码库", "repo":
		return "repo", base
	}
	return "project", base
}

// NormKey returns the normalized comparison key for an entity name after CanonicalName prefix stripping.
func NormKey(raw, typ string) string {
	return norm.Q(CanonicalName(raw, typ))
}

var (
	localPathRe = regexp.MustCompile(`(?i)^(/Users/|/home/|/tmp/|/var/|/etc/|/opt/|/private/|~/|\./|\.\./|[a-z]:\\)`)
	fileExtRe   = regexp.MustCompile(`(?i)/(?:[^/\s]+/)*[^/\s]+\.(go|md|json|jsonl|ya?ml|py|tsx?|jsx?|txt|sh|sql|swift|kt|rs|toml|xml|csv|pdf|docx?|xlsx?)$`)
	issuePRRe   = regexp.MustCompile(`(?i)^(?:(?:PR|pull\s*request|MR|issue|ticket|bug)\s*#?\d+|#\d{1,7})$`)
)

// LooksLikeLocalPath reports whether name is a local filesystem path rather than a persistent entity.
func LooksLikeLocalPath(name string) bool {
	s := strings.TrimSpace(name)
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return false
	}
	return localPathRe.MatchString(s) || fileExtRe.MatchString(s)
}

// LooksLikeIssueOrPR reports whether name is a bare Issue or PR identifier (e.g. "PR #1234", "Issue #42", "#6100").
func LooksLikeIssueOrPR(name string) bool {
	return issuePRRe.MatchString(strings.TrimSpace(name))
}

// Predicates are the built-in relations. Functional ones have one current value per subject: a new value
// supersedes the old. Noise predicates ("about", "same_circle") are removed in v0.4.0.
var Predicates = []Predicate{
	{schema.Predicate{Name: "owns", Meaning: "is responsible for", Subject: []string{"person", "org"}, Object: []string{"project", "system", "repo"}}, false},
	{schema.Predicate{Name: "member_of", Meaning: "belongs to", Subject: []string{"person", "system", "repo", "project"}, Object: []string{"org", "project"}}, false},
	{schema.Predicate{Name: "reports_to", Meaning: "reports to", Subject: []string{"person"}, Object: []string{"person"}}, true},
	{schema.Predicate{Name: "depends_on", Meaning: "depends on", Subject: []string{"project", "system", "repo"}, Object: []string{"system", "resource", "project", "repo"}}, false},
	{schema.Predicate{Name: "blocked_by", Meaning: "is held up by", Subject: []string{"project", "system", "repo"}, Object: []string{"person", "resource", "system", "project"}}, true},
	{schema.Predicate{Name: "waiting_on", Meaning: "waits for a reply or action from", Subject: []string{"project", "system"}, Object: []string{"person", "org"}}, true},
	{schema.Predicate{Name: "decided", Meaning: "made the decision", Subject: []string{"person", "org"}, Object: []string{"project", "system", "repo"}}, false},
	{schema.Predicate{Name: "committed", Meaning: "promised to do (attrs.to: for whom, attrs.due)", Subject: []string{"person"}, Object: []string{"task"}}, false},
	{schema.Predicate{Name: "assigned", Meaning: "was given ownership of (attrs.by: by whom, attrs.due)", Subject: []string{"person"}, Object: []string{"project", "system", "repo"}}, false},
	{schema.Predicate{Name: "decides", Meaning: "has the final say on", Subject: []string{"person", "org"}, Object: []string{"project", "system", "repo", "resource"}}, false},
	{schema.Predicate{Name: "collaborates_with", Meaning: "works with (attrs.on: on what)", Subject: []string{"person", "org"}, Object: []string{"person", "org"}}, false},
	{schema.Predicate{Name: "allocated", Meaning: "is allocated to (attrs.quantity)", Subject: []string{"resource"}, Object: []string{"project", "org", "person"}}, false},
}

// Predicate is a relation type.
type Predicate struct {
	schema.Predicate
	Functional bool
}

// Registry is the types and relations in force: built-in plus the user's.
type Registry struct {
	Types      []string
	Predicates map[string]Predicate
}

// NewRegistry adds custom types ("name" or "name: meaning") and predicates
// ("name: subj1|subj2 -> obj1|obj2: meaning") to the built-in ones. Malformed entries are returned as problems.
func NewRegistry(customTypes, customPredicates []string) (*Registry, []string) {
	r := &Registry{Types: slices.Clone(Types), Predicates: map[string]Predicate{}}
	for _, p := range Predicates {
		r.Predicates[p.Name] = p
	}
	var problems []string
	for _, t := range customTypes {
		name, _, _ := strings.Cut(t, ":")
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || strings.ContainsAny(name, " |>") || IsDisallowedNodeType(name) {
			problems = append(problems, "类型写法不对："+t)
			continue
		}
		if !slices.Contains(r.Types, name) {
			r.Types = append(r.Types, name)
		}
	}
	for _, p := range customPredicates {
		parts := strings.SplitN(p, ":", 3)
		if len(parts) < 2 {
			problems = append(problems, "关系写法不对："+p)
			continue
		}
		name := strings.ToLower(strings.TrimSpace(parts[0]))
		subj, obj, ok := strings.Cut(parts[1], "->")
		if name == "" || !ok || IsStoppedPredicate(name) {
			problems = append(problems, "关系写法不对："+p)
			continue
		}
		pr := Predicate{Predicate: schema.Predicate{Name: name, Subject: splitTypes(subj), Object: splitTypes(obj)}}
		if len(parts) == 3 {
			pr.Meaning = strings.TrimSpace(parts[2])
		}
		r.Predicates[name] = pr
	}
	return r, problems
}

func splitTypes(s string) []string {
	var out []string
	for _, t := range strings.Split(s, "|") {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// List is the predicates for the prompt, in a stable order.
func (r *Registry) List() []schema.Predicate {
	var out []schema.Predicate
	for _, p := range r.Predicates {
		out = append(out, p.Predicate)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Builder changes a graph.
type Builder struct {
	G     *store.Graph
	Reg   *Registry
	Owner []string
	Today string
}

// Me is the owner's node.
const Me = "me"

// generic names never become aliases (a chat name or a word like "周会" is not a project's name).
var generic = []string{"周会", "例会", "这个需求", "这个项目", "项目", "需求", "群", "大家", "所有人", "全体成员", "team", "the team", "project", "this", "meeting", "weekly", "standup"}

// EnsureMe makes the owner's node.
func (b *Builder) EnsureMe() {
	if b.G.Nodes[Me] == nil {
		b.G.Nodes[Me] = &store.Node{ID: Me, Type: "person", Name: "我", Status: "active", Created: b.Today, LastSeen: b.Today}
	}
	n := b.G.Nodes[Me]
	// Generic words the hub may list as the owner's name ("用户", "owner") are never aliases: every "用户" in a
	// product chat would resolve to the owner.
	for _, o := range voice.Names(b.Owner) {
		if !hasAlias(n, o) {
			n.Aliases = append(n.Aliases, store.Alias{Name: o, From: "owner"})
		}
	}
}

// Seed adds a knowledge-base entity as a node (or finds the one that already points at it). Nodes are made only for
// allowed persistent entity types that take part in a relation, so a group's ten thousand silent members never become
// nodes and disallowed types (task/meeting/chat/doc/decision) are never seeded.
func (b *Builder) Seed(typ, ref, name string, aliases []string) string {
	typ = strings.ToLower(strings.TrimSpace(typ))
	if IsDisallowedNodeType(typ) {
		return ""
	}
	clean := CanonicalName(name, typ)
	if clean == "" || LooksLikeLocalPath(clean) || LooksLikeIssueOrPR(clean) {
		return ""
	}
	for _, n := range b.G.Nodes {
		if n.Status == "archived" || n.Status == "not_entity" {
			continue
		}
		if slices.Contains(n.Refs, ref) {
			return b.G.Resolve(n.ID)
		}
	}
	if id := b.find(clean, typ); id != "" {
		n := b.G.Nodes[id]
		if !slices.Contains(n.Refs, ref) {
			n.Refs = append(n.Refs, ref)
		}
		return id
	}
	if b.Reg != nil && !slices.Contains(b.Reg.Types, typ) && !IsPersistentNodeType(typ) {
		return ""
	}
	id := b.newNode(typ, clean, "active")
	n := b.G.Nodes[id]
	n.Refs = []string{ref}
	for _, a := range aliases {
		ca := CanonicalName(a, typ)
		if ca != "" && !isGeneric(ca) && !hasAlias(n, ca) {
			n.Aliases = append(n.Aliases, store.Alias{Name: ca, From: "kb"})
		}
	}
	return id
}

func (b *Builder) newNode(typ, name, status string) string {
	b.G.NextNode++
	id := fmt.Sprintf("n-%s-%d", typ, b.G.NextNode)
	b.G.Nodes[id] = &store.Node{ID: id, Type: typ, Name: name, Status: status, Created: b.Today, LastSeen: b.Today}
	return id
}

// find resolves a name to a node: the owner's names first, then exact names and aliases (canonical form after prefix
// stripping) of the same type (or any type when typ is empty). Ambiguous names resolve to nothing.
func (b *Builder) find(name, typ string) string {
	clean := CanonicalName(name, typ)
	q := norm.Q(clean)
	if q == "" {
		return ""
	}
	if clean == norm.Me || slices.ContainsFunc(voice.Names(b.Owner), func(o string) bool { return norm.Q(o) == q }) {
		return Me
	}
	var hits []string
	for _, n := range b.G.Nodes {
		if n.MergedInto != "" || n.Status == "not_entity" || n.Status == "archived" || (typ != "" && n.Type != typ) {
			continue
		}
		if NormKey(n.Name, n.Type) == q || slices.ContainsFunc(n.Aliases, func(a store.Alias) bool { return NormKey(a.Name, n.Type) == q }) {
			hits = append(hits, n.ID)
		}
	}
	sort.Strings(hits)
	if len(hits) == 1 {
		return hits[0]
	}
	return ""
}

// Resolve turns a mention into a persistent node, making a candidate node when nothing matches and the type passes
// the v0.4.0 persistent whitelist (person/org/project/system/repo or valid custom type). Conditional types
// (topic/resource) require ResolveConditional with >= 3 sessions and a structural edge; disallowed types
// (task/meeting/chat/doc/decision), local file paths and Issue/PR identifiers are rejected.
func (b *Builder) Resolve(surface, typ, knownID string, ev *store.Evidence) (string, bool) {
	typ = strings.ToLower(strings.TrimSpace(typ))
	clean := CanonicalName(surface, typ)
	if knownID != "" {
		if n := b.G.Nodes[knownID]; n != nil && n.Status != "archived" && n.Status != "not_entity" {
			resolved := b.G.Resolve(knownID)
			if rn := b.G.Nodes[resolved]; rn != nil && rn.Status != "archived" && !IsDisallowedNodeType(rn.Type) {
				return resolved, true
			}
		}
		for _, n := range b.G.Nodes {
			if n.Status == "archived" || n.Status == "not_entity" || IsDisallowedNodeType(n.Type) {
				continue
			}
			if slices.Contains(n.Refs, knownID) {
				return b.G.Resolve(n.ID), true
			}
		}
	}
	if IsDisallowedNodeType(typ) || LooksLikeLocalPath(clean) || LooksLikeIssueOrPR(clean) {
		return "", false
	}
	if typ == "person" {
		// "enkeep-memory 插件" is the project, not a person: resolve it to the project/system/repo of its base name,
		// or make it with the type its suffix says.
		if t, base := MisfiledPersonType(clean); t != "" {
			if id := b.find(base, ""); id != "" {
				if n := b.G.Nodes[id]; n != nil && (n.Type == "project" || n.Type == "system" || n.Type == "repo") {
					return id, true
				}
			}
			typ = t
		}
	}
	if id := b.find(clean, typ); id != "" {
		return id, true
	}
	if id := b.find(clean, ""); id != "" {
		if n := b.G.Nodes[id]; n != nil && !IsDisallowedNodeType(n.Type) {
			return id, true
		}
	}
	if IsConditionalNodeType(typ) {
		return "", false
	}
	if b.Reg == nil || !slices.Contains(b.Reg.Types, typ) || isGeneric(clean) {
		return "", false
	}
	id := b.newNode(typ, clean, "candidate")
	b.G.Nodes[id].Aliases = []store.Alias{{Name: clean, Evidence: ev, From: "extract"}}
	return id, true
}

// ResolveConditional resolves or creates a conditional node (topic/resource) only when it appears across at least
// MinConditionalSessions sessions and participates in a structural edge.
func (b *Builder) ResolveConditional(surface, typ string, sessions int, hasStructuralEdge bool, ev *store.Evidence) (string, bool) {
	typ = strings.ToLower(strings.TrimSpace(typ))
	clean := CanonicalName(surface, typ)
	if !IsConditionalNodeType(typ) || LooksLikeLocalPath(clean) || LooksLikeIssueOrPR(clean) || isGeneric(clean) {
		return "", false
	}
	if id := b.find(clean, typ); id != "" {
		return id, true
	}
	if !AllowConditionalNode(typ, sessions, hasStructuralEdge) {
		return "", false
	}
	id := b.newNode(typ, clean, "candidate")
	b.G.Nodes[id].Aliases = []store.Alias{{Name: clean, Evidence: ev, From: "extract"}}
	return id, true
}

// TypeOf is a node's type.
func (b *Builder) TypeOf(id string) string {
	if n := b.G.Nodes[b.G.Resolve(id)]; n != nil {
		return n.Type
	}
	return ""
}

// Names are the names a node goes by.
func (b *Builder) Names(id string) []string {
	n := b.G.Nodes[b.G.Resolve(id)]
	if n == nil {
		return nil
	}
	out := []string{n.Name}
	for _, a := range n.Aliases {
		out = append(out, a.Name)
	}
	if n.ID == Me {
		out = append(out, voice.Names(b.Owner)...)
	}
	return out
}

// Add records a verified assertion and returns its id and what happened: "new", "duplicate" (same relation,
// more evidence), "superseded" (it replaced an older value), "history" (older than the current value, kept as
// history), "conflict" (a different value on the same day: both wait for a person), or "skipped" (stopped
// predicate or self-loop). collaborates_with is normalized as an undirected edge min(s,o) -> max(s,o).
func (b *Builder) Add(s, p, o string, attrs map[string]string, when string, ev []store.Evidence, conf string, flags []string, prompt string) (string, string) {
	p = TypeName(p)
	if IsStoppedPredicate(p) {
		return "", "skipped"
	}
	s, o = b.G.Resolve(s), b.G.Resolve(o)
	if s == "" || o == "" || s == o {
		return "", "skipped"
	}
	if p == "collaborates_with" && s > o {
		s, o = o, s
	}
	day := ""
	if len(ev) > 0 {
		day = ev[0].Day
	}
	valid := when
	if valid == "" {
		valid = day
	}
	id := assertionID(s, p, o)
	a := b.G.Assertions[id]
	if b.Reg != nil {
		if pr, ok := b.Reg.Predicates[p]; ok && pr.Functional && a != nil {
			// A functional value can come back (B, then C, then B again), so a triple can have several periods. The
			// one this value's time falls in takes the evidence. When that period was replaced before this time, the
			// value starts a new period of its own, which supersedes the current value like any new one; the earlier
			// period stays as history.
			a = b.periodAt(a, valid)
			id = a.ID
			if a.Status == "superseded" && a.ValidTo != "" && valid > a.ValidTo {
				id, a = periodID(s, p, o, valid), nil
			}
		}
	}
	if a != nil {
		for _, e := range ev {
			if !slices.ContainsFunc(a.Evidence, func(x store.Evidence) bool { return x.Chat == e.Chat && x.Day == e.Day && x.N == e.N }) {
				a.Evidence = append(a.Evidence, e)
			}
		}
		a.Observed = day
		if a.Status == "stale" {
			a.Status = "active"
		}
		b.promote(a)
		return id, "duplicate"
	}
	a = &store.Assertion{ID: id, S: s, P: p, O: o, Attrs: attrs, ValidFrom: valid, Observed: day, Confidence: conf,
		Source: "extract", Evidence: ev, Flags: flags, Prompt: prompt, Ruleset: schema.Ruleset, Status: "candidate"}
	b.promote(a)
	outcome := "new"
	if b.Reg != nil {
		if pr, ok := b.Reg.Predicates[p]; ok && pr.Functional {
			for _, old := range b.G.Assertions {
				if old.S != s || old.P != p || old.O == o || (old.Status != "active" && old.Status != "candidate") {
					continue
				}
				switch {
				case old.ValidFrom < valid:
					old.Status, old.ValidTo = "superseded", valid
					a.Supersedes = old.ID
					outcome = "superseded"
				case old.ValidFrom > valid:
					a.Status, a.ValidTo = "superseded", old.ValidFrom
					outcome = "history"
				default:
					old.Status, a.Status = "candidate", "candidate"
					outcome = "conflict"
				}
			}
		}
	}
	b.G.Assertions[id] = a
	for _, nid := range []string{s, o} {
		if n := b.G.Nodes[nid]; n != nil {
			n.LastSeen = day
			if n.Status == "candidate" && a.Status == "active" {
				n.Status = "active"
			}
		}
	}
	return id, outcome
}

// promote: candidate → active when the evidence is plain (high confidence, no flags) or comes from two independent
// sources (different chats, or different people).
func (b *Builder) promote(a *store.Assertion) {
	if a.Status != "candidate" {
		return
	}
	if (a.Confidence == "high" && len(a.Flags) == 0) || independent(a.Evidence) >= 2 {
		a.Status = "active"
	}
}

func independent(ev []store.Evidence) int {
	seen := map[string]bool{}
	for _, e := range ev {
		seen[e.Chat+"\x00"+e.Speaker] = true
	}
	return len(seen)
}

// Age marks relations nobody mentioned for `days` as stale (functional ones stay: they hold until replaced), and
// closes tasks whose due date passed long ago only as a flag; nothing is deleted.
func (b *Builder) Age(today string, days int, before func(day string, days int) string) (staled int) {
	cut := before(today, days)
	for _, a := range b.G.Assertions {
		if a.Status != "active" {
			continue
		}
		if b.Reg != nil {
			if pr, ok := b.Reg.Predicates[a.P]; ok && pr.Functional {
				continue
			}
		}
		if a.Observed < cut {
			a.Status = "stale"
			staled++
		}
	}
	return staled
}

// Merge records that from is the same as into. Assertions keep their subject; queries follow MergedInto.
// The merged node's name is recorded as a reversible alias on into (tagged "merge:<from>") so Unmerge restores
// the exact pre-merge state.
func (b *Builder) Merge(from, into, proposal string) error {
	from, into = b.G.Resolve(from), b.G.Resolve(into)
	if from == into || b.G.Nodes[from] == nil || b.G.Nodes[into] == nil {
		return fmt.Errorf("cannot merge %s into %s", from, into)
	}
	if from == Me {
		from, into = into, from
	}
	fn, tn := b.G.Nodes[from], b.G.Nodes[into]
	fn.MergedInto = into
	tag := "merge:" + from
	if fn.Name != "" && !hasAlias(tn, fn.Name) {
		tn.Aliases = append(tn.Aliases, store.Alias{Name: fn.Name, From: tag})
	}
	for _, al := range fn.Aliases {
		if al.Name != "" && !hasAlias(tn, al.Name) {
			tn.Aliases = append(tn.Aliases, store.Alias{Name: al.Name, From: tag})
		}
	}
	b.G.Merges = append(b.G.Merges, &store.MergeRec{From: from, Into: into, At: b.Today, Proposal: proposal})
	return nil
}

// Unmerge undoes the last merge of from, restoring MergedInto and removing aliases added by that merge.
func (b *Builder) Unmerge(from string) error {
	for i := len(b.G.Merges) - 1; i >= 0; i-- {
		m := b.G.Merges[i]
		if m.From == from && m.Undone == "" {
			m.Undone = b.Today
			if fn := b.G.Nodes[from]; fn != nil {
				fn.MergedInto = ""
			}
			if tn := b.G.Nodes[m.Into]; tn != nil {
				tag := "merge:" + from
				tn.Aliases = slices.DeleteFunc(tn.Aliases, func(a store.Alias) bool { return a.From == tag })
			}
			return nil
		}
	}
	return fmt.Errorf("%s was not merged", from)
}

// SameNameCandidates are pairs of nodes of one type whose names are the same in canonical form (after prefix
// stripping) but were not merged (two knowledge-base people with one name, a candidate node beside a seeded one):
// merge proposals.
func (b *Builder) SameNameCandidates() [][2]string {
	by := map[string][]string{}
	for _, n := range b.G.Nodes {
		if n.MergedInto != "" || n.Status == "not_entity" || n.Status == "archived" || n.ID == Me {
			continue
		}
		key := NormKey(n.Name, n.Type)
		if key == "" {
			continue
		}
		by[n.Type+"\x00"+key] = append(by[n.Type+"\x00"+key], n.ID)
	}
	var out [][2]string
	for _, ids := range by {
		sort.Strings(ids)
		for i := 1; i < len(ids); i++ {
			out = append(out, [2]string{ids[i], ids[0]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func assertionID(s, p, o string) string {
	h := sha1.Sum([]byte(s + "\x00" + p + "\x00" + o))
	return "a-" + hex.EncodeToString(h[:6])
}

// periodID is the id of a later period of a functional relation's value: the triple again from valid on.
func periodID(s, p, o, valid string) string {
	h := sha1.Sum([]byte(s + "\x00" + p + "\x00" + o + "\x00" + valid))
	return "a-" + hex.EncodeToString(h[:6])
}

// periodAt is the period of first's triple that valid falls in: the last one starting on or before it (first, the
// earliest, when valid is before them all). A new period starts only after the last one ended, so starts never tie.
func (b *Builder) periodAt(first *store.Assertion, valid string) *store.Assertion {
	out := first
	for _, x := range b.G.Assertions {
		if x.S == first.S && x.P == first.P && x.O == first.O && x.ValidFrom <= valid && x.ValidFrom > out.ValidFrom {
			out = x
		}
	}
	return out
}

func hasAlias(n *store.Node, a string) bool {
	q := NormKey(a, n.Type)
	if q == "" {
		return true
	}
	return NormKey(n.Name, n.Type) == q || slices.ContainsFunc(n.Aliases, func(x store.Alias) bool { return NormKey(x.Name, n.Type) == q })
}

func isGeneric(s string) bool {
	q := norm.Q(s)
	if len([]rune(q)) < 2 {
		return true
	}
	return slices.ContainsFunc(generic, func(g string) bool { return norm.Q(g) == q })
}
