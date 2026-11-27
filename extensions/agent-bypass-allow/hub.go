package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The program on the hub: only the extension's page, which asks each switched-on computer's program for its state
// and its newest auto-allows (nodes.call → bypass.status) and lists them.

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
			return hub.Call(ctx, "nodes.call", map[string]any{"machine": machine, "method": "bypass.status", "params": map[string]any{}}, out)
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
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.noMachines, Detail: t.noMachinesHint}}}}
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
	plans := planMachineItems(errs, states, wire.ItemsMost-1)
	for i, p := range plans {
		v.Sections = append(v.Sections, machineSection(t, loc, machines[i], states[i], errs[i], p))
	}
	v.Sections = append(v.Sections, wire.Section{Items: []wire.Component{{Type: wire.ComponentText, Style: wire.TextStyleCaption, Text: t.footer}}})
	return v, nil
}

type machineItemPlan struct {
	dirs     int
	recent   int
	showNone bool
}

// planMachineItems distributes the page's item budget fairly across machines so the total component count across all
// machine sections never exceeds budget (wire.ItemsMost - 1, leaving 1 slot for the footer).
func planMachineItems(errs []error, states []status, budget int) []machineItemPlan {
	n := min(len(states), budget)
	if n <= 0 {
		return nil
	}
	plans := make([]machineItemPlan, n)
	rem := budget - n // reserve 1 guaranteed item per shown machine
	extraDirWants := make([]int, n)
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			continue // guaranteed 1 slot is the unreachable row
		}
		switch {
		case len(states[i].Dirs) > 0:
			plans[i].dirs = 1
			extraDirWants[i] = len(states[i].Dirs) - 1
		case len(states[i].Recent) > 0:
			plans[i].recent = 1
		default:
			plans[i].showNone = true
		}
	}
	extraDirs := distribute(extraDirWants, rem)
	for i, d := range extraDirs {
		plans[i].dirs += d
		rem -= d
	}
	tailWants := make([]int, n)
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			continue
		}
		if len(states[i].Recent) == 0 {
			if !plans[i].showNone {
				tailWants[i] = 1
			}
		} else if len(states[i].Recent) > plans[i].recent {
			tailWants[i] = len(states[i].Recent) - plans[i].recent
		}
	}
	tailAlloc := distribute(tailWants, rem)
	for i, a := range tailAlloc {
		if a == 0 {
			continue
		}
		if len(states[i].Recent) == 0 {
			plans[i].showNone = true
		} else {
			plans[i].recent += a
		}
	}
	return plans
}

func distribute(wants []int, budget int) []int {
	got := make([]int, len(wants))
	if budget <= 0 {
		return got
	}
	rem := budget
	for rem > 0 {
		progressed := false
		for i, w := range wants {
			if got[i] < w && rem > 0 {
				got[i]++
				rem--
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	return got
}

func machineSection(t text, loc *time.Location, machine string, st status, err error, plan machineItemPlan) wire.Section {
	s := wire.Section{Title: oneLine(machine)}
	if err != nil {
		s.Items = append(s.Items, wire.Component{Type: wire.ComponentRow, Title: t.unreachable, Subtitle: rpcErrorText(t, err),
			Badge: &wire.Badge{Text: t.offline, Tone: wire.ToneCaution}})
		return s
	}
	for _, d := range st.Dirs[:min(plan.dirs, len(st.Dirs))] {
		row := wire.Component{Type: wire.ComponentRow, Title: cmp.Or(oneLine(d.Dir), "-")}
		switch {
		case d.Error != "":
			row.Subtitle, row.Badge = dirErrorText(t, d.Error), &wire.Badge{Text: t.notInstalled, Tone: wire.ToneCaution}
		case d.Installed:
			row.Subtitle, row.Badge = sanitizeDisplayPath(d.Settings), &wire.Badge{Text: t.installed, Tone: wire.TonePositive}
		default:
			row.Subtitle, row.Badge = sanitizeDisplayPath(d.Settings), &wire.Badge{Text: t.notInstalled, Tone: wire.ToneCaution}
		}
		s.Items = append(s.Items, row)
	}
	if len(st.Recent) == 0 {
		if plan.showNone {
			s.Items = append(s.Items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: t.none})
		}
		return s
	}
	shown := st.Recent[:min(plan.recent, len(st.Recent))]
	for _, e := range shown {
		when := e.At
		if at, err := time.Parse(time.RFC3339, e.At); err == nil {
			when = at.In(loc).Format("01-02 15:04:05")
		}
		meta := []string{when}
		if e.Cwd != "" {
			meta = append(meta, filepath.Base(e.Cwd))
		}
		if len(e.Session) >= 8 {
			meta = append(meta, e.Session[:8])
		}
		if e.Agent != "" {
			meta = append(meta, t.subagent)
		}
		if e.Note != "" {
			meta = append(meta, e.Note)
		}
		title := cmp.Or(oneLineMax(e.Command, commandShown+1), oneLineMax(e.Tool, 64), "-")
		s.Items = append(s.Items, wire.Component{Type: wire.ComponentRow, Title: title, Subtitle: oneLineMax(strings.Join(meta, " · "), 300)})
	}
	if len(shown) > 0 {
		s.Footer = fmt.Sprintf(t.recent, len(shown))
	}
	return s
}

func rpcErrorText(t text, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return t.rpcTimeout
	}
	if errors.Is(err, ext.ErrClosed) {
		return t.rpcOffline
	}
	var rpcErr *ext.Error
	if errors.As(err, &rpcErr) {
		code := strings.ToLower(strings.TrimSpace(rpcErr.Code))
		switch {
		case strings.Contains(code, "timeout") || strings.Contains(code, "deadline"):
			return t.rpcTimeout
		case code == "offline" || code == "unavailable" || code == "not_found" || code == "disconnected":
			return t.rpcOffline
		}
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "timed out") || strings.Contains(msg, "timeout"):
		return t.rpcTimeout
	case strings.Contains(msg, "offline") || strings.Contains(msg, "not connected") || strings.Contains(msg, "not running") ||
		strings.Contains(msg, "unavailable") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "no such machine"):
		return t.rpcOffline
	default:
		return t.rpcError
	}
}

func dirErrorText(t text, raw string) string {
	msg := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.Contains(msg, "permission denied"):
		return t.dirPerm
	case strings.Contains(msg, "not an absolute path"):
		return t.dirNotAbs
	case strings.Contains(msg, "home directory"):
		return t.dirNoHome
	case strings.Contains(msg, "no such directory") || strings.Contains(msg, "no such file"):
		return t.dirMissing
	case strings.Contains(msg, "not a json object"):
		return t.dirBadJSON
	case strings.Contains(msg, "invalid hook structure") || strings.Contains(msg, "is not an object") || strings.Contains(msg, "is not a list"):
		return t.dirBadHooks
	case strings.Contains(msg, "read settings"):
		return t.dirReadErr
	case strings.Contains(msg, "write settings") || strings.Contains(msg, "backup"):
		return t.dirWriteErr
	default:
		return t.dirError
	}
}

// sanitizeDisplayPath folds any full host path (for example from an older node) and bounds its length for the UI.
func sanitizeDisplayPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "/") {
		cleaned := path.Clean(p)
		parts := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
		if len(parts) >= 3 && (parts[0] == "home" || parts[0] == "Users") {
			p = "~/" + strings.Join(parts[2:], "/")
		} else {
			p = path.Base(cleaned)
		}
	}
	return oneLine(p)
}

func oneLine(s string) string {
	return oneLineMax(s, 200)
}

func oneLineMax(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxRunes {
		s = string(r[:maxRunes]) + "…"
	}
	return s
}

// text is the page's words in one language.
type text struct {
	title, noMachines, noMachinesHint, unreachable, offline, installed, notInstalled, none, subagent, recent, footer string
	rpcTimeout, rpcOffline, rpcError                                                                                 string
	dirNotAbs, dirNoHome, dirMissing, dirBadJSON, dirBadHooks, dirPerm, dirReadErr, dirWriteErr, dirError            string
}

func texts(locale string) text {
	switch {
	case strings.HasPrefix(locale, "en"):
		return text{
			title: "Auto-allowed", noMachines: "No computer switched on",
			noMachinesHint: "Turn on a computer in this extension's settings: its Claude Code and Antigravity CLI then get the hook.",
			unreachable:    "Can't reach this computer's program", offline: "Unreachable",
			installed: "Hook installed", notInstalled: "No hook", none: "Nothing auto-allowed yet.", subagent: "subagent",
			recent: "Newest %d, newest first", footer: "Only Bash, read-only tools and subagents in bypassPermissions / always-proceed sessions are allowed. Deleting and overwriting go through only inside temporary and work directories, unless \"Auto-allow deleting and overwriting too\" is on. Force pushes, permission changes and your own ask and deny rules still ask. Commands are cut and secrets masked.",
			rpcTimeout: "Request timed out", rpcOffline: "Computer is offline or extension program is not running", rpcError: "Node returned an error",
			dirNotAbs: "Path must be absolute", dirNoHome: "Cannot determine home directory", dirMissing: "Directory does not exist",
			dirBadJSON: "Settings file is not a valid JSON object (left unchanged)", dirBadHooks: "Settings file has an invalid hooks structure (left unchanged)",
			dirPerm: "Permission denied accessing settings file", dirReadErr: "Cannot read settings file", dirWriteErr: "Cannot write settings file",
			dirError: "Could not install hook in this directory",
		}
	case strings.HasPrefix(locale, "ja"):
		return text{
			title: "自動許可の記録", noMachines: "オンのコンピュータがありません",
			noMachinesHint: "この拡張の設定でコンピュータをオンにすると、その Claude Code と Antigravity CLI に hook が入ります。",
			unreachable:    "このコンピュータのプログラムに接続できません", offline: "接続できません",
			installed: "hook あり", notInstalled: "hook なし", none: "まだ自動許可はありません。", subagent: "サブエージェント",
			recent: "新しい順に %d 件", footer: "許可するのは bypassPermissions / always-proceed のセッションの Bash、読み取り専用ツール、サブエージェントだけです。削除・上書きは一時ディレクトリと作業ディレクトリの中だけ許可します（設定の「削除と上書きも自動で許可」をオンにすると全部）。force push・権限の変更と、自分で設定した ask / deny ルールは引き続き確認されます。コマンドは切り詰め、秘密情報は伏せています。",
			rpcTimeout: "応答がタイムアウトしました", rpcOffline: "コンピュータがオフライン、またはプログラムが起動していません", rpcError: "ノードからエラーが返されました",
			dirNotAbs: "絶対パスを指定してください", dirNoHome: "ホームディレクトリを取得できません", dirMissing: "ディレクトリが存在しません",
			dirBadJSON: "設定ファイルが有効な JSON オブジェクトではありません（変更していません）", dirBadHooks: "設定ファイルの hooks 構造が無効です（変更していません）",
			dirPerm: "設定ファイルへのアクセス権限がありません", dirReadErr: "設定ファイルを読み込めません", dirWriteErr: "設定ファイルを書き込めません",
			dirError: "このディレクトリに hook を設定できませんでした",
		}
	}
	return text{
		title: "自动允许记录", noMachines: "还没有打开任何电脑",
		noMachinesHint: "在这个扩展的设置里打开一台电脑，那台电脑的 Claude Code 和 Antigravity CLI 就会装上 hook。",
		unreachable:    "连不上这台电脑上的程序", offline: "连不上",
		installed: "已装 hook", notInstalled: "没有 hook", none: "还没有自动允许过。", subagent: "子 Agent",
		recent: "最近 %d 条，新的在前", footer: "只放行 bypassPermissions / always-proceed 会话里的 Bash、只读工具和子 Agent。删除、覆盖只在目标都在临时目录、工作目录里时放行（设置里打开「删除和覆盖也自动允许」就都放行）；force push、改权限，以及你自己配的 ask / deny 规则，照常问你。命令已截断、去掉了密钥。",
		rpcTimeout: "请求超时", rpcOffline: "电脑离线或扩展程序未运行", rpcError: "节点返回异常",
		dirNotAbs: "路径必须是绝对路径", dirNoHome: "无法获取家目录", dirMissing: "目录不存在",
		dirBadJSON: "配置文件不是有效的 JSON 对象（未修改）", dirBadHooks: "配置文件的 hooks 结构无效（未修改）",
		dirPerm: "没有读写配置文件的权限", dirReadErr: "无法读取配置文件", dirWriteErr: "无法写入配置文件",
		dirError: "无法在此目录安装 hook",
	}
}
