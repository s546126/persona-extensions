package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/household/internal/imports"
	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
	"github.com/nwverse/persona-extensions/extensions/household/internal/ops"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/store"
)

// The interruption points of an import, with what can arrive while its entry is pending. All records are made up.

// diskApp is an app over the records and import drafts in dir, with its own store instances (as a second process
// would have).
func diskApp(dir string) *App {
	a := New(store.Open(dir), testLoc, nil).UseImports(imports.Open(dir))
	a.now = func() time.Time { return testNow }
	a.importsOf().SetClock(func() time.Time { return testNow })
	return a
}

type interruption struct {
	name string
	// crash leaves an entry pending for the draft: the records saved or not.
	crash func(t *testing.T, a *App, dir string, d imports.Draft)
	saved bool
}

func crashBeforeSave(t *testing.T, a *App, _ string, d imports.Draft) {
	t.Helper()
	err := a.st.View(func(b *kernel.Base) error {
		c := *b
		c.Spaces = append([]*kernel.Space{}, b.Spaces...)
		_, _, err := imports.Apply(&c, a.importsOf(), d.ID, d.Rev, false, testNow, testLoc())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func crashAfterSave(t *testing.T, a *App, _ string, d imports.Draft) {
	t.Helper()
	err := a.st.Update(func(b *kernel.Base, now time.Time) error {
		_, _, err := imports.Apply(b, a.importsOf(), d.ID, d.Rev, false, now, testLoc())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// crashFinishFails: the records save fails, and so does the finish that would take the entry back.
func crashFinishFails(t *testing.T, a *App, dir string, d imports.Draft) {
	t.Helper()
	var finish imports.Finish
	err := a.st.View(func(b *kernel.Base) error {
		c := *b
		c.Spaces = append([]*kernel.Space{}, b.Spaces...)
		var err error
		_, finish, err = imports.Apply(&c, a.importsOf(), d.ID, d.Rev, false, testNow, testLoc())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	ferr := finish(false)
	_ = os.Chmod(dir, 0o700)
	if ferr == nil && os.Geteuid() != 0 {
		t.Fatal("the finish was meant to fail")
	}
}

type event struct {
	name string
	run  func(t *testing.T, a, other *App, d, prev imports.Draft)
}

func TestInterruptedImportsKeepTheInvariants(t *testing.T) {
	crashes := []interruption{
		{"before the records save", crashBeforeSave, false},
		{"after the records save, before finish", crashAfterSave, true},
		{"records save and finish both fail", crashFinishFails, false},
	}
	events := []event{
		{"nothing (a read)", func(t *testing.T, a, _ *App, _, _ imports.Draft) { render(t, a, state{}) }},
		{"the user records a use of a touched item", func(t *testing.T, a, _ *App, _, _ imports.Draft) {
			act(t, a, state{}, nil, "use:"+entityID(t, a.st, model.TItem, "鸡蛋"))
		}},
		{"the user adds an item", func(t *testing.T, a, _ *App, _, _ imports.Draft) {
			act(t, a, state{F: "new:item"}, map[string]string{"title": "盐"}, "save")
		}},
		{"a second apply of the draft", func(t *testing.T, a, _ *App, d, _ imports.Draft) {
			act(t, a, state{}, nil, fmt.Sprintf("impapply:%s:%d", d.ID, d.Rev))
		}},
		{"an undo of the previous import", func(t *testing.T, a, _ *App, _, prev imports.Draft) {
			act(t, a, state{}, nil, "impundo:"+prev.ID)
		}},
		{"an assistant preview", func(t *testing.T, a, _ *App, _, _ imports.Draft) {
			if _, err := imports.Preview(a.importsOf(), home(t, a.st), imports.Input{Source: imports.SrcText,
				Lines: []imports.Line{{Title: "酱油", Qty: fp(1), Unit: "瓶"}}}, testLoc(), false); err != nil {
				t.Fatal(err)
			}
		}},
		{"a discard of the draft", func(t *testing.T, a, _ *App, d, _ imports.Draft) {
			act(t, a, state{}, nil, "impdrop:"+d.ID)
		}},
		{"a use recorded by a second process", func(t *testing.T, _, other *App, _, _ imports.Draft) {
			act(t, other, state{}, nil, "use:"+entityID(t, other.st, model.TItem, "鸡蛋"))
		}},
	}
	for _, cr := range crashes {
		for _, ev := range events {
			t.Run(cr.name+" / "+ev.name, func(t *testing.T) {
				dir := t.TempDir()
				a := diskApp(dir)
				if err := a.st.Update(func(b *kernel.Base, _ time.Time) error {
					_, err := ops.LoadSamples(b, testNow, testLoc())
					return err
				}); err != nil {
					t.Fatal(err)
				}
				// A real import first: its undo must survive whatever happens to the next one.
				prev := preview(t, a, imports.Input{Source: imports.SrcReceipt, Lines: []imports.Line{{Title: "番茄", Qty: fp(2), Unit: "个"}}})
				toastHas(t, act(t, a, state{}, nil, fmt.Sprintf("impapply:%s:%d", prev.ID, prev.Rev)), "已写入")
				d := preview(t, a, imports.Input{Source: imports.SrcReceipt, Lines: []imports.Line{
					{Title: "鸡蛋", Qty: fp(6), Unit: "个"}, {Title: "香蕉", Qty: fp(1), Unit: "把"}}})
				cr.crash(t, a, dir, d)

				// A read settles nothing on disk and writes no record.
				recs := snapshot(t, a.st)
				raw, _ := os.ReadFile(filepath.Join(dir, "imports.json"))
				render(t, a, state{})
				raw2, _ := os.ReadFile(filepath.Join(dir, "imports.json"))
				if snapshot(t, a.st) != recs || string(raw) != string(raw2) {
					t.Fatal("a read wrote")
				}

				ev.run(t, a, diskApp(dir), d, prev)
				checkInvariants(t, dir, cr.saved, ev.name, d, prev)
			})
		}
	}
}

// checkInvariants reads the records and the drafts with fresh store instances and checks what must hold whatever the
// interruption and the event.
func checkInvariants(t *testing.T, dir string, saved bool, ev string, d, prev imports.Draft) {
	t.Helper()
	b := diskApp(dir)
	h := home(t, b.st)
	bananas, eggs := 0, -1.0
	for _, it := range h.Items {
		if it.Title == "香蕉" {
			bananas++
		}
		if it.Title == "鸡蛋" {
			eggs = it.Qty
		}
	}
	var st imports.State
	err := b.st.View(func(base *kernel.Base) error {
		return b.importsOf().View(func(s *imports.State) error {
			st = *s
			st.Applied = append([]imports.Applied(nil), s.Applied...)
			st.Drafts = append([]imports.Draft(nil), s.Drafts...)
			st.Settle(base)
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	written := 0
	for _, a := range st.Applied {
		if a.Unknown != "" {
			t.Fatalf("an entry was left undecided: %+v", a)
		}
		if a.Draft == d.ID && !a.Pending {
			written++
		}
	}
	// A draft is applied at most once, and the records are fully old or fully new.
	if written > 1 || bananas > 1 || bananas != written {
		t.Fatalf("written %d times, %d bananas", written, bananas)
	}
	if (written == 1) != (eggs > 0) {
		t.Fatalf("written %d but eggs %v", written, eggs)
	}
	wantWritten := saved || ev == "a second apply of the draft"
	if (written == 1) != wantWritten {
		t.Fatalf("written %d, want %v", written, wantWritten)
	}
	_, draftLeft := st.Draft(d.ID)
	if written == 1 && draftLeft {
		t.Fatal("a written draft is still waiting")
	}
	if written == 0 && !draftLeft && ev != "a discard of the draft" {
		t.Fatal("a draft that was never written is gone")
	}
	// The previous real import's undo is neither lost nor wrongly offered.
	last, ok := st.Last()
	if !ok {
		t.Fatal("no last import")
	}
	var base *kernel.Base
	_ = b.st.View(func(x *kernel.Base) error { c := *x; base = &c; return nil })
	switch {
	case written == 1:
		if last.Draft != d.ID {
			t.Fatalf("last import %s, want the written one", last.Draft)
		}
	case ev == "an undo of the previous import":
		if last.Draft != prev.ID || last.Undone.IsZero() {
			t.Fatalf("the previous import was not undone: %+v", last)
		}
		if tom, _ := h.Item(entityID(t, b.st, model.TItem, "番茄")); tom.Qty != 4 {
			t.Fatalf("tomato %v after undo", tom.Qty)
		}
	default:
		if last.Draft != prev.ID || imports.CheckUndo(base, last) != nil {
			t.Fatalf("the previous import's undo is lost: %+v %v", last, imports.CheckUndo(base, last))
		}
	}
}

// Two apps on one directory act at once: the one lock order (records, then drafts) never deadlocks.
func TestTwoProcessesDoNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	a, b := diskApp(dir), diskApp(dir)
	if err := a.st.Update(func(base *kernel.Base, _ time.Time) error {
		_, err := ops.LoadSamples(base, testNow, testLoc())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var mu sync.Mutex
	wrote := 0
	go func() {
		var wg sync.WaitGroup
		for _, app := range []*App{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 10 {
					d, err := imports.Preview(app.importsOf(), home(t, app.st), imports.Input{Source: imports.SrcText,
						Lines: []imports.Line{{Title: "番茄", Qty: fp(1), Unit: "个"}}}, testLoc(), false)
					if err != nil {
						t.Error(err)
						return
					}
					// A draft the other process overtook is refused as stale: only what was written counts.
					if res := act(t, app, state{}, nil, fmt.Sprintf("impapply:%s:%d", d.ID, d.Rev)); strings.HasPrefix(res.Toast, "已写入") {
						mu.Lock()
						wrote++
						mu.Unlock()
					}
					render(t, app, state{})
				}
			}()
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock")
	}
	if tom, _ := home(t, a.st).Item(entityID(t, a.st, model.TItem, "番茄")); tom.Qty != float64(4+wrote) || wrote == 0 {
		t.Fatalf("tomato %v after %d imports written", tom.Qty, wrote)
	}
}
