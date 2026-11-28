// Package ui draws the Token 用量 page and the overview card. The page has three tabs: the overview (totals, the
// change, the daily trend and the top models, machines and launchers), the detail (a filterable table whose rows can be
// shown as CSV text to copy) and the budgets. Everything comes from the local store; the hub is asked only for the gateway account list,
// which is hidden when it cannot be read.
package ui

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/budget"
	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/hub"
	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/store"
	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/usage"
)

const (
	PageUI = "token-usage"
	CardUI = "token-usage-card"

	tabOverview = "overview"
	tabDetail   = "detail"
	tabBudget   = "budget"

	topN = 8

	// csvMost is how many bytes of CSV the page shows at once. A code component's text holds at most 200,000
	// characters (view.schema.json CodeComponent) and a view at most wire.CodeMost bytes of code; bytes are never fewer
	// than characters, so this stays within both.
	csvMost = 190_000

	filterFieldMost = 256
	choicesMost     = 50
	budgetsMost     = 100
	tableRowsMost   = 100
)

func clipRunes(s string, most int) string {
	if most <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= most {
		return s
	}
	r := []rune(s)
	if most == 1 {
		return "…"
	}
	return string(r[:most-1]) + "…"
}

func clampRatio(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// pageState is what a page remembers between draws.
type pageState struct {
	Tab        string `json:"tab,omitempty"`
	Days       int    `json:"days,omitempty"`
	Machine    string `json:"machine,omitempty"`
	Model      string `json:"model,omitempty"`
	Launcher   string `json:"launcher,omitempty"`
	BudgetSpan string `json:"budget_span,omitempty"`
	CSV        bool   `json:"csv,omitempty"` // the detail shows its rows as CSV text
}

func (p pageState) tab() string {
	switch p.Tab {
	case tabDetail, tabBudget:
		return p.Tab
	default:
		return tabOverview
	}
}

func (p pageState) days() int {
	if p.Days == 7 || p.Days == 30 || p.Days == 90 {
		return p.Days
	}
	return 30
}

func (p pageState) budgetSpan() string {
	if p.BudgetSpan == budget.Weekly {
		return budget.Weekly
	}
	return budget.Daily
}

func (p *pageState) clean() {
	p.Tab = p.tab()
	if p.Days != 0 {
		p.Days = p.days()
	}
	if p.BudgetSpan != "" {
		p.BudgetSpan = p.budgetSpan()
	}
	p.Machine = clipRunes(p.Machine, filterFieldMost)
	p.Model = clipRunes(p.Model, filterFieldMost)
	p.Launcher = clipRunes(p.Launcher, filterFieldMost)
}

// App draws both contributions from one store.
type App struct {
	st  *store.Store
	bf  *budget.File
	hub *hub.Client
	now func() time.Time
	loc func() *time.Location
}

// New returns an app. now and loc are functions so a test can pin them.
func New(st *store.Store, bf *budget.File, h *hub.Client, now func() time.Time, loc func() *time.Location) *App {
	return &App{st: st, bf: bf, hub: h, now: now, loc: loc}
}

// Page draws the navigation page.
func (a *App) Page(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	var ps pageState
	if err := r.DecodeState(&ps); err != nil {
		ps = pageState{}
	}
	return a.renderState(ctx, ps)
}

func (a *App) renderState(ctx context.Context, ps pageState) (*wire.View, error) {
	ps.clean()
	var view *wire.View
	err := a.st.View(func(st *store.State) error {
		var err error
		view, err = a.page(ctx, st, ps)
		return err
	})
	return view, err
}

// Act answers a button.
func (a *App) Act(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	var ps pageState
	if err := r.DecodeState(&ps); err != nil {
		ps = pageState{}
	}
	switch r.Action {
	case "tab":
		ps.Tab = r.Values["tab"]
		v, err := a.renderState(ctx, ps)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{View: v}, nil
	case "range":
		ps.Days, _ = strconv.Atoi(r.Values["days"])
		v, err := a.renderState(ctx, ps)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{View: v}, nil
	case "filter":
		ps.Tab = tabDetail
		if v, ok := r.Values["machine"]; ok {
			ps.Machine = v
		}
		if v, ok := r.Values["model"]; ok {
			ps.Model = v
		}
		if v, ok := r.Values["launcher"]; ok {
			ps.Launcher = v
		}
		v, err := a.renderState(ctx, ps)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{View: v}, nil
	case "budget-span":
		ps.Tab = tabBudget
		ps.BudgetSpan = r.Values["span"]
		v, err := a.renderState(ctx, ps)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{View: v}, nil
	case "budget-set":
		if err := a.setBudget(r.Values, ps.budgetSpan()); err != nil {
			return &wire.ActionResult{Toast: err.Error()}, nil
		}
		ps.Tab = tabBudget
		v, err := a.renderState(ctx, ps)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{View: v, Toast: "已保存预算"}, nil
	case "budget-delete":
		if err := a.bf.Delete(r.Values["span"], r.Values["model"]); err != nil {
			return &wire.ActionResult{Toast: err.Error()}, nil
		}
		ps.Tab = tabBudget
		v, err := a.renderState(ctx, ps)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{View: v, Toast: "已删除预算"}, nil
	case "export", "export-close":
		ps.Tab = tabDetail
		ps.CSV = r.Action == "export"
		v, err := a.renderState(ctx, ps)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{View: v}, nil
	}
	return &wire.ActionResult{}, nil
}

func (a *App) page(ctx context.Context, st *store.State, ps pageState) (*wire.View, error) {
	now, loc := a.now(), a.loc()
	v := &wire.View{
		Title: PageTitle,
		Tabs: &wire.Tabs{
			Name:  "tab",
			Value: ps.Tab,
			Items: []wire.Tab{
				{Value: tabOverview, Title: "总览"},
				{Value: tabDetail, Title: "明细"},
				{Value: tabBudget, Title: "预算"},
			},
			Action: wire.Action{ID: "tab"},
		},
		RefreshAfter: 60,
	}
	switch ps.Tab {
	case tabDetail:
		v.Sections = a.detail(st, ps, now, loc)
	case tabBudget:
		v.Sections = a.budgets(st, ps, now, loc)
	default:
		v.Sections = a.overview(ctx, st, ps, now, loc)
	}
	if ps.Tab != tabBudget {
		v.Sections = append(a.pullNotice(st), v.Sections...)
	}
	v.State = ext.EncodeState(ps)
	return v, nil
}

// overview is the first tab: today's totals against the previous period, the daily trend and the rankings.
func (a *App) overview(ctx context.Context, st *store.State, ps pageState, now time.Time, loc *time.Location) []wire.Section {
	days := ps.days()
	r := usage.Summarize(st, now, days, loc, usage.Filter{})
	today := usage.Summarize(st, now, 1, loc, usage.Filter{})
	week := usage.Summarize(st, now, 7, loc, usage.Filter{})
	month := usage.Summarize(st, now, 30, loc, usage.Filter{})

	change := func(label string, s usage.Range) wire.Stat {
		text, tone := formatChange(s.Total.Total(), s.Before.Total())
		return wire.Stat{Label: label, Value: formatTokens(s.Total.Total()), Detail: "较上期 " + text, Tone: tone}
	}
	sections := []wire.Section{{
		Items: []wire.Component{{
			Type: wire.ComponentStatGrid,
			Stats: []wire.Stat{
				change("今天", today),
				change("7 天", week),
				change("30 天", month),
			},
		}},
	}}

	sections = append(sections, wire.Section{
		Title: "按天",
		Items: []wire.Component{
			segmented("range", "days", strconv.Itoa(days), []wire.Choice{
				{Value: "7", Title: "7 天"}, {Value: "30", Title: "30 天"}, {Value: "90", Title: "90 天"},
			}),
			trendChart(r),
		},
	})
	sections = append(sections, ranking("按模型", "个模型", named(r.Models, modelName)))
	sections = append(sections, ranking("按电脑", "台电脑", named(r.Machines, machineName)))
	launchers := ranking("按启动方式", "种启动方式", named(r.Launchers, launcherName))
	launchers.Footer = strings.TrimPrefix(launchers.Footer+"。", "。") + launcherHint
	sections = append(sections, launchers)

	if ga, ok := a.hub.GatewayAccounts(ctx); ok {
		sections = append(sections, gateways(ga))
	}
	return sections
}

// named rewrites each share's name.
func named(in []usage.Share, name func(string) string) []usage.Share {
	out := make([]usage.Share, len(in))
	for i, s := range in {
		out[i] = s
		out[i].Name = name(s.Name)
	}
	return out
}

func machineName(name string) string {
	if name == "" {
		return "（未知电脑）"
	}
	return name
}

// launcherHint says what a launcher is.
const launcherHint = "启动方式是在电脑上开启这次对话的程序；在终端里直接开的对话算作“终端直接打开”。"

// msgUnexpected is the cause told for an error that does not carry the user's words (not a *hub.Error).
const msgUnexpected = "读取用量时出了意外的问题。这里每 10 分钟会自动再试；一直这样的话，请重启 Persona。"

// userMessage is err in the user's words: a *hub.Error's message, else msgUnexpected, never Go's English text.
func userMessage(err error) string {
	var he *hub.Error
	if errors.As(err, &he) && he.Message != "" {
		return he.Message
	}
	return msgUnexpected
}

// namesMost is how many failed computers the notice names before it folds the rest into a count.
const namesMost = 5

// pullNotice is what the page says about reading the usage: why nothing can be read, or which computers the last round
// could not read; and, while there is no data yet, when the first data will come. Nothing when the data is there and
// the last round read every computer.
func (a *App) pullNotice(st *store.State) []wire.Section {
	r := a.hub.LastPull()
	when := ""
	if !r.At.IsZero() {
		when = "最近一次尝试在 " + r.At.In(a.loc()).Format("1月2日 15:04") + "。"
	}
	before := ""
	if len(st.Days) > 0 {
		before = "下面是之前已经读到的数据。"
	}
	notice := func(title, detail string) []wire.Section {
		return []wire.Section{{Items: []wire.Component{{
			Type: wire.ComponentEmpty, Icon: "warning", Title: clipRunes(title, 200), Detail: clipRunes(detail, 1800),
		}}}}
	}
	if r.Err != nil {
		return notice("现在读不到用量", userMessage(r.Err)+when+before)
	}
	if n := len(r.Failed); n > 0 {
		names := make([]string, 0, namesMost)
		gone, other := 0, error(nil)
		for i, f := range r.Failed {
			if i < namesMost {
				names = append(names, clipRunes(f.Name, 80))
			}
			if hub.NotFound(f.Err) {
				gone++
			} else if other == nil {
				other = f.Err
			}
		}
		list := strings.Join(names, "、")
		if n > namesMost {
			list += fmt.Sprintf(" 等 %d 台", n)
		}
		var title, detail string
		if n == r.Total {
			title = "这次一台电脑的用量都没读到"
			detail = "没读到：" + list + "。"
		} else {
			title = fmt.Sprintf("有 %d 台电脑的用量没读到", n)
			detail = fmt.Sprintf("没读到：%s（共 %d 台电脑，其余的已照常更新）。", list, r.Total)
		}
		switch {
		case gone == n:
			if n == 1 {
				detail += "Persona 里已经找不到这台电脑，可能它被移除了；以前读到的用量还留在这里。"
			} else {
				detail += "Persona 里已经找不到这些电脑，可能它们被移除了；以前读到的用量还留在这里。"
			}
		case n == r.Total:
			detail += userMessage(other)
		default:
			detail += "没读到的电脑 10 分钟后会自动再试，不用做别的。"
			if gone > 0 {
				detail += "其中有的电脑在 Persona 里已经找不到，可能被移除了。"
			}
		}
		return notice(title, detail+when)
	}
	if len(st.Days) > 0 {
		return nil
	}
	detail := "这里会显示连着 Persona 的每台电脑用 AI 模型花掉的 token。扩展开着时每 10 分钟读一次，第一次会补上最近 7 天，刚装好请等一两分钟再来看。"
	if !r.At.IsZero() {
		detail = "已经读过一次，但最近 7 天里这些电脑还没有用过 AI 模型。用过之后，最多 10 分钟就会出现在这里。"
	}
	return []wire.Section{{Items: []wire.Component{{
		Type: wire.ComponentEmpty, Icon: "chart", Title: "还没有用量", Detail: detail,
	}}}}
}

// trendChart is the daily bar chart. A range longer than the chart allows is folded into weeks.
func trendChart(r usage.Range) wire.Component {
	points := make([]wire.ChartPoint, 0, len(r.Trend))
	if len(r.Trend) > 60 {
		var sum int64
		var from string
		flush := func(to string) {
			if from == "" {
				return
			}
			points = append(points, wire.ChartPoint{Label: from[5:] + "~", Value: float64(sum), Display: formatTokens(sum)})
			_ = to
			sum, from = 0, ""
		}
		for i, d := range r.Trend {
			if from == "" {
				from = d.Date
			}
			sum += d.Total()
			if (i+1)%7 == 0 {
				flush(d.Date)
			}
		}
		flush("")
	} else {
		for _, d := range r.Trend {
			points = append(points, wire.ChartPoint{Label: d.Date[5:], Value: float64(d.Total()), Display: formatTokens(d.Total())})
		}
	}
	if len(points) == 0 {
		points = []wire.ChartPoint{{Label: "-", Value: 0, Display: "0"}}
	}
	return wire.Component{Type: wire.ComponentChart, Kind: "bar", Series: []wire.ChartSeries{{Name: "token", Points: points}}}
}

// ranking is the top entries of one dimension, the rest folded into 其他. unit counts them (个模型).
func ranking(title, unit string, shares []usage.Share) wire.Section {
	if len(shares) == 0 {
		return wire.Section{Title: title, Items: []wire.Component{{Type: wire.ComponentText, Style: "secondary",
			Text: "选的这段时间里没有用量。"}}}
	}
	var total int64
	for _, s := range shares {
		total += s.Total()
	}
	shown := shares
	var rest int64
	if len(shares) > topN {
		shown = shares[:topN]
		for _, s := range shares[topN:] {
			rest += s.Total()
		}
	}
	rows := make([]wire.Component, 0, len(shown)+1)
	for _, s := range shown {
		ratio := 0.0
		if total > 0 {
			ratio = clampRatio(float64(s.Total()) / float64(total))
		}
		rows = append(rows, wire.Component{
			Type: wire.ComponentProgress, Label: clipRunes(s.Name, 200), Detail: formatTokens(s.Total()), Ratio: &ratio,
		})
	}
	if rest > 0 && total > 0 {
		ratio := clampRatio(float64(rest) / float64(total))
		rows = append(rows, wire.Component{Type: wire.ComponentProgress, Label: "其他", Detail: formatTokens(rest), Ratio: &ratio})
	}
	return wire.Section{Title: title, Footer: fmt.Sprintf("共 %d %s", len(shares), unit), Items: rows}
}

// detail is the filterable table.
func (a *App) detail(st *store.State, ps pageState, now time.Time, loc *time.Location) []wire.Section {
	models, machines, launchers := usage.Names(st)
	f := usage.Filter{Machine: ps.Machine, Model: ps.Model, Launcher: ps.Launcher}
	r := usage.Summarize(st, now, ps.days(), loc, f)
	rows := make([][]string, 0, len(r.Trend))
	for i := len(r.Trend) - 1; i >= 0; i-- {
		d := r.Trend[i]
		if d.Total() == 0 {
			continue
		}
		rows = append(rows, []string{d.Date, formatTokens(d.Total()), formatTokens(d.Input), formatTokens(d.Output), formatTokens(d.CacheRead), strconv.Itoa(d.Requests)})
	}
	items := []wire.Component{
		segmented("range", "days", strconv.Itoa(ps.days()), []wire.Choice{
			{Value: "7", Title: "7 天"}, {Value: "30", Title: "30 天"}, {Value: "90", Title: "90 天"},
		}),
		choice("machine", "电脑", ps.Machine, machines),
		choice("model", "模型", ps.Model, models),
		choice("launcher", "启动方式", ps.Launcher, append([]string{""}, launchers...)),
		{Type: wire.ComponentButton, Title: "应用筛选", Action: &wire.Action{ID: "filter", Submit: true}},
	}
	switch {
	case len(rows) > 0:
		items = append(items, wire.Component{
			Type: wire.ComponentTable,
			Columns: []wire.TableColumn{
				{Title: "日期"}, {Title: "总量", Align: "end"}, {Title: "输入", Align: "end"},
				{Title: "输出", Align: "end"}, {Title: "缓存读取", Align: "end"}, {Title: "请求次数", Align: "end"},
			},
			Rows: rows,
		})
	case f != usage.Filter{}:
		items = append(items, wire.Component{Type: wire.ComponentEmpty, Icon: "search", Title: "没有符合筛选条件的用量",
			Detail: fmt.Sprintf("最近 %d 天里，按现在选的电脑、模型和启动方式筛选后没有用量。可以把时间范围改长，或把筛选改回“全部”再点“应用筛选”。", ps.days())})
	default:
		items = append(items, wire.Component{Type: wire.ComponentEmpty, Icon: "chart", Title: "这段时间里没有用量",
			Detail: fmt.Sprintf("最近 %d 天里还没有用量。电脑上用过 AI 模型后，最多 10 分钟会出现在这里。", ps.days())})
	}
	sections := []wire.Section{{Items: items}}
	if !ps.CSV {
		sections[0].Items = append(sections[0].Items, wire.Component{Type: wire.ComponentButton, Title: "显示 CSV 文本（可复制到表格软件）", Action: &wire.Action{ID: "export"}})
		return sections
	}
	return append(sections, a.csvSection(st, ps, now, loc))
}

// choice is a drop-down that filters one dimension. The empty value keeps all of them.
func choice(name, title, value string, options []string) wire.Component {
	choices := []wire.Choice{{Value: "", Title: "全部" + title}}
	seen := map[string]bool{"": true}
	for _, o := range options {
		if o == "" || seen[o] || utf8.RuneCountInString(o) > wire.StringMost {
			continue
		}
		if len(choices) >= choicesMost {
			break
		}
		seen[o] = true
		choices = append(choices, wire.Choice{Value: o, Title: clipRunes(o, 120)})
	}
	return wire.Component{
		Type: wire.ComponentField, Kind: "choice", Name: name, Label: title, Value: clipRunes(value, filterFieldMost), Choices: choices,
	}
}

func segmented(action, name, value string, choices []wire.Choice) wire.Component {
	return wire.Component{Type: wire.ComponentSegmented, Name: name, Value: value, Choices: choices, Action: &wire.Action{ID: action}}
}

// csvHeader is the CSV's first line.
var csvHeader = []string{"日期", "电脑", "模型", "启动方式", "输入", "缓存写入", "缓存读取", "输出", "请求次数"}

// csvRows is the detail's range and filters as CSV records, one per day, machine, model and launcher, oldest first.
func csvRows(st *store.State, ps pageState, now time.Time, loc *time.Location) [][]string {
	from := now.In(loc).AddDate(0, 0, -(ps.days() - 1)).Format("2006-01-02")
	to := now.In(loc).Format("2006-01-02")
	f := usage.Filter{Machine: ps.Machine, Model: ps.Model, Launcher: ps.Launcher}
	type key struct{ date, machine, model, launcher string }
	sums := map[key]*store.Bucket{}
	for _, d := range st.Days {
		if d.Date < from || d.Date > to {
			continue
		}
		for _, b := range d.Buckets {
			if !f.Keeps(d.Machine, b) {
				continue
			}
			k := key{d.Date, d.Machine, b.Model, b.Launcher}
			s := sums[k]
			if s == nil {
				s = &store.Bucket{}
				sums[k] = s
			}
			s.Input += b.Input
			s.CacheWrite += b.CacheWrite
			s.CacheRead += b.CacheRead
			s.Output += b.Output
			s.Requests += b.Requests
		}
	}
	keys := make([]key, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.date != b.date {
			return a.date < b.date
		}
		if a.machine != b.machine {
			return a.machine < b.machine
		}
		if a.model != b.model {
			return a.model < b.model
		}
		return a.launcher < b.launcher
	})
	out := make([][]string, 0, len(keys))
	for _, k := range keys {
		s := sums[k]
		out = append(out, []string{k.date, machineName(k.machine), modelName(k.model), launcherName(k.launcher),
			strconv.FormatInt(s.Input, 10), strconv.FormatInt(s.CacheWrite, 10),
			strconv.FormatInt(s.CacheRead, 10), strconv.FormatInt(s.Output, 10), strconv.Itoa(s.Requests)})
	}
	return out
}

// csvText writes the header and as many records as fit in csvMost bytes; shown is how many records it holds.
func csvText(records [][]string) (text string, shown int) {
	var buf strings.Builder
	line := func(rec []string) string {
		var b strings.Builder
		w := csv.NewWriter(&b)
		_ = w.Write(rec)
		w.Flush()
		return b.String()
	}
	buf.WriteString(line(csvHeader))
	for _, rec := range records {
		l := line(rec)
		if buf.Len()+len(l) > csvMost {
			break
		}
		buf.WriteString(l)
		shown++
	}
	return strings.TrimSuffix(buf.String(), "\n"), shown
}

// csvSection shows the detail's records as CSV text in a code block. The apps cannot be handed a file: an action
// answers with a view, a toast, a link, a page or lines (wire.ActionResult), so the text is on the page to copy.
func (a *App) csvSection(st *store.State, ps pageState, now time.Time, loc *time.Location) wire.Section {
	records := csvRows(st, ps, now, loc)
	from := now.In(loc).AddDate(0, 0, -(ps.days() - 1)).Format("2006-01-02")
	to := now.In(loc).Format("2006-01-02")
	text, shown := csvText(records)
	intro := fmt.Sprintf("最近 %d 天、按上面的筛选，共 %d 行，每天每台电脑的每个模型和启动方式各一行，数字是 token 数。"+
		"把下面的文字全部复制，粘贴进表格软件，或存成扩展名为 .csv 的文件再打开。", ps.days(), len(records))
	if len(records) == 0 {
		intro = fmt.Sprintf("最近 %d 天、按上面的筛选没有用量，下面只有表头。", ps.days())
	}
	code := wire.Component{Type: wire.ComponentCode, Key: "csv", Path: "token-usage-" + from + "-" + to + ".csv",
		Language: "csv", Text: text, StartLine: 1}
	items := []wire.Component{{Type: wire.ComponentText, Text: intro}}
	if shown < len(records) {
		code.Truncated = true
		items = append(items, wire.Component{Type: wire.ComponentText, Style: "secondary",
			Text: fmt.Sprintf("内容太长，这里只显示了前 %d 行（共 %d 行）。把时间范围改短或加上筛选，可以分几次复制完。", shown, len(records))})
	}
	items = append(items, code,
		wire.Component{Type: wire.ComponentButton, Title: "收起 CSV 文本", Action: &wire.Action{ID: "export-close"}})
	return wire.Section{Title: "CSV 文本", Items: items}
}

// budgets is the third tab: the budgets and their progress, and the form for a new one.
func (a *App) budgets(st *store.State, ps pageState, now time.Time, loc *time.Location) []wire.Section {
	statuses, err := a.bf.Statuses(st, now, loc)
	items := []wire.Component{}
	if err != nil {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: "预算读不出来：" + err.Error()})
	}
	if len(statuses) == 0 {
		items = append(items, wire.Component{Type: wire.ComponentEmpty, Icon: "money", Title: "还没有设预算",
			Detail: "在下面选一个模型（或全部模型）和每日或每周，填上 token 上限，再点“保存预算”。用到八成和用满时，会各提醒你一次。"})
	}
	shownStatuses := statuses
	if len(shownStatuses) > budgetsMost {
		shownStatuses = statuses[:budgetsMost]
	}
	for _, s := range shownStatuses {
		who := "全部模型"
		if s.Budget.Model != "" {
			who = clipRunes(modelName(s.Budget.Model), 200)
		}
		span := "每日"
		if s.Budget.Span == budget.Weekly {
			span = "每周"
		}
		text, tone := formatRatio(s.Used, s.Budget.Limit)
		ratio := clampRatio(s.Ratio)
		items = append(items, wire.Component{
			Type: wire.ComponentProgress, Label: span + " · " + who, Tone: tone,
			Detail: formatTokens(s.Used) + " / " + formatTokens(s.Budget.Limit) + "（" + text + "）", Ratio: &ratio,
		})
		items = append(items, wire.Component{
			Type: wire.ComponentButton, Title: "删除" + who, Role: "destructive",
			Action: &wire.Action{ID: "budget-delete", Confirm: "删除这个预算？",
				Values: map[string]string{"span": s.Budget.Span, "model": s.Budget.Model}},
		})
	}
	if len(statuses) > budgetsMost {
		items = append(items, wire.Component{
			Type:  wire.ComponentText,
			Style: "secondary",
			Text:  fmt.Sprintf("只显示前 %d 条预算（共 %d 条）。", budgetsMost, len(statuses)),
		})
	}
	models, _, _ := usage.Names(st)
	modelChoices := []wire.Choice{{Value: "", Title: "全部模型"}}
	for _, m := range models {
		if m != "" && utf8.RuneCountInString(m) <= wire.StringMost {
			if len(modelChoices) >= choicesMost {
				break
			}
			modelChoices = append(modelChoices, wire.Choice{Value: m, Title: clipRunes(m, 120)})
		}
	}
	form := []wire.Component{
		{Type: wire.ComponentField, Kind: "choice", Name: "model", Label: "范围", Choices: modelChoices},
		{Type: wire.ComponentSegmented, Name: "span", Value: ps.budgetSpan(), Choices: []wire.Choice{
			{Value: budget.Daily, Title: "每日"}, {Value: budget.Weekly, Title: "每周"},
		}, Action: &wire.Action{ID: "budget-span"}},
		{Type: wire.ComponentField, Kind: "number", Name: "limit", Label: "上限（token）", Placeholder: "1000000"},
		{Type: wire.ComponentButton, Title: "保存预算", Action: &wire.Action{ID: "budget-set", Submit: true}},
	}
	return []wire.Section{{Title: "当前预算", Items: items}, {Title: "新增或修改", Items: form}}
}

// setBudget reads the form and stores the budget.
func (a *App) setBudget(values map[string]string, fallbackSpan string) error {
	limit, err := strconv.ParseInt(strings.TrimSpace(values["limit"]), 10, 64)
	if err != nil {
		return fmt.Errorf("上限要填一个整数")
	}
	span := values["span"]
	if span == "" {
		span = fallbackSpan
	}
	if span == "" {
		span = budget.Daily
	}
	return a.bf.Set(budget.Budget{Model: values["model"], Span: span, Limit: limit})
}

// gateways is the account list, shown only when the hub could provide it.
func gateways(ga hub.GatewayAccounts) wire.Section {
	if len(ga.Accounts) == 0 {
		return wire.Section{
			Title: "网关账号额度",
			Items: []wire.Component{{Type: wire.ComponentText, Style: "secondary", Text: "暂无网关账号额度信息。"}},
		}
	}
	shown := ga.Accounts
	if len(shown) > tableRowsMost {
		shown = ga.Accounts[:tableRowsMost]
	}
	rows := make([][]string, 0, len(shown))
	for _, ac := range shown {
		window := "—"
		if len(ac.Windows) > 0 && ac.Windows[0].UsedRatio != nil {
			if r := *ac.Windows[0].UsedRatio; !math.IsNaN(r) && !math.IsInf(r, 0) {
				window = fmt.Sprintf("%.0f%%", r*100)
			}
		}
		rows = append(rows, []string{clipRunes(ac.Provider, 120), clipRunes(ac.Name, 120), clipRunes(ac.Status, 120), window})
	}
	footer := ""
	if len(ga.Accounts) > tableRowsMost {
		footer = fmt.Sprintf("只显示前 %d 个账号（共 %d 个）。", tableRowsMost, len(ga.Accounts))
	}
	return wire.Section{Title: "网关账号额度", Footer: footer, Items: []wire.Component{{
		Type: wire.ComponentTable,
		Columns: []wire.TableColumn{
			{Title: "来源"}, {Title: "账号"}, {Title: "状态"}, {Title: "已用", Align: "end"},
		},
		Rows: rows,
	}}}
}
