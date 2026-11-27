package main

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/wire"
)

// Short X-axis labels sized so all six category columns stay visible without thinning on phone screens (>=360px, plotW >=255px).
var CategoryAxisLabels = map[string]string{
	CatGoCache:          "Go",
	CatNodePythonCache:  "Node/Py",
	CatDockerUnused:     "容器",
	CatRustXcodeBuild:   "Rust/XC",
	CatTmpLogs:          "日志",
	CatColdAgentSession: "旧对话",
}

// The breakdown table names the kinds by these same short labels, so the chart and the table
// above and below each other say one set of names, spelled out once (categoryLegend).

// CategoryPlainNames are the kinds as the pages name them wherever a whole line has room (rows,
// check boxes, confirms, the clean-up record, the plain summaries). CategoryLabels stays as it
// is: MCP answers carry it.
var CategoryPlainNames = map[string]string{
	CatGoCache:          "Go 的编译和下载缓存",
	CatNodePythonCache:  "Node 和 Python 下载的包",
	CatDockerUnused:     "Docker 没在用的镜像和构建缓存",
	CatRustXcodeBuild:   "Rust 和 Xcode 的编译缓存",
	CatTmpLogs:          "系统日志和过期的临时文件",
	CatColdAgentSession: "一周没动的 AI 对话",
}

// categoryName is a kind's plain name, or its id when it has none.
func categoryName(id string) string { return cmp.Or(CategoryPlainNames[id], CategoryLabels[id], id) }

// legendCategories are the kinds whose short label in a narrow chart column or table cell does
// not say what it is by itself.
var legendCategories = []string{CatNodePythonCache, CatRustXcodeBuild, CatDockerUnused, CatTmpLogs}

// categoryLegend spells out the short labels (from labels) of the kinds in ids that need it,
// in CategoryIDs order; "" when none does.
func categoryLegend(labels map[string]string, ids []string) string {
	var parts []string
	for _, cid := range legendCategories {
		if slices.Contains(ids, cid) && labels[cid] != "" {
			name := CategoryPlainNames[cid]
			if name != "" && name[0] < utf8.RuneSelf {
				name = " " + name // a space before a Latin word, as the pages write it
			}
			parts = append(parts, "「"+labels[cid]+"」是"+name)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "简称：" + strings.Join(parts, "，") + "。"
}

// platformLabel words a computer's system and chip (parsePlatformString: linux-arm64,
// darwin-amd64…) for people: Mac or Linux 电脑, with the chip in brackets; "" for none, and an
// unknown system as it was reported.
func platformLabel(p string) string {
	p = strings.TrimSpace(p)
	sys, arch, _ := strings.Cut(p, "-")
	var name string
	switch sys {
	case "darwin":
		name = "Mac"
	case "linux":
		name = "Linux 电脑"
	default:
		return p
	}
	switch arch {
	case "arm64":
		if sys == "darwin" {
			return name + "（Apple 芯片）"
		}
		return name + "（ARM 芯片）"
	case "amd64":
		if sys == "darwin" {
			return name + "（Intel 芯片）"
		}
		return name + "（Intel 或 AMD 芯片）"
	}
	return name
}

// estTextWidthPx mirrors Persona web's labelWidth(text, px) in web/src/lib/extension-ui.ts.
func estTextWidthPx(text string, px float64) float64 {
	var w float64
	for _, r := range text {
		if (r >= 0x2e80 && r <= 0x9fff) || (r >= 0xac00 && r <= 0xd7af) || (r >= 0xff00 && r <= 0xffef) {
			w += px
		} else {
			w += px * 0.6
		}
	}
	return w
}

// clipCellWidth bounds a table cell by both rune count and pixel width so 4-column tables fit 360-390px phones without horizontal overflow.
func clipCellWidth(s string, fontPx, maxPx float64, maxRunes int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxRunes && estTextWidthPx(s, fontPx) <= maxPx {
		return s
	}
	rs := []rune(s)
	limit := min(len(rs), max(1, maxRunes-1))
	ellipsisW := fontPx * 0.6
	var cur float64
	keep := 0
	for keep < limit {
		rw := fontPx * 0.6
		r := rs[keep]
		if (r >= 0x2e80 && r <= 0x9fff) || (r >= 0xac00 && r <= 0xd7af) || (r >= 0xff00 && r <= 0xffef) {
			rw = fontPx
		}
		if cur+rw+ellipsisW > maxPx && keep > 0 {
			break
		}
		cur += rw
		keep++
	}
	return strings.TrimSpace(string(rs[:keep])) + "…"
}

func compactBytes(b uint64) string {
	const (
		tiB = 1024 * gib
		kiB = 1024
	)
	switch {
	case b >= tiB:
		return fmt.Sprintf("%.2fT", float64(b)/float64(tiB))
	case b >= gib:
		return fmt.Sprintf("%.1fG", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.1fM", float64(b)/float64(mib))
	case b >= kiB:
		return fmt.Sprintf("%.0fK", float64(b)/float64(kiB))
	case b > 0:
		return fmt.Sprintf("%dB", b)
	default:
		return "0B"
	}
}

func compactCountBytes(count int, b uint64) string {
	if count == 0 && b == 0 {
		return "0"
	}
	return fmt.Sprintf("%d·%s", count, compactBytes(b))
}

func compactAvailChange(h *CleanRecord) string {
	if h.BeforeAvail > 0 || h.AfterAvail > 0 {
		if h.BeforeAvail >= gib && h.AfterAvail >= gib {
			return fmt.Sprintf("%.1f→%.1fG", float64(h.BeforeAvail)/float64(gib), float64(h.AfterAvail)/float64(gib))
		}
		return fmt.Sprintf("%s→%s", compactBytes(h.BeforeAvail), compactBytes(h.AfterAvail))
	}
	return clipCellWidth(h.Summary, 12.5, 67.5, 12)
}

func compactFreedCell(freed uint64) string {
	if freed == 0 {
		return "—"
	}
	return "+" + compactBytes(freed)
}

// localMachineCell is 运行 Persona 的电脑 in the 清理记录 table's 电脑 cell (48px on a 360px
// phone), where clipping would leave 「运行 P…」; the record rows below the table spell it out.
const localMachineCell = "本机"

// histMachineCell is the computer in the 清理记录 table's narrow 电脑 column.
func histMachineCell(st *State, h *CleanRecord) string {
	label := histMachineLabel(st, h)
	if label == localMachineName {
		return localMachineCell
	}
	return clipCellWidth(label, 12.5, 48.0, 10)
}

func histMachineLabel(st *State, h *CleanRecord) string {
	if st != nil {
		if m := st.machine(h.MachineID); m != nil {
			return machineLabel(m)
		}
	}
	name := strings.TrimSpace(h.MachineName)
	if i := strings.Index(name, " ("); i > 0 {
		name = strings.TrimSpace(name[:i])
	}
	return cmp.Or(name, h.MachineID)
}

func compactUsedTotalLabel(usedBytes, totalBytes uint64) string {
	uStr := FormatBytes(usedBytes)
	tStr := FormatBytes(totalBytes)
	uParts := strings.SplitN(uStr, " ", 2)
	tParts := strings.SplitN(tStr, " ", 2)
	if len(uParts) == 2 && len(tParts) == 2 && uParts[1] == tParts[1] {
		withPrefix := fmt.Sprintf("已用 %s / %s %s", uParts[0], tParts[0], tParts[1])
		if estTextWidthPx(withPrefix, 13.0) <= 176 {
			return withPrefix
		}
		return fmt.Sprintf("%s / %s %s", uParts[0], tParts[0], tParts[1])
	}
	withPrefix := fmt.Sprintf("已用 %s / %s", uStr, tStr)
	if estTextWidthPx(withPrefix, 13.0) <= 176 {
		return withPrefix
	}
	return fmt.Sprintf("%s / %s", uStr, tStr)
}

type categoryBreakdown struct {
	safe        map[string]uint64
	review      map[string]uint64
	cold        map[string]uint64
	counts      map[string]int
	totalSafe   uint64
	totalReview uint64
	totalCold   uint64
	totalShown  uint64
	maxCategory uint64
	coldPending bool
}

func summarizeCategories(items []ScanItem, sess *SessionSummary) categoryBreakdown {
	bd := categoryBreakdown{
		safe:   make(map[string]uint64, len(CategoryIDs)),
		review: make(map[string]uint64, len(CategoryIDs)),
		cold:   make(map[string]uint64, len(CategoryIDs)),
		counts: make(map[string]int, len(CategoryIDs)),
	}
	for _, it := range items {
		if it.InUse {
			continue
		}
		bd.counts[it.Category]++
		switch {
		case it.Category == CatColdAgentSession:
			bd.cold[it.Category] += it.Bytes
			bd.totalCold += it.Bytes
		case it.Safety == SafetyReview:
			bd.review[it.Category] += it.Bytes
			bd.totalReview += it.Bytes
		default:
			bd.safe[it.Category] += it.Bytes
			bd.totalSafe += it.Bytes
		}
	}
	if bd.cold[CatColdAgentSession] == 0 && sess != nil && sess.ColdBytes > 0 {
		bd.cold[CatColdAgentSession] = sess.ColdBytes
		bd.totalCold += sess.ColdBytes
		bd.counts[CatColdAgentSession] = sess.ColdCount
		bd.coldPending = true
	}
	bd.totalShown = bd.totalSafe + bd.totalReview + bd.totalCold
	for _, cid := range CategoryIDs {
		tot := bd.catTotal(cid)
		if tot > bd.maxCategory {
			bd.maxCategory = tot
		}
	}
	return bd
}

func (bd categoryBreakdown) catTotal(cid string) uint64 {
	return bd.safe[cid] + bd.review[cid] + bd.cold[cid]
}

func (bd categoryBreakdown) catModeText(cid string) string {
	tot := bd.catTotal(cid)
	if tot == 0 {
		return "没有"
	}
	n := bd.counts[cid]
	if cid == CatColdAgentSession {
		if bd.coldPending {
			if n > 0 {
				return fmt.Sprintf("缺小工具·%d", n)
			}
			return "缺小工具"
		}
		if n > 0 {
			return fmt.Sprintf("搬网盘·%d项", n)
		}
		return "搬到网盘"
	}
	switch {
	case bd.safe[cid] > 0 && bd.review[cid] > 0:
		return fmt.Sprintf("挑着删·%d项", n)
	case bd.review[cid] > 0:
		return fmt.Sprintf("先看·%d项", n)
	default:
		return fmt.Sprintf("放心删·%d项", n)
	}
}

// The fullness bands of EvaluateHealth in the words healthLevelText gives them, for the pages'
// legends: fullnessLegendShort for a stat's detail, fullnessLegend where a whole sentence fits.
// A computer put in a band by its free space alone says so on its own bar (healthWatermarkNote).
const (
	fullnessLegendShort = "用到 80% 算偏满 · 90% 算快满了（剩得太少也算）"
	fullnessLegend      = "用到 80% 算偏满、90% 算快满了；硬盘不小的话，只剩不到 25 GiB 也算偏满、不到 10 GiB 也算快满了"
)

func healthLevelText(level string) string {
	switch level {
	case HealthCritical:
		return "快满了"
	case HealthWarning:
		return "偏满"
	case HealthOffline:
		return "连不上"
	case HealthUnscanned:
		return "还没检查"
	default:
		return "宽裕"
	}
}

// healthWatermarkNote adds the free-space floor reason when a machine stays in warning (<25 GiB free on >=50 GiB disk)
// or critical (<10 GiB free on >=20 GiB disk) despite its percentage being below 80% or 90%.
func healthWatermarkNote(level string, usedPct float64, totalBytes, availBytes uint64) string {
	base := healthLevelText(level)
	if level == HealthCritical && usedPct < 90.0 && totalBytes >= 20*gib && availBytes < 10*gib {
		return fmt.Sprintf("%s，只剩 %s，不到 10 GiB", base, FormatBytes(availBytes))
	}
	if level == HealthWarning && usedPct < 80.0 && totalBytes >= 50*gib && availBytes < 25*gib {
		return fmt.Sprintf("%s，只剩 %s，不到 25 GiB", base, FormatBytes(availBytes))
	}
	return base
}

func watermarkChartRank(level string) int {
	switch level {
	case HealthCritical:
		return 0
	case HealthWarning:
		return 1
	case HealthOK:
		return 2
	case HealthOffline:
		return 3
	default:
		return 4
	}
}

func chartUnit(maxBytes uint64) string {
	if maxBytes >= gib {
		return "GiB"
	}
	return "MiB"
}

func chartValue(b uint64, unit string) float64 {
	if b == 0 {
		return 0
	}
	div := float64(mib)
	if unit == "GiB" {
		div = float64(gib)
	}
	v := math.Round(float64(b)/div*1000) / 1000
	if v == 0 {
		return 0.001
	}
	return v
}

// fullnessLevel is how full ms's system disk is by its last check, whether or not the computer
// answers now: a bar's colour and its words say only this. A computer that cannot be reached
// and was never checked stays HealthOffline (nothing known about its disk).
func fullnessLevel(ms MachineStatus) string {
	if ms.Level == HealthOffline && ms.TotalBytes > 0 {
		return EvaluateHealth(true, ms.TotalBytes, ms.AvailableBytes)
	}
	return ms.Level
}

// lastKnown reports a computer that cannot be reached now but whose disk was checked before:
// its numbers are the last check's.
func lastKnown(ms MachineStatus) bool { return ms.Level == HealthOffline && ms.TotalBytes > 0 }

func machineProgressComponent(ms MachineStatus) wire.Component {
	ratio := math.Max(0, math.Min(1, ms.UsedPercent/100.0))
	level := fullnessLevel(ms)
	detail := fmt.Sprintf("%.1f%% · %s", ms.UsedPercent, healthWatermarkNote(level, ms.UsedPercent, ms.TotalBytes, ms.AvailableBytes))
	if lastKnown(ms) {
		// The row above says it cannot be reached now and when these numbers are from.
		detail = "上次 " + detail
	}
	return wire.Component{
		Type:   wire.ComponentProgress,
		Key:    boundedKey("prog-", ms.Machine.ID),
		Label:  compactUsedTotalLabel(ms.UsedBytes, ms.TotalBytes),
		Detail: detail,
		Ratio:  &ratio,
		Tone:   progressTone(level),
	}
}

// rootUsageMeter is a computer's system disk as one compact bar (used of total, how full, what
// is left), where no other bar on the view shows it: the plan tab and the overview card.
func rootUsageMeter(ms MachineStatus, keyPrefix string) wire.Component {
	c := machineProgressComponent(ms)
	c.Key = boundedKey(keyPrefix+"-root-", ms.Machine.ID)
	return c
}

// reclaimCategoryChart builds a 6-category stacked bar chart (Go, Node/Py, 容器, Rust/XC, 日志, 冷会话)
// split across up to 3 safety/offload series (safe, review, cold sessions).
func reclaimCategoryChart(m *Machine, bd categoryBreakdown, keyPrefix string) (wire.Component, bool) {
	if bd.totalShown == 0 {
		return wire.Component{}, false
	}
	unit := chartUnit(bd.maxCategory)
	ptsFor := func(src map[string]uint64) []wire.ChartPoint {
		pts := make([]wire.ChartPoint, 0, len(CategoryIDs))
		for _, cid := range CategoryIDs {
			b := src[cid]
			pts = append(pts, wire.ChartPoint{
				Label:   CategoryAxisLabels[cid],
				Value:   chartValue(b, unit),
				Display: FormatBytes(b),
			})
		}
		return pts
	}

	var series []wire.ChartSeries
	var parts []string
	if bd.totalSafe > 0 {
		series = append(series, wire.ChartSeries{
			Name:   "放心删",
			Tone:   wire.TonePositive,
			Points: ptsFor(bd.safe),
		})
		parts = append(parts, "放心删 "+FormatBytes(bd.totalSafe))
	}
	if bd.totalReview > 0 {
		series = append(series, wire.ChartSeries{
			Name:   "建议先看",
			Tone:   wire.ToneCaution,
			Points: ptsFor(bd.review),
		})
		parts = append(parts, "建议先看 "+FormatBytes(bd.totalReview))
	}
	if bd.totalCold > 0 {
		coldName := "搬到网盘"
		if bd.coldPending {
			coldName = "搬到网盘（缺小工具）"
		}
		series = append(series, wire.ChartSeries{
			Name:   coldName,
			Color:  "blue",
			Points: ptsFor(bd.cold),
		})
		parts = append(parts, coldName+" "+FormatBytes(bd.totalCold))
	}

	return wire.Component{
		Type:    wire.ComponentChart,
		Key:     boundedKey(keyPrefix+"-cats-", m.ID),
		Kind:    wire.ChartKindBar,
		Stacked: true,
		Title:   "能腾出的空间，按种类",
		Unit:    unit,
		Series:  series,
		Detail:  strings.TrimSpace(strings.Join(parts, " · ") + "。" + categoryLegend(CategoryAxisLabels, CategoryIDs)),
	}, true
}

// cleanImpactChart compares root partition used vs free before and after safe cache cleanup on the Dry-Run Plan tab.
// When the post-cleanup watermark drops to a different severity tone, separate pre/post used series color each bar
// with its own health tone while keeping explicit status words on every point and explaining free-space floor rules.
func cleanImpactChart(ms MachineStatus, scan *Scan) (wire.Component, bool) {
	if scan == nil || scan.Mount.TotalBytes == 0 || scan.SafeBytes == 0 {
		return wire.Component{}, false
	}
	unit := chartUnit(scan.Mount.TotalBytes)
	afterUsed := uint64(0)
	if scan.Mount.UsedBytes > scan.SafeBytes {
		afterUsed = scan.Mount.UsedBytes - scan.SafeBytes
	}
	afterAvail := min(scan.Mount.TotalBytes, scan.Mount.AvailableBytes+scan.SafeBytes)
	afterPct := float64(afterUsed) * 100.0 / float64(scan.Mount.TotalBytes)
	afterLevel := EvaluateHealth(true, scan.Mount.TotalBytes, afterAvail)

	beforeText := healthLevelText(fullnessLevel(ms))
	afterText := healthLevelText(afterLevel)
	beforeTone := progressTone(fullnessLevel(ms))
	afterTone := progressTone(afterLevel)

	beforeUsedDisp := fmt.Sprintf("%s (%.1f%% · %s)", FormatBytes(scan.Mount.UsedBytes), ms.UsedPercent, beforeText)
	afterUsedDisp := fmt.Sprintf("%s (%.1f%% · %s)", FormatBytes(afterUsed), afterPct, afterText)

	var series []wire.ChartSeries
	if beforeTone != afterTone {
		series = []wire.ChartSeries{
			{
				Name: "删之前用了 · " + beforeText,
				Tone: beforeTone,
				Points: []wire.ChartPoint{
					{Label: "删之前", Value: chartValue(scan.Mount.UsedBytes, unit), Display: beforeUsedDisp},
					{Label: "删之后", Value: 0, Display: "—"},
				},
			},
			{
				Name: "删之后用了 · " + afterText,
				Tone: afterTone,
				Points: []wire.ChartPoint{
					{Label: "删之前", Value: 0, Display: "—"},
					{Label: "删之后", Value: chartValue(afterUsed, unit), Display: afterUsedDisp},
				},
			},
		}
	} else {
		series = []wire.ChartSeries{
			{
				Name: "用了 · " + beforeText,
				Tone: beforeTone,
				Points: []wire.ChartPoint{
					{Label: "删之前", Value: chartValue(scan.Mount.UsedBytes, unit), Display: beforeUsedDisp},
					{Label: "删之后", Value: chartValue(afterUsed, unit), Display: afterUsedDisp},
				},
			},
		}
	}
	series = append(series, wire.ChartSeries{
		Name: "还剩",
		Tone: wire.ToneNeutral,
		Points: []wire.ChartPoint{
			{Label: "删之前", Value: chartValue(scan.Mount.AvailableBytes, unit), Display: FormatBytes(scan.Mount.AvailableBytes)},
			{Label: "删之后", Value: chartValue(afterAvail, unit), Display: FormatBytes(afterAvail)},
		},
	})

	beforeNote := healthWatermarkNote(fullnessLevel(ms), ms.UsedPercent, scan.Mount.TotalBytes, scan.Mount.AvailableBytes)
	afterNote := healthWatermarkNote(afterLevel, afterPct, scan.Mount.TotalBytes, afterAvail)

	return wire.Component{
		Type:    wire.ComponentChart,
		Key:     boundedKey("plan-impact-", ms.Machine.ID),
		Kind:    wire.ChartKindBar,
		Stacked: true,
		Title:   "删掉放心删的之前和之后，系统盘对比",
		Unit:    unit,
		Series:  series,
		Detail: fmt.Sprintf("大约从用了 %.1f%%（%s）变成 %.1f%%（%s），多出 %s",
			ms.UsedPercent, beforeNote, afterPct, afterNote, FormatBytes(scan.SafeBytes)),
	}, true
}

// machineBreakdownTable renders a compact 4-column table so neither root usage nor category shares rely on color alone.
// legend spells the short names out in its caption: when no category chart above it does.
func machineBreakdownTable(ms MachineStatus, mountPoint string, bd categoryBreakdown, keyPrefix string, legend bool) wire.Component {
	rows := make([][]string, 0, len(CategoryIDs))
	for _, cid := range CategoryIDs {
		tot := bd.catTotal(cid)
		share := "—"
		if tot > 0 && bd.totalShown > 0 {
			share = fmt.Sprintf("%.1f%%", float64(tot)*100.0/float64(bd.totalShown))
		}
		rows = append(rows, []string{
			CategoryAxisLabels[cid],
			bd.catModeText(cid),
			FormatBytes(tot),
			share,
		})
	}
	mp := cmp.Or(mountPoint, "/")
	lvlNote := healthWatermarkNote(fullnessLevel(ms), ms.UsedPercent, ms.TotalBytes, ms.AvailableBytes)
	caption := fmt.Sprintf("系统盘 %s：用了 %s（%.1f%%，%s）· 还剩 %s / 共 %s · %s",
		mp, FormatBytes(ms.UsedBytes), ms.UsedPercent, lvlNote,
		FormatBytes(ms.AvailableBytes), FormatBytes(ms.TotalBytes), fullnessLegend)
	if lastKnown(ms) {
		caption = "上次检查时，" + caption
	}
	caption += "。"
	if legend {
		caption += categoryLegend(CategoryAxisLabels, CategoryIDs)
	}
	return wire.Component{
		Type:    wire.ComponentTable,
		Key:     boundedKey(keyPrefix+"-tbl-", ms.Machine.ID),
		Caption: caption,
		Columns: []wire.TableColumn{
			{Title: "种类"},
			{Title: "能删吗"},
			{Title: "大小", Align: "end"},
			{Title: "占比", Align: "end"},
		},
		Rows: rows,
	}
}

// machinePlainTextSummary provides a plain-text breakdown for UIVersion 1 clients and View.Fallback.
func machinePlainTextSummary(ms MachineStatus, scan *Scan) string {
	name := machineLabel(ms.Machine)
	if ms.TotalBytes == 0 {
		if ms.Machine.LastError != "" {
			return fmt.Sprintf("%s：连不上（%s）", name, sshProblem(ms.Machine, ms.Machine.LastError).Title)
		}
		return fmt.Sprintf("%s：还没检查", name)
	}
	mp := "/"
	var bd categoryBreakdown
	if scan != nil {
		mp = cmp.Or(scan.Mount.MountPoint, "/")
		bd = summarizeCategories(scan.Items, &scan.Sessions)
	}
	var catParts []string
	for _, cid := range CategoryIDs {
		if tot := bd.catTotal(cid); tot > 0 {
			catParts = append(catParts, fmt.Sprintf("%s %s", categoryName(cid), FormatBytes(tot)))
		}
	}
	catLine := "没有能删的缓存"
	if len(catParts) > 0 {
		catLine = "能删的：" + strings.Join(catParts, "、")
	}
	if lastKnown(ms) {
		name += "（" + offlineText + "，下面是上次检查的数）"
	}
	return fmt.Sprintf("%s [%s %.1f%%] 系统盘 %s 用了 %s / %s，还剩 %s，可以腾出 %s（其中放心删 %s）；%s",
		name, healthLevelText(fullnessLevel(ms)), ms.UsedPercent, mp,
		FormatBytes(ms.UsedBytes), FormatBytes(ms.TotalBytes), FormatBytes(ms.AvailableBytes),
		FormatBytes(ms.ReclaimableBytes), FormatBytes(ms.SafeBytes), catLine)
}

func (d draw) fallbackSummary(statuses []MachineStatus) string {
	var lines []string
	reclaim := fleetReclaimOf(statuses)
	head := fmt.Sprintf("Disklessly 硬盘空间（%d 台电脑，现在一共可以腾出 %s，%s；%s）",
		len(statuses), FormatBytes(reclaim.all), reclaim.statDetail(), fullnessLegend)
	lines = append(lines, head)
	for _, ms := range statuses[:min(len(statuses), 12)] {
		lines = append(lines, "- "+machinePlainTextSummary(ms, d.st.Scans[ms.Machine.ID]))
	}
	if len(statuses) > 12 {
		lines = append(lines, fmt.Sprintf("…其余 %d 台电脑请在页面上看", len(statuses)-12))
	}
	out := strings.Join(lines, "\n")
	return clip(out, 1950)
}
