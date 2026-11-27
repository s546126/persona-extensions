package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The nav page 健身 and the overview card. The page has two tabs, 总览 (this week, the next session, the weekly
// volume, the recent workouts and body weight) and 计划 (the next days, the coach week and the routines), and a
// workout's sets when one is opened. Everything comes from the cached document, read again when it is over a
// minute old.

const (
	tabOverview       = "overview"
	tabPlan           = "plan"
	recentShown       = 10
	weeksShown        = 12
	weightDays        = 90
	maxWorkoutEntries = 200
	maxCoachItems     = 50
	maxPlanRoutines   = 150
	maxActionIDBytes  = 128
	renderBudget      = 8 * time.Second
	// cardTitle is the card's header on the overview and its name among the contribution switches; it differs from
	// the page's 健身 so the two do not read as one listed twice (TestManifestMatchesTheCode).
	cardTitle = "健身 · 概况"
)

type pageState struct {
	Tab     string `json:"t,omitempty"`
	Workout string `json:"w,omitempty"`
}

func readState(r *ext.UIRequest) pageState {
	var st pageState
	_ = r.DecodeState(&st)
	return st
}

func encodeBoundedState(st pageState) string {
	raw := ext.EncodeState(st)
	if len(raw) <= wire.StateMost {
		return raw
	}
	return ext.EncodeState(pageState{Tab: st.Tab})
}

// pageZone is the zone to draw in: the app's when it says one, else the hub's.
func (a *app) pageZone(r *ext.UIRequest) *time.Location {
	if r != nil && r.TZ != "" {
		if l, err := time.LoadLocation(r.TZ); err == nil {
			return l
		}
	}
	return a.currentZone()
}

func (a *app) renderPage(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	ctx, cancel := context.WithTimeout(ctx, renderBudget)
	defer cancel()
	err := a.sync(ctx, false)
	return a.page(r, readState(r), err), nil
}

func (a *app) pageAction(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	st := readState(r)
	kind, arg, _ := strings.Cut(r.Action, ":")
	toast := ""
	var err error
	switch kind {
	case "tab":
		st.Tab, st.Workout = r.Values["tab"], ""
	case "workout":
		if arg == "" {
			arg = r.Values["id"]
		}
		st.Workout = arg
	case "back":
		st.Workout = ""
	case "refresh":
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err = a.sync(ctx, true); err != nil {
			toast = "没连上：" + explain(err).Title
		} else {
			toast = "已同步到最新"
		}
	}
	if kind != "refresh" {
		ctx, cancel := context.WithTimeout(ctx, renderBudget)
		defer cancel()
		err = a.sync(ctx, false)
	}
	return &wire.ActionResult{View: a.page(r, st, err), Toast: toast}, nil
}

func text(s string) wire.Component { return wire.Component{Type: wire.ComponentText, Text: s} }

func caution(s string) wire.Component {
	return wire.Component{Type: wire.ComponentText, Text: s, Tone: wire.ToneCaution}
}

// empty is an empty state (UI version 3) or, for an older app, a text.
func empty(version int, title, detail string) wire.Component {
	title = clip(title, 1800)
	detail = clip(detail, 1800)
	if version >= 3 {
		return wire.Component{Type: wire.ComponentEmpty, Title: title, Detail: detail, Icon: wire.NavIconHeart}
	}
	return text(clip(strings.TrimSpace(title+"\n"+detail), 1800))
}

func (a *app) page(r *ext.UIRequest, st pageState, syncErr error) *wire.View {
	v := &wire.View{Title: "健身", RefreshAfter: 300}
	version := r.UIVersion
	h := a.health()
	if h.Config != nil {
		v.Sections = setupSections(version, h)
		v.State = ext.EncodeState(pageState{})
		return v
	}
	s := a.snapshot()
	if s == nil {
		p, footer := problem{"还没有读到数据", "稍等一会儿再点「重试」。"}, ""
		if syncErr != nil {
			p, footer = explain(syncErr), clip(detail(syncErr), 1800)
		}
		v.Sections = []wire.Section{{Footer: footer, Items: []wire.Component{empty(version, p.Title, p.Do),
			{Type: wire.ComponentButton, Title: "重试", Action: &wire.Action{ID: "refresh"}}}}}
		v.State = ext.EncodeState(pageState{})
		return v
	}
	if s.Empty {
		v.Sections = []wire.Section{{Items: []wire.Component{empty(version, "还没有数据",
			"这个 openGym 账号还没有从手机同步过。在 App 里同步一次，这里就有了。")}}}
		v.State = ext.EncodeState(pageState{})
		return v
	}
	loc := a.pageZone(r)
	if st.Workout != "" && s.Me.can("workouts", "view") {
		if w := s.State.workout(st.Workout); w != nil {
			v.Title = clip(w.title(s.State), 1800)
			v.Sections = a.workoutSections(s.State, *w, loc)
			v.State = encodeBoundedState(st)
			return v
		}
		st.Workout = ""
	}
	if st.Tab != tabPlan {
		st.Tab = tabOverview
	}
	v.Tabs = &wire.Tabs{Name: "tab", Value: st.Tab, Action: wire.Action{ID: "tab"},
		Items: []wire.Tab{{Value: tabOverview, Title: "总览"}, {Value: tabPlan, Title: "计划"}}}
	var top []wire.Component
	if syncErr != nil {
		p := explain(syncErr)
		top = append(top, caution(clip("这次没连上："+p.Title+"。下面是 "+s.At.In(loc).Format("01-02 15:04")+" 读到的。", 1800)),
			secondary(clip(p.Do, 1800)), caption(clip(detail(syncErr), 1800)))
	}
	if st.Tab == tabPlan {
		v.Sections = a.planSections(s, loc)
	} else {
		v.Sections = a.overviewSections(s, loc, version)
	}
	if len(top) > 0 {
		v.Sections = append([]wire.Section{{Items: top}}, v.Sections...)
	}
	v.Sections = append(v.Sections, wire.Section{Items: []wire.Component{
		{Type: wire.ComponentRow, Title: clip("openGym · "+firstNonEmpty(s.Me.User.Name, h.Server), 1800),
			Subtitle: clip(fmt.Sprintf("%s · %s 读取 · 单位 %s", h.Server, s.At.In(loc).Format("01-02 15:04"), s.State.Unit), 1800)},
		{Type: wire.ComponentButton, Title: "刷新", Action: &wire.Action{ID: "refresh"}},
	}})
	v.State = encodeBoundedState(st)
	return v
}

// volumeTone is positive when this week's volume reached last week's.
func volumeTone(now, before float64) string {
	if now > 0 && now >= before {
		return wire.TonePositive
	}
	return wire.ToneNeutral
}

func (a *app) overviewSections(s *snapshot, loc *time.Location, version int) []wire.Section {
	st, now := s.State, a.now()
	var out []wire.Section
	if s.Me.can("workouts", "view") {
		weeks := weeklyTotals(st, now, loc, weeksShown)
		this, last := weeks[len(weeks)-1], weeks[len(weeks)-2]
		spark := make([]float64, len(weeks))
		for i, w := range weeks {
			spark[i] = w.Volume
		}
		tone := volumeTone(this.Volume, last.Volume)
		stats := []wire.Stat{
			{Label: "本周训练", Value: fmt.Sprintf("%d 次", this.Sessions), Detail: fmt.Sprintf("上周 %d 次", last.Sessions)},
			{Label: "本周训练量", Value: weight(this.Volume, st.Unit), Detail: "上周 " + weight(last.Volume, st.Unit), Tone: tone,
				Spark: &wire.Spark{Values: spark}},
			{Label: "本周时长", Value: fmt.Sprintf("%d 分钟", this.Minutes), Detail: fmt.Sprintf("上周 %d 分钟", last.Minutes)},
		}
		if bw, ok := st.latestWeight(); ok && s.Me.can("bodyweight", "view") {
			from := now.In(loc).AddDate(0, 0, -30).Format(time.DateOnly)
			detail := shortDate(bw.D)
			if old, ok := st.weightAround(from); ok && old.D != bw.D {
				detail = "30 天 " + signed(bw.W-old.W) + " " + st.Unit
			}
			stats = append(stats, wire.Stat{Label: "体重", Value: weight(bw.W, st.Unit), Detail: detail})
		}
		if version >= 2 {
			out = append(out, wire.Section{Items: []wire.Component{{Type: wire.ComponentStatGrid, Stats: stats}}})
		} else {
			var items []wire.Component
			for _, x := range stats {
				items = append(items, wire.Component{Type: wire.ComponentMetric, Label: x.Label, Value: x.Value, Detail: x.Detail})
			}
			out = append(out, wire.Section{Items: items})
		}
		out = append(out, a.nextSection(s, loc))
		if version >= 2 {
			points := make([]wire.ChartPoint, len(weeks))
			for i, w := range weeks {
				points[i] = wire.ChartPoint{Label: clip(shortDate(w.Start), 40), Value: w.Volume, Display: clip(fmt.Sprintf("%s · %d 次", weight(w.Volume, st.Unit), w.Sessions), 40)}
			}
			out = append(out, wire.Section{Title: "每周训练量", Footer: fmt.Sprintf("最近 %d 周，周一开始；训练量是热身以外每组的重量 × 次数（%s）", weeksShown, st.Unit),
				Items: []wire.Component{{Type: wire.ComponentChart, Kind: wire.ChartKindBar, Unit: st.Unit,
					Series: []wire.ChartSeries{{Name: "训练量", Points: points}}}}})
		}
		out = append(out, a.recentSection(st))
	} else {
		out = append(out, wire.Section{Items: []wire.Component{caution("访问密钥没有看训练记录的权限：" + tokenWhere + "给这个密钥的「训练」选「查看」。")}},
			a.nextSection(s, loc))
	}
	if version >= 2 && s.Me.can("bodyweight", "view") {
		from := now.In(loc).AddDate(0, 0, -weightDays).Format(time.DateOnly)
		var points []wire.ChartPoint
		for _, b := range st.Bodyweight {
			if b.D >= from {
				points = append(points, wire.ChartPoint{Label: clip(shortDate(b.D), 40), Value: b.W, Display: clip(weight(b.W, st.Unit), 40)})
			}
		}
		if len(points) > wire.PointsMost {
			points = points[len(points)-wire.PointsMost:]
		}
		if len(points) >= 2 {
			out = append(out, wire.Section{Title: "体重", Footer: fmt.Sprintf("最近 %d 天", weightDays), Items: []wire.Component{
				{Type: wire.ComponentChart, Kind: wire.ChartKindLine, Unit: st.Unit, Series: []wire.ChartSeries{{Name: "体重", Points: points}}}}})
		}
	}
	return out
}

func (a *app) nextSection(s *snapshot, loc *time.Location) wire.Section {
	sec := wire.Section{Title: "下一次训练"}
	if !s.Me.can("plans", "view") {
		sec.Items = []wire.Component{caution("访问密钥没有看计划的权限：" + tokenWhere + "给这个密钥的「计划」选「查看」。")}
		return sec
	}
	now := a.now()
	n := s.State.next(now, loc)
	if n == nil {
		sec.Items = []wire.Component{text("两周内没有安排训练。")}
		return sec
	}
	title := firstNonEmpty(n.Routine.Name, "训练")
	for _, id := range n.Also {
		title += " + " + s.State.routineName(id)
	}
	sub := dayLabel(n.Date, dateOf(now, loc))
	if t := byText(n.By); t != "" {
		sub += " · " + t
	}
	sec.Items = []wire.Component{{Type: wire.ComponentRow, Title: clip(title, 1800), Subtitle: clip(sub, 1800), Icon: wire.NavIconCalendar,
		Badge: &wire.Badge{Text: fmt.Sprintf("%d 个动作", len(n.Routine.Ex)), Tone: wire.ToneNeutral}}}
	for i, sl := range n.Routine.Ex {
		if i >= 12 {
			sec.Footer = fmt.Sprintf("还有 %d 个动作", len(n.Routine.Ex)-i)
			break
		}
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentRow, Title: clip(a.exName(sl.ID), 1800), Subtitle: clip(slotText(sl, s.State.Unit), 1800)})
	}
	return sec
}

func (a *app) recentSection(st gymState) wire.Section {
	sec := wire.Section{Title: "最近训练"}
	n := 0
	for i := len(st.Workouts) - 1; i >= 0 && n < recentShown; i-- {
		w := st.Workouts[i]
		n++
		parts := []string{shortDate(w.D) + " " + weekdayName(w.D)}
		if m := w.minutes(); m > 0 {
			parts = append(parts, fmt.Sprintf("%d 分钟", m))
		}
		parts = append(parts, fmt.Sprintf("%d 组", w.doneSets()), weight(w.volume(), st.Unit))
		row := wire.Component{Type: wire.ComponentRow, Title: clip(w.title(st), 120), Subtitle: clip(strings.Join(parts, " · "), 1800)}
		if len("workout:"+w.ID) <= maxActionIDBytes {
			row.Action = &wire.Action{ID: "workout:" + w.ID}
		} else if len([]rune(w.ID)) <= wire.StringMost && len(ext.EncodeState(pageState{Workout: w.ID})) <= wire.StateMost {
			row.Action = &wire.Action{ID: "workout", Values: map[string]string{"id": w.ID}}
		}
		if w.hasPR() {
			row.Badge = &wire.Badge{Text: "破纪录", Tone: wire.TonePositive}
		}
		sec.Items = append(sec.Items, row)
	}
	if n == 0 {
		sec.Items = []wire.Component{text("还没有完成的训练。")}
	} else if len(st.Workouts) > n {
		sec.Footer = fmt.Sprintf("共 %d 次训练，这里是最近 %d 次", len(st.Workouts), n)
	}
	return sec
}

func (a *app) workoutSections(st gymState, w workout, loc *time.Location) []wire.Section {
	facts := []string{w.D + " " + weekdayName(w.D)}
	if c := clockOf(w.Start, loc); c != "" {
		facts[0] += " " + c
	}
	if m := w.minutes(); m > 0 {
		facts = append(facts, fmt.Sprintf("%d 分钟", m))
	}
	facts = append(facts, fmt.Sprintf("%d 组", w.doneSets()), "训练量 "+weight(w.volume(), st.Unit))
	head := []wire.Component{text(clip(strings.Join(facts, " · "), 1800))}
	if w.hasPR() {
		var names []string
		for _, id := range w.PRs {
			names = append(names, a.exName(id))
		}
		head = append(head, wire.Component{Type: wire.ComponentText, Text: clip("破了个人纪录："+strings.Join(names, "、"), 1800), Tone: wire.TonePositive})
	}
	if w.Note != "" {
		head = append(head, text("备注："+clip(w.Note, 500)))
	}
	ex := wire.Section{Title: "动作"}
	for i, e := range w.Entries {
		if i >= maxWorkoutEntries {
			ex.Footer = fmt.Sprintf("共 %d 个动作，这里显示前 %d 个", len(w.Entries), maxWorkoutEntries)
			break
		}
		var sets []string
		for _, s := range e.Sets {
			sets = append(sets, setText(s, st.Unit))
		}
		row := wire.Component{Type: wire.ComponentRow, Title: clip(a.exName(e.ID), 1800), Subtitle: clip(strings.Join(sets, " · "), 300)}
		if containsID(w.PRs, e.ID) {
			row.Badge = &wire.Badge{Text: "破纪录", Tone: wire.TonePositive}
		}
		ex.Items = append(ex.Items, row)
	}
	if len(ex.Items) == 0 {
		ex.Items = []wire.Component{text("这次没有记动作。")}
	}
	return []wire.Section{{Items: head}, ex, {Items: []wire.Component{{Type: wire.ComponentButton, Title: "返回", Action: &wire.Action{ID: "back"}}}}}
}

func containsID(list []string, id string) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

func (a *app) planSections(s *snapshot, loc *time.Location) []wire.Section {
	if !s.Me.can("plans", "view") {
		return []wire.Section{{Items: []wire.Component{caution("访问密钥没有看计划的权限：" + tokenWhere + "给这个密钥的「计划」选「查看」。")}}}
	}
	st, now := s.State, a.now()
	today := dateOf(now, loc)
	out := []wire.Section{a.nextSection(s, loc)}
	days := wire.Section{Title: "未来 7 天"}
	for _, d := range st.planDays(now, loc, 7) {
		title := "休息"
		if len(d.IDs) > 0 {
			var names []string
			for _, id := range d.IDs {
				names = append(names, st.routineName(id))
			}
			title = strings.Join(names, " + ")
		}
		row := wire.Component{Type: wire.ComponentRow, Title: clip(title, 1800), Subtitle: dayLabel(d.Date, today)}
		if t := byText(d.By); t != "" && d.By != "weekday" {
			row.Badge = &wire.Badge{Text: t, Tone: wire.ToneNeutral}
		}
		days.Items = append(days.Items, row)
	}
	out = append(out, days)
	if cw := st.coachWeek(now, loc); cw != nil && len(cw.Items) > 0 {
		footer := "按顺序练，不固定哪天；" + cw.StartsOn + " 开始"
		if len(cw.Items) > maxCoachItems {
			footer = fmt.Sprintf("%s（共 %d 项，这里显示前 %d 项）", footer, len(cw.Items), maxCoachItems)
		}
		sec := wire.Section{Title: clip("教练周 · "+firstNonEmpty(cw.Label, "未命名"), 1800), Footer: clip(footer, 1800)}
		words := map[string]string{"done": "做完了", "next": "下一个", "pinned": "排在 ", "later": "之后"}
		tones := map[string]string{"done": wire.TonePositive, "next": wire.ToneCaution, "pinned": wire.ToneNeutral, "later": wire.ToneNeutral}
		for i, it := range cw.Items {
			if i >= maxCoachItems {
				break
			}
			row := wire.Component{Type: wire.ComponentRow, Title: clip(firstNonEmpty(it.Name, "未命名"), 1800)}
			w := words[it.State]
			if it.State == "pinned" {
				w += dayLabel(it.PinnedTo, today)
			}
			if w != "" {
				row.Badge = &wire.Badge{Text: clip(w, 100), Tone: tones[it.State]}
			}
			sec.Items = append(sec.Items, row)
		}
		out = append(out, sec)
	}
	routines := wire.Section{Title: "训练计划"}
	for i, r := range st.Routines {
		if i >= maxPlanRoutines {
			routines.Footer = fmt.Sprintf("共 %d 个计划，这里显示前 %d 个", len(st.Routines), maxPlanRoutines)
			break
		}
		routines.Items = append(routines.Items, wire.Component{Type: wire.ComponentRow, Title: clip(firstNonEmpty(r.Name, "未命名"), 120),
			Subtitle: clip(a.routineLine(r, st.Unit), 300)})
	}
	if len(routines.Items) == 0 {
		routines.Items = []wire.Component{text("还没有训练计划。")}
	}
	return append(out, routines)
}

// renderCard is the overview card: this week against last, and the next session.
func (a *app) renderCard(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	ctx, cancel := context.WithTimeout(ctx, renderBudget)
	defer cancel()
	err := a.sync(ctx, false)
	v := &wire.View{Title: cardTitle, State: ext.EncodeState(pageState{}), RefreshAfter: 600}
	h := a.health()
	s := a.snapshot()
	switch {
	case h.Config != nil:
		v.Sections = []wire.Section{{Items: []wire.Component{text(setupNeeded(h))}}}
		return v, nil
	case s == nil:
		msg := "还没有读到数据"
		if err != nil {
			msg = explain(err).Title + "，点开看怎么办"
		}
		v.Sections = []wire.Section{{Items: []wire.Component{caution(msg)}}}
		return v, nil
	case s.Empty:
		v.Sections = []wire.Section{{Items: []wire.Component{text("openGym 还没有从手机同步过数据")}}}
		return v, nil
	}
	loc, now := a.pageZone(r), a.now()
	var items []wire.Component
	if s.Me.can("workouts", "view") {
		weeks := weeklyTotals(s.State, now, loc, 2)
		this, last := weeks[1], weeks[0]
		tone := volumeTone(this.Volume, last.Volume)
		items = append(items, wire.Component{Type: wire.ComponentMetric, Label: "本周", Tone: tone,
			Value:  fmt.Sprintf("%d 次 · %s", this.Sessions, weight(this.Volume, s.State.Unit)),
			Detail: fmt.Sprintf("上周 %d 次 · %s", last.Sessions, weight(last.Volume, s.State.Unit))})
	}
	if s.Me.can("plans", "view") {
		if n := s.State.next(now, loc); n != nil {
			items = append(items, text(clip("下一次："+dayLabel(n.Date, dateOf(now, loc))+" "+firstNonEmpty(n.Routine.Name, "训练"), 1800)))
		} else {
			items = append(items, text("两周内没有安排训练"))
		}
	}
	if err != nil {
		items = append(items, caution("上次没连上 openGym"))
	}
	if len(items) == 0 {
		items = []wire.Component{text("访问密钥没有看训练和计划的权限")}
	}
	v.Sections = []wire.Section{{Items: items}}
	return v, nil
}
