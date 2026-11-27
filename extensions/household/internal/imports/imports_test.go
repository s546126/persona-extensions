package imports

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
	"github.com/nwverse/persona-extensions/extensions/household/internal/ops"
	"github.com/nwverse/persona-extensions/extensions/household/internal/rules"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

// All records here are made up.

var testNow = time.Date(2026, time.November, 26, 9, 30, 0, 0, time.UTC)

const testToday = model.Day("2026-11-26")

func openTestStore(dir string) *Store {
	imp := Open(dir)
	imp.SetClock(func() time.Time { return testNow })
	return imp
}

func f(v float64) *float64 { return &v }

// fixture is a household with a fridge and a pantry, milk counted in boxes, eggs counted one by one, untracked
// plasters, rice in kilograms, and a shopping list with the milk and a manual entry for dish soap.
type fixture struct {
	b                                         *kernel.Base
	fridge, pantry, milk, eggs, plaster, rice string
	buyMilk, buySoap                          string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fx := &fixture{b: &kernel.Base{Format: 1, Spaces: []*kernel.Space{}}}
	save := func(typ, title string, vals map[string]any) string {
		t.Helper()
		e, err := ops.Save(fx.b, kernel.EntityInput{Type: typ, Title: &title, Values: vals}, testNow, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		return e.ID
	}
	fx.fridge = save(model.TPlace, "厨房冰箱", nil)
	fx.pantry = save(model.TPlace, "厨房柜", nil)
	fx.milk = save(model.TItem, "牛奶", map[string]any{"category": "食材", "place": []string{fx.fridge}, "qty": 1.0, "unit": "盒"})
	fx.eggs = save(model.TItem, "鸡蛋", map[string]any{"category": "食材", "place": []string{fx.fridge}, "qty": 4.0, "unit": "个"})
	fx.plaster = save(model.TItem, "创可贴", map[string]any{"category": "药品"})
	fx.rice = save(model.TItem, "大米", map[string]any{"category": "食材", "place": []string{fx.pantry}, "qty": 2.0, "unit": "千克"})
	fx.buyMilk = save(model.TBuy, "牛奶", map[string]any{"item": []string{fx.milk}, "amount": 1.0, "unit": "盒"})
	fx.buySoap = save(model.TBuy, "洗洁精", map[string]any{"amount": 1.0, "unit": "瓶"})
	return fx
}

func (fx *fixture) home() *model.Home { return model.Load(household(fx.b), time.UTC) }

func plan(t *testing.T, fx *fixture, in Input, edits ...Edit) Plan {
	t.Helper()
	p, err := Make(fx.home(), in, edits, testToday)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func step(t *testing.T, p Plan, line int) Step {
	t.Helper()
	for _, s := range p.Steps {
		if s.Line == line {
			return s
		}
	}
	t.Fatalf("no step for line %d in %+v", line, p.Steps)
	return Step{}
}

func TestExactMatchAddsToTheItemAndMarksTheListEntryBought(t *testing.T) {
	fx := newFixture(t)
	p := plan(t, fx, Input{Source: SrcReceipt, Lines: []Line{{Title: "牛奶", Qty: f(2), Unit: "盒"}}})
	s := step(t, p, 1)
	if s.Action != ActAddToItem || s.Item.ID != fx.milk || *s.Before != 1 || *s.After != 3 {
		t.Fatalf("step %+v, want add 2 to milk 1 → 3", s)
	}
	if s.Buy == nil || s.Buy.ID != fx.buyMilk || p.Totals.Bought != 1 {
		t.Fatalf("step buy %+v totals %+v, want the milk entry marked bought", s.Buy, p.Totals)
	}
	if !strings.Contains(s.Says, "约 1 → 3盒") || !strings.Contains(s.Says, "标成已买") {
		t.Fatalf("says %q", s.Says)
	}
}

func TestExactMatchIgnoresWidthCaseAndSpaces(t *testing.T) {
	fx := newFixture(t)
	p := plan(t, fx, Input{Source: SrcText, Lines: []Line{{Title: " 鸡 蛋 ", Qty: f(6), Unit: "个"}}})
	if s := step(t, p, 1); s.Action != ActAddToItem || s.Item.ID != fx.eggs {
		t.Fatalf("step %+v, want eggs", s)
	}
	if halfWidth("ＡＢＣ１２（）　x") != "ABC12() x" {
		t.Fatalf("halfWidth %q", halfWidth("ＡＢＣ１２（）　x"))
	}
}

func TestNearNamesAreAskedNotMerged(t *testing.T) {
	fx := newFixture(t)
	for _, title := range []string{"纯牛奶", "伊利纯牛奶 250ml×12", "牛奶（进口）"} {
		p := plan(t, fx, Input{Source: SrcReceipt, Lines: []Line{{Title: title, Qty: f(1), Unit: "箱"}}})
		s := step(t, p, 1)
		if s.Action != ActAsk || s.Ask.Kind != AskSameItem {
			t.Fatalf("%s: step %+v, want a question", title, s)
		}
		if s.Ask.Choices[0].Match != fx.milk || s.Ask.Choices[len(s.Ask.Choices)-1].Match != MatchNew {
			t.Fatalf("%s: choices %+v", title, s.Ask.Choices)
		}
		if p.Totals.Questions != 1 || p.Totals.Changes != 0 {
			t.Fatalf("%s: totals %+v", title, p.Totals)
		}
	}
	// A one-character name does not come near a longer one.
	if closeness("盐", "盐焗鸡") != 0 || closeness("牛奶", "牛奶") != 1 {
		t.Fatal("closeness")
	}
}

func TestAnsweringTheQuestion(t *testing.T) {
	fx := newFixture(t)
	in := Input{Source: SrcReceipt, Lines: []Line{{Title: "纯牛奶", Qty: f(2), Unit: "盒"}}}
	if s := step(t, plan(t, fx, in, Edit{Line: 1, Match: fx.milk}), 1); s.Action != ActAddToItem || s.Item.ID != fx.milk {
		t.Fatalf("answered with the milk: %+v", s)
	}
	if s := step(t, plan(t, fx, in, Edit{Line: 1, Match: MatchNew}), 1); s.Action != ActNewItem || s.Title != "纯牛奶" {
		t.Fatalf("answered new: %+v", s)
	}
}

func TestUnitMismatchAsksInsteadOfMerging(t *testing.T) {
	fx := newFixture(t)
	p := plan(t, fx, Input{Source: SrcReceipt, Lines: []Line{{Title: "鸡蛋", Qty: f(1), Unit: "盒"}}})
	s := step(t, p, 1)
	if s.Action != ActAsk || s.Ask.Kind != AskUnit {
		t.Fatalf("step %+v, want a unit question", s)
	}
	// The user says the box holds 12: the edit gives the quantity in the item's unit.
	p = plan(t, fx, Input{Source: SrcReceipt, Lines: []Line{{Title: "鸡蛋", Qty: f(1), Unit: "盒"}}},
		Edit{Line: 1, Qty: f(12), Unit: strp("个")})
	if s := step(t, p, 1); s.Action != ActAddToItem || *s.After != 16 {
		t.Fatalf("after the edit %+v", s)
	}
	// Weights convert.
	p = plan(t, fx, Input{Source: SrcReceipt, Lines: []Line{{Title: "大米", Qty: f(10), Unit: "斤"}}})
	if s := step(t, p, 1); s.Action != ActAddToItem || *s.Qty != 5 || *s.After != 7 || len(s.Notes) == 0 {
		t.Fatalf("converted %+v", s)
	}
}

func strp(s string) *string { return &s }

func TestMissingQuantityForACountedItemIsAsked(t *testing.T) {
	fx := newFixture(t)
	p := plan(t, fx, Input{Source: SrcReceipt, Lines: []Line{{Title: "鸡蛋"}, {Title: "创可贴"}}})
	if s := step(t, p, 1); s.Action != ActAsk || s.Ask.Kind != AskQty {
		t.Fatalf("eggs without a quantity %+v", s)
	}
	if s := step(t, p, 2); s.Action != ActNothing {
		t.Fatalf("untracked plasters without a quantity %+v", s)
	}
}

func TestDuplicateLinesMerge(t *testing.T) {
	fx := newFixture(t)
	p := plan(t, fx, Input{Source: SrcReceipt, Lines: []Line{
		{Title: "番茄", Qty: f(2), Unit: "个", Price: f(3)},
		{Title: "牛奶", Qty: f(1), Unit: "盒"},
		{Title: "番茄 ", Qty: f(3), Unit: "个", Price: f(4.5)},
		{Title: "番茄", Qty: f(1), Unit: "斤"},
	}})
	s := step(t, p, 1)
	if s.Action != ActNewItem || *s.Qty != 5 || len(s.From) != 2 || s.From[1] != 3 {
		t.Fatalf("merged %+v", s)
	}
	// The 斤 line does not convert into 个: it is a question, not a second new 番茄.
	if d := step(t, p, 4); d.Action != ActAsk || d.Ask.Kind != AskDuplicate || !strings.Contains(d.Ask.Text, "第 1 行") {
		t.Fatalf("line 4 %+v", d)
	}
	if len(p.Steps) != 3 || *p.Totals.PriceTotal != 7.5 {
		t.Fatalf("steps %d totals %+v", len(p.Steps), p.Totals)
	}
}

func TestInferredFieldsAreMarked(t *testing.T) {
	fx := newFixture(t)
	p := plan(t, fx, Input{Source: SrcReceipt, Lines: []Line{
		{Title: "酸奶", Qty: f(4), Unit: "杯"},
		{Title: "神秘小零食", Qty: f(1), Unit: "包"},
		{Title: "洗衣液", Qty: f(1), Unit: "瓶", Category: "清洁", Place: "阳台"},
	}})
	y := step(t, p, 1)
	// The usual shelf life is said, never stored as a date.
	if y.Category != "食材" || y.Place == nil || y.Place.ID != fx.fridge || y.Expires != "" || y.Keeps != 14 ||
		!strings.Contains(strings.Join(y.Notes, " "), "不会记成过期日") {
		t.Fatalf("yogurt %+v", y)
	}
	for _, field := range []string{"category", "place"} {
		if !contains(y.Inferred, field) {
			t.Fatalf("yogurt inferred %v, want %s", y.Inferred, field)
		}
	}
	if !strings.Contains(y.Says, "（推测）") {
		t.Fatalf("says %q", y.Says)
	}
	u := step(t, p, 2)
	if u.Category != "" || u.Expires != "" || u.Place != nil || len(u.Inferred) != 0 {
		t.Fatalf("unknown snack got values %+v", u)
	}
	d := step(t, p, 3)
	if d.Category != "清洁" || contains(d.Inferred, "category") || d.Place.ID != "" || d.Place.Title != "阳台" || p.Totals.NewPlaces != 1 {
		t.Fatalf("detergent %+v", d)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestFridgePhotoCountsAndInventsNoDates(t *testing.T) {
	fx := newFixture(t)
	p := plan(t, fx, Input{Source: SrcFridge, Lines: []Line{{Title: "牛奶", Qty: f(3), Unit: "盒"}, {Title: "酸奶", Qty: f(2), Unit: "杯"}}})
	if s := step(t, p, 1); s.Action != ActSetCount || *s.After != 3 || *s.Before != 1 || s.Buy != nil {
		t.Fatalf("milk %+v, want counted 3 without marking the list", s)
	}
	if s := step(t, p, 2); s.Expires != "" || s.Place.ID != fx.fridge {
		t.Fatalf("yogurt from the fridge %+v", s)
	}
}

func TestShoppingTarget(t *testing.T) {
	fx := newFixture(t)
	p := plan(t, fx, Input{Source: SrcText, Target: ToShopping, Lines: []Line{
		{Title: "洗洁精"}, {Title: "鸡蛋", Qty: f(12)}, {Title: "洗洁精补充装"}, {Title: "牛奶"},
	}})
	if s := step(t, p, 1); s.Action != ActOnList || s.Buy.ID != fx.buySoap {
		t.Fatalf("soap %+v", s)
	}
	if s := step(t, p, 2); s.Action != ActAddToList || s.Item.ID != fx.eggs || s.Unit != "个" || !contains(s.Inferred, "unit") {
		t.Fatalf("eggs %+v", s)
	}
	if s := step(t, p, 3); s.Action != ActAsk || s.Ask.Kind != AskSameEntry {
		t.Fatalf("refill pack %+v", s)
	}
	if s := step(t, p, 4); s.Action != ActOnList || s.Buy.ID != fx.buyMilk {
		t.Fatalf("milk %+v", s)
	}
}

func TestCheckRefusesBadInput(t *testing.T) {
	for _, in := range []Input{
		{Source: "fax", Lines: []Line{{Title: "x"}}},
		{Source: SrcText},
		{Source: SrcText, Lines: []Line{{Title: " "}}},
		{Source: SrcText, Lines: []Line{{Title: "牛奶", Qty: f(-1)}}},
		{Source: SrcText, Lines: []Line{{Title: "牛奶", Expires: "2026-13-01"}}},
		{Source: SrcText, Target: "fridge", Lines: []Line{{Title: "牛奶"}}},
	} {
		if err := Check(in); err == nil {
			t.Fatalf("%+v accepted", in)
		}
	}
}

// ---- drafts ----

func TestDraftsExpireAndAreCapped(t *testing.T) {
	imp := Open("")
	clock := testNow
	imp.SetClock(func() time.Time { return clock })
	h := &model.Home{}
	in := Input{Source: SrcText, Lines: []Line{{Title: "牛奶", Qty: f(1)}}}
	first, err := Preview(imp, h, in, time.UTC, false)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Expires.Equal(testNow.Add(24 * time.Hour)) {
		t.Fatalf("expires %v", first.Expires)
	}
	for range DraftsMost {
		if _, err := Preview(imp, h, in, time.UTC, false); err != nil {
			t.Fatal(err)
		}
	}
	_ = imp.View(func(st *State) error {
		if len(st.Drafts) != DraftsMost {
			t.Fatalf("%d drafts kept", len(st.Drafts))
		}
		if _, ok := st.Draft(first.ID); ok {
			t.Fatal("the oldest draft was not pushed out")
		}
		return nil
	})
	clock = testNow.Add(25 * time.Hour)
	_ = imp.View(func(st *State) error {
		if len(st.Drafts) != 0 {
			t.Fatalf("%d drafts after a day", len(st.Drafts))
		}
		return nil
	})
	if _, err := Discard(imp, first.ID, false); err == nil || !strings.Contains(err.Error(), "24 小时") {
		t.Fatalf("discard of an expired draft: %v", err)
	}
}

func TestStoreOnDiskIsSharedAndRefusesABrokenFile(t *testing.T) {
	dir := t.TempDir()
	a, b := openTestStore(dir), openTestStore(dir)
	d, err := Preview(a, &model.Home{}, Input{Source: SrcText, Lines: []Line{{Title: "牛奶"}}}, time.UTC, false)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Changed() {
		t.Fatal("the other process did not see the change")
	}
	if err := b.View(func(st *State) error {
		if _, ok := st.Draft(d.ID); !ok {
			t.Fatal("draft not shared")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(a.path(), []byte("{broken")); err != nil {
		t.Fatal(err)
	}
	if err := a.Update(func(*State, time.Time) error { return nil }); err == nil {
		t.Fatal("broken file accepted")
	}
}

// ---- apply and undo ----

// applied previews the lines, applies the draft and returns the store and the import.
func apply(t *testing.T, fx *fixture, imp *Store, in Input, edits ...Edit) Applied {
	t.Helper()
	d, err := Preview(imp, fx.home(), in, time.UTC, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) > 0 {
		if d, err = Replan(imp, fx.home(), d.ID, d.Rev, edits, time.UTC, false); err != nil {
			t.Fatal(err)
		}
	}
	a, finish, err := Apply(fx.b, imp, d.ID, d.Rev, false, testNow, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(true); err != nil {
		t.Fatal(err)
	}
	return a
}

func snapshot(t *testing.T, b *kernel.Base) string {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestApplyWritesEveryStepAndRecordsIt(t *testing.T) {
	fx := newFixture(t)
	imp := openTestStore("")
	a := apply(t, fx, imp, Input{Source: SrcReceipt, Lines: []Line{
		{Title: "牛奶", Qty: f(2), Unit: "盒"},
		{Title: "酸奶", Qty: f(4), Unit: "杯"},
		{Title: "洗洁精", Qty: f(1), Unit: "瓶"},
		{Title: "大米", Qty: f(10), Unit: "斤"},
		{Title: "洗衣液", Qty: f(1), Unit: "瓶", Place: "阳台"},
	}})
	h := fx.home()
	milk, _ := h.Item(fx.milk)
	rice, _ := h.Item(fx.rice)
	if milk.Qty != 3 || rice.Qty != 7 {
		t.Fatalf("milk %v rice %v", milk.Qty, rice.Qty)
	}
	status := map[string]string{}
	for _, b := range h.Buys {
		status[b.ID] = b.Status
	}
	if status[fx.buyMilk] != model.BuyDone || status[fx.buySoap] != model.BuyDone {
		t.Fatalf("purchases %v", status)
	}
	var yogurt, soap, balcony bool
	for _, it := range h.Items {
		yogurt = yogurt || it.Title == "酸奶" && it.Qty == 4 && it.PlaceID == fx.fridge && it.Expires == ""
		soap = soap || it.Title == "洗洁精" && it.Qty == 1
	}
	for _, p := range h.Places {
		balcony = balcony || p.Title == "阳台"
	}
	if !yogurt || !soap || !balcony {
		t.Fatalf("new records: yogurt %v soap %v balcony %v", yogurt, soap, balcony)
	}
	if len(a.Says) != 5 || a.Totals.NewItems != 3 || a.Totals.NewPlaces != 1 {
		t.Fatalf("applied %+v", a)
	}
	// The draft is gone; the import is recorded.
	_ = imp.View(func(st *State) error {
		if len(st.Drafts) != 0 || len(st.Applied) != 1 {
			t.Fatalf("state %+v", st)
		}
		return nil
	})
}

func TestApplyIsAllOrNothing(t *testing.T) {
	fx := newFixture(t)
	imp := openTestStore("")
	d, err := Preview(imp, fx.home(), Input{Source: SrcReceipt, Lines: []Line{
		{Title: "牛奶", Qty: f(2), Unit: "盒"}, {Title: "纯牛奶", Qty: f(1), Unit: "盒"},
	}}, time.UTC, false)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, fx.b)
	// A question left: nothing is written.
	if _, _, err := Apply(fx.b, imp, d.ID, d.Rev, false, testNow, time.UTC); err == nil || !strings.Contains(err.Error(), "要用户定") {
		t.Fatalf("apply with a question: %v", err)
	}
	if snapshot(t, fx.b) != before {
		t.Fatal("refused apply changed the records")
	}
	// A step that fails half way (the milk purchase was closed meanwhile) leaves the records as they were.
	if _, err := ops.Purchase(fx.b, fx.buyMilk, false, testNow, time.UTC); err != nil {
		t.Fatal(err)
	}
	before = snapshot(t, fx.b)
	d, err = Replan(imp, model.Load(household(fx.b), time.UTC), d.ID, d.Rev, []Edit{{Line: 2, Match: MatchNew}}, time.UTC, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Use(fx.b, fx.milk, 1, testNow, time.UTC); err != nil {
		t.Fatal(err)
	}
	before = snapshot(t, fx.b)
	if _, _, err := Apply(fx.b, imp, d.ID, d.Rev, false, testNow, time.UTC); err == nil || !strings.Contains(err.Error(), "变了") {
		t.Fatalf("apply after the records changed: %v", err)
	}
	if snapshot(t, fx.b) != before {
		t.Fatal("stale apply changed the records")
	}
	// Writing fails part way: the base stays as it was.
	h := fx.home()
	bad := Plan{Steps: []Step{
		{Line: 1, Action: ActNewItem, Title: "酸奶", Qty: f(1)},
		{Line: 2, Action: ActAddToItem, Title: "x", Item: &Ref{ID: "nope"}, Qty: f(1)},
	}}
	if _, err := Write(fx.b, h, bad, testNow, time.UTC); err == nil {
		t.Fatal("bad plan written")
	}
	if snapshot(t, fx.b) != before {
		t.Fatal("failed write changed the records")
	}
}

func TestUndoPutsEverythingBack(t *testing.T) {
	fx := newFixture(t)
	imp := openTestStore("")
	before := fx.home()
	apply(t, fx, imp, Input{Source: SrcReceipt, Lines: []Line{
		{Title: "牛奶", Qty: f(2), Unit: "盒"}, {Title: "酸奶", Qty: f(4), Unit: "杯", Place: "阳台"}, {Title: "洗洁精"},
	}})
	later := testNow.Add(time.Hour)
	_, finish, err := UndoLast(fx.b, imp, "", later)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(true); err != nil {
		t.Fatal(err)
	}
	after := fx.home()
	if len(after.Items) != len(before.Items) || len(after.Places) != len(before.Places) || len(after.Buys) != len(before.Buys) {
		t.Fatalf("records after undo: %d items %d places %d buys", len(after.Items), len(after.Places), len(after.Buys))
	}
	milk, _ := after.Item(fx.milk)
	if milk.Qty != 1 || milk.Counted != testToday {
		t.Fatalf("milk after undo %+v", milk)
	}
	for _, b := range after.Buys {
		if b.Status != model.BuyTodo {
			t.Fatalf("purchase %s is %s after undo", b.Title, b.Status)
		}
	}
	if _, _, err := UndoLast(fx.b, imp, "", later); err == nil || !strings.Contains(err.Error(), "撤销过") {
		t.Fatalf("second undo: %v", err)
	}
}

func TestUndoRefusesWhenSomethingChangedSince(t *testing.T) {
	fx := newFixture(t)
	imp := openTestStore("")
	a := apply(t, fx, imp, Input{Source: SrcReceipt, Lines: []Line{{Title: "牛奶", Qty: f(2), Unit: "盒"}, {Title: "酸奶", Qty: f(4), Unit: "杯"}}})
	if _, err := ops.Use(fx.b, fx.milk, 1, testNow.Add(time.Minute), time.UTC); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, fx.b)
	_, _, err := UndoLast(fx.b, imp, "", testNow.Add(time.Hour))
	var blocked *Blocked
	if !errors.As(err, &blocked) || !strings.Contains(err.Error(), "「牛奶」在导入之后又改过") {
		t.Fatalf("undo after a change: %v", err)
	}
	if snapshot(t, fx.b) != before {
		t.Fatal("refused undo changed the records")
	}
	// A purchase made later for a new item blocks too.
	fx2 := newFixture(t)
	imp2 := openTestStore("")
	apply(t, fx2, imp2, Input{Source: SrcReceipt, Lines: []Line{{Title: "酸奶", Qty: f(1), Unit: "杯"}}})
	var yogurt string
	for _, it := range fx2.home().Items {
		if it.Title == "酸奶" {
			yogurt = it.ID
		}
	}
	title := "酸奶"
	if _, err := ops.Save(fx2.b, kernel.EntityInput{Type: model.TBuy, Title: &title, Values: map[string]any{"item": []string{yogurt}}}, testNow, time.UTC); err != nil {
		t.Fatal(err)
	}
	if _, _, err := UndoLast(fx2.b, imp2, "", testNow); err == nil || !strings.Contains(err.Error(), "用到了") {
		t.Fatalf("undo with a reference: %v", err)
	}
	_ = a
	if _, _, err := UndoLast(fx2.b, imp2, "imp-9", testNow); err == nil || !strings.Contains(err.Error(), "只能撤销最近一次") {
		t.Fatalf("undo of another draft: %v", err)
	}
}

func TestExpiredItemTakesTheNewBatchOnly(t *testing.T) {
	fx := newFixture(t)
	if _, err := ops.Save(fx.b, kernel.EntityInput{ID: fx.eggs, Values: map[string]any{"expires": string(testToday.Add(-2))}}, testNow, time.UTC); err != nil {
		t.Fatal(err)
	}
	imp := openTestStore("")
	p := plan(t, fx, Input{Source: SrcReceipt, Lines: []Line{{Title: "鸡蛋", Qty: f(10), Unit: "个", Expires: "2026-12-20"}}})
	if s := step(t, p, 1); *s.After != 10 || s.SetExpires != "2026-12-20" {
		t.Fatalf("step %+v", s)
	}
	apply(t, fx, imp, Input{Source: SrcReceipt, Lines: []Line{{Title: "鸡蛋", Qty: f(10), Unit: "个", Expires: "2026-12-20"}}})
	eggs, _ := fx.home().Item(fx.eggs)
	if eggs.Qty != 10 || eggs.Expires != "2026-12-20" {
		t.Fatalf("eggs %+v", eggs)
	}
	if f := rules.Findings(fx.home(), testToday); len(f) != 0 {
		for _, x := range f {
			if x.Item.ID == fx.eggs {
				t.Fatalf("eggs still flagged: %+v", x)
			}
		}
	}
}
