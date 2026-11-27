package imports

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

// The calls below are what the page and the assistant's tools do. Each that writes records runs inside an update of
// the records: b is the base being changed. The import store is written in two phases, so that it never claims more
// than the records hold: Apply and UndoLast record the change as pending (the draft stays), and the Finish they return
// confirms it once the records are saved, or takes it back when they are not. A pending change that was never
// finished (the program stopped in between) is settled from the records by Settle, the next time the store is read
// with them.

// ErrChanged refuses a yes given to another revision of a draft than the one stored.
var ErrChanged = errors.New("预览已经变了，请先看一遍新的再确认")

// Finish confirms (ok) or takes back a change recorded as pending, after the records were saved or not.
type Finish func(ok bool) error

// Preview plans the input against the records of h and stores it as a new draft. byUser marks a draft made on the
// page.
func Preview(imp *Store, h *model.Home, in Input, loc *time.Location, byUser bool) (Draft, error) {
	var out Draft
	err := imp.Update(func(st *State, now time.Time) error {
		plan, err := Make(h, in, nil, model.DayOf(now, loc))
		if err != nil {
			return err
		}
		out, err = st.Put(Draft{Input: in, Plan: plan}, now, byUser)
		return err
	})
	return out, err
}

// Replan plans a stored draft again against the records of h, with the edits added to the ones it has, and stores the
// new plan as its next revision: the user then agrees to that one. rev, when above zero, must be the revision the
// request was made for. Only the user (byUser) re-plans a draft made on the page.
func Replan(imp *Store, h *model.Home, id string, rev int, edits []Edit, loc *time.Location, byUser bool) (Draft, error) {
	var out Draft
	err := imp.Update(func(st *State, now time.Time) error {
		d, ok := st.Draft(id)
		if !ok {
			return missing(id)
		}
		if d.ByUser && !byUser {
			return errOnPage
		}
		if rev > 0 && d.Rev != rev {
			return ErrChanged
		}
		all := append(append([]Edit{}, d.Edits...), edits...)
		plan, err := Make(h, d.Input, all, model.DayOf(now, loc))
		if err != nil {
			return err
		}
		d.Edits, d.Plan = all, plan
		out, err = st.Put(d, now, byUser)
		return err
	})
	return out, err
}

// Discard drops a draft, and the entry of an earlier write of it that could not be decided. Nothing in the records
// changes. Only the user (byUser) discards a draft made on the page.
func Discard(imp *Store, id string, byUser bool) (Draft, error) {
	var out Draft
	err := imp.Update(func(st *State, _ time.Time) error {
		d, ok := st.Draft(id)
		if !ok {
			return missing(id)
		}
		if d.ByUser && !byUser {
			return errOnPage
		}
		st.Drop(id)
		st.Applied = slices.DeleteFunc(st.Applied, func(a Applied) bool { return a.Draft == id && a.Unknown == UnknownApply })
		out = d
		return nil
	})
	return out, err
}

// errOnPage refuses the assistant a draft the user made on the page.
var errOnPage = errors.New("这份是用户在家庭管家页面上粘贴的清单，只能由用户在页面上回答、修改、确认或放弃；请让用户去页面上处理")

// errUnknown refuses a draft whose earlier write was interrupted and could not be decided.
var errUnknown = errors.New("这份导入上次写入时被打断，说不清写进去没有，不能再写一次；请在家庭管家页面上核对库存后放弃这份，需要的话重新导入")

// Blocked reports whether the draft's earlier write could not be decided.
func (s *State) Blocked(id string) bool {
	return slices.ContainsFunc(s.Applied, func(a Applied) bool { return a.Draft == id && a.Unknown == UnknownApply })
}

func missing(id string) error {
	return fmt.Errorf("没有草稿「%s」：它可能已经写入、放弃，或者过了 24 小时作废了", id)
}

// Apply writes revision rev of a draft into b (see Prepare and Write) and records the import as pending; the draft
// stays until Finish(true). Nothing is changed when it fails. Only the user (byUser) applies a draft made on the page,
// and a draft whose earlier write could not be decided (Unknown) is not applied again.
func Apply(b *kernel.Base, imp *Store, id string, rev int, byUser bool, now time.Time, loc *time.Location) (Applied, Finish, error) {
	var out Applied
	err := imp.Update(func(st *State, _ time.Time) error {
		st.Settle(b)
		d, ok := st.Draft(id)
		if !ok {
			return missing(id)
		}
		if d.ByUser && !byUser {
			return errOnPage
		}
		if st.Blocked(id) {
			return errUnknown
		}
		if rev != d.Rev {
			return ErrChanged
		}
		h := model.Load(household(b), loc)
		plan, err := Prepare(h, d, model.DayOf(now, loc))
		if err != nil {
			return err
		}
		touched, err := Write(b, h, plan, now, loc)
		if err != nil {
			return err
		}
		out = Applied{Draft: d.ID, Source: d.Input.Source, At: now, Totals: plan.Totals, Touched: touched, Says: []string{},
			Pending: true}
		for _, s := range plan.Steps {
			if s.Writes() {
				out.Says = append(out.Says, s.Says)
			}
		}
		st.Record(out)
		return nil
	})
	if err != nil {
		return Applied{}, nil, err
	}
	finish := func(ok bool) error {
		return imp.Update(func(st *State, _ time.Time) error {
			i := pendingIndex(st, out.Draft, out.At)
			if i < 0 {
				return nil
			}
			if ok {
				st.Applied[i].Pending = false
				st.Drop(out.Draft)
			} else {
				st.Applied = append(st.Applied[:i], st.Applied[i+1:]...)
			}
			return nil
		})
	}
	out.Pending = false
	return out, finish, nil
}

func pendingIndex(st *State, draft string, at time.Time) int {
	for i := len(st.Applied) - 1; i >= 0; i-- {
		if a := st.Applied[i]; a.Pending && a.Draft == draft && a.At.Equal(at) {
			return i
		}
	}
	return -1
}

// UndoLast undoes the import applied last (see Undo for when it refuses) and records the undo as pending until
// Finish. want, when set, must be the draft id of that import, so that a stale request does not undo a newer one.
func UndoLast(b *kernel.Base, imp *Store, want string, now time.Time) (Applied, Finish, error) {
	var out Applied
	err := imp.Update(func(st *State, _ time.Time) error {
		st.Settle(b)
		last, ok := st.Last()
		if !ok {
			return errors.New("还没有导入过东西，没有可以撤销的")
		}
		if want != "" && last.Draft != want {
			return fmt.Errorf("最近一次导入是「%s」，不是「%s」；只能撤销最近一次", last.Draft, want)
		}
		if err := Undo(b, last, now); err != nil {
			return err
		}
		last.Undone, last.UndoPending = now, true
		out = *last
		return nil
	})
	if err != nil {
		return Applied{}, nil, err
	}
	finish := func(ok bool) error {
		return imp.Update(func(st *State, _ time.Time) error {
			for i := range st.Applied {
				a := &st.Applied[i]
				if a.Draft == out.Draft && a.At.Equal(out.At) && a.UndoPending {
					a.UndoPending = false
					if !ok {
						a.Undone = time.Time{}
					}
				}
			}
			return nil
		})
	}
	out.UndoPending = false
	return out, finish, nil
}

// Settle decides every pending change from the records of b, exactly: the records an import touched are compared with
// the states it stored. An import is written when every one of them is as the import left it (it then loses its
// draft), and never written when every one is as it was before (a created one absent): its entry goes and its draft
// stays. An undo is written when every created record is gone and every changed one is back as it was before, and not
// written when every one is still as the import left it. Settle runs at the start of every write of the records,
// under their lock and before anything else changes (see SettleRecords), when the records are exactly the state
// before or after the interrupted save; anything else (a program of another version wrote in between) is not guessed:
// the entry is marked Unknown, which blocks its draft and offers no undo, and is said on the page.
func (s *State) Settle(b *kernel.Base) {
	keep := s.Applied[:0]
	for _, a := range s.Applied {
		if a.Pending {
			a.Pending = false
			switch {
			case allAfter(b, a):
				s.Drop(a.Draft)
			case allBefore(b, a):
				continue
			default:
				a.Unknown = UnknownApply
			}
		}
		if a.UndoPending {
			a.UndoPending = false
			switch {
			case undone(b, a):
			case allAfter(b, a):
				a.Undone = time.Time{}
			default:
				// Not known whether it was undone: it is not said to be.
				a.Undone = time.Time{}
				a.Unknown = UnknownUndo
			}
		}
		keep = append(keep, a)
	}
	s.Applied = keep
}

// Unknown states of an entry whose interrupted save could not be decided.
const (
	UnknownApply = "apply" // not known whether the import was written: its draft cannot be applied
	UnknownUndo  = "undo"  // not known whether the undo was written: no undo is offered
)

// pending reports whether an entry waits to be settled.
func (s *State) pending() bool {
	for _, a := range s.Applied {
		if a.Pending || a.UndoPending {
			return true
		}
	}
	return false
}

// SettleRecords settles the pending changes against b and saves the store when there were any. Every write of the
// records calls it first, inside the records' update (records lock, then the store's: the one lock order). A store
// that cannot be read may hold an entry that is still pending, which this write could make undecidable, so the write
// is refused (ErrUnreadable) until the file is dealt with; reading the records goes on working.
func (s *Store) SettleRecords(b *kernel.Base) error {
	any := false
	if err := s.View(func(st *State) error { any = st.pending(); return nil }); err != nil {
		return &UnreadableError{Err: err}
	}
	if !any {
		return nil
	}
	return s.Update(func(st *State, _ time.Time) error {
		st.Settle(b)
		return nil
	})
}

// allAfter reports whether every record the import touched is exactly as the import left it.
func allAfter(b *kernel.Base, a Applied) bool {
	for _, t := range a.Touched {
		e, ok := entity(b, t.ID)
		if !ok {
			return false
		}
		cur, err := json.Marshal(e)
		if err != nil || !sameJSON(cur, t.After) {
			return false
		}
	}
	return len(a.Touched) > 0
}

// allBefore reports whether every record the import touched is exactly as it was before: a created one absent, a
// changed one unchanged.
func allBefore(b *kernel.Base, a Applied) bool {
	for _, t := range a.Touched {
		e, ok := entity(b, t.ID)
		if t.Before == nil {
			if ok {
				return false
			}
			continue
		}
		if !ok {
			return false
		}
		cur, err := json.Marshal(e)
		if err != nil || !sameJSON(cur, t.Before) {
			return false
		}
	}
	return true
}

// undone reports whether the import's undo is in the records: every record it created gone, every one it changed back
// to what it was (its stamp is the undo's).
func undone(b *kernel.Base, a Applied) bool {
	for _, t := range a.Touched {
		e, ok := entity(b, t.ID)
		if t.Before == nil {
			if ok {
				return false
			}
			continue
		}
		if !ok || !sameContent(e, t.Before) {
			return false
		}
	}
	return true
}

// sameContent reports whether the record has the title, stage and values of the stored one, whenever it was saved.
func sameContent(e *kernel.Entity, raw []byte) bool {
	var was kernel.Entity
	if json.Unmarshal(raw, &was) != nil {
		return false
	}
	x, _ := json.Marshal([]any{e.Title, e.Status, e.Values})
	y, _ := json.Marshal([]any{was.Title, was.Status, was.Values})
	return string(x) == string(y)
}

// Summary is a plan in plain Chinese, one line per step, then what was inferred, what needs an answer and the totals:
// what the assistant reads back to the user.
func Summary(src string, p Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "从%s读到 %d 样：\n", SourceLabel(src), len(p.Steps))
	var inferred, asks []string
	for _, s := range p.Steps {
		fmt.Fprintf(&b, "%d. %s\n", s.Line, s.Says)
		if w := InferredWords(s); w != "" && s.Writes() {
			inferred = append(inferred, fmt.Sprintf("第 %d 行的%s", s.Line, w))
		}
		if s.Ask != nil {
			asks = append(asks, fmt.Sprintf("第 %d 行「%s」", s.Line, s.Title))
		}
	}
	for _, u := range p.Unread {
		fmt.Fprintf(&b, "没读懂：「%s」（%s）\n", u.Text, u.Why)
	}
	if len(inferred) > 0 {
		fmt.Fprintf(&b, "推测的（可以改）：%s。\n", strings.Join(inferred, "，"))
	}
	if len(asks) > 0 {
		fmt.Fprintf(&b, "要你定：%s。\n", strings.Join(asks, "、"))
	}
	b.WriteString("合计：" + TotalsText(p.Totals))
	return b.String()
}

// TotalsText is what a plan changes, in one sentence.
func TotalsText(t Totals) string {
	var parts []string
	add := func(n int, f string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf(f, n))
		}
	}
	add(t.NewItems, "新加 %d 件")
	add(t.AddedTo, "给 %d 件加数量")
	add(t.Counted, "%d 件按看到的记数量")
	add(t.Lowered, "其中 %d 件会比现在记的少")
	add(t.Updated, "%d 件补上信息")
	add(t.Bought, "%d 条采购标成已买")
	add(t.ToList, "%d 样加到采购清单")
	add(t.NewPlaces, "新建 %d 个位置")
	add(t.OnList, "%d 样已经在清单上")
	add(t.Unchanged, "%d 样已经记着")
	add(t.Skipped, "%d 行不记")
	add(t.Questions, "%d 行要你定")
	add(t.Unread, "%d 行没读懂")
	if len(parts) == 0 {
		parts = append(parts, "没有要改的")
	}
	out := strings.Join(parts, "，") + "。"
	if t.PriceTotal != nil {
		out += fmt.Sprintf("这几行的金额合计 %s 元（金额不记到库存里）。", num(*t.PriceTotal))
	}
	return out
}

// ConfirmText is the sentence the page asks before writing a plan.
func ConfirmText(t Totals) string {
	return fmt.Sprintf("确认写入？会改动 %d 样：%s写入后可以撤销。", t.Changes, TotalsText(t))
}

// UnreadableError refuses a write of the records while the import store cannot be read.
type UnreadableError struct{ Err error }

func (e *UnreadableError) Error() string {
	return "导入记录读不出来，先处理好再改：" + e.Err.Error() + "。为了不把一次导入写两遍，这次没有改动任何记录。" +
		"请把这个文件修好；或者确认没有写到一半的导入后，把它改个名字挪开（家庭管家会重新开始记导入），再试一次。看记录照常可以。"
}

func (e *UnreadableError) Unwrap() error { return e.Err }

// Acknowledge marks the last import's undecided undo as seen by the user: the page stops showing it. Nothing in the
// records changes, and the import is still not offered for undo.
func Acknowledge(imp *Store, draft string) error {
	return imp.Update(func(st *State, _ time.Time) error {
		last, ok := st.Last()
		if !ok || last.Draft != draft || last.Unknown != UnknownUndo {
			return errors.New("这一条已经不用处理了")
		}
		last.Seen = true
		return nil
	})
}
