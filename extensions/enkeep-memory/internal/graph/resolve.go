package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

const (
	ProposalTier1Prefix  = "auto-tier1-prefix-v04"
	ProposalTier1Contact = "auto-tier1-contact-v04"
	ProposalTier1CNShort = "auto-tier1-cn-short-v04"
	ProposalTier2Haiku   = "auto-haiku-v04"

	tier2BatchSize = 30
	tier2MaxPairs  = 240 // at most 8 batches of 30 pairs
)

// MergePair records one merge executed by ResolveAndMerge.
type MergePair struct {
	From     string `json:"from"`
	FromName string `json:"from_name"`
	FromType string `json:"from_type"`
	Into     string `json:"into"`
	IntoName string `json:"into_name"`
	IntoType string `json:"into_type"`
	Tier     string `json:"tier"`
	Reason   string `json:"reason"`
}

// MergeReport summarizes everything ResolveAndMerge cleaned, archived, deduplicated, and merged.
type MergeReport struct {
	PrefixedCleaned    int         `json:"prefixed_cleaned"`
	ArchivedTasks      int         `json:"archived_tasks"`
	ArchivedAbout      int         `json:"archived_about"`
	ArchivedSameCircle int         `json:"archived_same_circle"`
	DedupedCollabEdges int         `json:"deduped_collab_edges"`
	Tier1PrefixMerged  int         `json:"tier1_prefix_merged"`
	Tier1ContactMerged int         `json:"tier1_contact_merged"`
	Tier1CNShortMerged int         `json:"tier1_cn_short_merged"`
	Tier2PairsChecked  int         `json:"tier2_pairs_checked"`
	Tier2Batches       int         `json:"tier2_batches"`
	Tier2Merged        int         `json:"tier2_merged"`
	TotalMerged        int         `json:"total_merged"`
	Merges             []MergePair `json:"merges,omitempty"`
}

// ResolveAndMerge runs the v0.4.0 entity governance and three-tier disambiguation pipeline on st.Graph.
// When m is nil, only Tier 1 deterministic governance and merges run (0 model calls). Every merge goes through
// Builder.Merge so it writes a store.MergeRec and can be fully restored via Builder.Unmerge.
func ResolveAndMerge(ctx context.Context, st *store.State, m model.Model) (*MergeReport, error) {
	today := time.Now().Format("2006-01-02")
	if st != nil && len(st.ActiveDays) > 0 {
		today = st.ActiveDays[len(st.ActiveDays)-1]
	}
	return ResolveAndMergeAt(ctx, st, m, today)
}

// ResolveAndMergeAt runs ResolveAndMerge using an explicit YYYY-MM-DD date for MergeRec timestamps.
func ResolveAndMergeAt(ctx context.Context, st *store.State, m model.Model, today string) (*MergeReport, error) {
	rep := &MergeReport{}
	if st == nil || st.Graph == nil {
		return rep, nil
	}
	if today == "" {
		today = time.Now().Format("2006-01-02")
	}
	g := st.Graph
	if g.Nodes == nil {
		g.Nodes = map[string]*store.Node{}
	}
	if g.Assertions == nil {
		g.Assertions = map[string]*store.Assertion{}
	}
	b := &Builder{G: g, Today: today}

	// Capture original display names and prefixed flags before stripping type: prefixes.
	origName := make(map[string]string, len(g.Nodes))
	hadPrefix := make(map[string]bool, len(g.Nodes))
	prefixType := make(map[string]string, len(g.Nodes))

	// Step 0: Archive legacy task nodes, archive about/same_circle edges, and strip leading type: prefixes.
	for _, id := range sortedNodeIDs(g) {
		n := g.Nodes[id]
		if n == nil || id == Me {
			continue
		}
		origName[id] = n.Name
		if n.MergedInto != "" || n.Status == "not_entity" {
			continue
		}
		if n.Type == "task" {
			if n.Status != "archived" {
				n.Status = "archived"
				rep.ArchivedTasks++
			}
			continue
		}
		if n.Status == "archived" {
			continue
		}
		if pt := extractedPrefixType(n.Name); pt != "" {
			hadPrefix[id] = true
			prefixType[id] = pt
		}
		if clean := CanonicalName(n.Name, n.Type); clean != "" && clean != n.Name {
			raw := n.Name
			n.Name = clean
			if !slices.ContainsFunc(n.Aliases, func(a store.Alias) bool { return a.Name == raw }) {
				n.Aliases = append(n.Aliases, store.Alias{Name: raw, From: "v04_prefix_strip"})
			}
			rep.PrefixedCleaned++
		}
	}

	for _, id := range sortedAssertionIDs(g) {
		a := g.Assertions[id]
		if a == nil || a.Status == "archived" {
			continue
		}
		p := TypeName(a.P)
		if p == "about" {
			a.Status = "archived"
			rep.ArchivedAbout++
		} else if p == "same_circle" {
			a.Status = "archived"
			rep.ArchivedSameCircle++
		}
	}

	// Helper to perform a tracked merge via Builder.Merge.
	doMerge := func(from, into, proposal, tier, reason string) bool {
		from, into = g.Resolve(from), g.Resolve(into)
		if from == "" || into == "" || from == into || from == Me {
			return false
		}
		fn, tn := g.Nodes[from], g.Nodes[into]
		if fn == nil || tn == nil || fn.MergedInto != "" || tn.MergedInto != "" {
			return false
		}
		fromDisp := origName[from]
		if fromDisp == "" {
			fromDisp = fn.Name
		}
		intoDisp := origName[into]
		if intoDisp == "" {
			intoDisp = tn.Name
		}
		if err := b.Merge(from, into, proposal); err != nil {
			return false
		}
		rep.TotalMerged++
		rep.Merges = append(rep.Merges, MergePair{
			From:     from,
			FromName: fromDisp,
			FromType: fn.Type,
			Into:     into,
			IntoName: intoDisp,
			IntoType: tn.Type,
			Tier:     tier,
			Reason:   reason,
		})
		return true
	}

	// Tier 1 Rule 1: Canonical key homograph merge (same type, chat->person, and project/system/repo with shared edge).
	rep.Tier1PrefixMerged += mergeTier1Prefix(g, hadPrefix, prefixType, doMerge)

	// Tier 1 Rule 2: Contact-table anchored merge (st.Contacts + st.PersonMerges + shared KB refs).
	rep.Tier1ContactMerged += mergeTier1Contacts(st, hadPrefix, doMerge)

	// Tier 1 Rule 3: Unambiguous 2-char Chinese person short-name merge (unique 3-char Chinese suffix match).
	rep.Tier1CNShortMerged += mergeTier1CNShort(st, doMerge)

	// Deduplicate bidirectional/mirror collaborates_with edges and normalize to min(s,o) -> max(s,o).
	rep.DedupedCollabEdges += dedupCollabEdges(g)

	// Tier 2: Lightweight Haiku disambiguation for residual fuzzy/shared-neighbor pairs.
	if m != nil {
		if err := runTier2Haiku(ctx, g, m, hadPrefix, rep, doMerge); err != nil {
			return rep, err
		}
		// Re-deduplicate collaborates_with in case Tier 2 merged two endpoints into a mirror edge.
		rep.DedupedCollabEdges += dedupCollabEdges(g)
	}

	return rep, nil
}

// PersonMergesFromGraph derives plugin-side entry.PersonMerge groups from merged person nodes in st.Graph so
// people cards and contacts consume merged canonical entities without introducing a store import in internal/people.
func PersonMergesFromGraph(st *store.State) []entry.PersonMerge {
	if st == nil || st.Graph == nil {
		return nil
	}
	g := st.Graph
	groups := map[string][]string{}
	names := map[string]string{}
	for _, id := range sortedNodeIDs(g) {
		n := g.Nodes[id]
		if n == nil || n.Type != "person" || n.Status == "archived" || n.Status == "not_entity" {
			continue
		}
		root := g.Resolve(id)
		rn := g.Nodes[root]
		if rn == nil || rn.Type != "person" || root == Me {
			continue
		}
		if names[root] == "" {
			names[root] = CanonicalName(rn.Name, "person")
		}
		for _, ref := range n.Refs {
			if kbID, ok := strings.CutPrefix(ref, "people/"); ok && kbID != "" {
				pk := "person:" + kbID
				if !slices.Contains(groups[root], pk) {
					groups[root] = append(groups[root], pk)
				}
			}
		}
	}
	// Also map un-reffed merged person nodes back to contacts when a contact matches by name.
	var roots []string
	for root, ps := range groups {
		if len(ps) >= 2 {
			roots = append(roots, root)
		}
	}
	sort.Strings(roots)
	out := make([]entry.PersonMerge, 0, len(roots))
	for _, root := range roots {
		ps := groups[root]
		sort.Strings(ps)
		out = append(out, entry.PersonMerge{
			ID:      "graph:" + root,
			Persons: ps,
			Name:    names[root],
		})
	}
	return out
}

func extractedPrefixType(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	for _, p := range typePrefixes {
		if strings.HasPrefix(lower, p) {
			t := strings.TrimRight(p, ":：")
			switch t {
			case "people", "人物":
				return "person"
			case "projects", "项目":
				return "project"
			case "docs":
				return "doc"
			case "系统":
				return "system"
			case "任务":
				return "task"
			case "仓库":
				return "repo"
			default:
				return t
			}
		}
	}
	return ""
}

func typePriority(typ string) int {
	switch typ {
	case "person":
		return 5
	case "project":
		return 4
	case "repo":
		return 3
	case "system":
		return 2
	case "org":
		return 1
	default:
		return 0
	}
}

func nodeDegrees(g *store.Graph) map[string]int {
	deg := map[string]int{}
	for _, a := range g.Assertions {
		if a == nil || a.Status == "archived" || a.Status == "retracted" || a.Status == "rejected" {
			continue
		}
		s, o := g.Resolve(a.S), g.Resolve(a.O)
		if s != "" {
			deg[s]++
		}
		if o != "" && o != s {
			deg[o]++
		}
	}
	return deg
}

func nodeNeighbors(g *store.Graph) map[string]map[string]bool {
	nb := map[string]map[string]bool{}
	add := func(u, v string) {
		if u == "" || v == "" || u == v {
			return
		}
		if nb[u] == nil {
			nb[u] = map[string]bool{}
		}
		nb[u][v] = true
	}
	for _, a := range g.Assertions {
		if a == nil || a.Status == "archived" || a.Status == "retracted" || a.Status == "rejected" {
			continue
		}
		if IsStoppedPredicate(a.P) {
			continue
		}
		s, o := g.Resolve(a.S), g.Resolve(a.O)
		add(s, o)
		add(o, s)
	}
	return nb
}

func sharesNeighborOrEdge(nb map[string]map[string]bool, a, b string) bool {
	na, nbSet := nb[a], nb[b]
	if len(na) == 0 || len(nbSet) == 0 {
		return false
	}
	if na[b] || nbSet[a] {
		return true
	}
	for x := range na {
		if nbSet[x] {
			return true
		}
	}
	return false
}

func countSharedNeighbors(nb map[string]map[string]bool, a, b string) int {
	na, nbSet := nb[a], nb[b]
	if len(na) == 0 || len(nbSet) == 0 {
		return 0
	}
	n := 0
	for x := range na {
		if x == Me || x == a || x == b {
			continue
		}
		if nbSet[x] {
			n++
		}
	}
	return n
}

func pickWinner(g *store.Graph, ids []string, deg map[string]int, hadPrefix map[string]bool) string {
	sorted := slices.Clone(ids)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := g.Nodes[sorted[i]], g.Nodes[sorted[j]]
		if sorted[i] == Me {
			return true
		}
		if sorted[j] == Me {
			return false
		}
		if pa, pb := typePriority(a.Type), typePriority(b.Type); pa != pb {
			return pa > pb
		}
		if hadPrefix[a.ID] != hadPrefix[b.ID] {
			return !hadPrefix[a.ID]
		}
		if len(a.Refs) != len(b.Refs) {
			return len(a.Refs) > len(b.Refs)
		}
		if deg[a.ID] != deg[b.ID] {
			return deg[a.ID] > deg[b.ID]
		}
		if (a.Status == "active") != (b.Status == "active") {
			return a.Status == "active"
		}
		if len(a.Aliases) != len(b.Aliases) {
			return len(a.Aliases) > len(b.Aliases)
		}
		return a.ID < b.ID
	})
	return sorted[0]
}

func mergeTier1Prefix(g *store.Graph, hadPrefix map[string]bool, prefixType map[string]string, doMerge func(from, into, proposal, tier, reason string) bool) int {
	merged := 0
	deg := nodeDegrees(g)

	// 1a. Same-type identical NormKey merge.
	byTypeKey := map[string][]string{}
	for _, id := range sortedNodeIDs(g) {
		n := g.Nodes[id]
		if !isLiveNode(n) || id == Me {
			continue
		}
		k := NormKey(n.Name, n.Type)
		if k == "" || isGeneric(n.Name) {
			continue
		}
		byTypeKey[n.Type+"\x00"+k] = append(byTypeKey[n.Type+"\x00"+k], id)
	}
	keys := make([]string, 0, len(byTypeKey))
	for k, ids := range byTypeKey {
		if len(ids) >= 2 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		ids := byTypeKey[k]
		winner := pickWinner(g, ids, deg, hadPrefix)
		for _, id := range ids {
			if id == winner {
				continue
			}
			reason := "same-type canonical key"
			if hadPrefix[id] {
				reason = "prefix stripped canonical match"
			}
			if doMerge(id, winner, ProposalTier1Prefix, "tier1_prefix", reason) {
				merged++
			}
		}
	}

	// 1b. Cross-type merges allowed by §1.5 Rule 1:
	// - chat with same-name person
	// - node whose stripped prefix explicitly matched another node's type (e.g. "person:X" typed as other -> person X)
	// - project/system/repo with same NormKey sharing >= 1 edge/neighbor
	nb := nodeNeighbors(g)
	deg = nodeDegrees(g)
	byKey := map[string][]string{}
	for _, id := range sortedNodeIDs(g) {
		n := g.Nodes[id]
		if !isLiveNode(n) || id == Me {
			continue
		}
		k := NormKey(n.Name, n.Type)
		if k == "" || isGeneric(n.Name) {
			continue
		}
		byKey[k] = append(byKey[k], id)
	}
	nkeys := make([]string, 0, len(byKey))
	for k, ids := range byKey {
		if len(ids) >= 2 {
			nkeys = append(nkeys, k)
		}
	}
	sort.Strings(nkeys)
	for _, k := range nkeys {
		ids := byKey[k]
		// chat -> person
		var personIDs, chatIDs, techIDs []string
		byT := map[string]string{}
		for _, id := range ids {
			n := g.Nodes[g.Resolve(id)]
			if !isLiveNode(n) {
				continue
			}
			if _, ok := byT[n.Type]; !ok {
				byT[n.Type] = n.ID
			}
			switch n.Type {
			case "person":
				personIDs = append(personIDs, n.ID)
			case "chat":
				chatIDs = append(chatIDs, n.ID)
			case "project", "system", "repo":
				techIDs = append(techIDs, n.ID)
			}
		}
		if len(personIDs) == 1 && len(chatIDs) > 0 {
			for _, cid := range chatIDs {
				if doMerge(cid, personIDs[0], ProposalTier1Prefix, "tier1_prefix", "chat merged into same-name person") {
					merged++
				}
			}
		}
		// Explicit prefix type match (e.g. node had "project:X" stripped and a project node "X" exists).
		for _, id := range ids {
			pt := prefixType[id]
			if pt == "" {
				continue
			}
			if target, ok := byT[pt]; ok && g.Resolve(id) != g.Resolve(target) {
				if doMerge(id, target, ProposalTier1Prefix, "tier1_prefix", "explicit type prefix match") {
					merged++
				}
			}
		}
		// project/system/repo with same NormKey sharing >= 1 neighbor/edge.
		if len(techIDs) >= 2 {
			winner := pickWinner(g, techIDs, deg, hadPrefix)
			for _, tid := range techIDs {
				if g.Resolve(tid) == g.Resolve(winner) {
					continue
				}
				if sharesNeighborOrEdge(nb, winner, tid) {
					if doMerge(tid, winner, ProposalTier1Prefix, "tier1_prefix", "project/system/repo same key with shared edge") {
						merged++
					}
				}
			}
		}
	}
	return merged
}

func mergeTier1Contacts(st *store.State, hadPrefix map[string]bool, doMerge func(from, into, proposal, tier, reason string) bool) int {
	g := st.Graph
	merged := 0
	deg := nodeDegrees(g)

	// Map KB ref ("people/<id>") -> live person node IDs.
	byRef := map[string][]string{}
	for _, id := range sortedNodeIDs(g) {
		n := g.Nodes[id]
		if !isLiveNode(n) || n.Type != "person" || id == Me {
			continue
		}
		for _, r := range n.Refs {
			if strings.HasPrefix(r, "people/") {
				byRef[r] = append(byRef[r], id)
			}
		}
	}

	// 2a. Multiple person nodes sharing the exact same KB ref.
	var refs []string
	for r, ids := range byRef {
		if len(ids) >= 2 {
			refs = append(refs, r)
		}
	}
	sort.Strings(refs)
	for _, r := range refs {
		ids := byRef[r]
		winner := pickWinner(g, ids, deg, hadPrefix)
		for _, id := range ids {
			if id == winner {
				continue
			}
			if doMerge(id, winner, ProposalTier1Contact, "tier1_contact", "shared kb person ref "+r) {
				merged++
			}
		}
	}

	// 2b. User-declared PersonMerges.
	for _, pm := range st.PersonMerges {
		var groupNodes []string
		for _, pk := range pm.Persons {
			kbID := strings.TrimPrefix(pk, "person:")
			for _, id := range byRef["people/"+kbID] {
				r := g.Resolve(id)
				if !slices.Contains(groupNodes, r) {
					groupNodes = append(groupNodes, r)
				}
			}
		}
		if len(groupNodes) >= 2 {
			winner := pickWinner(g, groupNodes, deg, hadPrefix)
			for _, id := range groupNodes {
				if id == winner {
					continue
				}
				if doMerge(id, winner, ProposalTier1Contact, "tier1_contact", "person_merge "+pm.ID) {
					merged++
				}
			}
		}
	}

	// 2c. Contact table anchored merge:
	// - Un-reffed person node matching a unique contact's name or alias -> merge into the contact's reffed node.
	// - Two reffed person nodes from different connectors sharing the exact same non-generic contact name
	//   (>= 3 CJK runes or multi-word Latin name, or sharing >= 1 graph neighbor) -> merge.
	if len(st.Contacts) == 0 {
		return merged
	}
	nb := nodeNeighbors(g)
	deg = nodeDegrees(g)

	// Group active contacts by canonical NormKey(contact.Name).
	type contactAnchor struct {
		key     string
		source  string
		name    string
		nodeIDs []string
	}
	byContactName := map[string][]contactAnchor{}
	var ckeys []string
	for k := range st.Contacts {
		ckeys = append(ckeys, k)
	}
	sort.Strings(ckeys)
	for _, ck := range ckeys {
		c := st.Contacts[ck]
		if c == nil || c.Manual == "removed" {
			continue
		}
		cn := CanonicalName(c.Name, "person")
		qk := norm.Q(cn)
		if qk == "" || isGeneric(cn) || entry.IsGenericPersonName(cn) {
			continue
		}
		kbID := strings.TrimPrefix(ck, "person:")
		var nids []string
		for _, nid := range byRef["people/"+kbID] {
			r := g.Resolve(nid)
			if isLiveNode(g.Nodes[r]) && !slices.Contains(nids, r) {
				nids = append(nids, r)
			}
		}
		if len(nids) == 0 {
			continue
		}
		byContactName[qk] = append(byContactName[qk], contactAnchor{
			key:     ck,
			source:  c.Source,
			name:    cn,
			nodeIDs: nids,
		})
		// Bilingual contact display name (e.g. "陈明 ChenMing"): also index the pure-Han full name component
		// when it has >= 2 Han runes and is not generic, so an un-reffed node bearing only the Chinese name
		// anchors cleanly onto the contact's reffed node (only if unique across all contacts).
		if fields := strings.Fields(cn); len(fields) >= 2 {
			for _, f := range fields {
				if isPureHan(f) && utf8.RuneCountInString(f) >= 2 && !genericCN2Stop[f] && !entry.IsGenericPersonName(f) {
					fq := norm.Q(f)
					if fq != "" && fq != qk {
						byContactName[fq] = append(byContactName[fq], contactAnchor{
							key:     ck,
							source:  c.Source,
							name:    f,
							nodeIDs: nids,
						})
					}
				}
			}
		}
	}

	// Index un-reffed live person nodes by their name & alias NormKeys.
	unreffedByKey := map[string][]string{}
	for _, id := range sortedNodeIDs(g) {
		n := g.Nodes[id]
		if !isLiveNode(n) || n.Type != "person" || id == Me || len(n.Refs) > 0 {
			continue
		}
		seenK := map[string]bool{}
		for _, raw := range append([]string{n.Name}, aliasNames(n)...) {
			k := NormKey(raw, "person")
			if k == "" || seenK[k] || isGeneric(raw) || entry.IsGenericPersonName(raw) {
				continue
			}
			seenK[k] = true
			unreffedByKey[k] = append(unreffedByKey[k], id)
		}
	}

	var qkeys []string
	for qk := range byContactName {
		qkeys = append(qkeys, qk)
	}
	sort.Strings(qkeys)
	for _, qk := range qkeys {
		anchors := byContactName[qk]
		// Collect all anchor node IDs.
		var anchorNodes []string
		sources := map[string]bool{}
		distinctSources := true
		for _, a := range anchors {
			if a.source != "" && sources[a.source] {
				distinctSources = false
			}
			if a.source != "" {
				sources[a.source] = true
			}
			for _, nid := range a.nodeIDs {
				r := g.Resolve(nid)
				if !slices.Contains(anchorNodes, r) {
					anchorNodes = append(anchorNodes, r)
				}
			}
		}
		// Merge cross-connector contacts that share the exact same full name.
		if len(anchorNodes) >= 2 && distinctSources {
			winner := pickWinner(g, anchorNodes, deg, hadPrefix)
			for _, id := range anchorNodes {
				if g.Resolve(id) == g.Resolve(winner) {
					continue
				}
				nameRunes := utf8.RuneCountInString(anchors[0].name)
				canMerge := (isPureHan(anchors[0].name) && nameRunes >= 3) ||
					strings.Contains(anchors[0].name, " ") ||
					sharesNeighborOrEdge(nb, winner, id)
				if canMerge {
					if doMerge(id, winner, ProposalTier1Contact, "tier1_contact", "cross-source contact name match") {
						merged++
					}
				}
			}
		}
		// If there is a unique resolved anchor node for this contact name, merge matching un-reffed person nodes into it.
		var liveAnchors []string
		for _, nid := range anchorNodes {
			r := g.Resolve(nid)
			if !slices.Contains(liveAnchors, r) {
				liveAnchors = append(liveAnchors, r)
			}
		}
		if len(liveAnchors) == 1 {
			target := liveAnchors[0]
			for _, uid := range unreffedByKey[qk] {
				if g.Resolve(uid) == target {
					continue
				}
				if doMerge(uid, target, ProposalTier1Contact, "tier1_contact", "contact-anchored alias match") {
					merged++
				}
			}
		}
	}

	return merged
}

// genericCN2Stop prevents common 2-char Chinese role/noun words from matching as person short names.
var genericCN2Stop = map[string]bool{
	"老板": true, "主管": true, "经理": true, "总监": true, "同事": true, "同学": true,
	"客户": true, "朋友": true, "家人": true, "老婆": true, "老公": true, "爸爸": true,
	"妈妈": true, "哥哥": true, "姐姐": true, "弟弟": true, "妹妹": true, "老师": true,
	"医生": true, "对方": true, "本人": true, "自己": true, "用户": true, "助手": true,
	"客服": true, "运营": true, "产品": true, "研发": true, "测试": true, "设计": true,
	"财务": true, "法务": true, "行政": true, "人事": true, "实习": true, "外包": true,
	"顾问": true, "专家": true, "领导": true, "大家": true, "团队": true, "小组": true,
	"部门": true, "公司": true, "开发": true, "管理": true, "系统": true, "中心": true,
	"服务": true, "技术": true, "项目": true, "平台": true, "工程": true, "架构": true,
	"秘书": true, "助理": true, "猎头": true, "招聘": true, "面试": true,
}

func mergeTier1CNShort(st *store.State, doMerge func(from, into, proposal, tier, reason string) bool) int {
	g := st.Graph
	merged := 0

	// Build full inventory of 3-char pure-Han person names across BOTH live person nodes and st.Contacts so any
	// conflicting surname anywhere in the store blocks an ambiguous 2-char short-name merge.
	suffixToFullNames := map[string]map[string]bool{}
	fullNameToNode := map[string][]string{}
	contactNameByRef := map[string]string{}

	recordFull := func(full string) {
		rs := []rune(full)
		if len(rs) != 3 || !isPureHan(full) {
			return
		}
		suf := string(rs[1:])
		if suffixToFullNames[suf] == nil {
			suffixToFullNames[suf] = map[string]bool{}
		}
		suffixToFullNames[suf][full] = true
	}

	for ck, c := range st.Contacts {
		if c == nil || c.Manual == "removed" {
			continue
		}
		cn := CanonicalName(c.Name, "person")
		kbID := strings.TrimPrefix(ck, "person:")
		contactNameByRef["people/"+kbID] = cn
		recordFull(cn)
	}
	for _, id := range sortedNodeIDs(g) {
		n := g.Nodes[id]
		if !isLiveNode(n) || n.Type != "person" || id == Me {
			continue
		}
		cn := CanonicalName(n.Name, "person")
		if len([]rune(cn)) == 3 && isPureHan(cn) {
			recordFull(cn)
			fullNameToNode[cn] = append(fullNameToNode[cn], id)
		}
	}

	for _, id := range sortedNodeIDs(g) {
		n := g.Nodes[id]
		if !isLiveNode(n) || n.Type != "person" || id == Me {
			continue
		}
		short := CanonicalName(n.Name, "person")
		rs := []rune(short)
		if len(rs) != 2 || !isPureHan(short) {
			continue
		}
		if genericCN2Stop[short] || rs[0] == '小' || rs[0] == '老' || rs[0] == '阿' {
			continue
		}
		fulls := suffixToFullNames[short]
		if len(fulls) != 1 {
			continue // zero or ambiguous (>1 conflicting surnames)
		}
		var fullName string
		for fn := range fulls {
			fullName = fn
		}
		// Ensure shortNode does not carry a contact ref or alias pointing to a conflicting 3-char name.
		conflict := false
		for _, r := range n.Refs {
			if cn := contactNameByRef[r]; len([]rune(cn)) == 3 && isPureHan(cn) && cn != fullName {
				conflict = true
				break
			}
		}
		if conflict {
			continue
		}
		for _, al := range n.Aliases {
			ac := CanonicalName(al.Name, "person")
			if len([]rune(ac)) == 3 && isPureHan(ac) && ac != fullName {
				conflict = true
				break
			}
		}
		if conflict {
			continue
		}
		targets := fullNameToNode[fullName]
		if len(targets) != 1 {
			continue
		}
		target := g.Resolve(targets[0])
		if target == g.Resolve(id) {
			continue
		}
		if doMerge(id, target, ProposalTier1CNShort, "tier1_cn_short", fmt.Sprintf("unique 2-char Chinese suffix of %s", fullName)) {
			merged++
		}
	}
	return merged
}

func dedupCollabEdges(g *store.Graph) int {
	type pairKey struct{ u, v string }
	groups := map[pairKey][]*store.Assertion{}
	deduped := 0

	for _, id := range sortedAssertionIDs(g) {
		a := g.Assertions[id]
		if a == nil || a.Status == "archived" || a.Status == "retracted" || a.Status == "rejected" {
			continue
		}
		if TypeName(a.P) != "collaborates_with" {
			continue
		}
		s, o := g.Resolve(a.S), g.Resolve(a.O)
		if s == "" || o == "" || s == o {
			a.Status = "archived"
			deduped++
			continue
		}
		u, v := s, o
		if u > v {
			u, v = v, u
		}
		groups[pairKey{u, v}] = append(groups[pairKey{u, v}], a)
	}

	keys := make([]pairKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].u != keys[j].u {
			return keys[i].u < keys[j].u
		}
		return keys[i].v < keys[j].v
	})

	statusRank := func(s string) int {
		switch s {
		case "active":
			return 3
		case "candidate":
			return 2
		case "stale":
			return 1
		default:
			return 0
		}
	}

	for _, k := range keys {
		list := groups[k]
		sort.Slice(list, func(i, j int) bool {
			if ri, rj := statusRank(list[i].Status), statusRank(list[j].Status); ri != rj {
				return ri > rj
			}
			if len(list[i].Evidence) != len(list[j].Evidence) {
				return len(list[i].Evidence) > len(list[j].Evidence)
			}
			if list[i].ValidFrom != list[j].ValidFrom {
				return list[i].ValidFrom < list[j].ValidFrom
			}
			return list[i].ID < list[j].ID
		})
		keep := list[0]
		keep.S, keep.O = k.u, k.v
		for _, dup := range list[1:] {
			for _, ev := range dup.Evidence {
				if !slices.ContainsFunc(keep.Evidence, func(x store.Evidence) bool {
					return x.Chat == ev.Chat && x.Day == ev.Day && x.N == ev.N
				}) {
					keep.Evidence = append(keep.Evidence, ev)
				}
			}
			if dup.ValidFrom != "" && (keep.ValidFrom == "" || dup.ValidFrom < keep.ValidFrom) {
				keep.ValidFrom = dup.ValidFrom
			}
			if dup.Observed > keep.Observed {
				keep.Observed = dup.Observed
			}
			dup.Status = "archived"
			deduped++
		}
	}
	return deduped
}

// ---- Tier 2: Lightweight Haiku Disambiguation -----------------------------------------------------------------

type tier2Pair struct {
	ID     string
	A      string
	B      string
	Reason string
	Score  int
}

var tier2Schema = json.RawMessage(`{
  "type": "object",
  "required": ["decisions"],
  "properties": {
    "decisions": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["pair_id", "verdict", "confidence"],
        "properties": {
          "pair_id": {"type": "string"},
          "verdict": {"type": "string", "enum": ["same_entity", "distinct", "uncertain"]},
          "confidence": {"type": "string", "enum": ["high", "medium", "low"]},
          "canonical": {"type": "string", "enum": ["a", "b", ""]},
          "reason": {"type": "string"}
        }
      }
    }
  }
}`)

const tier2SystemPrompt = `You disambiguate candidate entity pairs in a personal knowledge graph.
For each pair (a, b), decide whether they refer to the exact same real-world entity ("same_entity"), different entities ("distinct"), or cannot be determined ("uncertain").
Only mark verdict="same_entity" with confidence="high" when clearly aliases, abbreviations, bilingual equivalents, or repo/project variants of the same entity (e.g. "astergate" and "aster", "enkeep-memory" and "enkeep 记忆插件").
Never merge distinct modules, distinct people with different full names, or a parent org with a child project.
Never merge two people based solely on shared neighbors or shared team/project membership; specifically, never guess that an English name and a Chinese name (or any two people without direct name, pinyin, or alias correspondence) are the same person.
Set "canonical" to "a" or "b" indicating the cleaner, more complete name. Return JSON matching the schema.`

type tier2Response struct {
	Decisions []struct {
		PairID     string `json:"pair_id"`
		Verdict    string `json:"verdict"`
		Confidence string `json:"confidence"`
		Canonical  string `json:"canonical"`
		Reason     string `json:"reason"`
	} `json:"decisions"`
}

func runTier2Haiku(ctx context.Context, g *store.Graph, m model.Model, hadPrefix map[string]bool, rep *MergeReport, doMerge func(from, into, proposal, tier, reason string) bool) error {
	pairs := tier2Candidates(g)
	if len(pairs) == 0 {
		return nil
	}
	nb := nodeNeighbors(g)
	deg := nodeDegrees(g)

	for start := 0; start < len(pairs); start += tier2BatchSize {
		end := min(start+tier2BatchSize, len(pairs))
		batch := pairs[start:end]
		rep.Tier2Batches++
		rep.Tier2PairsChecked += len(batch)

		type nodeView struct {
			ID        string   `json:"id"`
			Type      string   `json:"type"`
			Name      string   `json:"name"`
			Aliases   []string `json:"aliases,omitempty"`
			Neighbors []string `json:"neighbors,omitempty"`
		}
		type pairView struct {
			PairID string   `json:"pair_id"`
			Hint   string   `json:"hint"`
			A      nodeView `json:"a"`
			B      nodeView `json:"b"`
		}
		makeView := func(id string) nodeView {
			n := g.Nodes[id]
			var nbs []string
			for nid := range nb[id] {
				if nn := g.Nodes[nid]; nn != nil && nn.Name != "" && nid != Me {
					nbs = append(nbs, nn.Name)
				}
			}
			sort.Strings(nbs)
			if len(nbs) > 6 {
				nbs = nbs[:6]
			}
			als := aliasNames(n)
			if len(als) > 4 {
				als = als[:4]
			}
			return nodeView{ID: n.ID, Type: n.Type, Name: n.Name, Aliases: als, Neighbors: nbs}
		}

		payload := struct {
			Pairs []pairView `json:"pairs"`
		}{}
		byPID := make(map[string]tier2Pair, len(batch))
		for i, p := range batch {
			pid := fmt.Sprintf("p%d", start+i+1)
			p.ID = pid
			byPID[pid] = p
			payload.Pairs = append(payload.Pairs, pairView{
				PairID: pid,
				Hint:   p.Reason,
				A:      makeView(p.A),
				B:      makeView(p.B),
			})
		}
		rawReq, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		resp, err := m.Complete(ctx, model.Request{
			Step:      "graph_resolve",
			Tier:      model.Small,
			System:    tier2SystemPrompt,
			User:      string(rawReq),
			MaxTokens: 2048,
			Schema:    tier2Schema,
		})
		if err != nil {
			return err
		}
		var parsed tier2Response
		if err := json.Unmarshal([]byte(extractJSONObject(resp.Text)), &parsed); err != nil {
			return fmt.Errorf("graph_resolve parse: %w", err)
		}
		for _, d := range parsed.Decisions {
			if d.Verdict != "same_entity" || d.Confidence != "high" {
				continue
			}
			p, ok := byPID[d.PairID]
			if !ok {
				continue
			}
			ra, rb := g.Resolve(p.A), g.Resolve(p.B)
			if ra == rb {
				continue
			}
			var from, into string
			switch d.Canonical {
			case "a":
				from, into = rb, ra
			case "b":
				from, into = ra, rb
			default:
				winner := pickWinner(g, []string{ra, rb}, deg, hadPrefix)
				if winner == ra {
					from, into = rb, ra
				} else {
					from, into = ra, rb
				}
			}
			reason := d.Reason
			if reason == "" {
				reason = p.Reason
			}
			if doMerge(from, into, ProposalTier2Haiku, "tier2_haiku", reason) {
				rep.Tier2Merged++
			}
		}
	}
	return nil
}

func tier2Candidates(g *store.Graph) []tier2Pair {
	nb := nodeNeighbors(g)
	type nameKey struct {
		key string
		tok string
	}
	type item struct {
		id      string
		typ     string
		key     string
		tok     string // leading ASCII token for bilingual matches like "enkeep" in "enkeep-memory" vs "enkeep 记忆插件"
		aliases []nameKey
	}
	var items []item
	for _, id := range sortedNodeIDs(g) {
		n := g.Nodes[id]
		if !isLiveNode(n) || id == Me || IsDisallowedNodeType(n.Type) {
			continue
		}
		k := NormKey(n.Name, n.Type)
		if utf8.RuneCountInString(k) < 2 || isGeneric(n.Name) {
			continue
		}
		var als []nameKey
		seenAl := map[string]bool{k: true}
		for _, a := range n.Aliases {
			ak := NormKey(a.Name, n.Type)
			if utf8.RuneCountInString(ak) < 2 || isGeneric(a.Name) || seenAl[ak] {
				continue
			}
			seenAl[ak] = true
			als = append(als, nameKey{key: ak, tok: leadingASCIIToken(a.Name)})
		}
		items = append(items, item{
			id:      id,
			typ:     n.Type,
			key:     k,
			tok:     leadingASCIIToken(n.Name),
			aliases: als,
		})
	}
	compatible := func(t1, t2 string) bool {
		if t1 == t2 {
			return true
		}
		isTech := func(t string) bool { return t == "project" || t == "system" || t == "repo" }
		return isTech(t1) && isTech(t2)
	}
	aliasEvidenceMatch := func(a, b item) (matched bool, exact bool) {
		check := func(x, y nameKey) bool {
			if x.key == y.key {
				exact = true
				return true
			}
			return fuzzyNameMatch(x.key, y.key, x.tok, y.tok)
		}
		ak := nameKey{key: a.key, tok: a.tok}
		bk := nameKey{key: b.key, tok: b.tok}
		for _, aa := range a.aliases {
			if check(aa, bk) {
				matched = true
				if exact {
					return true, true
				}
			}
		}
		for _, ba := range b.aliases {
			if check(ak, ba) {
				matched = true
				if exact {
					return true, true
				}
			}
		}
		for _, aa := range a.aliases {
			for _, ba := range b.aliases {
				if check(aa, ba) {
					matched = true
					if exact {
						return true, true
					}
				}
			}
		}
		return matched, exact
	}

	var out []tier2Pair
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			a, b := items[i], items[j]
			if !compatible(a.typ, b.typ) {
				continue
			}
			shared := countSharedNeighbors(nb, a.id, b.id)
			var reason string
			score := 0
			if fuzzyNameMatch(a.key, b.key, a.tok, b.tok) {
				reason = "prefix_or_containment"
				score = 10 + shared
			} else if ok, exact := aliasEvidenceMatch(a, b); ok {
				if exact {
					reason = "alias_exact"
					score = 12 + shared
				} else {
					reason = "alias_fuzzy"
					score = 8 + shared
				}
			}
			if reason != "" {
				out = append(out, tier2Pair{A: a.id, B: b.id, Reason: reason, Score: score})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	if len(out) > tier2MaxPairs {
		out = out[:tier2MaxPairs]
	}
	return out
}

func fuzzyNameMatch(ka, kb, toka, tokb string) bool {
	if ka == kb {
		return true
	}
	short, long := ka, kb
	if len(short) > len(long) {
		short, long = long, short
	}
	shortRunes := utf8.RuneCountInString(short)
	longRunes := utf8.RuneCountInString(long)
	if isASCIIString(short) {
		if shortRunes >= 4 && strings.Contains(long, short) && float64(shortRunes)/float64(longRunes) >= 0.30 {
			return true
		}
	} else {
		if shortRunes >= 2 && strings.Contains(long, short) && float64(shortRunes)/float64(longRunes) >= 0.40 {
			return true
		}
	}
	// Shared leading ASCII token >= 4 chars (e.g. "enkeep" in "enkeep-memory" vs "enkeep 记忆插件").
	if len(toka) >= 4 && toka == tokb {
		return true
	}
	return false
}

func leadingASCIIToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			break
		}
	}
	return b.String()
}

func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if j := strings.LastIndexByte(s, '}'); j > i {
			return s[i : j+1]
		}
	}
	return s
}

func isLiveNode(n *store.Node) bool {
	return n != nil && n.MergedInto == "" && n.Status != "archived" && n.Status != "not_entity"
}

func isPureHan(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.Is(unicode.Han, r) {
			return false
		}
	}
	return true
}

func isASCIIString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func aliasNames(n *store.Node) []string {
	var out []string
	for _, a := range n.Aliases {
		if a.Name != "" && !slices.Contains(out, a.Name) {
			out = append(out, a.Name)
		}
	}
	return out
}

func sortedNodeIDs(g *store.Graph) []string {
	ids := make([]string, 0, len(g.Nodes))
	for id := range g.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedAssertionIDs(g *store.Graph) []string {
	ids := make([]string, 0, len(g.Assertions))
	for id := range g.Assertions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
