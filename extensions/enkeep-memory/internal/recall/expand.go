package recall

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/graph"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

const (
	quoteSliceMost  = 80 // max runes per L0 user original quote slice
	quotesPerEntity = 2  // max L0 user original quote slices per entity/hit
	maxMatchedEnts  = 3  // cap matched entities per recall query
	maxSkeletonOut  = 12 // cap 1-hop skeleton edges per entity
	maxRulesPerEnt  = 8  // cap local rules/decisions attached to an EntityContext
)

// SkeletonEdge is one 1-hop structural relation edge around a matched entity.
type SkeletonEdge struct {
	From      string
	Predicate string
	To        string
	ValidFrom string
}

// EntityContext is the 1-hop expansion of a matched entity (person, project, repo, machine, system):
// its structural skeleton edges, local rules and decisions anchored on it, and up to 2 deduplicated
// L0 user original quote slices (<=80 runes, with date).
type EntityContext struct {
	Name     string
	Type     string
	Skeleton []SkeletonEdge
	Rules    []Hit
	Quotes   []string
}

// skeletonIdx caches the 1-hop structural skeleton adjacency of a store.Graph snapshot for O(1) neighbor lookup.
type skeletonIdx struct {
	g          *store.Graph
	nodes      int
	assertions int
	merges     int
	undone     int
	archived   int
	// byNorm maps graph.NormKey(entity name or alias, including merged-in aliases) to its resolved canonical node ids.
	byNorm map[string][]string
	// adj maps resolved node id to its sorted 1-hop skeleton edges (from, predicate, to, validFrom, otherID, otherName).
	adj map[string][]adjEdge
}

type adjEdge struct {
	edge      SkeletonEdge
	otherID   string
	otherName string
}

var skelCache struct {
	mu  sync.Mutex
	idx *skeletonIdx
}

func graphSnapshotCounts(g *store.Graph) (nodes, assertions, merges, undone, archived int) {
	nodes = len(g.Nodes)
	assertions = len(g.Assertions)
	merges = len(g.Merges)
	for _, m := range g.Merges {
		if m != nil && m.Undone != "" {
			undone++
		}
	}
	for _, n := range g.Nodes {
		if n != nil && (n.Status == "archived" || n.MergedInto != "") {
			archived++
		}
	}
	for _, a := range g.Assertions {
		if a != nil && a.Status == "archived" {
			archived++
		}
	}
	return
}

func getSkeletonIdx(st *store.State) *skeletonIdx {
	if st == nil || st.Graph == nil {
		return nil
	}
	g := st.Graph
	nNodes, nAsserts, nMerges, nUndone, nArch := graphSnapshotCounts(g)
	skelCache.mu.Lock()
	if skelCache.idx != nil && skelCache.idx.g == g &&
		skelCache.idx.nodes == nNodes && skelCache.idx.assertions == nAsserts &&
		skelCache.idx.merges == nMerges && skelCache.idx.undone == nUndone &&
		skelCache.idx.archived == nArch {
		idx := skelCache.idx
		skelCache.mu.Unlock()
		return idx
	}
	skelCache.mu.Unlock()

	idx := buildSkeletonIdx(g, nNodes, nAsserts, nMerges, nUndone, nArch)
	skelCache.mu.Lock()
	skelCache.idx = idx
	skelCache.mu.Unlock()
	return idx
}

func isActiveGraphNode(n *store.Node) bool {
	if n == nil || n.MergedInto != "" || n.Status == "not_entity" || n.Status == "archived" {
		return false
	}
	return !graph.IsDisallowedNodeType(n.Type)
}

func buildSkeletonIdx(g *store.Graph, nNodes, nAsserts, nMerges, nUndone, nArch int) *skeletonIdx {
	idx := &skeletonIdx{
		g:          g,
		nodes:      nNodes,
		assertions: nAsserts,
		merges:     nMerges,
		undone:     nUndone,
		archived:   nArch,
		byNorm:     map[string][]string{},
		adj:        map[string][]adjEdge{},
	}
	addName := func(name, typ, id string) {
		q := graph.NormKey(name, typ)
		if utf8.RuneCountInString(q) < 2 {
			return
		}
		if !slices.Contains(idx.byNorm[q], id) {
			idx.byNorm[q] = append(idx.byNorm[q], id)
		}
	}
	for id, n := range g.Nodes {
		if n == nil || n.Status == "not_entity" || n.Status == "archived" || id == "me" {
			continue
		}
		rootID := g.Resolve(id)
		root := g.Nodes[rootID]
		if rootID == "me" || !isActiveGraphNode(root) {
			continue
		}
		addName(n.Name, root.Type, rootID)
		for _, a := range n.Aliases {
			addName(a.Name, root.Type, rootID)
		}
	}
	for _, ids := range idx.byNorm {
		sort.Strings(ids)
	}
	seenUndirected := map[string]bool{}
	for _, a := range g.Assertions {
		if a == nil || (a.Status != "active" && a.Status != "candidate") {
			continue
		}
		p := strings.ToLower(strings.TrimSpace(a.P))
		if graph.IsStoppedPredicate(p) || !entry.IsSkeletonPredicate(p) {
			continue
		}
		sid, oid := g.Resolve(a.S), g.Resolve(a.O)
		if sid == "" || oid == "" || sid == oid {
			continue
		}
		if sid != "me" && !isActiveGraphNode(g.Nodes[sid]) {
			continue
		}
		if oid != "me" && !isActiveGraphNode(g.Nodes[oid]) {
			continue
		}
		if p == "collaborates_with" {
			u, v := sid, oid
			if u > v {
				u, v = v, u
			}
			uk := u + "\x00" + v
			if seenUndirected[uk] {
				continue
			}
			seenUndirected[uk] = true
			sid, oid = u, v
		}
		sn, on := graphNodeName(g, sid), graphNodeName(g, oid)
		if sn == "" || on == "" {
			continue
		}
		se := SkeletonEdge{From: sn, Predicate: p, To: on, ValidFrom: a.ValidFrom}
		if sid != "me" {
			idx.adj[sid] = append(idx.adj[sid], adjEdge{edge: se, otherID: oid, otherName: on})
		}
		if oid != "me" {
			idx.adj[oid] = append(idx.adj[oid], adjEdge{edge: se, otherID: sid, otherName: sn})
		}
	}
	for id := range idx.adj {
		sort.Slice(idx.adj[id], func(i, j int) bool {
			ai, aj := idx.adj[id][i], idx.adj[id][j]
			if ai.edge.ValidFrom != aj.edge.ValidFrom {
				return ai.edge.ValidFrom > aj.edge.ValidFrom
			}
			if ai.edge.Predicate != aj.edge.Predicate {
				return ai.edge.Predicate < aj.edge.Predicate
			}
			return ai.otherName < aj.otherName
		})
	}
	return idx
}

func graphNodeName(g *store.Graph, id string) string {
	if id == "me" {
		return "用户"
	}
	if n := g.Nodes[id]; n != nil {
		if clean := graph.CanonicalName(n.Name, n.Type); clean != "" {
			return clean
		}
	}
	return ""
}

// SkeletonNeighbors returns up to limit distinct 1-hop structural skeleton neighbor display names
// for the given entity names using st.Graph. Used by memsrc.Subjects to index entity-anchored rules.
func SkeletonNeighbors(st *store.State, entityNames []string, limit int) []string {
	if limit <= 0 || len(entityNames) == 0 {
		return nil
	}
	idx := getSkeletonIdx(st)
	if idx == nil {
		return nil
	}
	selfNorm := map[string]bool{}
	for _, name := range entityNames {
		if q := graph.NormKey(name, ""); q != "" {
			selfNorm[q] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, name := range entityNames {
		q := graph.NormKey(name, "")
		for _, id := range idx.byNorm[q] {
			for _, ae := range idx.adj[id] {
				if ae.otherID == "me" || ae.otherName == "" || ae.otherName == "用户" {
					continue
				}
				oq := graph.NormKey(ae.otherName, "")
				if oq == "" || selfNorm[oq] || seen[oq] {
					continue
				}
				seen[oq] = true
				out = append(out, ae.otherName)
				if len(out) >= limit {
					return out
				}
			}
		}
	}
	return out
}

// matchedEntity is one entity matched by a recall query.
type matchedEntity struct {
	name   string
	typ    string
	nodeID string
	norms  map[string]bool // graph.NormKey of canonical name & aliases (including merged-in aliases)
	exact  bool
}

// genericEntityStop lists generic common nouns and short ASCII tokens that must never trigger 1-hop entity expansion
// (unified design §2.2: prevents false matches like "ST" inside "astergate", "Launch" inside "launcher", or "文档").
var genericEntityStop = map[string]bool{
	"文档": true, "系统": true, "模型": true, "任务": true, "测试": true, "配置": true, "工具": true,
	"方案": true, "项目": true, "用户": true, "助手": true, "实验": true, "报告": true, "笔记": true,
	"推送": true, "代码": true, "文件": true, "目录": true, "接口": true, "服务": true, "机器": true,
	"launch": true, "push": true, "status": true, "config": true, "agent": true, "task": true,
	"test": true, "doc": true, "docs": true, "file": true, "code": true, "repo": true,
	"prompt": true, "session": true, "review": true, "model": true, "user": true, "system": true,
}

func isPureASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func matchEntities(st *store.State, qq string, words []string) []matchedEntity {
	var cands []matchedEntity
	seen := map[string]bool{}
	matches := func(nq string) (ok, exact bool) {
		if utf8.RuneCountInString(nq) < 2 || genericEntityStop[nq] {
			return false, false
		}
		if isPureASCII(nq) {
			if len(nq) < 3 {
				return false, false
			}
			// Pure ASCII entity names require exact or whole-token match so "st" never matches "astergate"
			// and "launch" never matches "launcher".
			if nq == qq || slices.Contains(words, nq) {
				return true, true
			}
			return false, false
		}
		if nq == qq || slices.Contains(words, nq) {
			return true, true
		}
		if strings.Contains(qq, nq) {
			return true, false
		}
		return false, false
	}

	if st.Graph != nil {
		// Collect all surface names (including names/aliases from nodes merged into each canonical root).
		allNames := map[string][]string{}
		var ids []string
		for id, n := range st.Graph.Nodes {
			if n == nil || n.Status == "not_entity" || n.Status == "archived" || id == "me" {
				continue
			}
			rootID := st.Graph.Resolve(id)
			root := st.Graph.Nodes[rootID]
			if rootID == "me" || !isActiveGraphNode(root) {
				continue
			}
			if id == rootID {
				ids = append(ids, id)
			}
			allNames[rootID] = append(allNames[rootID], n.Name)
			for _, a := range n.Aliases {
				allNames[rootID] = append(allNames[rootID], a.Name)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			n := st.Graph.Nodes[id]
			canon := graph.CanonicalName(n.Name, n.Type)
			if canon == "" {
				continue
			}
			ok, exact := matches(norm.Q(canon))
			if !ok {
				for _, raw := range allNames[id] {
					if aok, aex := matches(graph.NormKey(raw, n.Type)); aok {
						ok, exact = true, exact || aex
						break
					}
				}
			}
			if !ok {
				continue
			}
			key := norm.Q(canon)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			ns := map[string]bool{key: true}
			for _, raw := range allNames[id] {
				if aq := graph.NormKey(raw, n.Type); utf8.RuneCountInString(aq) >= 2 {
					ns[aq] = true
				}
			}
			cands = append(cands, matchedEntity{name: canon, typ: n.Type, nodeID: id, norms: ns, exact: exact})
		}
	}

	var contactKeys []string
	for k := range st.Contacts {
		contactKeys = append(contactKeys, k)
	}
	sort.Strings(contactKeys)
	for _, k := range contactKeys {
		ct := st.Contacts[k]
		if ct == nil || ct.Manual == "removed" {
			continue
		}
		cn := graph.CanonicalName(ct.Name, "person")
		if cn == "" || entry.IsGenericPersonName(cn) {
			continue
		}
		nq := norm.Q(cn)
		if seen[nq] {
			continue
		}
		if ok, exact := matches(nq); ok {
			seen[nq] = true
			cands = append(cands, matchedEntity{name: cn, typ: "person", norms: map[string]bool{nq: true}, exact: exact})
		}
	}

	// Fallback: match entity names declared on usable entries (e.g. when Graph has no node yet).
	if len(cands) == 0 {
		var eids []string
		for id := range st.Entries {
			eids = append(eids, id)
		}
		sort.Strings(eids)
		for _, id := range eids {
			e := st.Entries[id]
			for _, raw := range names(st, e) {
				nq := graph.NormKey(raw, "")
				if seen[nq] {
					continue
				}
				if ok, exact := matches(nq); ok {
					seen[nq] = true
					cands = append(cands, matchedEntity{name: graph.CanonicalName(raw, ""), norms: map[string]bool{nq: true}, exact: exact})
				}
			}
		}
	}

	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].exact != cands[j].exact {
			return cands[i].exact
		}
		li, lj := utf8.RuneCountInString(cands[i].name), utf8.RuneCountInString(cands[j].name)
		if li != lj {
			return li > lj
		}
		if (cands[i].nodeID != "") != (cands[j].nodeID != "") {
			return cands[i].nodeID != ""
		}
		return cands[i].name < cands[j].name
	})
	if len(cands) > maxMatchedEnts {
		cands = cands[:maxMatchedEnts]
	}
	return cands
}

// expandEntities builds 1-hop EntityContexts and returns 1-hop neighbor candidate entries for Search.
func expandEntities(st *store.State, q Query, qq string, words []string, today string) ([]EntityContext, map[string]hopCand) {
	matched := matchEntities(st, qq, words)
	if len(matched) == 0 {
		return nil, nil
	}
	idx := getSkeletonIdx(st)
	var contexts []EntityContext
	hopCands := map[string]hopCand{}

	for _, ent := range matched {
		ec := EntityContext{Name: ent.name, Type: ent.typ}
		edgeSeen := map[string]bool{}
		neighbors := map[string]string{} // norm.Q(neighbor) -> display name

		addEdge := func(se SkeletonEdge, otherName string) {
			u, v := norm.Q(se.From), norm.Q(se.To)
			key := u + "|" + se.Predicate + "|" + v
			if se.Predicate == "collaborates_with" && u > v {
				key = v + "|" + se.Predicate + "|" + u
			}
			if edgeSeen[key] || len(ec.Skeleton) >= maxSkeletonOut {
				return
			}
			edgeSeen[key] = true
			ec.Skeleton = append(ec.Skeleton, se)
			if otherName != "" && otherName != "用户" {
				if oq := norm.Q(otherName); utf8.RuneCountInString(oq) >= 2 && !ent.norms[oq] {
					neighbors[oq] = otherName
				}
			}
		}

		// 1. Structural skeleton edges from st.Graph.
		// Prune incoming depends_on fan-in edges when a shared hub (e.g. astergate) has >2 dependents so
		// unrelated upstream projects do not flood the entity context.
		if idx != nil && ent.nodeID != "" {
			inDeps := 0
			for _, ae := range idx.adj[ent.nodeID] {
				if ae.edge.Predicate == "depends_on" && ent.norms[norm.Q(ae.edge.To)] && !ent.norms[norm.Q(ae.edge.From)] {
					inDeps++
				}
			}
			for _, ae := range idx.adj[ent.nodeID] {
				if inDeps > 2 && ae.edge.Predicate == "depends_on" && ent.norms[norm.Q(ae.edge.To)] && !ent.norms[norm.Q(ae.edge.From)] {
					continue
				}
				addEdge(ae.edge, ae.otherName)
			}
		}

		// 2. Structural skeleton edges & local rules / ops_runbooks anchored on ent from st.Entries.
		var ruleEntries []*entry.Entry
		for _, e := range st.Entries {
			if !usable(st, e, q.Categories, today) {
				continue
			}
			// World skeleton edges mentioning ent.
			if e.Facet == entry.World && !graph.IsStoppedPredicate(e.Predicate) && entry.IsSkeletonPredicate(e.Predicate) && entryMatchesEntity(st, e, ent.norms) {
				sn := cleanEntityRef(st, e.Subject)
				tn := cleanEntityRef(st, e.Target)
				if sn != "" && tn != "" {
					other := tn
					if ent.norms[norm.Q(tn)] {
						other = sn
					}
					addEdge(SkeletonEdge{
						From:      sn,
						Predicate: strings.ToLower(strings.TrimSpace(e.Predicate)),
						To:        tn,
						ValidFrom: firstNonEmptyStr(e.ValidFrom, e.Last),
					}, other)
				}
			}
			if len(q.Facets) > 0 && !slices.Contains(q.Facets, e.Facet) {
				continue
			}
			if isAnchoredRuleOrDecision(e) && entryMatchesEntity(st, e, ent.norms) {
				ruleEntries = append(ruleEntries, e)
			}
		}

		sort.SliceStable(ruleEntries, func(i, j int) bool {
			wi, wj := entryRankWeight(ruleEntries[i], today), entryRankWeight(ruleEntries[j], today)
			if wi != wj {
				return wi > wj
			}
			if ruleEntries[i].Last != ruleEntries[j].Last {
				return ruleEntries[i].Last > ruleEntries[j].Last
			}
			return ruleEntries[i].ID < ruleEntries[j].ID
		})

		quoteSeen := map[string]bool{}
		for _, e := range ruleEntries {
			qs := UserQuotes(e, quotesPerEntity)
			for _, qsItem := range qs {
				if len(ec.Quotes) >= quotesPerEntity {
					break
				}
				qk := norm.Q(qsItem)
				if !quoteSeen[qk] {
					quoteSeen[qk] = true
					ec.Quotes = append(ec.Quotes, qsItem)
				}
			}
			if len(ec.Rules) < maxRulesPerEnt {
				ec.Rules = append(ec.Rules, Hit{
					ID:     e.ID,
					Facet:  e.Facet,
					Status: e.Status,
					Last:   e.Last,
					Kinds:  e.Kinds,
					Text:   strings.Join(strings.Fields(Line(e)), " "),
					Cats:   e.Cats(),
					Quotes: qs,
				})
			}
		}

		// 3. 1-hop neighbor rules/decisions: entries anchored on a 1-hop structural neighbor of ent.
		if len(neighbors) > 0 {
			for _, e := range st.Entries {
				if !usable(st, e, q.Categories, today) {
					continue
				}
				if len(q.Facets) > 0 && !slices.Contains(q.Facets, e.Facet) {
					continue
				}
				if !isAnchoredRuleOrDecision(e) && !(e.Facet == entry.World && !graph.IsStoppedPredicate(e.Predicate) && entry.IsSkeletonPredicate(e.Predicate)) {
					continue
				}
				if nbName := entryMatchesNeighbor(st, e, neighbors); nbName != "" {
					w := statusWeight[e.Status]
					if e.Pinned {
						w = 1
					}
					if today != "" {
						w *= entry.OpsRunbookWeight(e, today)
					}
					sc := 0.55 * w
					if old, ok := hopCands[e.ID]; !ok || sc > old.score {
						hopCands[e.ID] = hopCand{e: e, score: sc, via: ent.name + "→" + nbName}
					}
				}
			}
		}

		if len(ec.Skeleton) > 0 || len(ec.Rules) > 0 {
			contexts = append(contexts, ec)
		}
	}
	return contexts, hopCands
}

type hopCand struct {
	e     *entry.Entry
	score float64
	via   string
}

func isAnchoredRuleOrDecision(e *entry.Entry) bool {
	if entry.IsOpsRunbookEntry(e) {
		return true
	}
	switch e.Facet {
	case entry.Interaction, entry.Tools, entry.Judgement, entry.Workflow:
		return true
	case entry.World:
		t := strings.ToLower(strings.TrimSpace(e.Type))
		p := strings.ToLower(strings.TrimSpace(e.Predicate))
		return t == "decision" || t == "decided" || t == "committed" || p == "decided" || p == "decides" || p == "committed" || e.Commitment != nil
	}
	return false
}

func entryRankWeight(e *entry.Entry, today string) int {
	w := entry.Score(e)
	if e.Pinned {
		w += 100
	}
	if e.Status == entry.Stable {
		w += 20
	}
	if entry.IsOpsRunbookEntry(e) && today != "" {
		mult := entry.OpsRunbookWeight(e, today)
		if mult >= 1.0 {
			w += 15
		} else if mult <= 0.25 {
			w -= 40
		} else {
			w -= 20
		}
	}
	return w
}

func entryMatchesEntity(st *store.State, e *entry.Entry, norms map[string]bool) bool {
	for _, n := range entryEntityNames(st, e) {
		if norms[graph.NormKey(n, "")] {
			return true
		}
	}
	return false
}

func entryMatchesNeighbor(st *store.State, e *entry.Entry, neighbors map[string]string) string {
	for _, n := range entryEntityNames(st, e) {
		if nb, ok := neighbors[graph.NormKey(n, "")]; ok {
			return nb
		}
	}
	return ""
}

func entryEntityNames(st *store.State, e *entry.Entry) []string {
	ns := names(st, e)
	if o := graph.CanonicalName(e.Impact.Object, ""); o != "" {
		ns = append(ns, o)
	}
	return ns
}

func cleanEntityRef(st *store.State, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(raw), "person:") && st != nil {
		if k := st.Contacts[raw]; k != nil && k.Manual != "removed" && k.Name != "" {
			raw = k.Name
		}
	}
	clean := graph.CanonicalName(raw, "")
	if idx := getSkeletonIdx(st); idx != nil && clean != "" {
		if ids := idx.byNorm[norm.Q(clean)]; len(ids) == 1 {
			if cn := graphNodeName(idx.g, ids[0]); cn != "" {
				return cn
			}
		}
	}
	return clean
}

func firstNonEmptyStr(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// UserQuotes extracts up to limit deduplicated L0 user-authored original quote slices (<=80 runes, with date).
func UserQuotes(e *entry.Entry, limit int) []string {
	if e == nil || limit <= 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for i := len(e.Evidence) - 1; i >= 0 && len(out) < limit; i-- {
		ev := e.Evidence[i]
		if ev.Who != entry.WhoSelf || ev.Reported {
			continue
		}
		q := strings.Join(strings.Fields(ev.Quote), " ")
		if q == "" {
			continue
		}
		k := norm.Q(q)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		if r := []rune(q); len(r) > quoteSliceMost {
			q = string(r[:quoteSliceMost]) + "…"
		}
		if ev.Day != "" {
			out = append(out, "原话：「"+q+"」（"+ev.Day+"）")
		} else {
			out = append(out, "原话：「"+q+"」")
		}
	}
	return out
}

// FormatEntity renders one EntityContext (1-hop skeleton edges, anchored rules, and L0 quote slices).
func FormatEntity(ec EntityContext) string {
	var b strings.Builder
	title := ec.Name
	if ec.Type != "" {
		title += "（" + ec.Type + "）"
	}
	fmt.Fprintf(&b, "## 实体骨架 1 跳展开：%s\n", title)
	if len(ec.Skeleton) > 0 {
		b.WriteString("骨架关系：\n")
		for _, se := range ec.Skeleton {
			if se.ValidFrom != "" {
				fmt.Fprintf(&b, "- %s -[%s]-> %s（%s）\n", se.From, se.Predicate, se.To, se.ValidFrom)
			} else {
				fmt.Fprintf(&b, "- %s -[%s]-> %s\n", se.From, se.Predicate, se.To)
			}
		}
	}
	if len(ec.Rules) > 0 {
		b.WriteString("挂靠规矩与决定：\n")
		for _, r := range ec.Rules {
			b.WriteString(Format(r))
		}
	}
	if len(ec.Quotes) > 0 {
		b.WriteString("相关原话：\n")
		for _, q := range ec.Quotes {
			fmt.Fprintf(&b, "- %s\n", q)
		}
	}
	return b.String()
}
