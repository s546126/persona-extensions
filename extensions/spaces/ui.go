package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The contributions' ids (persona-extension.json).
const (
	pageUI           = "spaces"
	cardUI           = "spaces-card"
	widgetUI         = "spaces-widget"
	captureShortcut  = "spaces-capture"
	progressShortcut = "spaces-progress"
)

// The four tabs of a space, as Muse (Glint) has them.
const (
	tabActivity = "activity"
	tabIdeas    = "ideas"
	tabGoals    = "goals"
	tabLibrary  = "library"
)

var tabTitles = map[string]string{tabActivity: "动态", tabIdeas: "点子", tabGoals: "目标", tabLibrary: "资源库"}

var tabOrder = []string{tabActivity, tabIdeas, tabGoals, tabLibrary}

// Pages pushed on top of a space's tabs.
const (
	pageEntity = "entity"
	pageEdit   = "edit"
	pageSpaces = "spaces"
	pageType   = "type"
)

const (
	pageSize  = 20
	uiActor   = "你"
	noneValue = "-"
	newSpace  = "__new"
	manage    = "__manage"
)

type app struct {
	mu    sync.Mutex
	store *Store
	logf  func(string, ...any)
}

func newApp(dir string) *app {
	return &app{store: openStore(dir, nil), logf: func(string, ...any) {}}
}

func (a *app) init(_ context.Context, in *ext.Init, _ *ext.Hub) (map[string]any, error) {
	dir := in.Info().DataDir
	if dir == "" {
		return nil, nil
	}
	a.mu.Lock()
	if a.store.dir != dir {
		a.store = openStore(dir, a.logf)
	}
	a.mu.Unlock()
	rememberDataDir(dir, a.logf)
	return nil, nil
}

func (a *app) db() *Store {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.store
}

// uiState is the page's state.
type uiState struct {
	P     string `json:"p,omitempty"`
	Tab   string `json:"tab,omitempty"`
	Type  string `json:"type,omitempty"`
	ID    string `json:"id,omitempty"`
	Space string `json:"space,omitempty"`
	Q     string `json:"q,omitempty"`
	Shown int    `json:"n,omitempty"`
}

func decode(r *ext.UIRequest) uiState {
	var s uiState
	if err := r.DecodeState(&s); err != nil {
		return uiState{}
	}
	if tabTitles[s.Tab] == "" {
		s.Tab = tabActivity
	}
	return s
}

func shown(s uiState) int {
	if s.Shown <= 0 {
		return pageSize
	}
	return s.Shown
}

func (a *app) render(_ context.Context, r *ext.UIRequest) (*wire.View, error) {
	return a.draw(r, decode(r)), nil
}

// draw draws the page for state s, fitted to the app's UI version.
func (a *app) draw(r *ext.UIRequest, s uiState) *wire.View {
	var v *wire.View
	a.db().read(func(st *State) {
		switch s.P {
		case pageEntity:
			v = entityView(st, r, s)
		case pageEdit:
			v = editView(st, r, s)
		case pageSpaces:
			v = spacesView(st, r, s)
		case pageType:
			v = typeView(st, r, s)
		default:
			v = mainView(st, r, s)
		}
	})
	if v.State == "" {
		v.State = ext.EncodeState(s)
	}
	if s.P != "" && r.UIVersion > 0 && r.UIVersion < 3 { // drawn in place on an older app: a way back
		v.Sections = append(v.Sections, wire.Section{Items: []wire.Component{
			{Type: wire.ComponentButton, Title: "返回空间", Action: &wire.Action{ID: "back"}}}})
	}
	return adapt(v, r.UIVersion)
}

// ---- The space and its four tabs ----

func mainView(st *State, r *ext.UIRequest, s uiState) *wire.View {
	sp := st.activeSpace()
	if sp == nil {
		return &wire.View{Title: "空间", Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty,
			Title: "还没有空间", Icon: wire.NavIconFolder,
			Button: &wire.EmptyButton{Title: "创建空间", Action: *ext.Open("open-spaces", "", uiState{P: pageSpaces}, "空间")}}}}}}
	}
	v := &wire.View{Title: sp.Name}
	choices := []wire.Choice{}
	for _, x := range st.liveSpaces() {
		choices = append(choices, wire.Choice{Value: x.ID, Title: x.Name})
	}
	choices = append(choices, wire.Choice{Value: newSpace, Title: "+ 创建空间"}, wire.Choice{Value: manage, Title: "管理空间…"})
	v.Filter = &wire.Filter{Name: "space", Title: "空间", Choices: choices, Value: sp.ID, Action: wire.Action{ID: "space"}}
	tabs := &wire.Tabs{Name: "tab", Value: s.Tab, Action: wire.Action{ID: "tab"}}
	for _, t := range tabOrder {
		tab := wire.Tab{Value: t, Title: tabTitles[t]}
		switch t {
		case tabIdeas:
			tab.Badge = countBadge(sp.countStatus(TypeIdea, IdeaInbox))
		case tabGoals:
			tab.Badge = countBadge(sp.countStatus(TypeGoal, GoalActive))
		}
		tabs.Items = append(tabs.Items, tab)
	}
	v.Tabs = tabs
	head := wire.Section{Items: []wire.Component{{Type: wire.ComponentText, Style: wire.TextStyleSecondary,
		Text: truncate(fmt.Sprintf("%s · %d 个对象类型 · %d 个实体 · %d 条关系", firstNonEmpty(sp.Description, "空间"),
			len(sp.Types), len(sp.Entities)-sp.count(TypeFeed, false), len(sp.Relations)), 2000)}}}
	v.Sections = append(v.Sections, head)
	switch s.Tab {
	case tabIdeas:
		v.Sections = append(v.Sections, ideasTab(sp, r, s)...)
	case tabGoals:
		v.Sections = append(v.Sections, goalsTab(sp, r, s)...)
	case tabLibrary:
		v.Search = &wire.Search{Placeholder: "搜索标题、标签、属性", Value: s.Q, Action: wire.Action{ID: "search"}}
		v.Sections = append(v.Sections, libraryTab(sp, r, s)...)
	default:
		v.Sections = append(v.Sections, activityTab(sp, r, s)...)
	}
	return v
}

func countBadge(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func (sp *Space) countStatus(typ, status string) int {
	n := 0
	for _, e := range sp.Entities {
		if e.Type == typ && e.Status == status && !e.Archived {
			n++
		}
	}
	return n
}

func location(r *ext.UIRequest) *time.Location {
	if r != nil && r.TZ != "" {
		if loc, err := time.LoadLocation(r.TZ); err == nil {
			return loc
		}
	}
	return time.Local
}

func ago(t time.Time, now time.Time, loc *time.Location) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	}
	return t.In(loc).Format("01-02 15:04")
}

var eventIcons = map[string]string{
	"created": wire.NavIconCheck, "updated": wire.NavIconDoc, "status_changed": wire.NavIconBranch,
	"incubated": wire.NavIconMerge, "deleted": wire.NavIconWarning, "archived": wire.NavIconFolder,
	"note": wire.NavIconChat, "related": wire.NavIconLink, "schema_upserted": wire.NavIconSettings,
	"space_created": wire.NavIconFolder, "space_archived": wire.NavIconFolder,
}

var eventTitles = map[string]string{
	"created": "新建", "updated": "更新", "status_changed": "流转", "incubated": "孵化", "deleted": "删除",
	"archived": "归档", "note": "动态", "related": "关系", "schema_upserted": "本体", "space_created": "空间",
	"space_archived": "空间",
}

func feedEntries(sp *Space) []*Entity {
	out, _ := sp.query(Query{Type: TypeFeed, IncludeArchived: true})
	return out
}

func activityTab(sp *Space, r *ext.UIRequest, s uiState) []wire.Section {
	loc := location(r)
	now := time.Now()
	feed := feedEntries(sp)
	today := 0
	y, m, d := now.In(loc).Date()
	for _, e := range feed {
		ey, em, ed := e.CreatedAt.In(loc).Date()
		if ey == y && em == m && ed == d {
			today++
		}
	}
	stats := wire.Component{Type: wire.ComponentStatGrid, Key: "activity-stats", Stats: []wire.Stat{
		{Label: "总动态", Value: strconv.Itoa(len(feed))},
		{Label: "今日新增", Value: strconv.Itoa(today), Tone: toneIf(today > 0, wire.TonePositive)},
		{Label: "点子数", Value: strconv.Itoa(sp.count(TypeIdea, true))},
		{Label: "目标与资源数", Value: strconv.Itoa(sp.count(TypeGoal, true) + sp.count(TypeResource, true))},
	}}
	post := wire.Section{Title: "发一条动态", Items: []wire.Component{
		{Type: wire.ComponentField, Name: "note", Label: "动态", Kind: wire.FieldKindText, Placeholder: "进展、想法、刚做完的事…"},
		{Type: wire.ComponentButton, Title: "发布", Action: &wire.Action{ID: "post", Submit: true}},
	}}
	n := min(shown(s), len(feed))
	rows := make([]wire.Component, 0, n+1)
	for _, e := range feed[:n] {
		rows = append(rows, feedRow(sp, e, now, loc))
	}
	if len(feed) == 0 {
		rows = append(rows, wire.Component{Type: wire.ComponentEmpty, Title: "还没有动态", Detail: "新建、更新、孵化都会记在这里",
			Icon: wire.NavIconBell})
	}
	stream := wire.Section{Title: fmt.Sprintf("动态流 · %d 条", len(feed)), Items: rows}
	if n < len(feed) {
		stream.More = &wire.More{Action: wire.Action{ID: "more"}}
	}
	return []wire.Section{{Items: []wire.Component{stats}}, post, stream}
}

func feedRow(sp *Space, e *Entity, now time.Time, loc *time.Location) wire.Component {
	ev, _ := e.Properties["event_type"].(string)
	actor, _ := e.Properties["actor"].(string)
	summary, _ := e.Properties["summary"].(string)
	icon := eventIcons[ev]
	if icon == "" {
		icon = wire.NavIconBell
	}
	sub := ago(e.CreatedAt, now, loc)
	if actor != "" {
		sub = actor + " · " + sub
	}
	row := wire.Component{Type: wire.ComponentRow, Key: "feed-" + e.ID, Title: truncate(firstNonEmpty(summary, e.Title, "动态"), 200),
		Subtitle: sub, Icon: icon}
	if t := eventTitles[ev]; t != "" {
		tone := wire.ToneNeutral
		switch ev {
		case "incubated", "created":
			tone = wire.TonePositive
		case "deleted":
			tone = wire.ToneDanger
		}
		row.Badge = &wire.Badge{Text: t, Tone: tone}
	}
	target, _ := e.Properties["target_id"].(string)
	if _, ok := sp.entity(target); ok {
		row.Action = ext.Open("open:"+target, "", uiState{P: pageEntity, ID: target, Space: sp.ID}, sp.title(target))
	}
	return row
}

func toneIf(ok bool, tone string) string {
	if ok {
		return tone
	}
	return ""
}

var priorityChoices = []wire.Choice{{Value: noneValue, Title: "未定"}, {Value: "高", Title: "高"}, {Value: "中", Title: "中"}, {Value: "低", Title: "低"}}

func ideasTab(sp *Space, r *ext.UIRequest, s uiState) []wire.Section {
	capture := wire.Section{Title: "记个点子", Items: []wire.Component{
		{Type: wire.ComponentField, Name: "title", Label: "点子", Kind: wire.FieldKindText, Placeholder: "一句话说清楚"},
		{Type: wire.ComponentField, Name: "summary", Label: "说明", Kind: wire.FieldKindText, Placeholder: "为什么值得做（可不填）"},
		{Type: wire.ComponentField, Name: "priority", Label: "优先级", Kind: wire.FieldKindChoice, Value: "中", Choices: priorityChoices},
		{Type: wire.ComponentField, Name: "tags", Label: "标签", Kind: wire.FieldKindText, Placeholder: "用逗号分隔"},
		{Type: wire.ComponentButton, Title: "记下", Action: &wire.Action{ID: "idea-add", Submit: true}},
	}}
	out := []wire.Section{capture}
	for _, status := range []string{IdeaInbox, IdeaExploring, IdeaIncubated} {
		ideas, total := sp.query(Query{Type: TypeIdea, Status: status, IncludeArchived: true, Limit: 12})
		items := []wire.Component{}
		for _, e := range ideas {
			row := entityRow(sp, e)
			if status == IdeaIncubated {
				if id, ok := e.Properties["incubated_target_id"].(string); ok {
					if child, ok := sp.entity(id); ok {
						row.Subtitle = "→ " + sp.typeLabel(child.Type) + "：" + child.Title
					}
				}
			}
			items = append(items, row)
			switch status {
			case IdeaInbox:
				items = append(items, button("开始探索", "idea-explore:"+e.ID))
				fallthrough
			case IdeaExploring:
				items = append(items, button("孵化为目标", "idea-goal:"+e.ID), button("转化为资源", "idea-resource:"+e.ID),
					wire.Component{Type: wire.ComponentButton, Title: "归档", Action: &wire.Action{ID: "archive:" + e.ID}})
			}
		}
		if total == 0 {
			items = append(items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: "暂无"})
		}
		sec := wire.Section{Title: fmt.Sprintf("%s · %d", statusLabel(status), total), Items: items}
		if total > len(ideas) {
			sec.Footer = fmt.Sprintf("还有 %d 条，在资源库里按「点子」查看", total-len(ideas))
		}
		out = append(out, sec)
	}
	if n := archivedCount(sp, TypeIdea); n > 0 {
		out[len(out)-1].Footer = strings.TrimSpace(out[len(out)-1].Footer + fmt.Sprintf(" 已归档 %d 条。", n))
	}
	return out
}

func archivedCount(sp *Space, typ string) int {
	n := 0
	for _, e := range sp.Entities {
		if e.Type == typ && e.Archived {
			n++
		}
	}
	return n
}

func button(title, id string) wire.Component {
	return wire.Component{Type: wire.ComponentButton, Title: title, Action: &wire.Action{ID: id}}
}

func goalsTab(sp *Space, r *ext.UIRequest, s uiState) []wire.Section {
	goals, _ := sp.query(Query{Type: TypeGoal})
	sort.SliceStable(goals, func(i, j int) bool { return goalOrder(goals[i]) < goalOrder(goals[j]) })
	active, achieved := 0, 0
	sum := 0.0
	for _, g := range goals {
		switch g.Status {
		case GoalActive:
			active++
		case GoalAchieved:
			achieved++
		}
		sum += goalRatio(g)
	}
	avg := 0
	if len(goals) > 0 {
		avg = int(math.Round(sum / float64(len(goals)) * 100))
	}
	edges := 0
	for _, rel := range sp.Relations {
		if a, ok := sp.entity(rel.FromID); ok && a.Type == TypeGoal {
			edges++
		} else if b, ok := sp.entity(rel.ToID); ok && b.Type == TypeGoal {
			edges++
		}
	}
	stats := wire.Component{Type: wire.ComponentStatGrid, Key: "goal-stats", Stats: []wire.Stat{
		{Label: "进行中目标", Value: strconv.Itoa(active)},
		{Label: "已达成", Value: strconv.Itoa(achieved), Tone: toneIf(achieved > 0, wire.TonePositive)},
		{Label: "平均达成率", Value: fmt.Sprintf("%d%%", avg)},
		{Label: "关联关系数", Value: strconv.Itoa(edges)},
	}}
	items := []wire.Component{}
	if len(goals) > 25 {
		goals = goals[:25]
	}
	for _, g := range goals {
		items = append(items, entityRow(sp, g), goalProgress(g))
		if g.Status != GoalAchieved {
			items = append(items, button("推进进度 +10%", "goal-step:"+g.ID), button("标记达成", "goal-done:"+g.ID))
		}
	}
	if len(goals) == 0 {
		items = append(items, wire.Component{Type: wire.ComponentEmpty, Title: "还没有目标", Detail: "在下面新建，或者把点子孵化为目标",
			Icon: wire.NavIconGauge})
	}
	form := wire.Section{Title: "新建目标", Items: []wire.Component{
		{Type: wire.ComponentField, Name: "title", Label: "目标", Kind: wire.FieldKindText, Placeholder: "可度量的结果"},
		{Type: wire.ComponentField, Name: "target_value", Label: "目标值", Kind: wire.FieldKindNumber, Value: "100"},
		{Type: wire.ComponentField, Name: "current_value", Label: "当前值", Kind: wire.FieldKindNumber, Value: "0"},
		{Type: wire.ComponentField, Name: "unit", Label: "单位", Kind: wire.FieldKindText, Value: "%"},
		{Type: wire.ComponentField, Name: "deadline", Label: "截止日期", Kind: wire.FieldKindText, Placeholder: "YYYY-MM-DD"},
		{Type: wire.ComponentField, Name: "priority", Label: "优先级", Kind: wire.FieldKindChoice, Value: "中", Choices: priorityChoices},
		{Type: wire.ComponentButton, Title: "创建目标", Action: &wire.Action{ID: "goal-add", Submit: true}},
	}}
	return []wire.Section{{Items: []wire.Component{stats}}, {Title: "目标", Items: items}, form}
}

func goalOrder(e *Entity) int {
	switch e.Status {
	case GoalActive:
		return 0
	case GoalPlanned:
		return 1
	case GoalAchieved:
		return 2
	}
	return 3
}

func goalProgress(g *Entity) wire.Component {
	ratio := goalRatio(g)
	tone := ""
	if g.Status == GoalAchieved {
		tone = wire.TonePositive
	}
	return wire.Component{Type: wire.ComponentProgress, Key: "progress-" + g.ID, Label: truncate(g.Title, 60), Ratio: &ratio,
		Detail: goalDetail(g), Tone: tone}
}

func goalDetail(g *Entity) string {
	unit, _ := g.Properties["unit"].(string)
	cur, _ := number(g.Properties["current_value"])
	target, _ := number(g.Properties["target_value"])
	d := fmt.Sprintf("%s / %s %s · %d%%", fmtNum(cur), fmtNum(target), unit, int(math.Round(goalRatio(g)*100)))
	if dl, ok := g.Properties["deadline"].(string); ok {
		d += " · 截止 " + dl
	}
	return d
}

func fmtNum(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }

// entityRow is an entity as a row of a list: its key properties, its edges, its status; it opens the detail.
func entityRow(sp *Space, e *Entity) wire.Component {
	t, _ := sp.objectType(e.Type)
	icon := wire.NavIconList
	var parts []string
	if t != nil {
		icon = t.Icon
		for _, f := range t.Fields {
			v, ok := e.Properties[f.Key]
			if !ok || f.Key == "incubated_target_id" {
				continue
			}
			s := formatValue(f, v)
			if f.Kind == KindRelation {
				s = sp.title(s)
			}
			parts = append(parts, f.Label+"："+truncate(s, 40))
			if len(parts) == 3 {
				break
			}
		}
	}
	if len(e.Tags) > 0 {
		parts = append(parts, "#"+strings.Join(e.Tags, " #"))
	}
	row := wire.Component{Type: wire.ComponentRow, Key: "entity-" + e.ID, Title: truncate(firstNonEmpty(e.Title, e.ID), 200),
		Subtitle: truncate(strings.Join(parts, " · "), 300), Icon: icon,
		Action: ext.Open("open:"+e.ID, "", uiState{P: pageEntity, ID: e.ID, Space: sp.ID}, truncate(e.Title, 60))}
	out, in := sp.edges(e.ID)
	if len(out)+len(in) > 0 {
		row.Value = fmt.Sprintf("→%d ←%d", len(out), len(in))
	}
	if e.Status != "" {
		row.Badge = &wire.Badge{Text: statusLabel(e.Status), Tone: statusTone(e.Status)}
	} else if e.Archived {
		row.Badge = &wire.Badge{Text: "已归档"}
	}
	return row
}

func statusTone(s string) string {
	switch s {
	case GoalAchieved, IdeaIncubated, "已完成", "已下单", "可用", "运行中", "using", "常用", "在库":
		return wire.TonePositive
	case GoalActive, IdeaExploring, "进行中", "快用完", "关注中":
		return wire.ToneCaution
	case "已失效", "已过期", "已用完", "拉黑", "已释放":
		return wire.ToneDanger
	}
	return wire.ToneNeutral
}

func libraryTab(sp *Space, r *ext.UIRequest, s uiState) []wire.Section {
	typ := canonicalType(s.Type)
	if typ == TypeFeed {
		typ = ""
	}
	t, ok := sp.objectType(typ)
	if !ok {
		typ, t = "", nil
	}
	// The schema browser: a row per object type.
	typeRows := []wire.Component{{Type: wire.ComponentRow, Key: "type-all", Title: "全部", Icon: wire.NavIconSearch,
		Value: strconv.Itoa(len(sp.Entities) - sp.count(TypeFeed, false)), Badge: selected(typ == ""),
		Action: &wire.Action{ID: "type:all"}}}
	for i, ot := range sp.Types {
		if ot.Key == TypeFeed || i > 30 {
			continue
		}
		sub := fmt.Sprintf("%s · %d 个字段", ot.Key, len(ot.Fields))
		if ot.Builtin {
			sub += " · 内置"
		}
		typeRows = append(typeRows, wire.Component{Type: wire.ComponentRow, Key: "type-" + ot.Key, Title: ot.Label, Subtitle: sub,
			Icon: ot.Icon, Value: strconv.Itoa(sp.count(ot.Key, true)), Badge: selected(typ == ot.Key),
			Action: &wire.Action{ID: "type:" + ot.Key}})
	}
	typeRows = append(typeRows, wire.Component{Type: wire.ComponentButton, Title: "注册新对象类型",
		Action: ext.Open("new-type", "", uiState{P: pageType, Space: sp.ID}, "对象类型")})
	if t != nil && !t.Builtin {
		typeRows = append(typeRows, wire.Component{Type: wire.ComponentButton, Title: "修改「" + t.Label + "」的字段",
			Action: ext.Open("edit-type", "", uiState{P: pageType, Space: sp.ID, Type: t.Key}, t.Label)})
	}
	q := Query{Type: typ, Keyword: s.Q}
	if typ == "" {
		q.Type = ""
	}
	list, total := sp.query(q)
	// The numbers.
	stats := []wire.Stat{{Label: "实体总数", Value: strconv.Itoa(total)}}
	if t != nil {
		for _, st := range t.Statuses {
			if len(stats) >= 4 || st == StatusArchived {
				continue
			}
			n := 0
			for _, e := range list {
				if e.Status == st {
					n++
				}
			}
			stats = append(stats, wire.Stat{Label: statusLabel(st), Value: strconv.Itoa(n), Tone: statusTone(st)})
		}
	} else {
		stats = append(stats, wire.Stat{Label: "对象类型", Value: strconv.Itoa(len(sp.Types))})
	}
	edges := 0
	for _, rel := range sp.Relations {
		if typ == "" {
			edges++
			continue
		}
		a, _ := sp.entity(rel.FromID)
		b, _ := sp.entity(rel.ToID)
		if (a != nil && a.Type == typ) || (b != nil && b.Type == typ) {
			edges++
		}
	}
	fields := 0
	if t != nil {
		fields = len(t.Fields)
	} else {
		for _, ot := range sp.Types {
			fields += len(ot.Fields)
		}
	}
	stats = append(stats, wire.Stat{Label: "关系边", Value: strconv.Itoa(edges)}, wire.Stat{Label: "字段数", Value: strconv.Itoa(fields)})
	head := []wire.Component{{Type: wire.ComponentStatGrid, Key: "library-stats", Stats: stats}}
	if t != nil && t.Description != "" {
		head = append(head, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: truncate(t.Description, 2000)})
	}
	out := []wire.Section{{Title: "对象类型", Items: typeRows}, {Title: libraryTitle(t, s.Q), Items: head}}
	if total == 0 {
		out[1].Items = append(out[1].Items, wire.Component{Type: wire.ComponentEmpty, Title: "没有符合的实体", Icon: wire.NavIconSearch})
	} else {
		out[1].Items = append(out[1].Items, entityTable(sp, t, list))
		n := min(shown(s), len(list))
		visible := list[:n]
		if t != nil && len(t.Statuses) > 0 {
			seen := map[string]bool{}
			for _, st := range t.Statuses {
				if st == StatusArchived {
					continue
				}
				var group []wire.Component
				for _, e := range visible {
					if e.Status == st {
						group = append(group, entityRow(sp, e))
						seen[e.ID] = true
					}
				}
				if len(group) > 0 {
					out = append(out, wire.Section{Title: fmt.Sprintf("%s · %d", statusLabel(st), len(group)), Items: group})
				}
			}
			var rest []wire.Component
			for _, e := range visible {
				if !seen[e.ID] {
					rest = append(rest, entityRow(sp, e))
				}
			}
			if len(rest) > 0 {
				out = append(out, wire.Section{Title: fmt.Sprintf("其他状态 · %d", len(rest)), Items: rest})
			}
			if n < len(list) && len(out) > 2 {
				out[len(out)-1].More = &wire.More{Action: wire.Action{ID: "more"}}
			}
		} else {
			cards := []wire.Component{}
			for _, e := range visible {
				cards = append(cards, entityRow(sp, e))
			}
			sec := wire.Section{Title: fmt.Sprintf("实体卡片 · %d", total), Items: cards}
			if n < len(list) {
				sec.More = &wire.More{Action: wire.Action{ID: "more"}}
			}
			out = append(out, sec)
		}
	}
	if t != nil && t.Key != TypeFeed {
		out = append(out, wire.Section{Title: "新增" + t.Label, Items: entityFields(sp, t, nil, "entity-add", "保存")})
	}
	return out
}

func selected(ok bool) *wire.Badge {
	if ok {
		return &wire.Badge{Text: "当前", Tone: wire.TonePositive}
	}
	return nil
}

func libraryTitle(t *ObjectType, q string) string {
	title := "全部实体"
	if t != nil {
		title = t.Label + "（" + t.Key + "）"
	}
	if q != "" {
		title += " · 搜索「" + truncate(q, 30) + "」"
	}
	return title
}

// entityTable is the structured view: the columns come from the type's fields.
func entityTable(sp *Space, t *ObjectType, list []*Entity) wire.Component {
	if len(list) > 50 {
		list = list[:50]
	}
	tbl := wire.Component{Type: wire.ComponentTable, Key: "library-table", Rows: [][]string{}}
	if t == nil {
		tbl.Columns = []wire.TableColumn{{Title: "标题"}, {Title: "类型"}, {Title: "状态"}, {Title: "关系", Align: wire.TableAlignEnd}}
		for _, e := range list {
			out, in := sp.edges(e.ID)
			tbl.Rows = append(tbl.Rows, []string{truncate(e.Title, 60), sp.typeLabel(e.Type), statusLabel(e.Status),
				strconv.Itoa(len(out) + len(in))})
		}
		tbl.Caption = fmt.Sprintf("%d 行", len(tbl.Rows))
		return tbl
	}
	tbl.Columns = []wire.TableColumn{{Title: "标题"}}
	if len(t.Statuses) > 0 {
		tbl.Columns = append(tbl.Columns, wire.TableColumn{Title: "状态"})
	}
	var cols []FieldDef
	for _, f := range t.Fields {
		if len(tbl.Columns) >= 8 {
			break
		}
		col := wire.TableColumn{Title: f.Label}
		if f.Kind == KindNumber {
			col.Align = wire.TableAlignEnd
		}
		tbl.Columns = append(tbl.Columns, col)
		cols = append(cols, f)
	}
	for _, e := range list {
		row := []string{truncate(e.Title, 60)}
		if len(t.Statuses) > 0 {
			row = append(row, statusLabel(e.Status))
		}
		for _, f := range cols {
			s := formatValue(f, e.Properties[f.Key])
			if f.Kind == KindRelation && s != "" {
				s = sp.title(s)
			}
			row = append(row, truncate(s, 80))
		}
		tbl.Rows = append(tbl.Rows, row)
	}
	tbl.Caption = fmt.Sprintf("%s · %d 行 · 列来自 %d 个字段", t.Label, len(tbl.Rows), len(t.Fields))
	return tbl
}

// entityFields is a form for an entity of t, filled from e (nil: a new one).
func entityFields(sp *Space, t *ObjectType, e *Entity, action, title string) []wire.Component {
	val := func(key string) string {
		if e == nil {
			return ""
		}
		f, _ := t.field(key)
		v, ok := e.Properties[key]
		if !ok {
			return ""
		}
		if f.Kind == KindNumber {
			if n, ok := v.(float64); ok {
				return fmtNum(n)
			}
		}
		if f.Kind == KindBoolean {
			if b, ok := v.(bool); ok {
				return strconv.FormatBool(b)
			}
		}
		return formatValue(FieldDef{}, v)
	}
	items := []wire.Component{{Type: wire.ComponentField, Name: "title", Label: "标题", Kind: wire.FieldKindText}}
	if e != nil {
		items[0].Value = e.Title
	}
	if len(t.Statuses) > 0 {
		choices := []wire.Choice{}
		for _, s := range t.Statuses {
			choices = append(choices, wire.Choice{Value: s, Title: statusLabel(s)})
		}
		value := t.Statuses[0]
		if e != nil && e.Status != "" {
			value = e.Status
		}
		items = append(items, wire.Component{Type: wire.ComponentField, Name: "status", Label: "状态", Kind: wire.FieldKindChoice,
			Value: value, Choices: choices})
	}
	tags := ""
	if e != nil {
		tags = strings.Join(e.Tags, ", ")
	}
	items = append(items, wire.Component{Type: wire.ComponentField, Name: "tags", Label: "标签", Kind: wire.FieldKindText, Value: tags,
		Placeholder: "用逗号分隔"})
	for _, f := range t.Fields {
		label := f.Label
		if f.Required {
			label += " *"
		}
		if f.Unit != "" {
			label += "（" + f.Unit + "）"
		}
		c := wire.Component{Type: wire.ComponentField, Name: "f_" + f.Key, Label: label, Kind: wire.FieldKindText, Value: val(f.Key)}
		switch f.Kind {
		case KindNumber:
			c.Kind = wire.FieldKindNumber
		case KindBoolean:
			c.Kind = wire.FieldKindToggle
			if c.Value == "" {
				c.Value = "false"
			}
		case KindDate:
			c.Placeholder = "YYYY-MM-DD"
		case KindURL:
			c.Placeholder = "https://"
		case KindEnum:
			c.Kind = wire.FieldKindChoice
			c.Choices = []wire.Choice{{Value: noneValue, Title: "未填"}}
			for _, o := range f.Options {
				c.Choices = append(c.Choices, wire.Choice{Value: o, Title: o})
			}
			if c.Value == "" {
				c.Value = noneValue
			}
		case KindRelation:
			c.Kind = wire.FieldKindChoice
			c.Choices = append([]wire.Choice{{Value: noneValue, Title: "未关联"}}, entityChoices(sp, f.TargetType, "")...)
			if c.Value == "" {
				c.Value = noneValue
			}
		}
		items = append(items, c)
	}
	return append(items, wire.Component{Type: wire.ComponentButton, Title: title, Action: &wire.Action{ID: action, Submit: true}})
}

// entityChoices are the entities a relation can point at (of typ when given), but not except.
func entityChoices(sp *Space, typ, except string) []wire.Choice {
	out := []wire.Choice{}
	for _, e := range sp.Entities {
		if e.Type == TypeFeed || e.ID == except || (typ != "" && e.Type != typ) || e.Archived {
			continue
		}
		out = append(out, wire.Choice{Value: e.ID, Title: truncate(sp.typeLabel(e.Type)+" · "+e.Title, 60)})
		if len(out) == 100 {
			break
		}
	}
	return out
}

// ---- An entity ----

func gone() *wire.View {
	return &wire.View{Title: "不见了", Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty,
		Title: "这一条不见了", Detail: "可能已经被删除", Icon: wire.NavIconWarning}}}}}
}

func entityView(st *State, r *ext.UIRequest, s uiState) *wire.View {
	sp, e, ok := st.findEntity(s.ID)
	if !ok {
		return gone()
	}
	t, _ := sp.objectType(e.Type)
	if t == nil {
		t = &ObjectType{Key: e.Type, Label: e.Type, Icon: wire.NavIconList}
	}
	loc := location(r)
	md := fmt.Sprintf("**%s** · %s（%s）", e.Title, t.Label, t.Key)
	if e.Status != "" {
		md += " · " + statusLabel(e.Status)
	}
	if len(e.Tags) > 0 {
		md += "\n\n#" + strings.Join(e.Tags, " #")
	}
	info := []wire.Component{{Type: wire.ComponentMarkdown, Key: "head", Text: truncate(md, 2000)}}
	if e.Type == TypeGoal {
		info = append(info, goalProgress(e))
	}
	for _, f := range t.Fields {
		v, has := e.Properties[f.Key]
		if !has {
			continue
		}
		row := wire.Component{Type: wire.ComponentRow, Key: "prop-" + f.Key, Title: f.Label, Value: truncate(formatValue(f, v), 300),
			Subtitle: f.Key + " · " + f.Kind}
		if f.Kind == KindRelation {
			id := formatValue(f, v)
			row.Value = truncate(sp.title(id), 300)
			row.Icon = wire.NavIconLink
			row.Action = ext.Open("open:"+id, "", uiState{P: pageEntity, ID: id, Space: sp.ID}, sp.title(id))
		}
		if f.Kind == KindText && len([]rune(row.Value)) > 60 {
			row.Value = ""
			row.Subtitle = truncate(formatValue(f, v), 2000)
		}
		info = append(info, row)
	}
	if e.Type == TypeFeed {
		if target, ok := e.Properties["target_id"].(string); ok {
			if _, ok := sp.entity(target); ok {
				info = append(info, wire.Component{Type: wire.ComponentButton, Title: "打开「" + truncate(sp.title(target), 30) + "」",
					Action: ext.Open("open:"+target, "", uiState{P: pageEntity, ID: target, Space: sp.ID}, sp.title(target))})
			}
		}
	}
	info = append(info, wire.Component{Type: wire.ComponentRow, Key: "times", Title: "时间",
		Value: "创建 " + e.CreatedAt.In(loc).Format("2006-01-02 15:04") + " · 更新 " + e.UpdatedAt.In(loc).Format("01-02 15:04")})
	v := &wire.View{Title: truncate(e.Title, 60), Sections: []wire.Section{{Items: info}}}

	out, in := sp.edges(e.ID)
	rels := []wire.Component{}
	for _, rel := range out {
		other, _ := sp.entity(rel.ToID)
		rels = append(rels, relationRow(sp, rel, other, "→ "+relationLabel(rel.RelationType)+" → "))
	}
	for _, rel := range in {
		other, _ := sp.entity(rel.FromID)
		rels = append(rels, relationRow(sp, rel, other, "← "+relationLabel(rel.RelationType)+" ← "))
	}
	for _, rel := range append(slices.Clone(out), in...) {
		if !rel.ByField && len(rels) < 60 {
			rels = append(rels, wire.Component{Type: wire.ComponentButton, Title: "解除：" + truncate(relationLabel(rel.RelationType)+" "+sp.title(otherEnd(rel, e.ID)), 30),
				Action: &wire.Action{ID: "unrelate:" + rel.ID}})
		}
	}
	if len(rels) == 0 {
		rels = append(rels, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: "还没有关系"})
	}
	v.Sections = append(v.Sections, wire.Section{Title: fmt.Sprintf("关系 · 出 %d · 入 %d", len(out), len(in)), Items: rels})

	if e.Type != TypeFeed {
		acts := []wire.Component{}
		switch e.Type {
		case TypeIdea:
			if e.Status == IdeaInbox {
				acts = append(acts, button("开始探索", "idea-explore:"+e.ID))
			}
			if e.Status == IdeaInbox || e.Status == IdeaExploring {
				acts = append(acts, button("孵化为目标", "idea-goal:"+e.ID), button("转化为资源", "idea-resource:"+e.ID))
			}
		case TypeGoal:
			if e.Status != GoalAchieved {
				acts = append(acts, button("推进进度 +10%", "goal-step:"+e.ID), button("标记达成", "goal-done:"+e.ID))
			}
		}
		for _, status := range t.Statuses {
			if status != e.Status && status != StatusArchived && len(acts) < 10 && !(e.Type == TypeIdea && status == IdeaIncubated) {
				acts = append(acts, button("设为「"+statusLabel(status)+"」", "status:"+e.ID+":"+status))
			}
		}
		acts = append(acts, wire.Component{Type: wire.ComponentButton, Title: "编辑",
			Action: ext.Open("edit:"+e.ID, "", uiState{P: pageEdit, ID: e.ID, Space: sp.ID}, "编辑 "+truncate(e.Title, 30))})
		if e.Archived {
			acts = append(acts, button("取消归档", "unarchive:"+e.ID))
		} else {
			acts = append(acts, button("归档", "archive:"+e.ID))
		}
		acts = append(acts, wire.Component{Type: wire.ComponentButton, Title: "删除", Role: "destructive",
			Action: &wire.Action{ID: "delete:" + e.ID, Confirm: "删除「" + truncate(e.Title, 30) + "」和它的关系？"}})
		v.Sections = append(v.Sections, wire.Section{Title: "操作", Items: acts})

		relChoices := []wire.Choice{}
		for _, rt := range sp.relationTypeList() {
			relChoices = append(relChoices, wire.Choice{Value: rt, Title: relationLabel(rt) + "（" + rt + "）"})
		}
		targets := entityChoices(sp, "", e.ID)
		if len(targets) > 0 {
			v.Sections = append(v.Sections, wire.Section{Title: "添加关系", Items: []wire.Component{
				{Type: wire.ComponentField, Name: "rel_type", Label: "关系", Kind: wire.FieldKindChoice, Value: "relates_to", Choices: relChoices},
				{Type: wire.ComponentField, Name: "rel_target", Label: "指向", Kind: wire.FieldKindChoice, Value: targets[0].Value, Choices: targets},
				{Type: wire.ComponentButton, Title: "添加关系", Action: &wire.Action{ID: "relate", Submit: true}},
			}})
		}
	}
	return v
}

func otherEnd(rel Relation, id string) string {
	if rel.FromID == id {
		return rel.ToID
	}
	return rel.FromID
}

func relationRow(sp *Space, rel Relation, other *Entity, prefix string) wire.Component {
	if other == nil {
		return wire.Component{Type: wire.ComponentRow, Key: "rel-" + rel.ID, Title: prefix + "（已删除）"}
	}
	return wire.Component{Type: wire.ComponentRow, Key: "rel-" + rel.ID, Title: truncate(other.Title, 100),
		Subtitle: prefix + sp.typeLabel(other.Type) + " · " + rel.RelationType, Icon: wire.NavIconLink,
		Action: ext.Open("open:"+other.ID, "", uiState{P: pageEntity, ID: other.ID, Space: sp.ID}, truncate(other.Title, 60))}
}

func editView(st *State, r *ext.UIRequest, s uiState) *wire.View {
	sp, e, ok := st.findEntity(s.ID)
	if !ok {
		return gone()
	}
	t, ok := sp.objectType(e.Type)
	if !ok {
		return gone()
	}
	return &wire.View{Title: "编辑 " + truncate(e.Title, 40), Sections: []wire.Section{
		{Title: t.Label + " · 字段来自对象类型", Items: entityFields(sp, t, e, "entity-save", "保存")},
	}}
}

// ---- Spaces and types ----

func spacesView(st *State, r *ext.UIRequest, s uiState) *wire.View {
	rows := []wire.Component{}
	active := st.activeSpace()
	for _, sp := range st.Spaces {
		row := wire.Component{Type: wire.ComponentRow, Key: "space-" + sp.ID, Title: sp.Name, Icon: sp.Icon,
			Subtitle: truncate(fmt.Sprintf("%s · %d 个类型 · %d 个实体", sp.ID, len(sp.Types), len(sp.Entities)), 200)}
		switch {
		case sp.Archived:
			row.Badge = &wire.Badge{Text: "已归档"}
		case active != nil && sp.ID == active.ID:
			row.Badge = &wire.Badge{Text: "当前", Tone: wire.TonePositive}
		}
		rows = append(rows, row)
		switch {
		case sp.Archived:
			rows = append(rows, button("恢复「"+truncate(sp.Name, 20)+"」", "space-restore:"+sp.ID))
		default:
			if active == nil || sp.ID != active.ID {
				rows = append(rows, button("切换到「"+truncate(sp.Name, 20)+"」", "space-switch:"+sp.ID))
			}
			rows = append(rows, wire.Component{Type: wire.ComponentButton, Title: "归档「" + truncate(sp.Name, 20) + "」",
				Action: &wire.Action{ID: "space-archive:" + sp.ID, Confirm: "归档空间「" + sp.Name + "」？之后可以恢复。"}})
		}
	}
	tpls := []wire.Choice{}
	for _, t := range templates() {
		tpls = append(tpls, wire.Choice{Value: t.Key, Title: t.Name})
	}
	icons := []wire.Choice{{Value: noneValue, Title: "按模板"}}
	for _, ic := range wire.NavIcons[:20] {
		icons = append(icons, wire.Choice{Value: ic, Title: ic})
	}
	return &wire.View{Title: "空间", Sections: []wire.Section{
		{Title: "创建空间", Items: []wire.Component{
			{Type: wire.ComponentField, Name: "template", Label: "模板", Kind: wire.FieldKindChoice, Value: "blank", Choices: tpls},
			{Type: wire.ComponentField, Name: "name", Label: "名字", Kind: wire.FieldKindText, Placeholder: "不填用模板的名字"},
			{Type: wire.ComponentField, Name: "icon", Label: "图标", Kind: wire.FieldKindChoice, Value: noneValue, Choices: icons},
			{Type: wire.ComponentField, Name: "description", Label: "说明", Kind: wire.FieldKindText},
			{Type: wire.ComponentButton, Title: "创建空间", Action: &wire.Action{ID: "space-create", Submit: true}},
		}},
		{Title: fmt.Sprintf("全部空间 · %d", len(st.Spaces)), Items: rows},
	}}
}

// fieldSpec writes fields as the type form takes them: key:label:kind[:extra], one per line or ; between.
func fieldSpec(fields []FieldDef) string {
	parts := []string{}
	for _, f := range fields {
		p := f.Key + ":" + f.Label + ":" + f.Kind
		switch f.Kind {
		case KindEnum:
			p += ":" + strings.Join(f.Options, "|")
		case KindNumber:
			if f.Unit != "" {
				p += ":" + f.Unit
			}
		case KindRelation:
			p += ":" + f.TargetType
			if f.RelationType != "" {
				p += ":" + f.RelationType
			}
		}
		if f.Required {
			p += "!"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

// parseFieldSpec reads fieldSpec's form back.
func parseFieldSpec(spec string) ([]FieldDef, error) {
	var out []FieldDef
	for _, part := range strings.FieldsFunc(spec, func(r rune) bool { return r == ';' || r == '\n' || r == '；' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		req := strings.HasSuffix(part, "!")
		part = strings.TrimSuffix(part, "!")
		bits := strings.Split(strings.ReplaceAll(part, "：", ":"), ":")
		f := FieldDef{Key: strings.TrimSpace(bits[0]), Required: req, Kind: KindText}
		if len(bits) > 1 {
			f.Label = strings.TrimSpace(bits[1])
		}
		if len(bits) > 2 {
			f.Kind = strings.TrimSpace(bits[2])
		}
		if len(bits) > 3 {
			extra := strings.TrimSpace(bits[3])
			switch f.Kind {
			case KindEnum:
				for _, o := range strings.Split(extra, "|") {
					if o = strings.TrimSpace(o); o != "" {
						f.Options = append(f.Options, o)
					}
				}
			case KindNumber:
				f.Unit = extra
			case KindRelation:
				f.TargetType = extra
				if len(bits) > 4 {
					f.RelationType = strings.TrimSpace(bits[4])
				}
			}
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, invalidf("at least one field: key:标签:kind")
	}
	return out, nil
}

func typeView(st *State, r *ext.UIRequest, s uiState) *wire.View {
	sp, err := st.space(s.Space)
	if err != nil {
		sp = st.activeSpace()
	}
	if sp == nil {
		return gone()
	}
	var t ObjectType
	if old, ok := sp.objectType(s.Type); ok && !old.Builtin {
		t = *old
	}
	icons := []wire.Choice{}
	for _, ic := range wire.NavIcons {
		icons = append(icons, wire.Choice{Value: ic, Title: ic})
	}
	icon := firstNonEmpty(t.Icon, wire.NavIconList)
	title := "注册对象类型"
	if t.Key != "" {
		title = "修改「" + t.Label + "」"
	}
	help := "字段一行一个或用分号隔开：`key:标签:kind[:补充]`，末尾加 `!` 表示必填。kind 是 text、number、boolean、date、enum、url、relation；" +
		"enum 的补充是用 | 分隔的选项，number 的补充是单位，relation 的补充是目标类型（再加一段是关系名）。\n\n" +
		"例：`brand:品牌:text!; price:价格:number:元; size:尺码:enum:S|M|L; shop:店铺:relation:merchant:belongs_to`"
	types := wire.Component{Type: wire.ComponentTable, Key: "types", Columns: []wire.TableColumn{{Title: "类型"}, {Title: "标签"},
		{Title: "字段", Align: wire.TableAlignEnd}, {Title: "状态"}}, Rows: [][]string{}}
	for _, ot := range sp.Types {
		types.Rows = append(types.Rows, []string{ot.Key, ot.Label, strconv.Itoa(len(ot.Fields)), truncate(strings.Join(ot.Statuses, " / "), 80)})
	}
	return &wire.View{Title: title, Sections: []wire.Section{
		{Title: sp.Name + " 的本体", Items: []wire.Component{types}},
		{Title: title, Items: []wire.Component{
			{Type: wire.ComponentMarkdown, Text: help},
			{Type: wire.ComponentField, Name: "key", Label: "类型 key", Kind: wire.FieldKindText, Value: t.Key, Placeholder: "如 book、pet、subscription"},
			{Type: wire.ComponentField, Name: "label", Label: "名称", Kind: wire.FieldKindText, Value: t.Label},
			{Type: wire.ComponentField, Name: "icon", Label: "图标", Kind: wire.FieldKindChoice, Value: icon, Choices: icons},
			{Type: wire.ComponentField, Name: "description", Label: "说明", Kind: wire.FieldKindText, Value: t.Description},
			{Type: wire.ComponentField, Name: "fields", Label: "字段", Kind: wire.FieldKindText, Value: truncate(fieldSpec(t.Fields), 2000)},
			{Type: wire.ComponentField, Name: "primary_field", Label: "主字段", Kind: wire.FieldKindText, Value: t.PrimaryField},
			{Type: wire.ComponentField, Name: "statuses", Label: "状态（逗号分隔）", Kind: wire.FieldKindText, Value: strings.Join(t.Statuses, ", ")},
			{Type: wire.ComponentButton, Title: "保存对象类型", Action: &wire.Action{ID: "type-save", Submit: true}},
		}},
	}}
}

// ---- Actions ----

func (a *app) action(_ context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	s := decode(r)
	kind, arg, _ := strings.Cut(r.Action, ":")
	value := func(name string) string {
		if arg != "" {
			return arg
		}
		return strings.TrimSpace(r.Values[name])
	}
	redraw := func(toast string) (*wire.ActionResult, error) {
		r.State = ext.EncodeState(s)
		return &wire.ActionResult{View: a.draw(r, s), Toast: toast}, nil
	}
	fail := func(err error) (*wire.ActionResult, error) { return redraw(userError(err)) }
	db := a.db()
	switch kind {
	case "back":
		s = uiState{Tab: s.Tab}
		return redraw("")
	case "tab":
		s.Tab, s.Shown, s.Q = value("tab"), 0, ""
		if tabTitles[s.Tab] == "" {
			s.Tab = tabActivity
		}
		return redraw("")
	case "search":
		s.Q, s.Shown = strings.TrimSpace(r.Values["q"]), 0
		return redraw("")
	case "more":
		s.Shown = shown(s) + pageSize
		return redraw("")
	case "type":
		s.Type, s.Shown = value("type"), 0
		if s.Type == "all" {
			s.Type = ""
		}
		return redraw("")
	case "space":
		switch v := value("space"); v {
		case newSpace, manage:
			return a.push(r, uiState{P: pageSpaces}, "空间")
		default:
			if err := db.update(func(st *State, now time.Time) error { _, err := st.switchSpace(v); return err }); err != nil {
				return fail(err)
			}
			s.Shown, s.Q, s.Type = 0, "", ""
			return redraw("")
		}
	case "space-switch", "space-archive", "space-restore":
		var name string
		err := db.update(func(st *State, now time.Time) error {
			var sp *Space
			var err error
			switch kind {
			case "space-switch":
				sp, err = st.switchSpace(arg)
			case "space-archive":
				sp, err = st.archiveSpace(arg, true, uiActor, now)
			default:
				sp, err = st.archiveSpace(arg, false, uiActor, now)
			}
			if sp != nil {
				name = sp.Name
			}
			return err
		})
		if err != nil {
			return fail(err)
		}
		return redraw(map[string]string{"space-switch": "切换到", "space-archive": "归档了", "space-restore": "恢复了"}[kind] + "「" + name + "」")
	case "space-create":
		var name string
		err := db.update(func(st *State, now time.Time) error {
			icon := r.Values["icon"]
			if icon == noneValue {
				icon = ""
			}
			sp, err := st.createSpace(SpaceInput{Template: r.Values["template"], Name: r.Values["name"], Icon: icon,
				Description: r.Values["description"]}, uiActor, now)
			if sp != nil {
				name = sp.Name
			}
			return err
		})
		if err != nil {
			return fail(err)
		}
		return a.back(r, "创建了空间「"+name+"」")
	case "type-save":
		fields, err := parseFieldSpec(r.Values["fields"])
		if err != nil {
			return fail(err)
		}
		var statuses []string
		for _, x := range strings.FieldsFunc(r.Values["statuses"], func(r rune) bool { return r == ',' || r == '，' || r == '/' }) {
			statuses = append(statuses, strings.TrimSpace(x))
		}
		t := ObjectType{Key: strings.TrimSpace(r.Values["key"]), Label: r.Values["label"], Icon: r.Values["icon"],
			Description: strings.TrimSpace(r.Values["description"]), PrimaryField: strings.TrimSpace(r.Values["primary_field"]),
			Statuses: statuses, Fields: fields}
		err = db.update(func(st *State, now time.Time) error {
			sp, err := st.space(s.Space)
			if err != nil {
				return err
			}
			_, _, err = st.upsertType(sp, t, uiActor, now)
			return err
		})
		if err != nil {
			return fail(err)
		}
		if r.UIVersion >= 3 || r.UIVersion == 0 {
			return &wire.ActionResult{Toast: "保存了对象类型「" + firstNonEmpty(t.Label, t.Key) + "」", Pop: true}, nil
		}
		s = uiState{Tab: tabLibrary, Type: t.Key}
		return redraw("保存了对象类型「" + firstNonEmpty(t.Label, t.Key) + "」")
	case "post":
		note := strings.TrimSpace(r.Values["note"])
		if note == "" {
			return redraw("先写点什么")
		}
		err := db.update(func(st *State, now time.Time) error {
			_, _, err := st.upsertEntity(EntityInput{Type: TypeFeed, Title: truncate(note, 80),
				Properties: map[string]any{"event_type": "note", "summary": note, "actor": uiActor}}, uiActor, now)
			return err
		})
		if err != nil {
			return fail(err)
		}
		return redraw("发布了")
	case "idea-add":
		title := strings.TrimSpace(r.Values["title"])
		if title == "" {
			return redraw("点子不能是空的")
		}
		props := map[string]any{"summary": r.Values["summary"]}
		if p := r.Values["priority"]; p != "" && p != noneValue {
			props["priority"] = p
		}
		err := db.update(func(st *State, now time.Time) error {
			_, _, err := st.upsertEntity(EntityInput{Type: TypeIdea, Title: title, Properties: props,
				Tags: []string{r.Values["tags"]}}, uiActor, now)
			return err
		})
		if err != nil {
			return fail(err)
		}
		return redraw("记下了「" + truncate(title, 20) + "」")
	case "goal-add":
		title := strings.TrimSpace(r.Values["title"])
		if title == "" {
			return redraw("目标不能是空的")
		}
		props := map[string]any{"target_value": r.Values["target_value"], "current_value": r.Values["current_value"],
			"unit": r.Values["unit"], "deadline": r.Values["deadline"]}
		if p := r.Values["priority"]; p != "" && p != noneValue {
			props["priority"] = p
		}
		err := db.update(func(st *State, now time.Time) error {
			_, _, err := st.upsertEntity(EntityInput{Type: TypeGoal, Title: title, Status: GoalActive, Properties: props}, uiActor, now)
			return err
		})
		if err != nil {
			return fail(err)
		}
		return redraw("创建了目标「" + truncate(title, 20) + "」")
	case "idea-explore", "idea-goal", "idea-resource", "goal-step", "goal-done", "archive", "unarchive", "status":
		id, status, _ := strings.Cut(arg, ":")
		var toast string
		err := db.update(func(st *State, now time.Time) error {
			var e *Entity
			var err error
			switch kind {
			case "idea-explore":
				e, err = st.setStatus(id, IdeaExploring, uiActor, now)
				toast = "开始探索"
			case "idea-goal":
				e, err = st.incubate(id, TypeGoal, uiActor, now)
				toast = "孵化为目标"
			case "idea-resource":
				e, err = st.incubate(id, TypeResource, uiActor, now)
				toast = "转化为资源"
			case "goal-step":
				e, err = st.advanceGoal(id, 0.1, nil, uiActor, now)
				if e != nil {
					toast = fmt.Sprintf("进度 %d%%", int(math.Round(goalRatio(e)*100)))
				}
			case "goal-done":
				e, err = st.setStatus(id, GoalAchieved, uiActor, now)
				if e != nil {
					if t, ok := number(e.Properties["target_value"]); ok {
						e.Properties["current_value"] = t
					}
				}
				toast = "标记达成"
			case "archive":
				e, err = st.deleteEntity(id, true, uiActor, now)
				toast = "归档了"
			case "unarchive":
				f, back := false, ""
				if sp, old, ok := st.findEntity(id); ok && old.Status == StatusArchived {
					if t, ok := sp.objectType(old.Type); ok && len(t.Statuses) > 0 {
						back = t.Statuses[0]
					}
				}
				e, _, err = st.upsertEntity(EntityInput{ID: id, Archived: &f, Status: back}, uiActor, now)
				toast = "取消归档"
			case "status":
				e, err = st.setStatus(id, status, uiActor, now)
				toast = "设为「" + statusLabel(status) + "」"
			}
			if e != nil {
				toast += "：「" + truncate(e.Title, 20) + "」"
			}
			return err
		})
		if err != nil {
			return fail(err)
		}
		return redraw(toast)
	case "delete":
		var title string
		err := db.update(func(st *State, now time.Time) error {
			e, err := st.deleteEntity(arg, false, uiActor, now)
			if e != nil {
				title = e.Title
			}
			return err
		})
		if err != nil {
			return fail(err)
		}
		if s.P == pageEntity && s.ID == arg {
			return a.back(r, "删除了「"+truncate(title, 20)+"」")
		}
		return redraw("删除了「" + truncate(title, 20) + "」")
	case "relate":
		err := db.update(func(st *State, now time.Time) error {
			_, err := st.addRelation("", s.ID, r.Values["rel_type"], r.Values["rel_target"], uiActor, now)
			return err
		})
		if err != nil {
			return fail(err)
		}
		return redraw("添加了关系")
	case "unrelate":
		err := db.update(func(st *State, now time.Time) error {
			for _, sp := range st.Spaces {
				if n := sp.dropRelations(func(rel Relation) bool { return rel.ID == arg }); n > 0 {
					st.log(sp, "related", "解除了一条关系", uiActor, s.ID, "", now)
					return nil
				}
			}
			return notFoundf("no relation %q", arg)
		})
		if err != nil {
			return fail(err)
		}
		return redraw("解除了关系")
	case "entity-add", "entity-save":
		var title string
		err := db.update(func(st *State, now time.Time) error {
			in := EntityInput{Title: r.Values["title"], Status: r.Values["status"], Tags: []string{r.Values["tags"]},
				Properties: map[string]any{}}
			var t *ObjectType
			if kind == "entity-add" {
				sp := st.activeSpace()
				if sp == nil {
					return notFoundf("no space")
				}
				ot, ok := sp.objectType(canonicalType(s.Type))
				if !ok {
					return invalidf("choose an object type first")
				}
				t, in.SpaceID, in.Type = ot, sp.ID, ot.Key
			} else {
				sp, e, ok := st.findEntity(s.ID)
				if !ok {
					return notFoundf("no entity %q", s.ID)
				}
				t, _ = sp.objectType(e.Type)
				in.ID = e.ID
				if t == nil {
					return notFoundf("no type %q", e.Type)
				}
			}
			for _, f := range t.Fields {
				v, ok := r.Values["f_"+f.Key]
				if !ok {
					continue
				}
				if v == noneValue {
					v = ""
				}
				if kind == "entity-add" && strings.TrimSpace(v) == "" {
					continue
				}
				in.Properties[f.Key] = v
			}
			e, _, err := st.upsertEntity(in, uiActor, now)
			if e != nil {
				title = e.Title
			}
			return err
		})
		if err != nil {
			return fail(err)
		}
		if kind == "entity-save" {
			if r.UIVersion >= 3 || r.UIVersion == 0 {
				return &wire.ActionResult{Toast: "保存了", Pop: true}, nil
			}
			s.P = pageEntity
			return redraw("保存了")
		}
		return redraw("新增了「" + truncate(title, 20) + "」")
	}
	return redraw("")
}

// push opens a page on top (an older app gets it in place, by the SDK).
func (a *app) push(r *ext.UIRequest, s uiState, title string) (*wire.ActionResult, error) {
	return &wire.ActionResult{Push: &wire.Push{State: ext.EncodeState(s), Title: title}}, nil
}

// back goes back to the page below: a pop, or on an older app the space's tabs drawn in place.
func (a *app) back(r *ext.UIRequest, toast string) (*wire.ActionResult, error) {
	if r.UIVersion >= 3 || r.UIVersion == 0 {
		return &wire.ActionResult{Toast: toast, Pop: true}, nil
	}
	s := uiState{Tab: tabActivity}
	r.State = ext.EncodeState(s)
	return &wire.ActionResult{View: a.draw(r, s), Toast: toast}, nil
}

// userError words an error for a toast.
func userError(err error) string {
	msg := err.Error()
	for _, prefix := range []string{errInvalid.Error() + ": ", errNotFound.Error() + ": "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	if errors.Is(err, errNotFound) {
		return "找不到：" + msg
	}
	return "没有保存：" + msg
}

// ---- The overview card, the widget and the shortcuts ----

// topGoals are the goals to show outside the page: active first, by progress.
func topGoals(sp *Space, n int) []*Entity {
	goals, _ := sp.query(Query{Type: TypeGoal})
	goals = slices.DeleteFunc(goals, func(e *Entity) bool { return e.Status != GoalActive && e.Status != GoalPlanned })
	sort.SliceStable(goals, func(i, j int) bool { return goalOrder(goals[i]) < goalOrder(goals[j]) })
	if len(goals) > n {
		goals = goals[:n]
	}
	return goals
}

func (a *app) card(_ context.Context, r *ext.UIRequest) (*wire.View, error) {
	var v *wire.View
	a.db().read(func(st *State) {
		sp := st.activeSpace()
		if sp == nil {
			v = &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentText, Text: "还没有空间"}}}}}
			return
		}
		c := []wire.Component{
			{Type: wire.ComponentText, Style: wire.TextStyleCaption, Text: sp.Name},
			{Type: wire.ComponentStatGrid, Stats: []wire.Stat{
				{Label: "待探索点子", Value: strconv.Itoa(sp.countStatus(TypeIdea, IdeaInbox))},
				{Label: "进行中目标", Value: strconv.Itoa(sp.countStatus(TypeGoal, GoalActive))},
				{Label: "空间", Value: strconv.Itoa(len(st.liveSpaces()))},
			}},
		}
		for _, g := range topGoals(sp, 3) {
			c = append(c, goalProgress(g))
		}
		v = &wire.View{Sections: []wire.Section{{Items: c}}}
	})
	return adapt(v, r.UIVersion), nil
}

func (a *app) widget(_ context.Context, r *ext.UIRequest) (*wire.View, error) {
	var v *wire.View
	a.db().read(func(st *State) {
		sp := st.activeSpace()
		if sp == nil {
			v = &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentText, Text: "还没有空间"}}}}}
			return
		}
		goals := topGoals(sp, 4)
		active := sp.countStatus(TypeGoal, GoalActive)
		var c []wire.Component
		switch r.Size {
		case wire.WidgetFamilyAccessoryInline:
			text := fmt.Sprintf("%s · %d 个目标进行中", sp.Name, active)
			if len(goals) > 0 {
				text = fmt.Sprintf("%s · %d%%", truncate(goals[0].Title, 16), int(math.Round(goalRatio(goals[0])*100)))
			}
			c = []wire.Component{{Type: wire.ComponentText, Text: text}}
		case wire.WidgetFamilyAccessoryRectangular:
			c = []wire.Component{{Type: wire.ComponentMetric, Label: truncate(sp.Name, 12), Value: fmt.Sprintf("%d 个目标", active)}}
			if len(goals) > 0 {
				c = append(c, goalProgress(goals[0]))
			}
		case wire.WidgetFamilySmall:
			c = []wire.Component{{Type: wire.ComponentMetric, Label: "进行中目标", Value: strconv.Itoa(active), Detail: truncate(sp.Name, 20)}}
			if len(goals) > 0 {
				c = append(c, goalProgress(goals[0]))
			}
		default:
			c = []wire.Component{{Type: wire.ComponentStatGrid, Stats: []wire.Stat{
				{Label: "进行中", Value: strconv.Itoa(active)},
				{Label: "已达成", Value: strconv.Itoa(sp.countStatus(TypeGoal, GoalAchieved)), Tone: wire.TonePositive},
				{Label: "点子", Value: strconv.Itoa(sp.count(TypeIdea, true))},
			}}}
			n := 1
			if r.Size == wire.WidgetFamilyLarge {
				n = 4
			}
			for i, g := range goals {
				if i < n {
					c = append(c, goalProgress(g))
				}
			}
			if r.Size == wire.WidgetFamilyLarge {
				now, loc := time.Now(), location(r)
				for i, e := range feedEntries(sp) {
					if i == 3 {
						break
					}
					c = append(c, feedRow(sp, e, now, loc))
				}
			}
		}
		if len(c) == 0 {
			c = []wire.Component{{Type: wire.ComponentText, Text: sp.Name}}
		}
		v = &wire.View{Sections: []wire.Section{{Items: c}}}
	})
	// A widget cannot be tapped into rows: no actions.
	for i := range v.Sections {
		for j := range v.Sections[i].Items {
			v.Sections[i].Items[j].Action = nil
		}
	}
	return adapt(v, r.UIVersion), nil
}

func (a *app) shortcut(_ context.Context, method string, raw json.RawMessage) (any, error) {
	if method != "run" {
		return nil, ext.UnknownMethod("shortcut." + method)
	}
	var p wire.ShortcutRunParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &ext.Error{Code: "invalid", Message: err.Error()}
	}
	db := a.db()
	switch p.Contribution {
	case captureShortcut:
		text := strings.TrimSpace(p.Values["text"])
		if text == "" {
			return nil, &ext.Error{Code: "invalid", Message: "点子内容不能是空的"}
		}
		var spName string
		var count int
		err := db.update(func(st *State, now time.Time) error {
			sp := st.activeSpace()
			if sp == nil {
				return notFoundf("no space")
			}
			_, _, err := st.upsertEntity(EntityInput{SpaceID: sp.ID, Type: TypeIdea, Title: truncate(text, 60),
				Properties: map[string]any{"summary": text}}, "快捷指令", now)
			spName, count = sp.Name, sp.countStatus(TypeIdea, IdeaInbox)
			return err
		})
		if err != nil {
			return nil, &ext.Error{Code: "invalid", Message: userError(err)}
		}
		return wire.ShortcutResult{Text: fmt.Sprintf("记到了「%s」的点子里", spName), View: &wire.View{Sections: []wire.Section{{Items: []wire.Component{
			{Type: wire.ComponentMetric, Label: "待探索点子", Value: strconv.Itoa(count), Detail: truncate(spName, 40), Tone: wire.TonePositive},
			{Type: wire.ComponentText, Text: truncate(text, 2000)},
		}}}}}, nil
	case progressShortcut:
		name := strings.TrimSpace(p.Values["goal"])
		val, err := strconv.ParseFloat(strings.TrimSpace(p.Values["value"]), 64)
		if name == "" || err != nil || math.IsNaN(val) || math.IsInf(val, 0) {
			return nil, &ext.Error{Code: "invalid", Message: "要目标名称和一个数字进度"}
		}
		var goal Entity
		err = db.update(func(st *State, now time.Time) error {
			sp := st.activeSpace()
			if sp == nil {
				return notFoundf("no space")
			}
			match := findGoal(sp, name)
			if match == nil {
				return notFoundf("当前空间「%s」没有叫「%s」的目标", sp.Name, name)
			}
			e, err := st.advanceGoal(match.ID, 0, &val, "快捷指令", now)
			if e != nil {
				goal = *e
			}
			return err
		})
		if err != nil {
			return nil, &ext.Error{Code: "invalid", Message: userError(err)}
		}
		text := fmt.Sprintf("「%s」进度更新为 %s", goal.Title, goalDetail(&goal))
		if goal.Status == GoalAchieved {
			text += "，已达成！"
		}
		return wire.ShortcutResult{Text: text, View: &wire.View{Sections: []wire.Section{{Items: []wire.Component{goalProgress(&goal)}}}}}, nil
	}
	return nil, ext.NoContribution(p.Contribution)
}

// findGoal matches a goal by its name: exactly, else the first containing it.
func findGoal(sp *Space, name string) *Entity {
	goals, _ := sp.query(Query{Type: TypeGoal})
	low := strings.ToLower(name)
	for _, g := range goals {
		if strings.EqualFold(g.Title, name) {
			return g
		}
	}
	for _, g := range goals {
		if strings.Contains(strings.ToLower(g.Title), low) {
			return g
		}
	}
	return nil
}
