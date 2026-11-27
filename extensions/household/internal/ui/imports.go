package ui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/household/internal/imports"
	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
)

// The import part of the page: the drafts waiting for the user's yes (made by the assistant from a receipt, a photo or
// a text, or by 粘贴清单 on this page), the result of the last import with 撤销这次导入, and the ways to add things (addSection).
//
// A yes is always for what the user saw: the buttons carry the revision of the plan they were drawn for, and an
// answer names its choice, so a plan the assistant changed in between is refused and drawn again. 确认写入 is only
// offered where every line of the draft is listed; a longer draft opens its full list first.

// Limits of the import part, so that the overview stays within a page's budget whatever is stored: at most
// draftsShown drafts with at most stepsShown lines each on the overview; the full list of a draft has every line.
const (
	draftsShown = 3
	stepsShown  = 8
	pasteForm   = "paste"  // the form state of 粘贴清单
	draftForm   = "draft:" // the full list of a draft: draft:<id>
	answerForm  = "ans:"   // the answers to one line: ans:<id>:<rev>:<line>
	pasteMost   = 4000     // runes the paste field takes
)

// UseImports makes imp the store of the import drafts; the assistant's tools share it. It returns the app.
func (a *App) UseImports(imp *imports.Store) *App {
	a.imp = imp
	return a
}

// importsOf is the app's draft store.
func (a *App) importsOf() *imports.Store {
	if a.imp == nil {
		a.imp = imports.Open("")
	}
	return a.imp
}

// loadImports reads the drafts and the imports applied for the request, settled against the records (see
// imports.State.Settle). A store that cannot be read is kept as the error, which the page says.
func (c *ctx) loadImports() {
	c.imps, c.impErr = &imports.State{}, nil
	err := c.app.importsOf().View(func(st *imports.State) error {
		cp := *st
		cp.Drafts = append([]imports.Draft(nil), st.Drafts...)
		cp.Applied = append([]imports.Applied(nil), st.Applied...)
		cp.Settle(c.b)
		c.imps = &cp
		return nil
	})
	if err != nil {
		c.impErr = err
	}
}

// The three ways to add things, each a row saying what it is in one sentence, with its button right under it: one
// item by its form, a pasted list the page reads itself (no assistant, no model), and the assistant, which a page
// cannot open or write to: the user sends the picture or the text in the Persona conversation.
const (
	addOneWords    = "一样一样填：名字、数量、放在哪、什么时候过期。"
	pasteWords     = "每行一样，例如「牛奶 2 盒」，由家庭管家自己读、不经过助手；读完先给你看一遍，你点「确认写入」才写进去。"
	assistantWords = "在 Persona 里把小票、冰箱或柜子的照片、网购订单截图或一段文字发给助手，说一句「把这张小票记到家庭管家」；" +
		"助手整理好会放到这页的「待确认的导入」里，你确认了才写进去。"
)

// wayRow is one way to add things: its name and the one sentence on it.
func wayRow(title, words string) wire.Component {
	return wire.Component{Type: wire.ComponentRow, Title: title, Subtitle: words}
}

// pasteButton opens the 粘贴清单 form.
func pasteButton() wire.Component {
	return button("粘贴清单", ext.Open("paste", PageUI, state{F: pasteForm}, "粘贴清单"), wire.ButtonRoleNormal)
}

// addSection is 记新东西, the ways to add things: 记一样 with 添加物品, 自己粘贴一份清单 with 粘贴清单, and 让助手帮我记,
// which has no button.
func addSection() wire.Section {
	return wire.Section{Title: addTitle, Items: []wire.Component{
		wayRow("记一样", addOneWords),
		button("添加物品", addAction(model.TItem), wire.ButtonRoleNormal),
		wayRow("自己粘贴一份清单", pasteWords),
		pasteButton(),
		wayRow("让助手帮我记", assistantWords),
	}}
}

// addTitle heads the ways to add things.
const addTitle = "记新东西"

// importSections are the drafts waiting, newest first, then the last import when it was within a day. A store that
// cannot be read is said in plain words instead.
func (c *ctx) importSections() []wire.Section {
	if c.impErr != nil {
		return []wire.Section{{Title: "待确认的导入", Items: []wire.Component{quiet(cut(
			"导入草稿的文件现在读不了，所以这里先不显示（别的功能照常用）："+c.impErr.Error(), 400))}}}
	}
	var out []wire.Section
	drafts := c.imps.Drafts
	shown := 0
	for i := len(drafts) - 1; i >= 0 && shown < draftsShown; i-- {
		out = append(out, c.draftSection(drafts[i], shown == 0))
		shown++
	}
	if left := len(drafts) - shown; left > 0 {
		out[len(out)-1].Footer = join(out[len(out)-1].Footer, fmt.Sprintf("还有 %d 份更早的导入没有显示，先处理上面的", left))
	}
	if s, ok := c.lastImportSection(); ok {
		out = append(out, s)
	}
	return out
}

// draftSection is one draft on the overview: where it came from and when, its first lines saying what will happen,
// the totals, and its buttons. When it has more lines than shown, 确认写入 is on its full list, which a button opens.
func (c *ctx) draftSection(d imports.Draft, first bool) wire.Section {
	title := "待确认的导入"
	if !first {
		title = "另一份待确认的导入"
	}
	var items []wire.Component
	for _, s := range d.Plan.Steps {
		if len(items) < stepsShown {
			items = append(items, stepRow(d, s))
		}
	}
	for _, u := range d.Plan.Unread {
		if len(items) < stepsShown {
			items = append(items, unreadRow(u))
		}
	}
	all := len(d.Plan.Steps)+len(d.Plan.Unread) <= stepsShown
	items = append(items, quiet("合计："+imports.TotalsText(d.Plan.Totals)))
	if all {
		items = append(items, c.draftButtons(d)...)
	} else {
		n := len(d.Plan.Steps) + len(d.Plan.Unread)
		items = append(items, button(fmt.Sprintf("看全部 %d 行再确认", n),
			ext.Open("impopen:"+d.ID, PageUI, state{F: draftForm + d.ID}, "待确认的导入"), wire.ButtonRoleNormal),
			button("放弃", dropAction(d), wire.ButtonRoleNormal))
	}
	return wire.Section{Title: title, Footer: c.draftFooter(d), Items: items}
}

func (c *ctx) draftFooter(d imports.Draft) string {
	return join(imports.DraftLabel(d)+" · "+c.when(d.Created), cut(d.Input.Note, 60), "24 小时内没确认会自动作废")
}

// draftButtons are the end of a fully listed draft: 确认写入 for the revision drawn (or why it cannot be confirmed
// yet, with 重新算一遍 for a draft the records have overtaken), and 放弃.
func (c *ctx) draftButtons(d imports.Draft) []wire.Component {
	var items []wire.Component
	rev := strconv.Itoa(d.Rev)
	switch {
	case c.imps.Blocked(d.ID):
		items = append(items, quiet("上次写入这份时被打断，说不清写进去没有，所以不能再写一次。请看看库存对不对，然后放弃这份，需要的话重新导入。"))
	case c.draftStale(d):
		items = append(items, quiet("家里的记录在这之后变了，写进去会和上面说的不一样。先重新算一遍，看过再确认。"),
			button("重新算一遍", &wire.Action{ID: "impreplan:" + d.ID + ":" + rev}, wire.ButtonRoleNormal))
	case d.Plan.Totals.Questions > 0:
		items = append(items, quiet(fmt.Sprintf("还有 %d 行要你先定（点那一行回答），定好了才能写入。", d.Plan.Totals.Questions)))
	case d.Plan.Totals.Changes > 0:
		items = append(items, button("确认写入", &wire.Action{ID: "impapply:" + d.ID + ":" + rev,
			Confirm: imports.ConfirmText(d.Plan.Totals)}, wire.ButtonRoleNormal))
	}
	return append(items, button("放弃", dropAction(d), wire.ButtonRoleNormal))
}

func dropAction(d imports.Draft) *wire.Action {
	return &wire.Action{ID: "impdrop:" + d.ID, Confirm: "放弃这份导入？记录不会有任何改动。"}
}

// draftPage is the full list of a draft: every line and every line that could not be read, the totals and the buttons.
// nil when the draft is gone.
func (c *ctx) draftPage(id string) *wire.View {
	d, ok := c.imps.Draft(id)
	if !ok {
		return nil
	}
	var items []wire.Component
	for _, s := range d.Plan.Steps {
		items = append(items, stepRow(d, s))
	}
	for _, u := range d.Plan.Unread {
		items = append(items, unreadRow(u))
	}
	items = append(items, quiet("合计："+imports.TotalsText(d.Plan.Totals)))
	items = append(items, c.draftButtons(d)...)
	return &wire.View{Title: "待确认的导入", Sections: []wire.Section{{Footer: c.draftFooter(d), Items: items}}}
}

// draftStale reports whether the records changed since the draft was planned, so that writing it would now do
// anything other than what it says.
func (c *ctx) draftStale(d imports.Draft) bool {
	p, err := imports.Make(c.home, d.Input, d.Edits, c.today)
	if err != nil {
		return true
	}
	now, was := p.Sigs(), d.Plan.Sigs()
	if len(now) != len(was) {
		return true
	}
	for line, sig := range now {
		if was[line] != sig {
			return true
		}
	}
	return false
}

// stepRow is one line of a draft: its number and name, what will happen, and a badge for a question, a count that goes
// down, an inferred value, or a line that changes nothing. A question opens its answers.
func stepRow(d imports.Draft, s imports.Step) wire.Component {
	sub := s.Says
	if len(s.Notes) > 0 {
		sub += "。" + strings.Join(s.Notes, "；")
	}
	row := wire.Component{Type: wire.ComponentRow, Title: cut(fmt.Sprintf("%d. %s", s.Line, s.Title), maxTitle),
		Subtitle: cut(sub, 400)}
	switch {
	case s.Action == imports.ActAsk:
		row.Badge = &wire.Badge{Text: "要你定", Tone: wire.ToneCaution}
		f := fmt.Sprintf("%s%s:%d:%d", answerForm, d.ID, d.Rev, s.Line)
		row.Action = ext.Open("impask:"+d.ID, PageUI, state{F: f}, fmt.Sprintf("第 %d 行", s.Line))
	case !s.Writes():
		row.Badge = &wire.Badge{Text: "不改"}
	case s.Lowers:
		row.Badge = &wire.Badge{Text: "会变少", Tone: wire.ToneCaution}
	case len(s.Inferred) > 0:
		row.Badge = &wire.Badge{Text: "有推测", Tone: wire.ToneCaution}
	}
	return row
}

func unreadRow(u imports.Unread) wire.Component {
	return wire.Component{Type: wire.ComponentRow, Title: cut(u.Text, maxTitle), Subtitle: u.Why,
		Badge: &wire.Badge{Text: "没读懂"}}
}

// answerPage is the question of one line of a draft with a button per answer, for the revision it was opened for.
// nil when the draft is gone; a line that has no question any more says so.
func (c *ctx) answerPage(arg string) *wire.View {
	parts := strings.Split(arg, ":")
	if len(parts) != 3 {
		return nil
	}
	d, ok := c.imps.Draft(parts[0])
	if !ok {
		return nil
	}
	line, _ := strconv.Atoi(parts[2])
	v := &wire.View{Title: fmt.Sprintf("第 %d 行", line)}
	if parts[1] != strconv.Itoa(d.Rev) {
		v.Sections = []wire.Section{{Items: []wire.Component{quiet("预览已经变了，请回去看一遍新的。"),
			button("返回", &wire.Action{ID: "back"}, wire.ButtonRoleNormal)}}}
		return v
	}
	for _, s := range d.Plan.Steps {
		if s.Line != line || s.Ask == nil {
			continue
		}
		items := []wire.Component{{Type: wire.ComponentRow, Title: cut(s.Title, maxTitle), Subtitle: cut(s.Ask.Text, 400)}}
		for _, ch := range s.Ask.Choices {
			items = append(items, button(cut(ch.Label, 40),
				&wire.Action{ID: fmt.Sprintf("impans:%s:%d:%d:%s", d.ID, d.Rev, line, ch.Key())}, wire.ButtonRoleNormal))
		}
		items = append(items, quiet("要改数量或单位，可以跟助手说，或者放弃这份重新粘贴。"),
			button("返回", &wire.Action{ID: "back"}, wire.ButtonRoleNormal))
		v.Sections = []wire.Section{{Items: items}}
		return v
	}
	v.Sections = []wire.Section{{Items: []wire.Component{quiet("这一行已经不用回答了。"),
		button("返回", &wire.Action{ID: "back"}, wire.ButtonRoleNormal)}}}
	return v
}

// lastImportSection is 刚才的导入: what the last import wrote, with 撤销这次导入 while it can be undone; shown for a
// day after the import.
func (c *ctx) lastImportSection() (wire.Section, bool) {
	last, ok := c.imps.Last()
	if !ok || c.now.Sub(last.At) > 24*time.Hour {
		return wire.Section{}, false
	}
	items := []wire.Component{quiet(fmt.Sprintf("%s 写入了%s：%s", c.when(last.At), imports.SourceLabel(last.Source),
		imports.TotalsText(last.Totals)))}
	switch {
	case last.Unknown == imports.UnknownUndo && last.Seen:
		return wire.Section{}, false
	case last.Unknown == imports.UnknownUndo:
		items = append(items, quiet("上次撤销这次导入时被打断，说不清撤销了没有，所以不再提供撤销。请看看这几样的数量对不对，不对就手动改一下。"),
			button("知道了", &wire.Action{ID: "impack:" + last.Draft}, wire.ButtonRoleNormal))
	case !last.Undone.IsZero():
		items = append(items, quiet("已经撤销（"+c.when(last.Undone)+"）。"))
	default:
		if err := imports.CheckUndo(c.b, last); err != nil {
			items = append(items, quiet(cut("不能撤销了："+err.Error(), 400)))
		} else {
			items = append(items, button("撤销这次导入", &wire.Action{ID: "impundo:" + last.Draft,
				Confirm: "撤销这次导入？这次新加的记录会删掉，改过的数量和采购会恢复成导入前的样子。"}, wire.ButtonRoleNormal))
		}
	}
	return wire.Section{Title: "刚才的导入", Items: items}, true
}

// when is a time as the page says it: 今天 09:30, 昨天 18:05, or 11-24 18:05.
func (c *ctx) when(t time.Time) string {
	lt := t.In(c.loc)
	day := lt.Format("01-02")
	switch lt.Format(time.DateOnly) {
	case string(c.today):
		day = "今天"
	case string(c.today.Add(-1)):
		day = "昨天"
	}
	return day + " " + lt.Format("15:04")
}

// pastePage is the 粘贴清单 form: one thing per line, where it goes, and whether it adds to what is there.
func (c *ctx) pastePage() *wire.View {
	return &wire.View{Title: "粘贴清单", Sections: []wire.Section{{
		Items: []wire.Component{
			quiet("每行写一样东西：名称、数量和单位，可以带过期日，例如「牛奶 2 盒」「鸡蛋 12 个 2026-12-05」。" +
				"先给你看会怎么记，你确认了才写进去；看不懂的行会标出来，不会乱猜。"),
			{Type: wire.ComponentField, Name: "paste_text", Label: "清单", Kind: wire.FieldKindMultiline,
				Placeholder: "牛奶 2 盒\n鸡蛋 12 个 2026-12-05\n洗洁精 1 瓶"},
			{Type: wire.ComponentField, Name: "paste_target", Label: "记到哪", Kind: wire.FieldKindChoice,
				Value: choicePrefix + imports.ToStock, Choices: []wire.Choice{
					{Value: choicePrefix + imports.ToStock, Title: "库存（家里有的）"},
					{Value: choicePrefix + imports.ToShopping, Title: "采购清单（要买的）"},
				}},
			{Type: wire.ComponentField, Name: "paste_mode", Label: "数量怎么算", Kind: wire.FieldKindChoice,
				Value: choicePrefix + imports.ModeAdd, Choices: []wire.Choice{
					{Value: choicePrefix + imports.ModeAdd, Title: "刚买回来，加到原有的数量上"},
					{Value: choicePrefix + imports.ModeCount, Title: "数过了，家里现在就这么多（可能会变少）"},
				}},
			button("看看会怎么记", &wire.Action{ID: "imppaste", Submit: true}, wire.ButtonRoleNormal),
			button("取消", &wire.Action{ID: "back"}, wire.ButtonRoleNormal),
		},
	}}}
}

// actPasteOpen opens the 粘贴清单 form (for an app that calls the action instead of pushing).
func actPasteOpen(_ *ctx, _ string) (*wire.ActionResult, error) {
	return &wire.ActionResult{Push: pasteButton().Action.Push}, nil
}

// actOpenDraft opens the full list of a draft (for an app that calls the action instead of pushing).
func actOpenDraft(_ *ctx, id string) (*wire.ActionResult, error) {
	return &wire.ActionResult{Push: ext.Open("impopen:"+id, PageUI, state{F: draftForm + id}, "待确认的导入").Push}, nil
}

// actPaste reads the pasted list into a draft and goes back to the overview, where it waits for 确认写入.
func actPaste(c *ctx, _ string) (*wire.ActionResult, error) {
	text := c.r.Values["paste_text"]
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("先把要记的东西粘贴进来，一行一样")
	}
	text = cut(text, pasteMost)
	lines, unread, notes := imports.ParsePaste(text, c.today)
	if len(lines) == 0 {
		return nil, errors.New("一行都没读懂：每行写名称和数量，例如「牛奶 2 盒」")
	}
	target := strings.TrimPrefix(c.r.Values["paste_target"], choicePrefix)
	if target != imports.ToShopping {
		target = imports.ToStock
	}
	mode := strings.TrimPrefix(c.r.Values["paste_mode"], choicePrefix)
	if mode != imports.ModeCount {
		mode = imports.ModeAdd
	}
	in := imports.Input{Source: imports.SrcPaste, Target: target, Mode: mode, Lines: lines, Unread: unread,
		Note: cut(strings.Join(notes, "；"), imports.NoteMost)}
	if _, err := imports.Preview(c.app.importsOf(), c.home, in, c.loc, true); err != nil {
		return nil, err
	}
	c.st = state{}
	return &wire.ActionResult{Toast: fmt.Sprintf("读到 %d 样，请在「待确认的导入」里看一遍再确认", len(lines)), Pop: true}, nil
}

// idRev reads "<draft>:<revision>".
func idRev(arg string) (string, int, error) {
	id, r, ok := strings.Cut(arg, ":")
	rev, err := strconv.Atoi(r)
	if !ok || err != nil || rev < 1 {
		return "", 0, errors.New("这个按钮已经过时了，请看一遍新的预览")
	}
	return id, rev, nil
}

// actImportApply writes the revision of a draft the user saw and confirmed: <draft>:<revision>.
func actImportApply(c *ctx, arg string) (*wire.ActionResult, error) {
	id, rev, err := idRev(arg)
	if err != nil {
		return nil, err
	}
	done, finish, err := imports.Apply(c.b, c.app.importsOf(), id, rev, true, c.now, c.loc)
	if err != nil {
		return nil, err
	}
	c.onSaved(func(ok bool) { _ = finish(ok) })
	res := &wire.ActionResult{Toast: "已写入：" + imports.TotalsText(done.Totals)}
	if strings.HasPrefix(c.st.F, draftForm) {
		res.Pop = true
	}
	return res, nil
}

// actImportDrop drops a draft; nothing in the records changes.
func actImportDrop(c *ctx, id string) (*wire.ActionResult, error) {
	if _, err := imports.Discard(c.app.importsOf(), id, true); err != nil {
		return nil, err
	}
	res := &wire.ActionResult{Toast: "已放弃这份导入，记录没有改动"}
	if strings.HasPrefix(c.st.F, draftForm) {
		res.Pop = true
	}
	return res, nil
}

// actImportReplan plans a draft again against the records as they are now: <draft>:<revision>.
func actImportReplan(c *ctx, arg string) (*wire.ActionResult, error) {
	id, rev, err := idRev(arg)
	if err != nil {
		return nil, err
	}
	if _, err := imports.Replan(c.app.importsOf(), c.home, id, rev, nil, c.loc, true); err != nil {
		return nil, err
	}
	return &wire.ActionResult{Toast: "已按现在的记录重新算好，请再看一遍"}, nil
}

// actImportAnswer answers the question of a draft's line: <draft>:<revision>:<line>:<choice key>. The choice is found
// by what it does, in the revision the answer was drawn for; anything else is refused.
func actImportAnswer(c *ctx, arg string) (*wire.ActionResult, error) {
	parts := strings.SplitN(arg, ":", 4)
	if len(parts) != 4 {
		return nil, errors.New("这个回答看不懂，请刷新页面再试")
	}
	rev, err1 := strconv.Atoi(parts[1])
	line, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil {
		return nil, errors.New("这个回答看不懂，请刷新页面再试")
	}
	d, ok := c.imps.Draft(parts[0])
	if !ok {
		return nil, errors.New("这份导入已经不在了：可能已经写入、放弃，或者过了 24 小时")
	}
	if d.Rev != rev {
		return nil, imports.ErrChanged
	}
	var choice *imports.Choice
	for _, s := range d.Plan.Steps {
		if s.Line != line || s.Ask == nil {
			continue
		}
		for k := range s.Ask.Choices {
			if s.Ask.Choices[k].Key() == parts[3] {
				choice = &s.Ask.Choices[k]
			}
		}
	}
	if choice == nil {
		return nil, imports.ErrChanged
	}
	edit := imports.Edit{Line: line, Match: choice.Match, Qty: choice.Qty, Skip: choice.Skip}
	if _, err := imports.Replan(c.app.importsOf(), c.home, d.ID, rev, []imports.Edit{edit}, c.loc, true); err != nil {
		return nil, err
	}
	res := &wire.ActionResult{Toast: "已记下：" + choice.Label}
	if strings.HasPrefix(c.st.F, answerForm) {
		res.Pop = true
	}
	return res, nil
}

// actImportUndo undoes the last import.
func actImportUndo(c *ctx, id string) (*wire.ActionResult, error) {
	_, finish, err := imports.UndoLast(c.b, c.app.importsOf(), id, c.now)
	if err != nil {
		return nil, err
	}
	c.onSaved(func(ok bool) { _ = finish(ok) })
	return &wire.ActionResult{Toast: "已撤销这次导入"}, nil
}

// actImportAck hides an undo whose outcome could not be decided, once the user has seen it. No record changes.
func actImportAck(c *ctx, draft string) (*wire.ActionResult, error) {
	if err := imports.Acknowledge(c.app.importsOf(), draft); err != nil {
		return nil, err
	}
	return &wire.ActionResult{Toast: "好的，这条不再显示"}, nil
}
