package ui

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/spaces/internal/templates"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

// overTab is the value of the overview tab, which no type key can take.
const overTab = "/"

// cardRows is the most entities in progress the overview card lists.
const (
	cardRows        = 5
	focusRows       = 6
	upcomingHorizon = 14
)

// pickSlot is the new space's state while its template is not chosen yet.
const pickSlot = "?"

// welcome is the screen with no space: what spaces are for with a few examples, the first step, and a row per
// template, which opens that template's form.
func (c *ctx) welcome() *wire.View {
	return &wire.View{Sections: []wire.Section{
		{Items: []wire.Component{
			{Type: wire.ComponentHeader, Title: "空间", Subtitle: "自己搭几个小本子，把想记的事分门别类记下来"},
			{Type: wire.ComponentText, Style: wire.TextStyleBody, Text: "比如：读书清单（想读、在读、读完，读到第几页）；" +
				"订阅和续费（每月花多少钱、哪天续费）；家里的东西（放在哪、还剩多少、想买什么）。"},
			caption("还没有空间。第一步：从下面选一个模板，马上就能用；也可以直接跟助手说「帮我建一个读书清单」。"),
		}},
		{Title: "选一个模板开始", Items: templateRows()},
	}}
}

// newSpacePage is the screen of a new space the state names: the template list, or the form of its template. With no
// space the list is the welcome. It is nil when the state names no new space, or a template that does not exist.
func (c *ctx) newSpacePage() *wire.View {
	switch c.st.C {
	case "":
		return nil
	case pickSlot:
		if c.sp == nil {
			c.st.C = ""
			return c.welcome()
		}
		return picker()
	}
	t, ok := templates.Get(c.st.C)
	if !ok {
		c.st.C = ""
		return nil
	}
	return c.createForm(t)
}

// picker is the template list of a new space, and the button that leaves it.
func picker() *wire.View {
	items := []wire.Component{{Type: wire.ComponentHeader, Title: "新建空间", Subtitle: "选一个模板，之后里面的分类和填写项都还能改"}}
	return &wire.View{Sections: []wire.Section{
		{Items: append(items, templateRows()...)},
		{Items: []wire.Component{button("取消", &wire.Action{ID: "cancel"}, wire.ButtonRoleNormal)}},
	}}
}

// templateRows is a row per template: its icon, name and about, and the names of its types. A row opens the
// template's form.
func templateRows() []wire.Component {
	out := []wire.Component{}
	for _, t := range templates.All() {
		out = append(out, wire.Component{
			Type: wire.ComponentRow, Icon: iconName(t.Icon), Title: t.Name, Subtitle: cut(t.About, maxText),
			Value: templateTypeNames(t), Action: &wire.Action{ID: "new:" + t.Key},
		})
	}
	return out
}

// rowTypeNames is how many type names a template row shows; a template with more shows the first ones and how many
// there are in all.
const rowTypeNames = 4

// templateTypeNames is the names of the template's types for its row, such as 「书 · 笔记 · 剪藏 · 主题」, or
// 「a · b · c 等 6 个」 for a template with more than rowTypeNames types; "" for a template without types.
func templateTypeNames(t templates.Template) string {
	var names []string
	for _, ty := range t.Types {
		names = append(names, ty.Name)
	}
	if len(names) > rowTypeNames {
		return strings.Join(names[:rowTypeNames-1], " · ") + fmt.Sprintf(" 等 %d 个", len(names))
	}
	return strings.Join(names, " · ")
}

// createForm is the form of a new space from a template: what the template brings, the name (a free default), the
// samples choice when it has samples, the primary create, and the button back to the template list.
func (c *ctx) createForm(t templates.Template) *wire.View {
	items := []wire.Component{{Type: wire.ComponentHeader, Title: t.Name, Subtitle: cut(t.About, maxText)}}
	if len(t.Types) > 0 {
		var names []string
		for _, ty := range t.Types {
			names = append(names, ty.Name)
		}
		items = append(items, wire.Component{
			Type: wire.ComponentText, Text: cut("里面有这几个分类："+strings.Join(names, "、"), maxText), Style: wire.TextStyleCaption,
		})
	}
	items = append(items, wire.Component{
		Type: wire.ComponentField, Name: "name", Label: "空间名称", Kind: wire.FieldKindText, Value: c.defaultName(t),
	})
	if len(t.Samples) > 0 {
		items = append(items, toggleField("samples", "先放几条示例（之后可以删）", true))
	}
	items = append(items, button("创建空间", &wire.Action{ID: "create:" + t.Key, Submit: true}, wire.ButtonRoleNormal))
	return &wire.View{Sections: []wire.Section{
		{Items: items},
		{Items: []wire.Component{button("返回", &wire.Action{ID: "new"}, wire.ButtonRoleNormal)}},
	}}
}

// defaultName is the name a new space from the template gets: the template's name, followed by " 2", " 3", ... when
// a space already has it.
func (c *ctx) defaultName(t templates.Template) string {
	taken := map[string]bool{}
	for _, sp := range c.b.Spaces {
		if sp != nil {
			taken[sp.Name] = true
		}
	}
	return freeName(t.Name, taken)
}

// freeName is base, or base followed by " 2", " 3", ... when taken has it. The answer is never longer than a name
// may be: base is cut to the limit, and cut further to leave room for the suffix, so the name can be saved as it is.
func freeName(base string, taken map[string]bool) string {
	r := []rune(strings.TrimSpace(base))
	if len(r) > kernel.MaxName {
		r = r[:kernel.MaxName]
	}
	name := strings.TrimSpace(string(r))
	for i := 2; taken[name]; i++ {
		suffix := fmt.Sprintf(" %d", i)
		head := r
		if room := kernel.MaxName - len(suffix); len(head) > room {
			head = head[:room]
		}
		name = strings.TrimSpace(string(head)) + suffix
	}
	return name
}

// overview is the space workbench with no type tab: a responsive multi-column layout on wide screens (stacking on
// phones) with the space hero and totals at full width, open folds directly below it, ontology-driven focus cards
// (upcoming/overdue dates and items in progress), type cards with direct creation shortcuts, and recent updates.
func (c *ctx) overview() (*wire.View, error) {
	sp := c.sp
	filter, tabs := c.chrome()
	v := &wire.View{
		Sections: []wire.Section{}, Filter: filter, Tabs: tabs,
		Layout: wire.LayoutColumns,
	}
	upcoming := c.w.Upcoming(c.todayStr(), upcomingHorizon)
	headItems := []wire.Component{
		c.spaceHeader(),
		statGrid(spaceStats(tallyOf(c.w, sp.Entities), len(upcoming))),
	}
	if len(sp.Types) > 0 {
		headItems = append(headItems, button("快速记一条", &wire.Action{ID: "fold:quick"}, wire.ButtonRoleNormal))
		if c.uiAtLeast(10) && len(c.acyclicSchemaEdges()) > 0 {
			headItems = append(headItems, button("关系图", &wire.Action{ID: "fold:graph"}, wire.ButtonRoleNormal))
		}
	}
	headItems = append(headItems,
		button("空间设置", &wire.Action{ID: "fold:space"}, wire.ButtonRoleNormal),
		button("新建空间", &wire.Action{ID: "new"}, wire.ButtonRoleNormal),
	)
	v.Sections = append(v.Sections, wire.Section{Span: wire.SectionSpanFull, Items: headItems})
	switch c.st.F {
	case "space":
		v.Sections = append(v.Sections, c.spaceFold())
	case "quick":
		if len(sp.Types) > 0 {
			v.Sections = append(v.Sections, c.quickSpaceFold())
		}
	case "graph":
		if len(sp.Types) > 0 {
			v.Sections = append(v.Sections, c.spaceGraphSection())
		}
	}
	if len(sp.Types) == 0 {
		// the empty state's own button adds the first category
		v.Sections = append(v.Sections, wire.Section{Span: wire.SectionSpanFull, Items: []wire.Component{c.noTypes()}})
		return v, nil
	}
	if len(upcoming) > 0 {
		v.Sections = append(v.Sections, c.upcomingSection(upcoming))
	}
	if doing := c.doingEntities(focusRows); len(doing) > 0 {
		v.Sections = append(v.Sections, wire.Section{
			Title: fmt.Sprintf("正在进行 · %d", len(doing)),
			Items: c.rows(doing, true),
		})
	}
	types := append(c.typeRows(), button("新建分类", ext.Open("schema:+", "", state{S: sp.ID, D: "+"}, "新建分类"), wire.ButtonRoleNormal))
	v.Sections = append(v.Sections, wire.Section{
		Title: "分类", Items: types,
		Footer: cut("点一个分类，查看和添加条目；也可以直接跟助手说「在「"+sp.Name+"」里记一条……」。", maxText),
	})
	recent, err := c.recent()
	if err != nil {
		return nil, err
	}
	if len(recent) > 0 {
		v.Sections = append(v.Sections, wire.Section{Title: "最近更新", Items: c.rows(recent, true)})
	}
	return v, nil
}

// chrome is the space's filter (only with two or more spaces) and its tabs: the overview and a tab per type, each
// with its count. A space without types has no tabs.
func (c *ctx) chrome() (*wire.Filter, *wire.Tabs) {
	var filter *wire.Filter
	if len(c.b.Spaces) >= 2 {
		choices := []wire.Choice{}
		for _, sp := range c.b.Spaces {
			choices = append(choices, wire.Choice{Value: sp.ID, Title: sp.Name})
		}
		filter = &wire.Filter{Name: "space", Title: "空间", Choices: choices, Value: c.sp.ID, Action: wire.Action{ID: "space"}}
	}
	if len(c.sp.Types) == 0 {
		return filter, nil
	}
	items := []wire.Tab{{Value: overTab, Title: "概览"}}
	for _, t := range c.sp.Types {
		items = append(items, wire.Tab{Value: t.Key, Title: t.Name, Badge: strconv.Itoa(len(c.entitiesOf(t.Key)))})
	}
	cur := overTab
	if c.typeOf(c.st.T) != nil {
		cur = c.st.T
	}
	return filter, &wire.Tabs{Name: "tab", Value: cur, Items: items, Action: wire.Action{ID: "tab"}}
}

// spaceHeader names the space, with its about and its facts: the number of types, entities, links and latest change.
func (c *ctx) spaceHeader() wire.Component {
	sp := c.sp
	sum := c.w.Summary()
	facts := []wire.Fact{
		{Label: "分类", Value: fmt.Sprintf("%d 个", len(sp.Types))},
		{Label: "条目", Value: fmt.Sprintf("%d 条", len(sp.Entities))},
	}
	if sum.Relations > 0 {
		facts = append(facts, wire.Fact{Label: "连接", Value: fmt.Sprintf("%d 条", sum.Relations)})
	}
	if last := latest(sp.Entities); last != nil {
		facts = append(facts, wire.Fact{Label: "最近更新", Value: c.ago(last.Updated)})
	}
	return wire.Component{Type: wire.ComponentHeader, Title: sp.Name, Subtitle: cut(sp.About, maxText), Facts: facts}
}

// noTypes is the overview of a space with no types: one empty state, whose button adds the first type.
func (c *ctx) noTypes() wire.Component {
	return wire.Component{
		Type: wire.ComponentEmpty, Title: "这个空间还是空的",
		Detail: "分类就是你要记的一种东西，例如「电影」「药品」。先建一个分类，再往里加条目。",
		Button: &wire.EmptyButton{Title: "新建分类", Action: *ext.Open("schema:+", "", state{S: c.sp.ID, D: "+"}, "新建分类")},
	}
}

// typeRows is a row per type: its icon and name, the kind of its default view with its count and active workload, and
// its value: the share done when it has a flow, else the count. Each row opens that type's tab and offers quick-add
// and schema-editor shortcuts in its row actions.
func (c *ctx) typeRows() []wire.Component {
	out := []wire.Component{}
	for _, t := range c.sp.Types {
		set := c.entitiesOf(t.Key)
		tl := tallyOf(c.w, set)
		value := strconv.Itoa(len(set))
		if pct, ok := tl.percent(); ok && len(t.Flow) > 0 {
			value = fmt.Sprintf("完成 %d%%", pct)
		}
		parts := []string{strconv.Itoa(len(set)) + " 条"}
		if tl.doing > 0 {
			parts = append(parts, fmt.Sprintf("进行中 %d", tl.doing))
		}
		parts = append(parts, "显示成"+viewLabels[t.ViewsOrDefault()[0].Kind])
		row := wire.Component{
			Type: wire.ComponentRow, Icon: iconName(t.Icon), Title: t.Name,
			Subtitle: strings.Join(parts, " · "), Value: value,
			Action: &wire.Action{ID: "tab:" + t.Key},
		}
		if c.rowActBudget > 0 {
			c.rowActBudget--
			row.Actions = []wire.RowAction{
				{Title: "新建" + t.Name, Swipe: true, Action: wire.Action{ID: "tab:" + t.Key + ":new"}},
				{Title: "分类设置", Action: *ext.Open("schema:"+t.Key, "", state{S: c.sp.ID, D: t.Key}, "分类设置")},
			}
		}
		out = append(out, row)
	}
	return out
}

// doingEntities returns up to limit entities in phase doing, newest updated first.
func (c *ctx) doingEntities(limit int) []*kernel.Entity {
	var out []*kernel.Entity
	for _, e := range c.sp.Entities {
		if e != nil && phaseOf(c.w, e) == string(kernel.Doing) {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b *kernel.Entity) int { return b.Updated.Compare(a.Updated) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// upcomingSection renders upcoming and recently overdue items detected from date properties across all types.
func (c *ctx) upcomingSection(items []kernel.DueItem) wire.Section {
	shown := items
	if len(shown) > focusRows {
		shown = shown[:focusRows]
	}
	rows := make([]wire.Component, 0, len(shown))
	for _, it := range shown {
		badgeText, tone := dueBadge(it.Days)
		r := wire.Component{
			Type:     wire.ComponentRow,
			Icon:     iconName(it.Type.Icon),
			Title:    cut(it.Entity.Title, maxTitle),
			Subtitle: cut(it.Type.Name+" · "+it.Prop.Name+" "+it.Date, maxText),
			Badge:    &wire.Badge{Text: badgeText, Tone: tone},
			Action:   ext.Open("open:"+it.Entity.ID, PageUI, state{S: c.sp.ID, E: it.Entity.ID}, cut(it.Entity.Title, maxTitle)),
		}
		if acts := c.entityRowActions(it.Type, it.Entity); len(acts) > 0 {
			r.Actions = acts
		}
		rows = append(rows, r)
	}
	footer := ""
	if len(items) > focusRows {
		footer = fmt.Sprintf("共 %d 条近期提醒，其余可在对应分类中查看", len(items))
	}
	return wire.Section{Title: fmt.Sprintf("需要关注 · %d", len(items)), Footer: footer, Items: rows}
}

// dueBadge formats a relative day offset into a concise badge label and tone.
func dueBadge(days int) (string, string) {
	switch {
	case days < 0:
		return fmt.Sprintf("逾期 %d 天", -days), wire.ToneDanger
	case days == 0:
		return "今天", wire.ToneCaution
	case days == 1:
		return "明天", wire.ToneCaution
	case days == 2:
		return "后天", wire.ToneNeutral
	default:
		return fmt.Sprintf("%d 天后", days), wire.ToneNeutral
	}
}

// quickSpaceFold is a space-level quick capture form that lets the user pick any type and add a record right from the
// overview without switching tabs.
func (c *ctx) quickSpaceFold() wire.Section {
	choices := []wire.Choice{}
	for _, ty := range c.sp.Types {
		if ty != nil {
			choices = append(choices, wire.Choice{Value: choicePrefix + ty.Key, Title: ty.Name})
		}
	}
	return wire.Section{
		Span:  wire.SectionSpanFull,
		Title: "快速记一条",
		Items: []wire.Component{
			{Type: wire.ComponentField, Name: "type", Label: "放到哪个分类", Kind: wire.FieldKindChoice, Value: choices[0].Value, Choices: choices},
			{Type: wire.ComponentField, Name: "title", Label: "标题", Kind: wire.FieldKindText, Placeholder: "写下这一条叫什么"},
			button("添加", &wire.Action{ID: "quickadd", Submit: true}, wire.ButtonRoleNormal),
		},
	}
}

// acyclicSchemaEdges returns deduplicated non-self schema relation edges filtered to guarantee a DAG for wire.CheckGraph.
func (c *ctx) acyclicSchemaEdges() []kernel.SchemaEdge {
	raw := c.w.SchemaEdges()
	var out []kernel.SchemaEdge
	seen := map[[2]string]bool{}
	adj := map[string][]string{}
	reaches := func(start, goal string) bool {
		visited := map[string]bool{}
		queue := []string{start}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if cur == goal {
				return true
			}
			for _, nxt := range adj[cur] {
				if !visited[nxt] {
					visited[nxt] = true
					queue = append(queue, nxt)
				}
			}
		}
		return false
	}
	for _, e := range raw {
		if e.FromType == e.ToType {
			continue
		}
		pair := [2]string{e.FromType, e.ToType}
		if seen[pair] || reaches(e.ToType, e.FromType) {
			continue
		}
		seen[pair] = true
		adj[e.FromType] = append(adj[e.FromType], e.ToType)
		out = append(out, e)
	}
	return out
}

// spaceGraphSection renders an interactive layered ontology graph of the space's types and their connections.
func (c *ctx) spaceGraphSection() wire.Section {
	nodes := make([]wire.GraphNode, 0, len(c.sp.Types))
	for _, t := range c.sp.Types {
		if t == nil {
			continue
		}
		set := c.entitiesOf(t.Key)
		tl := tallyOf(c.w, set)
		node := wire.GraphNode{
			ID:       t.Key,
			Title:    cut(t.Name, maxTitle),
			Subtitle: fmt.Sprintf("%d 条", len(set)),
			Icon:     iconName(t.Icon),
			Action:   &wire.Action{ID: "tab:" + t.Key},
		}
		if pct, ok := tl.percent(); ok && len(t.Flow) > 0 {
			r := float64(pct) / 100.0
			node.Progress = &r
			node.Badge = &wire.Badge{Text: fmt.Sprintf("%d%%", pct), Tone: wire.TonePositive}
		} else if tl.doing > 0 {
			node.Badge = &wire.Badge{Text: fmt.Sprintf("进行中 %d", tl.doing), Tone: wire.ToneCaution}
		}
		nodes = append(nodes, node)
	}
	edges := []wire.GraphEdge{}
	for _, e := range c.acyclicSchemaEdges() {
		edges = append(edges, wire.GraphEdge{From: e.FromType, To: e.ToType})
	}
	return wire.Section{
		Span:   wire.SectionSpanFull,
		Title:  "分类关系图",
		Footer: "点任意节点即可跳到对应分类",
		Items: []wire.Component{{
			Type:      wire.ComponentGraph,
			Key:       "space_graph_" + c.sp.ID,
			Layout:    wire.GraphLayoutLayered,
			Direction: wire.GraphDirectionRight,
			Nodes:     nodes,
			Edges:     edges,
		}},
	}
}

// recent are the five latest entities of the space, of any type.
func (c *ctx) recent() ([]*kernel.Entity, error) {
	res, err := c.w.Run(kernel.Query{Limit: cardRows})
	if err != nil {
		return nil, err
	}
	var out []*kernel.Entity
	for _, g := range res.Groups {
		out = append(out, g.Items...)
	}
	return out, nil
}

// spaceFold is the settings of the space, open: its name and about, save, and the destructive delete, whose
// confirmation names what it deletes.
func (c *ctx) spaceFold() wire.Section {
	sp := c.sp
	return wire.Section{Span: wire.SectionSpanFull, Items: []wire.Component{
		{Type: wire.ComponentField, Name: "name", Label: "名称", Kind: wire.FieldKindText, Value: sp.Name},
		{Type: wire.ComponentField, Name: "about", Label: "简介", Kind: wire.FieldKindText, Value: sp.About,
			Placeholder: "一句话说明这个空间记什么，例如 家里每月的订阅"},
		button("保存", &wire.Action{ID: "save", Submit: true}, wire.ButtonRoleNormal),
		button("删除空间", &wire.Action{ID: "drop", Confirm: fmt.Sprintf(
			"删除空间「%s」？里面的 %d 个分类和全部 %d 条会一起删除，不能撤销。", sp.Name, len(sp.Types), len(sp.Entities))}, wire.ButtonRoleDestructive),
	}}
}

// card draws the overview card: the totals over all spaces, and up to five entities in progress, newest first.
func (c *ctx) card() *wire.View {
	if len(c.b.Spaces) == 0 {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{
			Type: wire.ComponentEmpty, Title: "还没有空间",
			Detail: "选个模板就能开始记，比如读书清单、订阅续费、家里的东西",
			Button: &wire.EmptyButton{Title: "选个模板", Action: *ext.Open("start", PageUI, state{}, "空间")},
		}}}}}
	}
	var all tally
	var doing []located
	entries := 0
	upcoming := 0
	today := c.todayStr()
	for _, sp := range c.b.Spaces {
		entries += len(sp.Entities)
		w := kernel.Open(sp)
		all.add(tallyOf(w, sp.Entities))
		upcoming += len(w.Upcoming(today, 7))
		for _, e := range sp.Entities {
			if phaseOf(w, e) == string(kernel.Doing) {
				doing = append(doing, located{sp: sp, e: e})
			}
		}
	}
	slices.SortStableFunc(doing, func(a, b located) int { return b.e.Updated.Compare(a.e.Updated) })
	if len(doing) > cardRows {
		doing = doing[:cardRows]
	}
	pct, detail, tone := "—", "", ""
	if p, ok := all.percent(); ok {
		pct, detail, tone = fmt.Sprintf("%d%%", p), fmt.Sprintf("已完成 %d", all.done), wire.TonePositive
	}
	todoDetail, todoTone := "", ""
	if upcoming > 0 {
		todoDetail, todoTone = fmt.Sprintf("近期提醒 %d", upcoming), wire.ToneCaution
	}
	v := &wire.View{Sections: []wire.Section{{Items: []wire.Component{statGrid([]wire.Stat{
		{Label: "进行中", Value: strconv.Itoa(all.doing), Detail: fmt.Sprintf("%d 个空间，共 %d 条", len(c.b.Spaces), entries)},
		{Label: "待办", Value: strconv.Itoa(all.todo), Detail: todoDetail, Tone: todoTone},
		{Label: "完成度", Value: pct, Detail: detail, Tone: tone},
	})}}}}
	if len(doing) > 0 {
		items := []wire.Component{}
		for _, l := range doing {
			items = append(items, c.cardRow(l))
		}
		v.Sections = append(v.Sections, wire.Section{Title: "正在进行", Items: items})
	}
	return v
}

// cardRow is an entity in progress on the card: its space and type, its toned status, and the page that opens it.
func (c *ctx) cardRow(l located) wire.Component {
	t := typeIn(l.sp, l.e.Type)
	subtitle := l.sp.Name
	row := wire.Component{
		Type: wire.ComponentRow, Title: cut(l.e.Title, maxTitle),
		Action: ext.Open("open:"+l.e.ID, PageUI, state{S: l.sp.ID, E: l.e.ID}, l.e.Title),
	}
	if t != nil {
		subtitle += " · " + t.Name
		if l.e.Status != "" {
			row.Badge = &wire.Badge{Text: l.e.Status, Tone: toneOf(t, l.e.Status)}
		}
	}
	row.Subtitle = subtitle
	return row
}

// located is an entity with the space it is in.
type located struct {
	sp *kernel.Space
	e  *kernel.Entity
}

// tally counts entities by phase. Entities without a flow count for nothing.
type tally struct{ todo, doing, done int }

func tallyOf(w *kernel.World, set []*kernel.Entity) tally {
	var t tally
	for _, e := range set {
		switch phaseOf(w, e) {
		case string(kernel.Todo):
			t.todo++
		case string(kernel.Doing):
			t.doing++
		case string(kernel.Done):
			t.done++
		}
	}
	return t
}

func (t *tally) add(o tally) {
	t.todo += o.todo
	t.doing += o.doing
	t.done += o.done
}

// percent is the share of the entities in phase done among those with a flow that are not dropped; false when there
// are none.
func (t tally) percent() (int, bool) {
	den := t.todo + t.doing + t.done
	if den == 0 {
		return 0, false
	}
	return int(math.Round(float64(t.done) * 100 / float64(den))), true
}

// phaseOf is the phase of an entity's status, "" when it has no flow.
func phaseOf(w *kernel.World, e *kernel.Entity) string {
	p, _ := w.Value(e, "phase").(string)
	return p
}

// spaceStats are the totals of a space: the three phases and the share done, with subtle tones and upcoming hint.
func spaceStats(t tally, upcoming int) []wire.Stat {
	pct, pctTone, pctDetail := "—", "", ""
	if p, ok := t.percent(); ok {
		pct = fmt.Sprintf("%d%%", p)
		if p >= 80 {
			pctTone = wire.TonePositive
		}
	}
	if upcoming > 0 {
		pctDetail = fmt.Sprintf("近期提醒 %d", upcoming)
	}
	doingTone := ""
	if t.doing > 0 {
		doingTone = wire.ToneCaution
	}
	return []wire.Stat{
		{Label: "待办", Value: strconv.Itoa(t.todo)},
		{Label: "进行中", Value: strconv.Itoa(t.doing), Tone: doingTone},
		{Label: "已完成", Value: strconv.Itoa(t.done)},
		{Label: "完成度", Value: pct, Detail: pctDetail, Tone: pctTone},
	}
}

// entitiesOf are the entities of the space shown with the type key.
func (c *ctx) entitiesOf(key string) []*kernel.Entity {
	var out []*kernel.Entity
	for _, e := range c.sp.Entities {
		if e != nil && e.Type == key {
			out = append(out, e)
		}
	}
	return out
}

// latest is the entity changed last; nil for none.
func latest(set []*kernel.Entity) *kernel.Entity {
	var best *kernel.Entity
	for _, e := range set {
		if e != nil && (best == nil || e.Updated.After(best.Updated)) {
			best = e
		}
	}
	return best
}

// typeIn is the type of the space with the key; nil when there is none.
func typeIn(sp *kernel.Space, key string) *kernel.Type {
	for _, t := range sp.Types {
		if t != nil && t.Key == key {
			return t
		}
	}
	return nil
}

// statGrid is a row of stats.
func statGrid(stats []wire.Stat) wire.Component {
	return wire.Component{Type: wire.ComponentStatGrid, Stats: stats}
}

// button is a button with a role.
func button(title string, a *wire.Action, role string) wire.Component {
	return wire.Component{Type: wire.ComponentButton, Title: title, Action: a, Role: role}
}
