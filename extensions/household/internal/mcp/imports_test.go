package mcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/household/internal/imports"
	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

// receipt is what an assistant would send after reading a made-up supermarket receipt.
const receipt = `{"source":"receipt","note":"周六超市","lines":[
	{"title":"纯牛奶","qty":2,"unit":"盒","price":12},
	{"title":"鸡蛋","qty":12,"unit":"个","expires":"2026-12-30","price":15.8},
	{"title":"洗衣液","qty":1,"unit":"瓶","price":29.9},
	{"title":"香蕉","qty":1,"unit":"把","price":8.5}
]}`

func lineOf(t *testing.T, preview any, n int) map[string]any {
	t.Helper()
	for _, l := range get(t, preview, "lines").([]any) {
		if get(t, l, "line") == float64(n) {
			return asMap(t, l)
		}
	}
	t.Fatalf("no line %d in %v", n, preview)
	return nil
}

func TestImportFromAReceiptEndToEnd(t *testing.T) {
	s := start(t, seeded(t, true))
	defer s.finish()
	statusBefore := s.done("household_status", `{}`)

	pre := s.done("household_import_preview", oneLine(receipt))
	id := asStr(t, get(t, pre, "draft_id"))
	if get(t, lineOf(t, pre, 1), "action") != "add_to_item" || get(t, lineOf(t, pre, 3), "action") != "new_item" {
		t.Fatalf("preview lines %v", get(t, pre, "lines"))
	}
	// The manual entry 洗衣液 on the shopping list is marked bought with the new item.
	if get(t, lineOf(t, pre, 3), "buy", "title") != "洗衣液" {
		t.Fatalf("detergent line %v", lineOf(t, pre, 3))
	}
	// The banana's category is inferred, and said so.
	banana := lineOf(t, pre, 4)
	if inf := get(t, banana, "inferred").([]any); len(inf) == 0 || !strings.Contains(asStr(t, get(t, banana, "says")), "推测") {
		t.Fatalf("banana %v", banana)
	}
	if !strings.Contains(asStr(t, get(t, pre, "summary")), "合计") || get(t, pre, "totals", "price_total") != 66.2 {
		t.Fatalf("summary %v totals %v", get(t, pre, "summary"), get(t, pre, "totals"))
	}
	// The preview changed no record.
	if after := s.done("household_status", `{}`); fmt.Sprint(after) != fmt.Sprint(statusBefore) {
		t.Fatal("preview changed the records")
	}
	// The draft is listed.
	if l := s.done("household_import_list", `{}`); get(t, l, "drafts", 0, "draft_id") != id {
		t.Fatalf("list %v", l)
	}

	done := s.done("household_import_apply", fmt.Sprintf(`{"draft_id":%q,"revision":1}`, id))
	if len(get(t, done, "done").([]any)) != 4 {
		t.Fatalf("applied %v", done)
	}
	milk := rec(t, s, model.TItem, "纯牛奶")
	if get(t, milk, "qty") != float64(2) {
		t.Fatalf("milk after import %v (the expired batch is gone, only the new 2 count)", milk)
	}
	eggs := rec(t, s, model.TItem, "鸡蛋")
	if get(t, eggs, "qty") != float64(12) || get(t, eggs, "expires") != "2026-12-30" {
		t.Fatalf("eggs after import %v", eggs)
	}
	if b := rec(t, s, model.TBuy, "洗衣液"); get(t, b, "status") != "已买" {
		t.Fatalf("detergent purchase %v", b)
	}
	list := s.done("household_import_list", `{}`)
	if get(t, list, "last_import", "can_undo") != true || len(get(t, list, "drafts").([]any)) != 0 {
		t.Fatalf("list after apply %v", list)
	}
	// Applying twice is refused: the draft is gone.
	if msg := s.failed("household_import_apply", fmt.Sprintf(`{"draft_id":%q,"revision":1}`, id)); !strings.Contains(msg, "没有草稿") {
		t.Fatalf("second apply: %s", msg)
	}

	undone := s.done("household_import_undo", `{}`)
	if get(t, undone, "removed") != float64(2) {
		t.Fatalf("undo %v", undone)
	}
	if after := s.done("household_status", `{}`); fmt.Sprint(get(t, after, "summary")) != fmt.Sprint(get(t, statusBefore, "summary")) {
		t.Fatalf("status after undo %v, before %v", get(t, after, "summary"), get(t, statusBefore, "summary"))
	}
	if msg := s.failed("household_import_undo", `{}`); !strings.Contains(msg, "撤销过") {
		t.Fatalf("second undo: %s", msg)
	}
}

func TestImportQuestionsAndAnswers(t *testing.T) {
	s := start(t, seeded(t, true))
	defer s.finish()
	pre := s.done("household_import_preview", `{"source":"text","lines":[{"title":"牛奶","qty":1,"unit":"盒"},{"title":"番茄","qty":1,"unit":"斤"}]}`)
	id := asStr(t, get(t, pre, "draft_id"))
	milk := lineOf(t, pre, 1)
	if get(t, milk, "action") != "ask" || get(t, milk, "question", "kind") != "same_item" {
		t.Fatalf("milk %v", milk)
	}
	if get(t, lineOf(t, pre, 2), "question", "kind") != "unit" {
		t.Fatalf("tomato %v", lineOf(t, pre, 2))
	}
	if msg := s.failed("household_import_apply", fmt.Sprintf(`{"draft_id":%q,"revision":1}`, id)); !strings.Contains(msg, "要用户定") {
		t.Fatalf("apply with questions: %s", msg)
	}
	milkID := idOf(t, s, model.TItem, "纯牛奶")
	// Re-preview with the answers, then apply.
	again := s.done("household_import_preview", fmt.Sprintf(`{"draft_id":%q,"edits":[{"line":1,"match":%q},{"line":2,"qty":3,"unit":"个"}]}`, id, milkID))
	if get(t, lineOf(t, again, 1), "action") != "add_to_item" || get(t, lineOf(t, again, 2), "after") != float64(7) {
		t.Fatalf("re-preview %v", get(t, again, "lines"))
	}
	// The first revision was not agreed to after the re-preview: it is refused, the new one is written.
	if msg := s.failed("household_import_apply", fmt.Sprintf(`{"draft_id":%q,"revision":1}`, id)); !strings.Contains(msg, "预览已经变了") {
		t.Fatalf("apply of the old revision: %s", msg)
	}
	s.done("household_import_apply", fmt.Sprintf(`{"draft_id":%q,"revision":%v}`, id, get(t, again, "revision")))
	if got := get(t, rec(t, s, model.TItem, "番茄"), "qty"); got != float64(7) {
		t.Fatalf("tomato %v", got)
	}
}

func TestImportUndoRefusedAfterALaterChange(t *testing.T) {
	s := start(t, seeded(t, true))
	defer s.finish()
	pre := s.done("household_import_preview", `{"source":"order_screenshot","lines":[{"title":"番茄","qty":2,"unit":"个"}]}`)
	s.done("household_import_apply", fmt.Sprintf(`{"draft_id":%q,"revision":1}`, asStr(t, get(t, pre, "draft_id"))))
	s.done("household_use", fmt.Sprintf(`{"id":%q,"amount":1}`, idOf(t, s, model.TItem, "番茄")))
	if msg := s.failed("household_import_undo", `{}`); !strings.Contains(msg, "「番茄」在导入之后又改过") {
		t.Fatalf("undo: %s", msg)
	}
	if l := s.done("household_import_list", `{}`); get(t, l, "last_import", "can_undo") != false || get(t, l, "last_import", "why_not") == "" {
		t.Fatalf("list %v", l)
	}
}

func TestImportDiscardAndBadInput(t *testing.T) {
	s := start(t, seeded(t, false))
	defer s.finish()
	pre := s.done("household_import_preview", `{"source":"fridge_photo","lines":[{"title":"酸奶","qty":3,"unit":"杯"}]}`)
	id := asStr(t, get(t, pre, "draft_id"))
	if get(t, pre, "mode") != "count" {
		t.Fatalf("fridge photo mode %v", get(t, pre, "mode"))
	}
	s.done("household_import_discard", fmt.Sprintf(`{"draft_id":%q}`, id))
	if msg := s.failed("household_import_apply", fmt.Sprintf(`{"draft_id":%q,"revision":1}`, id)); !strings.Contains(msg, "没有草稿") {
		t.Fatalf("apply after discard: %s", msg)
	}
	for name, args := range map[string]string{
		"no source":     `{"lines":[{"title":"x"}]}`,
		"bad source":    `{"source":"fax","lines":[{"title":"x"}]}`,
		"no lines":      `{"source":"text","lines":[]}`,
		"bad date":      `{"source":"text","lines":[{"title":"牛奶","expires":"明天"}]}`,
		"unknown field": `{"source":"text","lines":[{"title":"牛奶","brand":"x"}]}`,
		"edits alone":   `{"source":"text","lines":[{"title":"牛奶"}],"edits":[{"line":1}]}`,
		"draft and new": `{"draft_id":"imp-1","source":"text"}`,
	} {
		if msg := s.failed("household_import_preview", args); msg == "" {
			t.Errorf("%s: no message", name)
		}
	}
}

// oneLine puts a JSON text on one line: the protocol is one message per line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestImportApplyNeedsARevision(t *testing.T) {
	s := start(t, seeded(t, true))
	defer s.finish()
	pre := s.done("household_import_preview", `{"source":"text","lines":[{"title":"番茄","qty":1,"unit":"个"}]}`)
	if get(t, pre, "revision") != float64(1) {
		t.Fatalf("revision %v", get(t, pre, "revision"))
	}
	if msg := s.failed("household_import_apply", fmt.Sprintf(`{"draft_id":%q}`, asStr(t, get(t, pre, "draft_id")))); !strings.Contains(msg, "revision") {
		t.Fatalf("apply without a revision: %s", msg)
	}
}

func TestPreviewDescriptionSaysItStoresADraftAndCountsCanLower(t *testing.T) {
	if !strings.Contains(aboutPreview, "只存一份草稿") || !strings.Contains(aboutPreview, "可能比现在记的少") {
		t.Fatal(aboutPreview)
	}
}

// N4 (i): apply writes exactly a previewed revision: corrections go through the preview.
func TestImportApplyTakesOnlyDraftAndRevision(t *testing.T) {
	s := start(t, seeded(t, true))
	defer s.finish()
	pre := s.done("household_import_preview", `{"source":"shelf_photo","lines":[{"title":"大米","qty":2,"unit":"千克"}]}`)
	id := asStr(t, get(t, pre, "draft_id"))
	if msg := s.failed("household_import_apply", fmt.Sprintf(`{"draft_id":%q,"revision":1,"edits":[{"line":1,"qty":0.1}]}`, id)); !strings.Contains(msg, "参数不对") {
		t.Fatalf("apply with edits: %s", msg)
	}
	if msg := s.failed("household_import_apply", fmt.Sprintf(`{"draft_id":%q,"revision":1,"skip":[1]}`, id)); !strings.Contains(msg, "参数不对") {
		t.Fatalf("apply with skip: %s", msg)
	}
	again := s.done("household_import_preview", fmt.Sprintf(`{"draft_id":%q,"edits":[{"line":1,"qty":0.5}]}`, id))
	s.done("household_import_apply", fmt.Sprintf(`{"draft_id":%q,"revision":%v}`, id, get(t, again, "revision")))
	if got := get(t, rec(t, s, model.TItem, "大米"), "qty"); got != 0.5 {
		t.Fatalf("rice %v, want the previewed 0.5", got)
	}
	if strings.Contains(aboutApply, "skip 是") || !strings.Contains(aboutApply, "household_import_preview 带 draft_id 和 edits") {
		t.Fatal(aboutApply)
	}
}

// Round 3, item 1: the guidance never sends corrections to household_import_apply.
func TestGuidancePointsCorrectionsAtThePreview(t *testing.T) {
	s := start(t, seeded(t, true))
	defer s.finish()
	for _, args := range []string{
		`{"source":"text","lines":[{"title":"牛奶","qty":1,"unit":"盒"}]}`,
		`{"source":"text","lines":[{"title":"番茄","qty":1,"unit":"个"}]}`,
	} {
		next := asStr(t, get(t, s.done("household_import_preview", args), "next"))
		if !strings.Contains(next, "household_import_preview") || !strings.Contains(next, "household_import_apply") ||
			strings.Contains(next, "edits）调用 household_import_apply") || strings.Contains(next, "不要的行放进 skip") {
			t.Fatalf("next %q", next)
		}
	}
	skill, err := os.ReadFile("../../skills/household/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(skill), "\n") {
		if strings.Contains(line, "`household_import_apply`：") && (strings.Contains(line, `"skip"`) || strings.Contains(line, `"edits"`)) {
			t.Fatalf("skill line %q", line)
		}
	}
	for _, tl := range Tools() {
		if strings.Contains(tl.About, "household_import_apply（带") {
			t.Fatalf("%s: %s", tl.Name, tl.About)
		}
	}
	// A pasted list is the page's: the tool does not take that source.
	if msg := s.failed("household_import_preview", `{"source":"paste","lines":[{"title":"牛奶"}]}`); !strings.Contains(msg, "source") {
		t.Fatalf("paste from the tool: %s", msg)
	}
}

// Round 3, item 3: the list says an undecided undo truthfully.
func TestListSaysAnUndecidedUndo(t *testing.T) {
	st := seeded(t, true)
	imp := imports.Open("")
	pre := func() map[string]any {
		s := startWith(t, st, imp)
		defer s.finish()
		p := s.done("household_import_preview", `{"source":"text","lines":[{"title":"番茄","qty":1,"unit":"个"}]}`)
		s.done("household_import_apply", fmt.Sprintf(`{"draft_id":%q,"revision":1}`, asStr(t, get(t, p, "draft_id"))))
		_ = imp.Update(func(s *imports.State, _ time.Time) error {
			last, _ := s.Last()
			last.Unknown = imports.UnknownUndo
			return nil
		})
		return asMap(t, s.done("household_import_list", `{}`))
	}()
	if get(t, pre, "last_import", "undone") != false || get(t, pre, "last_import", "undo_unknown") != true ||
		get(t, pre, "last_import", "can_undo") != false {
		t.Fatalf("list %v", pre)
	}
}

// startWith is start with a given draft store.
func startWith(t *testing.T, st kernel.Storage, imp *imports.Store) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &session{t: t, in: inW, out: bufio.NewReader(outR), finished: make(chan error, 1)}
	go func() {
		err := ServeImports(context.Background(), inR, outW, st, imp, shanghai)
		outW.Close()
		s.finished <- err
	}()
	return s
}
