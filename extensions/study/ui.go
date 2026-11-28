package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
	fsrs "github.com/open-spaced-repetition/go-fsrs/v4"
)

const (
	pageID   = "study"
	widgetID = "study-due"

	maxListRows = 100 // rows of one list on a page, well under wire.ItemsMost with the rest of the page
)

// Page modes.
const (
	modeHome     = ""
	modeReview   = "review"
	modeDrafts   = "drafts"
	modeTopics   = "topics"
	modeNotion   = "notion"
	modeObsidian = "obsidian"
	modeNLM      = "nlm"
	modeCards    = "cards"
	modeCard     = "card"
	modeWrite    = "write"
	modeLogseq   = "logseq"
	modeWeak     = "weak"
	modeTutor    = "tutor"
	modeGuide    = "guide"
	modeMap      = "map"
)

type pageState struct {
	Mode     string `json:"m,omitempty"`
	Card     string `json:"c,omitempty"` // the card on screen (review, card)
	Shown    bool   `json:"s,omitempty"` // the answer is showing
	Query    string `json:"q,omitempty"`
	Notebook string `json:"n,omitempty"`
	NBTitle  string `json:"t,omitempty"`
	Done     int    `json:"d,omitempty"` // cards reviewed in this sitting
	Topic    string `json:"p,omitempty"` // the knowledge point (weak points, tutor)
	Status   string `json:"f,omitempty"` // the mastery filter (weak points)
	Kind     string `json:"k,omitempty"` // the exercise kind (tutor)
	Exercise string `json:"x,omitempty"` // the exercise on screen (tutor)
	Hints    int    `json:"h,omitempty"` // hints shown for it
	// The guided lesson: which one, the section on screen, and its quick question once answered or revealed.
	Guide        string `json:"g,omitempty"`
	Section      int    `json:"sec,omitempty"`
	GuideChecked bool   `json:"gc,omitempty"`
	GuideAns     string `json:"ga,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	// Text the page clips by characters is checked by characters, so a long Chinese query never locks the page.
	if err := r.DecodeState(&st); err != nil || len(st.Card) > 64 || utf8.RuneCountInString(st.Query) > 200 || len(st.Notebook) > 128 ||
		utf8.RuneCountInString(st.NBTitle) > 300 ||
		utf8.RuneCountInString(st.Topic) > maxTopic || len(st.Exercise) > 64 || st.Hints < 0 || st.Hints > maxHints || st.Done < 0 ||
		(st.Status != "" && !validStatus(st.Status)) || (st.Kind != "" && !validKind(st.Kind)) ||
		len(st.Guide) > 64 || st.Section < 0 || st.Section >= maxGuideSection || utf8.RuneCountInString(st.GuideAns) > maxCheckText {
		return pageState{}, errors.New("invalid page state")
	}
	return st, nil
}

// The component helpers hold their strings to what a view may carry (wire.CheckView): required text is never
// empty and nothing is longer than wire.StringMost.
func fit(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return clip(s, wire.StringMost)
}

// text draws a paragraph. A headline or title longer than the apps draw one (wire.TitleMost) becomes body text, so a
// long question is shown whole rather than refused or cut.
func text(s, style string) wire.Component {
	s = fit(s, "—")
	if (style == wire.TextStyleHeadline || style == wire.TextStyleTitle) && utf8.RuneCountInString(s) > wire.TitleMost {
		style = wire.TextStyleBody
	}
	return wire.Component{Type: wire.ComponentText, Text: s, Style: style}
}
func button(title, id string, values map[string]string) wire.Component {
	return wire.Component{Type: wire.ComponentButton, Title: fit(title, "打开"), Action: &wire.Action{ID: id, Values: fitValues(values)}}
}
func row(title, subtitle, id string, values map[string]string) wire.Component {
	return wire.Component{Type: wire.ComponentRow, Title: fit(oneLine(title), "无标题"), Subtitle: clip(subtitle, wire.StringMost), Action: &wire.Action{ID: id, Values: fitValues(values)}}
}

// fitValues holds an action's values to the 2000 characters each may have.
func fitValues(values map[string]string) map[string]string {
	for k, v := range values {
		if utf8.RuneCountInString(v) > wire.StringMost {
			values[k] = clip(v, wire.StringMost)
		}
	}
	return values
}
func empty(title, detail string) wire.Component {
	return wire.Component{Type: wire.ComponentEmpty, Title: fit(title, "这里还没有内容"), Text: clip(detail, wire.StringMost)}
}

// answerBlock shows a card's answer as Markdown when it is in the subset the apps draw (https links, no images), and
// as plain text otherwise: an Obsidian ![[embed]] or an http link must not make the whole page refused.
func answerBlock(s string) wire.Component {
	if strings.TrimSpace(s) == "" {
		return text("", wire.TextStyleBody)
	}
	md := wire.Component{Type: wire.ComponentMarkdown, Text: clip(s, 20000)}
	if wire.CheckView(&wire.View{Sections: []wire.Section{{Items: []wire.Component{md}}}}) == nil {
		return md
	}
	return text(s, wire.TextStyleBody)
}

func (a *app) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	st, err := readState(r)
	if err != nil {
		return nil, err
	}
	return a.view(ctx, st, "")
}

func (a *app) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	st, err := readState(r)
	if err != nil {
		return nil, err
	}
	v := r.Values
	toast := ""
	switch r.Action {
	case "home":
		st = pageState{}
	case "go":
		switch m := v["mode"]; m {
		case modeReview, modeDrafts, modeTopics, modeNotion, modeObsidian, modeNLM, modeCards, modeWrite, modeLogseq, modeWeak, modeGuide:
			st = pageState{Mode: m}
		case modeMap:
			st = pageState{Mode: m, Topic: cleanTopic(v["topic"])}
		case modeTutor:
			if len(v["card"]) > 64 {
				return nil, errors.New("卡片 ID 不对")
			}
			st = pageState{Mode: m, Topic: cleanTopic(v["topic"]), Card: v["card"], Kind: st.Kind}
			if st.Kind == "" {
				st.Kind = kindConcept
			}
		default:
			return nil, errors.New("unknown page")
		}
	case "show":
		st.Shown = true
	case "grade":
		r, err := parseRating(v["r"])
		if err != nil {
			return nil, err
		}
		if st.Card == "" || v["id"] != st.Card {
			return nil, errors.New("这张卡片已经换了，刷新一下")
		}
		if _, err := a.grade(st.Card, r); err != nil {
			return nil, err
		}
		st.Card, st.Shown, st.Done = "", false, st.Done+1
	case "keep", "drop":
		var ids []string
		if id := v["id"]; id != "" {
			ids = []string{id}
		} else if v["all"] != "1" {
			return nil, errors.New("选一张卡片")
		}
		a.mu.Lock()
		n := 0
		err := a.update(func(s *state) error {
			if r.Action == "keep" {
				n = s.keepDrafts(ids, a.now())
			} else {
				n = s.dropDrafts(ids)
			}
			return nil
		})
		a.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if r.Action == "keep" {
			toast = fmt.Sprintf("留下了 %d 张", n)
		} else {
			toast = fmt.Sprintf("去掉了 %d 张", n)
		}
	case "make_topic":
		n, err := a.fromTopic(ctx, v["id"])
		if err != nil {
			return nil, err
		}
		st, toast = pageState{Mode: modeDrafts}, fmt.Sprintf("做了 %d 张卡片，确认一下", n)
	case "notion_search":
		st.Query = clip(strings.TrimSpace(v["q"]), 200)
	case "make_notion":
		n, err := a.fromNotion(ctx, v["id"])
		if err != nil {
			return nil, err
		}
		st, toast = pageState{Mode: modeDrafts}, fmt.Sprintf("做了 %d 张卡片，确认一下", n)
	case "import_vault":
		n, notes, err := a.importVault(ctx)
		if err != nil {
			return nil, err
		}
		toast = fmt.Sprintf("从 %d 条闪卡笔记导入了 %d 张新卡片", notes, n)
	case "vault_search":
		st.Query = clip(strings.TrimSpace(v["q"]), 200)
	case "make_vault":
		n, err := a.fromVault(ctx, v["path"])
		if err != nil {
			return nil, err
		}
		toast = fmt.Sprintf("这条笔记加了 %d 张卡片", n)
		if a.draftCount() > 0 {
			st = pageState{Mode: modeDrafts}
		}
	case "nlm_open":
		if !nlmID.MatchString(v["id"]) {
			return nil, errors.New("NotebookLM 笔记本 ID 不对")
		}
		st.Notebook, st.NBTitle = v["id"], clip(v["title"], 300)
	case "nlm_back":
		st.Notebook, st.NBTitle = "", ""
	case "import_nlm":
		n, err := a.fromNotebookLM(ctx, st.Notebook, v["id"], v["kind"], st.NBTitle)
		if err != nil {
			return nil, err
		}
		st, toast = pageState{Mode: modeDrafts}, fmt.Sprintf("从 NotebookLM 读回 %d 张，确认一下", n)
	case "import_logseq":
		n, pages, err := a.importLogseq(ctx)
		if err != nil {
			return nil, err
		}
		toast = fmt.Sprintf("从 %d 个页面导入了 %d 张新卡片", pages, n)
	case "logseq_search":
		st.Query = clip(strings.TrimSpace(v["q"]), 200)
	case "make_logseq":
		n, err := a.fromLogseq(ctx, v["path"])
		if err != nil {
			return nil, err
		}
		toast = fmt.Sprintf("这个页面加了 %d 张卡片", n)
		if a.draftCount() > 0 {
			st = pageState{Mode: modeDrafts}
		}
	case "weak_filter":
		f := v["status"]
		if f == "all" {
			f = ""
		}
		if f != "" && !validStatus(f) {
			return nil, errors.New("unknown filter")
		}
		st.Status = f
	case "go_topic":
		st = pageState{Mode: modeWeak, Topic: cleanTopic(v["topic"])}
	case "weak_topic":
		st.Topic, st.Status = cleanTopic(v["topic"]), ""
	case "guide_open", "guide_make":
		topic, body, title := cleanTopic(v["topic"]), "", ""
		if r.Action == "guide_make" {
			topic, body, title = "", strings.TrimSpace(v["text"]), v["title"]
			if body == "" {
				return nil, errors.New("先贴一段要精读的材料")
			}
			if utf8.RuneCountInString(body) > maxGuideText {
				return nil, fmt.Errorf("材料太长了，最多 %d 个字", maxGuideText)
			}
		} else if topic == "" {
			return nil, errors.New("先选一个知识点")
		}
		g, err := a.guide(ctx, topic, body, title)
		if err != nil {
			return nil, err
		}
		st = pageState{Mode: modeGuide, Guide: g.ID}
		if g.Plain {
			toast = "模型暂时不可用，先按卡片带你过一遍"
		}
	case "guide_show":
		if v["id"] == "" || len(v["id"]) > 64 {
			return nil, errors.New("精读 ID 不对")
		}
		st = pageState{Mode: modeGuide, Guide: v["id"]}
	case "guide_next", "guide_prev":
		g, ok := a.guideByID(st.Guide)
		if !ok {
			return nil, errors.New("这份精读已经不在了，重新生成一份")
		}
		i := min(max(st.Section, 0), len(g.Sections)-1)
		if r.Action == "guide_next" {
			i = min(i+1, len(g.Sections)-1)
		} else {
			i = max(i-1, 0)
		}
		st.Section, st.GuideChecked, st.GuideAns = i, false, ""
	case "guide_check":
		answer := strings.TrimSpace(v["answer"])
		if answer == "" {
			return nil, errors.New("先写下你的回答；想不起来就点「查看参考答案」")
		}
		st.GuideChecked, st.GuideAns = true, clip(strings.Join(strings.Fields(answer), " "), maxCheckText)
	case "guide_reveal":
		st.GuideChecked, st.GuideAns = true, ""
	case "guide_drafts":
		n, err := a.guideDrafts(st.Guide)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			toast = "这份精读的卡片都已经有了"
		} else {
			st, toast = pageState{Mode: modeDrafts}, fmt.Sprintf("做了 %d 张卡片，确认一下", n)
		}
	case "tutor_kind":
		if !validKind(v["kind"]) {
			return nil, errors.New("unknown exercise kind")
		}
		st.Kind, st.Exercise, st.Hints = v["kind"], "", 0
	case "tutor_start":
		e, err := a.practice(ctx, st.Card, st.Topic, cmp.Or(st.Kind, kindConcept))
		if err != nil {
			return nil, err
		}
		st.Exercise, st.Hints, st.Kind = e.ID, 0, cmp.Or(st.Kind, kindConcept)
		if e.Plain {
			toast = "模型暂时不可用，先用卡片原题练"
		}
	case "tutor_hint":
		n, err := a.serveHint(st.Exercise, st.Hints+1)
		if err != nil {
			return nil, err
		}
		st.Hints = n
	case "tutor_answer":
		answer := v["answer"]
		if strings.TrimSpace(answer) == "" {
			return nil, errors.New("先写下你的答案；不会的话点「不会，看答案」")
		}
		if _, err := a.checkAnswer(ctx, st.Exercise, answer, st.Hints); err != nil {
			return nil, err
		}
	case "reinforce_exercise", "reinforce_topic":
		var n int
		var err error
		if r.Action == "reinforce_exercise" {
			n, _, err = a.reinforce(ctx, st.Exercise, "", 3)
		} else {
			n, _, err = a.reinforce(ctx, "", st.Topic, 3)
		}
		if err != nil {
			return nil, err
		}
		st, toast = pageState{Mode: modeDrafts}, fmt.Sprintf("做了 %d 张强化卡，确认一下", n)
	case "set_topic":
		topic := cleanTopic(v["topic"])
		id := st.Card
		a.mu.Lock()
		err := a.update(func(s *state) error {
			c := s.Cards[id]
			if c == nil {
				return errors.New("没有这张卡片")
			}
			c.Topic = topic
			return nil
		})
		a.mu.Unlock()
		if err != nil {
			return nil, err
		}
		toast = "知识点改好了"
	case "open_card":
		if len(v["id"]) > 64 {
			return nil, errors.New("卡片 ID 不对")
		}
		st = pageState{Mode: modeCard, Card: v["id"]}
	case "pause", "resume", "delete":
		id := st.Card
		a.mu.Lock()
		err := a.update(func(s *state) error {
			c := s.Cards[id]
			if c == nil {
				return errors.New("没有这张卡片")
			}
			switch r.Action {
			case "pause":
				c.Suspended = true
			case "resume":
				c.Suspended = false
			default:
				delete(s.Cards, id)
			}
			return nil
		})
		a.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if r.Action == "delete" {
			st, toast = pageState{Mode: modeCards}, "删掉了"
		}
	case "write":
		front, back := strings.TrimSpace(v["front"]), strings.TrimSpace(v["back"])
		if front == "" || back == "" {
			return nil, errors.New("问题和答案都要写")
		}
		n, err := a.addDirect([]cardText{{Front: front, Back: back}}, Source{Kind: srcManual, Ref: "page"})
		if err != nil {
			return nil, err
		}
		if n == 0 {
			toast = "这张卡片已经有了"
		} else {
			toast = "加好了"
		}
	default:
		return nil, fmt.Errorf("unknown page action %q", r.Action)
	}
	view, err := a.view(ctx, st, toast)
	if err != nil {
		return nil, err
	}
	return &wire.ActionResult{View: view, Toast: toast}, nil
}

func (a *app) draftCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.st.Drafts)
}

func (a *app) view(ctx context.Context, st pageState, _ string) (*wire.View, error) {
	var v *wire.View
	var err error
	switch st.Mode {
	case modeReview:
		v = a.reviewView(st)
	case modeDrafts:
		v = a.draftsView(st)
	case modeTopics:
		v, err = a.topicsView(ctx, st)
	case modeNotion:
		v, err = a.notionView(ctx, st)
	case modeObsidian:
		v, err = a.obsidianView(ctx, st)
	case modeNLM:
		v, err = a.nlmView(ctx, st)
	case modeCards:
		v = a.cardsView(st)
	case modeCard:
		v = a.cardView(st)
	case modeWrite:
		v = writeView(st)
	case modeLogseq:
		v, err = a.logseqView(ctx, st)
	case modeWeak:
		v = a.weakView(st)
	case modeTutor:
		v = a.tutorView(st)
	case modeGuide:
		v = a.guideView(st)
	case modeMap:
		v = a.mapView(st)
	default:
		v = a.homeView(st)
	}
	if err != nil {
		v = &wire.View{Title: "学习", Sections: []wire.Section{
			{Items: []wire.Component{empty("没读到", err.Error())}},
			{Items: []wire.Component{button("返回", "home", nil)}},
		}}
	}
	v.Title = clip(v.Title, wire.StringMost)
	v.State = ext.EncodeState(a.reviewState(st))
	return v, nil
}

func backSection() wire.Section {
	return wire.Section{Items: []wire.Component{button("返回", "home", nil)}}
}

func (a *app) homeView(_ pageState) *wire.View {
	a.mu.Lock()
	s := computeStats(a.st, a.now(), a.loc, a.cfg.NewPerDay)
	a.mu.Unlock()
	top := wire.Component{Type: wire.ComponentStatGrid, Stats: []wire.Stat{
		{Label: "待复习", Value: fmt.Sprint(s.Due), Tone: map[bool]string{true: wire.ToneCaution, false: wire.ToneNeutral}[s.Due > 0]},
		{Label: "今天已复习", Value: fmt.Sprint(s.ReviewedToday)},
		{Label: "连续天数", Value: fmt.Sprint(s.Streak)},
		{Label: "已掌握", Value: fmt.Sprint(s.Mature), Detail: fmt.Sprintf("共 %d 张", s.Total)},
	}}
	first := []wire.Component{top}
	if s.Due > 0 {
		first = append(first, button(fmt.Sprintf("开始复习（%d 张）", s.Due), "go", map[string]string{"mode": modeReview}))
	} else if s.Total == 0 {
		first = append(first, text("还没有卡片。从下面的笔记和知识库里做一些，或者自己写一张。", wire.TextStyleSecondary))
	} else {
		first = append(first, text("今天没有要复习的了。", wire.TextStyleSecondary))
	}
	if s.RetentionN >= 10 {
		first = append(first, text(fmt.Sprintf("最近 30 天记得的比例 %.0f%%（%d 次复习）", s.Retention*100, s.RetentionN), wire.TextStyleCaption))
	}
	sections := []wire.Section{{Items: first}}
	if s.Total > 0 {
		sections = append(sections, wire.Section{Title: "深度辅导", Items: []wire.Component{
			row("知识地图", "按掌握度分层看全部知识点，点进去看卡片和错因", "go", map[string]string{"mode": modeMap}),
			row("精读学习", "一节一节讲：直觉、类比、要点，再来一道随堂小测", "go", map[string]string{"mode": modeGuide}),
			row("薄弱知识点", a.weakSummary(), "go", map[string]string{"mode": modeWeak}),
			row("Tutor 练习", "概念辨析、填空、选择、场景变式，一步步提示，答完诊断错因", "go", map[string]string{"mode": modeTutor}),
		}})
	}
	if s.Drafts > 0 {
		sections = append(sections, wire.Section{Items: []wire.Component{row(fmt.Sprintf("有 %d 张卡片等你确认", s.Drafts), "模型、NotebookLM 和辅导做的卡片，留下的才会排进复习", "go", map[string]string{"mode": modeDrafts})}})
	}
	if s.Total == 0 {
		// A lesson can be made from a pasted text, which needs no cards.
		sections = append(sections, wire.Section{Title: "深度辅导", Items: []wire.Component{
			row("精读学习", "贴一段材料，一节一节讲：直觉、类比、要点，再来一道随堂小测", "go", map[string]string{"mode": modeGuide}),
		}})
	}
	sections = append(sections, wire.Section{Title: "加卡片", Items: []wire.Component{
		row("知识库主题", "从 Persona 记下的知识点做卡片", "go", map[string]string{"mode": modeTopics}),
		row("Notion", "从分享给 integration 的页面做卡片，折叠块直接成卡", "go", map[string]string{"mode": modeNotion}),
		row("Obsidian", "导入写了闪卡语法的笔记，或从一条笔记做卡片", "go", map[string]string{"mode": modeObsidian}),
		row("Logseq", "导入标了 #card 的块，或从一个页面做卡片", "go", map[string]string{"mode": modeLogseq}),
		row("NotebookLM", "读回 NotebookLM 生成的闪卡和测验", "go", map[string]string{"mode": modeNLM}),
		row("自己写一张", "", "go", map[string]string{"mode": modeWrite}),
	}})
	if s.Total > 0 {
		sections = append(sections, wire.Section{Items: []wire.Component{row("全部卡片", fmt.Sprintf("%d 张，新卡 %d，学习中 %d，暂停 %d", s.Total, s.New, s.Learning, s.Suspended), "go", map[string]string{"mode": modeCards})}})
	}
	return &wire.View{Title: "学习", Sections: sections}
}

func (a *app) reviewView(st pageState) *wire.View {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	q := queue(a.st, now, a.loc, a.cfg.NewPerDay)
	var c *Card
	if st.Card != "" {
		c = a.st.Cards[st.Card]
	}
	if c == nil {
		st.Shown = false
		if len(q) > 0 {
			c = q[0]
		}
	}
	if c == nil {
		msg := "今天的都复习完了。"
		if st.Done > 0 {
			msg = fmt.Sprintf("复习完了，这次 %d 张。", st.Done)
		}
		return &wire.View{Title: "复习", Sections: []wire.Section{{Items: []wire.Component{empty(msg, "到时间会提醒你下一批。")}}, backSection()}}
	}
	st.Card = c.ID
	head := []wire.Component{
		text(fmt.Sprintf("还剩 %d 张 · %s", len(q), sourceLine(c.Source)), wire.TextStyleCaption),
		text(c.Front, wire.TextStyleHeadline),
	}
	sections := []wire.Section{{Items: head}}
	if !st.Shown {
		sections = append(sections, wire.Section{Items: []wire.Component{button("看答案", "show", nil)}})
	} else {
		pv := a.sched.preview(c, now)
		var btns []wire.Component
		for _, r := range ratings {
			btns = append(btns, button(fmt.Sprintf("%s · %s", ratingLabel(r), humanGap(now, pv[r])), "grade", map[string]string{"id": c.ID, "r": ratingKey(r)}))
		}
		sections = append(sections, wire.Section{Items: []wire.Component{answerBlock(c.Back)}}, wire.Section{Title: "记得怎么样？", Items: btns})
	}
	sections = append(sections, backSection())
	return &wire.View{Title: "复习", Sections: sections}
}

// The review view picks the card; the state it hands out must name it, so render writes it back.
func (a *app) reviewState(st pageState) pageState {
	if st.Mode != modeReview {
		return st
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if st.Card != "" && a.st.Cards[st.Card] != nil {
		return st
	}
	st.Shown = false
	if q := queue(a.st, a.now(), a.loc, a.cfg.NewPerDay); len(q) > 0 {
		st.Card = q[0].ID
	} else {
		st.Card = ""
	}
	return st
}

func (a *app) draftsView(_ pageState) *wire.View {
	a.mu.Lock()
	drafts := append([]Draft(nil), a.st.Drafts...)
	a.mu.Unlock()
	if len(drafts) == 0 {
		return &wire.View{Title: "待确认", Sections: []wire.Section{{Items: []wire.Component{empty("没有等确认的卡片", "")}}, backSection()}}
	}
	sections := []wire.Section{{Footer: "这些卡片是模型、NotebookLM 或辅导做的，可能不准。留下的才会排进复习。", Items: []wire.Component{
		button(fmt.Sprintf("全部留下（%d 张）", len(drafts)), "keep", map[string]string{"all": "1"}),
		{Type: wire.ComponentButton, Title: "全部不要", Action: &wire.Action{ID: "drop", Values: map[string]string{"all": "1"}, Confirm: "去掉全部待确认的卡片？"}},
	}}}
	for i, d := range drafts {
		if i == 30 {
			sections = append(sections, wire.Section{Items: []wire.Component{text(fmt.Sprintf("还有 %d 张，先处理上面的", len(drafts)-30), wire.TextStyleCaption)}})
			break
		}
		title := sourceLine(d.Source)
		if d.Topic != "" {
			title += " · " + d.Topic
		}
		sections = append(sections, wire.Section{Title: clip(title, wire.StringMost), Items: []wire.Component{
			text(d.Front, wire.TextStyleBody),
			text(d.Back, wire.TextStyleSecondary),
			button("留下", "keep", map[string]string{"id": d.ID}),
			button("不要", "drop", map[string]string{"id": d.ID}),
		}})
	}
	sections = append(sections, backSection())
	return &wire.View{Title: "待确认", Sections: sections}
}

func (a *app) topicsView(ctx context.Context, _ pageState) (*wire.View, error) {
	items, err := a.topics(ctx)
	if err != nil {
		return nil, err
	}
	var rows []wire.Component
	for _, t := range items[:min(len(items), maxListRows)] {
		rows = append(rows, row(cmp.Or(t.Name, t.ID), clip(t.OneLine, 120), "make_topic", map[string]string{"id": t.ID}))
	}
	if len(rows) == 0 {
		rows = []wire.Component{empty("知识库里还没有主题", "Persona 从对话和笔记里整理出主题后，这里就能做卡片。")}
	}
	return &wire.View{Title: "知识库主题", Sections: []wire.Section{{Footer: "点一个主题，模型会从它的知识点做几张卡片，等你确认。", Items: rows}, backSection()}}, nil
}

func (a *app) notionView(ctx context.Context, st pageState) (*wire.View, error) {
	if !a.notion.ready() {
		return &wire.View{Title: "Notion", Sections: []wire.Section{{Items: []wire.Component{empty("还没连 Notion", "在扩展设置里填 Notion integration secret，并把要学的页面分享给它。")}}, backSection()}}, nil
	}
	search := []wire.Component{
		{Type: wire.ComponentField, Name: "q", Label: "搜索页面", Kind: wire.FieldKindText, Value: st.Query, Placeholder: "页面标题"},
		{Type: wire.ComponentButton, Title: "搜索", Action: &wire.Action{ID: "notion_search", Submit: true}},
	}
	pages, err := a.notion.search(ctx, st.Query)
	if err != nil {
		return nil, err
	}
	var rows []wire.Component
	for _, p := range pages[:min(len(pages), maxListRows)] {
		rows = append(rows, row(pageTitle(p), "", "make_notion", map[string]string{"id": p.ID}))
	}
	if len(rows) == 0 {
		rows = []wire.Component{empty("没找到页面", "只能看到分享给 integration 的页面。")}
	}
	return &wire.View{Title: "Notion", Sections: []wire.Section{{Items: search},
		{Footer: "点一页做卡片：页面里的折叠块（标题是问题，折叠的内容是答案）直接成卡，没有折叠块时由模型来做。", Items: rows}, backSection()}}, nil
}

func (a *app) obsidianView(ctx context.Context, st pageState) (*wire.View, error) {
	a.mu.Lock()
	tag := a.cfg.FlashTag
	a.mu.Unlock()
	if _, err := a.machine(); err != nil {
		return &wire.View{Title: "Obsidian", Sections: []wire.Section{{Items: []wire.Component{empty("还没选电脑", "在扩展设置里选放 Obsidian 库的电脑，再填库的文件夹。")}}, backSection()}}, nil
	}
	top := []wire.Component{
		text(fmt.Sprintf("带 #%s 标签的笔记里，用 Spaced Repetition 插件语法写的卡片（问题::答案、? 分行、==挖空==）会直接导入。", tag), wire.TextStyleSecondary),
		button("导入闪卡笔记", "import_vault", nil),
	}
	search := []wire.Component{
		{Type: wire.ComponentField, Name: "q", Label: "找一条笔记", Kind: wire.FieldKindText, Value: st.Query, Placeholder: "文件名"},
		{Type: wire.ComponentButton, Title: "搜索", Action: &wire.Action{ID: "vault_search", Submit: true}},
	}
	var scan vaultScan
	if err := a.node(ctx, "vault_notes", map[string]any{"query": st.Query}, &scan); err != nil {
		return nil, err
	}
	var rows []wire.Component
	for _, n := range scan.Notes[:min(len(scan.Notes), maxListRows)] {
		rows = append(rows, row(cmp.Or(n.Title, n.Path), n.Path, "make_vault", map[string]string{"path": n.Path}))
	}
	if len(rows) == 0 {
		rows = []wire.Component{empty("没找到笔记", "")}
	}
	return &wire.View{Title: "Obsidian", Sections: []wire.Section{{Items: top}, {Items: search},
		{Footer: "点一条笔记做卡片：有闪卡语法就直接导入，没有就由模型来做。", Items: rows}, backSection()}}, nil
}

func (a *app) nlmView(ctx context.Context, st pageState) (*wire.View, error) {
	intro := text("通过社区开源的 notebooklm-py 命令行读回 NotebookLM 生成的闪卡和测验。它用的是 Google 网页的非公开接口，随时可能失效。", wire.TextStyleSecondary)
	if _, err := a.machine(); err != nil {
		return &wire.View{Title: "NotebookLM", Sections: []wire.Section{{Items: []wire.Component{intro, empty("还没选电脑", "在扩展设置里选装了 notebooklm 命令的电脑。")}}, backSection()}}, nil
	}
	var status nlmState
	if err := a.node(ctx, "nlm_status", map[string]any{}, &status); err != nil {
		return nil, err
	}
	if !status.SignedIn {
		help := "在那台电脑上装好 notebooklm-py，再运行 notebooklm login 登录 Google。"
		if status.Installed {
			help = "在那台电脑上运行 notebooklm login 重新登录 Google。"
		}
		return &wire.View{Title: "NotebookLM", Sections: []wire.Section{{Items: []wire.Component{intro, empty(status.Message, help)}}, backSection()}}, nil
	}
	var items []nlmItem
	if err := a.node(ctx, "nlm_list", map[string]any{"notebook": st.Notebook}, &items); err != nil {
		return nil, err
	}
	var rows []wire.Component
	if len(items) > maxListRows {
		items = items[:maxListRows]
	}
	if st.Notebook == "" {
		for _, it := range items {
			rows = append(rows, row(it.Title, "", "nlm_open", map[string]string{"id": it.ID, "title": it.Title}))
		}
		if len(rows) == 0 {
			rows = []wire.Component{empty("没有笔记本", "")}
		}
		return &wire.View{Title: "NotebookLM", Sections: []wire.Section{{Items: []wire.Component{intro}}, {Title: "笔记本", Items: rows}, backSection()}}, nil
	}
	for _, it := range items {
		kind := map[string]string{"flashcards": "闪卡", "quiz": "测验"}[it.Kind]
		rows = append(rows, row(it.Title, kind, "import_nlm", map[string]string{"id": it.ID, "kind": it.Kind}))
	}
	if len(rows) == 0 {
		rows = []wire.Component{empty("这个笔记本还没有闪卡或测验", "先在 NotebookLM 里生成一份，再回来读。")}
	}
	return &wire.View{Title: fit(st.NBTitle, "NotebookLM"), Sections: []wire.Section{
		{Footer: "点一份读回来，卡片先进待确认。", Items: rows},
		{Items: []wire.Component{button("其他笔记本", "nlm_back", nil)}}, backSection()}}, nil
}

func (a *app) cardsView(_ pageState) *wire.View {
	a.mu.Lock()
	cards := a.st.sortedCards()
	now, loc := a.now(), a.loc
	a.mu.Unlock()
	var rows []wire.Component
	for i, c := range cards {
		if i == 200 {
			rows = append(rows, text(fmt.Sprintf("只列最新的 200 张，共 %d 张", len(cards)), wire.TextStyleCaption))
			break
		}
		rows = append(rows, row(oneLine(c.Front), cardWhen(c, now, loc), "open_card", map[string]string{"id": c.ID}))
	}
	if len(rows) == 0 {
		rows = []wire.Component{empty("还没有卡片", "")}
	}
	return &wire.View{Title: "全部卡片", Sections: []wire.Section{{Items: rows}, backSection()}}
}

func cardWhen(c *Card, now time.Time, loc *time.Location) string {
	switch {
	case c.Suspended:
		return "暂停中 · " + sourceLine(c.Source)
	case c.FSRS.State == fsrs.New:
		return "新卡 · " + sourceLine(c.Source)
	case !c.FSRS.Due.After(now):
		return "该复习了 · " + sourceLine(c.Source)
	}
	return c.FSRS.Due.In(loc).Format("1月2日") + " 复习 · " + sourceLine(c.Source)
}

func (a *app) cardView(st pageState) *wire.View {
	a.mu.Lock()
	c := a.st.Cards[st.Card]
	var cp Card
	if c != nil {
		cp = *c
	}
	now, loc := a.now(), a.loc
	a.mu.Unlock()
	if c == nil {
		return &wire.View{Title: "卡片", Sections: []wire.Section{{Items: []wire.Component{empty("没有这张卡片", "")}}, backSection()}}
	}
	items := []wire.Component{
		text(cp.Front, wire.TextStyleHeadline),
		answerBlock(cp.Back),
		text(cardWhen(&cp, now, loc), wire.TextStyleCaption),
		text(fmt.Sprintf("知识点：%s · 掌握度 %s", topicOf(&cp), a.cardMasteryLabel(&cp)), wire.TextStyleCaption),
	}
	if cp.FSRS.Reps > 0 {
		items = append(items, text(fmt.Sprintf("复习过 %d 次，忘过 %d 次", cp.FSRS.Reps, cp.FSRS.Lapses), wire.TextStyleCaption))
	}
	if cp.Source.URL != "" {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: "打开原文", URL: cp.Source.URL})
	}
	pause := button("暂停复习", "pause", nil)
	if cp.Suspended {
		pause = button("恢复复习", "resume", nil)
	}
	del := wire.Component{Type: wire.ComponentButton, Title: "删除", Action: &wire.Action{ID: "delete", Confirm: "删除这张卡片和它的复习进度？"}}
	topic := []wire.Component{
		{Type: wire.ComponentField, Name: "topic", Label: "知识点", Kind: wire.FieldKindText, Value: clip(cp.Topic, maxTopic), Placeholder: clip(topicOf(&cp), maxTopic)},
		{Type: wire.ComponentButton, Title: "改知识点", Action: &wire.Action{ID: "set_topic", Submit: true}},
	}
	practice := button("练这张卡", "go", map[string]string{"mode": modeTutor, "card": cp.ID})
	return &wire.View{Title: "卡片", Sections: []wire.Section{{Items: items}, {Items: []wire.Component{practice, pause, del}}, {Footer: "空着就归到来源的标题下。", Items: topic},
		{Items: []wire.Component{button("返回", "go", map[string]string{"mode": modeCards})}}}}
}

func writeView(_ pageState) *wire.View {
	return &wire.View{Title: "写一张卡片", Sections: []wire.Section{{Items: []wire.Component{
		{Type: wire.ComponentField, Name: "front", Label: "问题", Kind: wire.FieldKindMultiline},
		{Type: wire.ComponentField, Name: "back", Label: "答案", Kind: wire.FieldKindMultiline},
		{Type: wire.ComponentButton, Title: "加上", Action: &wire.Action{ID: "write", Submit: true}},
	}}, backSection()}}
}

// ---- widget ----

func (a *app) renderWidget(_ context.Context, r *ext.UIRequest) (*wire.View, error) {
	a.mu.Lock()
	s := computeStats(a.st, a.now(), a.loc, a.cfg.NewPerDay)
	a.mu.Unlock()
	tone := wire.TonePositive
	summary := "今天复习完了"
	if s.Due > 0 {
		tone, summary = wire.ToneCaution, fmt.Sprintf("%d 张待复习", s.Due)
	} else if s.Total == 0 {
		tone, summary = wire.ToneNeutral, "还没有卡片"
	}
	var items []wire.Component
	switch r.Family() {
	case wire.WidgetFamilyAccessoryInline, wire.WidgetFamilyAccessoryCircular:
		items = []wire.Component{{Type: wire.ComponentText, Text: map[bool]string{true: fmt.Sprint(s.Due), false: summary}[r.Family() == wire.WidgetFamilyAccessoryCircular], Tone: tone}}
	default:
		items = []wire.Component{{Type: wire.ComponentMetric, Label: "待复习", Value: fmt.Sprint(s.Due), Tone: tone}}
		if r.Family() != wire.WidgetFamilyAccessoryRectangular {
			items = append(items, text(fmt.Sprintf("今天已复习 %d · 连续 %d 天", s.ReviewedToday, s.Streak), wire.TextStyleCaption))
		}
	}
	return &wire.View{Sections: []wire.Section{{Items: items}}, RefreshAfter: 1800}, nil
}
