package imports

import (
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
	"github.com/nwverse/persona-extensions/extensions/household/internal/ops"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

// Second-round review. All records are made up.

// copyBase is a base that shares nothing a write could change with b (writes are copy-on-write).
func copyBase(b *kernel.Base) *kernel.Base {
	c := *b
	c.Spaces = append([]*kernel.Space{}, b.Spaces...)
	return &c
}

// N1: a record that merely has the id the import would have created is no proof it was written.
func TestSettleDoesNotTakeAReusedIDAsWritten(t *testing.T) {
	fx := newFixture(t)
	imp := openTestStore("")
	apply(t, fx, imp, Input{Source: SrcReceipt, Lines: []Line{{Title: "番茄", Qty: f(2), Unit: "个"}}})
	prev := lastApplied(t, imp)
	d, _ := Preview(imp, fx.home(), Input{Source: SrcReceipt, Lines: []Line{{Title: "香蕉", Qty: f(1), Unit: "把"}}}, time.UTC, false)
	// The import runs on a copy that is never saved (the program stops).
	if _, _, err := Apply(copyBase(fx.b), imp, d.ID, d.Rev, false, testNow, time.UTC); err != nil {
		t.Fatal(err)
	}
	// Without settling first, the user's next record would get the same id.
	title := "盐"
	e, err := ops.Save(fx.b, kernel.EntityInput{Type: model.TItem, Title: &title}, testNow, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var pendingID string
	_ = imp.View(func(st *State) error {
		pendingID = st.Applied[len(st.Applied)-1].Touched[0].ID
		return nil
	})
	if e.ID != pendingID {
		t.Fatalf("the scenario needs the same id: %s %s", e.ID, pendingID)
	}
	_ = imp.Update(func(st *State, _ time.Time) error { st.Settle(fx.b); return nil })
	_ = imp.View(func(st *State) error {
		a := st.Applied[len(st.Applied)-1]
		if a.Draft != d.ID || a.Unknown != UnknownApply || a.Pending {
			t.Fatalf("entry %+v, want it unknown, not written", a)
		}
		if _, ok := st.Draft(d.ID); !ok || !st.Blocked(d.ID) {
			t.Fatal("the draft is gone or not blocked")
		}
		return nil
	})
	if last := lastApplied(t, imp); last.Draft != prev.Draft || CheckUndo(fx.b, last) != nil {
		t.Fatalf("the previous import's undo is lost: %+v", last)
	}
	if _, _, err := Apply(fx.b, imp, d.ID, d.Rev, false, testNow, time.UTC); err == nil || !strings.Contains(err.Error(), "被打断") {
		t.Fatalf("re-apply of an undecided draft: %v", err)
	}
	// Discarding the draft clears the undecided entry too.
	if _, err := Discard(imp, d.ID, true); err != nil {
		t.Fatal(err)
	}
	_ = imp.View(func(st *State) error {
		if st.Blocked(d.ID) || len(st.Applied) != 1 {
			t.Fatalf("after discard %+v", st.Applied)
		}
		return nil
	})
}

// N2: a written import whose records were edited afterwards is not taken as never written.
func TestSettleDoesNotForgetAWrittenImport(t *testing.T) {
	fx := newFixture(t)
	imp := openTestStore("")
	d, _ := Preview(imp, fx.home(), Input{Source: SrcReceipt, Lines: []Line{{Title: "鸡蛋", Qty: f(6), Unit: "个"}}}, time.UTC, false)
	if _, _, err := Apply(fx.b, imp, d.ID, d.Rev, false, testNow, time.UTC); err != nil { // saved; finish lost
		t.Fatal(err)
	}
	if _, err := ops.Save(fx.b, kernel.EntityInput{ID: fx.eggs, Values: map[string]any{"qty": 4.0}}, testNow.Add(time.Minute), time.UTC); err != nil {
		t.Fatal(err)
	}
	_ = imp.Update(func(st *State, _ time.Time) error { st.Settle(fx.b); return nil })
	if _, _, err := Apply(fx.b, imp, d.ID, d.Rev, false, testNow, time.UTC); err == nil {
		t.Fatal("the same draft applied twice")
	}
	if eggs, _ := fx.home().Item(fx.eggs); eggs.Qty != 4 {
		t.Fatalf("eggs %v", eggs.Qty)
	}
}

// N3: lines the user says are the same item are one step with the true result.
func TestLinesResolvedToOneItemAreMerged(t *testing.T) {
	fx := newFixture(t)
	tomato := func() string {
		title := "番茄"
		e, _ := ops.Save(fx.b, kernel.EntityInput{Type: model.TItem, Title: &title, Values: map[string]any{"qty": 4.0, "unit": "个"}}, testNow, time.UTC)
		return e.ID
	}()
	in := Input{Source: SrcFridge, Lines: []Line{{Title: "番茄", Qty: f(3), Unit: "个"}, {Title: "小番茄", Qty: f(5), Unit: "个"}}}
	if s := step(t, plan(t, fx, in), 2); s.Action != ActAsk {
		t.Fatalf("line 2 before the answer %+v", s)
	}
	p := plan(t, fx, in, Edit{Line: 2, Match: tomato})
	if len(p.Steps) != 1 || p.Steps[0].Action != ActSetCount || *p.Steps[0].After != 8 || p.Steps[0].Lowers {
		t.Fatalf("count %+v", p.Steps)
	}
	add := Input{Source: SrcReceipt, Lines: []Line{{Title: "鸡蛋", Qty: f(6), Unit: "个"}, {Title: "土鸡蛋", Qty: f(6), Unit: "个"}}}
	p = plan(t, fx, add, Edit{Line: 2, Match: fx.eggs})
	if len(p.Steps) != 1 || *p.Steps[0].Before != 4 || *p.Steps[0].After != 16 || !strings.Contains(p.Steps[0].Says, "约 4 → 16个") {
		t.Fatalf("add %+v", p.Steps)
	}
	// A unit that does not convert, resolved to the same item, is the question, not two counts.
	p = plan(t, fx, Input{Source: SrcFridge, Lines: []Line{{Title: "鸡蛋", Qty: f(6), Unit: "个"}, {Title: "土鸡蛋", Qty: f(1), Unit: "盒"}}},
		Edit{Line: 2, Match: fx.eggs})
	if s := step(t, p, 2); s.Action != ActAsk || s.Ask.Kind != AskDuplicate {
		t.Fatalf("unconvertible %+v", s)
	}
	// Applying the merged count writes 8.
	imp := openTestStore("")
	a := apply(t, fx, imp, in, Edit{Line: 2, Match: tomato})
	if it, _ := fx.home().Item(tomato); it.Qty != 8 || a.Totals.Counted != 1 {
		t.Fatalf("tomato %v", it.Qty)
	}
}

// N4 (ii): the assistant does not apply or discard a draft the user pasted on the page.
func TestTheAssistantDoesNotApplyOrDiscardThePagesDrafts(t *testing.T) {
	fx := newFixture(t)
	imp := openTestStore("")
	d, _ := Preview(imp, fx.home(), Input{Source: SrcPaste, Lines: []Line{{Title: "牛奶", Qty: f(1), Unit: "盒"}}}, time.UTC, true)
	before := snapshot(t, fx.b)
	if _, _, err := Apply(fx.b, imp, d.ID, d.Rev, false, testNow, time.UTC); err == nil || !strings.Contains(err.Error(), "页面") {
		t.Fatalf("assistant apply: %v", err)
	}
	if _, err := Discard(imp, d.ID, false); err == nil {
		t.Fatal("assistant discard")
	}
	if snapshot(t, fx.b) != before {
		t.Fatal("records changed")
	}
	if _, finish, err := Apply(fx.b, imp, d.ID, d.Rev, true, testNow, time.UTC); err != nil {
		t.Fatalf("the page's apply: %v", err)
	} else {
		_ = finish(true)
	}
}

func TestTableNits(t *testing.T) {
	for _, n := range []string{"凤梨", "姜糖", "农药", "老鼠药"} {
		if g, ok := lookup(n); ok {
			t.Errorf("%s → %+v", n, g)
		}
	}
	if g, ok := lookup("雪梨"); !ok || g.days != 21 {
		t.Errorf("雪梨 %+v", g)
	}
}

// A lost undo-finish, then an edit: settling at the start of the edit records the undo as done.
func TestALostUndoFinishIsSettledBeforeTheNextWrite(t *testing.T) {
	fx := newFixture(t)
	imp := openTestStore("")
	apply(t, fx, imp, Input{Source: SrcReceipt, Lines: []Line{{Title: "番茄", Qty: f(2), Unit: "个"}}})
	if _, _, err := UndoLast(fx.b, imp, "", testNow.Add(time.Minute)); err != nil { // saved; finish lost
		t.Fatal(err)
	}
	if err := imp.SettleRecords(fx.b); err != nil { // the start of the next write
		t.Fatal(err)
	}
	if _, err := ops.Use(fx.b, fx.eggs, 1, testNow.Add(2*time.Minute), time.UTC); err != nil {
		t.Fatal(err)
	}
	last := lastApplied(t, imp)
	if last.Undone.IsZero() || last.UndoPending || last.Unknown != "" || !strings.Contains(CheckUndo(fx.b, last).Error(), "撤销过") {
		t.Fatalf("after the edit %+v", last)
	}
}
