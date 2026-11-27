package main

import (
	"cmp"
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	pageUI = "disklessly"
	cardUI = "disklessly-card"

	homeTitle = "硬盘空间"
	// pageStateLost is what a page whose saved state cannot be read says.
	pageStateLost = "这一页的状态过期了，请关掉它重新打开。"

	// inlineChartMachines caps full per-machine chart+table blocks in fleetTab so a 64-machine fleet stays well under
	// the View schema's 300-component limit while keeping every machine's row, watermark bar and actions.
	inlineChartMachines = 12
	// maxShownScanItems caps tappable ScanItem rows in itemsTab to match the 100-row table and stay well within wire.ItemsMost.
	maxShownScanItems = 100
)

type page struct {
	Machine string `json:"m,omitempty"`
	Tab     string `json:"t,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Query   string `json:"q,omitempty"`
	// SessQuery is the cold-session search the old 会话与台账 tab kept; read, no longer drawn.
	SessQuery string `json:"sq,omitempty"`
	// Open is the part of the page whose 详情 is shown.
	Open string `json:"x,omitempty"`
}

const (
	tabFleet   = "fleet"
	tabItems   = "items"
	tabPlan    = "plan"
	tabHistory = "history"
	// tabSessions is the old 会话与台账 tab: it now opens 清理记录 (sessions live on AI 对话).
	tabSessions = "sessions"

	modeHome       = ""
	modeNewMachine = "new-machine"
	modeEditMach   = "edit-machine"
	// modeAdvice is the pushed page of every 帮我腾空间 step (read-only).
	modeAdvice = "advice"
)

type draw struct {
	st        *State
	now       time.Time
	loc       *time.Location
	uiVersion int
	deep      *deepResult // a deep search's hits for this render only
	// jobs are the background jobs by machine id, as of this render.
	jobs map[string]coldJob
	// open is the part of the page whose 详情 is shown.
	open string
	// leadItems is the component count of leading sections (e.g. sandbox banner) reserved against wire.ItemsMost.
	leadItems int
}

func (d draw) v2() bool { return d.uiVersion == 0 || d.uiVersion >= 2 }
func (d draw) v3() bool { return d.uiVersion == 0 || d.uiVersion >= 3 }
func (d draw) v4() bool { return d.uiVersion == 0 || d.uiVersion >= 4 }
func (d draw) v6() bool { return d.uiVersion == 0 || d.uiVersion >= 6 }
func (d draw) v7() bool { return d.uiVersion == 0 || d.uiVersion >= 7 }

func (d draw) fitVersion(v *wire.View) *wire.View {
	if v == nil || d.uiVersion == 0 {
		return v
	}
	if !d.v2() {
		v.RefreshAfter = 0
	}
	if !d.v3() {
		v.Search, v.Filter = nil, nil
	}
	if !d.v4() {
		v.Tabs, v.Layout = nil, ""
	}
	for si := range v.Sections {
		s := &v.Sections[si]
		if !d.v7() {
			s.Key = ""
		}
		for ii := range s.Items {
			it := &s.Items[ii]
			if !d.v6() {
				it.Actions = nil
				for k := range it.Series {
					it.Series[k].Color = ""
				}
			}
			if !d.v4() && it.Type == wire.ComponentCode {
				s.Items[ii] = wire.Component{
					Type:  wire.ComponentText,
					Key:   it.Key,
					Style: wire.TextStyleCode,
					Text:  clip(cmp.Or(strings.TrimSpace(it.Text), "—"), wire.StringMost),
				}
				it = &s.Items[ii]
			}
			if !d.v3() {
				it.Key = ""
				it.Icon = ""
				it.Avatar = nil
				it.Thumbnail = nil
			}
		}
	}
	return v
}

func (d draw) statComponents(key string, stats []wire.Stat) []wire.Component {
	if d.v2() {
		return []wire.Component{{Type: wire.ComponentStatGrid, Key: key, Stats: stats}}
	}
	out := make([]wire.Component, 0, len(stats))
	for _, st := range stats {
		out = append(out, wire.Component{
			Type:   wire.ComponentMetric,
			Label:  st.Label,
			Value:  st.Value,
			Detail: st.Detail,
			Tone:   st.Tone,
		})
	}
	return out
}

func (d draw) emptyComponents(icon, title, detail string, btn *wire.EmptyButton) []wire.Component {
	if d.v3() {
		return []wire.Component{{
			Type:   wire.ComponentEmpty,
			Icon:   icon,
			Title:  title,
			Detail: detail,
			Button: btn,
		}}
	}
	out := []wire.Component{{
		Type:  wire.ComponentText,
		Style: wire.TextStyleSecondary,
		Text:  strings.TrimSpace(title + "：" + detail),
	}}
	if btn != nil {
		act := btn.Action
		out = append(out, wire.Component{
			Type:   wire.ComponentButton,
			Title:  btn.Title,
			Action: &act,
		})
	}
	return out
}

func (a *app) render(_ context.Context, r *ext.UIRequest) (*wire.View, error) {
	var p page
	if err := r.DecodeState(&p); err != nil {
		return nil, &ext.Error{Code: "invalid", Message: pageStateLost}
	}
	return a.pageView(r, p, nil)
}

func (a *app) pageView(r *ext.UIRequest, p page, deep *deepResult) (*wire.View, error) {
	var v *wire.View
	err := a.db().read(func(st *State) {
		d := a.drawer(st, r)
		d.deep, d.open = deep, p.Open
		v = d.view(p)
	})
	return v, err
}

func (a *app) drawer(st *State, r *ext.UIRequest) draw {
	var tz string
	var uiVer int
	if r != nil {
		tz = r.TZ
		uiVer = r.UIVersion
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	return draw{st: st, now: a.now(), loc: loc, uiVersion: uiVer, jobs: a.jobSnapshot()}
}

func (d draw) view(p page) *wire.View {
	switch p.Mode {
	case modeNewMachine:
		return d.machineForm(p, nil)
	case modeEditMach:
		m := d.st.machine(p.Machine)
		if m == nil {
			return d.home(page{})
		}
		return d.machineForm(p, m)
	case modeAdvice:
		return d.adviceAllView(p)
	}
	return d.home(p)
}

func (d draw) home(p page) *wire.View {
	statuses := d.st.statuses()
	if len(statuses) == 0 {
		d.st.ensureDefaults(d.now)
		statuses = d.st.statuses()
	}

	selected := d.st.machine(p.Machine)
	if selected == nil || !selected.Enabled {
		selected = statuses[0].Machine
	}
	p.Machine = selected.ID
	if p.Tab == tabSessions {
		p.Tab = tabHistory
	}
	if p.Tab != tabFleet && p.Tab != tabItems && p.Tab != tabPlan && p.Tab != tabHistory {
		p.Tab = tabFleet
	}

	v := &wire.View{
		Title:    homeTitle,
		Fallback: d.fallbackSummary(statuses),
	}

	if d.v3() {
		const maxFilterChoices = 100
		const maxMachineChoices = maxFilterChoices - 1 // reserve 1 slot for "＋ 添加电脑"
		choices := make([]wire.Choice, 0, min(len(statuses), maxMachineChoices)+1)
		selectedIncluded := false
		for _, ms := range statuses {
			if len(choices) >= maxMachineChoices {
				break
			}
			choices = append(choices, wire.Choice{Value: ms.Machine.ID, Title: diskChipLabel(ms)})
			if ms.Machine.ID == selected.ID {
				selectedIncluded = true
			}
		}
		if !selectedIncluded && len(choices) > 0 {
			choices[len(choices)-1] = wire.Choice{Value: selected.ID, Title: diskChipLabel(d.st.machineStatus(selected))}
		}
		choices = append(choices, wire.Choice{Value: "+", Title: "＋ 添加电脑"})
		v.Filter = &wire.Filter{
			Name:    "machine",
			Title:   "电脑",
			Choices: choices,
			Value:   selected.ID,
			Action:  wire.Action{ID: "machine"},
		}
	}

	scan := d.st.Scans[selected.ID]
	itemCount := 0
	if scan != nil {
		itemCount = len(scan.Items)
	}
	planCount := 0
	if scan != nil {
		if pl, err := BuildCleanPlan(selected, scan, nil, nil, false); err == nil {
			planCount = len(pl.Items)
		}
	}

	if d.v4() {
		v.Tabs = &wire.Tabs{
			Name:  "tab",
			Value: p.Tab,
			Items: []wire.Tab{
				{Value: tabFleet, Title: "所有电脑", Badge: strconv.Itoa(len(statuses))},
				{Value: tabItems, Title: "能删什么", Badge: strconv.Itoa(itemCount)},
				{Value: tabPlan, Title: "先看看会删什么", Badge: strconv.Itoa(planCount)},
				{Value: tabHistory, Title: "清理记录", Badge: strconv.Itoa(len(d.st.History))},
			},
			Action: wire.Action{ID: "tab"},
		}
	}
	v.State = ext.EncodeState(p)

	var lead []wire.Section
	if d.v4() {
		lead = sandboxSection()
	} else if sandboxed() {
		sp := sandboxProblem()
		lead = []wire.Section{{
			Title: sp.Title,
			Items: []wire.Component{{Type: wire.ComponentText, Text: sp.message()}},
		}}
	}
	if p.Tab == tabFleet {
		lead = append(lead, homeIntro(statuses))
	}
	d.leadItems = sectionItemCount(lead)

	switch p.Tab {
	case tabItems:
		if d.v3() {
			v.Search = &wire.Search{Placeholder: "搜种类、路径或说明", Value: p.Query, Action: wire.Action{ID: "search"}}
		}
		v.Sections = d.itemsTab(p, selected, scan)
	case tabPlan:
		v.Sections = d.planTab(p, selected, scan)
	case tabHistory:
		v.Sections = d.historyTab()
	default:
		v.Sections = d.fleetTab(p, statuses, selected)
	}
	if d.v2() {
		v.RefreshAfter = d.refreshAfter()
	}
	if len(lead) > 0 {
		v.Sections = append(lead, v.Sections...)
	}
	return d.fitVersion(v)
}

// homeIntro is the first thing the 所有电脑 tab says: what the page is for, that looking deletes
// nothing, and where the AI conversations are.
func homeIntro(statuses []MachineStatus) wire.Section {
	text := "看看每台电脑的硬盘用了多少，把能重新生成的缓存和过期的旧日志删掉腾地方。「检查」只读取大小，不会删除任何东西；真要删的时候，会先告诉你删什么、再让你确认。一周没动的 AI 对话可以在「AI 对话」页搬到网盘。"
	unchecked := 0
	for _, ms := range statuses {
		if ms.Level == HealthUnscanned {
			unchecked++
		}
	}
	if unchecked == len(statuses) {
		text += "第一步：点下面的「" + scanAllText + "」。"
	}
	return wire.Section{Key: "intro", Items: []wire.Component{{Type: wire.ComponentText, Text: text}}}
}

// scanAllText is the button that checks every computer.
const scanAllText = "检查所有电脑（只看不删）"

// planText is the 硬盘空间 page's button (and tab) that lists what a cleanup would delete.
const planText = "先看看会删什么"

// fleetAlertSummary words how full the computers' disks are (a computer that cannot be reached
// now counts by its last check) and how many cannot be reached now, apart: offline is that count,
// for the stat's detail, so a computer that does not answer never reads as a full disk.
func fleetAlertSummary(statuses []MachineStatus) (text, tone string, offline int) {
	var warnCount, critCount, unscannedCount, offlineCount int
	for _, ms := range statuses {
		if ms.Level == HealthOffline {
			offline++
		}
		switch fullnessLevel(ms) {
		case HealthCritical:
			critCount++
		case HealthWarning:
			warnCount++
		case HealthUnscanned:
			unscannedCount++
		case HealthOffline:
			offlineCount++
		}
	}
	switch {
	case critCount > 0:
		return fmt.Sprintf("%d 台快满了", critCount), wire.ToneDanger, offline
	case warnCount > 0:
		return fmt.Sprintf("%d 台偏满", warnCount), wire.ToneCaution, offline
	case offlineCount > 0:
		// Never checked and not answering: nothing is known about how full they are.
		return fmt.Sprintf("%d 台还不知道", offlineCount), wire.ToneNeutral, offline
	case unscannedCount > 0:
		return fmt.Sprintf("%d 台还没检查", unscannedCount), wire.ToneNeutral, offline
	default:
		return "都还宽裕", wire.TonePositive, offline
	}
}

// fleetReclaim splits what the computers of statuses can free into what can be done now and
// what sits on computers that cannot be reached now (their last check's numbers).
type fleetReclaim struct {
	all, safe       uint64 // computers that answer (or were never tried) now
	offline         uint64 // computers that cannot be reached now, by their last check
	offlineMachines int
}

func fleetReclaimOf(statuses []MachineStatus) fleetReclaim {
	var r fleetReclaim
	for _, ms := range statuses {
		if ms.Level == HealthOffline {
			if ms.ReclaimableBytes > 0 {
				r.offline += ms.ReclaimableBytes
				r.offlineMachines++
			}
			continue
		}
		r.all += ms.ReclaimableBytes
		r.safe += ms.SafeBytes
	}
	return r
}

// offlineNote is the part about computers that cannot be reached now, or "".
func (r fleetReclaim) offlineNote() string {
	if r.offline == 0 {
		return ""
	}
	return fmt.Sprintf("另有 %s 在连不上的 %d 台电脑上，按上次检查的数", FormatBytes(r.offline), r.offlineMachines)
}

// statDetail is the 可以腾出 stat's detail: the 放心删 part of what can be done now, then the rest.
func (r fleetReclaim) statDetail() string {
	text := "其中放心删 " + FormatBytes(r.safe)
	if n := r.offlineNote(); n != "" {
		text += "；" + n
	}
	return text
}

// reclaimWords is one computer's 可以腾出 in a row: everything, and how much of it is 放心删 (what
// its 删掉放心删的缓存 button takes), so the two numbers never look like a mismatch.
func reclaimWords(all, safe uint64) string {
	switch {
	case all == 0 || safe >= all:
		return "可以腾出 " + FormatBytes(all)
	case FormatBytes(safe) == FormatBytes(all):
		return "可以腾出 " + FormatBytes(all) + "，几乎都是放心删的"
	}
	return "可以腾出 " + FormatBytes(all) + "，其中放心删 " + FormatBytes(safe)
}

// offlineNote is the stat detail's part about computers that cannot be reached now.
func offlineNote(offline int) string {
	if offline == 0 {
		return ""
	}
	return fmt.Sprintf(" · %d 台连不上", offline)
}

// ---- Tab 1: Fleet Disk Overview ----

func (d draw) fleetTab(_ page, statuses []MachineStatus, selected *Machine) []wire.Section {
	var totalFreed uint64
	reclaim := fleetReclaimOf(statuses)
	for _, h := range d.st.History {
		totalFreed += h.FreedBytes
	}
	alertText, alertTone, offline := fleetAlertSummary(statuses)

	topStats := []wire.Stat{
		{Label: "电脑", Value: fmt.Sprintf("%d 台", len(statuses)), Detail: "运行 Persona 的电脑和远程电脑"},
		{Label: "硬盘用了多少", Value: alertText, Detail: fullnessLegendShort + offlineNote(offline), Tone: alertTone},
		{Label: "可以腾出", Value: FormatBytes(reclaim.all), Detail: reclaim.statDetail(), Tone: toneIf(reclaim.all > 0, wire.TonePositive)},
		{Label: "已经腾出", Value: FormatBytes(totalFreed), Detail: fmt.Sprintf("共 %d 次清理", len(d.st.History))},
	}
	topItems := d.statComponents("fleet-summary", topStats)
	topItems = append(topItems, wire.Component{
		Type:   wire.ComponentButton,
		Title:  scanAllText,
		Action: &wire.Action{ID: "scan-all"},
	})
	// Charts and the breakdown are drawn for the selected computer only; say where the others' are.
	if len(statuses) > 1 && d.v2() {
		where := "想看别的电脑，点它那一行看它能删什么。"
		if d.v3() {
			where = "想看别的电脑，在顶部「电脑」里选它，或者点它那一行看它能删什么。"
		}
		topItems = append(topItems, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleCaption,
			Text: clip("图表和各类明细只画选中的「"+machineLabel(selected)+"」，其他电脑只写用了多少、能腾出多少；"+where, 300)})
	}

	// 帮我腾空间 comes right after the summary numbers, which stay first on a phone.
	adviceSec := d.adviceSection()
	secs := []wire.Section{{
		Title: "每台电脑的硬盘",
		Items: topItems,
	}, adviceSec}

	chartLimit := inlineChartMachines
	if len(statuses) > 32 {
		chartLimit = 8
	}

	// Reserve budget for lead (sandbox), top summary section, the 帮我腾空间 block and trailing manage section (2 items).
	fleetBudget := max(0, wire.ItemsMost-d.leadItems-len(topItems)-len(adviceSec.Items)-2)
	used := 0

	for idx, ms := range statuses {
		rem := len(statuses) - idx - 1
		if used >= fleetBudget {
			break
		}
		if used == fleetBudget-1 && rem > 0 {
			secs = append(secs, wire.Section{
				Items: []wire.Component{{
					Type:  wire.ComponentText,
					Style: wire.TextStyleSecondary,
					Text:  fmt.Sprintf("还有 %d 台电脑没列出来，在顶部「电脑」里选它查看。", len(statuses)-idx),
				}},
			})
			break
		}
		m := ms.Machine
		scan := d.st.Scans[m.ID]
		badge := healthBadge(ms)
		compactTail := len(statuses) > 28 && idx >= chartLimit
		// Only the selected computer gets its stats, charts and table; the others are one row
		// (name, state, how full, how much can be freed) with their bar and buttons.
		detailed := m.ID == selected.ID

		var subParts []string
		// Reachability comes first, in words, apart from how full the disk is (bar and badge).
		if lastKnown(ms) && ms.LastScannedAt != nil {
			subParts = append(subParts, d.offlineWords(m)+" · 下面是 "+d.when(*ms.LastScannedAt)+"检查时的数")
		} else if ms.Level == HealthOffline {
			subParts = append(subParts, d.offlineWords(m))
		}
		if !m.IsLocal {
			subParts = append(subParts, "远程电脑 "+m.Host)
		}
		if p := platformLabel(m.Platform); p != "" {
			subParts = append(subParts, p)
		}
		if ms.TotalBytes > 0 && !detailed {
			subParts = append(subParts, "还剩 "+FormatBytes(ms.AvailableBytes)+" · "+reclaimWords(ms.ReclaimableBytes, ms.SafeBytes))
		}
		if lastKnown(ms) {
			// The check's time is said with the reachability above.
		} else if ms.LastScannedAt != nil {
			subParts = append(subParts, d.when(*ms.LastScannedAt))
			if ms.Partial {
				subParts = append(subParts, "缓存太多没算完（硬盘用量是准的）")
			}
		} else {
			subParts = append(subParts, "还没检查")
		}

		makeRow := func() wire.Component {
			return wire.Component{
				Type:     wire.ComponentRow,
				Key:      boundedKey("mach-", m.ID),
				Icon:     machineIcon(ms.Level),
				Title:    clip(cmp.Or(strings.TrimSpace(machineLabel(m)), strings.TrimSpace(m.ID), "没起名的电脑"), 200),
				Subtitle: clip(strings.Join(subParts, " · "), 400),
				Badge:    badge,
				Action:   &wire.Action{ID: "select-machine", Values: map[string]string{"m": m.ID, "t": tabItems}},
			}
		}

		makeCompact := func() wire.Section {
			sec := wire.Section{
				Title: machineLabel(m),
				Items: []wire.Component{makeRow()},
			}
			if ms.TotalBytes > 0 {
				sec.Items = append(sec.Items, machineProgressComponent(ms))
			}
			footer := machinePlainTextSummary(ms, scan)
			if m.LastError != "" && ms.TotalBytes > 0 {
				footer = "上次检查没成功：" + sshProblem(m, m.LastError).Title + " · " + footer
			}
			if ms.CleanCount > 0 {
				footer += fmt.Sprintf(" · 一共清理 %d 次，腾出 %s", ms.CleanCount, FormatBytes(ms.TotalFreedBytes))
			}
			sec.Footer = clip(footer, 1800)
			if d.busy(m) {
				// Only the running job's line: no buttons while it runs.
				sec.Items = append(sec.Items, d.machineJobItems(m, retryHint{}, retryHint{}, nil)...)
				return sec
			}
			if ms.SafeBytes > 0 {
				sec.Items = append(sec.Items, wire.Component{
					Type:  wire.ComponentButton,
					Key:   boundedKey("btn-clean-", m.ID),
					Title: fmt.Sprintf("删掉放心删的缓存（%s）", FormatBytes(ms.SafeBytes)),
					Action: &wire.Action{
						ID:      "clean-safe",
						Values:  map[string]string{"m": m.ID},
						Confirm: safeCleanConfirm(m, ms.SafeBytes, scan),
					},
				})
			} else {
				sec.Items = append(sec.Items, wire.Component{
					Type:   wire.ComponentButton,
					Key:    boundedKey("btn-scan-", m.ID),
					Title:  d.checkButton(m),
					Action: &wire.Action{ID: "scan", Values: map[string]string{"m": m.ID}},
				})
			}
			return sec
		}

		var sec wire.Section
		if compactTail {
			sec = makeCompact()
		} else {
			sec = wire.Section{
				Title: machineLabel(m),
				Items: []wire.Component{makeRow()},
			}
			if ms.TotalBytes > 0 {
				mountPoint := "/"
				var bd categoryBreakdown
				if scan != nil {
					mountPoint = cmp.Or(scan.Mount.MountPoint, "/")
					bd = summarizeCategories(scan.Items, &scan.Sessions)
				}
				sec.Items = append(sec.Items, machineProgressComponent(ms))

				if !detailed {
					// The row's subtitle says how much is left and can be freed.
				} else if d.v2() && idx < chartLimit {
					afterSafe := min(ms.TotalBytes, ms.AvailableBytes+ms.SafeBytes)
					sec.Items = append(sec.Items, d.statComponents(boundedKey("fleet-stats-", m.ID), []wire.Stat{
						{Label: "还剩", Value: FormatBytes(ms.AvailableBytes), Detail: "共 " + FormatBytes(ms.TotalBytes)},
						{Label: "可以腾出", Value: FormatBytes(ms.ReclaimableBytes), Detail: "其中放心删 " + FormatBytes(ms.SafeBytes), Tone: toneIf(ms.ReclaimableBytes > 0, wire.TonePositive)},
						{Label: "删完还剩", Value: FormatBytes(afterSafe), Detail: "删掉放心删的以后", Tone: toneIf(ms.SafeBytes > 0, wire.TonePositive)},
						{Label: "一周没动的 AI 对话", Value: fmt.Sprintf("%d 个", ms.Sessions.ColdCount), Detail: FormatBytes(ms.Sessions.ColdBytes) + " · 最近一周用过 " + strconv.Itoa(ms.Sessions.HotCount) + " 个", Tone: toneIf(ms.Sessions.ColdBytes > 0, wire.ToneCaution)},
					})...)

					// The bar above is this computer's system disk; the table's caption has its numbers.
					if catChart, ok := reclaimCategoryChart(m, bd, "fleet"); ok {
						sec.Items = append(sec.Items, catChart)
					} else {
						sec.Items = append(sec.Items, wire.Component{
							Type:  wire.ComponentText,
							Style: wire.TextStyleSecondary,
							Text:  "没找到能删的缓存，也没有一周没动的 AI 对话。",
						})
					}
					if d.v3() {
						sec.Items = append(sec.Items, machineBreakdownTable(ms, mountPoint, bd, "fleet", bd.totalShown == 0))
					} else {
						sec.Items = append(sec.Items, wire.Component{
							Type:  wire.ComponentText,
							Style: wire.TextStyleSecondary,
							Text:  machinePlainTextSummary(ms, scan),
						})
					}
				} else {
					sec.Items = append(sec.Items, wire.Component{
						Type:  wire.ComponentText,
						Style: wire.TextStyleSecondary,
						Text:  machinePlainTextSummary(ms, scan),
					})
				}
			} else if m.LastError != "" {
				// The problem is drawn with the job lines below.
			} else if j, ok := d.jobs[m.ID]; ok && j.running() {
				// The 检查 button is hidden while a job runs.
				text := "还没检查过；正在做的事完成后，可以检查这台电脑硬盘用了多少、哪些缓存能删。"
				if j.Kind == jobScan {
					text = "正在检查这台电脑硬盘用了多少、哪些缓存能删：只读取大小，不会删除任何东西。"
				}
				sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: text})
			} else {
				sec.Items = append(sec.Items, wire.Component{
					Type:  wire.ComponentText,
					Style: wire.TextStyleSecondary,
					Text:  "还没检查过。点「检查 " + machineLabel(m) + "」看看硬盘用了多少、哪些缓存能删：检查只读取大小，不会删除任何东西。",
				})
			}

			if ms.CleanCount > 0 {
				sec.Footer = fmt.Sprintf("上次腾出 %s · 一共清理 %d 次，腾出 %s",
					FormatBytes(ms.LastFreedBytes), ms.CleanCount, FormatBytes(ms.TotalFreedBytes))
			}

			// The outcome lines, then the buttons they name (only while nothing runs), then a
			// failed check's command.
			var buttons []wire.Component
			if !d.busy(m) {
				buttons = append(buttons,
					wire.Component{
						Type:   wire.ComponentButton,
						Key:    boundedKey("btn-scan-", m.ID),
						Title:  d.checkButton(m),
						Action: &wire.Action{ID: "scan", Values: map[string]string{"m": m.ID}},
					},
					wire.Component{
						Type:   wire.ComponentButton,
						Key:    boundedKey("btn-plan-", m.ID),
						Title:  planText,
						Action: &wire.Action{ID: "open-plan", Values: map[string]string{"m": m.ID}},
					},
				)
				if ms.SafeBytes > 0 {
					buttons = append(buttons, wire.Component{
						Type:  wire.ComponentButton,
						Key:   boundedKey("btn-clean-", m.ID),
						Title: fmt.Sprintf("删掉放心删的缓存（%s）", FormatBytes(ms.SafeBytes)),
						Action: &wire.Action{
							ID:      "clean-safe",
							Values:  map[string]string{"m": m.ID},
							Confirm: safeCleanConfirm(m, ms.SafeBytes, scan),
						},
					})
				}
			}
			sec.Items = append(sec.Items, d.machineJobItems(m, retryHint{button: d.checkButton(m)}, retryHint{button: planText}, buttons)...)
			// Fall back to the compact card when expanding this machine would starve remaining machines past wire.ItemsMost.
			if used+len(sec.Items)+rem*3 > fleetBudget {
				sec = makeCompact()
			}
		}

		// Ultimate guard for abnormally large fleets (> ~95 machines): collapse to a single row so every machine fits.
		if used+len(sec.Items)+rem > fleetBudget && len(sec.Items) > 1 {
			sec.Items = sec.Items[:1]
		}
		used += len(sec.Items)
		secs = append(secs, sec)
	}

	manage := wire.Section{
		Title:  "添加或修改电脑",
		Footer: "这里会自动列出 Persona 里登记的电脑和 ~/.ssh/config 里写好的电脑；也可以自己添加一台能远程登录（SSH）的电脑。",
		Items: []wire.Component{
			{
				Type:     wire.ComponentRow,
				Icon:     "settings",
				Title:    "修改「" + machineLabel(selected) + "」的设置",
				Subtitle: cmp.Or(selected.Host, "localhost") + " · " + sourceLabel(selected.Source),
				Action:   &wire.Action{ID: "edit-machine", Values: map[string]string{"m": selected.ID}},
			},
			{
				Type:   wire.ComponentButton,
				Title:  "添加一台电脑",
				Action: &wire.Action{ID: "new-machine"},
			},
		},
	}
	secs = append(secs, manage)
	return secs
}

// ---- Tab 2: Reclaimable Categories & Items Table ----

func (d draw) itemsTab(p page, m *Machine, scan *Scan) []wire.Section {
	ms := d.st.machineStatus(m)
	head := d.machineHeaderOrText(ms, scan)
	rescan := retryHint{button: d.checkButton(m)}
	top := wire.Section{Items: []wire.Component{head}}
	if scan != nil && scan.Mount.TotalBytes > 0 {
		top.Items = append(top.Items, machineProgressComponent(ms))
	}
	// The check button right after the line that names it; a computer never checked (and not
	// found unreachable) has it in the empty state below instead.
	var buttons []wire.Component
	if !d.busy(m) && (scan != nil || m.LastError != "") {
		buttons = []wire.Component{{
			Type:   wire.ComponentButton,
			Title:  rescan.button,
			Action: &wire.Action{ID: "scan", Values: map[string]string{"m": m.ID}},
		}}
	}
	top.Items = append(top.Items, d.machineJobItems(m, rescan, rescan, buttons)...)
	if scan == nil {
		var empty *wire.EmptyButton
		if len(buttons) == 0 {
			empty = &wire.EmptyButton{
				Title:  d.checkButton(m),
				Action: wire.Action{ID: "scan", Values: map[string]string{"m": m.ID}},
			}
		}
		return []wire.Section{
			top,
			{
				Items: d.emptyComponents(
					"gauge",
					machineLabel(m)+" 还没检查",
					"检查只读取大小，不会删除任何东西：看看系统盘用了多少，Go、Node/Python、Docker、Rust/Xcode 这些编译和下载缓存、系统日志，还有一周没动的 AI 对话各占多少。",
					empty,
				),
			},
		}
	}

	q := strings.ToLower(strings.TrimSpace(p.Query))
	var filtered []ScanItem
	for _, it := range scan.Items {
		if q != "" {
			hay := strings.ToLower(it.Title + " " + itemTitle(it) + " " + itemReason(it) + " " + it.Path + " " + it.Category + " " + CategoryLabels[it.Category] + " " + categoryName(it.Category) + " " + it.Reason)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		filtered = append(filtered, it)
	}

	var sessForChart *SessionSummary
	if q == "" {
		sessForChart = &scan.Sessions
	}
	bd := summarizeCategories(filtered, sessForChart)

	if d.v2() && scan.Mount.TotalBytes > 0 {
		chartSec := wire.Section{
			Title: "能删的东西",
		}
		// The bar at the top is the system disk; the chart shows the kinds only.
		if catChart, ok := reclaimCategoryChart(m, bd, "items"); ok {
			chartSec.Items = append(chartSec.Items, catChart)
		} else {
			chartSec.Items = append(chartSec.Items, wire.Component{
				Type:  wire.ComponentText,
				Style: wire.TextStyleSecondary,
				Text:  "没找到能删的缓存，也没有一周没动的 AI 对话。",
			})
		}
		if len(filtered) == 0 {
			catSummarySec := wire.Section{Title: "能删的东西，按种类"}
			if d.v3() {
				catSummarySec.Items = append(catSummarySec.Items, machineBreakdownTable(ms, scan.Mount.MountPoint, bd, "items", bd.totalShown == 0))
			} else {
				catSummarySec.Items = append(catSummarySec.Items, wire.Component{
					Type:  wire.ComponentText,
					Style: wire.TextStyleSecondary,
					Text:  machinePlainTextSummary(ms, scan),
				})
			}
			return []wire.Section{
				top,
				chartSec,
				catSummarySec,
				{
					Items: d.emptyComponents("check", "没找到能删的东西", "这台电脑很干净，或者搜索没有匹配到。", nil),
				},
			}
		}
		return append([]wire.Section{top, chartSec}, d.itemsBreakdownSections(m, ms, scan, filtered, bd)...)
	}

	if len(filtered) == 0 {
		return []wire.Section{
			top,
			{
				Items: d.emptyComponents("check", "没找到能删的东西", "这台电脑很干净，或者搜索没有匹配到。", nil),
			},
		}
	}
	return append([]wire.Section{top}, d.itemsBreakdownSections(m, ms, scan, filtered, bd)...)
}

func (d draw) itemsBreakdownSections(m *Machine, ms MachineStatus, scan *Scan, filtered []ScanItem, bd categoryBreakdown) []wire.Section {
	shownCat := map[string]bool{}
	for _, it := range filtered {
		shownCat[it.Category] = true
	}

	catSec := wire.Section{
		Title: "能删的东西，按种类（点一行就删这一类）",
	}
	// The category chart above (drawn under these conditions) spells the short names out; the
	// tables below use the same names and say them again only without it.
	legend := !(d.v2() && scan.Mount.TotalBytes > 0 && bd.totalShown > 0)
	if d.v3() {
		catSec.Items = append(catSec.Items, machineBreakdownTable(ms, scan.Mount.MountPoint, bd, "items", legend))
	} else {
		catSec.Items = append(catSec.Items, wire.Component{
			Type:  wire.ComponentText,
			Style: wire.TextStyleSecondary,
			Text:  machinePlainTextSummary(ms, scan),
		})
	}

	// Per-category quick clean rows. Their numbers come from the very plan the tap runs (the
	// whole kind, whatever the search shows; in-use and empty items left out), so the row and its
	// confirmation count exactly what gets deleted.
	for _, cid := range CategoryIDs {
		if !shownCat[cid] {
			continue
		}
		plan := categoryCleanPlan(m, scan, cid)
		if plan == nil || plan.ExpectedFree == 0 {
			continue
		}
		sub := fmt.Sprintf("%d 项 · 大约腾出 %s", len(plan.Items), FormatBytes(plan.ExpectedFree))
		if k := len(reviewItems(plan.Items)); k > 0 {
			sub = fmt.Sprintf("%d 项 · 其中 %d 项建议先看 · 大约腾出 %s", len(plan.Items), k, FormatBytes(plan.ExpectedFree))
		}
		catSec.Items = append(catSec.Items, wire.Component{
			Type:     wire.ComponentRow,
			Key:      "cat-clean-" + cid,
			Icon:     "folder",
			Title:    "删掉：" + categoryName(cid),
			Subtitle: sub,
			Badge:    &wire.Badge{Text: FormatBytes(plan.ExpectedFree), Tone: wire.TonePositive},
			Action: &wire.Action{
				ID:      "clean-category",
				Values:  map[string]string{"m": m.ID, "cat": cid},
				Confirm: categoryConfirm(m, scan, cid, plan),
			},
		})
	}

	// Detailed items table (4 phone-friendly columns; full path stays in tappable rows below)
	detailSec := wire.Section{
		Title:  "每一项（从大到小）",
		Footer: "这些绝不碰：正在运行的服务和容器、项目里还没提交的改动、最近 7 天用过的 AI 对话、密码和密钥。",
	}
	shownItems := filtered[:min(len(filtered), maxShownScanItems)]
	if d.v3() {
		itemRows := make([][]string, 0, len(shownItems))
		var kinds []string
		for _, it := range shownItems {
			kinds = append(kinds, it.Category)
			safetyTag := itemSafetyTag(it)
			itemRows = append(itemRows, []string{
				clipCellWidth(itemTitle(it), 12.5, 82.5, 13),
				CategoryAxisLabels[it.Category],
				safetyTag,
				FormatBytes(it.Bytes),
			})
		}
		detailSec.Items = append(detailSec.Items, wire.Component{
			Type:    wire.ComponentTable,
			Key:     boundedKey("items-detail-tbl-", m.ID),
			Caption: legendIf(legend, kinds),
			Columns: []wire.TableColumn{
				{Title: "项目"},
				{Title: "种类"},
				{Title: "能删吗"},
				{Title: "大小", Align: "end"},
			},
			Rows: itemRows,
		})
	}

	seenItemKeys := map[string]bool{}
	for idx, it := range shownItems {
		bTone := wire.TonePositive
		if it.Safety == SafetyReview {
			bTone = wire.ToneCaution
		}
		bText := itemSafetyLabel(it) + " · " + FormatBytes(it.Bytes)
		actVerb := "删掉"
		if it.Action == "archive" {
			actVerb = "搬到网盘"
		}
		key := boundedKey("item-", it.ID)
		for n := idx; seenItemKeys[key]; n++ {
			key = boundedKey(fmt.Sprintf("item-%d-", n), it.ID)
		}
		seenItemKeys[key] = true
		detailSec.Items = append(detailSec.Items, wire.Component{
			Type:     wire.ComponentRow,
			Key:      key,
			Title:    clip(cmp.Or(itemTitle(it), strings.TrimSpace(it.ID), "没有名字的一项"), 200),
			Subtitle: clip(fmt.Sprintf("%s · %s", clip(it.Path, 48), itemReason(it)), 400),
			Badge:    &wire.Badge{Text: bText, Tone: bTone},
			Action: &wire.Action{
				ID:      "clean-item",
				Values:  map[string]string{"m": m.ID, "item": it.ID},
				Confirm: itemConfirm(m, it, actVerb),
			},
		})
	}
	if len(filtered) > len(shownItems) {
		detailSec.Footer = fmt.Sprintf("这里只列出最大的 %d 项（共 %d 项），可以用搜索缩小范围。%s", len(shownItems), len(filtered), detailSec.Footer)
	}

	return []wire.Section{catSec, detailSec}
}

// ---- Tab 3: 先看看会删什么 (dry-run plan) & safe clean ----

func (d draw) planTab(_ page, m *Machine, scan *Scan) []wire.Section {
	ms := d.st.machineStatus(m)
	head := d.machineHeaderOrText(ms, scan)
	if scan == nil {
		return []wire.Section{
			{Items: []wire.Component{head}},
			{
				Items: d.emptyComponents(
					"list",
					"还不知道会删什么",
					"先检查这台电脑，才能列出会删什么。检查只读取大小，不会删除任何东西。",
					&wire.EmptyButton{
						Title:  "检查并列出会删什么",
						Action: wire.Action{ID: "scan", Values: map[string]string{"m": m.ID}},
					},
				),
			},
		}
	}

	plan, _ := BuildCleanPlan(m, scan, nil, nil, true)
	afterSafeAvail := scan.Mount.AvailableBytes + scan.SafeBytes
	if scan.Mount.TotalBytes > 0 {
		afterSafeAvail = min(scan.Mount.TotalBytes, afterSafeAvail)
	}
	var secs []wire.Section

	// This tab draws no check button; its clean button is the one below the summary.
	cleanAllTitle := fmt.Sprintf("删掉下面所有放心删的（+%s）", FormatBytes(scan.SafeBytes))
	rescan := retryHint{how: "想再检查一次：回到所有电脑那一栏，点这台电脑的检查按钮。"}
	reclean := retryHint{how: "想再删一次：用下面的删除按钮。"}
	if scan.SafeBytes > 0 {
		reclean = retryHint{button: cleanAllTitle}
	}
	summarySec := wire.Section{
		Title:  "先看看会删什么（不会真删）",
		Footer: "「放心删」是能重新生成或重新下载的编译和下载缓存，以及系统日志里 7 天以前的部分、系统换下来的旧日志、临时文件夹里编译和安装剩下的文件（最近的也删）和临时文件夹里放了 7 天以上的文件（日志和临时文件删了就没有了，最近 7 天的系统日志会留着）；不包括 AI 对话。「建议先看」是比较大的东西（比如自动化测试用的浏览器），删了以后下次用到时会重新下载。",
		Items:  append([]wire.Component{head}, d.machineJobItems(m, rescan, reclean, nil)...),
	}
	summarySec.Items = append(summarySec.Items, d.statComponents(boundedKey("plan-stats-", m.ID), []wire.Stat{
		{Label: "还剩", Value: FormatBytes(scan.Mount.AvailableBytes), Detail: "系统盘 " + scan.Mount.MountPoint},
		{Label: "放心删能腾出", Value: FormatBytes(scan.SafeBytes), Tone: wire.TonePositive, Detail: "缓存和过期的旧日志"},
		{Label: "删完还剩", Value: FormatBytes(afterSafeAvail), Tone: wire.TonePositive, Detail: fmt.Sprintf("%s → %s", FormatBytes(scan.Mount.AvailableBytes), FormatBytes(afterSafeAvail))},
		{Label: "建议先看", Value: FormatBytes(scan.ReviewBytes), Detail: "默认不删", Tone: toneIf(scan.ReviewBytes > 0, wire.ToneCaution)},
	})...)

	if d.v2() {
		if impact, ok := cleanImpactChart(ms, scan); ok {
			summarySec.Items = append(summarySec.Items, impact)
		} else if scan.Mount.TotalBytes > 0 {
			summarySec.Items = append(summarySec.Items, rootUsageMeter(ms, "plan"))
		}
	}

	if scan.SafeBytes > 0 {
		summarySec.Items = append(summarySec.Items, wire.Component{
			Type:  wire.ComponentButton,
			Title: cleanAllTitle,
			Action: &wire.Action{
				ID:      "clean-safe",
				Values:  map[string]string{"m": m.ID},
				Confirm: fmt.Sprintf("确定在「%s」上删掉下面列出的所有「放心删」的项目吗？剩余空间大约从 %s 变成 %s（腾出 +%s）。%s「建议先看」的和 AI 对话都不删。", machineLabel(m), FormatBytes(scan.Mount.AvailableBytes), FormatBytes(afterSafeAvail), FormatBytes(scan.SafeBytes), safeLogsNote(scan)),
			},
		})
	}

	secs = append(secs, summarySec)

	// Interactive category checklist form for customized clean execution
	formSec := wire.Section{
		Title:  "自己勾选要删的种类",
		Footer: "打开要删的种类，再点「按勾选的种类删除」。删之前和删之后都会量一下剩余空间，实际腾出多少记在「清理记录」里。",
	}
	catTotals := map[string]uint64{}
	catSafe := map[string]uint64{}
	for _, it := range scan.Items {
		if !it.InUse {
			catTotals[it.Category] += it.Bytes
			if it.Category != CatColdAgentSession && it.Safety == SafetySafe {
				catSafe[it.Category] += it.Bytes
			}
		}
	}
	hasAnyToggle := false
	for _, cid := range CategoryIDs {
		tot := catTotals[cid]
		if tot == 0 {
			continue
		}
		hasAnyToggle = true
		defaultVal := "true"
		if cid == CatColdAgentSession || catSafe[cid] == 0 {
			defaultVal = "false"
		}
		formSec.Items = append(formSec.Items, wire.Component{
			Type:  wire.ComponentField,
			Name:  "cat_" + cid,
			Label: fmt.Sprintf("%s (%s)", categoryName(cid), FormatBytes(tot)),
			Kind:  wire.FieldKindToggle,
			Value: defaultVal,
		})
	}
	formSec.Items = append(formSec.Items, wire.Component{
		Type:  wire.ComponentField,
		Name:  "include_review",
		Label: "也删「建议先看」的（比如 Playwright 测试用的浏览器）",
		Kind:  wire.FieldKindToggle,
		Value: "false",
	})
	if hasAnyToggle {
		formSec.Items = append(formSec.Items, wire.Component{
			Type:  wire.ComponentButton,
			Title: "按勾选的种类删除",
			Action: &wire.Action{
				ID:      "clean-selected",
				Submit:  true,
				Values:  map[string]string{"m": m.ID},
				Confirm: fmt.Sprintf("确定在「%s」上删掉勾选的种类吗？勾了「%s」的话，那些对话会先放进网盘、读回核对无误才删电脑上的那份。删之前和之后会量一下剩余空间，记到清理记录里。", machineLabel(m), categoryName(CatColdAgentSession)),
			},
		})
		secs = append(secs, formSec)
	}

	if plan != nil && len(plan.Items) > 0 {
		shownPlan := plan.Items[:min(len(plan.Items), 100)]
		if d.v3() {
			rows := make([][]string, 0, len(shownPlan))
			var kinds []string
			for _, it := range shownPlan {
				kinds = append(kinds, it.Category)
				rows = append(rows, []string{
					clipCellWidth(itemTitle(it), 12.5, 82.5, 13),
					CategoryAxisLabels[it.Category],
					safetyTag(it.Safety),
					FormatBytes(it.Bytes),
				})
			}
			secs = append(secs, wire.Section{
				Title: "会删的每一项（现在还没删）",
				Items: []wire.Component{
					{
						Type:    wire.ComponentTable,
						Key:     boundedKey("plan-tbl-", m.ID),
						Caption: categoryLegend(CategoryAxisLabels, kinds),
						Columns: []wire.TableColumn{
							{Title: "项目"},
							{Title: "种类"},
							{Title: "能删吗"},
							{Title: "大约腾出", Align: "end"},
						},
						Rows: rows,
					},
				},
			})
		} else {
			planItems := make([]wire.Component, 0, len(shownPlan))
			for _, it := range shownPlan {
				planItems = append(planItems, wire.Component{
					Type:     wire.ComponentRow,
					Title:    clip(cmp.Or(itemTitle(it), strings.TrimSpace(it.ID), "没有名字的一项"), 200),
					Subtitle: categoryName(it.Category) + " · " + safetyLabel(it.Safety),
					Value:    FormatBytes(it.Bytes),
				})
			}
			secs = append(secs, wire.Section{
				Title: "会删的每一项（现在还没删）",
				Items: planItems,
			})
		}
	}

	return secs
}

// ---- Tab 4: 清理记录 (cleanup history) ----

// historyTab lists past cleanups. AI conversations are on the AI 对话 page only: the tab links
// there instead of drawing their state a second time.
func (d draw) historyTab() []wire.Section {
	secs := []wire.Section{{Key: "to-sessions", Items: []wire.Component{{
		Type:     wire.ComponentRow,
		Key:      "open-sessions",
		Icon:     "chat",
		Title:    "AI 对话在「" + sessionsTitle + "」页",
		Subtitle: "哪些旧对话能搬到网盘、搬过去的在哪、怎么拿回来，都在那一页。",
		Action:   &wire.Action{ID: "open-sessions", Push: &wire.Push{UI: sessionsUI, Title: sessionsTitle}},
	}}}}

	histSec := wire.Section{
		Title:  "清理记录",
		Footer: "每次清理前后都会量一下剩余空间（比如 5.4 GiB → 18.0 GiB，腾出 12.6 GiB）。",
	}
	if len(d.st.History) == 0 {
		histSec.Items = d.emptyComponents(
			"clock",
			"还没有清理记录",
			"在「所有电脑」或「先看看会删什么」里删过东西以后，每次实际腾出多少都会记在这里。",
			nil,
		)
	} else {
		if d.v3() {
			var hRows [][]string
			for _, h := range d.st.History[:min(len(d.st.History), 50)] {
				hRows = append(hRows, []string{
					d.stamp(h.CreatedAt),
					histMachineCell(d.st, h),
					compactAvailChange(h),
					compactFreedCell(h.FreedBytes),
				})
			}
			histSec.Items = append(histSec.Items, wire.Component{
				Type: wire.ComponentTable,
				Key:  "hist-tbl",
				Columns: []wire.TableColumn{
					{Title: "时间"},
					{Title: "电脑"},
					{Title: "剩余空间"},
					{Title: "腾出", Align: "end"},
				},
				Rows: hRows,
			})
		}
		seenHistKeys := map[string]bool{}
		for idx, h := range d.st.History[:min(len(d.st.History), 20)] {
			key := boundedKey("hist-row-", h.ID)
			for n := idx; seenHistKeys[key]; n++ {
				key = boundedKey(fmt.Sprintf("hist-row-%d-", n), h.ID)
			}
			seenHistKeys[key] = true
			histSec.Items = append(histSec.Items, wire.Component{
				Type:     wire.ComponentRow,
				Key:      key,
				Icon:     "check",
				Title:    clip(fmt.Sprintf("%s · %s", histMachineLabel(d.st, h), h.Summary), 200),
				Subtitle: clip(fmt.Sprintf("%s · 处理了 %d 项（%s）", d.when(h.CreatedAt), len(h.Items), categoryNames(h.Categories)), 400),
				Badge:    &wire.Badge{Text: "+" + FormatBytes(h.FreedBytes), Tone: wire.TonePositive},
			})
		}
	}
	return append(secs, histSec)
}

// categoryNames words a record's category IDs for people.
func categoryNames(ids []string) string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, categoryName(id))
	}
	return strings.Join(names, "、")
}

// ---- Machine Form (Add / Edit SSH Machine) ----

func (d draw) machineForm(p page, m *Machine) *wire.View {
	title := "添加一台电脑"
	hostVal, nameVal, userVal, portVal, idFileVal := "", "", "", "", ""
	intro := "Disklessly 通过远程登录（SSH）连到别的电脑，只读取硬盘用了多少；要删东西之前都会先问你。运行 Persona 的电脑需要能用 ssh 不输密码直接登录它（一般已经写在 ~/.ssh/config 里）。"
	if m != nil {
		title = "修改电脑设置 · " + machineLabel(m)
		intro = "改的只是 Disklessly 怎么称呼、怎么连这台电脑，不会动这台电脑上的任何文件。"
		hostVal = m.Host
		nameVal = m.Name
		userVal = m.User
		if m.Port > 0 {
			portVal = strconv.Itoa(m.Port)
		}
		idFileVal = m.IdentityFile
	}

	items := []wire.Component{{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: intro}}
	if m == nil || !m.IsLocal {
		items = append(items, wire.Component{
			Type:        wire.ComponentField,
			Name:        "host",
			Label:       "怎么连到它（必填）：~/.ssh/config 里的名字或电脑地址",
			Kind:        wire.FieldKindText,
			Value:       hostVal,
			Placeholder: "例如 office-pc，或 dev.example.com",
		})
	}
	items = append(items,
		wire.Component{
			Type:        wire.ComponentField,
			Name:        "name",
			Label:       "在这里显示的名字",
			Kind:        wire.FieldKindText,
			Value:       nameVal,
			Placeholder: "例如 书房的台式机、公司开发机",
		},
	)
	if m == nil || !m.IsLocal {
		items = append(items,
			wire.Component{
				Type:        wire.ComponentField,
				Name:        "user",
				Label:       "登录用户名（可以不填，默认用 ~/.ssh/config 里的）",
				Kind:        wire.FieldKindText,
				Value:       userVal,
				Placeholder: "例如 ubuntu",
			},
			wire.Component{
				Type:        wire.ComponentField,
				Name:        "port",
				Label:       "端口（可以不填，默认 22）",
				Kind:        wire.FieldKindNumber,
				Value:       portVal,
				Placeholder: "22",
			},
			wire.Component{
				Type:        wire.ComponentField,
				Name:        "identity_file",
				Label:       "登录用的密钥文件（可以不填，默认用 ~/.ssh/config 里的）",
				Kind:        wire.FieldKindText,
				Value:       idFileVal,
				Placeholder: "~/.ssh/id_ed25519",
			},
		)
	}
	items = append(items,
		wire.Component{
			Type:   wire.ComponentButton,
			Title:  "保存",
			Action: &wire.Action{ID: "save-machine", Submit: true},
		},
		wire.Component{
			Type:   wire.ComponentButton,
			Title:  "取消",
			Action: &wire.Action{ID: "cancel"},
		},
	)
	if m != nil && !m.IsLocal {
		items = append(items, wire.Component{
			Type:  wire.ComponentButton,
			Title: "不再管这台电脑",
			Action: &wire.Action{
				ID:      "delete-machine",
				Values:  map[string]string{"m": m.ID},
				Confirm: fmt.Sprintf("从 Disklessly 里去掉「%s」？只是这里不再列出它，不会动它上面的任何文件。", machineLabel(m)),
			},
		})
	}
	return d.fitVersion(&wire.View{
		Title:    title,
		Fallback: title,
		State:    ext.EncodeState(p),
		Sections: []wire.Section{{Items: items}},
	})
}

// ---- Overview Card ----

func (a *app) card(_ context.Context, r *ext.UIRequest) (*wire.View, error) {
	var out *wire.View
	err := a.db().read(func(st *State) {
		d := a.drawer(st, r)
		statuses := st.statuses()
		fallback := d.fallbackSummary(statuses)
		var items []wire.Component
		if len(statuses) > 0 {
			reclaim := fleetReclaimOf(statuses)
			var featured *MachineStatus
			for i := range statuses {
				ms := &statuses[i]
				if ms.TotalBytes > 0 && (featured == nil || watermarkChartRank(ms.Level) < watermarkChartRank(featured.Level) ||
					(watermarkChartRank(ms.Level) == watermarkChartRank(featured.Level) && ms.UsedPercent > featured.UsedPercent)) {
					featured = ms
				}
			}

			alertText, alertTone, offline := fleetAlertSummary(statuses)
			if d.v2() {
				items = append(items, wire.Component{
					Type: wire.ComponentStatGrid,
					Key:  "card-stats",
					Stats: []wire.Stat{
						{Label: "电脑", Value: fmt.Sprintf("%d 台", len(statuses))},
						{Label: "硬盘", Value: alertText, Detail: strings.TrimPrefix(offlineNote(offline), " · "), Tone: alertTone},
						{Label: "可以腾出", Value: FormatBytes(reclaim.all), Detail: reclaim.offlineNote(), Tone: toneIf(reclaim.all > 0, wire.TonePositive)},
					},
				})
				if featured != nil {
					ch := rootUsageMeter(*featured, "card")
					ch.Label = machineLabel(featured.Machine) + " · " + ch.Label
					items = append(items, ch)
				}
			} else {
				items = append(items, wire.Component{
					Type:   wire.ComponentMetric,
					Label:  "硬盘用量",
					Value:  alertText,
					Detail: fmt.Sprintf("%d 台电脑 · 可以腾出 %s", len(statuses), FormatBytes(reclaim.all)) + offlineNote(offline),
					Tone:   alertTone,
				})
			}

			for _, ms := range statuses[:min(len(statuses), 4)] {
				sub := "还没检查"
				if ms.TotalBytes > 0 {
					sub = fmt.Sprintf("用了 %.0f%%（还剩 %s）· %s",
						ms.UsedPercent, FormatBytes(ms.AvailableBytes), reclaimWords(ms.ReclaimableBytes, ms.SafeBytes))
					if ms.Partial {
						sub = fmt.Sprintf("用了 %.0f%%（还剩 %s）· 缓存没算完",
							ms.UsedPercent, FormatBytes(ms.AvailableBytes))
					}
					if lastKnown(ms) {
						sub = offlineText + " · 上次" + sub
					}
				} else if ms.Machine.LastError != "" {
					sub = offlineText + "：" + sshProblem(ms.Machine, ms.Machine.LastError).Title
				}
				items = append(items, wire.Component{
					Type:     wire.ComponentRow,
					Key:      boundedKey("card-mach-", ms.Machine.ID),
					Icon:     machineIcon(ms.Level),
					Title:    machineLabel(ms.Machine),
					Subtitle: sub,
					Badge:    healthBadge(ms),
				})
			}
			if len(st.History) > 0 {
				last := st.History[0]
				items = append(items, wire.Component{
					Type:  wire.ComponentText,
					Style: wire.TextStyleCaption,
					Text:  fmt.Sprintf("现在一共可以腾出 %s · 最近一次清理 %s：%s", FormatBytes(reclaim.all), histMachineLabel(st, last), last.Summary),
				})
			} else if reclaim.all > 0 {
				items = append(items, wire.Component{
					Type:  wire.ComponentText,
					Style: wire.TextStyleCaption,
					Text:  fmt.Sprintf("共 %d 台电脑 · 现在一共可以腾出 %s", len(statuses), FormatBytes(reclaim.all)),
				})
			}
		}
		if len(items) == 0 {
			msg := "Disklessly：看每台电脑的硬盘用了多少，删掉能重新生成的缓存，把一周没动的 AI 对话搬到网盘。检查只读取，不会删东西。"
			items = []wire.Component{{
				Type:  wire.ComponentText,
				Style: wire.TextStyleSecondary,
				Text:  msg,
			}}
			if fallback == "" {
				fallback = msg
			}
		}
		out = d.fitVersion(&wire.View{Fallback: fallback, Sections: []wire.Section{{Items: items}}})
	})
	return out, err
}

// ---- UI Actions ----

func (a *app) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	var p page
	if err := r.DecodeState(&p); err != nil {
		return nil, &ext.Error{Code: "invalid", Message: pageStateLost}
	}
	res := &wire.ActionResult{}
	var deep *deepResult

	switch r.Action {
	case "machine":
		if r.Values["machine"] == "+" {
			p.Mode = modeNewMachine
		} else {
			p.Machine = r.Values["machine"]
			p.Mode = modeHome
		}
	case "tab":
		p.Tab = r.Values["tab"]
		p.Mode = modeHome
	case "search":
		p.Query = strings.TrimSpace(r.Values["q"])
	case "details":
		p.Open = toggle(p.Open, r.Values["k"])
	case "scan", "scan-all", "clean-safe", "clean-category", "clean-item", "clean-selected":
		toast, err := a.mainJob(r, &p)
		if err != nil {
			return nil, &ext.Error{Code: "invalid", Message: userMessage(err)}
		}
		res.Toast = toast
	case "open-sessions":
		// An app that does not push pages calls this: open AI 对话 on top.
		res.Push = &wire.Push{UI: sessionsUI, Title: sessionsTitle}
	case "cold-search", "cold-deep":
		// The old 会话与台账 tab's search: conversations are on AI 对话 now.
		p.SessQuery = clipQuery(cmp.Or(strings.TrimSpace(r.Values["q"]), p.SessQuery))
		res.Push = &wire.Push{UI: sessionsUI, Title: sessionsTitle, State: ext.EncodeState(sessPage{Machine: p.Machine, Query: p.SessQuery})}
	case "cold-refresh", "cold-offload", "cold-restore", "archive-sessions", "cold-install", "cold-brew":
		// Buttons of the old 会话与台账 tab still work; their results show on AI 对话.
		p.Machine = cmp.Or(r.Values["m"], p.Machine)
		toast, err := a.coldAction(ctx, r.Action, p.Machine, r.Values["id"])
		if err != nil {
			return nil, &ext.Error{Code: "invalid", Message: userMessage(err)}
		}
		res.Toast = toast
		res.Push = &wire.Push{UI: sessionsUI, Title: sessionsTitle, State: ext.EncodeState(sessPage{Machine: p.Machine})}
	case "select-machine":
		p.Machine = cmp.Or(r.Values["m"], p.Machine)
		p.Tab = cmp.Or(r.Values["t"], tabItems)
		p.Mode = modeHome
	case "open-plan":
		p.Machine = cmp.Or(r.Values["m"], p.Machine)
		p.Tab = tabPlan
		p.Mode = modeHome
	case "advice-all":
		// An app that does not push pages: draw every step in place.
		p.Mode = modeAdvice
	case "new-machine":
		p.Mode = modeNewMachine
	case "edit-machine":
		if m := r.Values["m"]; m != "" {
			p.Machine = m
		}
		p.Mode = modeEditMach
	case "cancel":
		p.Mode = modeHome
	default:
		err := a.db().update(func(st *State, now time.Time) error {
			return a.handleStateAction(st, now, r, &p, res)
		})
		if err != nil {
			return nil, &ext.Error{Code: "invalid", Message: userMessage(err)}
		}
	}

	var err error
	res.View, err = a.pageView(r, p, deep)
	return res, err
}

func (a *app) handleStateAction(
	st *State,
	now time.Time,
	r *ext.UIRequest,
	p *page,
	res *wire.ActionResult,
) error {
	targetID := cmp.Or(r.Values["m"], p.Machine, "local")
	switch r.Action {
	case "save-machine":
		port, _ := strconv.Atoi(strings.TrimSpace(r.Values["port"]))
		in := MachineInput{
			Host:         r.Values["host"],
			Name:         r.Values["name"],
			User:         r.Values["user"],
			Port:         port,
			IdentityFile: r.Values["identity_file"],
			Source:       "manual",
		}
		if p.Mode == modeEditMach {
			in.ID = p.Machine
			if existing := st.machine(p.Machine); existing != nil && existing.IsLocal {
				in.Host = "localhost"
			}
		}
		m, err := st.upsertMachine(in, now)
		if err != nil {
			return err
		}
		p.Machine = m.ID
		p.Mode = modeHome
		res.Toast = "已保存「" + machineLabel(m) + "」"

	case "delete-machine":
		if err := st.deleteMachine(targetID); err != nil {
			return err
		}
		p.Machine = "local"
		p.Mode = modeHome
		res.Toast = "已经不再管这台电脑，它上面的文件没有动"

	default:
		return fmt.Errorf("这个按钮已经不能用了（%q），请刷新页面", r.Action)
	}
	return nil
}

// ---- UI Helpers ----

func (d draw) machineHeaderOrText(ms MachineStatus, scan *Scan) wire.Component {
	if d.v4() {
		return d.machineHeaderComponent(ms, scan)
	}
	return wire.Component{
		Type: wire.ComponentText,
		Text: machinePlainTextSummary(ms, scan),
	}
}

func (d draw) machineHeaderComponent(ms MachineStatus, scan *Scan) wire.Component {
	m := ms.Machine
	b := healthBadge(ms)
	facts := []wire.Fact{
		{Label: "地址", Value: cmp.Or(m.Host, "localhost")},
		{Label: "怎么来的", Value: sourceLabel(m.Source)},
	}
	if p := platformLabel(m.Platform); p != "" {
		facts = append(facts, wire.Fact{Label: "系统", Value: p})
	}
	if scan != nil && scan.Mount.TotalBytes > 0 {
		facts = append(facts,
			wire.Fact{Label: "系统盘大小", Value: FormatBytes(scan.Mount.TotalBytes)},
			wire.Fact{Label: "还剩", Value: FormatBytes(scan.Mount.AvailableBytes)},
			wire.Fact{Label: "可以腾出", Value: FormatBytes(scan.ReclaimableBytes)},
		)
	}
	if ms.CleanCount > 0 {
		facts = append(facts, wire.Fact{Label: "一共腾出过", Value: FormatBytes(ms.TotalFreedBytes)})
	}
	sub := "还没检查：检查只读取大小，不会删除任何东西"
	if scan != nil {
		sub = fmt.Sprintf("系统盘 %s · %s检查", scan.Mount.MountPoint, d.when(scan.ScannedAt))
		if lastKnown(ms) {
			sub = fmt.Sprintf("%s · 系统盘 %s 的数是%s检查时的", d.offlineWords(m), scan.Mount.MountPoint, spaced(d.when(scan.ScannedAt)))
		}
	}
	var badges []wire.Badge
	if b != nil {
		badges = append(badges, *b)
	}
	if lastKnown(ms) {
		badges = append(badges, *offlineBadge())
	}
	return wire.Component{
		Type:     wire.ComponentHeader,
		Title:    machineLabel(m),
		Subtitle: sub,
		Badges:   badges,
		Facts:    facts,
	}
}

// offlineBadge is a computer that does not answer now: neutral, so it never reads as a disk
// that is filling up.
func offlineBadge() *wire.Badge { return &wire.Badge{Text: offlineText, Tone: wire.ToneNeutral} }

// healthBadge is how full ms's disk is. A computer that cannot be reached shows its last check's
// fullness, marked 上次, when there is one (its row says it cannot be reached), else 连不上这台电脑.
func healthBadge(ms MachineStatus) *wire.Badge {
	if lastKnown(ms) {
		b := healthBadge(MachineStatus{Level: fullnessLevel(ms), TotalBytes: ms.TotalBytes, UsedPercent: ms.UsedPercent})
		b.Text = "上次 " + b.Text
		return b
	}
	switch ms.Level {
	case HealthUnscanned:
		return &wire.Badge{Text: "还没检查", Tone: wire.ToneNeutral}
	case HealthOffline:
		return offlineBadge()
	case HealthCritical:
		return &wire.Badge{Text: fmt.Sprintf("快满了 %.0f%%", ms.UsedPercent), Tone: wire.ToneDanger}
	case HealthWarning:
		return &wire.Badge{Text: fmt.Sprintf("偏满 %.0f%%", ms.UsedPercent), Tone: wire.ToneCaution}
	default:
		if ms.TotalBytes > 0 {
			return &wire.Badge{Text: fmt.Sprintf("宽裕 %.0f%%", ms.UsedPercent), Tone: wire.TonePositive}
		}
		return &wire.Badge{Text: "还没检查", Tone: wire.ToneNeutral}
	}
}

func progressTone(level string) string {
	switch level {
	case HealthCritical:
		return wire.ToneDanger
	case HealthWarning:
		return wire.ToneCaution
	case HealthUnscanned, HealthOffline:
		return wire.ToneNeutral
	default:
		return wire.TonePositive
	}
}

// machineIcon is a computer row's icon: reachability (wifi) for one that cannot be reached, a
// warning for a disk filling up.
func machineIcon(level string) string {
	switch level {
	case HealthOffline:
		return "wifi"
	case HealthCritical, HealthWarning:
		return "warning"
	default:
		return "gauge"
	}
}

func toneIf(cond bool, t string) string {
	if cond {
		return t
	}
	return ""
}

// chipUnits is how wide a machine filter's label may be, in chipWidth units: the web draws the filter as a select at
// most 200 px wide, 162 px of it for the text at 13 px (about 25 units), and cuts off whatever is longer.
const chipUnits = 25

// diskChipLabel is a machine's label in the disk page's filter: its disk use first, then its name cut to the room left,
// so a long name loses its end and the number is never cut off ("50% · 运行 Persona 的电脑").
func diskChipLabel(ms MachineStatus) string {
	if ms.TotalBytes == 0 {
		return chipFit(machineLabel(ms.Machine), chipUnits)
	}
	head := fmt.Sprintf("%.0f%% · ", ms.UsedPercent)
	return head + chipFit(machineLabel(ms.Machine), chipUnits-chipWidth(head))
}

// chipWidth is s's width in units of about half a CJK character: a wide rune (CJK, kana, hangul, full-width forms)
// counts 2, any other 1.
func chipWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeUnits(r)
	}
	return n
}

func runeUnits(r rune) int {
	switch {
	case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul),
		r >= 0x3000 && r <= 0x303F,                             // CJK symbols and punctuation
		r >= 0xFF00 && r <= 0xFF60, r >= 0xFFE0 && r <= 0xFFE6: // full-width forms
		return 2
	}
	return 1
}

// chipFit cuts s to at most maxUnits chipWidth units, ending in "…" when it cuts.
func chipFit(s string, maxUnits int) string {
	s = strings.TrimSpace(s)
	if chipWidth(s) <= maxUnits {
		return s
	}
	n, out := 0, []rune{}
	for _, r := range s {
		if n+runeUnits(r) > maxUnits-1 { // room for the "…"
			break
		}
		n += runeUnits(r)
		out = append(out, r)
	}
	return strings.TrimSpace(string(out)) + "…"
}

func clip(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes == 1 {
		return string(r[:1])
	}
	return string(r[:maxRunes-1]) + "…"
}

// sectionItemCount counts components across secs using wire.CheckView's accounting.
func sectionItemCount(secs []wire.Section) int {
	n := 0
	for _, s := range secs {
		for _, e := range s.Items {
			n += max(1, len(e.Tiles)+len(e.Nodes)+len(e.Pins)+len(e.Markers))
		}
	}
	return n
}

// boundedKey returns prefix+raw unchanged when <= 128 runes, or truncates raw with an 8-hex FNV-1a suffix.
func boundedKey(prefix, raw string) string {
	full := prefix + raw
	if utf8.RuneCountInString(full) <= 128 {
		return full
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(raw))
	suffix := fmt.Sprintf("-%08x", h.Sum32())
	keep := 128 - utf8.RuneCountInString(prefix) - len(suffix)
	if keep <= 0 {
		pr := []rune(prefix)
		if len(pr) > 128-len(suffix) {
			pr = pr[:max(0, 128-len(suffix))]
		}
		return string(pr) + suffix
	}
	rr := []rune(raw)
	if len(rr) > keep {
		rr = rr[:keep]
	}
	return prefix + string(rr) + suffix
}

func (d draw) when(t time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	ago := d.now.Sub(t)
	switch {
	case ago < time.Minute:
		return "刚刚"
	case ago < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(ago.Minutes()))
	case ago < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(ago.Hours()))
	case ago < 7*24*time.Hour:
		return fmt.Sprintf("%d 天前", int(ago.Hours()/24))
	}
	return d.shortDate(t)
}

// legendIf is categoryLegend of kinds when legend is set, else "".
func legendIf(legend bool, kinds []string) string {
	if !legend {
		return ""
	}
	return categoryLegend(CategoryAxisLabels, kinds)
}

// spaced puts a space before a time that starts with a number ("3 天前"), as after a Chinese
// character the pages write it: 「页 3 天前检查」 but 「页刚刚检查」.
func spaced(when string) string {
	if when != "" && when[0] >= '0' && when[0] <= '9' {
		return " " + when
	}
	return when
}

// stamp is when a conversation or a clean-up was, as when words it, with the time of day once it
// is a day old or more (several of one day stay apart): 「5 小时前」「3 天前 16:17」「10月24日 16:17」.
func (d draw) stamp(t time.Time) string {
	if d.now.Sub(t) < 24*time.Hour {
		return d.when(t)
	}
	return d.when(t) + " " + t.In(d.loc).Format("15:04")
}

// sessionOffloadConfirm describes what a cold-session run does: it covers every agent at once,
// uploads off the machine and deletes the local copies only after a verified read-back.
func sessionOffloadConfirm(m *Machine) string {
	return fmt.Sprintf("把「%s」上超过 7 天没动的 AI 对话（Claude Code、Codex 等所有 AI 助手的）搬到网盘？会先上传到网盘，再从网盘读回来核对，核对无误才删掉电脑上的那份。正在运行的程序、herdr 或 Persona 还在用的对话不会动；网盘连不上、或者确认不了哪些还在用时，一个都不删。", machineLabel(m))
}

// safeCleanConfirm says what 删掉放心删的缓存 deletes on m: the rebuildable caches and, when
// the scan found them, old logs and stale temporary files, which do not come back; and what it
// never touches.
func safeCleanConfirm(m *Machine, bytes uint64, scan *Scan) string {
	return fmt.Sprintf("在「%s」上删掉所有「放心删」的东西（大约腾出 %s）？包括能重新生成或重新下载的编译和下载缓存（Go、Node/Python、Docker、Rust/Xcode 等）。%s正在运行的容器、正在开发的项目和 AI 对话都不会碰；一周没动的 AI 对话要在「%s」页另外搬到网盘。", machineLabel(m), FormatBytes(bytes), safeLogsNote(scan), sessionsTitle)
}

// safeLogsNote names the old logs and temporary files among scan's 放心删 items: unlike the
// caches they are gone once deleted. Each claim follows what BuildItemCleanScript runs for that
// item: build and install leftovers in /tmp go whatever their age, every rotated log file goes,
// and the journal size shown is the whole journal although only its part older than 7 days goes.
// "" when there are none.
func safeLogsNote(scan *Scan) string {
	if scan == nil {
		return ""
	}
	var logs []ScanItem
	for _, it := range scan.Items {
		if it.Category == CatTmpLogs && it.Safety == SafetySafe && !it.InUse {
			logs = append(logs, it)
		}
	}
	if len(logs) == 0 {
		return ""
	}
	return fmt.Sprintf("还会删掉 %d 项日志和临时文件，这些删了就找不回来：%s。", len(logs), logsNoteParts(logs))
}

// logsNoteParts says, item by item, what deleting each old log or temporary-file item removes.
func logsNoteParts(logs []ScanItem) string {
	var parts []string
	for _, it := range logs {
		size := FormatBytes(it.Bytes)
		switch it.ID {
		case "journalctl-vacuum":
			parts = append(parts, "系统日志里 7 天以前的部分（这里显示的 "+size+" 是全部系统日志的大小，只删其中 7 天以前的，最近 7 天的留着，所以实际腾出的会少一些）")
		case "logs-rotated":
			parts = append(parts, "系统换下来的旧日志（"+size+"：/var/log 里 .gz 压缩的和 .1、.old 结尾的旧日志文件，正在写的日志不碰）")
		case "tmp-stale":
			parts = append(parts, "临时文件夹 /tmp 里的文件（"+size+"：编译和安装剩下的，比如 go-build、npm、pnpm、playwright、cargo-install 开头的，最近的也会删；还有其他放了 7 天以上的）")
		default:
			parts = append(parts, cmp.Or(itemTitle(it), it.ID)+"（"+size+"）")
		}
	}
	return strings.Join(parts, "；")
}

// categoryCleanArgs are the disklessly clean arguments the 删掉：<种类> row's clean-category
// action runs with: the kind by name, which takes its 建议先看 items too (see BuildCleanPlan).
func categoryCleanArgs(cid string) (categories []string, includeReview bool) {
	return []string{cid}, true
}

// categoryCleanPlan is what tapping 删掉：<种类> deletes on m right now; nil when it would refuse.
func categoryCleanPlan(m *Machine, scan *Scan, cid string) *CleanPlan {
	cats, includeReview := categoryCleanArgs(cid)
	plan, err := BuildCleanPlan(m, scan, cats, nil, includeReview)
	if err != nil {
		return nil
	}
	return plan
}

func reviewItems(items []ScanItem) []ScanItem {
	var out []ScanItem
	for _, it := range items {
		if it.Safety == SafetyReview {
			out = append(out, it)
		}
	}
	return out
}

// maxNamedReviewItems is how many 建议先看 items a kind's confirmation names before 等 N 项.
const maxNamedReviewItems = 3

// categoryConfirm says what tapping 删掉：<种类> deletes: plan is categoryCleanPlan's, so the
// count and size are what really goes. Caches come back by downloading or building again; old
// logs and temporary files do not; 建议先看 items are named with their own reason, since this
// one tap deletes them too. Old conversations are moved, not deleted, and keep their own words.
func categoryConfirm(m *Machine, scan *Scan, cid string, plan *CleanPlan) string {
	if cid == CatColdAgentSession {
		return sessionOffloadConfirm(m)
	}
	var caches, logs []ScanItem
	review := reviewItems(plan.Items)
	for _, it := range plan.Items {
		switch {
		case it.Safety == SafetyReview:
		case it.Category == CatTmpLogs:
			logs = append(logs, it)
		default:
			caches = append(caches, it)
		}
	}
	n := len(plan.Items)
	var b strings.Builder
	fmt.Fprintf(&b, "在「%s」上删掉「%s」这一类？一共 %d 项，大约腾出 %s。", machineLabel(m), categoryName(cid), n, FormatBytes(plan.ExpectedFree))
	group := func(k int) string {
		switch {
		case k == 1 && n == 1:
			return "这 1 项是"
		case k == n:
			return fmt.Sprintf("这 %d 项都是", k)
		}
		return fmt.Sprintf("其中 %d 项是", k)
	}
	if len(caches) > 0 {
		b.WriteString(group(len(caches)) + "「放心删」的缓存，删了以后用到时会重新下载或重新生成。")
	}
	if len(logs) > 0 {
		b.WriteString(group(len(logs)) + "日志和临时文件，删了就找不回来：" + logsNoteParts(logs) + "。")
	}
	if len(review) > 0 {
		var named []string
		for _, it := range review[:min(len(review), maxNamedReviewItems)] {
			why := strings.TrimRight(itemReason(it), "。；，")
			named = append(named, fmt.Sprintf("%s（%s），%s", cmp.Or(itemTitle(it), it.ID), FormatBytes(it.Bytes), clip(cmp.Or(why, "比较大，删了重新下载或重新生成很慢"), 120)))
		}
		more := ""
		if len(review) > maxNamedReviewItems {
			more = fmt.Sprintf("等 %d 项", len(review))
		}
		them := "它"
		if len(review) > 1 {
			them = "它们"
		}
		keep := "不想删" + them + "，就到下面的每一项里只删别的。"
		if len(review) == n {
			keep = "不想删" + them + "，就先别点。"
		}
		fmt.Fprintf(&b, "%s「建议先看」的，平时只有自己挑了才删：%s%s。点这一行，%s也会一起删掉；%s", group(len(review)), strings.Join(named, "；"), more, them, keep)
	}
	var inUse int
	for _, it := range scan.Items {
		if it.Category == cid && it.InUse && (it.Bytes > 0 || it.SessionCnt > 0) {
			inUse++
		}
	}
	if inUse > 0 {
		fmt.Fprintf(&b, "另有 %d 项正在用，不删，也没算在里面。", inUse)
	}
	return b.String()
}

func itemConfirm(m *Machine, it ScanItem, verb string) string {
	if it.Category == CatColdAgentSession {
		return sessionOffloadConfirm(m)
	}
	return fmt.Sprintf("确定在「%s」上%s「%s」吗？（位置 %s，大约腾出 %s）", machineLabel(m), verb, cmp.Or(itemTitle(it), it.ID), it.Path, FormatBytes(it.Bytes))
}

// safetyLabel words a safety level: 放心删 (rebuilt on its own) or 建议先看.
func safetyLabel(s string) string {
	if s == SafetyReview {
		return "建议先看"
	}
	return "放心删"
}

// itemSafetyLabel is safetyLabel for a scan item; old AI conversations are moved, not deleted.
func itemSafetyLabel(it ScanItem) string {
	if it.Safety != SafetyReview && it.Category == CatColdAgentSession {
		return "搬到网盘"
	}
	return safetyLabel(it.Safety)
}

// safetyTag and itemSafetyTag are the short words of a narrow table cell.
func safetyTag(s string) string {
	if s == SafetyReview {
		return "先看"
	}
	return "放心删"
}

func itemSafetyTag(it ScanItem) string {
	if it.Safety != SafetyReview && it.Category == CatColdAgentSession {
		return "搬网盘"
	}
	return safetyTag(it.Safety)
}

// sourceLabel says where a computer in the list came from.
func sourceLabel(src string) string {
	switch src {
	case "local":
		return localMachineName
	case "persona":
		return "Persona 里登记的"
	case "ssh-config":
		return "~/.ssh/config 里写的"
	case "manual":
		return "自己添加的"
	}
	return cmp.Or(src, "不清楚")
}
