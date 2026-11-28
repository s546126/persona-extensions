package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nwverse/persona/sdk/go/wire"
)

// The pages of 0.3.0: the guided lesson, walked a section at a time, and the knowledge map.

const (
	guideTopicRows  = 20 // knowledge points offered on the lesson's start page
	guideRecentRows = 8  // earlier lessons listed there
	mapCardRows     = 40 // cards listed under one knowledge point of the map
	maxGuideText    = maxMaterial
)

// badge marks a row with its mastery level, in the level's tone.
func badge(c wire.Component, status string) wire.Component {
	c.Badge = &wire.Badge{Text: statusLabels[status], Tone: statusTone(status)}
	return c
}

func (a *app) guideView(st pageState) *wire.View {
	if st.Guide == "" {
		return a.guideStart()
	}
	g, ok := a.guideByID(st.Guide)
	if !ok {
		return &wire.View{Title: "精读学习", Sections: []wire.Section{
			{Items: []wire.Component{empty("这份精读已经不在了", "精读只留最近的一些，重新生成一份就好。")}},
			{Items: []wire.Component{button("重新选", "go", map[string]string{"mode": modeGuide}), button("返回", "home", nil)}},
		}}
	}
	// The state is the client's: a section number past the end shows the last section instead of failing.
	n := len(g.Sections)
	i := min(max(st.Section, 0), n-1)
	sec := g.Sections[i]

	head := []wire.Component{text(fmt.Sprintf("第 %d / %d 节 · %s", i+1, n, sec.Title), wire.TextStyleHeadline)}
	if g.Plain {
		head = append(head, text("模型暂时不可用，这份精读直接用卡片做的：一张卡一节。", wire.TextStyleCaption))
	}
	sections := []wire.Section{{Items: head}}
	if sec.Intuition != "" {
		sections = append(sections, wire.Section{Title: "一句话直觉", Items: []wire.Component{text(sec.Intuition, wire.TextStyleBody)}})
	}
	if sec.Analogy != "" {
		sections = append(sections, wire.Section{Title: "打个比方", Items: []wire.Component{text(sec.Analogy, wire.TextStyleBody)}})
	}
	var points []wire.Component
	for _, p := range sec.KeyPoints[:min(len(sec.KeyPoints), maxKeyPoints)] {
		points = append(points, text("• "+p, wire.TextStyleBody))
	}
	sections = append(sections, wire.Section{Title: "要点", Items: points})

	if mc := sec.MicroCheck; mc.ok() {
		check := []wire.Component{text(mc.Question, wire.TextStyleBody)}
		if !st.GuideChecked {
			check = append(check,
				wire.Component{Type: wire.ComponentField, Name: "answer", Label: "你的回答", Kind: wire.FieldKindText, Placeholder: "一句话就行"},
				wire.Component{Type: wire.ComponentButton, Title: "提交", Action: &wire.Action{ID: "guide_check", Submit: true}},
				button("查看参考答案", "guide_reveal", nil),
			)
			sections = append(sections, wire.Section{Title: "随堂小测", Footer: "只是确认这一节看懂了，不记分，也不影响复习安排。", Items: check})
		} else {
			if st.GuideAns != "" {
				verdict, tone := "和参考答案不太一样，对照看看", wire.ToneCaution
				if checkMicro(mc, st.GuideAns) {
					verdict, tone = "答对了", wire.TonePositive
				}
				check = append(check,
					wire.Component{Type: wire.ComponentMetric, Label: "随堂小测", Value: verdict, Tone: tone},
					text("你的回答："+st.GuideAns, wire.TextStyleCaption),
				)
			}
			check = append(check, text("参考答案："+mc.Answer, wire.TextStyleBody))
			if len(mc.Accepted) > 0 {
				check = append(check, text("这样答也对："+strings.Join(mc.Accepted, "、"), wire.TextStyleCaption))
			}
			footer := "对照参考答案想一想，再进下一节。"
			if i == n-1 {
				footer = "对照参考答案想一想。"
			}
			if st.GuideAns != "" {
				footer = "按字面宽松比对，不分大小写和标点；意思对就算对，自己把握。"
			}
			sections = append(sections, wire.Section{Title: "随堂小测", Footer: footer, Items: check})
		}
	}

	var nav []wire.Component
	if i < n-1 {
		nav = append(nav, button("下一节", "guide_next", nil))
	}
	if i > 0 {
		nav = append(nav, button("上一节", "guide_prev", nil))
	}
	sections = append(sections, wire.Section{Items: nav})
	if i == n-1 {
		var end []wire.Component
		footer := "用练习确认真的会了。"
		if !g.Plain {
			end = append(end, button("做成待确认卡片", "guide_drafts", nil))
			footer = "卡片先进待确认，留下的才排进复习；已经有的不会重复加。"
		}
		if g.Topic != "" && a.hasTopic(g.Topic) {
			end = append(end, button("用深度辅导练这章", "go", map[string]string{"mode": modeTutor, "topic": g.Topic}))
		}
		if len(end) > 0 {
			sections = append(sections, wire.Section{Title: "学完了", Footer: footer, Items: end})
		}
	}
	// An empty section is not drawn; drop it so the page has none.
	out := sections[:0]
	for _, s := range sections {
		if len(s.Items) > 0 {
			out = append(out, s)
		}
	}
	out = append(out, wire.Section{Items: []wire.Component{button("换一份精读", "go", map[string]string{"mode": modeGuide}), button("返回", "home", nil)}})
	return &wire.View{Title: fit(g.Title, "精读学习"), Sections: out}
}

// hasTopic says whether any card that is not paused counts toward the knowledge point.
func (a *app) hasTopic(topic string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.st.Cards {
		if !c.Suspended && topicOf(c) == topic {
			return true
		}
	}
	return false
}

// guideStart offers what a lesson can be made from: a knowledge point, weakest first, or a text.
func (a *app) guideStart() *wire.View {
	a.mu.Lock()
	all := masteryByTopic(a.sched, a.st, a.now())
	recent := make([]Guide, 0, len(a.st.Guides))
	for _, g := range a.st.Guides {
		recent = append(recent, Guide{ID: g.ID, Title: g.Title, Created: g.Created, Plain: g.Plain, Sections: make([]GuideSection, len(g.Sections))})
	}
	a.mu.Unlock()
	sort.Slice(recent, func(i, j int) bool {
		if !recent[i].Created.Equal(recent[j].Created) {
			return recent[i].Created.After(recent[j].Created)
		}
		return recent[i].ID < recent[j].ID
	})

	sections := []wire.Section{{Items: []wire.Component{
		text("把一个知识点或一段材料拆成几节，一节一节讲：先说直觉，再打比方，列出要点，最后一道随堂小测。", wire.TextStyleSecondary)}}}
	if len(all) > 0 {
		var rows []wire.Component
		for _, g := range all[:min(len(all), guideTopicRows)] {
			rows = append(rows, badge(row(g.Topic, topicLine(g), "guide_open", map[string]string{"topic": g.Topic}), g.Status))
		}
		if len(all) > guideTopicRows {
			rows = append(rows, text(fmt.Sprintf("只列最弱的 %d 个，共 %d 个；其余的从知识地图里进。", guideTopicRows, len(all)), wire.TextStyleCaption))
		}
		sections = append(sections, wire.Section{Title: "精读一个知识点", Footer: "按这个知识点里的卡片和你最近答错的地方来讲，薄弱的先讲。", Items: rows})
	}
	sections = append(sections, wire.Section{Title: "精读一段材料", Footer: "材料只交给 Persona 的模型拆成几节，不发到别处。", Items: []wire.Component{
		{Type: wire.ComponentField, Name: "title", Label: "标题（可选）", Kind: wire.FieldKindText},
		{Type: wire.ComponentField, Name: "text", Label: "材料", Kind: wire.FieldKindMultiline, Placeholder: "贴一段笔记或文章"},
		{Type: wire.ComponentButton, Title: "开始精读", Action: &wire.Action{ID: "guide_make", Submit: true}},
	}})
	if len(recent) > 0 {
		var rows []wire.Component
		for _, g := range recent[:min(len(recent), guideRecentRows)] {
			sub := fmt.Sprintf("%d 节", len(g.Sections))
			if g.Plain {
				sub += " · 按卡片做的"
			}
			rows = append(rows, row(g.Title, sub, "guide_show", map[string]string{"id": g.ID}))
		}
		sections = append(sections, wire.Section{Title: "最近的精读", Items: rows})
	}
	sections = append(sections, backSection())
	return &wire.View{Title: "精读学习", Sections: sections}
}

func (a *app) mapView(st pageState) *wire.View {
	a.mu.Lock()
	all := masteryByTopic(a.sched, a.st, a.now())
	causes := topCauses(a.st)
	detail := ""
	if st.Topic != "" {
		detail = causeSummary(a.st, st.Topic)
	}
	a.mu.Unlock()
	if len(all) == 0 {
		return &wire.View{Title: "知识地图", Sections: []wire.Section{{Items: []wire.Component{
			empty("地图还是空的", "加一些卡片、复习或练习几次之后，这里会按掌握度画出你的知识点。")}}, backSection()}}
	}
	if st.Topic != "" {
		for _, g := range all {
			if g.Topic == st.Topic {
				return mapTopicView(g, st, detail)
			}
		}
	}
	root := summarizeMap(all)
	top := []wire.Component{
		{Type: wire.ComponentStatGrid, Stats: []wire.Stat{
			{Label: statusLabels[statusWeak], Value: fmt.Sprint(root.ByStatus[statusWeak]), Tone: statusTone(statusWeak)},
			{Label: statusLabels[statusShaky], Value: fmt.Sprint(root.ByStatus[statusShaky]), Tone: statusTone(statusShaky)},
			{Label: statusLabels[statusSolid], Value: fmt.Sprint(root.ByStatus[statusSolid]), Tone: statusTone(statusSolid)},
			{Label: statusLabels[statusNew], Value: fmt.Sprint(root.ByStatus[statusNew])},
		}},
		text(fmt.Sprintf("全部知识：%d 个知识点，%d 张卡片", root.Topics, root.Cards), wire.TextStyleSecondary),
		statusFilter(st.Status),
	}
	sections := []wire.Section{{Items: top}}
	// One budget for the whole page: the levels share maxListRows, the weakest level first.
	left, hidden := maxListRows, 0
	for _, level := range mapOrder {
		if st.Status != "" && st.Status != level {
			continue
		}
		group := filterTopics(all, level, "")
		if len(group) == 0 {
			continue
		}
		var rows []wire.Component
		for _, g := range group {
			if left == 0 {
				hidden++
				continue
			}
			left--
			sub := topicLine(g)
			if c := causeLabels[causes[g.Topic]]; c != "" {
				sub += " · 常错在" + c
			}
			rows = append(rows, badge(row(g.Topic, sub, "weak_topic", map[string]string{"topic": g.Topic}), g.Status))
		}
		if len(rows) > 0 {
			sections = append(sections, wire.Section{Title: fmt.Sprintf("%s（%d）", statusLabels[level], len(group)), Items: rows})
		}
	}
	if len(sections) == 1 {
		sections = append(sections, wire.Section{Items: []wire.Component{empty("没有这类知识点", "换一个筛选看看。")}})
	}
	if hidden > 0 {
		sections = append(sections, wire.Section{Items: []wire.Component{
			text(fmt.Sprintf("只列了最需要看的 %d 个知识点，还有 %d 个没列出；用上面的筛选缩小范围。", maxListRows, hidden), wire.TextStyleCaption)}})
	}
	sections = append(sections, wire.Section{Footer: "颜色和分组按掌握度来：综合了 FSRS 的记忆稳定性、难度、遗忘次数和最近 30 天复习与练习的正确率。",
		Items: []wire.Component{button("返回", "home", nil)}})
	return &wire.View{Title: "知识地图", Sections: sections}
}

// mapTopicView is one branch of the map: the knowledge point's numbers, where it came from, what goes wrong, and its
// cards, weakest first.
func mapTopicView(g topicScore, st pageState, causes string) *wire.View {
	score, recent := "—", "—"
	if g.Status != statusNew {
		score = fmt.Sprint(g.Score)
	}
	if g.RecentN > 0 {
		recent = fmt.Sprintf("%.0f%%", g.Recent*100)
	}
	head := []wire.Component{
		{Type: wire.ComponentStatGrid, Stats: []wire.Stat{
			{Label: "掌握度", Value: score, Detail: statusLabels[g.Status], Tone: statusTone(g.Status)},
			{Label: "卡片", Value: fmt.Sprint(len(g.Cards)), Detail: fmt.Sprintf("学过 %d 张", g.Seen)},
			{Label: "薄弱", Value: fmt.Sprint(g.Weak), Tone: map[bool]string{true: wire.ToneDanger, false: wire.ToneNeutral}[g.Weak > 0]},
			{Label: "最近正确率", Value: recent},
		}},
		text("卡片分布："+levelLine(cardLevels(g)), wire.TextStyleSecondary),
		text("来源："+sourceSummary(g), wire.TextStyleSecondary),
	}
	if causes != "" {
		head = append(head, text("错因统计："+causes, wire.TextStyleSecondary))
	}
	act := []wire.Component{
		button("先精读这个知识点", "guide_open", map[string]string{"topic": g.Topic}),
		button("针对弱点做 Tutor 练习", "go", map[string]string{"mode": modeTutor, "topic": g.Topic}),
	}
	var rows []wire.Component
	matched := 0
	for _, cs := range g.Cards {
		if st.Status != "" && cs.Status != st.Status {
			continue
		}
		if matched++; matched > mapCardRows {
			continue
		}
		sub := "还没学过"
		if cs.Status != statusNew {
			sub = fmt.Sprintf("掌握度 %d", cs.Score)
			if l := cs.Card.FSRS.Lapses; l > 0 {
				sub += fmt.Sprintf(" · 忘过 %d 次", l)
			}
			if cs.Mistakes > 0 {
				sub += fmt.Sprintf(" · 练习答错 %d 次", cs.Mistakes)
			}
		}
		rows = append(rows, badge(row(cs.Card.Front, sub, "open_card", map[string]string{"id": cs.Card.ID}), cs.Status))
	}
	if matched > mapCardRows {
		rows = append(rows, text(fmt.Sprintf("只列最弱的 %d 张，还有 %d 张没列出。", mapCardRows, matched-mapCardRows), wire.TextStyleCaption))
	}
	if len(rows) == 0 {
		rows = []wire.Component{empty("没有这类卡片", "换一个筛选看看。")}
	}
	return &wire.View{Title: g.Topic, Sections: []wire.Section{
		{Items: head},
		{Title: "下一步", Footer: "先精读把概念理顺，再用练习确认真的会了。", Items: act},
		{Title: "卡片（最弱的在前）", Items: append([]wire.Component{statusFilter(st.Status)}, rows...)},
		{Items: []wire.Component{button("整张地图", "weak_topic", map[string]string{"topic": ""}), button("返回", "home", nil)}},
	}}
}
