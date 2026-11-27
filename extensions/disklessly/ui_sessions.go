package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The cold-session backup page (nav page AI 对话): computer filter, network-drive status, each
// computer's state with what to do next and its upload action, search over the local cold index
// (deep full-text search on demand), results with their location and restore. Session status is
// drawn on this page only; the main page links here.

const (
	sessionsUI      = "disklessly-sessions"
	allMachines     = "all"
	coldShown       = 50
	coldSearchHint  = "搜对话：标题、第一句话、项目或关键词"
	coldOffloadText = "把一周没动的 AI 对话搬到网盘"
	coldRetryText   = "重试：搬到网盘"
	coldRefreshText = "重新检查所有电脑"
	coldDeepText    = "搜全文"
	sessionsTitle   = "AI 对话"
	// coldQueryRunes keeps a query well under the 2000-character Search.value limit.
	coldQueryRunes = 200
)

// clipQuery trims a search query and cuts it to coldQueryRunes runes.
func clipQuery(q string) string {
	q = strings.TrimSpace(q)
	if r := []rune(q); len(r) > coldQueryRunes {
		q = strings.TrimSpace(string(r[:coldQueryRunes]))
	}
	return q
}

// sessPage is the AI 对话 page state: the machine filter and the query only (16 KB limit).
type sessPage struct {
	Machine string `json:"m,omitempty"`
	Query   string `json:"q,omitempty"`
	// Open is the part of the page whose 详情 is shown.
	Open string `json:"x,omitempty"`
}

// deepResult carries a deep search's hits into one render; they are never stored in page state.
type deepResult struct {
	hits []ColdHit
	errs []string
}

// uncheckedText is what the page calls LocUnknown: the stat, the rows' badges and every sentence
// use this one word. MCP answers and tool descriptions keep LocUnknown itself.
const uncheckedText = "还没核对过"

// locationText is a conversation's place as the page words it.
func locationText(loc string) string {
	if loc == LocUnknown {
		return uncheckedText
	}
	return loc
}

// pageWords rewrites a sentence shared with MCP answers (coldProblem, a job's error) to the
// page's word for LocUnknown.
func pageWords(s string) string {
	return strings.ReplaceAll(s, "「"+LocUnknown+"」", "「"+uncheckedText+"」")
}

func locationTone(loc string) string {
	switch loc {
	case LocCloud:
		return wire.TonePositive
	case LocMissing:
		return wire.ToneDanger
	case LocLocal:
		return wire.ToneNeutral
	}
	return wire.ToneCaution
}

// coldSections draws the cold backup status, actions and results for machine m (nil: all machines),
// keeping the total component count within maxItems (defaulting to wire.ItemsMost).
func (d draw) coldSections(m *Machine, query string, deep *deepResult, maxItems int) []wire.Section {
	if maxItems <= 0 {
		maxItems = wire.ItemsMost
	}
	query = clipQuery(query)
	machineArg := ""
	if m != nil {
		machineArg = m.ID
	}
	var machines []*Machine
	for _, mm := range d.st.Machines {
		if (m == nil && mm.Enabled) || (m != nil && mm.ID == m.ID) {
			machines = append(machines, mm)
		}
	}

	counts := map[string]int{}
	var total, indexed int
	var capped bool
	var lastCheck time.Time
	byState := map[string]int{}
	// The summary counts by the rows' badges: a state, or how a drive is down.
	byLabel, labelRank, labelTone := map[string]int{}, map[string]int{}, map[string]string{}
	var running []string
	ready := 0
	type machineRow struct {
		m     *Machine
		cat   *ColdCatalog
		state string
	}
	var rows []machineRow
	for _, mm := range machines {
		cat := d.st.cold(mm.ID)
		state := d.pageState(mm, cat)
		byState[state]++
		label := d.pageLabel(mm, cat)
		byLabel[label]++
		labelRank[label], labelTone[label] = coldStateRank(state), coldStateTone(state)
		if state == ColdReady {
			ready++
		}
		rows = append(rows, machineRow{mm, cat, state})
		if j, ok := d.jobs[mm.ID]; ok && j.running() {
			running = append(running, "「"+machineLabel(mm)+"」"+strings.TrimPrefix(j.Label, "正在"))
		}
		if cat == nil {
			continue
		}
		total += cat.Total
		indexed += len(cat.Entries)
		capped = capped || cat.Capped
		if cat.Backend.CheckedAt.After(lastCheck) {
			lastCheck = cat.Backend.CheckedAt
		}
		for _, e := range cat.Entries {
			counts[cat.location(e, mm, d.now)]++
		}
	}
	slices.SortStableFunc(rows, func(a, b machineRow) int { return cmp.Compare(coldStateRank(a.state), coldStateRank(b.state)) })
	// The upload button's title as machineStateItems draws it on ready rows: none while a job
	// runs there, 重试 after a failed upload; the plain one wins when any row shows it.
	uploadTitle := ""
	for _, r := range rows {
		if r.state != ColdReady {
			continue
		}
		t := coldOffloadText
		if j, ok := d.jobs[r.m.ID]; ok {
			if j.running() {
				continue
			}
			if j.Kind == jobOffload && j.Err != "" {
				t = coldRetryText
			}
		}
		if uploadTitle == "" || t == coldOffloadText {
			uploadTitle = t
		}
	}

	// The summary counts machines by state ("2 正常 · 2 未安装 CLI · 1 CLI 版本过旧"), worst first.
	var labels []string
	for l := range byLabel {
		labels = append(labels, l)
	}
	slices.SortFunc(labels, func(a, b string) int { return cmp.Or(cmp.Compare(labelRank[a], labelRank[b]), cmp.Compare(a, b)) })
	// The stat's detail is two short counts (a narrow card cuts a longer one off); which states
	// the others are in goes in the full-width line below it.
	var parts []string
	worst := wire.TonePositive
	for _, l := range labels {
		if l != coldStateLabel(ColdReady) {
			parts = append(parts, fmt.Sprintf("%d 台%s", byLabel[l], l))
		}
		if t := labelTone[l]; t == wire.ToneDanger || (t == wire.ToneCaution && worst == wire.TonePositive) {
			worst = t
		}
	}
	if worst == wire.ToneDanger && ready > 0 {
		// The detail says how many are fine too: alarm only when none is.
		worst = wire.ToneCaution
	}
	machineStat := wire.Stat{Label: "电脑", Value: fmt.Sprintf("%d 台", len(machines)), Detail: machineCounts(ready, len(machines)), Tone: worst}
	if len(running) > 0 {
		machineStat.Detail = fmt.Sprintf("%d 台正在处理 · %s", len(running), machineStat.Detail)
	}
	checked := "还没检查过"
	if !lastCheck.IsZero() {
		checked = d.when(lastCheck) + "检查"
	}
	indexDetail := checked
	if capped {
		indexDetail = fmt.Sprintf("只列出最新的 %d 个（共 %d 个）· %s", indexed, total, indexDetail)
	}
	status := wire.Section{
		Title:  "AI 对话：旧对话搬到网盘，要用时再拿回来",
		Footer: "「在网盘里」只看最近 24 小时内的检查结果；网盘连不上、还没检查或电脑关机时显示「" + uncheckedText + "」，不会当成丢了。",
		Items: []wire.Component{{Type: wire.ComponentText, Style: wire.TextStyleSecondary,
			Text: "Claude Code、Codex 这些 AI 助手的聊天记录会一直占着硬盘。一周没动的对话可以搬到你自己的网盘：先上传、再从网盘读回来核对无误，才删掉电脑上的那份；还在用的对话不会动。要用时在下面搜到它，点「拿回来」。"}},
	}
	status.Items = append(status.Items, d.statComponents("", []wire.Stat{
		{Label: "记下的对话", Value: fmt.Sprintf("%d 个", indexed), Detail: indexDetail},
		{Label: LocCloud, Value: fmt.Sprintf("%d 个", counts[LocCloud]), Tone: toneIf(counts[LocCloud] > 0, wire.TonePositive)},
		uncheckedStat(counts[LocUnknown]),
		missingStat(counts[LocMissing]),
		machineStat,
	})...)
	if note := d.whereaboutsNote(m, counts[LocUnknown], counts[LocMissing]); note != "" {
		status.Items = append(status.Items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: clip(note, 600)})
	}
	if len(running) > 0 {
		status.Items = append(status.Items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary,
			Text: clip("正在进行："+strings.Join(running, "、")+"。完成后页面会自动更新，可以先离开这页。", 300)})
	}
	// Where the upload is: under each ready computer's row below; say so, or why there is none.
	shownLabel := ""
	if m != nil {
		shownLabel = d.pageLabel(m, d.st.cold(m.ID))
	}
	status.Items = append(status.Items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary,
		Text: clip(offloadHint(m, ready, len(machines), byState, uploadTitle, shownLabel, parts), 400)})
	if m == nil {
		// One computer's own row below has its 重新检查 button.
		status.Items = append(status.Items, wire.Component{
			Type: wire.ComponentButton, Title: coldRefreshText,
			Action: &wire.Action{ID: "cold-refresh", Values: map[string]string{"m": machineArg}},
		})
	}
	if strings.TrimSpace(query) != "" {
		status.Items = append(status.Items, wire.Component{
			Type: wire.ComponentButton, Title: coldDeepText,
			Action: &wire.Action{ID: "cold-deep", Values: map[string]string{"m": machineArg, "q": query}},
		})
	}

	var hits []ColdHit
	title := "已经搬到网盘的对话"
	if deep != nil {
		hits = deep.hits
		title = fmt.Sprintf("搜全文「%s」（%d 个）", clip(query, 24), len(hits))
	} else {
		hits = d.pageSearch(query, machineArg, 0)
		if strings.TrimSpace(query) != "" {
			title = fmt.Sprintf("搜索「%s」（%d 个）", clip(query, 24), len(hits))
		}
	}
	results := wire.Section{Title: title}
	if deep != nil {
		for _, e := range deep.errs[:min(len(deep.errs), 4)] {
			msg, _ := humanize(e)
			results.Items = append(results.Items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: clip("搜全文没有搜完："+msg, 300)})
		}
	}
	shown := hits[:min(len(hits), coldShown)]
	seenColdKeys := map[string]bool{}
	for i, h := range shown {
		row := d.coldRow(h)
		for n := i; seenColdKeys[row.Key]; n++ {
			row.Key = boundedKey(fmt.Sprintf("cold-%d-", n), h.MachineID+"-"+h.ID)
		}
		seenColdKeys[row.Key] = true
		results.Items = append(results.Items, row)
	}
	var notes []string
	if len(hits) > len(shown) {
		notes = append(notes, fmt.Sprintf("显示最新的 %d 个，共 %d 个；多输几个词可以缩小范围。", len(shown), len(hits)))
	}
	if capped {
		notes = append(notes, fmt.Sprintf("这里每台电脑只列出最新的 %d 个对话，更早的请用「%s」。", maxColdEntries, coldDeepText))
	}
	notes = append(notes, searchHint)
	results.Footer = strings.Join(notes, " ")
	if len(shown) == 0 {
		detail := "还没有搬到网盘的对话。点「" + coldRefreshText + "」看看电脑的情况；检查只读取信息，不会搬走或删除任何东西。"
		if m != nil {
			// This computer's own row above carries its check button (or says why there is none).
			detail = "还没有搬到网盘的对话。这台电脑的情况和该点的按钮在上面它那一行；检查只读取信息，不会搬走或删除任何东西。"
		}
		if strings.TrimSpace(query) != "" {
			detail = "这里没有找到；可以试试「" + coldDeepText + "」，在电脑上搜对话的完整内容。"
		}
		results.Items = append(results.Items, d.emptyComponents("chat", "没有对话", detail, nil)...)
	}

	adviceSec := d.sessionsAdviceSection(m)
	head := len(status.Items) + len(adviceSec.Items)
	if allowResults := max(0, maxItems-head); len(results.Items) > allowResults {
		results.Items = results.Items[:allowResults]
	}
	machineSec := wire.Section{Key: "cold-machines", Title: "每台电脑的情况"}
	machineBudget := max(0, maxItems-head-len(results.Items))
	compactCount := 0
	for i, r := range rows {
		rem := len(rows) - i - 1
		if machineBudget <= 0 || len(machineSec.Items) >= machineBudget {
			break
		}
		if len(rows) > 1 && len(machineSec.Items) == machineBudget-1 && rem > 0 {
			machineSec.Items = append(machineSec.Items, wire.Component{
				Type:  wire.ComponentText,
				Style: wire.TextStyleSecondary,
				Text:  fmt.Sprintf("还有 %d 台电脑没列出来，在顶部「电脑」里选它查看。", len(rows)-i),
			})
			break
		}
		full := d.machineStateItems(r.m, r.cat, r.state)
		if len(rows) == 1 || len(machineSec.Items)+len(full)+rem <= machineBudget {
			if len(machineSec.Items)+len(full) <= machineBudget {
				machineSec.Items = append(machineSec.Items, full...)
				continue
			}
		}
		machineSec.Items = append(machineSec.Items, d.machineStateCompactRow(r.m, r.cat, r.state))
		compactCount++
	}
	if compactCount > 0 {
		machineSec.Footer = fmt.Sprintf("后面 %d 台电脑只显示一行；点它（或在顶部「电脑」里选它）可以看到全部操作。", compactCount)
	}
	secs := []wire.Section{status}
	if len(adviceSec.Items) > 0 {
		secs = append(secs, adviceSec)
	}
	if len(machineSec.Items) > 0 {
		secs = append(secs, machineSec)
	}
	return append(secs, results)
}

// recheckButton is the title of the button on this page that checks again where conversations
// are: 重新检查所有电脑 under 全部电脑, else the shown computer's own button, or "" while its row
// draws none (a job runs there, or trying again cannot help).
func (d draw) recheckButton(m *Machine) string {
	if m == nil {
		return coldRefreshText
	}
	if j, ok := d.jobs[m.ID]; ok && j.running() {
		return ""
	}
	cat := d.st.cold(m.ID)
	if adv := d.pageProblem(m, cat, d.pageState(m, cat)); adv.retry != "" {
		return adv.retry + " " + machineLabel(m)
	}
	return ""
}

// uncheckedStat counts the conversations whose place is LocUnknown (see ColdCatalog.location):
// not confirmed in the network drive by a check of the last 24 hours that reached both the
// computer and its network drive and got to their data package. Not a loss.
func uncheckedStat(n int) wire.Stat {
	s := wire.Stat{Label: uncheckedText, Value: fmt.Sprintf("%d 个", n), Detail: "搬走的都核对过了"}
	if n > 0 {
		s.Detail = "不是丢了，说明在下面"
	}
	return s
}

// missingStat counts the conversations whose place is LocMissing: recorded as moved, but the
// check found the data package holding them missing from the network drive.
func missingStat(n int) wire.Stat {
	s := wire.Stat{Label: LocMissing, Value: fmt.Sprintf("%d 个", n), Detail: "没有发现少了的"}
	if n > 0 {
		s.Tone, s.Detail = wire.ToneDanger, "现在拿不回来，说明在下面"
	}
	return s
}

// whereaboutsNote explains the 还没核对过 and 网盘里找不到 counts when either is not zero: what
// each means, whether to worry, and what lowers it (naming only a button the page draws).
func (d draw) whereaboutsNote(m *Machine, unknown, missing int) string {
	var parts []string
	if unknown > 0 {
		t := fmt.Sprintf("「%s」的 %d 个不是丢了：只是最近 24 小时里没能核对它们在不在网盘里，比如电脑或网盘没连上、还没检查，或者上次检查没来得及核对完。", uncheckedText, unknown)
		if b := d.recheckButton(m); b != "" {
			t += "电脑和网盘都连得上时点「" + b + "」，核对过的就不算在这里了。"
		} else {
			t += "电脑和网盘都连得上时再检查一次，核对过的就不算在这里了。"
		}
		parts = append(parts, t)
	}
	if missing > 0 {
		parts = append(parts, fmt.Sprintf("「%s」的 %d 个要留意：记着已经搬到网盘，但检查时网盘里没找到存它们的数据包，现在拿不回来。先别再删网盘里的东西；看看这台电脑是不是换了网盘或网盘目录（每台电脑那一行写着用的哪个网盘），换回原来的再检查一次。", LocMissing, missing))
	}
	return strings.Join(parts, "")
}

// offloadHint says where the upload button is (under each ready computer's row), or why no
// computer shown can upload now. upload is the button's title as drawn; "" while every ready
// computer is busy.
//
// others names the states of the computers that are not ready ("1 台连不上这台电脑"), worst first.
func offloadHint(m *Machine, ready, total int, byState map[string]int, upload, label string, others []string) string {
	if ready > 0 && upload == "" {
		return "正在进行的事完成后，可以在电脑那一行把一周没动的 AI 对话搬到网盘。"
	}
	if m != nil {
		state := ""
		for s := range byState {
			state = s
		}
		if state == ColdReady {
			return "要腾地方：点下面这台电脑的「" + upload + "」。"
		}
		return "「" + machineLabel(m) + "」现在还不能把对话搬到网盘（" + label + "）：按下面这台电脑那一行的提示处理。"
	}
	switch {
	case total == 0:
		return "还没有电脑。先到「硬盘空间」页添加电脑。"
	case ready == total:
		return "要腾地方：在下面某台电脑那一行点「" + upload + "」，每次搬一台。"
	case ready > 0:
		return fmt.Sprintf("%d 台电脑可以搬：在它那一行点「%s」。其余 %d 台还不能搬（%s），原因和处理办法写在各自那一行。", ready, upload, total-ready, strings.Join(others, "、"))
	}
	return "现在没有哪台电脑能把对话搬到网盘（" + strings.Join(others, "、") + "）：先按下面每台电脑的提示处理，再点「" + coldRefreshText + "」。"
}

// refreshAfter asks the app to draw the page again while a background job runs (hubs that do
// not take ui.invalidate still redraw); 0 otherwise.
func (d draw) refreshAfter() int {
	for _, j := range d.jobs {
		if j.running() {
			return coldPollSeconds
		}
	}
	return 0
}

// coldPollSeconds is the redraw interval while a job runs (the view schema's minimum).
const coldPollSeconds = 30

// coldAdvice is what a machine row says and offers: the problem, and which fixes apply.
type coldAdvice struct {
	problem
	brew    bool         // offer 用 Homebrew 装 / 升级
	upgrade bool         // Homebrew already manages the CLI: upgrade it
	copy    installOffer // src set: offer copy-install
	docs    bool         // link to the install documentation
	retry   string       // the refresh button's verb
}

// coldProblem words machine m's cold-backup state for people: the same text on the page and in
// disklessly_sessions_status.
func coldProblem(st *State, m *Machine, cat *ColdCatalog, state string) coldAdvice {
	name := "「" + machineLabel(m) + "」"
	a := coldAdvice{retry: "重新检查"}
	a.Title = coldStateLabel(state)
	switch state {
	case ColdReady:
		a.retry = "重新检查"
	case ColdNoCLI, ColdCLIOld:
		a.docs = true
		if state == ColdNoCLI {
			a.Reason = name + "上还没装 Disklessly 的配套小工具 disklessly，所以它的旧对话还不能搬到网盘。"
		} else {
			a.Reason = name + "上的配套小工具 disklessly " + cmp.Or(shortVersion(cmp.Or(cat.CLI.Version, m.BinaryVersion)), "（版本不清楚）") +
				" 太旧，还不会搬对话，需要升级。"
			a.Raw = cat.Detail
		}
		a.upgrade = state == ColdCLIOld && brewManaged(cat.CLI)
		cmd := brewInstallCmd
		if a.upgrade {
			cmd = brewUpgradeCmd
		}
		manual := brewSetupComment + "\n" + brewSetupCommand + "\n"
		if strings.HasPrefix(cmp.Or(catPlatform(cat), m.Platform), "linux") {
			manual += brewLinuxShellEnv + "\n"
		}
		manual += brewInstallCmd
		offer := installSourceFor(st, m)
		recheck := "「重新检查 " + machineLabel(m) + "」"
		// Each fact in a sentence of its own: whether it has Homebrew, whether another computer
		// can lend its copy (and why not), what to tap.
		switch {
		case cat != nil && cat.Brew != "":
			a.brew = true
			a.Next = "这台电脑有 Homebrew（一个装软件的工具）：点「" + brewButtonTitle(a.upgrade) + "」就行，装好后会自动重新检查。它在后台运行的命令如下。"
			a.Command = cmd
			a.Lead = a.Reason + "这台电脑有 Homebrew（一个装软件的工具），不用开终端：点「" + brewButtonTitle(a.upgrade) + "」就行，装好后会自动重新检查。"
			a.CmdIntro = "点了以后，它会在" + name + "上运行这条命令（不用自己运行）："
		case cat == nil || !cat.BrewKnown:
			a.copy = offer
			a.Next = "还不知道这台电脑有没有 Homebrew（一个装软件的工具）。先点" + recheck + "，有的话会出现「" + brewButtonTitle(a.upgrade) + "」。"
			if offer.src != nil {
				a.Next += "也可以直接点「从 " + machineLabel(offer.src) + " 复制过来」。"
			} else {
				a.Next += installOfferReason(offer)
			}
		case offer.src != nil:
			a.copy = offer
			src := machineLabel(offer.src)
			copyText := "点「从 " + src + " 复制过来」，会把「" + src + "」上的 " + offeredCLI(offer) + " 复制过来。两台都是 " + platformLabel(offer.platform) + "。"
			a.Next = "这台电脑没有 Homebrew。" + copyText + "也可以在这台电脑上运行下面的命令，先装 Homebrew 再装配套小工具。"
			a.Command = manual
			a.Lead = a.Reason + "这台电脑没有 Homebrew（一个装软件的工具），不过不用开终端。" + copyText
			a.CmdIntro = "懂终端的话，也可以在" + name + "上运行下面的命令，先装 Homebrew，再装配套小工具："
		default:
			a.Next = "这台电脑没有 Homebrew。" + installOfferReason(offer) + "请在这台电脑上运行下面的命令，先装 Homebrew 再装配套小工具，然后点" + recheck + "。"
			a.Command = manual
			a.Lead = a.Reason + "这台电脑没有 Homebrew（一个装软件的工具）。" + installOfferReason(offer) +
				"这一步页面上没有按钮能代劳，只能在" + name + "上开终端装一次，命令在最下面。装好后点" + recheck + "。"
			a.CmdIntro = "在" + name + "的终端里依次运行下面的命令：先装 Homebrew，再装配套小工具。"
		}
	case ColdNoBackend:
		a.problem = noBackendProblem(m)
		a.Raw = cat.Detail
		a.retry = "重新检查"
	case ColdBackendDown:
		b := cat.Backend
		id := cmp.Or(b.ID, b.Type, "（未命名）")
		// The computer answered; its network drive did not. Said apart from 连不上这台电脑.
		a.Reason = "能连上" + name + "，但它连不上自己用的网盘（" + id + "），" + backendUnknownClause
		a.Next = "确认网盘（或家里的 NAS）在线、网络通了，再点「重新检查 " + machineLabel(m) + "」；想看具体原因，在这台电脑上运行下面的命令。"
		a.Command = "disklessly doctor"
		a.Lead = a.Reason + "多半是：网盘（或家里的 NAS）关机、断网了，" + name + "暂时上不了网，或者网盘的登录过期了。先确认网盘在线、网络通了，再点「重新检查 " + machineLabel(m) + "」。"
		a.CmdIntro = "懂终端的话，可以在" + name + "上运行这条命令看具体原因："
		if noRepo(b) {
			// The backend answers, but holds no Disklessly repository.
			a.Reason = name + "能连上自己用的网盘（" + id + "），但里面还没有 Disklessly 的存放目录，找不到 repo.json，" + noRepoUnknownClause
			a.Next = "多半是选错了网盘目录：在这台电脑上用下面的命令看看设置了哪些网盘，换成正确的那个；如果这就是要新用的目录，就在里面新建。然后点「重新检查 " + machineLabel(m) + "」。"
			a.Command = "disklessly backend list\ndisklessly backend use <后端 ID>   # 换到已经在用的那个网盘\ndisklessly init                    # 或者：在当前网盘里新建存放目录"
			a.Lead = a.Reason + "多半是这台电脑设置的网盘目录不对，比如换过网盘、改过路径。换回原来的目录这一步页面上没有按钮能代劳，只能在" + name + "上开终端做一次；做完点「重新检查 " + machineLabel(m) + "」。"
			a.CmdIntro = "在" + name + "的终端里运行：第一条列出设置过的网盘，把第二条里的「<后端 ID>」换成原来那个网盘的名字再运行；如果这就是要新用的目录，改运行第三条，在里面新建存放目录。"
		}
		a.Raw = strings.TrimSpace(fmt.Sprintf("%s（%s，%d ms）", b.Error, cmp.Or(b.Type, "网盘"), b.LatencyMs))
	case ColdOffline:
		errText := m.LastError
		if cat != nil {
			errText = cmp.Or(cat.Error, errText)
		}
		a.problem = sshProblem(m, errText)
		a.retry = "重试"
		if !a.Retry {
			a.retry = ""
		}
	case ColdUnreadable:
		// It answered: the button is 重新检查, as on 硬盘空间 for a computer that answers.
		a.retry = "重新检查"
		hung := cat != nil && strings.HasPrefix(cat.Error, "disklessly 没有正常结束")
		if hung {
			a.Reason = name + "上的配套小工具 3 分钟内没有答完，网盘里的情况暂时不确定。"
			a.Next = "过一会儿点「重新检查 " + machineLabel(m) + "」；一直这样的话，在这台电脑上运行下面的命令看看卡在哪里。"
			a.Lead = a.Reason + "多半是这台电脑这会儿很忙，或者网盘很慢。过一会儿点「重新检查 " + machineLabel(m) + "」。"
			a.CmdIntro = "一直这样的话，懂终端可以在" + name + "上运行这条命令，看看卡在哪里："
		} else {
			a.Reason = name + "上的配套小工具出错了（回答看不懂），网盘里的情况暂时不确定。"
			a.Next = "点「重新检查 " + machineLabel(m) + "」；还是这样的话，在这台电脑上运行下面的命令，把输出（「详情」里也有）发给维护者。"
			a.Lead = a.Reason + "先点「重新检查 " + machineLabel(m) + "」，多数时候再试一次就好了。"
			a.CmdIntro = "还是这样的话，懂终端可以在" + name + "上运行这条命令，把输出（「详情」里也有）发给维护者："
		}
		a.Command = "disklessly --json sessions status --check-segments"
		if cat != nil {
			_, raw := humanize(cat.Error)
			lines := []string{cmp.Or(raw, cat.Error)}
			if cat.Detail != "" {
				lines = append(lines, "配套小工具输出的第一行："+cat.Detail)
			}
			a.Raw = strings.TrimSpace(strings.Join(lines, "\n"))
		}
	default:
		a.retry = "检查"
		a.Reason = "还没检查过" + name + "。"
		a.Next = "点「检查 " + machineLabel(m) + "」：只读取信息，不会搬走或删除任何东西。"
	}
	// sshProblem (offline) words its next step with a generic 「重试」: name the row's button
	// 「<retry> <name>」 instead, or drop it when the row draws none.
	r := retryHint{}
	if a.retry != "" {
		r.button = a.retry + " " + machineLabel(m)
	}
	a.problem = a.renamed(r)
	return a
}

// What coldProblem says about the conversations whose place a drive problem leaves unknown; the
// page says instead how many of this computer's conversations that really is (unknownClause).
const (
	backendUnknownClause = "所以对话在不在网盘里暂时显示「暂时不确定」，不会当成丢了。"
	noRepoUnknownClause  = "所以暂时显示「暂时不确定」。"
)

// pageRecheck is the AI 对话 page's own state (never an MCP state) of a computer this page could
// not reach while a later 硬盘空间 check did: it needs checking again here.
const pageRecheck = "recheck"

// recheckText is pageRecheck's badge.
const recheckText = "要重新检查"

// The two pages share one rule: the newest check of a computer, whichever page ran it, says
// whether it can be reached. Both set m.Reachable (and m.LastError when they could not).

// laterUnreachable reports a computer the newest check could not reach while this page's own
// last check reached it (or never ran): this page calls it 连不上这台电脑 too.
func laterUnreachable(m *Machine, cat *ColdCatalog) bool {
	return !m.Reachable && m.LastError != "" && cat.stateCode() != ColdOffline
}

// laterReached reports a computer this page's last check could not reach while a later
// 硬盘空间 check did.
func (d draw) laterReached(m *Machine, cat *ColdCatalog) bool {
	scan := d.st.Scans[m.ID]
	return cat.stateCode() == ColdOffline && m.Reachable && m.LastError == "" && scan != nil && scan.ScannedAt.After(cat.CheckedAt)
}

// pageState is the state this page draws for m: its catalog's, except as the newest check says.
func (d draw) pageState(m *Machine, cat *ColdCatalog) string {
	switch {
	case laterUnreachable(m, cat):
		return ColdOffline
	case d.laterReached(m, cat):
		return pageRecheck
	}
	return cat.stateCode()
}

// pageLabel is m's badge and summary word on this page, after pageState.
func (d draw) pageLabel(m *Machine, cat *ColdCatalog) string {
	switch d.pageState(m, cat) {
	case ColdOffline:
		return offlineText
	case pageRecheck:
		return recheckText
	}
	return cat.pageLabel()
}

// pageProblem is coldProblem as this page words it for m in page state state: a computer the
// newest check could not reach says so (and what this page found when it last reached it), and
// a drive problem says how many conversations it really leaves unchecked.
func (d draw) pageProblem(m *Machine, cat *ColdCatalog, state string) coldAdvice {
	name := machineLabel(m)
	switch {
	case state == ColdOffline && laterUnreachable(m, cat):
		// The catalog's error is not an SSH one: word the newest check's.
		a := coldProblem(d.st, m, nil, ColdOffline)
		if older := d.olderFinding(cat); older != "" {
			a.Lead = a.lead() + older
			a.Reason += older
		}
		return a
	case state == pageRecheck:
		a := coldAdvice{retry: "重新检查"}
		a.Title = recheckText
		a.Reason = "这一页上次检查（" + d.when(cat.CheckedAt) + "）时连不上「" + name + "」，后来「" + homeTitle + "」页检查（" +
			d.when(d.st.Scans[m.ID].ScannedAt) + "）时连上了它。"
		a.Next = "点「重新检查 " + name + "」，看看它的对话现在能不能搬。"
		return a
	}
	a := coldProblem(d.st, m, cat, state)
	if clause := d.unknownClause(m, cat); clause != "" {
		for _, c := range []string{backendUnknownClause, noRepoUnknownClause} {
			a.Reason = strings.ReplaceAll(a.Reason, c, clause)
			a.Lead = strings.ReplaceAll(a.Lead, c, clause)
		}
	}
	return a
}

// olderFinding is what this page's own last check found when it reached the computer, as a
// sentence added after the newest check's 连不上; "" when it never reached it.
func (d draw) olderFinding(cat *ColdCatalog) string {
	state := cat.stateCode()
	if !reachedState(state) && state != ColdUnreadable {
		return ""
	}
	when := d.when(cat.CheckedAt)
	if state == ColdReady {
		return "上次连得上时（" + when + "检查）一切正常。"
	}
	return "上次连得上时（" + when + "检查）发现：" + cat.pageLabel() + "，连上以后还要处理。"
}

// unknownClause says how many of m's conversations a drive problem leaves 还没核对过, the count
// the top of the page includes them in, or that there are none.
func (d draw) unknownClause(m *Machine, cat *ColdCatalog) string {
	if cat == nil {
		return ""
	}
	n := 0
	for _, e := range cat.Entries {
		if cat.location(e, m, d.now) == LocUnknown {
			n++
		}
	}
	if n == 0 {
		return "不过这台电脑还没有搬到网盘的对话，所以没有对话受影响。"
	}
	return fmt.Sprintf("所以它搬到网盘的 %d 个对话暂时算在「%s」里，不会当成丢了。", n, uncheckedText)
}

// machineCounts is the 电脑 stat's detail: how many are ready, how many need something.
func machineCounts(ready, total int) string {
	switch {
	case total == 0:
		return "还没有电脑"
	case ready == total:
		return "都正常"
	case ready == 0:
		return fmt.Sprintf("%d 台要处理", total)
	}
	return fmt.Sprintf("%d 台正常 · %d 台要处理", ready, total-ready)
}

// brewButtonTitle is the AI 对话 page's Homebrew button: install, or upgrade a Homebrew install.
func brewButtonTitle(upgrade bool) string {
	if upgrade {
		return "用 Homebrew 升级配套小工具"
	}
	return "用 Homebrew 安装配套小工具"
}

// noBackendCommand sets up a network drive for the CLI; the commands stay exact.
const noBackendCommand = "disklessly backend list\ndisklessly backend add rclone <远端:路径>   # 也可以是 smb、nfs、webdav、openlist\ndisklessly cloud connect gdrive --use      # 或者直接连一个网盘"

// noBackendProblem is what a computer whose CLI has no network drive set up says, on its row and
// (m nil: the row's own computer) after an upload the CLI refused for that reason.
func noBackendProblem(m *Machine) problem {
	reason := "「" + machineLabel(m) + "」还没有设置网盘，旧对话没地方放，所以还不能搬。"
	next := "先在这台电脑上设置一个网盘（Google Drive 这类网盘、家里的 NAS 都行）：运行下面的命令，然后点「重新检查 " + machineLabel(m) + "」。"
	if m == nil {
		reason = "这台电脑还没有设置网盘，对话没地方放，这次一个都没有搬走或删除。"
		next = "先在这台电脑上设置一个网盘（Google Drive 这类网盘、家里的 NAS 都行）：运行下面的命令，设置好后点「重试」。"
	}
	p := problem{
		Title:   coldStateLabel(ColdNoBackend),
		Reason:  reason,
		Next:    next,
		Command: noBackendCommand,
	}
	if m != nil {
		p.Lead = reason + "设置网盘（Google Drive 这类网盘、家里的 NAS 都行）这一步页面上没有按钮能代劳，只能在「" + machineLabel(m) + "」上开终端做一次；设置好后点「重新检查 " + machineLabel(m) + "」。"
		p.CmdIntro = "在「" + machineLabel(m) + "」的终端里运行：先看已有的网盘，再加一个（选一种就行）。"
	}
	return p
}

// detailsKey names what the 详情 toggle of a part of the page opens.
func detailsKey(kind, id string) string { return kind + "-" + id }

// detailsItems is the 详情 toggle for raw (nothing without it) and, when open, the raw text.
func (d draw) detailsItems(key, raw string) []wire.Component {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if d.open != key {
		return []wire.Component{{Type: wire.ComponentButton, Title: "详情", Action: &wire.Action{ID: "details", Values: map[string]string{"k": key}}}}
	}
	return []wire.Component{
		{Type: wire.ComponentCode, Key: boundedKey("raw-", key), Path: rawBlockLabel, Text: clipRunes(raw, 4000)},
		{Type: wire.ComponentButton, Title: "收起详情", Action: &wire.Action{ID: "details", Values: map[string]string{"k": key}}},
	}
}

// The apps head a code block with its path, or with 文件 when it has none (and its line count):
// a command block names itself instead, so it does not read as a file. The path is only shown;
// nothing loads or links it without load or line_url.
const (
	commandBlockLabel = "终端命令"
	rawBlockLabel     = "原始信息"
)

// commandItem draws a command to copy.
func commandItem(key, command string) []wire.Component {
	if command == "" {
		return nil
	}
	return []wire.Component{{Type: wire.ComponentCode, Key: boundedKey("cmd-", key), Path: commandBlockLabel, Language: "sh", Text: command}}
}

// commandItems draws a command to copy, after the line that says where to run it and why.
func commandItems(key, intro, command string) []wire.Component {
	items := commandItem(key, command)
	if len(items) == 0 || intro == "" {
		return items
	}
	return append([]wire.Component{{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: intro}}, items...)
}

// machineStateRow builds the header row and advice for one machine.
func (d draw) machineStateRow(m *Machine, cat *ColdCatalog, state string) (wire.Component, coldAdvice) {
	name := machineLabel(m)
	row := wire.Component{
		Type:  wire.ComponentRow,
		Key:   boundedKey("mach-", m.ID),
		Icon:  "gauge",
		Title: clip(cmp.Or(strings.TrimSpace(name), strings.TrimSpace(m.ID), "没起名的电脑"), 200),
		Badge: &wire.Badge{Text: d.pageLabel(m, cat), Tone: coldStateTone(state)},
	}
	var sub []string
	if p := platformLabel(cmp.Or(catPlatform(cat), m.Platform)); p != "" {
		sub = append(sub, p)
	}
	if cat != nil && cat.CLI.Version != "" {
		sub = append(sub, "配套小工具 "+shortVersion(cat.CLI.Version))
	}
	if cat != nil && !cat.CheckedAt.IsZero() {
		sub = append(sub, d.when(cat.CheckedAt)+"检查")
	} else if cat != nil && !cat.RefreshedAt.IsZero() {
		sub = append(sub, d.when(cat.RefreshedAt)+"检查")
	}
	if laterUnreachable(m, cat) {
		// The 硬盘空间 page found it unreachable after this page's check (which reached it, or
		// never ran): the row is 连不上这台电脑, as there; say which check found it.
		when := "后来"
		if cat.stateCode() == ColdUnchecked {
			when = ""
		}
		if j, ok := d.jobs[m.ID]; ok && j.Kind == jobScan && j.Err != "" && !j.running() {
			when = d.when(j.Finished)
		}
		sub = append(sub, when+"「"+homeTitle+"」页检查时"+offlineText)
	}
	row.Subtitle = clip(strings.Join(sub, " · "), 400)

	adv := d.pageProblem(m, cat, state)
	if state == ColdReady {
		n, cloud, missing := 0, 0, 0
		for _, e := range cat.Entries {
			n++
			switch cat.location(e, m, d.now) {
			case LocCloud:
				cloud++
			case LocMissing:
				missing++
			}
		}
		b := cat.Backend
		row.Detail = fmt.Sprintf("一切正常，可以搬。记下 %d 个对话：%d 个在网盘里，%d 个网盘里找不到 · 网盘 %s 连得上（%d 毫秒）",
			n, cloud, missing, cmp.Or(b.ID, b.Type, "（没起名）"), b.LatencyMs)
		if len(cat.Missing) > 0 {
			row.Detail += fmt.Sprintf("；网盘里少了 %d 个数据包，里面的对话拿不回来", len(cat.Missing))
		}
		if len(cat.Unchecked) > 0 || cat.TimedOut {
			row.Detail += fmt.Sprintf("；还有 %d 个数据包没来得及核对", len(cat.Unchecked))
		}
	} else {
		row.Detail = adv.text()
	}
	row.Detail = clip(pageWords(row.Detail), 400)
	return row, adv
}

// machineStateCompactRow draws one machine as a single tappable summary row that switches the
// machine filter to m so its full commands and actions remain reachable within wire.ItemsMost.
func (d draw) machineStateCompactRow(m *Machine, cat *ColdCatalog, state string) wire.Component {
	row, _ := d.machineStateRow(m, cat, state)
	if j, ok := d.jobs[m.ID]; ok && j.running() {
		row.Badge = &wire.Badge{Text: "进行中", Tone: wire.ToneNeutral}
		row.Detail = clip(fmt.Sprintf("%s…（%s开始）", j.Label, d.when(j.Started)), 400)
	}
	// Encode m.ID in Action.ID when it fits within 128 runes so UIVersion 1-3 clients
	// (which do not send Action.Values) can also select the folded machine; keep Values for UIv4+ and >120-rune IDs.
	actionID := "machine"
	if utf8.RuneCountInString("machine:"+m.ID) <= 128 {
		actionID = "machine:" + m.ID
	}
	row.Action = &wire.Action{ID: actionID, Values: map[string]string{"machine": m.ID}}
	return row
}

// machineStateItems draws one machine's row (name, state, cause and next step) and the actions
// and command that fix it; raw output stays behind 详情.
func (d draw) machineStateItems(m *Machine, cat *ColdCatalog, state string) []wire.Component {
	name := machineLabel(m)
	row, adv := d.machineStateRow(m, cat, state)

	raw := adv.Raw
	// A computer that is not ready leads with what is wrong and what to do, in a line of its
	// own under its row; its buttons follow, and the commands come last, as a secondary step.
	var lead []wire.Component
	if state != ColdReady {
		row.Detail = ""
		lead = []wire.Component{{Type: wire.ComponentText, Text: clip(pageWords(adv.lead()), 600)}}
	}
	items := append([]wire.Component{row}, lead...)
	command, intro := adv.Command, adv.CmdIntro
	var last *coldJob
	if j, ok := d.jobs[m.ID]; ok {
		if j.running() {
			row.Badge = &wire.Badge{Text: "进行中", Tone: wire.ToneNeutral}
			// Only the running job's line: the lead names buttons that are hidden meanwhile.
			items = []wire.Component{row}
			items = append(items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary,
				Text: fmt.Sprintf("%s…（%s开始，完成后页面会自动更新）", j.Label, d.when(j.Started))})
			return items // no actions while it runs: a second start would be refused anyway
		}
		if j.Kind == jobScan && j.Err != "" && laterUnreachable(m, cat) {
			// The failed 硬盘空间 check is the problem the lead words; say it once.
		} else if j.Kind != jobRefresh {
			last = &j
			items = append(items, d.jobOutcome(j, retryFor(j, m, state, adv))...)
			if j.Raw != "" {
				raw = strings.TrimSpace(raw + "\n上次操作：" + j.Raw)
			}
			if j.Err != "" && j.Command != "" {
				command, intro = j.Command, "懂终端的话，可以在「"+name+"」上运行下面的命令："
			}
		}
	}
	// The buttons come right after the plain line (and the last job's outcome) that names them;
	// the command block, a step for people who use a terminal, comes after them.
	// The upload: offered on a ready computer only; a failed upload offers a real retry.
	switch {
	case last != nil && last.Kind == jobOffload && last.Err != "" && state == ColdReady:
		items = append(items, offloadButton(m, coldRetryText))
	case state == ColdReady:
		items = append(items, offloadButton(m, coldOffloadText))
	}
	if adv.brew {
		verb, cmd := "安装", brewInstallCmd
		if adv.upgrade {
			verb, cmd = "升级", brewUpgradeCmd
		}
		confirm := fmt.Sprintf("在「%s」上用 Homebrew %s配套小工具 disklessly？会在这台电脑上运行：%s。装好后先运行 disklessly version 确认能用，再重新检查这台电脑。", name, verb, cmd)
		if strings.HasPrefix(cmp.Or(catPlatform(cat), m.Platform), "darwin") {
			confirm += "在 Mac 上还会去掉 Homebrew 给这个没签名的程序加的隔离标记，不然远程登录时它运行不了。"
		}
		if cat != nil && cat.CLI.Path != "" && !brewManaged(cat.CLI) {
			confirm += "原来的 " + cat.CLI.Path + " 不会删；如果系统先找到的还是它，装完会提醒你。"
		}
		items = append(items, wire.Component{Type: wire.ComponentButton, Title: brewButtonTitle(adv.upgrade),
			Action: &wire.Action{ID: "cold-brew", Values: map[string]string{"m": m.ID}, Confirm: confirm}})
	}
	if adv.copy.src != nil {
		items = append(items, wire.Component{Type: wire.ComponentButton, Title: "从 " + machineLabel(adv.copy.src) + " 复制过来",
			Action: &wire.Action{ID: "cold-install", Values: map[string]string{"m": m.ID, "id": adv.copy.src.ID}, Confirm: installConfirm(adv.copy, m)}})
	}
	if adv.retry != "" {
		items = append(items, wire.Component{Type: wire.ComponentButton, Title: adv.retry + " " + name,
			Action: &wire.Action{ID: "cold-refresh", Values: map[string]string{"m": m.ID}}})
	}
	if adv.docs {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: "怎么装配套小工具", URL: coldInstallDocsURL})
	}
	// 详情 goes with the explanation and its buttons; the command block, for people who use a
	// terminal, comes last.
	items = append(items, d.detailsItems(detailsKey("cold", m.ID), raw)...)
	return append(items, commandItems(detailsKey("cold", m.ID), intro, command)...)
}

// offloadButton starts machine m's cold-session upload, behind the confirm that says what moves,
// what is checked and what is deleted.
func offloadButton(m *Machine, title string) wire.Component {
	return wire.Component{Type: wire.ComponentButton, Key: boundedKey("offload-", m.ID), Title: title,
		Action: &wire.Action{ID: "cold-offload", Values: map[string]string{"m": m.ID}, Confirm: sessionOffloadConfirm(m)}}
}

// retryHint is how a failed job is tried again from the page: the title of a button that the
// row really draws, or else a sentence saying where to do it.
type retryHint struct{ button, how string }

// retryFor names, for the failed job j on machine m, the button under its row that runs the same
// work again; only buttons machineStateItems draws for m's state are named. A restore is retried
// from its conversation's row, and a file offload from Persona's disk usage map.
func retryFor(j coldJob, m *Machine, state string, adv coldAdvice) retryHint {
	fallback := retryHint{}
	if adv.retry != "" {
		fallback.button = adv.retry + " " + machineLabel(m)
	}
	switch j.Kind {
	case jobOffload:
		if state == ColdReady {
			return retryHint{button: coldRetryText}
		}
	case jobRestore:
		return retryHint{how: "想再试一次：在下面找到这个对话，点「拿回来」。"}
	case jobInstall:
		if adv.copy.src != nil {
			return retryHint{button: "从 " + machineLabel(adv.copy.src) + " 复制过来"}
		}
	case jobBrew:
		if adv.brew {
			return retryHint{button: brewButtonTitle(adv.upgrade)}
		}
	case jobFileOffload, jobFileRestore:
		return retryHint{how: "想再试一次：回到 Persona 的磁盘占用图，重新选中它们。"}
	}
	return fallback
}

// jobOutcome is the line under a machine saying how its last job ended. A failure's 「重试」 names
// the button retry gives, so the text never points at a button that is not there (or at one
// that does something else).
func (d draw) jobOutcome(j coldJob, retry ...retryHint) []wire.Component {
	text := j.Note
	if j.Err != "" {
		text = "没有成功：" + j.Err
		if len(retry) > 0 {
			text = retry[0].apply(text)
		}
	}
	if text == "" {
		return nil
	}
	return []wire.Component{{Type: wire.ComponentText, Style: wire.TextStyleSecondary,
		Text: clip(fmt.Sprintf("%s · %s", d.when(j.Finished), pageWords(text)), 400)}}
}

// apply rewrites the generic 「重试」 in text to the button r names, or, with no such button,
// drops the button and adds r.how.
func (r retryHint) apply(text string) string {
	text = r.rename(text)
	if r.button == "" {
		if r.how != "" {
			text = strings.TrimRight(text, "。；") + "。" + r.how
		}
		return text
	}
	if !strings.Contains(text, r.button) {
		text = strings.TrimRight(text, "。；") + "。处理好后点「" + r.button + "」。"
	}
	return text
}

// rename only rewrites the generic 「重试」 in text to the button r names, or drops it.
func (r retryHint) rename(text string) string {
	if r.button == "" {
		text = strings.ReplaceAll(text, "修好后点「重试」", "修好后再试")
		text = strings.ReplaceAll(text, "然后点「重试」", "然后再试")
		return strings.ReplaceAll(text, "点「重试」", "再试一次")
	}
	return strings.ReplaceAll(text, "「重试」", "「"+r.button+"」")
}

// sandboxSection is the page's first section while the extension runs in the hub's sandbox.
func sandboxSection() []wire.Section {
	if !sandboxed() {
		return nil
	}
	p := sandboxProblem()
	return []wire.Section{{Key: "sandbox", Title: "Disklessly 被关在隔离环境（沙箱）里，连不上其他电脑", Items: append(
		[]wire.Component{{Type: wire.ComponentText, Text: p.text()}}, commandItem("sandbox", p.Command)...)}}
}

func catPlatform(c *ColdCatalog) string {
	if c == nil {
		return ""
	}
	return c.Platform
}

// noProjectText is what a conversation's row says when the index knows no project for it.
const noProjectText = "没归到项目"

// untitledText starts the title of a conversation that has neither a title nor a first message.
const untitledText = "没有标题的对话"

// promptTitleRunes is how much of the first message a title made from it keeps.
const promptTitleRunes = 50

var idLikeRE = regexp.MustCompile(`^(?i:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9a-f]{12,})$`)

// realTitle is e's title when it is one a person wrote or the agent made up: not empty, and
// not an id (the CLI's entry id, the session id or a UUID like one) standing in for a title.
func realTitle(e ColdEntry) string {
	t := strings.TrimSpace(e.Title)
	if t == "" || t == e.ID || t == e.SessionID || idLikeRE.MatchString(t) || strings.HasPrefix(t, "_") {
		return ""
	}
	return t
}

// A first message says what a conversation is about only when it has some content: "reply OK",
// "hi", "继续", a bare /command or a single word make the same title for many conversations.
// fillerLatin and fillerHan are the words that carry none on their own (Latin words compared in
// lower case; a run of Chinese counts as filler when it is made of these pieces only).
var (
	fillerLatin = map[string]bool{
		"ok": true, "okay": true, "k": true, "kk": true, "yes": true, "yep": true, "yeah": true, "y": true, "no": true, "nope": true,
		"hi": true, "hello": true, "hey": true, "yo": true, "test": true, "testing": true, "ping": true, "pong": true,
		"thanks": true, "thank": true, "you": true, "thx": true, "ty": true, "continue": true, "go": true, "on": true, "next": true,
		"reply": true, "say": true, "with": true, "please": true, "pls": true, "plz": true, "again": true, "retry": true,
		"done": true, "sure": true, "start": true, "hmm": true, "lol": true, "ack": true,
	}
	fillerHan = []string{"继续", "接着", "好的", "好", "嗯", "哦", "是的", "是", "对的", "对", "可以", "行", "你好", "您好", "在吗",
		"测试", "谢谢", "开始", "再来", "重试", "收到", "请", "吧", "啊", "呢", "呀", "一下", "回复"}
	slashCommandRE = regexp.MustCompile(`^/[A-Za-z][\w:.-]*$`)
)

// promptHasContent reports whether a first message (on one line) can stand for its conversation:
// after a leading /command, its words that are not filler count one each (Latin words, numbers
// not counted) and its Chinese, Japanese or Korean characters one each per two; it needs 2.
func promptHasContent(p string) bool {
	f := strings.Fields(p)
	if len(f) > 0 && slashCommandRE.MatchString(f[0]) {
		f = f[1:]
	}
	score := 0.0
	var word, han []rune
	flush := func() {
		if len(word) > 0 {
			w := strings.ToLower(string(word))
			if !fillerLatin[w] && strings.TrimFunc(w, unicode.IsDigit) != "" {
				score++
			}
		}
		if len(han) > 0 && !fillerOnly(string(han)) {
			score += float64(len(han)) / 2
		}
		word, han = word[:0], han[:0]
	}
	for _, r := range strings.Join(f, " ") {
		switch {
		case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
			if len(word) > 0 {
				flush()
			}
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '_':
			if len(han) > 0 {
				flush()
			}
			word = append(word, r)
		default:
			flush()
		}
	}
	flush()
	return score >= 2
}

// fillerOnly reports a run of Chinese made of fillerHan pieces only.
func fillerOnly(s string) bool {
	if s == "" {
		return true
	}
	for _, w := range fillerHan {
		if rest, ok := strings.CutPrefix(s, w); ok && fillerOnly(rest) {
			return true
		}
	}
	return false
}

// promptTitle cuts a first message to a title: up to its first sentence end when that comes
// early enough, and at most promptTitleRunes runes; "" when the message has no content to stand
// for the conversation (promptHasContent). The index keeps the message on one line.
func promptTitle(p string) string {
	p = oneLine(p)
	if p == "" || idLikeRE.MatchString(p) || !promptHasContent(p) {
		return ""
	}
	r := []rune(p)
	for i, c := range r {
		if i >= promptTitleRunes {
			break
		}
		if i >= 8 && strings.ContainsRune("。！？!?", c) {
			return string(r[:i+1])
		}
	}
	return clipRunes(p, promptTitleRunes)
}

// coldProject is the project a conversation belongs to as the index knows it (its project, or
// the last folder of its working folder), or "" when it does not know: the CLI writes
// "_unknown" (and the like) for a conversation it could not place.
func coldProject(e ColdEntry) string {
	if p := strings.TrimSpace(e.Project); p != "" && !strings.HasPrefix(p, "_") {
		return p
	}
	return strings.TrimSpace(lastPathElem(e.Cwd))
}

// coldTool is the AI assistant a conversation was held with, by its tool or its file's place.
func coldTool(e ColdEntry) string { return toolLabel(cmp.Or(e.Tool, toolFromPath(e.Path))) }

// shortDate words t as 11月8日 (with the year when it is not now's year): what when says of a
// time a week ago or more.
func (d draw) shortDate(t time.Time) string {
	t = t.In(d.loc)
	if t.Year() != d.now.In(d.loc).Year() {
		return fmt.Sprintf("%d年%d月%d日", t.Year(), int(t.Month()), t.Day())
	}
	return fmt.Sprintf("%d月%d日", int(t.Month()), t.Day())
}

// coldTitle is the title a conversation's row shows, never an id: its real title, else the
// start of its first message when that has content, else 没有标题的对话 with its date, time and
// AI assistant (the index holds nothing else that names a conversation; its keywords are single
// frequent words). fromPrompt reports a title cut from the first message.
func (d draw) coldTitle(e ColdEntry) (title string, fromPrompt bool) {
	if t := realTitle(e); t != "" {
		return t, false
	}
	if t := promptTitle(e.FirstPrompt); t != "" {
		return t, true
	}
	parts := []string{untitledText}
	if t := entryTime(e); !t.IsZero() {
		// With the time of day: several untitled conversations of one day stay apart.
		parts = append(parts, d.stamp(t))
	}
	if tool := coldTool(e); tool != "" {
		parts = append(parts, tool)
	}
	return strings.Join(parts, " · "), false
}

// coldID is the id a conversation is restored and resumed by, shown as a secondary detail.
func coldID(e ColdEntry) string { return strings.TrimSpace(cmp.Or(e.SessionID, e.ID)) }

func (d draw) coldRow(h ColdHit) wire.Component {
	sub := []string{h.MachineName, cmp.Or(coldTool(h.ColdEntry), "AI 助手"), cmp.Or(coldProject(h.ColdEntry), noProjectText)}
	// The working folder too when the project named is another one: what the index knows that
	// tells rows of one project and day apart, with the time and the size.
	if dir := strings.TrimSpace(lastPathElem(h.Cwd)); dir != "" && dir != coldProject(h.ColdEntry) {
		sub = append(sub, "文件夹 "+dir)
	}
	title, fromPrompt := d.coldTitle(h.ColdEntry)
	// An untitled conversation's title already has its date and time.
	if t := entryTime(h.ColdEntry); !t.IsZero() && !strings.HasPrefix(title, untitledText) {
		sub = append(sub, d.stamp(t))
	}
	if h.Bytes > 0 {
		sub = append(sub, FormatBytes(h.Bytes))
	}
	if !h.Local {
		sub = append(sub, "来自另一台电脑")
	}
	// The detail: what a deep search found, or the first message unless the title already is
	// all of it (named as such when the title is not taken from it, so a short one such as
	// "reply OK" is still seen); then the id (search finds it, and restore commands need it).
	text := strings.TrimSpace(h.Preview)
	if prompt := oneLine(h.FirstPrompt); text == "" && prompt != "" && (!fromPrompt || prompt != title) {
		text = prompt
		if !fromPrompt {
			text = "第一句话：" + prompt
		}
	}
	detail := ""
	if id := coldID(h.ColdEntry); id != "" {
		detail = "对话编号 " + id
	}
	if text != "" {
		if detail == "" {
			detail = clip(text, 240)
		} else {
			detail = clip(text, 240-utf8.RuneCountInString(detail)-3) + " · " + detail
		}
	}
	row := wire.Component{
		Type:     wire.ComponentRow,
		Key:      boundedKey("cold-", h.MachineID+"-"+h.ID),
		Icon:     "chat",
		Title:    clip(title, 120),
		Subtitle: clip(strings.Join(sub, " · "), 400),
		Detail:   clip(detail, 240),
		Badge:    &wire.Badge{Text: locationText(h.Location), Tone: locationTone(h.Location)},
	}
	// Another device's row is not restored here: its original path belongs to that device.
	if h.Location != LocLocal && h.Local {
		confirm := fmt.Sprintf("把「%s」从网盘拿回来，放回原来的位置 %s？", clip(title, 60), cmp.Or(h.Path, "（原来的位置）"))
		if hint := resumeHint(h.ColdEntry); hint != "" {
			confirm += "拿回来后可以运行 " + hint + " 接着聊。"
		}
		row.Actions = []wire.RowAction{{Title: "拿回来", Swipe: true, Action: wire.Action{
			ID:      "cold-restore",
			Values:  map[string]string{"m": h.MachineID, "id": h.ID},
			Confirm: confirm,
		}}}
	}
	return row
}

// toggle opens key's 详情, or closes it when it is the one open.
func toggle(open, key string) string {
	if open == key {
		return ""
	}
	return clipRunes(key, 100)
}

func lastPathElem(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// coldUIBudget bounds a deep search run inside a UI request (the hub allows about 12 seconds).
const coldUIBudget = 8 * time.Second

// coldDeep runs a deep search on the machines in parallel, within coldUIBudget and without
// holding the store lock while they answer.
func (a *app) coldDeep(ctx context.Context, machine, query string) (*deepResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("请先输入要搜的内容")
	}
	if machine == allMachines {
		machine = ""
	}
	runner := a.run()
	var ms []*Machine
	if err := a.db().read(func(st *State) {
		for _, m := range deepTargets(st, machine) {
			mc := *m
			ms = append(ms, &mc)
		}
	}); err != nil {
		return nil, err
	}
	var runs map[string]deepRun
	if words := deepSearchWords(query); len(words) > 0 {
		runs = deepSearchMachines(ctx, runner, ms, words, coldShown, coldUIBudget)
	}
	var res deepResult
	err := a.db().read(func(st *State) {
		res.hits, res.errs = mergeDeepSearch(st, query, machine, coldShown, a.now(), runs)
	})
	return &res, err
}

// ---- AI 对话 nav page ----

func (a *app) renderSessions(_ context.Context, r *ext.UIRequest) (*wire.View, error) {
	var p sessPage
	if err := r.DecodeState(&p); err != nil {
		return nil, &ext.Error{Code: "invalid", Message: pageStateLost}
	}
	return a.sessionsView(r, p, nil)
}

func (a *app) sessionsView(r *ext.UIRequest, p sessPage, deep *deepResult) (*wire.View, error) {
	var v *wire.View
	err := a.db().read(func(st *State) {
		d := a.drawer(st, r)
		d.open = p.Open
		v = d.sessionsPage(p, deep)
	})
	return v, err
}

func (d draw) sessionsFallbackSummary(m *Machine, query string) string {
	scope := "全部电脑"
	machineArg := ""
	if m != nil {
		scope = machineLabel(m)
		machineArg = m.ID
	}
	hits := d.pageSearch(clipQuery(query), machineArg, 5)
	var lines []string
	lines = append(lines, fmt.Sprintf("Disklessly AI 对话（%s，找到 %d 个对话）", scope, len(hits)))
	for _, mm := range d.st.Machines {
		if (m == nil && mm.Enabled) || (m != nil && mm.ID == m.ID) {
			cat := d.st.cold(mm.ID)
			lines = append(lines, fmt.Sprintf("- %s：%s", machineLabel(mm), d.pageLabel(mm, cat)))
		}
	}
	return clip(strings.Join(lines, "\n"), 1950)
}

func (d draw) sessionsPage(p sessPage, deep *deepResult) *wire.View {
	var m *Machine
	if p.Machine != "" && p.Machine != allMachines {
		if m = d.st.machine(p.Machine); m == nil || !m.Enabled {
			m, p.Machine = nil, ""
		}
	}
	const maxFilterChoices = 100
	choices := []wire.Choice{{Value: allMachines, Title: "全部电脑"}}
	selectedIncluded := m == nil
	for _, mm := range d.st.Machines {
		if !mm.Enabled {
			continue
		}
		if len(choices) >= maxFilterChoices {
			break
		}
		choices = append(choices, wire.Choice{Value: mm.ID, Title: chipFit(machineLabel(mm), chipUnits)})
		if m != nil && mm.ID == m.ID {
			selectedIncluded = true
		}
	}
	if !selectedIncluded && m != nil && len(choices) > 1 {
		choices[len(choices)-1] = wire.Choice{Value: m.ID, Title: chipFit(machineLabel(m), chipUnits)}
	}
	value := allMachines
	if m != nil {
		value = m.ID
	}
	var filter *wire.Filter
	if len(choices) >= 2 {
		filter = &wire.Filter{Name: "machine", Title: "电脑", Choices: choices, Value: value, Action: wire.Action{ID: "machine"}}
	}
	var lead []wire.Section
	if d.v4() {
		lead = sandboxSection()
	} else if sandboxed() {
		sp := sandboxProblem()
		lead = []wire.Section{{Title: sp.Title, Items: []wire.Component{{Type: wire.ComponentText, Text: sp.message()}}}}
	}
	return d.fitVersion(&wire.View{
		Title:        sessionsTitle,
		Fallback:     d.sessionsFallbackSummary(m, p.Query),
		State:        ext.EncodeState(p),
		RefreshAfter: d.refreshAfter(),
		Search:       &wire.Search{Placeholder: coldSearchHint, Value: clipQuery(p.Query), Action: wire.Action{ID: "search"}},
		Filter:       filter,
		Sections:     append(lead, d.coldSections(m, p.Query, deep, wire.ItemsMost-sectionItemCount(lead))...),
	})
}

func (a *app) actionSessions(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	var p sessPage
	if err := r.DecodeState(&p); err != nil {
		return nil, &ext.Error{Code: "invalid", Message: pageStateLost}
	}
	res := &wire.ActionResult{}
	var deep *deepResult
	switch {
	case r.Action == "machine":
		p.Machine = r.Values["machine"]
	case strings.HasPrefix(r.Action, "machine:"):
		p.Machine = cmp.Or(r.Values["machine"], strings.TrimPrefix(r.Action, "machine:"))
	case r.Action == "search":
		p.Query = clipQuery(r.Values["q"])
	case r.Action == "details":
		p.Open = toggle(p.Open, r.Values["k"])
	case r.Action == "cold-deep":
		p.Query = clipQuery(cmp.Or(strings.TrimSpace(r.Values["q"]), p.Query))
		var err error
		if deep, err = a.coldDeep(ctx, cmp.Or(r.Values["m"], p.Machine), p.Query); err != nil {
			return nil, &ext.Error{Code: "invalid", Message: userMessage(err)}
		}
	default:
		toast, err := a.coldAction(ctx, r.Action, cmp.Or(r.Values["m"], p.Machine), r.Values["id"])
		if err != nil {
			return nil, &ext.Error{Code: "invalid", Message: userMessage(err)}
		}
		res.Toast = toast
	}
	var err error
	res.View, err = a.sessionsView(r, p, deep)
	return res, err
}
