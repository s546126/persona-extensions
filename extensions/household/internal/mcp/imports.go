package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/household/internal/imports"
	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
)

// The import tools: the assistant reads a receipt, a photo or a text itself and passes the lines; the extension plans
// them against the records as a draft (read-only, allowed), and writes, undoes or drops a draft only when the user said
// yes (the hub asks for every call of those).

const aboutPreview = "把从小票、冰箱或柜子的照片、订单截图或一段文字里读到的东西做成一份导入草稿：只存一份草稿、给出预览，不改任何物品、采购等记录。" +
	"图片和文字由你自己读：每样东西一行，title 写名称；qty（数量）、unit（单位）、expires（过期日）、bought_on（购买日）、" +
	"price（这一行的金额）、category、place 只写看得清、用户说过的，看不清的不要猜，先问用户。" +
	"target 为 stock（记到库存，默认）或 shopping（加到采购清单），可用 default_target 给所有行设。" +
	"mode 为 add（买回来的，加到现有数量上；小票、订单、文字默认）或 count（看到多少就记成多少；冰箱、柜子照片默认）。" +
	"注意 count 会把数量改成看到的数，可能比现在记的少（这些行 lowers 为真）；照片可能没拍全时，要把这一点告诉用户并问清楚。" +
	"工具会和已有记录比对：同名的加到原来的物品上，采购清单上有这一项的顺便标成已买；名字相近、单位对不上、缺数量的列成问题（question），" +
	"不会自己合并；分类、位置和常见保质期只是推测，列在 inferred 里，可以改。" +
	"常见保质期只作为提示（usual_days），不会记成过期日。" +
	"返回 draft_id、revision（这一版预览的编号，写入时要带上）、每一行会怎么处理（says）、问题、合计和一段可以念给用户的 summary。" +
	"草稿保存 24 小时，用户在家庭管家页面上也能看到、确认或放弃。" +
	"带 draft_id（可再带 edits）调用则按用户的修改重新预览那份草稿，不用再传 lines；会得到新的 revision，要把新的预览念给用户。" +
	"用户在页面上粘贴的草稿只能由用户在页面上修改。"

const aboutList = "列出等用户确认的导入草稿（包括用户在家庭管家页面上粘贴的清单），以及最近一次写入的导入和它现在能不能撤销。只读。"

const aboutApply = "把一份导入草稿写进记录：新建物品、给已有物品加数量或记数量、把采购标成已买、加到采购清单，一次全部写入，有一步失败就什么都不写。" +
	"会改数据：必须先把预览念给用户，用户明确同意后才调用。只带 draft_id 和 revision（用户同意的那一版预览的编号），写入的正是那一版预览。" +
	"用户要改某行、回答问题或不要某行时，不要在这里改：用 household_import_preview 带 draft_id 和 edits（line 为行号；match 填物品或采购的编号表示就是它，" +
	"new 表示当新的记；skip 为真表示这行不记；也可以改 title、qty、unit、category、place、expires、target），把新的预览念给用户，再按新的 revision 写入。" +
	"草稿后来又被改过、还有没回答的问题、或预览之后记录变了、结果会和预览不一样时会拒绝，并说明原因。" +
	"用户在家庭管家页面上粘贴的草稿（made_on_page）不能由你写入或放弃，请让用户在页面上确认。写入后可以用 household_import_undo 撤销。"

const aboutUndo = "撤销最近一次写入的导入：这次新建的记录删掉，改过的物品和采购恢复成导入前的样子。会改数据，先问用户。" +
	"导入之后这些记录又被改过（例如又记了用量），或者新建的物品已经被别的记录用到时，不撤销，并说明是哪一条挡住了。" +
	"draft_id 可选：填了就只在最近一次导入正是这份草稿时撤销。"

const aboutDiscard = "放弃一份导入草稿：草稿删掉，记录没有任何改动。用户说不要这份导入时调用；用户没有确认的草稿 24 小时后也会自己作废。" +
	"用户在页面上粘贴的草稿只能由用户在页面上放弃。"

// ---- schemas ----

func noInput() map[string]any { return object(map[string]any{}) }

func num(desc string) map[string]any {
	return map[string]any{"type": "number", "exclusiveMinimum": 0, "description": desc}
}

func date(desc string) map[string]any {
	return map[string]any{"type": "string", "format": "date", "description": desc + "，格式 YYYY-MM-DD"}
}

func targetProp(desc string) map[string]any {
	return map[string]any{"type": "string", "enum": []string{imports.ToStock, imports.ToShopping}, "description": desc}
}

func categoryHint() string {
	return "分类，只能是 " + strings.Join(model.Categories, "、") + " 之一；不确定就不填"
}

func lineSchema() map[string]any {
	return object(map[string]any{
		"title":     map[string]any{"type": "string", "minLength": 1, "maxLength": 100, "description": "名称，按小票或包装上的写，例如「纯牛奶」"},
		"qty":       num("数量；看不清就不填"),
		"unit":      str("单位，例如 盒、个、瓶、千克"),
		"category":  str(categoryHint()),
		"place":     str("放在哪，例如 冰箱；用户没说就不填"),
		"expires":   date("过期日，只填包装或用户给的日期"),
		"bought_on": date("购买日，例如小票上的日期"),
		"price":     map[string]any{"type": "number", "minimum": 0, "description": "这一行的金额（元），只用来合计，不记到库存"},
		"target":    targetProp("这一行记到哪：stock 库存，shopping 采购清单；不填用 default_target"),
	}, "title")
}

func editSchema() map[string]any {
	return object(map[string]any{
		"line":     map[string]any{"type": "integer", "minimum": 1, "description": "行号，和预览里的 line 一样"},
		"title":    str("改成的名称"),
		"qty":      num("改成的数量"),
		"unit":     str("改成的单位"),
		"category": str(categoryHint()),
		"place":    str("改成的位置"),
		"expires":  date("改成的过期日"),
		"target":   targetProp("改记到哪"),
		"match":    str("回答「是不是同一样」：物品或采购的编号表示就是它，new 表示当新的记"),
		"skip":     map[string]any{"type": "boolean", "description": "true 表示这一行不记"},
	}, "line")
}

func importPreviewInput() map[string]any {
	return object(map[string]any{
		"source": map[string]any{"type": "string", "enum": imports.Sources,
			"description": "从哪读到的：receipt 小票、fridge_photo 冰箱照片、shelf_photo 柜子或货架照片、order_screenshot 订单截图、text 文字、other 其他"},
		"note":           str("一句备注，例如「周六超市」；会显示在页面上"),
		"default_target": targetProp("没写 target 的行记到哪，默认 stock"),
		"mode":           map[string]any{"type": "string", "enum": []string{imports.ModeAdd, imports.ModeCount}, "description": "add 加到现有数量上，count 按看到的记数量；不填按来源定"},
		"lines":          map[string]any{"type": "array", "maxItems": imports.MaxLines, "items": lineSchema(), "description": "读到的每一样东西，一样一行"},
		"draft_id":       str("重新预览已有的草稿时填；这时不用 source 和 lines"),
		"edits":          map[string]any{"type": "array", "items": editSchema(), "description": "重新预览时带上用户的修改、回答和不要的行（skip）"},
	})
}

func importApplyInput() map[string]any {
	return object(map[string]any{
		"draft_id": str("预览给的草稿编号"),
		"revision": map[string]any{"type": "integer", "minimum": 1, "description": "用户同意的那一版预览的 revision"},
	}, "draft_id", "revision")
}

func importUndoInput() map[string]any {
	return object(map[string]any{"draft_id": str("可选：要撤销的那次导入的草稿编号")})
}

func importDiscardInput() map[string]any {
	return object(map[string]any{"draft_id": str("要放弃的草稿编号")}, "draft_id")
}

// ---- answers ----

type previewOut struct {
	DraftID     string            `json:"draft_id"`
	Revision    int               `json:"revision"`
	ByUser      bool              `json:"made_on_page,omitempty"`
	Source      string            `json:"source"`
	SourceLabel string            `json:"source_label"`
	Mode        string            `json:"mode"`
	Note        string            `json:"note,omitempty"`
	Created     time.Time         `json:"created"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Lines       []imports.Step    `json:"lines"`
	Unread      []imports.Unread  `json:"unread"`
	Totals      imports.Totals    `json:"totals"`
	Summary     string            `json:"summary"`
	Next        string            `json:"next"`
	Inferred    map[string]string `json:"inferred_words"`
}

func newPreview(d imports.Draft) previewOut {
	mode := d.Input.Mode
	if mode == "" {
		mode = imports.DefaultMode(d.Input.Source)
	}
	out := previewOut{
		DraftID: d.ID, Revision: d.Rev, ByUser: d.ByUser, Source: d.Input.Source, SourceLabel: imports.DraftLabel(d), Mode: mode,
		Note: d.Input.Note, Created: d.Created, ExpiresAt: d.Expires,
		Lines: d.Plan.Steps, Unread: d.Plan.Unread, Totals: d.Plan.Totals,
		Summary:  imports.Summary(d.Input.Source, d.Plan),
		Inferred: map[string]string{"category": "分类", "place": "位置", "expires": "过期日", "unit": "单位"},
	}
	switch {
	case d.Plan.Totals.Questions > 0:
		out.Next = fmt.Sprintf("有 %d 行要用户定：先把 summary 念给用户，逐个问清楚（question 里有问题和可选的回答），"+
			"再调用 household_import_preview 带 draft_id 和用户的回答（edits），把新的预览念给用户；用户同意后，"+
			"用新预览给的 draft_id 和 revision 调用 household_import_apply（它只收这两个参数）。", d.Plan.Totals.Questions)
	case d.Plan.Totals.Changes == 0:
		out.Next = "这份草稿没有要写入的东西，告诉用户即可；可以用 household_import_discard 放弃它。"
	default:
		out.Next = fmt.Sprintf("把 summary 简短念给用户，推测的部分要说出来。用户要改某行或不要某行时，先调用 household_import_preview "+
			"带 draft_id 和 edits（不要的行写 {\"line\": n, \"skip\": true}），把新的预览念给用户。用户同意后调用 household_import_apply，"+
			"只带 draft_id 和用户同意的那一版的 revision（现在是 %d）。没同意就不要写入。写入后可以撤销。", d.Rev)
		if d.Plan.Totals.Lowered > 0 {
			out.Next += fmt.Sprintf("有 %d 件按看到的数会比现在记的少，要特别说出来，照片没拍全的话先问用户。", d.Plan.Totals.Lowered)
		}
	}
	return out
}

type importedOut struct {
	DraftID string         `json:"draft_id"`
	Done    []string       `json:"done"`
	Totals  imports.Totals `json:"totals"`
	Summary string         `json:"summary"`
	Next    string         `json:"next"`
}

type undoneOut struct {
	DraftID  string   `json:"draft_id"`
	Undone   []string `json:"undone"`
	Restored int      `json:"restored"`
	Removed  int      `json:"removed"`
	Summary  string   `json:"summary"`
}

type draftOut struct {
	DraftID     string         `json:"draft_id"`
	Revision    int            `json:"revision"`
	ByUser      bool           `json:"made_on_page,omitempty"`
	Source      string         `json:"source"`
	SourceLabel string         `json:"source_label"`
	Note        string         `json:"note,omitempty"`
	Created     time.Time      `json:"created"`
	ExpiresAt   time.Time      `json:"expires_at"`
	Totals      imports.Totals `json:"totals"`
	Summary     string         `json:"summary"`
}

type lastOut struct {
	DraftID string         `json:"draft_id"`
	Source  string         `json:"source"`
	At      time.Time      `json:"at"`
	Done    []string       `json:"done"`
	Totals  imports.Totals `json:"totals"`
	Undone  bool           `json:"undone"`
	// UndoUnknown: an undo of it was interrupted and it is not known whether it was written.
	UndoUnknown bool   `json:"undo_unknown,omitempty"`
	CanUndo     bool   `json:"can_undo"`
	WhyNot      string `json:"why_not,omitempty"`
}

type importListOut struct {
	Drafts []draftOut `json:"drafts"`
	Last   *lastOut   `json:"last_import"`
}

// ---- runs ----

func runImportPreview(e env, raw json.RawMessage) (any, error) {
	var a struct {
		Source  string         `json:"source"`
		Note    string         `json:"note"`
		Target  string         `json:"default_target"`
		Mode    string         `json:"mode"`
		Lines   []imports.Line `json:"lines"`
		DraftID string         `json:"draft_id"`
		Edits   []imports.Edit `json:"edits"`
	}
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	h := model.Load(space(e.b), e.loc)
	if a.DraftID != "" {
		if a.Source != "" || len(a.Lines) > 0 || a.Target != "" || a.Mode != "" || a.Note != "" {
			return nil, errors.New("重新预览草稿时只带 draft_id 和 edits；要记新的东西，不带 draft_id 另做一份")
		}
		d, err := imports.Replan(e.imp, h, a.DraftID, 0, a.Edits, e.loc, false)
		if err != nil {
			return nil, err
		}
		return newPreview(d), nil
	}
	if len(a.Edits) > 0 {
		return nil, errors.New("edits 只用于重新预览已有的草稿：带 draft_id 和 edits 调用 household_import_preview")
	}
	if a.Source == "" || !slices.Contains(imports.Sources, a.Source) {
		return nil, fmt.Errorf("source 需要是 %s 之一", strings.Join(imports.Sources, "、"))
	}
	d, err := imports.Preview(e.imp, h, imports.Input{
		Source: a.Source, Note: strings.TrimSpace(a.Note), Target: a.Target, Mode: a.Mode, Lines: a.Lines,
	}, e.loc, false)
	if err != nil {
		return nil, err
	}
	return newPreview(d), nil
}

func runImportList(e env, raw json.RawMessage) (any, error) {
	if err := noArgs(raw); err != nil {
		return nil, err
	}
	out := importListOut{Drafts: []draftOut{}}
	err := e.imp.View(func(st *imports.State) error {
		st.Settle(e.b) // what an interrupted save left, read from the records
		for i := len(st.Drafts) - 1; i >= 0; i-- {
			d := st.Drafts[i]
			out.Drafts = append(out.Drafts, draftOut{
				DraftID: d.ID, Revision: d.Rev, ByUser: d.ByUser, Source: d.Input.Source, SourceLabel: imports.DraftLabel(d), Note: d.Input.Note,
				Created: d.Created, ExpiresAt: d.Expires, Totals: d.Plan.Totals, Summary: imports.Summary(d.Input.Source, d.Plan),
			})
		}
		if last, ok := st.Last(); ok {
			l := &lastOut{DraftID: last.Draft, Source: last.Source, At: last.At, Done: last.Says, Totals: last.Totals,
				Undone: !last.Undone.IsZero(), UndoUnknown: last.Unknown == imports.UnknownUndo}
			if err := imports.CheckUndo(e.b, last); err != nil {
				l.WhyNot = err.Error()
			} else {
				l.CanUndo = true
			}
			out.Last = l
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func runImportApply(e env, raw json.RawMessage) (any, error) {
	var a struct {
		DraftID  string `json:"draft_id"`
		Revision int    `json:"revision"`
	}
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if a.DraftID == "" {
		return nil, errors.New("需要草稿编号 draft_id")
	}
	if a.Revision == 0 {
		return nil, errors.New("需要 revision：用户同意的那一版预览的编号（预览结果里有）")
	}
	done, finish, err := imports.Apply(e.b, e.imp, a.DraftID, a.Revision, false, e.now, e.loc)
	if err != nil {
		return nil, err
	}
	e.onSaved(func(ok bool) { _ = finish(ok) })
	return importedOut{DraftID: done.Draft, Done: done.Says, Totals: done.Totals,
		Summary: "已写入：" + imports.TotalsText(done.Totals),
		Next:    "告诉用户写好了，并说一声：要是不对，可以说「撤销刚才的导入」，或在家庭管家页面上点「撤销这次导入」。"}, nil
}

func runImportUndo(e env, raw json.RawMessage) (any, error) {
	var a struct {
		DraftID string `json:"draft_id"`
	}
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	done, finish, err := imports.UndoLast(e.b, e.imp, a.DraftID, e.now)
	if err != nil {
		return nil, err
	}
	e.onSaved(func(ok bool) { _ = finish(ok) })
	out := undoneOut{DraftID: done.Draft, Undone: done.Says}
	for _, t := range done.Touched {
		if t.Before == nil {
			out.Removed++
		} else {
			out.Restored++
		}
	}
	out.Summary = fmt.Sprintf("已撤销：删掉这次新建的 %d 条记录，%d 条恢复成导入前的样子。", out.Removed, out.Restored)
	return out, nil
}

func runImportDiscard(e env, raw json.RawMessage) (any, error) {
	var a struct {
		DraftID string `json:"draft_id"`
	}
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if a.DraftID == "" {
		return nil, errors.New("需要草稿编号 draft_id")
	}
	d, err := imports.Discard(e.imp, a.DraftID, false)
	if err != nil {
		return nil, err
	}
	return map[string]any{"discarded": d.ID, "summary": "已放弃这份" + imports.DraftLabel(d) + "的导入，记录没有改动。"}, nil
}
