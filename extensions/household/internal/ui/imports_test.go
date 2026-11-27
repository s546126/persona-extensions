package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/household/internal/imports"
	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

func fp(v float64) *float64 { return &v }

// preview stores a draft as the assistant's tool would.
func preview(t *testing.T, a *App, in imports.Input) imports.Draft {
	t.Helper()
	d, err := imports.Preview(a.importsOf(), home(t, a.st), in, testLoc(), false)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func buttonTitledIn(s wire.Section, title string) (wire.Component, bool) {
	for _, c := range s.Items {
		if c.Type == wire.ComponentButton && c.Title == title {
			return c, true
		}
	}
	return wire.Component{}, false
}

func rowTitled(s wire.Section, suffix string) (wire.Component, bool) {
	for _, c := range s.Items {
		if c.Type == wire.ComponentRow && strings.HasSuffix(c.Title, suffix) {
			return c, true
		}
	}
	return wire.Component{}, false
}

func receiptInput() imports.Input {
	return imports.Input{Source: imports.SrcReceipt, Note: "周六超市", Lines: []imports.Line{
		{Title: "番茄", Qty: fp(2), Unit: "个"},
		{Title: "酸奶", Qty: fp(4), Unit: "杯"},
		{Title: "洗衣液", Qty: fp(1), Unit: "瓶"},
		{Title: "香蕉", Qty: fp(1), Unit: "把"},
	}}
}

// confirmOf is the 确认写入 button of the first draft on the overview.
func confirmOf(t *testing.T, v *wire.View) wire.Component {
	t.Helper()
	b, ok := buttonTitledIn(v.Sections[1], "确认写入")
	if !ok {
		t.Fatalf("no 确认写入 in %+v", v.Sections[1].Items)
	}
	return b
}

func TestDraftSectionShowsLinesAndConfirm(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	d := preview(t, a, receiptInput())
	v := render(t, a, state{})
	checkView(t, "overview with a draft", v, false)
	if v.Sections[1].Title != "待确认的导入" {
		t.Fatalf("second section %q, want the draft right under the numbers", v.Sections[1].Title)
	}
	sec := v.Sections[1]
	if !strings.Contains(sec.Footer, "小票") || !strings.Contains(sec.Footer, "周六超市") {
		t.Fatalf("footer %q", sec.Footer)
	}
	banana, _ := rowTitled(sec, "香蕉")
	if banana.Badge == nil || banana.Badge.Text != "有推测" || !strings.Contains(banana.Subtitle, "（推测）") {
		t.Fatalf("banana row %+v", banana)
	}
	ok := confirmOf(t, v)
	if ok.Action.ID != fmt.Sprintf("impapply:%s:%d", d.ID, d.Rev) || !strings.Contains(ok.Action.Confirm, "会改动 4 样") {
		t.Fatalf("确认写入 %+v", ok)
	}
	if drop, found := buttonTitledIn(sec, "放弃"); !found || drop.Action.Confirm == "" {
		t.Fatalf("放弃 %+v", drop)
	}
	cv, err := a.Card().Render(t.Context(), request(state{}, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	checkView(t, "card with a draft", cv, true)
	if !strings.Contains(fmt.Sprint(cv.Sections), "有 1 份导入等你确认") {
		t.Fatalf("card %+v", cv.Sections)
	}
}

func TestConfirmOnThePageWritesAndOffersUndo(t *testing.T) {
	st := newStore(t, true)
	a, changed := newApp(st)
	preview(t, a, receiptInput())
	res := act(t, a, state{}, nil, confirmOf(t, render(t, a, state{})).Action.ID)
	toastHas(t, res, "已写入")
	if *changed == 0 {
		t.Fatal("the changed hook was not called")
	}
	if got := home(t, st); len(got.Items) != 12 {
		t.Fatalf("%d items after the import", len(got.Items))
	}
	v := res.View
	checkView(t, "after apply", v, false)
	if _, ok := sectionTitled(v, "待确认的导入"); ok {
		t.Fatal("the applied draft is still shown")
	}
	last, ok := sectionTitled(v, "刚才的导入")
	if !ok {
		t.Fatal("no 刚才的导入")
	}
	undo, ok := buttonTitledIn(last, "撤销这次导入")
	if !ok || undo.Action.Confirm == "" {
		t.Fatalf("undo button %+v", last.Items)
	}
	res = act(t, a, state{}, nil, undo.Action.ID)
	toastHas(t, res, "已撤销")
	if after := home(t, st); len(after.Items) != 10 {
		t.Fatalf("%d items after undo", len(after.Items))
	}
	last, _ = sectionTitled(res.View, "刚才的导入")
	if _, ok := buttonTitledIn(last, "撤销这次导入"); ok || !strings.Contains(fmt.Sprint(last.Items), "已经撤销") {
		t.Fatalf("after undo %+v", last.Items)
	}
}

func TestUndoOnThePageSaysWhyNot(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	preview(t, a, receiptInput())
	act(t, a, state{}, nil, confirmOf(t, render(t, a, state{})).Action.ID)
	act(t, a, state{}, nil, "use:"+entityID(t, st, model.TItem, "番茄"))
	last, _ := sectionTitled(render(t, a, state{}), "刚才的导入")
	if _, ok := buttonTitledIn(last, "撤销这次导入"); ok || !strings.Contains(fmt.Sprint(last.Items), "「番茄」在导入之后又改过") {
		t.Fatalf("last import %+v", last.Items)
	}
}

// Review item 1: a button drawn for one revision does not write another.
func TestConfirmIsForThePlanThatWasShown(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	d := preview(t, a, imports.Input{Source: imports.SrcReceipt, Lines: []imports.Line{{Title: "番茄", Qty: fp(2), Unit: "个"}}})
	shown := confirmOf(t, render(t, a, state{}))
	// The assistant re-previews the same draft with an edit after the page was drawn.
	if _, err := imports.Replan(a.importsOf(), home(t, st), d.ID, 0, []imports.Edit{{Line: 1, Qty: fp(50)}}, testLoc(), false); err != nil {
		t.Fatal(err)
	}
	snap := snapshot(t, st)
	res := act(t, a, state{}, nil, shown.Action.ID)
	toastHas(t, res, "预览已经变了，请先看一遍新的再确认")
	if snapshot(t, st) != snap {
		t.Fatal("the old button wrote the new plan")
	}
	// The page is drawn again with the new preview, whose button writes it.
	row, _ := rowTitled(res.View.Sections[1], "番茄")
	if !strings.Contains(row.Subtitle, "加 50个") {
		t.Fatalf("redrawn row %+v", row)
	}
	toastHas(t, act(t, a, state{}, nil, confirmOf(t, res.View).Action.ID), "已写入")
	if tom, _ := home(t, st).Item(entityID(t, st, model.TItem, "番茄")); tom.Qty != 54 {
		t.Fatalf("tomato %v", tom.Qty)
	}
}

// Review item 1, for answers: an answer names its choice and the revision; reordered choices cannot be picked by
// position.
func TestAnswersAreBoundToTheirRevisionAndChoice(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	d := preview(t, a, imports.Input{Source: imports.SrcText, Lines: []imports.Line{{Title: "牛奶", Qty: fp(1), Unit: "盒"}}})
	v := render(t, a, state{})
	sec := v.Sections[1]
	if _, ok := buttonTitledIn(sec, "确认写入"); ok {
		t.Fatal("确认写入 offered while a question is open")
	}
	row, _ := rowTitled(sec, "牛奶")
	if row.Badge == nil || row.Badge.Text != "要你定" || row.Action == nil || row.Action.Push == nil {
		t.Fatalf("question row %+v", row)
	}
	ans := decodeState(t, row.Action.Push.State)
	av := render(t, a, ans)
	checkView(t, "answer page", av, false)
	same, ok := buttonTitledIn(av.Sections[0], "就是「纯牛奶」")
	if !ok || !strings.Contains(same.Action.ID, fmt.Sprintf(":%d:1:match=", d.Rev)) {
		t.Fatalf("answer buttons %+v", av.Sections[0].Items)
	}
	// An answer for another revision is refused.
	stale := strings.Replace(same.Action.ID, fmt.Sprintf(":%d:", d.Rev), fmt.Sprintf(":%d:", d.Rev+1), 1)
	toastHas(t, act(t, a, ans, nil, stale), "预览已经变了")
	// An unknown choice is refused rather than picked by position.
	toastHas(t, act(t, a, ans, nil, fmt.Sprintf("impans:%s:%d:1:match=e999", d.ID, d.Rev)), "预览已经变了")
	snap := snapshot(t, st)
	res := act(t, a, ans, nil, same.Action.ID)
	toastHas(t, res, "已记下")
	if !res.Pop || snapshot(t, st) != snap {
		t.Fatal("answering changed the records, or stayed on the answer page")
	}
	confirmOf(t, render(t, a, state{}))
}

// Review item 4: every line and question is reachable before confirming, and the confirmation counts by kind.
func TestLongDraftsAreConfirmedOnTheirFullList(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	var lines []imports.Line
	for i := range 20 {
		// Distinct names: names that contain one another would be asked about.
		lines = append(lines, imports.Line{Title: fmt.Sprintf("东西%c", 'A'+i), Qty: fp(1)})
	}
	lines = append(lines, imports.Line{Title: "牛奶", Qty: fp(1), Unit: "盒"}) // a question on line 21
	d := preview(t, a, imports.Input{Source: imports.SrcText, Lines: lines})
	v := render(t, a, state{})
	sec := v.Sections[1]
	if _, ok := buttonTitledIn(sec, "确认写入"); ok {
		t.Fatal("确认写入 for lines the overview does not show")
	}
	all, ok := buttonTitledIn(sec, "看全部 21 行再确认")
	if !ok {
		t.Fatalf("no full list button: %+v", sec.Items)
	}
	full := render(t, a, decodeState(t, all.Action.Push.State))
	checkView(t, "full list", full, false)
	if n := len(full.Sections[0].Items); n < 21 {
		t.Fatalf("full list has %d items", n)
	}
	q, ok := rowTitled(full.Sections[0], "21. 牛奶")
	if !ok || q.Action == nil {
		t.Fatalf("line 21's question is not reachable: %+v", q)
	}
	ans := decodeState(t, q.Action.Push.State)
	b, _ := buttonTitledIn(render(t, a, ans).Sections[0], "当新的记")
	act(t, a, ans, nil, b.Action.ID)
	full = render(t, a, state{F: draftForm + d.ID})
	ok2, found := buttonTitledIn(full.Sections[0], "确认写入")
	if !found || !strings.Contains(ok2.Action.Confirm, "新加 21 件") {
		t.Fatalf("confirm on the full list %+v", ok2)
	}
	res := act(t, a, state{F: draftForm + d.ID}, nil, ok2.Action.ID)
	toastHas(t, res, "已写入")
	if !res.Pop {
		t.Fatal("the full list stays open after writing")
	}
}

// Review item 5: a count that lowers the quantity says so on its line and in the confirmation.
func TestCountsThatLowerAreFlagged(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	preview(t, a, imports.Input{Source: imports.SrcFridge, Lines: []imports.Line{{Title: "番茄", Qty: fp(1), Unit: "个"}}})
	v := render(t, a, state{})
	row, _ := rowTitled(v.Sections[1], "番茄")
	if row.Badge == nil || row.Badge.Text != "会变少" || !strings.Contains(row.Subtitle, "会从约 4个 改成 1个") {
		t.Fatalf("row %+v", row)
	}
	if c := confirmOf(t, v); !strings.Contains(c.Action.Confirm, "1 件按看到的记数量，其中 1 件会比现在记的少") {
		t.Fatalf("confirm %q", c.Action.Confirm)
	}
}

func TestStaleDraftAsksToRecompute(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	d := preview(t, a, imports.Input{Source: imports.SrcReceipt, Lines: []imports.Line{{Title: "洗衣液", Qty: fp(1), Unit: "瓶"}}})
	act(t, a, state{}, nil, "skip:"+entityID(t, st, model.TBuy, "洗衣液"))
	sec := render(t, a, state{}).Sections[1]
	if _, ok := buttonTitledIn(sec, "确认写入"); ok {
		t.Fatal("确认写入 on a stale draft")
	}
	re, ok := buttonTitledIn(sec, "重新算一遍")
	if !ok {
		t.Fatalf("no 重新算一遍: %+v", sec.Items)
	}
	res := act(t, a, state{}, nil, re.Action.ID)
	confirmOf(t, res.View)
	snap := snapshot(t, st)
	res = act(t, a, state{}, nil, "impdrop:"+d.ID)
	toastHas(t, res, "已放弃")
	if snapshot(t, st) != snap {
		t.Fatal("discard changed the records")
	}
}

func TestPasteMakesADraft(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	v := render(t, a, state{})
	bulk, ok := sectionTitled(v, addTitle)
	if !ok {
		t.Fatal("no 记新东西")
	}
	if !strings.Contains(fmt.Sprint(bulk.Items), "把这张小票记到家庭管家") {
		t.Fatalf("assistant hint %+v", bulk.Items)
	}
	paste, ok := buttonTitledIn(bulk, "粘贴清单")
	if !ok || paste.Action.Push == nil {
		t.Fatalf("粘贴清单 %+v", paste)
	}
	form := decodeState(t, paste.Action.Push.State)
	fv := render(t, a, form)
	checkView(t, "paste form", fv, false)
	if fv.Title != "粘贴清单" {
		t.Fatalf("form %+v", fv)
	}
	toastHas(t, act(t, a, form, map[string]string{"paste_text": " "}, "imppaste"), "先把要记的东西粘贴进来")
	toastHas(t, act(t, a, form, map[string]string{"paste_text": "!!!\n123"}, "imppaste"), "一行都没读懂")
	snap := snapshot(t, st)
	res := act(t, a, form, map[string]string{"paste_text": "番茄 2 个\n???\n鸡蛋 十个 2026-12-05", "paste_target": "=stock", "paste_mode": "=add"}, "imppaste")
	toastHas(t, res, "读到 2 样")
	if !res.Pop || snapshot(t, st) != snap {
		t.Fatalf("paste result %+v, or the records changed", res)
	}
	sec := render(t, a, state{}).Sections[1]
	if sec.Title != "待确认的导入" || !strings.Contains(sec.Footer, "粘贴的清单") || !strings.Contains(fmt.Sprint(sec.Items), "没读懂") {
		t.Fatalf("draft section %+v", sec)
	}
	// The pasted draft is the user's: the assistant cannot re-plan it.
	var d imports.Draft
	_ = a.importsOf().View(func(s *imports.State) error { d = s.Drafts[0]; return nil })
	if !d.ByUser {
		t.Fatal("pasted draft not marked as the user's")
	}
	if _, err := imports.Replan(a.importsOf(), home(t, st), d.ID, 0, nil, testLoc(), false); err == nil {
		t.Fatal("the assistant re-planned a pasted draft")
	}
}

func TestEmptyHouseholdOffersPasteAndShowsDrafts(t *testing.T) {
	st := newStore(t, false)
	a, _ := newApp(st)
	v := render(t, a, state{})
	if _, ok := sectionTitled(v, addTitle); !ok {
		t.Fatal("no 记新东西 on the empty overview")
	}
	preview(t, a, imports.Input{Source: imports.SrcText, Lines: []imports.Line{{Title: "牛奶", Qty: fp(2), Unit: "盒"}}})
	v = render(t, a, state{})
	checkView(t, "empty overview with a draft", v, false)
	if v.Sections[0].Items[0].Type != wire.ComponentEmpty || v.Sections[1].Title != "待确认的导入" {
		t.Fatalf("sections %q %q", v.Sections[0].Items[0].Type, v.Sections[1].Title)
	}
}

// Review item 11: a draft file that cannot be read is said on the page.
func TestUnreadableDraftsAreSaid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "imports.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _ := newApp(newStore(t, true))
	a.UseImports(imports.Open(dir))
	v := render(t, a, state{})
	checkView(t, "overview with a broken draft file", v, false)
	if v.Sections[1].Title != "待确认的导入" || !strings.Contains(fmt.Sprint(v.Sections[1].Items), "导入草稿的文件现在读不了") {
		t.Fatalf("section %+v", v.Sections[1])
	}
}

func TestOverviewWithManyDraftsStaysInBudget(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	var lines []imports.Line
	for i := range imports.MaxLines {
		lines = append(lines, imports.Line{Title: fmt.Sprintf("东西%d", i), Qty: fp(1)})
	}
	lines[0] = imports.Line{Title: "牛奶", Qty: fp(1), Unit: "盒"}
	var unread []imports.Unread
	for i := range 61 {
		unread = append(unread, imports.Unread{Line: 61 + i, Text: "?", Why: "看不出"})
	}
	var last imports.Draft
	for range imports.DraftsMost {
		last = preview(t, a, imports.Input{Source: imports.SrcText, Lines: lines, Unread: unread})
	}
	v := render(t, a, state{})
	checkView(t, "overview with many drafts", v, false)
	n := 0
	for _, s := range v.Sections {
		if strings.Contains(s.Title, "待确认的导入") {
			n++
		}
	}
	if n != draftsShown || !strings.Contains(fmt.Sprint(v.Sections), "还有 2 份更早的导入") {
		t.Fatalf("%d drafts shown", n)
	}
	checkView(t, "full list of the largest draft", render(t, a, state{F: draftForm + last.ID}), false)
}

// N4 (ii): the assistant cannot apply or discard a pasted draft; the page can.
func TestPastedDraftsAreTheUsersToConfirm(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	form := state{F: pasteForm}
	act(t, a, form, map[string]string{"paste_text": "番茄 2 个", "paste_target": "=stock", "paste_mode": "=add"}, "imppaste")
	var d imports.Draft
	_ = a.importsOf().View(func(s *imports.State) error { d = s.Drafts[0]; return nil })
	err := st.Update(func(b *kernel.Base, now time.Time) error {
		_, _, err := imports.Apply(b, a.importsOf(), d.ID, d.Rev, false, now, testLoc())
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "页面") {
		t.Fatalf("assistant apply of a pasted draft: %v", err)
	}
	if _, err := imports.Discard(a.importsOf(), d.ID, false); err == nil {
		t.Fatal("assistant discard of a pasted draft")
	}
	toastHas(t, act(t, a, state{}, nil, confirmOf(t, render(t, a, state{})).Action.ID), "已写入")
}

// Round 3, item 2: with the draft file unreadable, the page's writes are refused in plain words; reading works.
func TestUnreadableDraftsHoldPageWrites(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "imports.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := newStore(t, true)
	a, _ := newApp(st)
	a.UseImports(imports.Open(dir))
	snap := snapshot(t, st)
	res := act(t, a, state{}, nil, "use:"+entityID(t, st, model.TItem, "番茄"))
	toastHas(t, res, "导入记录读不出来，先处理好再改")
	if snapshot(t, st) != snap {
		t.Fatal("a write went ahead past an unreadable draft file")
	}
	checkView(t, "overview with an unreadable draft file", render(t, a, state{}), false)
}

// Round 3, item 3: an undo that could not be decided says so, and 知道了 hides it without touching records.
func TestUndecidedUndoOnThePage(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	preview(t, a, receiptInput())
	act(t, a, state{}, nil, confirmOf(t, render(t, a, state{})).Action.ID)
	err := a.importsOf().Update(func(s *imports.State, _ time.Time) error {
		last, _ := s.Last()
		last.Unknown = imports.UnknownUndo
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	last, _ := sectionTitled(render(t, a, state{}), "刚才的导入")
	text := fmt.Sprint(last.Items)
	if strings.Contains(text, "已经撤销") || !strings.Contains(text, "说不清撤销了没有") {
		t.Fatalf("undecided undo %+v", last.Items)
	}
	ack, ok := buttonTitledIn(last, "知道了")
	if !ok {
		t.Fatal("no 知道了")
	}
	snap := snapshot(t, st)
	res := act(t, a, state{}, nil, ack.Action.ID)
	if snapshot(t, st) != snap {
		t.Fatal("acknowledging changed the records")
	}
	if _, ok := sectionTitled(res.View, "刚才的导入"); ok {
		t.Fatal("still shown after 知道了")
	}
}
