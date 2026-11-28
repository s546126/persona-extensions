package main

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/nwverse/persona/sdk/go/wire"
)

// The pages of guided study: weak knowledge points, tutor practice, and adding cards from Logseq.

// statusFilter is the mastery filter: five choices, each title well under the 12 characters a segmented allows.
func statusFilter(value string) wire.Component {
	choices := []wire.Choice{{Value: "all", Title: "全部"}}
	for _, s := range []string{statusWeak, statusShaky, statusSolid, statusNew} {
		choices = append(choices, wire.Choice{Value: s, Title: statusLabels[s]})
	}
	return wire.Component{Type: wire.ComponentSegmented, Name: "status", Choices: choices, Value: cmp.Or(value, "all"),
		Action: &wire.Action{ID: "weak_filter"}}
}

// kindPicker chooses the exercise kind: four choices.
func kindPicker(value string) wire.Component {
	var choices []wire.Choice
	for _, k := range exerciseKinds {
		choices = append(choices, wire.Choice{Value: k, Title: kindLabels[k]})
	}
	return wire.Component{Type: wire.ComponentSegmented, Name: "kind", Choices: choices, Value: cmp.Or(value, kindConcept),
		Action: &wire.Action{ID: "tutor_kind"}}
}

func statusTone(s string) string {
	switch s {
	case statusWeak:
		return wire.ToneDanger
	case statusShaky:
		return wire.ToneCaution
	case statusSolid:
		return wire.TonePositive
	}
	return wire.ToneNeutral
}

func (a *app) weakSummary() string {
	a.mu.Lock()
	all := masteryByTopic(a.sched, a.st, a.now())
	a.mu.Unlock()
	weak, shaky := 0, 0
	for _, g := range all {
		switch g.Status {
		case statusWeak:
			weak++
		case statusShaky:
			shaky++
		}
	}
	if weak+shaky == 0 {
		return fmt.Sprintf("%d 个知识点，暂时没有薄弱的", len(all))
	}
	return fmt.Sprintf("%d 个薄弱、%d 个不稳，按掌握度排好了", weak, shaky)
}

func (a *app) cardMasteryLabel(c *Card) string {
	a.mu.Lock()
	cs := scoreCard(a.sched, c, recentTallies(a.st, a.now())[c.ID], a.now())
	a.mu.Unlock()
	if cs.Status == statusNew {
		return "未学"
	}
	return fmt.Sprintf("%d（%s）", cs.Score, statusLabels[cs.Status])
}

func topicLine(g topicScore) string {
	if g.Status == statusNew {
		return fmt.Sprintf("未学 · %d 张卡片", len(g.Cards))
	}
	s := fmt.Sprintf("掌握度 %d · %s · %d 张", g.Score, statusLabels[g.Status], len(g.Cards))
	if g.Weak > 0 {
		s += fmt.Sprintf("（%d 张薄弱）", g.Weak)
	}
	if g.RecentN > 0 {
		s += fmt.Sprintf(" · 最近正确率 %.0f%%", g.Recent*100)
	}
	return s
}

func causeSummary(st state, topic string) string {
	counts := map[string]int{}
	for _, at := range st.Attempts {
		if at.Topic == topic && at.Verdict != verdictCorrect && causeLabels[at.Cause] != "" {
			counts[at.Cause]++
		}
	}
	var parts []string
	for _, c := range []string{"concept", "recall", "misread", "reasoning", "transfer", "careless"} {
		if counts[c] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d 次", causeLabels[c], counts[c]))
		}
	}
	return strings.Join(parts, "、")
}

func (a *app) weakView(st pageState) *wire.View {
	a.mu.Lock()
	all := masteryByTopic(a.sched, a.st, a.now())
	causes := ""
	if st.Topic != "" {
		causes = causeSummary(a.st, st.Topic)
	}
	a.mu.Unlock()
	if len(all) == 0 {
		return &wire.View{Title: "薄弱知识点", Sections: []wire.Section{{Items: []wire.Component{
			empty("还没有卡片", "加一些卡片、复习或练习几次之后，这里会按知识点排出掌握度。")}}, backSection()}}
	}
	if st.Topic != "" {
		for _, g := range all {
			if g.Topic == st.Topic {
				return topicView(g, st, causes)
			}
		}
		st.Topic = ""
	}
	topics := filterTopics(all, st.Status, "")
	var rows []wire.Component
	for _, g := range topics[:min(len(topics), maxListRows)] {
		rows = append(rows, row(g.Topic, topicLine(g), "weak_topic", map[string]string{"topic": g.Topic}))
	}
	if len(topics) > maxListRows {
		rows = append(rows, text(fmt.Sprintf("只列最弱的 %d 个，共 %d 个", maxListRows, len(topics)), wire.TextStyleCaption))
	}
	if len(rows) == 0 {
		rows = []wire.Component{empty("没有这类知识点", "换一个筛选看看。")}
	}
	return &wire.View{Title: "薄弱知识点", Sections: []wire.Section{
		{Footer: "掌握度综合了 FSRS 的记忆稳定性、难度、遗忘次数和最近 30 天复习与练习的正确率。", Items: []wire.Component{statusFilter(st.Status)}},
		{Title: "知识点", Items: rows},
		backSection(),
	}}
}

func topicView(g topicScore, st pageState, causes string) *wire.View {
	score, recent := "—", "—"
	if g.Status != statusNew {
		score = fmt.Sprint(g.Score)
	}
	if g.RecentN > 0 {
		recent = fmt.Sprintf("%.0f%%", g.Recent*100)
	}
	head := []wire.Component{{Type: wire.ComponentStatGrid, Stats: []wire.Stat{
		{Label: "掌握度", Value: score, Detail: statusLabels[g.Status], Tone: statusTone(g.Status)},
		{Label: "卡片", Value: fmt.Sprint(len(g.Cards)), Detail: fmt.Sprintf("学过 %d 张", g.Seen)},
		{Label: "薄弱", Value: fmt.Sprint(g.Weak)},
		{Label: "最近正确率", Value: recent},
	}}}
	if causes != "" {
		head = append(head, text("最近的错因："+causes, wire.TextStyleSecondary))
	}
	head = append(head,
		button("练这个知识点", "go", map[string]string{"mode": modeTutor, "topic": g.Topic}),
		button("生成强化卡", "reinforce_topic", nil),
	)
	var rows []wire.Component
	n := 0
	for _, cs := range g.Cards {
		if st.Status != "" && cs.Status != st.Status {
			continue
		}
		if n++; n > maxListRows {
			rows = append(rows, text(fmt.Sprintf("只列最弱的 %d 张", maxListRows), wire.TextStyleCaption))
			break
		}
		sub := statusLabels[cs.Status]
		if cs.Status != statusNew {
			sub = fmt.Sprintf("掌握度 %d · %s", cs.Score, sub)
			if l := cs.Card.FSRS.Lapses; l > 0 {
				sub += fmt.Sprintf(" · 忘过 %d 次", l)
			}
			if cs.Mistakes > 0 {
				sub += fmt.Sprintf(" · 练习答错 %d 次", cs.Mistakes)
			}
		}
		rows = append(rows, row(oneLine(cs.Card.Front), sub, "open_card", map[string]string{"id": cs.Card.ID}))
	}
	if len(rows) == 0 {
		rows = []wire.Component{empty("没有这类卡片", "换一个筛选看看。")}
	}
	return &wire.View{Title: g.Topic, Sections: []wire.Section{
		{Items: head},
		{Title: "卡片（最弱的在前）", Items: append([]wire.Component{statusFilter(st.Status)}, rows...)},
		{Items: []wire.Component{button("全部知识点", "weak_topic", map[string]string{"topic": ""}), button("返回", "home", nil)}},
	}}
}

func (a *app) tutorView(st pageState) *wire.View {
	var e *Exercise
	if st.Exercise != "" {
		if x, ok := a.exercise(st.Exercise); ok {
			e = &x
		}
	}
	if e == nil {
		return a.tutorStart(st)
	}
	head := []wire.Component{
		text(fmt.Sprintf("%s · %s", kindLabels[e.Kind], e.Topic), wire.TextStyleCaption),
		text(e.Question, wire.TextStyleHeadline),
	}
	if e.Plain {
		head = append(head, text("模型暂时不可用，这是卡片原题。", wire.TextStyleCaption))
	}
	if e.Result != nil {
		return tutorResult(*e, head)
	}
	sections := []wire.Section{{Items: head}}
	var hints []wire.Component
	for i := 0; i < st.Hints && i < len(e.Hints); i++ {
		hints = append(hints, text(fmt.Sprintf("提示 %d：%s", i+1, e.Hints[i]), wire.TextStyleSecondary))
	}
	if len(hints) > 0 {
		sections = append(sections, wire.Section{Title: "提示", Items: hints})
	}
	var answer []wire.Component
	if e.Kind == kindChoice {
		for k, o := range e.Options {
			// The whole option, not a row's one-line title: a cut option could be a different answer.
			answer = append(answer, wire.Component{Type: wire.ComponentRow, Title: fit(optionLetter(k)+". "+o, optionLetter(k)),
				Action: &wire.Action{ID: "tutor_answer", Values: map[string]string{"answer": optionLetter(k)}}})
		}
	} else {
		answer = []wire.Component{
			{Type: wire.ComponentField, Name: "answer", Label: "你的答案", Kind: wire.FieldKindMultiline, Placeholder: "用自己的话写"},
			{Type: wire.ComponentButton, Title: "提交", Action: &wire.Action{ID: "tutor_answer", Submit: true}},
		}
	}
	sections = append(sections, wire.Section{Title: "作答", Items: answer})
	more := []wire.Component{}
	if left := len(e.Hints) - st.Hints; left > 0 {
		more = append(more, button(fmt.Sprintf("要个提示（还有 %d 个）", left), "tutor_hint", nil))
	}
	more = append(more,
		button("不会，看答案", "tutor_answer", map[string]string{"answer": "不知道"}),
		button("换一道", "tutor_start", nil),
	)
	sections = append(sections, wire.Section{Footer: "提示会一步步缩小范围，但不直接说答案；用了提示，这张卡记得会轻一点。", Items: more}, backSection())
	return &wire.View{Title: "Tutor 练习", Sections: sections}
}

func (a *app) tutorStart(st pageState) *wire.View {
	a.mu.Lock()
	total := len(a.st.Cards)
	var card *Card
	if st.Card != "" {
		if c := a.st.Cards[st.Card]; c != nil {
			cp := *c
			card = &cp
		}
	}
	a.mu.Unlock()
	if total == 0 {
		return &wire.View{Title: "Tutor 练习", Sections: []wire.Section{{Items: []wire.Component{
			empty("还没有卡片", "先从笔记或知识库做一些卡片，再来练习。")}}, backSection()}}
	}
	focus := "从你掌握得最弱的卡片出题。"
	switch {
	case card != nil:
		focus = "练这张卡：" + oneLine(card.Front)
	case st.Topic != "":
		focus = "练知识点「" + st.Topic + "」里最弱的卡片。"
	}
	return &wire.View{Title: "Tutor 练习", Sections: []wire.Section{
		{Title: "题型", Footer: "概念辨析考理解和区分，填空考关键词，选择考易混点，场景变式考迁移应用。", Items: []wire.Component{kindPicker(st.Kind)}},
		{Items: []wire.Component{text(focus, wire.TextStyleSecondary), button("出一道题", "tutor_start", nil)}},
		backSection(),
	}}
}

func tutorResult(e Exercise, head []wire.Component) *wire.View {
	j := e.Result
	verdict := fmt.Sprintf("%s · %d 分", verdictLabels[j.Verdict], j.Score)
	tone := map[string]string{verdictCorrect: wire.TonePositive, verdictPartial: wire.ToneCaution, verdictWrong: wire.ToneDanger}[j.Verdict]
	res := []wire.Component{{Type: wire.ComponentMetric, Label: "判分", Value: fit(verdict, "—"), Tone: tone}}
	if l := causeLabels[j.Cause]; l != "" && j.Verdict != verdictCorrect {
		line := "错因：" + l
		if j.Diagnosis != "" {
			line += "。" + j.Diagnosis
		}
		res = append(res, text(line, wire.TextStyleBody))
	} else if j.Diagnosis != "" {
		res = append(res, text(j.Diagnosis, wire.TextStyleBody))
	}
	if j.Feedback != "" {
		res = append(res, text(j.Feedback, wire.TextStyleSecondary))
	}
	if j.Answer != "" {
		res = append(res, text("你的答案："+j.Answer, wire.TextStyleCaption))
	}
	if j.Rough {
		res = append(res, text("模型暂时不可用，只按字面粗略判了一下，自己再对照看看。", wire.TextStyleCaption))
	}
	if j.Rated != "" {
		if r, err := parseRating(j.Rated); err == nil {
			res = append(res, text(fmt.Sprintf("这张卡正好到期，已经记作「%s」排下一次复习。", ratingLabel(r)), wire.TextStyleCaption))
		}
	}
	ref := []wire.Component{text(exerciseCard(e).Back, wire.TextStyleBody)}
	next := []wire.Component{button("下一题", "tutor_start", nil)}
	if j.Verdict != verdictCorrect {
		next = append(next, button("生成强化卡", "reinforce_exercise", nil))
	}
	next = append(next, button("看这个知识点", "go_topic", map[string]string{"topic": e.Topic}))
	return &wire.View{Title: "Tutor 练习", Sections: []wire.Section{
		{Items: head},
		{Title: "结果", Items: res},
		{Title: "参考答案", Items: ref},
		{Footer: "强化卡会针对这次的错因来出，先进待确认，留下的才排进复习。", Items: next},
		backSection(),
	}}
}

func (a *app) logseqView(ctx context.Context, st pageState) (*wire.View, error) {
	if _, err := a.machine(); err != nil {
		return &wire.View{Title: "Logseq", Sections: []wire.Section{{Items: []wire.Component{empty("还没选电脑", "在扩展设置里选放 Logseq 图谱的电脑，再填图谱的文件夹。")}}, backSection()}}, nil
	}
	top := []wire.Component{
		text("pages 和 journals 里标了 #card 的块会直接导入：块本身是问题，它下面缩进的子块是答案；{{cloze 词}} 做成填空卡。", wire.TextStyleSecondary),
		button("导入 #card 卡片", "import_logseq", nil),
	}
	search := []wire.Component{
		{Type: wire.ComponentField, Name: "q", Label: "找一个页面", Kind: wire.FieldKindText, Value: st.Query, Placeholder: "页面名"},
		{Type: wire.ComponentButton, Title: "搜索", Action: &wire.Action{ID: "logseq_search", Submit: true}},
	}
	var scan vaultScan
	if err := a.node(ctx, "logseq_pages", map[string]any{"query": st.Query}, &scan); err != nil {
		return nil, err
	}
	var rows []wire.Component
	for _, n := range scan.Notes[:min(len(scan.Notes), maxListRows)] {
		rows = append(rows, row(cmp.Or(n.Title, n.Path), n.Path, "make_logseq", map[string]string{"path": n.Path}))
	}
	if len(rows) == 0 {
		rows = []wire.Component{empty("没找到页面", "")}
	}
	return &wire.View{Title: "Logseq", Sections: []wire.Section{{Items: top}, {Items: search},
		{Footer: "点一个页面做卡片：有 #card 块就直接导入，没有就由模型来做，按页面名归到知识点。", Items: rows}, backSection()}}, nil
}
