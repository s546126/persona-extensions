package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The program on the hub: only the extension's page, which asks each switched-on computer's program for its state
// and its newest pairs (nodes.call → ebd.status) and lists them.

const logUI = "log"

// nodeCaller is how the page reaches a computer's program: the hub's nodes.call, or a fake in tests.
type nodeCaller func(ctx context.Context, machine string, out any) error

type hubApp struct {
	mu       sync.Mutex
	machines []string
	call     nodeCaller
}

func (a *hubApp) program(id, version string) ext.Program {
	return ext.Program{
		ID: id, Version: version, Manifest: manifest,
		Init: a.init,
		UI:   map[string]ext.Page{logUI: {Render: a.render}},
	}
}

func (a *hubApp) init(_ context.Context, in *ext.Init, hub *ext.Hub) (map[string]any, error) {
	var c config
	if err := in.Config(&c); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.machines = c.Machines
	if a.call == nil {
		a.call = func(ctx context.Context, machine string, out any) error {
			return hub.Call(ctx, "nodes.call", map[string]any{"machine": machine, "method": "ebd.status", "params": map[string]any{}}, out)
		}
	}
	return nil, nil
}

func (a *hubApp) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	t := texts(r.Locale)
	loc := time.Local
	if l, err := time.LoadLocation(r.TZ); err == nil && r.TZ != "" {
		loc = l
	}
	a.mu.Lock()
	machines, call := append([]string(nil), a.machines...), a.call
	a.mu.Unlock()

	v := &wire.View{Title: t.title, RefreshAfter: 60}
	if len(machines) == 0 {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.noMachines, Text: t.noMachinesHint}}}}
		return v, nil
	}
	states := make([]status, len(machines))
	errs := make([]error, len(machines))
	var wg sync.WaitGroup
	for i, m := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			errs[i] = call(cctx, m, &states[i])
		}()
	}
	wg.Wait()
	for i, m := range machines {
		v.Sections = append(v.Sections, machineSection(t, loc, m, states[i], errs[i]))
	}
	v.Sections = append(v.Sections, wire.Section{Items: []wire.Component{{Type: wire.ComponentText, Style: wire.TextStyleCaption, Text: t.footer}}})
	return v, nil
}

func machineSection(t text, loc *time.Location, machine string, st status, err error) wire.Section {
	s := wire.Section{Title: machine}
	if err != nil {
		s.Items = append(s.Items, wire.Component{Type: wire.ComponentRow, Title: t.unreachable, Subtitle: oneLine(err.Error()),
			Badge: &wire.Badge{Text: t.offline, Tone: wire.ToneCaution}})
		return s
	}
	for _, d := range st.Dirs {
		row := wire.Component{Type: wire.ComponentRow, Title: d.Dir}
		switch {
		case d.Error != "":
			row.Subtitle, row.Badge = oneLine(d.Error), &wire.Badge{Text: t.notInstalled, Tone: wire.ToneCaution}
		case d.Installed:
			row.Subtitle, row.Badge = t.modeName(st.Mode), &wire.Badge{Text: t.installed, Tone: wire.TonePositive}
		default:
			row.Subtitle, row.Badge = d.Settings, &wire.Badge{Text: t.notInstalled, Tone: wire.ToneCaution}
		}
		s.Items = append(s.Items, row)
	}
	s.Items = append(s.Items,
		wire.Component{Type: wire.ComponentRow, Title: t.total, Value: fmt.Sprint(st.Total)},
		wire.Component{Type: wire.ComponentRow, Title: t.due, Value: fmt.Sprint(st.Due)})
	if len(st.Recent) == 0 {
		s.Items = append(s.Items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: t.none})
		return s
	}
	for _, e := range st.Recent {
		when := e.At
		if at, err := time.Parse(time.RFC3339, e.At); err == nil {
			when = at.In(loc).Format("01-02 15:04")
		}
		english := e.English
		if english == "" {
			english = t.noEnglish
		}
		meta := []string{when, t.entryMode(e.Mode)}
		if e.Score != nil {
			meta = append(meta, fmt.Sprintf(t.score, *e.Score))
		}
		if e.Natural != nil && !*e.Natural && e.NaturalHint != "" {
			meta = append(meta, t.better+e.NaturalHint)
		}
		s.Items = append(s.Items, wire.Component{Type: wire.ComponentRow, Title: oneLine(e.Original),
			Subtitle: oneLine(english) + "\n" + strings.Join(meta, " · ")})
	}
	s.Footer = fmt.Sprintf(t.recent, len(st.Recent))
	return s
}

// text is the page's words in one language.
type text struct {
	title, noMachines, noMachinesHint, unreachable, offline, installed, notInstalled, none, total, due, noEnglish,
	score, better, recent, footer string
	modes, entryModes map[string]string
}

func (t text) modeName(m string) string { return or(t.modes[m], m) }

func (t text) entryMode(m string) string { return or(t.entryModes[m], m) }

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func texts(locale string) text {
	switch {
	case strings.HasPrefix(locale, "en"):
		return text{
			title: "Pairs", noMachines: "No computer switched on",
			noMachinesHint: "Turn on a computer in this extension's settings: its Claude Code then gets the hook.",
			unreachable:    "Can't reach this computer's program", offline: "Unreachable",
			installed: "Hook installed", notInstalled: "No hook", none: "No pairs yet.", total: "Pairs saved", due: "Due for review",
			noEnglish: "(no English yet)", score: "score %d", better: "more natural: ", recent: "Newest %d, newest first",
			footer:     "What you quote (code, quoted text, links, paths, pasted text after an English line ending in a colon) does not count; only your own words do. Slash commands, urgent words and Persona's own assistant conversations go through as they are. Pairs stay on each computer.",
			modes:      map[string]string{"block": "Block", "warn": "Warn", "log": "Log only"},
			entryModes: map[string]string{"rewrite": "rewritten", "unverified": "unchecked", "giveup": "gave up", "warn": "warned", "log": "logged", "stopword": "urgent"},
		}
	case strings.HasPrefix(locale, "ja"):
		return text{
			title: "対訳", noMachines: "オンのコンピュータがありません",
			noMachinesHint: "この拡張の設定でコンピュータをオンにすると、その Claude Code に hook が入ります。",
			unreachable:    "このコンピュータのプログラムに接続できません", offline: "接続できません",
			installed: "hook あり", notInstalled: "hook なし", none: "まだ対訳はありません。", total: "記録した対訳", due: "復習の時期",
			noEnglish: "（英語はまだありません）", score: "スコア %d", better: "より自然に：", recent: "新しい順に %d 件",
			footer:     "引用したもの（コード、かぎかっこや引用符の中、リンク、パス、コロンで終わる英語の行の後に貼った文章）は数えず、自分で書いた部分だけで判断します。スラッシュコマンド、緊急ワード、Persona のアシスタント自身の会話はそのまま通します。対訳は各コンピュータに残ります。",
			modes:      map[string]string{"block": "ブロック", "warn": "注意", "log": "記録のみ"},
			entryModes: map[string]string{"rewrite": "書き直し", "unverified": "未判定", "giveup": "諦めた", "warn": "注意", "log": "記録", "stopword": "緊急"},
		}
	}
	return text{
		title: "中英对照", noMachines: "还没有打开任何电脑",
		noMachinesHint: "在这个扩展的设置里打开一台电脑，那台电脑的 Claude Code 就会装上 hook。",
		unreachable:    "连不上这台电脑上的程序", offline: "连不上",
		installed: "已装 hook", notInstalled: "没有 hook", none: "还没有对照。", total: "记下的对照", due: "该复习了",
		noEnglish: "（还没有英文）", score: "得分 %d", better: "更地道：", recent: "最近 %d 条，新的在前",
		footer:     "只看你自己写的部分：代码、引号里的文字、链接、路径，以及英文句子以冒号结尾后贴上的内容都不算。斜杠命令、带紧急词的提示和 Persona 助手自己的对话照常发出。对照记在各自的电脑上。",
		modes:      map[string]string{"block": "拦截", "warn": "提醒", "log": "只记录"},
		entryModes: map[string]string{"rewrite": "自己改写", "unverified": "未判断", "giveup": "放弃", "warn": "提醒", "log": "记录", "stopword": "紧急"},
	}
}
