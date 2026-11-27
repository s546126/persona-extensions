package export

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
)

type line struct {
	id   string
	text string
}

func (l line) String() string { return "- " + l.text + "\n" }

// fill takes lines in order while they fit in budget bytes (a heading included), and says what it used.
func fill(heading string, ls []line, budget int) (string, []string) {
	if len(ls) == 0 {
		return "", nil
	}
	var b strings.Builder
	var ids []string
	b.WriteString(heading + "\n")
	for _, l := range ls {
		s := l.String()
		if b.Len()+len(s) > budget {
			continue // a shorter line further down may still fit
		}
		b.WriteString(s)
		if l.id != "" {
			ids = append(ids, l.id)
		}
	}
	if b.Len() == len(heading)+1 {
		return "", nil // nothing fitted: the section is left out, never a heading alone
	}
	return b.String(), ids
}

type part struct {
	key, heading string
	lines        []line
}

// fillParts shares a budget between parts: each non-empty part first gets an equal floor (no facet crowds out
// another), then what is left goes to the parts that still have lines, in order.
func fillParts(head string, parts []part, budget int) (string, []string) {
	var live []part
	for _, p := range parts {
		if len(p.lines) > 0 {
			live = append(live, p)
		}
	}
	if len(live) == 0 {
		return "", nil
	}
	avail := budget - len(head) - 1
	floor := avail / len(live)
	taken := make([]int, len(live)) // lines taken per part
	size := make([]int, len(live))
	for i, p := range live {
		size[i] = len(p.heading) + 1
		for _, l := range p.lines {
			if size[i]+len(l.String()) > floor {
				break
			}
			size[i] += len(l.String())
			taken[i]++
		}
	}
	total := 0
	for _, s := range size {
		total += s
	}
	for i, p := range live { // leftover, in order
		for taken[i] < len(p.lines) && total+len(p.lines[taken[i]].String()) <= avail {
			total += len(p.lines[taken[i]].String())
			size[i] += len(p.lines[taken[i]].String())
			taken[i]++
		}
	}
	var b strings.Builder
	var ids []string
	b.WriteString(head + "\n")
	for i, p := range live {
		if taken[i] == 0 {
			continue
		}
		b.WriteString("\n" + p.heading + "\n")
		for _, l := range p.lines[:taken[i]] {
			b.WriteString(l.String())
			if l.id != "" {
				ids = append(ids, l.id)
			}
		}
	}
	out := b.String()
	for len(out) > budget && len(ids) > 0 { // the blank lines between parts: trim the last line if they overflow
		i := strings.LastIndex(strings.TrimRight(out, "\n"), "\n- ")
		if i < 0 {
			break
		}
		out, ids = out[:i+1], ids[:len(ids)-1]
	}
	return out, ids
}

func (c *compiler) lines(es []*entry.Entry, texts map[string]string, single bool) []line {
	var out []line
	for _, e := range es {
		if t := texts[e.ID]; t != "" {
			if single && e.SingleSource {
				t += c.fw.SingleSrc
			}
			out = append(out, line{id: e.ID, text: oneLine(t)})
		}
	}
	return dedupe(out)
}

// dedupe drops a line whose text an earlier line already says (two entries rewritten to the same words).
func dedupe(ls []line) []line {
	seen := map[string]bool{}
	out := ls[:0]
	for _, l := range ls {
		if !seen[l.text] {
			seen[l.text] = true
			out = append(out, l)
		}
	}
	return out
}

// commitmentLines are open commitments as titles with their due dates (no rewrite: the title is the promise).
func (c *compiler) commitmentLines(es []*entry.Entry) []line {
	var out []line
	for _, e := range es {
		if e.Commitment == nil {
			continue
		}
		t := oneLine(firstNonEmpty(e.Commitment.What, e.Shown()))
		if len(Check(t, c.o.Lang)) > 0 {
			continue
		}
		t = cutRunes(t, 80)
		if e.Commitment.Due != "" {
			t += fmt.Sprintf(c.fw.Due, e.Commitment.Due)
		}
		out = append(out, line{id: e.ID, text: t})
	}
	return out
}

func (c *compiler) render(p *plan, texts map[string]string) []file {
	var files []file
	// Exit A.
	files = append(files, c.instructions(p, texts))
	// Exit B.
	files = append(files, c.skills(p, texts)...)
	// Exit D.
	slugs := make([]string, 0, len(p.projects))
	for s := range p.projects {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		pr := p.projects[slug]
		var parts []part
		if c.on(entry.K19ProjectGeneral) {
			// The rules for working with the user that hold everywhere, ahead of the project's own: an agent does not
			// read the assistant's CLAUDE.md (0.2.0-beta.37). At most three lines, from exit A's first.
			gen := c.lines(p.a["interaction"], texts, false)
			if len(gen) > 3 {
				gen = gen[:3]
			}
			parts = append(parts, part{key: "general", heading: c.fw.ProjGen, lines: gen})
		}
		// T2 project block assembly (unified design §2.2): goal anchors, project-specific rules,
		// 1-hop owners/dependencies skeleton, then tools/judgement/workflow/world.
		var goalEntries, otherWorld []*entry.Entry
		for _, e := range pr.parts["world"] {
			if isProjectGoal(e) {
				goalEntries = append(goalEntries, e)
			} else {
				otherWorld = append(otherWorld, e)
			}
		}
		if len(goalEntries) > 0 {
			parts = append(parts, part{key: "goals", heading: c.fw.ProjParts["goals"], lines: c.lines(goalEntries, texts, true)})
		}
		if len(pr.parts["interaction"]) > 0 {
			heading := c.fw.ProjParts["interaction"]
			if heading == "" {
				heading = c.fw.Sections["interaction"]
			}
			parts = append(parts, part{key: "interaction", heading: heading, lines: c.lines(pr.parts["interaction"], texts, false)})
		}
		if len(pr.skeleton) > 0 {
			var sl []line
			for _, s := range pr.skeleton {
				sl = append(sl, line{text: s})
			}
			parts = append(parts, part{key: "skeleton", heading: c.fw.ProjParts["skeleton"], lines: sl})
		}
		for _, f := range []string{"tools", "judgement", "workflow", "world"} {
			src := pr.parts[f]
			if f == "world" {
				src = otherWorld
			}
			var ls []line
			for _, e := range src {
				if openCommitment(e) {
					ls = append(ls, c.commitmentLines([]*entry.Entry{e})...)
				} else {
					ls = append(ls, c.lines([]*entry.Entry{e}, texts, f == "world")...)
				}
			}
			heading := c.fw.ProjParts[f]
			if heading == "" {
				heading = c.fw.Sections[f]
			}
			parts = append(parts, part{key: f, heading: heading, lines: ls})
		}
		own := false
		for _, pt := range parts {
			own = own || (pt.key != "general" && len(pt.lines) > 0)
		}
		if !own {
			continue // the general part alone says nothing about the project
		}
		content, ids := fillParts(fmt.Sprintf(c.fw.Project, pr.name), parts, ProjectMax)
		if content != "" {
			files = append(files, file{path: path.Join(ProjectsDir, slug+".md"), exit: "D", content: content, ids: ids})
		}
	}
	// Exit E.
	var world []line
	if len(p.contacts) > 0 {
		world = append(world, line{text: fmt.Sprintf(c.fw.Contacts, list(c.o.Lang, p.contacts))})
	}
	world = append(world, c.commitmentLines(p.inbox["world"])...)
	var people []line
	for _, e := range p.inbox["people"] {
		if t := texts[e.ID]; t != "" {
			people = append(people, line{id: e.ID, text: fmt.Sprintf(c.fw.With, c.name(e.Person)) + oneLine(t)})
		}
	}
	people = dedupe(people)
	parts := []part{
		{key: "world", heading: c.fw.InboxParts["world"], lines: world},
		{key: "people", heading: c.fw.InboxParts["people"], lines: people},
		{key: "persona", heading: c.fw.InboxParts["persona"], lines: c.lines(p.inbox["persona"], texts, false)},
		{key: "interaction", heading: c.fw.InboxParts["interaction"], lines: c.lines(p.inbox["interaction"], texts, false)},
	}
	content, ids := fillParts(strings.TrimSpace(strings.Split(c.fw.Title, "\n")[0]), parts, InboxMax)
	files = append(files, file{path: InboxContextPath, exit: "E", content: content, ids: ids})
	var nb strings.Builder
	for _, l := range c.never {
		if nb.Len()+len(l)+1 > NeverQuoteMax {
			break
		}
		nb.WriteString(l + "\n")
	}
	files = append(files, file{path: NeverQuotePath, exit: "E", content: nb.String()})
	return files
}

// instructions is exit A: sections in their order, each first given its share of what the title and the fixed lines
// leave of 6 KB (SectionShare), then what the sections did not use goes to those that still have lines, in order;
// each ordered by count × source kinds; an empty section is left out.
func (c *compiler) instructions(p *plan, texts map[string]string) file {
	f := file{path: InstructionsPath, exit: "A"}
	if !c.on(entry.K1PerTurnBlock) {
		return f
	}
	world := c.commitmentLines(p.a["world"])
	if c.on(entry.K18NamesIndex) {
		world = append(world, c.namesIndex(p)...)
	}
	sections := map[string][]line{
		"interaction": c.lines(p.a["interaction"], texts, false),
		"judgement":   c.lines(p.a["judgement"], texts, false),
		"workflow":    c.lines(p.a["workflow"], texts, false),
		"world":       world,
		"persona":     c.lines(p.a["persona"], texts, false),
	}
	askHint := entry.Switch(c.st.Switches, entry.K2Judgement) != "off" && len(sections["judgement"]) > 0
	head := c.fw.Title + "\n"
	tail := "\n- " + c.fw.PeopleNote + "\n- " + c.fw.RecallHint + "\n"
	if askHint {
		tail += "- " + c.fw.AskHint + "\n"
	}
	avail := InstructionsMax - len(head) - len(tail) - len(SectionOrder) // a blank line before each section
	render := func(s string, budget int) (string, []string) {
		if s == "tools" {
			return c.toolsIndex(p.objects, budget), nil
		}
		return fill(c.fw.Sections[s], sections[s], budget)
	}
	texts2 := map[string]string{}
	got := map[string][]string{}
	used := 0
	for _, s := range SectionOrder {
		texts2[s], got[s] = render(s, avail*SectionShare[s]/100)
		used += len(texts2[s])
	}
	// What is left goes to the sections that left lines out, in proportion to their shares; a last round in order
	// takes what rounding and lines too long for a part left.
	hungry := func(s string) bool { return s != "tools" && len(got[s]) < len(sections[s]) }
	for round := 0; round < 4; round++ {
		left := avail - used
		if left <= 0 {
			break
		}
		shares := 0
		for _, s := range SectionOrder {
			if hungry(s) {
				shares += SectionShare[s]
			}
		}
		if shares == 0 {
			break
		}
		for _, s := range SectionOrder {
			if !hungry(s) {
				continue
			}
			extra := left * SectionShare[s] / shares
			if round == 3 {
				extra = avail - used
			}
			if t, g := render(s, len(texts2[s])+extra); t != "" && len(t) > len(texts2[s]) {
				used += len(t) - len(texts2[s])
				texts2[s], got[s] = t, g
			}
		}
	}
	var b strings.Builder
	var ids []string
	for _, s := range SectionOrder {
		if texts2[s] == "" {
			continue
		}
		c.rep.Sections[s] = len(texts2[s])
		c.rep.SecLines[s] = len(got[s])
		b.WriteString("\n" + texts2[s])
		ids = append(ids, got[s]...)
	}
	if b.Len() == 0 {
		return f // nothing to say: an empty file, no placeholder
	}
	f.content = head + b.String() + tail
	f.ids = ids
	return f
}

// namesIndex is ⑤'s names index in exit A (K18): the people the user deals with most and the projects with the most
// entries, names only; what is known about them is looked up with decisions_query (the hub recalls memory.entries).
func (c *compiler) namesIndex(p *plan) []line {
	var out []line
	if c.on(entry.K14People) {
		if names := c.contactNames(8); len(names) > 0 {
			out = append(out, line{text: fmt.Sprintf(c.fw.Contacts, list(c.o.Lang, names))})
		}
	}
	type pc struct {
		name string
		n    int
	}
	var ps []pc
	for _, pr := range p.projects {
		n := 0
		for _, es := range pr.parts {
			n += len(es)
		}
		ps = append(ps, pc{pr.name, n})
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].n != ps[j].n {
			return ps[i].n > ps[j].n
		}
		return ps[i].name < ps[j].name
	})
	var names []string
	for _, x := range ps {
		if len(names) == 8 {
			break
		}
		names = append(names, x.name)
	}
	if len(names) > 0 {
		out = append(out, line{text: fmt.Sprintf(c.fw.Projects, list(c.o.Lang, names))})
	}
	return out
}

// toolsIndex is ②'s one line in exit A: where the details are, and which machines and tools they cover.
func (c *compiler) toolsIndex(objects []string, budget int) string {
	if len(objects) == 0 {
		return ""
	}
	heading := c.fw.Sections["tools"] + "\n"
	var out string
	for n := len(objects); n >= 1; n-- {
		out = heading + "- " + fmt.Sprintf(c.fw.ToolsIndex, list(c.o.Lang, objects[:n])) + "\n"
		if len(out) <= budget {
			return out
		}
	}
	return ""
}

// skills is exit B: the machines-and-tools skill (② by object), a look-up handbook, within 256 KB (the least used
// lines go first when over).
func (c *compiler) skills(p *plan, texts map[string]string) []file {
	type skill struct {
		dir, desc, title string
		groups           []part
	}
	var ss []*skill
	if len(p.tools) > 0 {
		byObj := map[string][]*entry.Entry{} // by the object's name in lower case; shown as first written
		shown := map[string]string{}
		var order []string
		for _, e := range p.tools {
			o := objectOf(e)
			if o == "" {
				o = c.fw.General
			}
			k := strings.ToLower(o)
			if _, ok := byObj[k]; !ok {
				order, shown[k] = append(order, k), o
			}
			byObj[k] = append(byObj[k], e)
		}
		sk := &skill{dir: ToolsSkill, title: c.fw.ToolsSkill, desc: fmt.Sprintf(c.fw.ToolsDesc, list(c.o.Lang, capList(p.objects, 900)))}
		for _, k := range order {
			if ls := c.lines(byObj[k], texts, false); len(ls) > 0 {
				sk.groups = append(sk.groups, part{heading: "## " + shown[k], lines: ls})
			}
		}
		if len(sk.groups) > 0 {
			ss = append(ss, sk)
		}
	}
	render := func(sk *skill) (string, []string) {
		desc, _ := json.Marshal(sk.desc) // a YAML double-quoted scalar
		var b strings.Builder
		var ids []string
		fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\n---\n\n%s\n", sk.dir, desc, sk.title)
		for _, g := range sk.groups {
			if len(g.lines) == 0 {
				continue
			}
			b.WriteString("\n")
			if g.heading != "" {
				b.WriteString(g.heading + "\n")
			}
			for _, l := range g.lines {
				b.WriteString(l.String())
				ids = append(ids, l.id)
			}
		}
		return b.String(), ids
	}
	for {
		total, biggest, at := 0, 0, -1
		for i, sk := range ss {
			s, _ := render(sk)
			total += len(s)
			if len(s) > biggest {
				biggest, at = len(s), i
			}
		}
		if total <= SkillsMax || at < 0 {
			break
		}
		g := ss[at].groups
		last := len(g) - 1
		for last >= 0 && len(g[last].lines) == 0 {
			last--
		}
		if last < 0 {
			break
		}
		g[last].lines = g[last].lines[:len(g[last].lines)-1]
	}
	var out []file
	for _, sk := range ss {
		s, ids := render(sk)
		if len(ids) == 0 {
			continue
		}
		out = append(out, file{path: path.Join(SkillsDir, sk.dir, "SKILL.md"), exit: "B", content: s, ids: ids})
	}
	return out
}

func capList(xs []string, bytes int) []string {
	n := 0
	for i, x := range xs {
		n += len(x) + 3
		if n > bytes {
			return xs[:i]
		}
	}
	return xs
}

var (
	notSlugRune    = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	trailingParens = regexp.MustCompile(`\s*[（(][^（()）]*[)）]\s*$`)
)

// stripProjectDisplay removes display-form decorations ("名称（别名）：一句话" → "名称") before matching or slugging,
// mirroring the hub's kb.FindProject displayBases rule.
func stripProjectDisplay(name string) string {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "project:"))
	if i := strings.Index(s, "："); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	for i := 0; i < 4; i++ {
		next := strings.TrimSpace(trailingParens.ReplaceAllString(s, ""))
		if next == "" || next == s {
			break
		}
		s = next
	}
	return s
}

func isProjectGoal(e *entry.Entry) bool {
	if e == nil || e.Facet != entry.World || openCommitment(e) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(e.Type)) {
	case "goal", "objective", "target", "milestone", "decision", "decided":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(e.Predicate)) {
	case "decided", "decides":
		return true
	}
	return false
}

// KBSlug is a project name as the knowledge base makes its directory name and the hub looks exit D files up by
// (kb.Slug): lower case, every run of characters that are neither letters nor digits (of any script) one hyphen, no
// hyphen at either end, at most 40 characters.
func KBSlug(name string) string {
	base := stripProjectDisplay(name)
	if base == "" {
		base = name
	}
	return cutRunes(strings.Trim(notSlugRune.ReplaceAllString(strings.ToLower(base), "-"), "-"), 40)
}

var projectSlugRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}-]{0,79}$`)

// projectSlug says whether the hub reads a project file of this name at all (extmgr projectSlug).
func projectSlug(s string) bool { return projectSlugRe.MatchString(s) }

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}
