package main

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	// allValue is the switcher's choice for every space; a space's slug may not take it (validSlug).
	allValue = "all"
	// newValue is the switcher's choice to create a new space; a space's slug may not take it (validSlug).
	newValue = "new"
	// pickPrefix names the space in a switcher row's action id for apps that send no action values.
	pickPrefix = "pick:"
	// tabsMost is how many items tabs hold (All and + New space included); more spaces get a drop-down filter.
	tabsMost = 8
	// filterMost is how many choices a filter holds.
	filterMost = 100
	// fetchMost spaces are read per page, fetchParallel at a time; the rest are listed without counts.
	fetchMost     = 20
	fetchParallel = 4
	// conversationsLimit is how many conversations a page asks per space.
	conversationsLimit = 20
	// mergedMost is how many conversations the page of every space lists.
	mergedMost = 30
	// excerptLines and excerptRunes bound the SPACE.md excerpt.
	excerptLines = 8
	excerptRunes = 600
	// spaceMDWait bounds reading SPACE.md, which lives on the assistant's computer and can be offline.
	spaceMDWait = 8 * time.Second
	// tabTitleMost and tabBadgeMost are the tab's title and badge bounds.
	tabTitleMost = 40
	tabBadgeMost = 8
)

// clock is the time relative times are written against; tests set it.
var clock = time.Now

// env is what a page needs from the request: the app's UI version (0: not said, the newest) and its time zone.
type env struct {
	ui  int
	loc *time.Location
	now time.Time
}

func envOf(r *ext.UIRequest) env {
	loc := time.Local
	if r != nil && r.TZ != "" {
		if l, err := time.LoadLocation(r.TZ); err == nil {
			loc = l
		}
	}
	e := env{loc: loc, now: clock()}
	if r != nil {
		e.ui = r.UIVersion
	}
	return e
}

// atLeast says whether the app draws UI version v; an app that does not say draws the newest.
func (e env) atLeast(v int) bool { return e.ui == 0 || e.ui >= v }

// spaceInfo is what the page read about one space.
type spaceInfo struct {
	track   Track
	fetched bool
	machine string
	total   int
	chats   []Conversation
	chatErr error
	withMD  bool
	md      string
	mdErr   error
}

// fetch reads the conversations (and SPACE.md, when withMD) of each space, fetchParallel at a time. A failure is kept
// on its space and logged; it never fails the page.
func fetch(ctx context.Context, c *Client, infos []*spaceInfo) {
	sem := make(chan struct{}, fetchParallel)
	var wg sync.WaitGroup
	for _, info := range infos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			info.fetched = true
			info.machine, info.total, info.chats, info.chatErr = c.SpaceConversations(ctx, info.track.Slug, conversationsLimit)
			if info.chatErr != nil {
				log.Printf("空间对话：读取空间 %s 的对话失败：%v", info.track.Slug, info.chatErr)
			}
			if info.withMD {
				mdCtx, cancel := context.WithTimeout(ctx, spaceMDWait)
				md, err := c.SpaceMD(mdCtx, info.track.Slug)
				cancel()
				info.md, info.mdErr = md.Content, err
			}
		}()
	}
	wg.Wait()
}

// pageView draws the space page (home, and space with its state): a switcher of the spaces, then the chosen space
// (sel), or every space when sel is "" or all, or names a space that is gone.
func pageView(ctx context.Context, c *Client, e env, sel string) (*wire.View, error) {
	tracks, err := c.Tracks(ctx)
	if err != nil {
		return nil, err
	}
	sel = strings.TrimSpace(sel)
	if sel == allValue {
		sel = ""
	}
	var selected *Track
	for i := range tracks {
		if sel != "" && tracks[i].Slug == sel && isSpace(tracks[i]) {
			selected = &tracks[i]
			break
		}
	}
	missing := sel != "" && selected == nil

	var spaces []Track
	seenSlug := make(map[string]struct{}, len(tracks))
	for _, t := range tracks {
		if !isActiveSpace(t) || strings.TrimSpace(t.Slug) == "" {
			continue
		}
		if _, dup := seenSlug[t.Slug]; dup {
			continue
		}
		seenSlug[t.Slug] = struct{}{}
		spaces = append(spaces, t)
	}
	if selected != nil && selected.Archived {
		if _, dup := seenSlug[selected.Slug]; !dup {
			// still in the switcher, so its value is one of the items
			spaces = append(spaces, *selected)
		}
	}
	if len(spaces) == 0 {
		var items []wire.Component
		if missing {
			items = append(items, secondary("这个空间已不存在。"))
		}
		items = append(items, button("new", "新建空间", nil), noSpaces())
		return &wire.View{Title: "空间对话", State: allValue, Sections: []wire.Section{{Title: "空间", Items: items}}, RefreshAfter: 60}, nil
	}

	infos := map[string]*spaceInfo{}
	var batch []*spaceInfo
	for i, t := range spaces {
		if i >= fetchMost && (selected == nil || t.Slug != selected.Slug) {
			continue
		}
		info := &spaceInfo{track: t, withMD: selected == nil || t.Slug == selected.Slug}
		infos[t.Slug] = info
		batch = append(batch, info)
	}
	var launchers TrackLaunchers
	if selected != nil && !selected.Archived {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			lCtx, cancel := context.WithTimeout(ctx, spaceMDWait)
			defer cancel()
			if res, err := c.TrackLaunchers(lCtx); err == nil {
				launchers = res
			}
		}()
		fetch(ctx, c, batch)
		wg.Wait()
	} else {
		fetch(ctx, c, batch)
	}

	value := allValue
	if selected != nil {
		value = selected.Slug
	}
	v := &wire.View{Title: "空间对话", State: value, RefreshAfter: 30}
	switcherRows := switcher(v, e, spaces, infos, value)
	if selected != nil {
		v.Title = spaceTitle(*selected)
		if len(switcherRows) > 0 {
			v.Sections = append(v.Sections, wire.Section{Items: switcherRows})
		}
		v.Sections = append(v.Sections, selectedSections(e, infos[selected.Slug], launchers, countItems(v))...)
		return v, nil
	}
	v.Sections = allSections(e, spaces, infos, missing)
	return v, nil
}

func countItems(v *wire.View) int {
	n := 0
	for _, s := range v.Sections {
		n += len(s.Items)
	}
	return n
}

type switchItem struct{ value, title, badge string }

// switcher sets the view's tabs (at most tabsMost items) or filter, as the app draws them, with "+ 新建空间" at the end.
// An app before UI version 3 gets rows instead, returned for the page of one space.
func switcher(v *wire.View, e env, spaces []Track, infos map[string]*spaceInfo, value string) []wire.Component {
	items := []switchItem{{value: allValue, title: "全部"}}
	for _, t := range spaces {
		it := switchItem{value: t.Slug, title: clipRunes(spaceTitle(t), tabTitleMost)}
		if info := infos[t.Slug]; info != nil && info.fetched && info.chatErr == nil {
			it.badge = countBadge(info.total)
		}
		items = append(items, it)
	}
	items = append(items, switchItem{value: newValue, title: "+ 新建空间"})
	pick := wire.Action{ID: "pick"}
	switch {
	case e.atLeast(4) && len(items) <= tabsMost:
		tabs := &wire.Tabs{Name: "space", Value: value, Action: pick}
		for _, it := range items {
			tabs.Items = append(tabs.Items, wire.Tab{Value: it.value, Title: it.title, Badge: it.badge})
		}
		v.Tabs = tabs
	case e.atLeast(3):
		filter := &wire.Filter{Name: "space", Title: "空间", Value: value, Action: pick}
		for _, it := range limitChoices(items, value) {
			filter.Choices = append(filter.Choices, wire.Choice{Value: it.value, Title: it.title})
		}
		v.Filter = filter
	default:
		if value == allValue {
			return nil
		}
		var rows []wire.Component
		for _, it := range limitChoices(items, value) {
			row := wire.Component{Type: wire.ComponentRow, Title: it.title, Value: it.badge, Action: pickAction(e, it.value)}
			if it.value == value {
				row.Badge = &wire.Badge{Text: "当前"}
			}
			rows = append(rows, row)
		}
		return rows
	}
	return nil
}

// limitChoices keeps at most filterMost items, the chosen one among them.
func limitChoices(items []switchItem, value string) []switchItem {
	if len(items) <= filterMost {
		return items
	}
	out := slices.Clone(items[:filterMost])
	if !slices.ContainsFunc(out, func(it switchItem) bool { return it.value == value }) {
		if i := slices.IndexFunc(items, func(it switchItem) bool { return it.value == value }); i >= 0 {
			out[filterMost-1] = items[i]
		}
	}
	return out
}

// pickAction chooses a space: values {space} from UI version 4, the space in the id before (falling back to values when the id would exceed 128 bytes).
func pickAction(e env, value string) *wire.Action {
	if e.atLeast(4) || len(pickPrefix)+len(value) > 128 {
		return action("pick", map[string]string{"space": clipRunes(value, wire.StringMost)})
	}
	return action(pickPrefix+value, nil)
}

// countBadge writes a count for a tab's badge: nothing for none, 99+ above 99.
func countBadge(n int) string {
	switch {
	case n <= 0:
		return ""
	case n > 99:
		return "99+"
	}
	return clipRunes(strconv.Itoa(n), tabBadgeMost)
}

// selectedSections draws one space in conversation-first order: a compact quick-start box (with launcher, model and
// effort selectors when the hub provides them, unless archived), recent conversations on the first screen, and a folded
// single-line SPACE.md summary with view/edit and archive below. used is how many items the view already has.
func selectedSections(e env, info *spaceInfo, launchers TrackLaunchers, used int) []wire.Section {
	t := info.track
	slug := t.Slug
	var sections []wire.Section
	if !t.Archived {
		startItems := []wire.Component{
			{Type: wire.ComponentField, Name: "prompt", Label: "消息", Kind: wire.FieldKindText, Placeholder: "输入消息，直接开始新对话…"},
		}
		if len(launchers.Launchers) > 0 {
			defaultLabel := "跟随默认"
			if launchers.Default.Launcher != "" {
				defaultLabel = "默认（" + clipRunes(launchers.Default.Launcher, 24) + "）"
			}
			choices := []wire.Choice{{Value: "", Title: defaultLabel}}
			seenLaunchers := map[string]struct{}{"": {}}
			for _, l := range launchers.Launchers {
				if len(choices) >= filterMost {
					break
				}
				name := strings.TrimSpace(l.Name)
				if name == "" {
					continue
				}
				if _, dup := seenLaunchers[name]; dup {
					continue
				}
				seenLaunchers[name] = struct{}{}
				label := cmp.Or(strings.TrimSpace(l.DisplayName), name)
				if l.DefaultModel != "" {
					label += " · " + l.DefaultModel
				}
				choices = append(choices, wire.Choice{Value: clipRunes(name, wire.StringMost), Title: clipRunes(label, wire.StringMost)})
			}
			modelPlaceholder := "留空用默认模型，或输入模型 ID"
			if launchers.Default.Model != "" {
				modelPlaceholder = "留空默认：" + clipRunes(launchers.Default.Model, 40)
			}
			startItems = append(startItems,
				wire.Component{Type: wire.ComponentField, Name: "launcher", Label: "启动器", Kind: wire.FieldKindChoice, Choices: choices},
				wire.Component{Type: wire.ComponentField, Name: "model", Label: "模型（可选）", Kind: wire.FieldKindText, Placeholder: modelPlaceholder},
				wire.Component{Type: wire.ComponentField, Name: "effort", Label: "思考档位", Kind: wire.FieldKindChoice, Choices: []wire.Choice{
					{Value: "", Title: "默认"},
					{Value: "low", Title: "低"},
					{Value: "medium", Title: "中"},
					{Value: "high", Title: "高"},
				}},
			)
		}
		startItems = append(startItems, wire.Component{
			Type:   wire.ComponentButton,
			Title:  "开始对话",
			Action: &wire.Action{ID: "start", Submit: true, Values: map[string]string{"slug": slug}},
		})
		sections = append(sections, wire.Section{Title: "新对话", Items: startItems})
		used += len(startItems)
	}

	// Reserve room for the folded SPACE.md summary row and archive/restore action below the conversations.
	const aboutItems = 2
	rows := []wire.Component{}
	for _, chat := range info.chats {
		if used+aboutItems+len(rows) >= wire.ItemsMost {
			break
		}
		rows = append(rows, chatRow(e, t, info.machine, chat))
	}
	if len(rows) == 0 {
		rows = append(rows, wire.Component{Type: wire.ComponentEmpty, Title: "还没有对话"})
	}
	sections = append(sections, wire.Section{Title: "最近对话", Footer: summaryLine(e, info), Items: rows})

	about := []wire.Component{
		{
			Type:     wire.ComponentRow,
			Title:    "SPACE.md",
			Subtitle: clipRunes(spaceSummaryLine(info), wire.StringMost),
			Value:    "查看 / 编辑",
			Icon:     "doc",
			Action:   action("edit", map[string]string{"slug": slug}),
		},
	}
	if t.Archived {
		about = append(about, button("unarchive", "恢复空间", map[string]string{"slug": slug}))
	} else {
		about = append(about, button("archive", "归档空间", map[string]string{"slug": slug}))
	}
	return append(sections, wire.Section{Title: "空间说明", Items: about})
}

// spaceSummaryLine folds SPACE.md (or the space description) into a single plain-text line for the compact summary row.
func spaceSummaryLine(info *spaceInfo) string {
	if info.mdErr != nil {
		return "SPACE.md 暂时读不到（助手所在的电脑可能离线）"
	}
	title := spaceTitle(info.track)
	if line := firstLine(spaceExcerpt(info.md, title)); line != "" {
		return line
	}
	if desc := strings.TrimSpace(info.track.Description); desc != "" {
		return desc
	}
	return "SPACE.md 还没有内容"
}

// summaryLine is "N 个对话 · 最近 <when>" for a space.
func summaryLine(e env, info *spaceInfo) string {
	if info.chatErr != nil {
		return "对话暂时读不到"
	}
	line := fmt.Sprintf("%d 个对话", info.total)
	if len(info.chats) > 0 {
		if when := relativeTime(info.chats[0].Updated(), e.now, e.loc); when != "" {
			line += " · 最近 " + when
		}
	}
	return line
}

// allSections draws every space: the spaces, each opening its own page, and their conversations merged.
// "+ 新建空间" lives in the top switcher bar so it stays one tap away without taking card height here.
func allSections(e env, spaces []Track, infos map[string]*spaceInfo, missing bool) []wire.Section {
	var items []wire.Component
	if missing {
		items = append(items, secondary("这个空间已不存在，下面是全部空间。"))
	}
	if !e.atLeast(3) {
		items = append(items, button("new", "新建空间", nil))
	}
	type labeled struct {
		track   Track
		machine string
		chat    Conversation
	}
	var merged []labeled
	rowsMost := wire.ItemsMost - mergedMost - len(items)
	for _, t := range spaces {
		info := infos[t.Slug]
		if len(items) < rowsMost {
			row := wire.Component{
				Type:   wire.ComponentRow,
				Title:  spaceTitle(t),
				Icon:   "chat",
				Action: pickAction(e, t.Slug),
			}
			subtitle := strings.TrimSpace(t.Description)
			if info != nil && info.mdErr == nil {
				subtitle = cmp.Or(firstLine(spaceExcerpt(info.md, spaceTitle(t))), subtitle)
			}
			row.Subtitle = clipRunes(subtitle, wire.StringMost)
			if info != nil && info.fetched && info.chatErr == nil {
				row.Value = fmt.Sprintf("%d 个对话", info.total)
			}
			items = append(items, row)
		}
		if info != nil {
			for _, chat := range info.chats {
				merged = append(merged, labeled{t, info.machine, chat})
			}
		}
	}
	slices.SortStableFunc(merged, func(a, b labeled) int { return b.chat.Updated().Compare(a.chat.Updated()) })
	rows := []wire.Component{}
	for _, m := range merged {
		if len(rows) >= mergedMost {
			break
		}
		rows = append(rows, chatRow(e, m.track, m.machine, m.chat))
	}
	if len(rows) == 0 {
		rows = append(rows, wire.Component{Type: wire.ComponentEmpty, Title: "还没有对话"})
	}
	return []wire.Section{{Title: "空间", Items: items}, {Title: "最近对话 · 全部空间", Items: rows}}
}

// chatRow is a conversation of space t, labeled with the space and opening the native chat.
func chatRow(e env, t Track, machine string, chat Conversation) wire.Component {
	return wire.Component{
		Type:     wire.ComponentRow,
		Title:    clipRunes(cmp.Or(strings.TrimSpace(chat.Title), strings.TrimSpace(chat.SessionID), "未命名对话"), wire.StringMost),
		Subtitle: clipRunes(strings.TrimSpace(chat.LastText), wire.StringMost),
		Value:    relativeTime(chat.Updated(), e.now, e.loc),
		Icon:     "chat",
		Badge:    &wire.Badge{Text: clipRunes(spaceTitle(t), tabTitleMost)},
		Action:   action("chat", map[string]string{"slug": t.Slug, "sid": chat.SessionID, "mid": cmp.Or(chat.MachineID, machine)}),
	}
}

var (
	managedBlock = regexp.MustCompile(`(?s)<!--\s*persona:managed:[^>]*?:begin\s*-->.*?<!--\s*persona:managed:[^>]*?:end\s*-->`)
	htmlComment  = regexp.MustCompile(`(?s)<!--.*?-->`)
)

// spaceExcerpt is the start of a SPACE.md for the page: without Persona's managed blocks and other HTML comments,
// without a leading "# title" that only repeats the space's title, at most excerptLines lines that are not blank and
// excerptRunes characters.
func spaceExcerpt(md, title string) string {
	md = managedBlock.ReplaceAllString(md, "")
	md = htmlComment.ReplaceAllString(md, "")
	if i := strings.Index(md, "<!--"); i >= 0 { // a comment never closed hides the rest
		md = md[:i]
	}
	var out []string
	kept, blank, first := 0, false, true
	for _, line := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			blank = len(out) > 0
			continue
		}
		if first {
			first = false
			if h, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok && strings.TrimSpace(h) == strings.TrimSpace(title) {
				continue
			}
		}
		if kept == excerptLines {
			break
		}
		if blank {
			out = append(out, "")
			blank = false
		}
		out = append(out, line)
		kept++
	}
	return strings.TrimSpace(clipRunes(strings.Join(out, "\n"), excerptRunes))
}

// firstLine is an excerpt's first line as plain words, for a row's subtitle.
func firstLine(excerpt string) string {
	line, _, _ := strings.Cut(excerpt, "\n")
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#>*-+ "))
}

// relativeTime writes t against now: 刚刚, N 分钟前, N 小时前, 昨天, N 天前, else the date (MM-DD); "" for no time.
func relativeTime(t, now time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	if loc == nil {
		loc = time.Local
	}
	d := now.Sub(t)
	days := dayNumber(now.In(loc)) - dayNumber(t.In(loc))
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d/time.Minute))
	case days == 0 || d < 6*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d/time.Hour))
	case days == 1:
		return "昨天"
	case days < 7:
		return fmt.Sprintf("%d 天前", days)
	}
	return t.In(loc).Format("01-02")
}

// dayNumber counts calendar days, so two times on the same date in a zone share it.
func dayNumber(t time.Time) int {
	y, m, d := t.Date()
	return int(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}
