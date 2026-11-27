package imports

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
	"github.com/nwverse/persona-extensions/extensions/household/internal/ops"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

// Tests for what an independent review of the import reproduced. All records are made up.

func lastApplied(t *testing.T, imp *Store) *Applied {
	t.Helper()
	var out *Applied
	_ = imp.View(func(st *State) error {
		if a, ok := st.Last(); ok {
			cp := *a
			out = &cp
		}
		return nil
	})
	return out
}

func draftOf(t *testing.T, imp *Store, id string) (Draft, bool) {
	t.Helper()
	var d Draft
	var ok bool
	_ = imp.View(func(st *State) error { d, ok = st.Draft(id); return nil })
	return d, ok
}

// 1: a yes is for one revision of the plan.
func TestApplyNeedsTheRevisionThatWasShown(t *testing.T) {
	fx := newFixture(t)
	imp := openTestStore("")
	d, err := Preview(imp, fx.home(), Input{Source: SrcReceipt, Lines: []Line{{Title: "牛奶", Qty: f(2), Unit: "盒"}}}, time.UTC, false)
	if err != nil {
		t.Fatal(err)
	}
	shown := d.Rev
	// The assistant re-previews with an edit after the page was drawn.
	d2, err := Replan(imp, fx.home(), d.ID, 0, []Edit{{Line: 1, Qty: f(54)}}, time.UTC, false)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Rev != shown+1 {
		t.Fatalf("revision %d after a re-plan of %d", d2.Rev, shown)
	}
	before := snapshot(t, fx.b)
	if _, _, err := Apply(fx.b, imp, d.ID, shown, false, testNow, time.UTC); !errors.Is(err, ErrChanged) {
		t.Fatalf("apply of the old revision: %v", err)
	}
	if snapshot(t, fx.b) != before {
		t.Fatal("a refused apply wrote")
	}
	// An answer given for the old revision is refused too.
	if _, err := Replan(imp, fx.home(), d.ID, shown, []Edit{{Line: 1, Match: MatchNew}}, time.UTC, true); !errors.Is(err, ErrChanged) {
		t.Fatalf("answer for the old revision: %v", err)
	}
	a, finish, err := Apply(fx.b, imp, d.ID, d2.Rev, false, testNow, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	_ = finish(true)
	if milk, _ := fx.home().Item(fx.milk); milk.Qty != 55 || a.Totals.AddedTo != 1 {
		t.Fatalf("milk %v", milk.Qty)
	}
}

// 2: the stale check covers the outcome, not only the action.
func TestStaleWhenTheOldBatchExpiresOvernight(t *testing.T) {
	fx := newFixture(t)
	if _, err := ops.Save(fx.b, kernel.EntityInput{ID: fx.eggs, Values: map[string]any{"expires": string(testToday)}}, testNow, time.UTC); err != nil {
		t.Fatal(err)
	}
	imp := openTestStore("")
	d, err := Preview(imp, fx.home(), Input{Source: SrcReceipt, Lines: []Line{{Title: "鸡蛋", Qty: f(6), Unit: "个"}}}, time.UTC, false)
	if err != nil {
		t.Fatal(err)
	}
	if s := step(t, d.Plan, 1); *s.After != 10 || s.OldGone {
		t.Fatalf("preview %+v, want 4 → 10", s)
	}
	// Advance across midnight into the next calendar day while staying within the 24-hour draft TTL.
	tomorrow := testNow.Add(20 * time.Hour)
	before := snapshot(t, fx.b)
	_, _, err = Apply(fx.b, imp, d.ID, d.Rev, false, tomorrow, time.UTC)
	if err == nil || !strings.Contains(err.Error(), "变了") {
		t.Fatalf("apply the next day: %v", err)
	}
	if snapshot(t, fx.b) != before {
		t.Fatal("stale apply wrote")
	}
	// Recomputing shows the new outcome, which can then be agreed to.
	imp.SetClock(func() time.Time { return tomorrow })
	d2, err := Replan(imp, model.Load(household(fx.b), time.UTC), d.ID, 0, nil, time.UTC, true)
	if err != nil {
		t.Fatal(err)
	}
	if s := step(t, d2.Plan, 1); !s.OldGone || *s.After != 6 || !strings.Contains(s.Says, "旧的已过期") {
		t.Fatalf("recomputed %+v", s)
	}
}

// 3: counted duplicates convert, or ask; they never overwrite each other.
func TestCountModeDuplicatesConvertOrAsk(t *testing.T) {
	fx := newFixture(t)
	p := plan(t, fx, Input{Source: SrcShelf, Lines: []Line{{Title: "大米", Qty: f(1), Unit: "千克"}, {Title: "大米", Qty: f(4), Unit: "斤"}}})
	if len(p.Steps) != 1 || p.Steps[0].Action != ActSetCount || *p.Steps[0].After != 3 {
		t.Fatalf("rice %+v", p.Steps)
	}
	p = plan(t, fx, Input{Source: SrcFridge, Lines: []Line{{Title: "牛奶", Qty: f(2)}, {Title: "牛奶", Qty: f(3), Unit: "盒"}}})
	if s := step(t, p, 2); s.Action != ActAsk || s.Ask.Kind != AskDuplicate {
		t.Fatalf("milk line 2 %+v", s)
	}
	imp := openTestStore("")
	d, _ := Preview(imp, fx.home(), Input{Source: SrcFridge, Lines: []Line{{Title: "牛奶", Qty: f(2)}, {Title: "牛奶", Qty: f(3), Unit: "盒"}}}, time.UTC, false)
	if _, _, err := Apply(fx.b, imp, d.ID, d.Rev, false, testNow, time.UTC); err == nil {
		t.Fatal("two counts of one item written")
	}
	// The user says the first line is boxes too: one count of 5.
	d, err := Replan(imp, fx.home(), d.ID, d.Rev, []Edit{{Line: 1, Unit: strp("盒")}}, time.UTC, false)
	if err != nil {
		t.Fatal(err)
	}
	a, finish, err := Apply(fx.b, imp, d.ID, d.Rev, false, testNow, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	_ = finish(true)
	if milk, _ := fx.home().Item(fx.milk); milk.Qty != 5 || a.Totals.Counted != 1 {
		t.Fatalf("milk %v", milk.Qty)
	}
}

// 5: a count below the estimate says so.
func TestCountBelowTheEstimateIsFlagged(t *testing.T) {
	fx := newFixture(t)
	p := plan(t, fx, Input{Source: SrcFridge, Lines: []Line{{Title: "大米", Qty: f(1), Unit: "千克"}, {Title: "鸡蛋", Qty: f(6), Unit: "个"}}})
	r := step(t, p, 1)
	if !r.Lowers || !strings.Contains(r.Says, "会从约 2千克 改成 1千克") {
		t.Fatalf("rice %+v", r)
	}
	if e := step(t, p, 2); e.Lowers {
		t.Fatalf("eggs going up flagged %+v", e)
	}
	if p.Totals.Lowered != 1 || !strings.Contains(ConfirmText(p.Totals), "其中 1 件会比现在记的少") ||
		!strings.Contains(ConfirmText(p.Totals), "2 件按看到的记数量") {
		t.Fatalf("totals %+v, confirm %q", p.Totals, ConfirmText(p.Totals))
	}
}

// 6: a records save that fails or never happens leaves the draft, and the last real import's undo.
func TestALostRecordsSaveLeavesTheDraft(t *testing.T) {
	fx := newFixture(t)
	imp := openTestStore("")
	apply(t, fx, imp, Input{Source: SrcReceipt, Lines: []Line{{Title: "酸奶", Qty: f(1), Unit: "杯"}}})
	first := lastApplied(t, imp)
	d, _ := Preview(imp, fx.home(), Input{Source: SrcReceipt, Lines: []Line{{Title: "牛奶", Qty: f(2), Unit: "盒"}}}, time.UTC, false)

	// The save fails: the caller says so.
	work := *fx.b
	work.Spaces = append([]*kernel.Space{}, fx.b.Spaces...)
	_, finish, err := Apply(&work, imp, d.ID, d.Rev, false, testNow, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(false); err != nil {
		t.Fatal(err)
	}
	if _, ok := draftOf(t, imp, d.ID); !ok {
		t.Fatal("the draft is gone after a failed save")
	}
	if last := lastApplied(t, imp); last == nil || last.Draft != first.Draft || CheckUndo(fx.b, last) != nil {
		t.Fatalf("last import %+v", last)
	}

	// The program stops between the two saves: settling from the records that were kept says not written.
	work = *fx.b
	work.Spaces = append([]*kernel.Space{}, fx.b.Spaces...)
	if _, _, err := Apply(&work, imp, d.ID, d.Rev, false, testNow, time.UTC); err != nil {
		t.Fatal(err)
	}
	if last := lastApplied(t, imp); last.Draft != first.Draft {
		t.Fatal("a pending import hides the last real one")
	}
	if err := imp.Update(func(st *State, _ time.Time) error { st.Settle(fx.b); return nil }); err != nil {
		t.Fatal(err)
	}
	_ = imp.View(func(st *State) error {
		if len(st.Applied) != 1 {
			t.Fatalf("applied after settling an unsaved import: %+v", st.Applied)
		}
		if _, ok := st.Draft(d.ID); !ok {
			t.Fatal("draft lost")
		}
		return nil
	})

	// It stops after the records were saved: settling says written.
	if _, _, err := Apply(fx.b, imp, d.ID, d.Rev, false, testNow, time.UTC); err != nil {
		t.Fatal(err)
	}
	if err := imp.Update(func(st *State, _ time.Time) error { st.Settle(fx.b); return nil }); err != nil {
		t.Fatal(err)
	}
	if last := lastApplied(t, imp); last.Draft != d.ID || CheckUndo(fx.b, last) != nil {
		t.Fatalf("settled import %+v", last)
	}
	if _, ok := draftOf(t, imp, d.ID); ok {
		t.Fatal("the written draft is still waiting")
	}
}

// 6, for undo: an undo whose records were not saved is not recorded as done.
func TestALostUndoSaveKeepsTheImport(t *testing.T) {
	fx := newFixture(t)
	imp := openTestStore("")
	apply(t, fx, imp, Input{Source: SrcReceipt, Lines: []Line{{Title: "酸奶", Qty: f(1), Unit: "杯"}}})
	work := *fx.b
	work.Spaces = append([]*kernel.Space{}, fx.b.Spaces...)
	_, finish, err := UndoLast(&work, imp, "", testNow)
	if err != nil {
		t.Fatal(err)
	}
	_ = finish(false)
	if last := lastApplied(t, imp); !last.Undone.IsZero() || CheckUndo(fx.b, last) != nil {
		t.Fatalf("after a failed undo save %+v", last)
	}
}

// 7: the table matches whole product words and stores no guessed date.
func TestTableMatchesWholeWords(t *testing.T) {
	for _, name := range []string{"鱼罐头", "贝果", "葱油饼", "糖果", "盐焗鸡", "果冻", "棉花糖"} {
		if g, ok := lookup(name); ok {
			t.Errorf("%s matched %+v", name, g)
		}
	}
	for name, want := range map[string]int{"鲜牛奶": 7, "土鸡蛋": 30, "洋葱": 30, "进口香蕉": 5, "伊利酸牛奶 200g": 14} {
		if g, ok := lookup(name); !ok || g.days != want {
			t.Errorf("%s: %+v %v, want %d days", name, g, ok, want)
		}
	}
	if g, ok := lookup("冰糖"); !ok || g.category != "调味" {
		t.Errorf("冰糖 %+v", g)
	}
}

// 8: a given date that replaces another shows both.
func TestAGivenDateShowsOldAndNew(t *testing.T) {
	fx := newFixture(t)
	if _, err := ops.Save(fx.b, kernel.EntityInput{ID: fx.eggs, Values: map[string]any{"expires": "2026-12-20"}}, testNow, time.UTC); err != nil {
		t.Fatal(err)
	}
	p := plan(t, fx, Input{Source: SrcFridge, Lines: []Line{{Title: "鸡蛋", Qty: f(6), Unit: "个", Expires: "2026-12-10"}}})
	if s := step(t, p, 1); s.OldExpires != "2026-12-20" || !strings.Contains(s.Says, "过期日 12-20 → 12-10") {
		t.Fatalf("step %+v", s)
	}
}

// 9: limits are checked when previewing.
func TestLimitsAreCheckedAtPreview(t *testing.T) {
	long := strings.Repeat("长", 50)
	for name, in := range map[string]Input{
		"tiny qty":   {Source: SrcText, Lines: []Line{{Title: "牛奶", Qty: f(1e-9)}}},
		"huge qty":   {Source: SrcText, Lines: []Line{{Title: "牛奶", Qty: f(1e9)}}},
		"long unit":  {Source: SrcText, Lines: []Line{{Title: "牛奶", Qty: f(1), Unit: long}}},
		"long place": {Source: SrcText, Lines: []Line{{Title: "牛奶", Place: long}}},
		"long note":  {Source: SrcText, Note: strings.Repeat(long, 5), Lines: []Line{{Title: "牛奶"}}},
	} {
		if _, err := Preview(openTestStore(""), &model.Home{}, in, time.UTC, false); err == nil {
			t.Errorf("%s: previewed", name)
		}
	}
}

// 10: the assistant does not push out or re-plan a draft the user made on the page.
func TestTheAssistantLeavesTheUsersDraftsAlone(t *testing.T) {
	imp := openTestStore("")
	h := &model.Home{}
	in := Input{Source: SrcPaste, Lines: []Line{{Title: "牛奶", Qty: f(1)}}}
	var mine []Draft
	for range DraftsMost {
		d, err := Preview(imp, h, in, time.UTC, true)
		if err != nil {
			t.Fatal(err)
		}
		mine = append(mine, d)
	}
	if _, err := Preview(imp, h, Input{Source: SrcReceipt, Lines: in.Lines}, time.UTC, false); err == nil || !strings.Contains(err.Error(), "页面上") {
		t.Fatalf("assistant preview over the user's drafts: %v", err)
	}
	if _, err := Replan(imp, h, mine[0].ID, 0, []Edit{{Line: 1, Qty: f(9)}}, time.UTC, false); err == nil {
		t.Fatal("the assistant re-planned the user's draft")
	}
	if _, err := Replan(imp, h, mine[0].ID, mine[0].Rev, []Edit{{Line: 1, Qty: f(9)}}, time.UTC, true); err != nil {
		t.Fatalf("the user's own answer: %v", err)
	}
	// The user's own paste may push out the oldest.
	if _, err := Preview(imp, h, in, time.UTC, true); err != nil {
		t.Fatal(err)
	}
}

// 11: the file has a version; a newer one is refused, not overwritten.
func TestStoreVersion(t *testing.T) {
	dir := t.TempDir()
	imp := openTestStore(dir)
	if _, err := Preview(imp, &model.Home{}, Input{Source: SrcText, Lines: []Line{{Title: "牛奶"}}}, time.UTC, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(imp.path())
	if !strings.Contains(string(raw), `"version": 1`) {
		t.Fatalf("file %s", raw)
	}
	newer := []byte(`{"version": 2, "seq": 1, "drafts": [], "applied": []}`)
	if err := writeAtomic(imp.path(), newer); err != nil {
		t.Fatal(err)
	}
	if err := imp.View(func(*State) error { return nil }); err == nil || !strings.Contains(err.Error(), "更新版本") {
		t.Fatalf("newer file: %v", err)
	}
	if err := imp.Update(func(*State, time.Time) error { return nil }); err == nil {
		t.Fatal("newer file overwritten")
	}
	if got, _ := os.ReadFile(imp.path()); string(got) != string(newer) {
		t.Fatal("newer file changed")
	}
}
