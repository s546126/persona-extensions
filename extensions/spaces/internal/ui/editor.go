package ui

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

// The schema editor is the page of one type's structure, pushed from 结构. Every change builds a whole kernel.Type from a
// copy of the type shown and saves it with PutType, the path the type_write tool takes: the kernel checks it and
// migrates the entities, and a refused change saves nothing and shows its sentence as a toast. The editor has no
// validation of its own; it reads what the form sent and nothing else.

// newSlot is the argument that adds an item to a list: a new stage, view or property kind, or a new type ("+" as D).
const newSlot = "+"

// phaseOrder lists the phases in the order the choices offer them.
var phaseOrder = []kernel.Phase{kernel.Todo, kernel.Doing, kernel.Done, kernel.Dropped}

// phaseTitles name the phases in the editor.
var phaseTitles = map[kernel.Phase]string{
	kernel.Todo: "待办", kernel.Doing: "进行中", kernel.Done: "已完成", kernel.Dropped: "已放弃",
}

// viewOrder lists the view kinds in the order the choices offer them.
var viewOrder = []string{"list", "board", "table", "progress", "feed"}

// builtinKeys are the built-in fields of every type, in the order the field choices offer them.
var builtinKeys = []string{"title", "status", "phase", "type", "origin", "created", "updated"}

// optEntry is one option input of a property: the fields that show it, and the setter for the value the form sent.
type optEntry struct {
	name  string
	field func(c *ctx, t *kernel.Type, p kernel.Prop) []wire.Component
	set   func(p *kernel.Prop, raw string)
}

// optInput is the table of the option inputs of a property, in the order they are shown. A kind shows the inputs of
// its Opts only, and the form sends them as "o_" and the name.
var optInput = []optEntry{
	{"unit", unitInput, setUnit},
	{"options", optionsInput, setChoices},
	{"target", targetInput, setTarget},
	{"many", manyInput, setMany},
	{"back", backInput, setBack},
	{"via", viaInput, setVia},
	{"op", opInput, setOp},
	{"of", ofInput, setOf},
}

// viaPath is one path a rollup may count along: an own relation of the type, or the backlinks of a relation that
// points at it. related is the type the path reaches.
type viaPath struct{ value, title, related string }

// editorPage is the schema editor of the type in c.st.D: a page of rows, collapsed by default, each row opening the
// fold of one part of the structure. D "+" is the form of a new type.
func (c *ctx) editorPage() (*wire.View, error) {
	if c.st.D == newSlot {
		return newTypePage(), nil
	}
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	return &wire.View{Title: cut(t.Name, maxTitle), Sections: []wire.Section{
		{Items: c.typeHead(t)},
		c.flowSection(t),
		c.propsSection(t),
		c.cardSection(t),
		{Items: []wire.Component{c.typeDelete(t)}},
	}}, nil
}

// newTypePage is the form of a new type: its name and an optional key (auto-generated when left blank), and create.
func newTypePage() *wire.View {
	return &wire.View{Title: "新建分类", Sections: []wire.Section{{Items: []wire.Component{
		{Type: wire.ComponentHeader, Title: "新建分类", Subtitle: "分类就是你要记的一种东西，例如「电影」「药品」「任务」"},
		caption("建好以后，可以再给它加要填的内容（例如 导演、评分）和几个状态（例如 想看、看过）。"),
		textField("t_name", "名称", "", "例如 电影"),
		textField("t_key", "代号（可留空）", "", "留空会自动生成；或填小写英文字母开头的短名字，例如 film"),
		button("创建", &wire.Action{ID: "typenew", Submit: true}, wire.ButtonRoleNormal),
	}}}}
}

// typeHead is the first section: the header with the type's name and record count, and the row that folds the name
// and the icon.
func (c *ctx) typeHead(t *kernel.Type) []wire.Component {
	items := []wire.Component{
		{Type: wire.ComponentHeader, Title: cut(t.Name, maxTitle), Subtitle: fmt.Sprintf("分类设置 · 现在有 %d 条", len(c.entitiesOf(t.Key)))},
		caption("在这里决定这一类要填哪些内容、有哪些状态、在列表里怎么显示。改动会马上用到已有的每一条上。"),
		{Type: wire.ComponentRow, Title: "名称与图标", Subtitle: "代号 " + t.Key, Action: &wire.Action{ID: "fold:name"}},
	}
	if c.st.F == "name" {
		items = append(items,
			textField("t_name", "名称", t.Name, "1 到 40 个字"),
			optionalChoice("t_icon", "图标", iconChoices(t.Icon), t.Icon),
			button("保存", &wire.Action{ID: "typeset", Submit: true}, wire.ButtonRoleNormal),
		)
	}
	return items
}

// flowSection is the lifecycle: a row per stage that folds it, and the row that adds one. A type with no stage adds
// two at once, since a lifecycle has two stages at least.
func (c *ctx) flowSection(t *kernel.Type) wire.Section {
	items := []wire.Component{}
	for i, st := range t.Flow {
		id := strconv.Itoa(i)
		items = append(items, wire.Component{
			Type: wire.ComponentRow, Title: cut(st.Name, maxTitle),
			Badge:  &wire.Badge{Text: phaseTitles[st.Phase], Tone: phaseTone(st.Phase)},
			Action: &wire.Action{ID: "fold:stage:" + id},
		})
		if c.st.F == "stage:"+id {
			items = append(items, c.stageFields(t, i)...)
		}
	}
	items = append(items, wire.Component{Type: wire.ComponentRow, Title: "添加状态", Action: &wire.Action{ID: "fold:stage:" + newSlot}})
	if c.st.F == "stage:"+newSlot {
		items = append(items, c.stageFields(t, -1)...)
	}
	footer := "每一条按顺序从一个状态挪到下一个，例如 想读 → 在读 → 读完。"
	if len(t.Flow) == 0 {
		footer = "还没有状态，只用来记东西也可以。想跟进度，就加几个状态，例如 想读 → 在读 → 读完（至少两个）。"
	}
	return wire.Section{Title: "状态", Footer: footer, Items: items}
}

// stageFields is the fold of a stage: its name, its phase, and the buttons that save, move up and delete it. For a new
// stage on a type without a lifecycle, one-tap presets are offered above the custom inputs.
func (c *ctx) stageFields(t *kernel.Type, i int) []wire.Component {
	if i < 0 {
		var items []wire.Component
		if len(t.Flow) == 0 {
			items = append(items,
				button("套用：待办 → 进行中 → 已完成", &wire.Action{ID: "stagepreset:todo3"}, wire.ButtonRoleNormal),
				button("套用：想做 → 在做 → 已完成 → 搁置", &wire.Action{ID: "stagepreset:want4"}, wire.ButtonRoleNormal),
			)
		}
		items = append(items, textField("stage_name", "名称", "", "例如 想读"), phaseField("stage_phase", kernel.Todo))
		if len(t.Flow) == 0 {
			items = append(items, textField("stage2_name", "第二个状态", "", "例如 读完"), phaseField("stage2_phase", kernel.Done))
		}
		return append(items, button("添加", &wire.Action{ID: "stagesave:" + newSlot, Submit: true}, wire.ButtonRoleNormal))
	}
	st := t.Flow[i]
	ref := nameRef(i, st.Name)
	items := []wire.Component{
		textField("stage_name", "名称", st.Name, ""),
		phaseField("stage_phase", st.Phase),
		button("保存", &wire.Action{ID: "stagesave:" + ref, Submit: true}, wire.ButtonRoleNormal),
	}
	if i > 0 {
		items = append(items, button("上移", &wire.Action{ID: "stageup:" + ref}, wire.ButtonRoleNormal))
	}
	return append(items, button("删除", &wire.Action{ID: "stagedrop:" + ref,
		Confirm: fmt.Sprintf("删除状态「%s」？", cut(st.Name, maxTitle))}, wire.ButtonRoleDestructive))
}

// propsSection is the properties: a row per property that folds it, the row that adds one, and under it the eight
// kinds to choose from. The chosen kind's fold follows its row.
func (c *ctx) propsSection(t *kernel.Type) wire.Section {
	items := []wire.Component{}
	for i := range t.Props {
		p := t.Props[i]
		items = append(items, c.propRow(p))
		if c.st.F == "prop:"+p.Key {
			items = append(items, c.propFields(t, i)...)
		}
	}
	items = append(items, wire.Component{Type: wire.ComponentRow, Title: "添加填写项", Action: &wire.Action{ID: "fold:addprop"}})
	if c.st.F == "addprop" || strings.HasPrefix(c.st.F, "new:") {
		items = append(items, c.kindRows(t)...)
	}
	return wire.Section{Title: "填写项", Footer: "每一条除了标题，还要记哪些内容，例如 作者、页数、下次续费日期。", Items: items}
}

// propRow is a property: its kind's icon, its name, its summary from the descriptor, and its key as the value.
func (c *ctx) propRow(p kernel.Prop) wire.Component {
	row := wire.Component{
		Type: wire.ComponentRow, Title: cut(p.Name, maxTitle), Subtitle: c.propSummary(p), Value: p.Key,
		Action: &wire.Action{ID: "fold:prop:" + p.Key},
	}
	if k, ok := kernel.KindOf(p.Kind); ok {
		row.Icon = iconName(k.Icon)
	}
	return row
}

// kindRows are the descriptor rows, one per kind: icon, label and hint. Choosing one opens the property fold for that
// kind, which follows the row.
func (c *ctx) kindRows(t *kernel.Type) []wire.Component {
	var items []wire.Component
	for _, k := range kernel.Kinds {
		items = append(items, wire.Component{
			Type: wire.ComponentRow, Title: kindLabel(k), Subtitle: kindAbout(k), Icon: iconName(k.Icon),
			Action: &wire.Action{ID: "fold:new:" + k.Name},
		})
		if c.st.F == "new:"+k.Name {
			items = append(items, c.newPropFields(t, k)...)
		}
	}
	return items
}

// propFields is the fold of a property: its name, the inputs of its kind's options, and the buttons that save, move up
// and delete it.
func (c *ctx) propFields(t *kernel.Type, i int) []wire.Component {
	p := t.Props[i]
	items := []wire.Component{textField("p_name", "名称", p.Name, "")}
	items = append(items, c.optFields(t, p)...)
	items = append(items, button("保存", &wire.Action{ID: "propsave:" + p.Key, Submit: true}, wire.ButtonRoleNormal))
	if i > 0 {
		items = append(items, button("上移", &wire.Action{ID: "propup:" + p.Key}, wire.ButtonRoleNormal))
	}
	return append(items, button("删除", &wire.Action{ID: "propdrop:" + p.Key,
		Confirm: fmt.Sprintf("删除填写项「%s」？每一条里填过的这一项会一起删除，不能撤销。", cut(p.Name, maxTitle))}, wire.ButtonRoleDestructive))
}

// newPropFields is the fold that creates a property of a kind: its name, optional key (auto-generated when blank),
// the inputs of its options, and the button that adds it.
func (c *ctx) newPropFields(t *kernel.Type, k *kernel.Kind) []wire.Component {
	p := kernel.Prop{Kind: k.Name}
	items := []wire.Component{
		textField("p_name", "名称", "", "例如 作者"),
		textField("p_key", "代号（可留空）", "", "留空会自动生成；或填小写英文字母开头的短名字，例如 author"),
	}
	items = append(items, c.optFields(t, p)...)
	return append(items, button("添加", &wire.Action{ID: "propnew:" + k.Name, Submit: true}, wire.ButtonRoleNormal))
}

// optFields are the inputs of the options a kind reads, in the order of optInput.
func (c *ctx) optFields(t *kernel.Type, p kernel.Prop) []wire.Component {
	k, ok := kernel.KindOf(p.Kind)
	if !ok {
		return nil
	}
	var out []wire.Component
	for _, o := range optInput {
		if slices.Contains(k.Opts, o.name) {
			out = append(out, o.field(c, t, p)...)
		}
	}
	return out
}

// cardSection is the card and the views: the card's row that folds it, a row per view, and the row that adds one.
func (c *ctx) cardSection(t *kernel.Type) wire.Section {
	items := []wire.Component{{Type: wire.ComponentRow, Title: "列表里的样子", Subtitle: c.cardSummary(t), Action: &wire.Action{ID: "fold:card"}}}
	if c.st.F == "card" {
		items = append(items, c.cardFields(t)...)
	}
	for i, v := range t.Views {
		id := strconv.Itoa(i)
		items = append(items, wire.Component{
			Type: wire.ComponentRow, Title: cut(v.Name, maxTitle), Subtitle: c.viewSummary(t, v),
			Action: &wire.Action{ID: "fold:view:" + id},
		})
		if c.st.F == "view:"+id {
			items = append(items, c.viewFields(t, i, v)...)
		}
	}
	items = append(items, wire.Component{Type: wire.ComponentRow, Title: "添加显示方式", Action: &wire.Action{ID: "fold:view:" + newSlot}})
	if c.st.F == "view:"+newSlot {
		items = append(items, c.viewFields(t, -1, kernel.View{})...)
	}
	return wire.Section{Title: "怎么显示", Footer: "每种显示方式是分类页上的一个切换按钮，例如 列表、看板、表格。", Items: items}
}

// cardFields is the fold of the card: the lead, the pills as names, and the progress as a ratio of two fields.
func (c *ctx) cardFields(t *kernel.Type) []wire.Component {
	of, over := "", ""
	if t.Card.Progress != nil {
		of, over = t.Card.Progress.Of, t.Card.Progress.Over
	}
	choices := c.fieldChoices(t)
	return []wire.Component{
		optionalChoice("c_lead", "标题下面一行显示", choices, t.Card.Lead),
		textField("c_pills", "旁边显示", c.pillNames(t, t.Card.Pills), "写填写项的名称，用、分隔，例如 评分、页数"),
		optionalChoice("c_of", "进度条：已完成多少", choices, of),
		optionalChoice("c_over", "进度条：一共多少", choices, over),
		button("保存", &wire.Action{ID: "cardsave", Submit: true}, wire.ButtonRoleNormal),
	}
}

// viewOpChoices are the condition operators offered in the schema editor.
var viewOpChoices = []wire.Choice{
	{Value: "eq", Title: "等于"},
	{Value: "ne", Title: "不等于"},
	{Value: "has", Title: "包含"},
	{Value: "gt", Title: "大于"},
	{Value: "ge", Title: "大于等于"},
	{Value: "lt", Title: "小于"},
	{Value: "le", Title: "小于等于"},
	{Value: "empty", Title: "为空"},
}

// viewFields is the fold of a view (i < 0 for a new one): its name, kind, group, sort, primary filter and primary
// aggregate, plus a caption when extra conditions/aggregates exist, and save/delete buttons.
func (c *ctx) viewFields(t *kernel.Type, i int, v kernel.View) []wire.Component {
	sortBy, desc := "", false
	if len(v.Sort) > 0 {
		sortBy, desc = v.Sort[0].By, v.Sort[0].Desc
	}
	wField, wOp, wVal := "", "eq", ""
	if len(v.Where) > 0 {
		wField, wOp = v.Where[0].Field, v.Where[0].Op
		if v.Where[0].Value != nil {
			wVal = fmt.Sprint(v.Where[0].Value)
		}
	}
	aOp, aOf := "", ""
	if len(v.Agg) > 0 {
		aOp, aOf = v.Agg[0].Op, v.Agg[0].Of
	}
	choices := c.fieldChoices(t)
	numChoices := c.ownNumericChoices(t)
	items := []wire.Component{
		textField("v_name", "名称", v.Name, ""),
		requiredChoice("v_kind", "显示成", viewKindChoices(), textOr(v.Kind, viewOrder[0])),
		optionalChoice("v_group", "按什么分组", choices, v.Group),
		optionalChoice("v_sort", "按什么排序", choices, sortBy),
		toggleField("v_desc", "倒过来排（从大到小、从新到旧）", desc),
		optionalChoice("v_wfield", "筛选哪一项", choices, wField),
		requiredChoice("v_wop", "筛选方式", viewOpChoices, wOp),
		textField("v_wval", "筛选内容", wVal, "例如 在读、高；选「为空」时可留空"),
		optionalChoice("v_aop", "顶部统计", aggChoices(), aOp),
		optionalChoice("v_aof", "统计哪一项（合计、平均时用）", numChoices, aOf),
	}
	if len(v.Where) > 1 || len(v.Agg) > 1 {
		items = append(items, caption(fmt.Sprintf("这个显示方式另外还有 %d 个筛选条件、%d 项统计，会继续保留", max(0, len(v.Where)-1), max(0, len(v.Agg)-1))))
	}
	slot := newSlot
	if i >= 0 {
		slot = nameRef(i, v.Name)
	}
	items = append(items, button("保存", &wire.Action{ID: "viewsave:" + slot, Submit: true}, wire.ButtonRoleNormal))
	if i >= 0 {
		items = append(items, button("删除这个显示方式", &wire.Action{ID: "viewdrop:" + slot,
			Confirm: fmt.Sprintf("删除显示方式「%s」？条目本身不会被删。", cut(v.Name, maxTitle))}, wire.ButtonRoleDestructive))
	}
	return items
}

// ownNumericChoices returns numeric properties declared on t itself (for view aggregates).
func (c *ctx) ownNumericChoices(t *kernel.Type) []wire.Choice {
	var out []wire.Choice
	for _, p := range t.Props {
		if k, ok := kernel.KindOf(p.Kind); ok && k.Num != nil {
			out = append(out, wire.Choice{Value: p.Key, Title: p.Name})
		}
	}
	return out
}

// typeDelete is the button that deletes the type with its records; the confirmation says how many.
func (c *ctx) typeDelete(t *kernel.Type) wire.Component {
	return button("删除这个分类", &wire.Action{ID: "typedrop", Confirm: fmt.Sprintf("删除分类「%s」？里面的 %d 条会一起删除，不能撤销。",
		cut(t.Name, maxTitle), len(c.entitiesOf(t.Key)))}, wire.ButtonRoleDestructive)
}

// cardSummary is the card in words: its lead, its pills and its progress; "按默认" when none is set.
func (c *ctx) cardSummary(t *kernel.Type) string {
	var parts []string
	if t.Card.Lead != "" {
		parts = append(parts, "下面一行 "+c.fieldName(t, t.Card.Lead))
	}
	if len(t.Card.Pills) > 0 {
		parts = append(parts, "旁边 "+c.pillNames(t, t.Card.Pills))
	}
	if pr := t.Card.Progress; pr != nil {
		s := "进度条 " + c.fieldName(t, pr.Of)
		if pr.Over != "" {
			s += " / " + c.fieldName(t, pr.Over)
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "默认样子"
	}
	return strings.Join(parts, " · ")
}

// viewSummary is a view's kind and its group, as in 看板 · 按 状态 分组.
func (c *ctx) viewSummary(t *kernel.Type, v kernel.View) string {
	s := textOr(viewLabels[v.Kind], v.Kind)
	if v.Group != "" {
		s += " · 按 " + c.fieldName(t, v.Group) + " 分组"
	}
	return s
}

// propSummary is a property's kind in words: a relation's target, a rollup's aggregate and path, or a number's unit.
func (c *ctx) propSummary(p kernel.Prop) string {
	k, ok := kernel.KindOf(p.Kind)
	if !ok {
		return p.Kind
	}
	switch {
	case k.Input == kernel.InPick:
		s := "连到「" + c.typeName(p.Target) + "」"
		if p.Many {
			s += " · 可以连多条"
		}
		return s
	case k.Eval != nil:
		s := kindLabel(k) + " · " + aggLabel(p.Op)
		if p.Via != "" {
			s += " · " + c.viaTitle(p.Via)
		}
		return s
	case p.Unit != "":
		return kindLabel(k) + " · " + p.Unit
	}
	return kindLabel(k)
}

// fieldChoices are the fields of the type as choices: the built-in fields, then the declared properties.
func (c *ctx) fieldChoices(t *kernel.Type) []wire.Choice {
	var out []wire.Choice
	for _, p := range c.fieldsOf(t) {
		out = append(out, wire.Choice{Value: p.Key, Title: p.Name})
	}
	return out
}

// fieldsOf are the fields of the type: the built-in fields, then the declared properties.
func (c *ctx) fieldsOf(t *kernel.Type) []kernel.Prop {
	var out []kernel.Prop
	for _, key := range builtinKeys {
		if p, ok := c.fieldOf(t, key); ok {
			out = append(out, p)
		}
	}
	return append(out, t.Props...)
}

// fieldName is the name of a field of the type, or the key when there is none.
func (c *ctx) fieldName(t *kernel.Type, key string) string {
	if p, ok := c.fieldOf(t, key); ok {
		return p.Name
	}
	return key
}

// pillNames are the names of the pills' fields, separated by 、.
func (c *ctx) pillNames(t *kernel.Type, keys []string) string {
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		names = append(names, c.fieldName(t, key))
	}
	return strings.Join(names, "、")
}

// pillKeys are the keys of the fields named by a text separated by 、; a name that is no field of the type is refused.
func (c *ctx) pillKeys(t *kernel.Type, raw string) ([]string, error) {
	var out []string
	for _, name := range splitList(raw) {
		key := ""
		for _, p := range c.fieldsOf(t) {
			if p.Name == name {
				key = p.Key
				break
			}
		}
		if key == "" {
			return nil, fmt.Errorf("没有叫「%s」的填写项。请写这个分类里已有的名称，用、分隔", name)
		}
		out = append(out, key)
	}
	return out, nil
}

// typeChoices are the types of the space as choices.
func (c *ctx) typeChoices() []wire.Choice {
	var out []wire.Choice
	for _, t := range c.sp.Types {
		if t != nil {
			out = append(out, wire.Choice{Value: t.Key, Title: t.Name})
		}
	}
	return out
}

// typeName is the name of the type of the space with the key, or the key when there is none.
func (c *ctx) typeName(key string) string {
	if t := typeIn(c.sp, key); t != nil {
		return t.Name
	}
	return key
}

// viaPaths are the paths a rollup of the type may count along: each own relation, and each relation that points at the
// type (its backlinks, written type.prop).
func (c *ctx) viaPaths(t *kernel.Type) []viaPath {
	var out []viaPath
	for _, p := range t.Props {
		if k, ok := kernel.KindOf(p.Kind); ok && k.Input == kernel.InPick {
			out = append(out, viaPath{value: p.Key, title: p.Name, related: p.Target})
		}
	}
	for _, st := range c.sp.Types {
		if st == nil {
			continue
		}
		for _, p := range st.Props {
			if k, ok := kernel.KindOf(p.Kind); ok && k.Input == kernel.InPick && p.Target == t.Key {
				out = append(out, viaPath{value: st.Key + "." + p.Key, title: st.Name + " · " + p.Name, related: st.Key})
			}
		}
	}
	return out
}

// viaTitle names a rollup's path as the via choice does: a backlink "type.prop" as the type and the relation, an own
// relation of the type in the editor by its name; the path itself when it names nothing left.
func (c *ctx) viaTitle(via string) string {
	if tk, pk, ok := strings.Cut(via, "."); ok {
		if st := typeIn(c.sp, tk); st != nil {
			if p, ok := propOf(st, pk); ok {
				return st.Name + " · " + p.Name
			}
		}
		return via
	}
	if t := c.typeOf(c.st.D); t != nil {
		if p, ok := propOf(t, via); ok {
			return p.Name
		}
	}
	return via
}

// numericChoices are the stored numeric properties of the types the paths of the type reach, each key once.
func (c *ctx) numericChoices(t *kernel.Type) []wire.Choice {
	var out []wire.Choice
	seen := map[string]bool{}
	for _, pa := range c.viaPaths(t) {
		rt := typeIn(c.sp, pa.related)
		if rt == nil {
			continue
		}
		for _, p := range rt.Props {
			if k, ok := kernel.KindOf(p.Kind); ok && k.Num != nil && k.Eval == nil && !seen[p.Key] {
				seen[p.Key] = true
				out = append(out, wire.Choice{Value: p.Key, Title: p.Name})
			}
		}
	}
	return out
}

// aggChoices are the aggregates as choices, in vocabulary order.
func aggChoices() []wire.Choice {
	out := make([]wire.Choice, 0, len(kernel.Aggregates))
	for _, a := range kernel.Aggregates {
		out = append(out, wire.Choice{Value: a.Name, Title: a.Label})
	}
	return out
}

// aggLabel is the label of an aggregate, or its name when it has none.
func aggLabel(name string) string {
	for _, a := range kernel.Aggregates {
		if a.Name == name {
			return a.Label
		}
	}
	return name
}

// viewKindChoices are the view kinds as choices.
func viewKindChoices() []wire.Choice {
	out := make([]wire.Choice, 0, len(viewOrder))
	for _, k := range viewOrder {
		out = append(out, wire.Choice{Value: k, Title: viewLabels[k]})
	}
	return out
}

// iconChoices are the icons the apps draw, as choices, and the current icon of a type when the apps do not draw it: the
// icon is an opaque name in the base, and a save of the name must keep it.
func iconChoices(current string) []wire.Choice {
	out := make([]wire.Choice, 0, len(wire.NavIcons)+1)
	for _, name := range wire.NavIcons {
		out = append(out, wire.Choice{Value: name, Title: iconTitle(name)})
	}
	if current != "" && !slices.Contains(wire.NavIcons, current) {
		out = append(out, wire.Choice{Value: current, Title: current})
	}
	return out
}

// phaseChoices are the four phases as choices.
func phaseChoices() []wire.Choice {
	out := make([]wire.Choice, 0, len(phaseOrder))
	for _, p := range phaseOrder {
		out = append(out, wire.Choice{Value: string(p), Title: phaseTitles[p]})
	}
	return out
}

// phaseTone tones a phase: a doing stage is a caution, a done one positive, the rest neutral.
func phaseTone(p kernel.Phase) string {
	switch p {
	case kernel.Doing:
		return wire.ToneCaution
	case kernel.Done:
		return wire.TonePositive
	}
	return wire.ToneNeutral
}

// Inputs. A field is named as the form sends it; an optional choice sends "=" and its value, "=" alone is unset.

func textField(name, label, value, placeholder string) wire.Component {
	return wire.Component{Type: wire.ComponentField, Name: name, Label: label, Kind: wire.FieldKindText, Value: value, Placeholder: placeholder}
}

func toggleField(name, label string, on bool) wire.Component {
	return wire.Component{Type: wire.ComponentField, Name: name, Label: label, Kind: wire.FieldKindToggle, Value: strconv.FormatBool(on)}
}

// requiredChoice is a choice that always has a value; current is held when it is one of the options, else the first.
// No option at all is a caption: a choice field needs choices.
func requiredChoice(name, label string, options []wire.Choice, current string) wire.Component {
	if len(options) == 0 {
		return caption("「" + label + "」现在没有可以选的")
	}
	value := options[0].Value
	if slices.ContainsFunc(options, func(ch wire.Choice) bool { return ch.Value == current }) {
		value = current
	}
	return wire.Component{Type: wire.ComponentField, Name: name, Label: label, Kind: wire.FieldKindChoice, Value: value, Choices: options}
}

// optionalChoice is a choice that may be unset: its first choice is 未设置, sent as "=", and each option is sent as
// "=" and its value.
func optionalChoice(name, label string, options []wire.Choice, current string) wire.Component {
	choices := []wire.Choice{{Value: choicePrefix, Title: unsetLabel}}
	value := choicePrefix
	for _, ch := range options {
		choices = append(choices, wire.Choice{Value: choicePrefix + ch.Value, Title: textOr(ch.Title, ch.Value)})
		if current != "" && ch.Value == current {
			value = choicePrefix + current
		}
	}
	return wire.Component{Type: wire.ComponentField, Name: name, Label: label, Kind: wire.FieldKindChoice, Value: value, Choices: choices}
}

// phaseField is the choice of a stage's phase.
func phaseField(name string, p kernel.Phase) wire.Component {
	return requiredChoice(name, "算作", phaseChoices(), string(p))
}

// caption is a small text that says something about the fold it is in.
func caption(text string) wire.Component {
	return wire.Component{Type: wire.ComponentText, Text: text, Style: wire.TextStyleCaption}
}

// The option inputs of optInput. Each one shows its field and reads its value back.

func unitInput(_ *ctx, _ *kernel.Type, p kernel.Prop) []wire.Component {
	return []wire.Component{textField("o_unit", "单位", p.Unit, "例如 页")}
}

func setUnit(p *kernel.Prop, raw string) { p.Unit = strings.TrimSpace(raw) }

// optionsInput is the options as one text separated by 、. Options that would not fit a text of a screen are not
// shown; their field is then not sent, so they are kept as they are.
func optionsInput(_ *ctx, _ *kernel.Type, p kernel.Prop) []wire.Component {
	text := strings.Join(p.Options, "、")
	if utf8.RuneCountInString(text) > maxText {
		return []wire.Component{caption("选项太多，这里放不下，可以让助手改")}
	}
	return []wire.Component{textField("o_options", "选项", text, "用、分隔，例如 ★、★★、★★★")}
}

func setChoices(p *kernel.Prop, raw string) { p.Options = splitList(raw) }

func targetInput(c *ctx, _ *kernel.Type, p kernel.Prop) []wire.Component {
	return []wire.Component{requiredChoice("o_target", "连到哪个分类", c.typeChoices(), p.Target)}
}

func setTarget(p *kernel.Prop, raw string) { p.Target = raw }

func manyInput(_ *ctx, _ *kernel.Type, p kernel.Prop) []wire.Component {
	return []wire.Component{toggleField("o_many", "可以连多条", p.Many)}
}

func setMany(p *kernel.Prop, raw string) { p.Many = raw == "true" }

func backInput(_ *ctx, _ *kernel.Type, p kernel.Prop) []wire.Component {
	return []wire.Component{textField("o_back", "在对方那边叫什么", p.Back, "例如 笔记")}
}

func setBack(p *kernel.Prop, raw string) { p.Back = strings.TrimSpace(raw) }

func viaInput(c *ctx, t *kernel.Type, p kernel.Prop) []wire.Component {
	paths := c.viaPaths(t)
	if len(paths) == 0 {
		return []wire.Component{caption("还没有可以统计的：要先有「连到别的条目」的填写项（例如 笔记出自哪本书），才能统计连着的条目。")}
	}
	choices := make([]wire.Choice, 0, len(paths))
	for _, pa := range paths {
		choices = append(choices, wire.Choice{Value: pa.value, Title: pa.title})
	}
	return []wire.Component{requiredChoice("o_via", "统计哪些连着的条目", choices, p.Via)}
}

func setVia(p *kernel.Prop, raw string) { p.Via = raw }

func opInput(_ *ctx, _ *kernel.Type, p kernel.Prop) []wire.Component {
	return []wire.Component{requiredChoice("o_op", "怎么算", aggChoices(), p.Op)}
}

func setOp(p *kernel.Prop, raw string) { p.Op = raw }

func ofInput(c *ctx, t *kernel.Type, p kernel.Prop) []wire.Component {
	return []wire.Component{optionalChoice("o_of", "算哪一项（求和、平均时用）", c.numericChoices(t), p.Of)}
}

func setOf(p *kernel.Prop, raw string) { p.Of = strings.TrimPrefix(raw, choicePrefix) }

// splitList splits a text the form sent, separated by 、, dropping the blanks.
func splitList(raw string) []string {
	var out []string
	for _, s := range strings.Split(raw, "、") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// The actions of the editor. Each saves one whole type, built from a copy of the type shown.

// editedType is the type the editor shows.
func (c *ctx) editedType() (*kernel.Type, error) {
	if t := c.typeOf(c.st.D); t != nil {
		return t, nil
	}
	return nil, errors.New("找不到这个分类，它可能刚被删掉了。请返回再看")
}

// putType saves a whole type: PutType checks it and migrates its records, and a refused change leaves the space as it was.
func (c *ctx) putType(nt kernel.Type) error {
	return c.sp.PutType(nt, c.now)
}

// sentOr is the value the form sent under name, or old when the form did not send it.
func (c *ctx) sentOr(name, old string) string {
	if v, ok := c.r.Values[name]; ok {
		return v
	}
	return old
}

// choiceOr is the value of an optional choice the form sent under name ("=" alone is unset), or old when not sent.
func (c *ctx) choiceOr(name, old string) string {
	if v, ok := c.r.Values[name]; ok {
		return strings.TrimPrefix(v, choicePrefix)
	}
	return old
}

// cloneType copies a type so that the copy can change without touching the type it came from.
func cloneType(t *kernel.Type) kernel.Type {
	nt := *t
	nt.Flow = slices.Clone(t.Flow)
	nt.Props = slices.Clone(t.Props)
	nt.Views = slices.Clone(t.Views)
	nt.Card.Pills = slices.Clone(t.Card.Pills)
	if t.Card.Progress != nil {
		pr := *t.Card.Progress
		nt.Card.Progress = &pr
	}
	return nt
}

// nameRef is the argument of an action that names an existing stage or view: its index and its name, "<index>:<name>".
// The index alone would name another item once the list has changed under a fold that is still open.
func nameRef(i int, name string) string {
	return strconv.Itoa(i) + ":" + name
}

// slotOf is the item an action names among the names of the list; newSlot is -1, a new item.
func slotOf(arg string, names []string, what string) (int, error) {
	if arg == newSlot {
		return -1, nil
	}
	return nameAt(arg, names, what)
}

// nameAt is the index an action names among the names of a list, as nameRef writes it. The item at that index must
// still have the name the action carries; otherwise the action is refused, so that a fold drawn before a change never
// changes another item. Anything else is refused too.
func nameAt(arg string, names []string, what string) (int, error) {
	idx, name, _ := strings.Cut(arg, ":")
	i, err := strconv.Atoi(idx)
	if err != nil || i < 0 || i >= len(names) {
		return 0, errors.New("找不到这个" + what + "，它可能刚被删掉了。请重新打开再试")
	}
	if names[i] != name {
		return 0, errors.New("这个" + what + "刚被改过了。请重新打开再试")
	}
	return i, nil
}

// stageNames are the names of the stages of a type, in flow order.
func stageNames(t *kernel.Type) []string {
	out := make([]string, 0, len(t.Flow))
	for _, s := range t.Flow {
		out = append(out, s.Name)
	}
	return out
}

// viewNames are the names of the views of a type, in order.
func viewNames(t *kernel.Type) []string {
	out := make([]string, 0, len(t.Views))
	for _, v := range t.Views {
		out = append(out, v.Name)
	}
	return out
}

// propAt is the index of the property with the key, or -1.
func propAt(t *kernel.Type, key string) int {
	return slices.IndexFunc(t.Props, func(p kernel.Prop) bool { return p.Key == key })
}

// applyProp is p with the name and the options the form sent. An option the form did not send is kept, and an option
// the kind does not read is never set.
func (c *ctx) applyProp(p kernel.Prop, k *kernel.Kind) kernel.Prop {
	p.Name = strings.TrimSpace(c.sentOr("p_name", p.Name))
	for _, o := range optInput {
		if raw, sent := c.r.Values["o_"+o.name]; sent && slices.Contains(k.Opts, o.name) {
			o.set(&p, raw)
		}
	}
	return p
}

// stageFrom is the stage the form sent under prefix, with old as the value of what it did not send.
func (c *ctx) stageFrom(prefix string, old kernel.Stage) kernel.Stage {
	return kernel.Stage{
		Name:  strings.TrimSpace(c.sentOr(prefix+"_name", old.Name)),
		Phase: kernel.Phase(c.sentOr(prefix+"_phase", string(old.Phase))),
	}
}

// commonKeyWords maps everyday Chinese type/property names to clean English keys when the user leaves 代号 blank.
var commonKeyWords = map[string]string{
	"电影": "film", "影片": "film", "作品": "work", "书": "book", "书籍": "book",
	"笔记": "note", "观后感": "review", "任务": "task", "目标": "goal", "点子": "idea",
	"想法": "idea", "清单": "list", "物品": "item", "位置": "place", "预算": "budget",
	"账单": "expense", "支出": "expense", "收入": "income", "习惯": "habit", "打卡": "checkin",
	"旅行": "trip", "日程": "plan", "行李": "pack", "订阅": "sub", "扣款": "payment",
	"作者": "author", "导演": "director", "评分": "rating", "价格": "price", "单价": "price",
	"金额": "amount", "日期": "date", "截止": "due", "备注": "note", "简介": "summary",
	"说明": "about", "类别": "category", "分类": "category", "地点": "place", "链接": "url",
	"网址": "url", "主页": "url", "页数": "pages", "总页数": "pages", "已读": "read",
	"时长": "minutes", "数量": "qty", "优先级": "priority", "来源": "source", "标签": "tag",
	"标签页": "tag", "已装": "packed", "完成": "done", "周期": "cycle", "方式": "way",
	"城市": "city", "生日": "birthday", "药品": "medicine", "菜谱": "recipe", "课程": "course",
}

// autoKey derives a valid, unique kernel key from a display name when the user leaves 代号 empty.
func autoKey(name, prefix string, taken func(string) bool) string {
	trimmed := strings.TrimSpace(name)
	base := commonKeyWords[trimmed]
	if base == "" {
		var b strings.Builder
		for _, r := range strings.ToLower(trimmed) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
				b.WriteRune(r)
			} else if r == ' ' || r == '-' {
				b.WriteByte('_')
			}
		}
		cand := strings.Trim(b.String(), "_")
		if len(cand) > 0 && cand[0] >= 'a' && cand[0] <= 'z' {
			if len(cand) > 24 {
				cand = cand[:24]
			}
			base = cand
		}
	}
	if base != "" && !taken(base) {
		return base
	}
	if base == "" {
		base = prefix
	}
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s%d", base, i)
		if !taken(cand) {
			return cand
		}
	}
}

// actTypeNew creates a type from the form of a new type, and turns the page into its editor. Leaving 代号 blank
// generates a clean unique key automatically. A key that is taken is refused: PutType would replace that type instead.
func actTypeNew(c *ctx, _ string) (*wire.ActionResult, error) {
	if c.sp == nil {
		return nil, errors.New("还没有空间。请先从模板新建一个空间")
	}
	name := strings.TrimSpace(c.sentOr("t_name", ""))
	key := strings.TrimSpace(c.sentOr("t_key", ""))
	if key == "" && name != "" {
		key = autoKey(name, "t", func(k string) bool { return typeIn(c.sp, k) != nil })
	}
	nt := kernel.Type{Key: key, Name: name}
	if typeIn(c.sp, nt.Key) != nil {
		return nil, fmt.Errorf("已经有代号是「%s」的分类了，请换一个代号", nt.Key)
	}
	if err := c.putType(nt); err != nil {
		return nil, err
	}
	c.st = state{S: c.sp.ID, D: nt.Key}
	return &wire.ActionResult{Toast: "已新建「" + nt.Name + "」"}, nil
}

// actTypeSet changes the name and the icon of the type shown.
func actTypeSet(c *ctx, _ string) (*wire.ActionResult, error) {
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	nt := cloneType(t)
	nt.Name = strings.TrimSpace(c.sentOr("t_name", t.Name))
	nt.Icon = c.choiceOr("t_icon", t.Icon)
	if err := c.putType(nt); err != nil {
		return nil, err
	}
	c.st.F = ""
	return &wire.ActionResult{Toast: "已保存"}, nil
}

// actTypeDrop deletes the type shown with its records, and leaves its editor.
func actTypeDrop(c *ctx, _ string) (*wire.ActionResult, error) {
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	name := cut(t.Name, maxTitle)
	if err := c.sp.DropType(t.Key); err != nil {
		return nil, err
	}
	c.st = state{S: c.sp.ID}
	return &wire.ActionResult{Toast: "已删除「" + name + "」", Pop: true}, nil
}

// actStageSave changes a stage, or adds one at the end (newSlot). A type with no stage gets two at once: a lifecycle
// needs two stages, and the kernel refuses one.
func actStageSave(c *ctx, arg string) (*wire.ActionResult, error) {
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	i, err := slotOf(arg, stageNames(t), "状态")
	if err != nil {
		return nil, err
	}
	nt := cloneType(t)
	if i < 0 {
		nt.Flow = append(nt.Flow, c.stageFrom("stage", kernel.Stage{Phase: kernel.Todo}))
		if len(t.Flow) == 0 {
			nt.Flow = append(nt.Flow, c.stageFrom("stage2", kernel.Stage{Phase: kernel.Done}))
		}
	} else {
		nt.Flow[i] = c.stageFrom("stage", t.Flow[i])
	}
	if err := c.putType(nt); err != nil {
		return nil, err
	}
	c.st.F = ""
	return &wire.ActionResult{Toast: "已保存"}, nil
}

// actStageUp moves a stage one place earlier; its fold stays open.
func actStageUp(c *ctx, arg string) (*wire.ActionResult, error) {
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	i, err := nameAt(arg, stageNames(t), "状态")
	if err != nil {
		return nil, err
	}
	if i == 0 {
		return nil, errors.New("已经在最前面了")
	}
	nt := cloneType(t)
	nt.Flow[i-1], nt.Flow[i] = nt.Flow[i], nt.Flow[i-1]
	if err := c.putType(nt); err != nil {
		return nil, err
	}
	c.st.F = "stage:" + strconv.Itoa(i-1)
	return nil, nil
}

// actStageDrop deletes a stage. The kernel refuses it while a record is in it.
func actStageDrop(c *ctx, arg string) (*wire.ActionResult, error) {
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	i, err := nameAt(arg, stageNames(t), "状态")
	if err != nil {
		return nil, err
	}
	nt := cloneType(t)
	nt.Flow = slices.Delete(nt.Flow, i, i+1)
	if len(nt.Flow) == 0 {
		nt.Flow = nil
	}
	if err := c.putType(nt); err != nil {
		return nil, hinted(err, "先把这个状态里的条目挪到别的状态，再删除")
	}
	c.st.F = ""
	return &wire.ActionResult{Toast: "已删除状态"}, nil
}

// stagePresets are one-tap lifecycle presets offered on a type without a lifecycle.
var stagePresets = map[string][]kernel.Stage{
	"todo3": {
		{Name: "待办", Phase: kernel.Todo},
		{Name: "进行中", Phase: kernel.Doing},
		{Name: "已完成", Phase: kernel.Done},
	},
	"want4": {
		{Name: "想做", Phase: kernel.Todo},
		{Name: "在做", Phase: kernel.Doing},
		{Name: "已完成", Phase: kernel.Done},
		{Name: "搁置", Phase: kernel.Dropped},
	},
}

// actStagePreset applies a preset lifecycle to a type that currently has no stages.
func actStagePreset(c *ctx, arg string) (*wire.ActionResult, error) {
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	preset, ok := stagePresets[arg]
	if !ok {
		return nil, errors.New("找不到这组预设状态，请重新选择")
	}
	if len(t.Flow) > 0 {
		return nil, errors.New("这个分类已经有状态了，可以直接在上面修改")
	}
	nt := cloneType(t)
	nt.Flow = slices.Clone(preset)
	if err := c.putType(nt); err != nil {
		return nil, err
	}
	c.st.F = ""
	return &wire.ActionResult{Toast: "已套用预设状态"}, nil
}

// actPropNew adds a property of the kind named by the argument. Leaving 代号 blank generates a clean unique key
// automatically. A key the type already has is refused: PutType would replace that property.
func actPropNew(c *ctx, arg string) (*wire.ActionResult, error) {
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	k, ok := kernel.KindOf(arg)
	if !ok {
		return nil, fmt.Errorf("没有「%s」这种填写项。请从列表里重新选一种", arg)
	}
	name := strings.TrimSpace(c.sentOr("p_name", ""))
	key := strings.TrimSpace(c.sentOr("p_key", ""))
	if key == "" && name != "" {
		key = autoKey(name, "p", func(cand string) bool {
			return propAt(t, cand) >= 0 || slices.Contains(builtinKeys, cand)
		})
	}
	if propAt(t, key) >= 0 {
		return nil, fmt.Errorf("这个分类已经有代号是「%s」的填写项了，请换一个代号", key)
	}
	nt := cloneType(t)
	nt.Props = append(nt.Props, c.applyProp(kernel.Prop{Key: key, Kind: k.Name}, k))
	if err := c.putType(nt); err != nil {
		return nil, err
	}
	c.st.F = ""
	return &wire.ActionResult{Toast: "已添加「" + cut(textOr(name, key), maxTitle) + "」"}, nil
}

// actPropSave changes the name and the options of the property with the key. The key is the property's identity.
func actPropSave(c *ctx, key string) (*wire.ActionResult, error) {
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	i := propAt(t, key)
	if i < 0 {
		return nil, errors.New("找不到这个填写项，它可能刚被删掉了。请重新打开再试")
	}
	k, ok := kernel.KindOf(t.Props[i].Kind)
	if !ok {
		return nil, errors.New("这个填写项的种类认不出来，这里改不了。可以让助手看看")
	}
	nt := cloneType(t)
	nt.Props[i] = c.applyProp(t.Props[i], k)
	if err := c.putType(nt); err != nil {
		return nil, err
	}
	c.st.F = ""
	return &wire.ActionResult{Toast: "已保存"}, nil
}

// actPropUp moves a property one place earlier in the display order.
func actPropUp(c *ctx, key string) (*wire.ActionResult, error) {
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	i := propAt(t, key)
	if i < 0 {
		return nil, errors.New("找不到这个填写项，它可能刚被删掉了。请重新打开再试")
	}
	if i == 0 {
		return nil, errors.New("已经在最前面了")
	}
	nt := cloneType(t)
	nt.Props[i-1], nt.Props[i] = nt.Props[i], nt.Props[i-1]
	return nil, c.putType(nt)
}

// actPropDrop deletes the property with the key and its values. The kernel refuses it while a card, a view or a rollup
// still names it.
func actPropDrop(c *ctx, key string) (*wire.ActionResult, error) {
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	i := propAt(t, key)
	if i < 0 {
		return nil, errors.New("找不到这个填写项，它可能刚被删掉了。请重新打开再试")
	}
	nt := cloneType(t)
	nt.Props = slices.Delete(nt.Props, i, i+1)
	if len(nt.Props) == 0 {
		nt.Props = nil
	}
	if err := c.putType(nt); err != nil {
		if strings.Contains(err.Error(), "「"+key+"」") {
			return nil, fmt.Errorf("删不掉「%s」：它还用在「列表里的样子」、某个显示方式或统计里。先在那里去掉它，再删除", cut(t.Props[i].Name, maxTitle))
		}
		return nil, hinted(err, "它可能还用在「列表里的样子」、某个显示方式或统计里，先在那里去掉，再删除")
	}
	c.st.F = ""
	return &wire.ActionResult{Toast: "已删除填写项"}, nil
}

// actCardSave changes the lead, the pills and the progress of the card. A pill names a field of the type.
func actCardSave(c *ctx, _ string) (*wire.ActionResult, error) {
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	nt := cloneType(t)
	nt.Card.Lead = c.choiceOr("c_lead", t.Card.Lead)
	if raw, ok := c.r.Values["c_pills"]; ok {
		pills, err := c.pillKeys(t, raw)
		if err != nil {
			return nil, err
		}
		nt.Card.Pills = pills
	}
	of, over := "", ""
	if t.Card.Progress != nil {
		of, over = t.Card.Progress.Of, t.Card.Progress.Over
	}
	of, over = c.choiceOr("c_of", of), c.choiceOr("c_over", over)
	nt.Card.Progress = nil
	if of != "" || over != "" {
		nt.Card.Progress = &kernel.Ratio{Of: of, Over: over}
	}
	if err := c.putType(nt); err != nil {
		return nil, err
	}
	c.st.F = ""
	return &wire.ActionResult{Toast: "已保存"}, nil
}

// actViewSave changes a view, or adds one at the end (newSlot). Its filters and aggregates are kept as they are; the
// sort is its first key, and the keys after it are kept too.
func actViewSave(c *ctx, arg string) (*wire.ActionResult, error) {
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	i, err := slotOf(arg, viewNames(t), "显示方式")
	if err != nil {
		return nil, err
	}
	var old kernel.View
	if i >= 0 {
		old = t.Views[i]
	}
	sortBy, desc := "", false
	if len(old.Sort) > 0 {
		sortBy, desc = old.Sort[0].By, old.Sort[0].Desc
	}
	if v, ok := c.r.Values["v_desc"]; ok {
		desc = v == "true"
	}
	nv := old
	nv.Name = strings.TrimSpace(c.sentOr("v_name", old.Name))
	nv.Kind = c.sentOr("v_kind", old.Kind)
	nv.Group = c.choiceOr("v_group", old.Group)
	nv.Sort = sortWith(old.Sort, c.choiceOr("v_sort", sortBy), desc)
	if _, sent := c.r.Values["v_wfield"]; sent {
		wf := c.choiceOr("v_wfield", "")
		wop := strings.TrimSpace(c.sentOr("v_wop", "eq"))
		wval := strings.TrimSpace(c.sentOr("v_wval", ""))
		nv.Where = whereWith(old.Where, wf, wop, wval)
	}
	if _, sent := c.r.Values["v_aop"]; sent {
		aop := c.choiceOr("v_aop", "")
		aof := c.choiceOr("v_aof", "")
		nv.Agg = aggWith(old.Agg, aop, aof)
	}
	nt := cloneType(t)
	if i < 0 {
		nt.Views = append(nt.Views, nv)
	} else {
		nt.Views[i] = nv
	}
	if err := c.putType(nt); err != nil {
		return nil, err
	}
	c.st.F = ""
	return &wire.ActionResult{Toast: "已保存"}, nil
}

// whereWith updates the primary filter condition of a view while keeping any additional conditions intact.
func whereWith(old []kernel.Cond, field, op, rawVal string) []kernel.Cond {
	var rest []kernel.Cond
	if len(old) > 1 {
		rest = old[1:]
	}
	if field == "" {
		return slices.Clone(rest)
	}
	cond := kernel.Cond{Field: field, Op: textOr(op, "eq")}
	if rawVal != "" {
		cond.Value = rawVal
	}
	return append([]kernel.Cond{cond}, rest...)
}

// aggWith updates the primary aggregate of a view while keeping any additional aggregates intact.
func aggWith(old []kernel.Agg, op, of string) []kernel.Agg {
	var rest []kernel.Agg
	if len(old) > 1 {
		rest = old[1:]
	}
	if op == "" {
		return slices.Clone(rest)
	}
	agg := kernel.Agg{Op: op}
	if op != "count" && op != "percent_done" {
		agg.Of = of
	}
	return append([]kernel.Agg{agg}, rest...)
}

// sortWith is the sort of a view with its first key replaced by by (none when by is empty); the keys after the first
// are kept.
func sortWith(old []kernel.Order, by string, desc bool) []kernel.Order {
	var rest []kernel.Order
	if len(old) > 1 {
		rest = old[1:]
	}
	if by == "" {
		return slices.Clone(rest)
	}
	return append([]kernel.Order{{By: by, Desc: desc}}, rest...)
}

// actViewDrop deletes a view of the type.
func actViewDrop(c *ctx, arg string) (*wire.ActionResult, error) {
	t, err := c.editedType()
	if err != nil {
		return nil, err
	}
	i, err := nameAt(arg, viewNames(t), "显示方式")
	if err != nil {
		return nil, err
	}
	nt := cloneType(t)
	nt.Views = slices.Delete(nt.Views, i, i+1)
	if len(nt.Views) == 0 {
		nt.Views = nil
	}
	if err := c.putType(nt); err != nil {
		return nil, err
	}
	c.st.F = ""
	return &wire.ActionResult{Toast: "已删除显示方式"}, nil
}
