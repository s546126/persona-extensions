package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The nav page "网站订阅": what it is for and how it stands on top (sources, unread, last check, the RSSHub instance,
// what is failing), then either the latest entries with the subscriptions grouped by tag, one feed's entries, one
// entry's text, or a source's preview; a form at the bottom previews or subscribes to a source. The words are in
// words.go.

const (
	pageEntries    = 30
	pageFeedMost   = 60
	pageTextMost   = 1800 // characters of one text component
	pageEntryParts = 6    // text components of an entry's body
	refreshBudget  = 90 * time.Second
)

// pageState is what the page keeps between draws.
type pageState struct {
	Feed    string `json:"f,omitempty"`
	Entry   string `json:"e,omitempty"`
	Preview string `json:"p,omitempty"`
	Unread  bool   `json:"u,omitempty"`
}

func readState(r *ext.UIRequest) pageState {
	var st pageState
	_ = r.DecodeState(&st)
	st.Preview = clip(st.Preview, routeMost)
	return st
}

func (a *app) render(_ context.Context, r *ext.UIRequest) (*wire.View, error) {
	return a.page(r, readState(r)), nil
}

func (a *app) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	st := readState(r)
	kind, arg, _ := strings.Cut(r.Action, ":")
	toast := ""
	switch kind {
	case "refresh":
		ctx, cancel := context.WithTimeout(ctx, refreshBudget)
		defer cancel()
		if arg != "" {
			n, err := a.refresh(ctx, arg)
			if err != nil {
				toast = explain(err.Error()).toast("没读到：")
			} else {
				toast = freshWords(n)
			}
			a.badge()
			break
		}
		if a.health().Config != nil {
			toast = explain(a.health().Config.Error()).toast("")
			break
		}
		n, failed := a.refreshAll(ctx)
		toast = freshWords(n)
		if failed > 0 {
			toast += fmt.Sprintf("；%d 个网站没读到，点开标着「出错」的看原因", failed)
		}
	case "ping":
		ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		defer cancel()
		if err := a.ping(ctx); err != nil {
			toast = explain(err.Error()).toast("连不上 RSSHub：")
		} else {
			toast = "能连上 RSSHub"
		}
	case "feed":
		st.Feed, st.Entry, st.Preview = arg, "", ""
	case "entry":
		st.Entry = arg
		_, _ = a.markRead("", []string{arg}, false, true)
	case "unread":
		if _, err := a.markRead("", []string{arg}, false, false); err == nil {
			toast = "标回未读了"
		}
		st.Entry = ""
	case "read":
		n, err := a.markRead(arg, nil, arg == "", true)
		if err != nil {
			toast = err.Error()
		} else {
			toast = fmt.Sprintf("%d 篇标为已读", n)
		}
	case "only_unread":
		st.Unread = !st.Unread
		if v, ok := r.Values["filter"]; ok {
			st.Unread = v == "unread"
		}
	case "back":
		switch {
		case st.Entry != "":
			st.Entry = ""
		case st.Preview != "":
			st.Preview = ""
		default:
			st.Feed = ""
		}
	case "preview":
		route := strings.TrimSpace(firstNonEmpty(r.Values["route"], arg))
		p, err := a.preview(ctx, route)
		if err != nil {
			toast = explain(err.Error()).toast("")
			break
		}
		a.keepPreview(p)
		st.Preview, st.Feed, st.Entry = p.Source, "", ""
	case "subscribe":
		route := strings.TrimSpace(firstNonEmpty(r.Values["route"], st.Preview))
		ctx, cancel := context.WithTimeout(ctx, fetchTimeout+5*time.Second)
		defer cancel()
		f, added, n, err := a.subscribe(ctx, route, r.Values["title"], r.Values["tag"])
		switch {
		case err != nil:
			toast = explain(err.Error()).toast("没订阅上：")
		case !added:
			toast = "已经订阅过了"
			st.Feed, st.Preview, st.Entry = f.ID, "", ""
		default:
			toast = fmt.Sprintf("订阅了「%s」，现在有 %d 篇", f.name(), n)
			st.Feed, st.Preview, st.Entry = f.ID, "", ""
		}
	case "unsubscribe":
		f, err := a.unsubscribe(arg)
		if err != nil {
			toast = err.Error()
		} else {
			toast = "取消订阅了「" + f.name() + "」"
		}
		st.Feed, st.Entry = "", ""
	}
	return &wire.ActionResult{View: a.page(r, st), Toast: toast}, nil
}

// freshWords is the toast after a check: how many new entries it found.
func freshWords(n int) string {
	if n == 0 {
		return "检查完了，没有新文章"
	}
	return fmt.Sprintf("检查完了，有 %d 篇新文章", n)
}

// ping asks the instance for its front page: whether it answers at all.
func (a *app) ping(ctx context.Context) error {
	inst := a.instance()
	if h := a.health(); h.Config != nil {
		return h.Config
	}
	u := *inst.base
	u.Path += "/"
	_, err := fetch(ctx, a.client, dialURL(&u, a.getenv), "", "", about.Version)
	a.instanceAnswered(err)
	if err != nil {
		return fmt.Errorf("%s", inst.redact(err.Error()))
	}
	return nil
}

// --- drawing ---

const timeFormat = "01-02 15:04"

func (a *app) page(r *ext.UIRequest, st pageState) *wire.View {
	loc := zone(r.TZ)
	st0 := a.feeds()
	feeds := st0.list()
	v := &wire.View{Title: pageTitle, Sections: []wire.Section{a.overview(r, feeds, loc)}}
	switch {
	case st.Entry != "":
		if f, e, ok := st0.entryByID(st.Entry); ok {
			v.Sections = append(v.Sections, entrySection(f, e, loc)...)
			break
		}
		st.Entry = ""
		fallthrough
	case st.Preview != "":
		if st.Preview != "" {
			if p, ok := a.previews.Load(st.Preview); ok {
				v.Sections = append(v.Sections, previewSection(p.(*preview), loc))
				break
			}
			st.Preview = ""
		}
		fallthrough
	case st.Feed != "":
		if st.Feed != "" {
			if f, ok := st0.get(st.Feed); ok {
				v.Sections = append(v.Sections, feedSections(r, f, st, loc)...)
				break
			}
			st.Feed = ""
		}
		fallthrough
	default:
		v.Sections = append(v.Sections, a.latest(r, st, feeds))
		add := addSection(st)
		budget := wire.ItemsMost - viewItemCount(v) - len(add.Items)
		v.Sections = append(v.Sections, feedGroups(feeds, loc, budget)...)
		v.Sections = append(v.Sections, add)
		v.State = ext.EncodeState(st)
		if r.UIVersion >= 2 {
			v.RefreshAfter = 300
		}
		return v
	}
	v.Sections = append(v.Sections, addSection(st))
	v.State = ext.EncodeState(st)
	if r.UIVersion >= 2 {
		v.RefreshAfter = 300
	}
	return v
}

func viewItemCount(v *wire.View) int {
	n := 0
	for _, s := range v.Sections {
		n += len(s.Items)
	}
	return n
}

// pageTitle is the page's and the navigation entry's name (persona-extension.json contributes.ui).
const pageTitle = "网站订阅"

// lastCheck is when any feed was last read, zero before the first.
func lastCheck(feeds []feed) time.Time {
	var t time.Time
	for _, f := range feeds {
		if f.LastFetch.After(t) {
			t = f.LastFetch
		}
	}
	return t
}

// overview is the top of the page: what this is, how it stands (sources, unread, last check, the RSSHub service)
// and what is wrong, with the buttons that act on everything.
func (a *app) overview(r *ext.UIRequest, feeds []feed, loc *time.Location) wire.Section {
	h := a.health()
	unread := 0
	var failing []string
	for _, f := range feeds {
		unread += f.unread()
		if f.LastError != "" {
			failing = append(failing, "「"+clip(f.name(), 40)+"」")
		}
	}
	// Until something is read through RSSHub (a source previewed or read, or the button below) nothing has asked
	// the service; with nothing subscribed the stat says nothing needs doing, so "not tried" is not read as a task.
	state, tone := "还没用到", wire.ToneNeutral
	switch {
	case h.Config != nil:
		state, tone = "地址填错了", wire.ToneDanger
	case h.failing():
		state, tone = "连不上", wire.ToneDanger
	case !h.OK.IsZero():
		state, tone = "能连上", wire.TonePositive
	}
	service := clip(h.Host, 60)
	switch {
	case h.Config != nil:
		service = "设置里的地址用不了"
	case h.Host == "rsshub.app":
		service = "免费公共服务 rsshub.app"
	}
	if len(feeds) == 0 && h.Config == nil && !h.failing() && h.OK.IsZero() {
		service += "；" + serviceIdleNote
	}
	checked := "还没检查过"
	if t := lastCheck(feeds); !t.IsZero() {
		checked = t.In(loc).Format(timeFormat)
	}
	every := int(h.Every / time.Minute)
	sec := wire.Section{Items: []wire.Component{{Type: wire.ComponentText, Text: introText}}}
	if r.UIVersion >= 2 {
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentStatGrid, Stats: []wire.Stat{
			{Label: "订阅的网站", Value: strconv.Itoa(len(feeds))},
			{Label: "未读文章", Value: strconv.Itoa(unread)},
			{Label: "上次检查", Value: checked, Detail: fmt.Sprintf("每 %d 分钟自动检查", every)},
			{Label: "RSSHub 服务", Value: state, Detail: service, Tone: tone},
		}})
	} else {
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentText,
			Text: fmt.Sprintf("订阅了 %d 个网站 · 未读 %d 篇 · 上次检查：%s · RSSHub 服务：%s（%s）", len(feeds), unread, checked, state, service)})
	}
	if len(failing) > 0 {
		names := failing
		if len(names) > 5 {
			names = append(names[:5:5], "等")
		}
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentText,
			Text: fmt.Sprintf("有 %d 个网站上次没读到：%s。点开下面标着「出错」的网站，看原因和怎么办。", len(failing), strings.Join(names, ""))})
	}
	switch {
	case h.Config != nil:
		sec.Items = append(sec.Items, problemTexts("", explain(h.Config.Error()))...)
	case h.failing():
		sec.Items = append(sec.Items, problemTexts("上次连不上 RSSHub：", explain(h.Err.Error()))...)
	}
	if len(feeds) > 0 {
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentButton, Title: "现在检查更新", Action: &wire.Action{ID: "refresh"}})
	}
	if unread > 0 {
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentButton, Title: "全部标为已读", Action: &wire.Action{ID: "read", Confirm: "把所有网站的文章都标为已读？"}})
	}
	sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentButton, Title: "试试能不能连上 RSSHub", Action: &wire.Action{ID: "ping"}})
	sec.Footer = fmt.Sprintf("每 %d 分钟自动检查一次有没有新文章；间隔和 RSSHub 服务地址在这个扩展的设置里改，一般不用改。", every)
	return sec
}

// problemTexts is a failure on the page: what happened and what to do, then the raw message in small print.
func problemTexts(prefix string, p problem) []wire.Component {
	out := []wire.Component{
		{Type: wire.ComponentText, Text: clip(prefix+p.Cause+"。", pageTextMost)},
		{Type: wire.ComponentText, Text: clip("怎么办："+p.Todo+"。", pageTextMost)},
	}
	if p.Detail != "" {
		out = append(out, wire.Component{Type: wire.ComponentText, Text: clip(detailPrefix+p.Detail, pageTextMost), Style: wire.TextStyleCaption})
	}
	return out
}

// latest is the newest entries of every feed (or the unread ones), or what to do first when there are none.
func (a *app) latest(r *ext.UIRequest, st pageState, feeds []feed) wire.Section {
	list, total := a.feeds().entries(entryFilter{UnreadOnly: st.Unread, Limit: pageEntries})
	title := "最新文章"
	if st.Unread {
		title = "未读文章"
	}
	sec := wire.Section{Title: title}
	if len(feeds) == 0 {
		sec.Items = append(sec.Items, emptyItems(r, "还没有订阅任何网站",
			"在下面「添加订阅」里粘贴一个地址，先点「先看看」确认能读到，再点「订阅」。不知道填什么，就点下面的按钮试一个例子。",
			&wire.EmptyButton{Title: "试一个例子：" + exampleRouteTitle, Action: wire.Action{ID: "preview:" + exampleRoute}})...)
		return sec
	}
	sec.Items = append(sec.Items, unreadToggle(r, st))
	if len(list) == 0 {
		text, detail := "订阅的网站暂时还没有文章", fmt.Sprintf("每 %d 分钟会自动检查一次，也可以点上面的「现在检查更新」。", int(a.health().Every/time.Minute))
		if st.Unread {
			text, detail = "没有未读的了", ""
		}
		sec.Items = append(sec.Items, emptyState(r, text, detail, nil))
		return sec
	}
	for _, e := range list {
		sec.Items = append(sec.Items, entryRow(e.entry, e.FeedName, zone(r.TZ)))
	}
	if total > len(list) {
		sec.Footer = fmt.Sprintf("一共 %d 篇，这里是最新的 %d 篇", total, len(list))
	}
	return sec
}

func unreadToggle(r *ext.UIRequest, st pageState) wire.Component {
	value := "all"
	if st.Unread {
		value = "unread"
	}
	if r.UIVersion >= 2 {
		return wire.Component{Type: wire.ComponentSegmented, Name: "filter", Value: value, Action: &wire.Action{ID: "only_unread"},
			Choices: []wire.Choice{{Value: "all", Title: "全部"}, {Value: "unread", Title: "只看未读"}}}
	}
	title := "只看未读"
	if st.Unread {
		title = "看全部"
	}
	return wire.Component{Type: wire.ComponentButton, Title: title, Action: &wire.Action{ID: "only_unread"}}
}

// emptyState is "nothing here" with what to do next; an app before UI version 3 gets texts and a button.
func emptyState(r *ext.UIRequest, title, detail string, button *wire.EmptyButton) wire.Component {
	if r.UIVersion >= 3 {
		return wire.Component{Type: wire.ComponentEmpty, Title: title, Detail: detail, Icon: wire.NavIconList, Button: button}
	}
	text := title
	if detail != "" {
		text += "。" + detail
	}
	return wire.Component{Type: wire.ComponentText, Text: text, Style: wire.TextStyleSecondary}
}

// emptyItems is emptyState with, for an older app, its button as a button of its own.
func emptyItems(r *ext.UIRequest, title, detail string, button *wire.EmptyButton) []wire.Component {
	out := []wire.Component{emptyState(r, title, detail, button)}
	if r.UIVersion < 3 && button != nil {
		act := button.Action
		out = append(out, wire.Component{Type: wire.ComponentButton, Title: button.Title, Action: &act})
	}
	return out
}

func entryRow(e entry, from string, loc *time.Location) wire.Component {
	var sub []string
	if from != "" {
		sub = append(sub, from)
	}
	if t := e.when(); !t.IsZero() {
		sub = append(sub, t.In(loc).Format(timeFormat))
	}
	if e.Summary != "" {
		sub = append(sub, e.Summary)
	}
	row := wire.Component{Type: wire.ComponentRow, Title: clip(firstNonEmpty(e.Title, "（无标题）"), 300),
		Subtitle: clip(strings.Join(sub, " · "), 300), Action: &wire.Action{ID: "entry:" + e.ID}}
	if !e.Read {
		row.Badge = &wire.Badge{Text: "未读", Tone: wire.ToneCaution}
	}
	return row
}

// feedGroups is the subscriptions, one section a tag, bounded to fit within budget items.
func feedGroups(feeds []feed, loc *time.Location, budget int) []wire.Section {
	if len(feeds) == 0 || budget <= 0 {
		return nil
	}
	var out []wire.Section
	shown := 0
	for _, t := range tagsOf(feeds) {
		if shown >= budget {
			break
		}
		sec := wire.Section{Title: "我订阅的 · " + t}
		for _, f := range feeds {
			if f.tag() != t {
				continue
			}
			if shown >= budget {
				break
			}
			row := wire.Component{Type: wire.ComponentRow, Title: clip(f.name(), 300), Subtitle: clip(f.Source+" · "+fetchState(f, loc), 300),
				Action: &wire.Action{ID: "feed:" + f.ID}}
			switch {
			case f.LastError != "":
				row.Badge = &wire.Badge{Text: "出错", Tone: wire.ToneDanger}
			case f.unread() > 0:
				row.Badge = &wire.Badge{Text: strconv.Itoa(f.unread()), Tone: wire.ToneCaution}
			}
			sec.Items = append(sec.Items, row)
			shown++
		}
		if len(sec.Items) > 0 {
			out = append(out, sec)
		}
	}
	if omitted := len(feeds) - shown; omitted > 0 && len(out) > 0 {
		out[len(out)-1].Footer = fmt.Sprintf("还有 %d 个网站未在此页显示", omitted)
	}
	return out
}

// fetchState is how a feed's last check went, short.
func fetchState(f feed, loc *time.Location) string {
	switch {
	case f.LastFetch.IsZero():
		return "还没检查过"
	case f.LastError != "":
		return f.LastFetch.In(loc).Format(timeFormat) + " 没读到：" + explain(f.LastError).Cause
	}
	return f.LastOK.In(loc).Format(timeFormat) + " 检查过"
}

// feedSections is one feed: what it is, how its last check went, its buttons and its entries.
func feedSections(r *ext.UIRequest, f feed, st pageState, loc *time.Location) []wire.Section {
	head := wire.Section{Title: clip(f.name(), 200), Items: []wire.Component{
		{Type: wire.ComponentText, Text: clip(fmt.Sprintf("地址 %s · 分类 %s · 未读 %d 篇，一共 %d 篇", f.Source, f.tag(), f.unread(), len(f.Entries)), pageTextMost), Style: wire.TextStyleSecondary},
	}}
	switch {
	case f.LastFetch.IsZero():
		head.Items = append(head.Items, wire.Component{Type: wire.ComponentText, Text: "还没检查过，稍后会自动检查。", Style: wire.TextStyleSecondary})
	case f.LastError != "":
		head.Items = append(head.Items, problemTexts("上次检查（"+f.LastFetch.In(loc).Format(timeFormat)+"）没读到：", explain(f.LastError))...)
	default:
		head.Items = append(head.Items, wire.Component{Type: wire.ComponentText, Text: "上次检查 " + f.LastOK.In(loc).Format(timeFormat) + "，一切正常。", Style: wire.TextStyleSecondary})
	}
	if httpsLink(f.SiteLink) {
		head.Items = append(head.Items, wire.Component{Type: wire.ComponentLink, Title: "打开网站", URL: f.SiteLink})
	}
	head.Items = append(head.Items,
		wire.Component{Type: wire.ComponentButton, Title: "现在检查更新", Action: &wire.Action{ID: "refresh:" + f.ID}},
	)
	if f.unread() > 0 {
		head.Items = append(head.Items, wire.Component{Type: wire.ComponentButton, Title: "全部标为已读", Action: &wire.Action{ID: "read:" + f.ID}})
	}
	head.Items = append(head.Items,
		wire.Component{Type: wire.ComponentButton, Title: "取消订阅", Role: wire.ButtonRoleDestructive,
			Action: &wire.Action{ID: "unsubscribe:" + f.ID, Confirm: "取消订阅「" + clip(f.name(), 60) + "」？它的文章会一起删掉。"}},
		wire.Component{Type: wire.ComponentButton, Title: "返回", Action: &wire.Action{ID: "back"}},
	)
	list := wire.Section{Title: "文章"}
	list.Items = append(list.Items, unreadToggle(r, st))
	n := 0
	for _, e := range f.Entries {
		if st.Unread && e.Read {
			continue
		}
		if n++; n > pageFeedMost {
			break
		}
		list.Items = append(list.Items, entryRow(e, "", loc))
	}
	if n == 0 {
		text := "这个网站还没有文章"
		if st.Unread {
			text = "没有未读的了"
		}
		list.Items = append(list.Items, emptyState(r, text, "", nil))
	}
	return []wire.Section{head, list}
}

// httpsLink reports whether a link component can carry raw: the view check refuses the whole view for any other.
func httpsLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && utf8.RuneCountInString(raw) <= wire.StringMost
}

// entrySection is one entry's text.
func entrySection(f feed, e entry, loc *time.Location) []wire.Section {
	var meta []string
	meta = append(meta, f.name())
	if e.Author != "" {
		meta = append(meta, e.Author)
	}
	if t := e.when(); !t.IsZero() {
		meta = append(meta, t.In(loc).Format("2006-01-02 15:04"))
	}
	sec := wire.Section{Title: clip(firstNonEmpty(e.Title, "（无标题）"), 300), Items: []wire.Component{
		{Type: wire.ComponentText, Text: clip(strings.Join(meta, " · "), pageTextMost), Style: wire.TextStyleSecondary},
	}}
	sec.Items = append(sec.Items, bodyTexts(firstNonEmpty(e.Content, e.Summary))...)
	switch {
	case httpsLink(e.Link):
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentLink, Title: "打开原文", URL: e.Link})
	case e.Link != "":
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentText, Text: clip("原文链接："+e.Link, pageTextMost), Style: wire.TextStyleCaption})
	}
	sec.Items = append(sec.Items,
		wire.Component{Type: wire.ComponentButton, Title: "标回未读", Action: &wire.Action{ID: "unread:" + e.ID}},
		wire.Component{Type: wire.ComponentButton, Title: "返回", Action: &wire.Action{ID: "back"}},
	)
	return []wire.Section{sec}
}

// bodyTexts cuts a text into a few text components at paragraph ends.
func bodyTexts(s string) []wire.Component {
	s = clip(s, pageTextMost*pageEntryParts)
	var out []wire.Component
	var cur strings.Builder
	push := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, wire.Component{Type: wire.ComponentText, Text: clip(t, pageTextMost)})
		}
		cur.Reset()
	}
	for _, para := range strings.Split(s, "\n") {
		if cur.Len() > 0 && len([]rune(cur.String()))+len([]rune(para)) > pageTextMost-2 {
			push()
		}
		if cur.Len() > 0 {
			cur.WriteString("\n")
		}
		cur.WriteString(para)
	}
	push()
	if len(out) > pageEntryParts {
		out = out[:pageEntryParts]
	}
	return out
}

// previewSection is a source read without subscribing: what it has now, or why it could not be read.
func previewSection(p *preview, loc *time.Location) wire.Section {
	sec := wire.Section{Title: "先看看 · " + clip(p.Source, 200)}
	if p.Err != "" {
		sec.Items = append(sec.Items, problemTexts("没读到：", explain(p.Err))...)
	} else {
		f := p.Feed
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentText,
			Text: clip(fmt.Sprintf("「%s」能读到，现在有 %d 篇（%s 读的）。合适就点「订阅这个」，以后有新文章会出现在这里。", firstNonEmpty(f.Title, p.Source), len(f.Entries), p.At.In(loc).Format("15:04")), pageTextMost), Style: wire.TextStyleSecondary})
		for i, pe := range f.Entries {
			if i >= 15 {
				break
			}
			var sub []string
			if !pe.Published.IsZero() {
				sub = append(sub, pe.Published.In(loc).Format(timeFormat))
			}
			if pe.Summary != "" {
				sub = append(sub, pe.Summary)
			}
			sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentRow, Title: clip(firstNonEmpty(pe.Title, "（无标题）"), 300), Subtitle: clip(strings.Join(sub, " · "), 300)})
		}
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentButton, Title: "订阅这个", Action: &wire.Action{ID: "subscribe", Submit: true}})
	}
	sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentButton, Title: "返回", Action: &wire.Action{ID: "back"}})
	return sec
}

// addSection is the form that previews or subscribes to a source, with what can be pasted into it.
func addSection(st pageState) wire.Section {
	return wire.Section{Title: "添加订阅", Items: []wire.Component{
		{Type: wire.ComponentText, Text: addressHelp, Style: wire.TextStyleSecondary},
		{Type: wire.ComponentField, Name: "route", Label: "要订阅的地址", Kind: wire.FieldKindText, Value: clip(st.Preview, wire.StringMost),
			Placeholder: "比如 " + exampleUPRoute + " 或 https://…/feed"},
		{Type: wire.ComponentField, Name: "tag", Label: "分类（可不填，用来分组）", Kind: wire.FieldKindText, Placeholder: "比如 新闻、学习"},
		{Type: wire.ComponentField, Name: "title", Label: "名字（可不填，默认用网站自己的）", Kind: wire.FieldKindText},
		{Type: wire.ComponentButton, Title: "先看看", Action: &wire.Action{ID: "preview", Submit: true}},
		{Type: wire.ComponentButton, Title: "订阅", Action: &wire.Action{ID: "subscribe", Submit: true}},
		{Type: wire.ComponentLink, Title: "查更多网站的 RSSHub 短地址", URL: routesDocs},
	}, Footer: "「先看看」只读一次给你看，不会订阅；读得到再点「订阅」。"}
}

func zone(tz string) *time.Location {
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	return time.Local
}
