package imports

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
	"github.com/nwverse/persona-extensions/extensions/household/internal/ops"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

// Prepare plans a draft for writing, exactly as it was last shown: with its answers and edits, against the records as
// they are. It refuses a draft whose plan the records have changed since it was shown (any line that would now promise
// something else), a plan with questions left, and a plan that writes nothing; the error says what to do.
func Prepare(h *model.Home, d Draft, today model.Day) (Plan, error) {
	plan, err := Make(h, d.Input, d.Edits, today)
	if err != nil {
		return Plan{}, err
	}
	shown := d.Plan.Sigs()
	var stale []string
	if len(shown) != len(plan.Steps) {
		return Plan{}, fmt.Errorf("预览之后家里的记录变了，为了不写错，这次没写入。请重新预览（household_import_preview 带 draft_id）" +
			"或在页面上点「重新算一遍」，再让用户确认")
	}
	for _, s := range plan.Steps {
		if was, ok := shown[s.Line]; !ok || was != s.sig() {
			stale = append(stale, fmt.Sprintf("第 %d 行「%s」", s.Line, s.Title))
		}
	}
	if len(stale) > 0 {
		return Plan{}, fmt.Errorf("预览之后家里的记录变了（%s），为了不写错，这次没写入。请重新预览（household_import_preview）"+
			"或在页面上点「重新算一遍」，再让用户确认", strings.Join(stale, "、"))
	}
	if asks := plan.Asks(); len(asks) > 0 {
		var parts []string
		for _, s := range asks {
			parts = append(parts, fmt.Sprintf("第 %d 行「%s」：%s", s.Line, s.Title, s.Ask.Text))
		}
		return Plan{}, fmt.Errorf("还有要用户定的地方，这次没写入：%s。把用户的回答用 household_import_preview 带 draft_id 和 edits"+
			"（match 填编号或 new，或改 qty、unit，或 skip）交给预览，念新的预览给用户，再按新的 revision 写入", strings.Join(parts, "；"))
	}
	if plan.Totals.Changes == 0 {
		return Plan{}, errors.New("这次导入没有要写入的东西：每一行都已经记着、已经在采购清单上或被跳过了")
	}
	return plan, nil
}

// Write applies the plan to b in one transaction, through the operations of the ops package, and returns every record
// it changed with what it was before and after. b changes only when every step succeeded.
func Write(b *kernel.Base, h *model.Home, plan Plan, now time.Time, loc *time.Location) ([]Touch, error) {
	nb := *b
	nb.Spaces = slices.Clone(b.Spaces)
	w := &writer{b: &nb, h: h, now: now, loc: loc, before: map[string]json.RawMessage{}, places: map[string]string{}}
	for _, s := range plan.Steps {
		if !s.Writes() {
			continue
		}
		if err := w.step(s); err != nil {
			return nil, fmt.Errorf("第 %d 行「%s」：%w", s.Line, s.Title, err)
		}
	}
	var out []Touch
	for _, id := range w.order {
		e, ok := entity(&nb, id)
		if !ok {
			continue
		}
		after, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		out = append(out, Touch{ID: id, Type: e.Type, Title: e.Title, Before: w.before[id], After: after})
	}
	*b = nb
	return out, nil
}

// writer applies the steps of one plan to a working base.
type writer struct {
	b      *kernel.Base
	h      *model.Home // the records as planned
	now    time.Time
	loc    *time.Location
	order  []string                   // the records touched, in the order first touched
	before map[string]json.RawMessage // what each existing record was; none for a created one
	places map[string]string          // places created, by key
}

// touch notes a record before it changes.
func (w *writer) touch(id string) error {
	if slices.Contains(w.order, id) {
		return nil
	}
	e, ok := entity(w.b, id)
	if !ok {
		return fmt.Errorf("没有记录「%s」", id)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	w.order = append(w.order, id)
	w.before[id] = raw
	return nil
}

// created notes a record the import made.
func (w *writer) created(id string) { w.order = append(w.order, id) }

func (w *writer) save(in kernel.EntityInput) (*kernel.Entity, error) {
	if in.ID != "" {
		if err := w.touch(in.ID); err != nil {
			return nil, err
		}
	}
	e, err := ops.Save(w.b, in, w.now, w.loc)
	if err != nil {
		return nil, err
	}
	if in.ID == "" {
		w.created(e.ID)
	}
	return e, nil
}

// place is the id of the step's place, creating it the first time a new one is named.
func (w *writer) place(r *Ref) (string, error) {
	if r == nil {
		return "", nil
	}
	if r.ID != "" {
		return r.ID, nil
	}
	key := titleKey(r.Title)
	if id, ok := w.places[key]; ok {
		return id, nil
	}
	title := r.Title
	e, err := w.save(kernel.EntityInput{Type: model.TPlace, Title: &title})
	if err != nil {
		return "", err
	}
	w.places[key] = e.ID
	return e.ID, nil
}

// purchase closes a shopping list entry as bought; with an amount, the entry gets it first, so that its item is
// stocked with what was bought.
func (w *writer) purchase(buyID string, amount *float64) error {
	if err := w.touch(buyID); err != nil {
		return err
	}
	if amount != nil {
		if _, err := ops.Save(w.b, kernel.EntityInput{ID: buyID, Values: map[string]any{"amount": *amount}}, w.now, w.loc); err != nil {
			return err
		}
	}
	if bu, ok := w.buy(buyID); ok && bu.ItemID != "" {
		if err := w.touch(bu.ItemID); err != nil {
			return err
		}
	}
	_, err := ops.Purchase(w.b, buyID, true, w.now, w.loc)
	return err
}

func (w *writer) buy(id string) (model.Buy, bool) {
	i := slices.IndexFunc(w.h.Buys, func(b model.Buy) bool { return b.ID == id })
	if i < 0 {
		return model.Buy{}, false
	}
	return w.h.Buys[i], true
}

// extras writes the values an existing item lacked.
func (w *writer) extras(s Step) error {
	vals := map[string]any{}
	if s.SetUnit != "" {
		vals["unit"] = s.SetUnit
	}
	if s.SetCategory != "" {
		vals["category"] = s.SetCategory
	}
	if s.SetPlace != nil {
		id, err := w.place(s.SetPlace)
		if err != nil {
			return err
		}
		vals["place"] = []string{id}
	}
	if s.SetExpires != "" {
		vals["expires"] = string(s.SetExpires)
	}
	if len(vals) == 0 {
		return nil
	}
	_, err := w.save(kernel.EntityInput{ID: s.Item.ID, Values: vals})
	return err
}

func (w *writer) step(s Step) error {
	switch s.Action {
	case ActNewItem:
		vals := map[string]any{}
		if s.Qty != nil {
			vals["qty"] = *s.Qty
		}
		if s.Unit != "" {
			vals["unit"] = s.Unit
		}
		if s.Category != "" {
			vals["category"] = s.Category
		}
		if s.Place != nil {
			id, err := w.place(s.Place)
			if err != nil {
				return err
			}
			vals["place"] = []string{id}
		}
		if s.Expires != "" {
			vals["expires"] = string(s.Expires)
		}
		title := cut(s.Title, kernel.MaxTitle)
		if _, err := w.save(kernel.EntityInput{Type: model.TItem, Title: &title, Values: vals}); err != nil {
			return err
		}
		if s.Buy != nil {
			return w.purchase(s.Buy.ID, nil)
		}
		return nil
	case ActAddToItem:
		if err := w.touch(s.Item.ID); err != nil {
			return err
		}
		bu, linked := model.Buy{}, false
		if s.Buy != nil {
			bu, _ = w.buy(s.Buy.ID)
			linked = bu.ItemID == s.Item.ID
		}
		switch {
		case linked && !s.Track:
			if err := w.purchase(s.Buy.ID, s.Qty); err != nil {
				return err
			}
		default:
			if s.Buy != nil {
				if err := w.purchase(s.Buy.ID, nil); err != nil {
					return err
				}
			}
			if _, err := ops.AddStock(w.b, s.Item.ID, *s.Qty, w.now, w.loc); err != nil {
				return err
			}
		}
		return w.extras(s)
	case ActSetCount:
		if _, err := w.save(kernel.EntityInput{ID: s.Item.ID, Values: map[string]any{"qty": *s.Qty}}); err != nil {
			return err
		}
		return w.extras(s)
	case ActUpdateItem:
		if s.Buy != nil {
			if err := w.purchase(s.Buy.ID, nil); err != nil {
				return err
			}
		}
		return w.extras(s)
	case ActAddToList:
		vals := map[string]any{}
		if s.Qty != nil {
			vals["amount"] = *s.Qty
		}
		if s.Unit != "" {
			vals["unit"] = s.Unit
		}
		if s.Category != "" {
			vals["category"] = s.Category
		}
		if s.Item != nil {
			vals["item"] = []string{s.Item.ID}
		}
		title := cut(s.Title, kernel.MaxTitle)
		_, err := w.save(kernel.EntityInput{Type: model.TBuy, Title: &title, Values: vals})
		return err
	}
	return nil
}

// Blocked is why an import cannot be undone: one sentence per record in the way.
type Blocked struct{ Reasons []string }

func (e *Blocked) Error() string {
	return "这次导入撤销不了：" + strings.Join(e.Reasons, "；") + "。撤销会把这些后来的改动也抹掉，所以没有动；要改回去，请在页面上手动改"
}

// CheckUndo is nil when the import can be undone in b: every record it touched is still exactly as the import left
// it, and nothing else has come to point at a record it created. Otherwise it is a *Blocked saying what is in the way.
func CheckUndo(b *kernel.Base, a *Applied) error {
	if a.Unknown == UnknownUndo {
		return errors.New("上次撤销时被打断，说不清撤销了没有，所以不再提供撤销；请在页面上核对这几样的数量")
	}
	if !a.Undone.IsZero() {
		return errors.New("最近一次导入已经撤销过了")
	}
	var reasons []string
	created := map[string]bool{}
	touched := map[string]bool{}
	for _, t := range a.Touched {
		touched[t.ID] = true
		if t.Before == nil {
			created[t.ID] = true
		}
	}
	for _, t := range a.Touched {
		e, ok := entity(b, t.ID)
		if !ok {
			reasons = append(reasons, "「"+t.Title+"」已经被删掉了")
			continue
		}
		cur, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if !sameJSON(cur, t.After) {
			reasons = append(reasons, fmt.Sprintf("「%s」在导入之后又改过", e.Title))
		}
	}
	if sp := household(b); sp != nil {
		for _, e := range sp.Entities {
			if e == nil || touched[e.ID] {
				continue
			}
			for _, v := range e.Values {
				ids, _ := v.([]string)
				for _, id := range ids {
					if created[id] {
						reasons = append(reasons, fmt.Sprintf("「%s」用到了这次新加的「%s」", e.Title, titleOf(a, id)))
					}
				}
			}
		}
	}
	if len(reasons) > 0 {
		return &Blocked{Reasons: reasons}
	}
	return nil
}

// Undo puts every record the import touched back as it was before, and removes the records it created, in one
// transaction; see CheckUndo for when it refuses. b changes only when it succeeds.
func Undo(b *kernel.Base, a *Applied, now time.Time) error {
	if err := CheckUndo(b, a); err != nil {
		return err
	}
	nb := *b
	nb.Spaces = slices.Clone(b.Spaces)
	for i := len(a.Touched) - 1; i >= 0; i-- {
		t := a.Touched[i]
		if t.Before == nil {
			continue
		}
		var was kernel.Entity
		if err := json.Unmarshal(t.Before, &was); err != nil {
			return fmt.Errorf("「%s」原来的样子读不出来：%w", t.Title, err)
		}
		if _, err := ops.Restore(&nb, was, now); err != nil {
			return fmt.Errorf("「%s」：%w", t.Title, err)
		}
	}
	for i := len(a.Touched) - 1; i >= 0; i-- {
		t := a.Touched[i]
		if t.Before != nil {
			continue
		}
		if err := ops.Remove(&nb, t.ID, now); err != nil {
			return fmt.Errorf("「%s」：%w", t.Title, err)
		}
	}
	*b = nb
	return nil
}

func titleOf(a *Applied, id string) string {
	for _, t := range a.Touched {
		if t.ID == id {
			return t.Title
		}
	}
	return id
}

// sameJSON compares two encodings of a record, whatever their spacing.
func sameJSON(a, b []byte) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return false
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// household is the household space of b, nil when there is none.
func household(b *kernel.Base) *kernel.Space {
	for _, sp := range b.Spaces {
		if sp != nil && sp.ID == model.SpaceID {
			return sp
		}
	}
	return nil
}

// entity is the record of the household space with the id.
func entity(b *kernel.Base, id string) (*kernel.Entity, bool) {
	sp := household(b)
	if sp == nil {
		return nil, false
	}
	for _, e := range sp.Entities {
		if e != nil && e.ID == id {
			return e, true
		}
	}
	return nil, false
}

// cut keeps at most n runes of s.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
