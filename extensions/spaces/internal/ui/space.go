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
const cardRows = 5

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

// overview is the space page with no type tab: the space's header and its totals with the buttons that open the
// settings or start a new space, the settings fold right under them when it is open, a row per type ending with the
// button that adds one, and the latest changes. Each button sits in the card it acts on: a section of buttons alone
// is drawn as a card of its own, an empty frame around them.
func (c *ctx) overview() (*wire.View, error) {
	sp := c.sp
	filter, tabs := c.chrome()
	v := &wire.View{Sections: []wire.Section{}, Filter: filter, Tabs: tabs}
	head := wire.Section{Items: []wire.Component{
		c.spaceHeader(),
		statGrid(spaceStats(tallyOf(c.w, sp.Entities))),
		button("空间设置", &wire.Action{ID: "fold:space"}, wire.ButtonRoleNormal),
		button("新建空间", &wire.Action{ID: "new"}, wire.ButtonRoleNormal),
	}}
	v.Sections = append(v.Sections, head)
	if c.st.F == "space" {
		v.Sections = append(v.Sections, c.spaceFold())
	}
	if len(sp.Types) == 0 {
		// the empty state's own button adds the first category
		v.Sections = append(v.Sections, wire.Section{Items: []wire.Component{c.noTypes()}})
		return v, nil
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

// spaceHeader names the space, with its about and its facts: the number of types and entities and the latest change.
func (c *ctx) spaceHeader() wire.Component {
	sp := c.sp
	facts := []wire.Fact{
		{Label: "分类", Value: fmt.Sprintf("%d 个", len(sp.Types))},
		{Label: "条目", Value: fmt.Sprintf("%d 条", len(sp.Entities))},
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

// typeRows is a row per type: its icon and name, the kind of its default view with its count, and its value: the
// share done when it has a flow, else the count. A row opens that type's tab.
func (c *ctx) typeRows() []wire.Component {
	out := []wire.Component{}
	for _, t := range c.sp.Types {
		set := c.entitiesOf(t.Key)
		value := strconv.Itoa(len(set))
		if pct, ok := tallyOf(c.w, set).percent(); ok && len(t.Flow) > 0 {
			value = fmt.Sprintf("完成 %d%%", pct)
		}
		subtitle := strconv.Itoa(len(set)) + " 条 · 显示成" + viewLabels[t.ViewsOrDefault()[0].Kind]
		out = append(out, wire.Component{
			Type: wire.ComponentRow, Icon: iconName(t.Icon), Title: t.Name, Subtitle: subtitle, Value: value,
			Action: &wire.Action{ID: "tab:" + t.Key},
		})
	}
	return out
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
	return wire.Section{Items: []wire.Component{
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
	for _, sp := range c.b.Spaces {
		entries += len(sp.Entities)
		w := kernel.Open(sp)
		all.add(tallyOf(w, sp.Entities))
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
	v := &wire.View{Sections: []wire.Section{{Items: []wire.Component{statGrid([]wire.Stat{
		{Label: "进行中", Value: strconv.Itoa(all.doing), Detail: fmt.Sprintf("%d 个空间，共 %d 条", len(c.b.Spaces), entries)},
		{Label: "待办", Value: strconv.Itoa(all.todo)},
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

// spaceStats are the totals of a space: the three phases and the share done.
func spaceStats(t tally) []wire.Stat {
	pct := "—"
	if p, ok := t.percent(); ok {
		pct = fmt.Sprintf("%d%%", p)
	}
	return []wire.Stat{
		{Label: "待办", Value: strconv.Itoa(t.todo)},
		{Label: "进行中", Value: strconv.Itoa(t.doing)},
		{Label: "已完成", Value: strconv.Itoa(t.done)},
		{Label: "完成度", Value: pct},
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
