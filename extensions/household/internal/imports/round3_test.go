package imports

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
	"github.com/nwverse/persona-extensions/extensions/household/internal/ops"
)

// Third-round review. All records are made up.

// 2: while the import store cannot be read, a write of the records is refused, so a pending import stays decidable.
func TestUnreadableStoreHoldsRecordWrites(t *testing.T) {
	fx := newFixture(t)
	dir := t.TempDir()
	imp := openTestStore(dir)
	d, err := Preview(imp, fx.home(), Input{Source: SrcReceipt, Lines: []Line{{Title: "香蕉", Qty: f(1), Unit: "把"}}}, time.UTC, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Apply(fx.b, imp, d.ID, d.Rev, false, testNow, time.UTC); err != nil { // saved; finish lost
		t.Fatal(err)
	}
	good, _ := os.ReadFile(imp.path())
	if err := writeAtomic(imp.path(), []byte("{broken")); err != nil {
		t.Fatal(err)
	}
	err = imp.SettleRecords(fx.b)
	var unreadable *UnreadableError
	if !errors.As(err, &unreadable) || !strings.Contains(err.Error(), "导入记录读不出来，先处理好再改") {
		t.Fatalf("settle with a broken file: %v", err)
	}
	// The write that would delete the banana does not happen; the file comes back; the import is decided written.
	if err := writeAtomic(imp.path(), good); err != nil {
		t.Fatal(err)
	}
	if err := imp.SettleRecords(fx.b); err != nil {
		t.Fatal(err)
	}
	if _, ok := draftOf(t, imp, d.ID); ok {
		t.Fatal("the written draft came back")
	}
	if last := lastApplied(t, imp); last == nil || last.Draft != d.ID {
		t.Fatalf("last %+v", last)
	}
}

// 3: an undo that cannot be decided is not said to be done, and can be acknowledged without touching records.
func TestAnUndecidedUndoIsNotCalledDone(t *testing.T) {
	fx := newFixture(t)
	imp := openTestStore("")
	apply(t, fx, imp, Input{Source: SrcReceipt, Lines: []Line{{Title: "鸡蛋", Qty: f(6), Unit: "个"}, {Title: "香蕉", Qty: f(1), Unit: "把"}}})
	work := copyBase(fx.b)
	if _, _, err := UndoLast(work, imp, "", testNow.Add(time.Minute)); err != nil { // pending
		t.Fatal(err)
	}
	// Another program changes one record in between: neither before nor after.
	if _, err := ops.Use(fx.b, fx.eggs, 1, testNow.Add(2*time.Minute), time.UTC); err != nil {
		t.Fatal(err)
	}
	_ = imp.Update(func(st *State, _ time.Time) error { st.Settle(fx.b); return nil })
	last := lastApplied(t, imp)
	if !last.Undone.IsZero() || last.Unknown != UnknownUndo || CheckUndo(fx.b, last) == nil {
		t.Fatalf("undecided undo %+v", last)
	}
	before := snapshot(t, fx.b)
	if err := Acknowledge(imp, last.Draft); err != nil {
		t.Fatal(err)
	}
	if l := lastApplied(t, imp); !l.Seen || snapshot(t, fx.b) != before {
		t.Fatalf("acknowledged %+v", l)
	}
}

// 4: "bought 2, still need more" in one import leaves the list entry open, as the preview says.
func TestBoughtAndStillWantedKeepsTheListEntry(t *testing.T) {
	fx := newFixture(t)
	in := Input{Source: SrcText, Lines: []Line{{Title: "牛奶", Qty: f(2), Unit: "盒"}, {Title: "牛奶", Target: ToShopping}}}
	p := plan(t, fx, in)
	stock, shop := step(t, p, 1), step(t, p, 2)
	if stock.Buy != nil || strings.Contains(stock.Says, "标成已买") || !strings.Contains(strings.Join(stock.Notes, ""), "留着") {
		t.Fatalf("stock line %+v", stock)
	}
	if shop.Action != ActOnList || p.Totals.Bought != 0 {
		t.Fatalf("shopping line %+v, totals %+v", shop, p.Totals)
	}
	imp := openTestStore("")
	apply(t, fx, imp, in)
	h := fx.home()
	milk, _ := h.Item(fx.milk)
	for _, b := range h.Buys {
		if b.ID == fx.buyMilk && b.Status != model.BuyTodo {
			t.Fatalf("the milk entry is %s", b.Status)
		}
	}
	if milk.Qty != 3 {
		t.Fatalf("milk %v", milk.Qty)
	}
}

// 5: the label follows who made the draft; the duplicate question names the right line; near new names are asked.
func TestRoundThreeNits(t *testing.T) {
	if DraftLabel(Draft{Input: Input{Source: SrcPaste}}) == SourceLabel(SrcPaste) {
		t.Fatal("an assistant draft labelled as pasted")
	}
	if DraftLabel(Draft{ByUser: true, Input: Input{Source: SrcPaste}}) != "粘贴的清单" {
		t.Fatal("a pasted draft not labelled as such")
	}
	fx := newFixture(t)
	p := plan(t, fx, Input{Source: SrcShelf, Lines: []Line{{Title: "大米", Qty: f(1), Unit: "千克"}, {Title: "东北大米", Qty: f(1), Unit: "袋"}}},
		Edit{Line: 2, Match: fx.rice})
	if s := step(t, p, 2); s.Ask == nil || !strings.Contains(s.Ask.Text, "第 1 行「大米」") || !strings.Contains(s.Ask.Text, "「东北大米」") {
		t.Fatalf("duplicate question %+v", s.Ask)
	}
	in := Input{Source: SrcReceipt, Lines: []Line{{Title: "酸奶", Qty: f(2), Unit: "杯"}, {Title: "老酸奶", Qty: f(1), Unit: "杯"}}}
	p = plan(t, fx, in)
	s := step(t, p, 2)
	if s.Action != ActAsk || !strings.Contains(s.Ask.Text, "第 1 行有「酸奶」") || s.Ask.Choices[0].Match != "line:1" {
		t.Fatalf("near new names %+v", s)
	}
	p = plan(t, fx, in, Edit{Line: 2, Match: "line:1"})
	if len(p.Steps) != 1 || p.Steps[0].Action != ActNewItem || *p.Steps[0].Qty != 3 {
		t.Fatalf("answered same %+v", p.Steps)
	}
	p = plan(t, fx, in, Edit{Line: 2, Match: MatchNew})
	if len(p.Steps) != 2 || p.Totals.NewItems != 2 {
		t.Fatalf("answered new %+v", p.Steps)
	}
}
