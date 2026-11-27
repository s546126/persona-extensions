// Package imports turns a list of things the user has (read off a receipt, a photo or a text by the assistant, or
// pasted on the page) into a draft of changes to the household records, applies a draft the user agreed to, and undoes
// the last one. It never reads an image and never calls a model: it gets lines with names and numbers, and every value
// it adds on its own (a category, a place, a shelf life, a unit) is marked as inferred.
//
// A draft is planned against the records as they are: each line becomes one step (add a new item, add to an item
// already there, count it, put it on the shopping list, or a question when it is not certain what is meant). Applying
// runs the steps through the ops package in one transaction and records what each record was before, so that the
// import can be undone while nothing it touched has changed since.
package imports

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
	"github.com/nwverse/persona-extensions/extensions/household/internal/rules"
)

// Where the lines come from. Paste is the page's own 粘贴清单; the tool takes the others.
const (
	SrcReceipt = "receipt"
	SrcFridge  = "fridge_photo"
	SrcShelf   = "shelf_photo"
	SrcOrder   = "order_screenshot"
	SrcText    = "text"
	SrcOther   = "other"
	SrcPaste   = "paste"
)

// Sources are the sources the assistant's tool accepts, in the order its schema lists them.
var Sources = []string{SrcReceipt, SrcFridge, SrcShelf, SrcOrder, SrcText, SrcOther}

// SourceLabel is the source in the user's words.
func SourceLabel(src string) string {
	switch src {
	case SrcReceipt:
		return "小票"
	case SrcFridge:
		return "冰箱照片"
	case SrcShelf:
		return "柜子照片"
	case SrcOrder:
		return "订单截图"
	case SrcText:
		return "文字"
	case SrcPaste:
		return "粘贴的清单"
	}
	return "其他"
}

// DraftLabel is where a draft came from in the user's words. A draft made on the page is 粘贴的清单; the source string
// alone does not make one.
func DraftLabel(d Draft) string {
	switch {
	case d.ByUser:
		return SourceLabel(SrcPaste)
	case d.Input.Source == SrcPaste:
		return SourceLabel(SrcOther)
	}
	return SourceLabel(d.Input.Source)
}

// Targets: a line goes into the stock or onto the shopping list.
const (
	ToStock    = "stock"
	ToShopping = "shopping"
)

// Modes of a stock import: add what was bought to what is there, or count what is there (a photo of the fridge
// shows what is in it, not what was added).
const (
	ModeAdd   = "add"
	ModeCount = "count"
)

// DefaultMode is the mode a source implies: a photo of the fridge or a shelf counts, everything else adds.
func DefaultMode(src string) string {
	if src == SrcFridge || src == SrcShelf {
		return ModeCount
	}
	return ModeAdd
}

// Line is one thing as the assistant or the paste field gives it. Only Title is needed.
type Line struct {
	Title    string   `json:"title"`
	Qty      *float64 `json:"qty,omitempty"`
	Unit     string   `json:"unit,omitempty"`
	Category string   `json:"category,omitempty"`
	Place    string   `json:"place,omitempty"`
	Expires  string   `json:"expires,omitempty"`
	BoughtOn string   `json:"bought_on,omitempty"`
	Price    *float64 `json:"price,omitempty"`
	Target   string   `json:"target,omitempty"`
}

// Unread is a pasted line the parser could not read, kept to be shown as it was.
type Unread struct {
	Line int    `json:"line"`
	Text string `json:"text"`
	Why  string `json:"why"`
}

// Input is what one import was asked with.
type Input struct {
	Source string   `json:"source"`
	Note   string   `json:"note,omitempty"`
	Target string   `json:"target,omitempty"` // the default target of the lines
	Mode   string   `json:"mode,omitempty"`
	Lines  []Line   `json:"lines"`
	Unread []Unread `json:"unread,omitempty"`
}

// Edit is a correction or an answer the user gave for one line (numbered from 1, as the preview numbers them). A set
// field replaces the line's; Match answers a question: the id of the recorded item (or, on the shopping list, the
// purchase) the line is, or "new" for a new one. Skip leaves the line out.
type Edit struct {
	Line     int      `json:"line"`
	Title    *string  `json:"title,omitempty"`
	Qty      *float64 `json:"qty,omitempty"`
	Unit     *string  `json:"unit,omitempty"`
	Category *string  `json:"category,omitempty"`
	Place    *string  `json:"place,omitempty"`
	Expires  *string  `json:"expires,omitempty"`
	Target   *string  `json:"target,omitempty"`
	Match    string   `json:"match,omitempty"`
	Skip     bool     `json:"skip,omitempty"`
}

// MatchNew answers a question with "a new one".
const MatchNew = "new"

// Step actions.
const (
	ActNewItem    = "new_item"    // a new item
	ActAddToItem  = "add_to_item" // more of an item already there
	ActSetCount   = "set_count"   // an item already there, counted
	ActUpdateItem = "update_item" // an item already there gets a date, unit, category or place it lacked
	ActNothing    = "nothing"     // an item already there, nothing to change
	ActAddToList  = "add_to_list" // a new purchase on the shopping list
	ActOnList     = "on_list"     // already on the shopping list
	ActAsk        = "ask"         // the user has to answer first
	ActSkip       = "skip"        // left out
)

// Ref is a record by id and title.
type Ref struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Choice is one answer to a question: the edit it makes, and its words.
type Choice struct {
	Label string   `json:"label"`
	Match string   `json:"match,omitempty"`
	Qty   *float64 `json:"qty,omitempty"`
	Skip  bool     `json:"skip,omitempty"`
}

// Question kinds.
const (
	AskSameItem  = "same_item"  // a recorded item looks like this one, or several have its name
	AskUnit      = "unit"       // the recorded item is counted in another unit
	AskQty       = "qty"        // no quantity, for an item that is counted
	AskSameEntry = "same_entry" // a purchase on the list looks like this one
	AskDuplicate = "duplicate"  // another line of the import is the same thing in a unit it cannot be added to
)

// Question is what the user has to decide about a line before it can be written.
type Question struct {
	Kind    string   `json:"kind"`
	Text    string   `json:"text"`
	Choices []Choice `json:"choices"`
}

// Step is what happens to one line, or to several lines of the same thing merged.
type Step struct {
	Line     int       `json:"line"`
	From     []int     `json:"from"`
	Title    string    `json:"title"`
	Target   string    `json:"target"`
	Action   string    `json:"action"`
	Qty      *float64  `json:"qty,omitempty"`
	Unit     string    `json:"unit,omitempty"`
	Before   *float64  `json:"before,omitempty"`
	After    *float64  `json:"after,omitempty"`
	Item     *Ref      `json:"item,omitempty"`
	Buy      *Ref      `json:"buy,omitempty"`
	Category string    `json:"category,omitempty"`
	Place    *Ref      `json:"place,omitempty"` // ID "" for a place that will be created
	Expires  model.Day `json:"expires,omitempty"`
	Price    *float64  `json:"price,omitempty"`
	OldGone  bool      `json:"old_batch_expired,omitempty"` // the recorded batch had expired: only the new one counts
	Lowers   bool      `json:"lowers,omitempty"`            // a count below the current estimate: the quantity goes down
	Keeps    int       `json:"usual_days,omitempty"`        // the usual shelf life the table suggests, never stored as a date
	Inferred []string  `json:"inferred"`
	Notes    []string  `json:"notes"`
	Ask      *Question `json:"question,omitempty"`
	Says     string    `json:"says"`

	// What the step writes on an item that is there, besides the quantity: values the item lacks, and a given expiry
	// date (OldExpires is the date it replaces).
	SetUnit     string    `json:"set_unit,omitempty"`
	SetCategory string    `json:"set_category,omitempty"`
	SetPlace    *Ref      `json:"set_place,omitempty"`
	SetExpires  model.Day `json:"set_expires,omitempty"`
	OldExpires  model.Day `json:"old_expires,omitempty"`
	Track       bool      `json:"starts_tracking,omitempty"` // the item had no quantity and gets one
}

// Writes reports whether applying the step changes a record.
func (s Step) Writes() bool {
	switch s.Action {
	case ActNewItem, ActAddToItem, ActSetCount, ActUpdateItem, ActAddToList:
		return true
	}
	return false
}

// sig is everything the step promises: what it does, to which records, and the values it writes (the quantity and
// unit, the resulting quantity, the dates, the place, the category, whether the old batch counts as gone or the
// quantity goes down). A draft whose steps would promise anything else against the records as they are now is out of
// date. The estimate before the change is not part of it: it is said for information, and the result is.
func (s Step) sig() string {
	ref := func(r *Ref) string {
		if r == nil {
			return ""
		}
		return r.ID + "/" + r.Title
	}
	f := func(p *float64) string {
		if p == nil {
			return ""
		}
		return num(*p)
	}
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	return strings.Join([]string{s.Action, s.Target, ref(s.Item), ref(s.Buy), ref(s.Place), f(s.Qty), s.Unit, f(s.After),
		string(s.Expires), s.Category, s.SetUnit, s.SetCategory, ref(s.SetPlace), string(s.SetExpires),
		string(s.OldExpires), b(s.OldGone), b(s.Track), b(s.Lowers)}, "|")
}

// Totals count the steps of a plan.
type Totals struct {
	Lines      int      `json:"lines"`
	NewItems   int      `json:"new_items"`
	AddedTo    int      `json:"added_to"`
	Counted    int      `json:"counted"`
	Updated    int      `json:"updated"`
	ToList     int      `json:"to_list"`
	OnList     int      `json:"on_list"`
	Bought     int      `json:"marked_bought"`
	NewPlaces  int      `json:"new_places"`
	Unchanged  int      `json:"unchanged"`
	Skipped    int      `json:"skipped"`
	Questions  int      `json:"questions"`
	Lowered    int      `json:"lowered"`
	Inferred   int      `json:"with_inferred"`
	Unread     int      `json:"unread"`
	Changes    int      `json:"changes"`
	PriceTotal *float64 `json:"price_total,omitempty"`
}

// Plan is an input planned against the records.
type Plan struct {
	Steps  []Step   `json:"steps"`
	Totals Totals   `json:"totals"`
	Unread []Unread `json:"unread"`
}

// Sigs is the decision of every step, by line.
func (p Plan) Sigs() map[int]string {
	out := map[int]string{}
	for _, s := range p.Steps {
		out[s.Line] = s.sig()
	}
	return out
}

// Asks are the steps still waiting for an answer.
func (p Plan) Asks() []Step {
	var out []Step
	for _, s := range p.Steps {
		if s.Action == ActAsk {
			out = append(out, s)
		}
	}
	return out
}

// Check refuses an input that cannot be planned: no lines, a line without a name, a quantity that is not above zero, a
// date that is not a date, an unknown source, target or mode. The error names the line.
func Check(in Input) error {
	if in.Source != SrcPaste && !slices.Contains(Sources, in.Source) {
		return fmt.Errorf("来源「%s」不认识，可用的有 %s", in.Source, strings.Join(Sources, "、"))
	}
	if in.Target != "" && in.Target != ToStock && in.Target != ToShopping {
		return fmt.Errorf("default_target 只能是 stock 或 shopping")
	}
	if in.Mode != "" && in.Mode != ModeAdd && in.Mode != ModeCount {
		return fmt.Errorf("mode 只能是 add 或 count")
	}
	if len([]rune(in.Note)) > NoteMost {
		return fmt.Errorf("备注太长（最多 %d 个字）", NoteMost)
	}
	if len(in.Lines) == 0 && len(in.Unread) == 0 {
		return fmt.Errorf("没有要记的东西：lines 是空的")
	}
	if len(in.Lines) > MaxLines {
		return fmt.Errorf("一次最多 %d 行，这次有 %d 行；请分几次记", MaxLines, len(in.Lines))
	}
	for i, l := range in.Lines {
		if err := checkLine(l); err != nil {
			return fmt.Errorf("第 %d 行：%w", i+1, err)
		}
	}
	return nil
}

// MaxLines is the most lines one import has.
const MaxLines = 60

// Limits of a line, checked when it is previewed, so that a draft that previews can always be written.
const (
	titleMost = 100
	placeMost = 40
	unitMost  = 10
	qtyLeast  = 0.001 // quantities are kept to three decimals
	qtyMost   = 1e6
	NoteMost  = 200
)

func checkLine(l Line) error {
	t := cleanTitle(l.Title)
	if t == "" {
		return fmt.Errorf("没有名称")
	}
	if len([]rune(t)) > titleMost {
		return fmt.Errorf("名称太长（最多 %d 个字）", titleMost)
	}
	if l.Qty != nil && !(*l.Qty >= qtyLeast && *l.Qty < qtyMost) {
		return fmt.Errorf("数量需要在 %s 到 %s 之间", num(qtyLeast), num(qtyMost))
	}
	if len([]rune(strings.TrimSpace(l.Unit))) > unitMost {
		return fmt.Errorf("单位太长（最多 %d 个字）", unitMost)
	}
	if len([]rune(cleanTitle(l.Place))) > placeMost {
		return fmt.Errorf("位置名称太长（最多 %d 个字）", placeMost)
	}
	if len([]rune(l.Category)) > 20 {
		return fmt.Errorf("分类太长")
	}
	if l.Price != nil && !(*l.Price >= 0 && *l.Price < 1e7) {
		return fmt.Errorf("价格不对")
	}
	for name, d := range map[string]string{"expires": l.Expires, "bought_on": l.BoughtOn} {
		if d == "" {
			continue
		}
		if _, ok := model.ParseDay(d); !ok {
			return fmt.Errorf("%s「%s」不是 YYYY-MM-DD 的日期", name, d)
		}
	}
	if l.Target != "" && l.Target != ToStock && l.Target != ToShopping {
		return fmt.Errorf("target 只能是 stock 或 shopping")
	}
	return nil
}

// withEdits is the lines with the edits applied, and the lines left out. Edits for a line that is not there are
// refused.
func withEdits(lines []Line, edits []Edit) ([]Line, map[int]string, map[int]bool, error) {
	out := slices.Clone(lines)
	match := map[int]string{}
	skip := map[int]bool{}
	for _, e := range edits {
		if e.Line < 1 || e.Line > len(out) {
			return nil, nil, nil, fmt.Errorf("没有第 %d 行", e.Line)
		}
		l := &out[e.Line-1]
		if e.Title != nil {
			l.Title = *e.Title
		}
		if e.Qty != nil {
			q := *e.Qty
			l.Qty = &q
		}
		if e.Unit != nil {
			l.Unit = *e.Unit
		}
		if e.Category != nil {
			l.Category = *e.Category
		}
		if e.Place != nil {
			l.Place = *e.Place
		}
		if e.Expires != nil {
			l.Expires = *e.Expires
		}
		if e.Target != nil {
			l.Target = *e.Target
		}
		if e.Match != "" {
			match[e.Line] = e.Match
		}
		if e.Skip {
			skip[e.Line] = true
		}
		if err := checkLine(*l); err != nil {
			return nil, nil, nil, fmt.Errorf("第 %d 行改过以后：%w", e.Line, err)
		}
	}
	return out, match, skip, nil
}

// group is lines of the same thing in one import, merged. dupOf is the first line of the same thing that this line
// could not be merged into (another unit that does not convert, or a quantity on only one of them), with why.
type group struct {
	line     Line
	from     []int
	notes    []string
	dupOf    int
	why      string
	dupTitle string // the title of the line it could not be merged into
	key      string // what the lines resolve to: a recorded item or purchase, or a name
	answer   string // the match the group is planned with
}

// merge groups the lines that are the same thing for the same target, after matching: resolve gives each line the key
// of what it is (the recorded item or purchase it was matched to, by name or by the user's answer, or else its name)
// and the match to plan it with. Lines with the same key are one step: their quantities add up when they share a unit,
// after converting a weight or a volume into the unit of the first line (or when none has a quantity), the earliest
// date is kept and the prices add up. A line of the same thing that cannot be added (a unit that does not convert, or
// a quantity on only one of the lines) is kept apart and becomes a question: written as a second step, it would
// overwrite or double the first.
func merge(lines []Line, target func(Line) string, skip map[int]bool, resolve func(n int, l Line) (string, string)) ([]group, []int) {
	var gs []group
	var skipped []int
	for i, l := range lines {
		n := i + 1
		if skip[n] {
			skipped = append(skipped, n)
			continue
		}
		l.Title = cleanTitle(l.Title)
		l.Unit = normUnit(l.Unit)
		l.Target = target(l)
		key, answer := resolve(n, l)
		joined := false
		for gi := range gs {
			g := &gs[gi]
			if g.line.Target != l.Target || g.key != key {
				continue
			}
			if g.dupOf != 0 {
				continue
			}
			bothQty := (g.line.Qty == nil) == (l.Qty == nil)
			sameUnit := g.line.Unit == l.Unit
			add := 0.0
			if bothQty && g.line.Qty == nil {
				sameUnit = true // nothing to add up
			}
			if bothQty && g.line.Qty != nil {
				if q, ok := convert(*l.Qty, l.Unit, g.line.Unit); ok && (sameUnit || l.Unit != "" && g.line.Unit != "") {
					add, sameUnit = q, true
					if l.Unit != g.line.Unit {
						g.notes = append(g.notes, fmt.Sprintf("第 %d 行的 %s%s 换算成 %s%s 合在一起", n, num(*l.Qty), l.Unit, num(q), g.line.Unit))
					}
				}
			}
			if !sameUnit || !bothQty {
				why := fmt.Sprintf("单位不同（%s / %s），没法合在一起", unitName(g.line.Unit), unitName(l.Unit))
				if !bothQty {
					why = "一行写了数量、一行没写，没法合在一起"
				}
				gs = append(gs, group{line: l, from: []int{n}, dupOf: g.from[0], dupTitle: g.line.Title, why: why, key: "dup", answer: answer})
				joined = true
				break
			}
			g.from = append(g.from, n)
			if l.Qty != nil {
				q := round3(*g.line.Qty + add)
				g.line.Qty = &q
			}
			if g.line.Unit == "" {
				g.line.Unit = l.Unit
			}
			if l.Expires != "" && (g.line.Expires == "" || l.Expires < g.line.Expires) {
				if g.line.Expires != "" {
					g.notes = append(g.notes, "几行的日期不同，按最早的 "+shortDay(model.Day(l.Expires))+" 记")
				}
				g.line.Expires = l.Expires
			}
			if l.BoughtOn != "" && (g.line.BoughtOn == "" || l.BoughtOn < g.line.BoughtOn) {
				g.line.BoughtOn = l.BoughtOn
			}
			g.line.Category = cmp.Or(g.line.Category, l.Category)
			g.line.Place = cmp.Or(g.line.Place, l.Place)
			if l.Price != nil {
				p := *l.Price
				if g.line.Price != nil {
					p += *g.line.Price
				}
				g.line.Price = &p
			}
			joined = true
			break
		}
		if !joined {
			gs = append(gs, group{line: l, from: []int{n}, key: key, answer: answer})
		}
	}
	for gi := range gs {
		if len(gs[gi].from) > 1 {
			gs[gi].notes = append([]string{fmt.Sprintf("第 %s 行是同一样东西，合在一起记，结果按合起来的数算", joinInts(gs[gi].from))},
				gs[gi].notes...)
		}
	}
	return gs, skipped
}

// Make plans the input with the edits against the records of h on the day today. It does not change h.
func Make(h *model.Home, in Input, edits []Edit, today model.Day) (Plan, error) {
	if err := Check(in); err != nil {
		return Plan{}, err
	}
	lines, match, skip, err := withEdits(in.Lines, edits)
	if err != nil {
		return Plan{}, err
	}
	mode := cmp.Or(in.Mode, DefaultMode(in.Source))
	target := func(l Line) string { return cmp.Or(l.Target, in.Target, ToStock) }
	p := planner{h: h, today: today, mode: mode, source: in.Source, newPlaces: map[string]bool{}, target: target}
	groups, skipped := merge(lines, target, skip, func(n int, l Line) (string, string) {
		return p.resolve(n, l, lines, match)
	})
	plan := Plan{Steps: []Step{}, Unread: slices.Clone(in.Unread)}
	if plan.Unread == nil {
		plan.Unread = []Unread{}
	}
	type newThing struct {
		line  int
		title string
	}
	var news []newThing // new items of this import so far, to ask about one whose name comes near
	for _, g := range groups {
		var s Step
		near := newThing{}
		if g.dupOf == 0 && g.line.Target == ToStock && g.answer == "" {
			if _, found, ask := p.findItem(g.line.Title, ""); !found && ask == nil {
				for _, o := range news {
					if closeness(o.title, g.line.Title) > 0 {
						near = o
						break
					}
				}
				news = append(news, newThing{g.from[0], g.line.Title})
			}
		}
		if near.line != 0 {
			s = Step{Line: g.from[0], From: g.from, Title: g.line.Title, Target: g.line.Target, Qty: g.line.Qty,
				Unit: g.line.Unit, Price: g.line.Price, Action: ActAsk, Ask: &Question{Kind: AskSameItem,
					Text: fmt.Sprintf("这次第 %d 行有「%s」，和这一行的「%s」是同一样吗？", near.line, near.title, g.line.Title),
					Choices: []Choice{
						{Label: fmt.Sprintf("和第 %d 行是同一样", near.line), Match: fmt.Sprintf("%s%d", matchLine, near.line)},
						{Label: "当新的记", Match: MatchNew}}}}
		} else if g.dupOf != 0 {
			s = Step{Line: g.from[0], From: g.from, Title: g.line.Title, Target: g.line.Target, Qty: g.line.Qty,
				Unit: g.line.Unit, Price: g.line.Price, Action: ActAsk, Ask: &Question{Kind: AskDuplicate,
					Text: fmt.Sprintf("这一行「%s」和第 %d 行「%s」是同一样东西，%s。请把两行改成同一个单位（告诉我折合多少），或者这行不记。",
						g.line.Title, g.dupOf, g.dupTitle, g.why),
					Choices: []Choice{{Label: "这行不记", Skip: true}}}}
		} else if g.line.Target == ToShopping {
			s = p.shopping(g, g.answer)
		} else {
			s = p.stock(g, g.answer)
		}
		s.Notes = append(g.notes, s.Notes...)
		if s.Notes == nil {
			s.Notes = []string{}
		}
		if s.Inferred == nil {
			s.Inferred = []string{}
		}
		s.Says = says(s)
		plan.Steps = append(plan.Steps, s)
	}
	keepListed(plan.Steps)
	for _, n := range skipped {
		l := lines[n-1]
		plan.Steps = append(plan.Steps, Step{Line: n, From: []int{n}, Title: cleanTitle(l.Title), Target: target(l),
			Action: ActSkip, Inferred: []string{}, Notes: []string{}, Says: "「" + cleanTitle(l.Title) + "」这行不记"})
	}
	slices.SortStableFunc(plan.Steps, func(a, b Step) int { return cmp.Compare(a.Line, b.Line) })
	plan.Totals = totals(plan, p.newPlaces)
	return plan, nil
}

// resolve is what a line is, for grouping, and the match to plan it with. The answer is the user's for this line, or
// else the one given for an earlier line of the same name and target (the same name is the same thing). A stock line
// that matches one recorded item (by name or by the answer) is that item; a shopping line on the list is that purchase;
// anything else (a new thing, or a question) is its name.
func (p *planner) resolve(n int, l Line, lines []Line, match map[int]string) (string, string) {
	answer := match[n]
	if m, ok := strings.CutPrefix(answer, matchLine); ok {
		// The user said this line is the same thing as an earlier line of the import.
		if k, err := strconv.Atoi(m); err == nil && k >= 1 && k < n {
			o := lines[k-1]
			o.Title, o.Unit = cleanTitle(o.Title), normUnit(o.Unit)
			o.Target = p.target(o)
			return p.resolve(k, o, lines, match)
		}
		answer = ""
	}
	name := titleKey(l.Title)
	if answer == "" {
		for m := 1; m < n; m++ {
			o := lines[m-1]
			if a := match[m]; a != "" && titleKey(cleanTitle(o.Title)) == name && cmp.Or(o.Target, l.Target) == l.Target {
				answer = a
				break
			}
		}
	}
	if l.Target == ToShopping {
		if answer != "" && answer != MatchNew {
			for _, b := range p.h.Buys {
				if b.ID == answer && b.Status == model.BuyTodo {
					return "buy:" + b.ID, answer
				}
			}
		}
		if answer == "" {
			for _, b := range p.h.Buys {
				if b.Status == model.BuyTodo && (titleKey(b.Title) == name || b.ItemID != "" && titleKey(p.itemTitle(b.ItemID)) == name) {
					return "buy:" + b.ID, answer
				}
			}
		}
		return "name:" + name, answer
	}
	if it, found, _ := p.findItem(l.Title, answer); found {
		return "item:" + it.ID, it.ID
	}
	return "name:" + name, answer
}

// matchLine answers "the same as line n of this import": match "line:<n>".
const matchLine = "line:"

// keepListed leaves an entry of the shopping list open when the same import also says it is still to be bought: a
// stock line ("bought 2") then does not mark it bought, and the shopping line ("need more") keeps it, as it says.
func keepListed(steps []Step) {
	wanted := map[string]bool{}
	for _, s := range steps {
		if s.Target == ToShopping && s.Action == ActOnList && s.Buy != nil {
			wanted[s.Buy.ID] = true
		}
	}
	for i := range steps {
		s := &steps[i]
		if s.Target != ToStock || s.Buy == nil || !wanted[s.Buy.ID] {
			continue
		}
		s.Notes = append(s.Notes, "采购清单上的「"+s.Buy.Title+"」留着，不标已买：这次也说了还要买")
		s.Buy = nil
		if s.Action == ActUpdateItem && s.SetUnit == "" && s.SetCategory == "" && s.SetPlace == nil && s.SetExpires == "" {
			s.Action = ActNothing
		}
		s.Says = says(*s)
	}
}

// planner plans the steps of one import.
type planner struct {
	target    func(Line) string
	h         *model.Home
	today     model.Day
	mode      string
	source    string
	newPlaces map[string]bool // places this import creates, by key
}

func ptr(f float64) *float64 { return &f }

// stock plans a line that goes into the stock.
func (p *planner) stock(g group, answer string) Step {
	l := g.line
	s := Step{Line: g.from[0], From: g.from, Title: l.Title, Target: ToStock, Qty: l.Qty, Unit: l.Unit, Price: l.Price}
	if l.Category != "" && !slices.Contains(model.Categories, l.Category) {
		s.Notes = append(s.Notes, fmt.Sprintf("分类「%s」不在可选的分类里（%s），没有用", l.Category,
			strings.Join(model.Categories, "、")))
		l.Category = ""
	}
	it, found, ask := p.findItem(l.Title, answer)
	if ask != nil {
		s.Action, s.Ask = ActAsk, ask
		return s
	}
	if !found {
		return p.newItem(s, l)
	}
	s.Item = &Ref{ID: it.ID, Title: it.Title}
	return p.existing(s, l, it)
}

// findItem is the recorded item the title names. An answer (an item id, or "new") decides; else one item with the
// same name is it; several with the same name, or items whose names come near, are a question.
func (p *planner) findItem(title, answer string) (model.Item, bool, *Question) {
	switch answer {
	case MatchNew:
		return model.Item{}, false, nil
	case "":
	default:
		if it, ok := p.h.Item(answer); ok {
			return it, true, nil
		}
		// An answer that names no item is asked again.
	}
	key := titleKey(title)
	var exact []model.Item
	for _, it := range p.h.Items {
		if titleKey(it.Title) == key {
			exact = append(exact, it)
		}
	}
	if len(exact) == 1 && answer == "" {
		return exact[0], true, nil
	}
	cands := exact
	text := fmt.Sprintf("库存里有 %d 件都叫「%s」，这次的是哪一件？", len(exact), title)
	if len(exact) == 0 {
		cands = p.nearItems(title)
		if len(cands) == 0 {
			if answer != "" {
				return model.Item{}, false, &Question{Kind: AskSameItem, Text: "回答里的编号不是库存里的物品，请重新选",
					Choices: []Choice{{Label: "当新的记", Match: MatchNew}, {Label: "这行不记", Skip: true}}}
			}
			return model.Item{}, false, nil
		}
		text = fmt.Sprintf("库存里有%s，和「%s」是同一样吗？", quoteItems(cands), title)
	}
	q := &Question{Kind: AskSameItem, Text: text}
	for _, it := range cands {
		q.Choices = append(q.Choices, Choice{Label: "就是「" + it.Title + "」", Match: it.ID})
	}
	q.Choices = append(q.Choices, Choice{Label: "当新的记", Match: MatchNew})
	return model.Item{}, false, q
}

// nearItems are up to three recorded items whose names come near the title, nearest first.
func (p *planner) nearItems(title string) []model.Item {
	type near struct {
		it model.Item
		c  int
	}
	var ns []near
	for _, it := range p.h.Items {
		if c := closeness(title, it.Title); c > 0 {
			ns = append(ns, near{it, c})
		}
	}
	slices.SortFunc(ns, func(a, b near) int {
		return cmp.Or(cmp.Compare(a.c, b.c), cmp.Compare(a.it.Title, b.it.Title), cmp.Compare(a.it.ID, b.it.ID))
	})
	var out []model.Item
	for _, n := range ns[:min(3, len(ns))] {
		out = append(out, n.it)
	}
	return out
}

func quoteItems(its []model.Item) string {
	var parts []string
	for _, it := range its {
		parts = append(parts, "「"+it.Title+"」")
	}
	return strings.Join(parts, "、")
}

// newItem plans a line that becomes a new item: its quantity and unit as given, the category and place as given or
// suggested by the table, the expiry date as given or, when the things were bought now, suggested from the usual shelf
// life. A shopping list entry of the same name is marked bought.
func (p *planner) newItem(s Step, l Line) Step {
	s.Action = ActNewItem
	g, known := lookup(l.Title)
	s.Category = l.Category
	if s.Category == "" && known {
		s.Category = g.category
		s.Inferred = append(s.Inferred, "category")
	}
	if l.Place != "" {
		s.Place = p.namedPlace(&s, l.Place)
	} else {
		words := g.places
		if p.source == SrcFridge {
			words = cold
		}
		if pl, ok := p.placeWith(words); ok {
			s.Place = &Ref{ID: pl.ID, Title: pl.Title}
			s.Inferred = append(s.Inferred, "place")
		}
	}
	// A usual shelf life is only said, never stored as the expiry date: a guessed date would raise 已过期 and make a
	// later purchase throw the batch away. The user can give the date on the package as an edit.
	switch {
	case l.Expires != "":
		s.Expires = model.Day(l.Expires)
	case known && g.days > 0 && p.mode == ModeAdd:
		s.Keeps = g.days
		s.Notes = append(s.Notes, fmt.Sprintf("这类东西一般能放 %d 天左右（推测，不会记成过期日；包装上有日期可以告诉我）", g.days))
	}
	if s.Qty == nil {
		s.Notes = append(s.Notes, "没有数量：只记下名字，不跟踪数量")
	}
	if p.mode == ModeAdd {
		if b, ok := p.listEntry(l.Title, ""); ok {
			s.Buy = &Ref{ID: b.ID, Title: b.Title}
		}
	}
	return s
}

// namedPlace is the place the line names: a recorded place of that name, one whose name comes near (marked 推测), or a
// new place.
func (p *planner) namedPlace(s *Step, name string) *Ref {
	name = cleanTitle(name)
	for _, pl := range p.h.Places {
		if titleKey(pl.Title) == titleKey(name) {
			return &Ref{ID: pl.ID, Title: pl.Title}
		}
	}
	best, bestC := model.Place{}, 0
	for _, pl := range p.h.Places {
		if c := closeness(name, pl.Title); c > 0 && (bestC == 0 || c < bestC) {
			best, bestC = pl, c
		}
	}
	if bestC > 0 {
		s.Inferred = append(s.Inferred, "place")
		s.Notes = append(s.Notes, fmt.Sprintf("位置「%s」按已有的「%s」算", name, best.Title))
		return &Ref{ID: best.ID, Title: best.Title}
	}
	p.newPlaces[titleKey(name)] = true
	s.Notes = append(s.Notes, "新建位置「"+name+"」")
	return &Ref{Title: name}
}

// placeWith is the first recorded place whose name has one of the words, the words tried in order.
func (p *planner) placeWith(words []string) (model.Place, bool) {
	for _, w := range words {
		for _, pl := range p.h.Places {
			if strings.Contains(titleKey(pl.Title), w) {
				return pl, true
			}
		}
	}
	return model.Place{}, false
}

// listEntry is the open purchase on the shopping list for the item, or else one with the same name and no item.
func (p *planner) listEntry(title, itemID string) (model.Buy, bool) {
	if itemID != "" {
		for _, b := range p.h.Buys {
			if b.Status == model.BuyTodo && b.ItemID == itemID {
				return b, true
			}
		}
	}
	for _, b := range p.h.Buys {
		if b.Status == model.BuyTodo && b.ItemID == "" && titleKey(b.Title) == titleKey(title) {
			return b, true
		}
	}
	return model.Buy{}, false
}

// existing plans a line for an item that is recorded: the units must agree (or convert), an item that is counted needs
// a quantity, and only values the item lacks are added.
func (p *planner) existing(s Step, l Line, it model.Item) Step {
	iu := normUnit(it.Unit)
	switch {
	case s.Unit == "" && iu != "":
		s.Unit = it.Unit
		if s.Qty != nil {
			s.Inferred = append(s.Inferred, "unit")
			s.Notes = append(s.Notes, "没写单位，按库存里的「"+it.Unit+"」算")
		}
	case s.Unit != "" && iu == "":
		s.SetUnit = s.Unit
	case s.Unit != "" && s.Unit != iu:
		if s.Qty != nil {
			if q, ok := convert(*s.Qty, s.Unit, iu); ok {
				s.Notes = append(s.Notes, fmt.Sprintf("%s%s 换算成 %s%s", num(*s.Qty), s.Unit, num(q), it.Unit))
				s.Qty, s.Unit = &q, it.Unit
				break
			}
		}
		s.Action = ActAsk
		s.Ask = &Question{Kind: AskUnit,
			Text: fmt.Sprintf("库存里的「%s」按「%s」记，这次是「%s」，没法直接加。可以告诉我折合多少%s，或者当成新的一样记。",
				it.Title, it.Unit, s.Unit, it.Unit),
			Choices: []Choice{{Label: "当新的记", Match: MatchNew}, {Label: "这行不记", Skip: true}}}
		return s
	}
	if s.Qty == nil && it.HasQty {
		how := "买了多少"
		if p.mode == ModeCount {
			how = "现在有多少"
		}
		s.Action = ActAsk
		s.Ask = &Question{Kind: AskQty,
			Text: fmt.Sprintf("「%s」在库存里记着数量（约 %s%s），这次没写%s。", it.Title,
				num(rules.Estimate(it, p.today)), it.Unit, how),
			Choices: []Choice{{Label: "按 1" + it.Unit + "记", Qty: ptr(1)}, {Label: "这行不记", Skip: true}}}
		return s
	}
	if it.Category == "" {
		if l.Category != "" {
			s.SetCategory = l.Category
		} else if g, ok := lookup(it.Title); ok {
			s.SetCategory = g.category
			s.Inferred = append(s.Inferred, "category")
		}
		s.Category = s.SetCategory
	}
	if l.Place != "" {
		if it.PlaceID == "" {
			s.SetPlace = p.namedPlace(&s, l.Place)
			s.Place = s.SetPlace
		} else if pl, ok := p.h.Place(it.PlaceID); ok && titleKey(pl.Title) != titleKey(l.Place) {
			s.Notes = append(s.Notes, "位置不改，还在「"+pl.Title+"」")
		}
	}
	if l.Expires != "" {
		d := model.Day(l.Expires)
		switch {
		case it.Expires != "" && it.Expires >= p.today && it.Expires < d && p.mode == ModeAdd:
			s.Notes = append(s.Notes, "过期日还按旧的一批 "+shortDay(it.Expires)+" 提醒")
		case it.Expires != d:
			s.SetExpires, s.Expires, s.OldExpires = d, d, it.Expires
		}
	}
	est := rules.Estimate(it, p.today)
	switch {
	case s.Qty == nil:
		s.Action = ActNothing
		if s.SetUnit != "" || s.SetCategory != "" || s.SetPlace != nil || s.SetExpires != "" {
			s.Action = ActUpdateItem
		}
		if p.mode == ModeAdd {
			if b, ok := p.listEntry(l.Title, it.ID); ok {
				s.Buy = &Ref{ID: b.ID, Title: b.Title}
				s.Action = ActUpdateItem
			}
		}
	case p.mode == ModeCount:
		s.Action = ActSetCount
		if it.HasQty {
			s.Before = ptr(est)
			s.Lowers = *s.Qty < round3(est)
		}
		s.After = ptr(*s.Qty)
	default:
		s.Action = ActAddToItem
		expired := it.Expires != "" && it.Expires < p.today
		switch {
		case !it.HasQty:
			s.Track = true
			s.After = ptr(*s.Qty)
			s.Notes = append(s.Notes, "库存里原来没记数量，从现在起记成这次的数量")
		case expired:
			s.Before, s.After, s.OldGone = ptr(est), ptr(*s.Qty), true
			s.Notes = append(s.Notes, "旧的一批已经过期，按扔掉算，只记这次买的")
			if s.SetExpires == "" {
				s.Notes = append(s.Notes, "旧的过期日会清掉")
			}
		default:
			s.Before, s.After = ptr(est), ptr(round3(est+*s.Qty))
		}
		if b, ok := p.listEntry(l.Title, it.ID); ok {
			s.Buy = &Ref{ID: b.ID, Title: b.Title}
		}
	}
	return s
}

// shopping plans a line that goes on the shopping list: it is there already when an open purchase has its name (or is
// for an item of its name); a purchase whose name comes near is a question; else it is added, for the recorded item
// of that name when there is exactly one.
func (p *planner) shopping(g group, answer string) Step {
	l := g.line
	s := Step{Line: g.from[0], From: g.from, Title: l.Title, Target: ToShopping, Qty: l.Qty, Unit: l.Unit, Price: l.Price}
	if l.Expires != "" {
		s.Notes = append(s.Notes, "要买的东西不记过期日")
	}
	if l.Place != "" {
		s.Notes = append(s.Notes, "要买的东西不记位置")
	}
	var open []model.Buy
	for _, b := range p.h.Buys {
		if b.Status == model.BuyTodo {
			open = append(open, b)
		}
	}
	key := titleKey(l.Title)
	if answer != "" && answer != MatchNew {
		for _, b := range open {
			if b.ID == answer {
				s.Action, s.Buy = ActOnList, &Ref{ID: b.ID, Title: b.Title}
				return s
			}
		}
	}
	if answer == "" {
		for _, b := range open {
			if titleKey(b.Title) == key || b.ItemID != "" && titleKey(p.itemTitle(b.ItemID)) == key {
				s.Action, s.Buy = ActOnList, &Ref{ID: b.ID, Title: b.Title}
				return s
			}
		}
		var near []model.Buy
		for _, b := range open {
			if closeness(l.Title, b.Title) > 0 {
				near = append(near, b)
			}
		}
		if len(near) > 0 {
			near = near[:min(3, len(near))]
			q := &Question{Kind: AskSameEntry}
			var names []string
			for _, b := range near {
				names = append(names, "「"+b.Title+"」")
				q.Choices = append(q.Choices, Choice{Label: "就是「" + b.Title + "」", Match: b.ID})
			}
			q.Text = fmt.Sprintf("采购清单上已经有%s，和「%s」是同一样吗？", strings.Join(names, "、"), l.Title)
			q.Choices = append(q.Choices, Choice{Label: "另加一条", Match: MatchNew})
			s.Action, s.Ask = ActAsk, q
			return s
		}
	}
	s.Action = ActAddToList
	var same []model.Item
	for _, it := range p.h.Items {
		if titleKey(it.Title) == key {
			same = append(same, it)
		}
	}
	if len(same) == 1 {
		s.Item = &Ref{ID: same[0].ID, Title: same[0].Title}
		if s.Unit == "" && same[0].Unit != "" && s.Qty != nil {
			s.Unit = same[0].Unit
			s.Inferred = append(s.Inferred, "unit")
		}
	}
	s.Category = l.Category
	if !slices.Contains(model.Categories, s.Category) {
		s.Category = ""
		if len(same) == 1 && same[0].Category != "" {
			s.Category = same[0].Category
		} else if gu, ok := lookup(l.Title); ok {
			s.Category = gu.category
			s.Inferred = append(s.Inferred, "category")
		}
	}
	return s
}

func (p *planner) itemTitle(id string) string {
	if it, ok := p.h.Item(id); ok {
		return it.Title
	}
	return ""
}

// totals counts the steps.
func totals(p Plan, newPlaces map[string]bool) Totals {
	t := Totals{Lines: len(p.Steps), Unread: len(p.Unread), NewPlaces: len(newPlaces)}
	var price float64
	priced := false
	for _, s := range p.Steps {
		switch s.Action {
		case ActNewItem:
			t.NewItems++
		case ActAddToItem:
			t.AddedTo++
		case ActSetCount:
			t.Counted++
		case ActUpdateItem:
			t.Updated++
		case ActAddToList:
			t.ToList++
		case ActOnList:
			t.OnList++
		case ActNothing:
			t.Unchanged++
		case ActSkip:
			t.Skipped++
		case ActAsk:
			t.Questions++
		}
		if s.Lowers && s.Writes() {
			t.Lowered++
		}
		if s.Buy != nil && s.Target == ToStock && s.Writes() {
			t.Bought++
		}
		if s.Writes() {
			t.Changes++
		}
		if len(s.Inferred) > 0 && s.Writes() {
			t.Inferred++
		}
		if s.Price != nil && s.Action != ActSkip {
			price += *s.Price
			priced = true
		}
	}
	if priced {
		t.PriceTotal = ptr(round3(price))
	}
	return t
}

// fieldNames are the inferred fields in the user's words.
var fieldNames = map[string]string{"category": "分类", "place": "位置", "expires": "过期日", "unit": "单位"}

// InferredWords is the inferred fields of a step in the user's words, joined.
func InferredWords(s Step) string {
	var out []string
	for _, f := range s.Inferred {
		out = append(out, fieldNames[f])
	}
	return strings.Join(out, "、")
}

// says is the step in one plain sentence; inferred values carry （推测）.
func says(s Step) string {
	inf := func(field, text string) string {
		if slices.Contains(s.Inferred, field) {
			return text + "（推测）"
		}
		return text
	}
	amount := func(q *float64) string {
		if q == nil {
			return ""
		}
		return num(*q) + s.Unit
	}
	var extra []string
	if s.Category != "" && (s.Action == ActNewItem || s.SetCategory != "") {
		extra = append(extra, inf("category", "分类「"+s.Category+"」"))
	}
	if s.Place != nil && (s.Action == ActNewItem || s.SetPlace != nil) {
		extra = append(extra, inf("place", "放「"+s.Place.Title+"」"))
	}
	switch {
	case s.Expires != "" && s.Action == ActNewItem:
		extra = append(extra, string(s.Expires)+" 过期")
	case s.SetExpires != "" && s.OldExpires != "":
		extra = append(extra, "过期日 "+shortDay(s.OldExpires)+" → "+shortDay(s.SetExpires))
	case s.SetExpires != "":
		extra = append(extra, string(s.SetExpires)+" 过期")
	}
	if s.SetUnit != "" {
		extra = append(extra, "单位记成「"+s.SetUnit+"」")
	}
	bought := ""
	if s.Buy != nil && s.Target == ToStock {
		bought = "采购清单上的「" + s.Buy.Title + "」标成已买"
	}
	var head string
	switch s.Action {
	case ActNewItem:
		head = "新加「" + s.Title + "」"
		if q := amount(s.Qty); q != "" {
			head += inf("unit", q)
		}
	case ActAddToItem:
		head = fmt.Sprintf("「%s」加 %s", s.Item.Title, inf("unit", amount(s.Qty)))
		if s.OldGone {
			head += fmt.Sprintf("：旧的已过期，记成 %s%s", num(*s.After), s.Unit)
		} else if s.Before != nil {
			head += fmt.Sprintf("：约 %s → %s%s", num(*s.Before), num(*s.After), s.Unit)
		} else {
			head += fmt.Sprintf("：记成 %s%s", num(*s.After), s.Unit)
		}
	case ActSetCount:
		head = fmt.Sprintf("「%s」记成 %s", s.Item.Title, inf("unit", amount(s.Qty)))
		switch {
		case s.Lowers:
			head += fmt.Sprintf("：会从约 %s%s 改成 %s%s，变少了", num(*s.Before), s.Unit, num(*s.After), s.Unit)
		case s.Before != nil:
			head += fmt.Sprintf("（原来约 %s%s）", num(*s.Before), s.Unit)
		}
	case ActUpdateItem:
		head = "「" + s.Item.Title + "」已经记着，数量不变"
	case ActNothing:
		return "「" + s.Item.Title + "」已经记着，这行没有要改的"
	case ActAddToList:
		head = "把「" + s.Title + "」"
		if q := amount(s.Qty); q != "" {
			head += inf("unit", q)
		}
		head += "加到采购清单"
		if s.Category != "" {
			extra = append(extra, inf("category", "分类「"+s.Category+"」"))
		}
	case ActOnList:
		return "「" + s.Title + "」已经在采购清单上（「" + s.Buy.Title + "」），不再加"
	case ActAsk:
		return "「" + s.Title + "」要你定：" + s.Ask.Text
	case ActSkip:
		return "「" + s.Title + "」这行不记"
	}
	parts := append([]string{head}, extra...)
	if bought != "" {
		parts = append(parts, bought)
	}
	return strings.Join(parts, "，")
}

// num is a number without trailing zeros.
func num(f float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.3f", f), "0"), ".")
}

// shortDay is a day as MM-DD.
func shortDay(d model.Day) string {
	if len(d) != 10 {
		return string(d)
	}
	return string(d)[5:]
}

func joinInts(ns []int) string {
	var parts []string
	for _, n := range ns {
		parts = append(parts, fmt.Sprint(n))
	}
	return strings.Join(parts, "、")
}

// unitName is a unit as the words say it, 没写单位 for none.
func unitName(u string) string {
	if u == "" {
		return "没写单位"
	}
	return u
}

// Key identifies the choice within its question, whatever its position: the edit it makes.
func (c Choice) Key() string {
	switch {
	case c.Skip:
		return "skip"
	case c.Qty != nil:
		return "qty=" + num(*c.Qty)
	}
	return "match=" + c.Match
}
