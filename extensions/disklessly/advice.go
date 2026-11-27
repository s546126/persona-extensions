package main

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// disklessly_advise and the 「帮我腾空间」 block of 硬盘空间 share buildAdvice: a ranked, read-only
// plan built only from the cached scan results, the conversation index and each computer's
// readiness. It deletes, moves and runs nothing: every step names the existing write tool (and
// the existing page button) that performs it, and those keep their own confirmations.

// adviceStaleAfter is the age after which a computer's scan is called old.
const adviceStaleAfter = 24 * time.Hour

// Step actions.
const (
	adviceClean     = "clean"              // delete 放心删 items of one kind (disklessly_clean items)
	adviceMove      = "move_conversations" // move week-old AI conversations (disklessly_sessions_offload)
	adviceLookFirst = "look_first"         // a 建议先看 item, the user's own call (disklessly_clean items)
)

// adviceCall is an existing tool call, exactly as the assistant should make it.
type adviceCall struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// adviceItem is one scan item a step covers.
type adviceItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Bytes uint64 `json:"bytes"`
	Size  string `json:"size"`
}

// adviceStep is one thing to do on one computer.
type adviceStep struct {
	Rank        int          `json:"rank,omitempty"`
	MachineID   string       `json:"machine_id"`
	MachineName string       `json:"machine_name"`
	Action      string       `json:"action"`
	Kind        string       `json:"kind"`
	KindName    string       `json:"kind_name"`
	What        string       `json:"what"`
	Items       []adviceItem `json:"items,omitempty"`
	FreesBytes  uint64       `json:"frees_bytes"`
	Frees       string       `json:"frees"`
	Risk        string       `json:"risk"`
	Possible    bool         `json:"possible"`
	Blocked     string       `json:"blocked,omitempty"`
	Unblock     string       `json:"unblock,omitempty"`
	UnblockCall *adviceCall  `json:"unblock_call,omitempty"`
	Command     string       `json:"command,omitempty"`
	Call        adviceCall   `json:"call"`
	PreviewCall *adviceCall  `json:"preview_call,omitempty"`
	Page        string       `json:"page"`
	PageButtons []string     `json:"page_buttons"`

	tier    int    // 0 caches, 1 conversations, 2 old logs and temporary files, 3 建议先看
	pageTab string // where the page row leads: tabItems or sessionsUI
	short   string // the reason it cannot be done now, in a few words, for the page
	count   int    // move_conversations: how many conversations, for the page
}

// adviceMachine is what the plan knows about one computer.
type adviceMachine struct {
	MachineID   string      `json:"machine_id"`
	MachineName string      `json:"machine_name"`
	Fullness    string      `json:"fullness"`
	FullnessSay string      `json:"fullness_text"`
	UsedPercent float64     `json:"used_percent,omitempty"`
	FreeBytes   uint64      `json:"free_bytes,omitempty"`
	Free        string      `json:"free,omitempty"`
	FreeAfter   string      `json:"free_after_steps,omitempty"`
	Reachable   bool        `json:"reachable"`
	CheckedAt   *time.Time  `json:"checked_at,omitempty"`
	Checked     string      `json:"checked"`
	Stale       bool        `json:"stale,omitempty"`
	Partial     bool        `json:"partial,omitempty"`
	Problem     string      `json:"problem,omitempty"`
	NextCall    *adviceCall `json:"next_call,omitempty"`
	// Conversations is the state of the computer's AI conversations moving, as disklessly_sessions_status words it.
	Conversations string `json:"conversations_state"`
	RecentKept    int    `json:"recent_conversations_kept"`

	m     *Machine
	rank  int
	used  float64
	avail uint64
	total uint64
}

// adviceGoal is what the user wants: free at least FreeGiB (0: as much as there is), and/or
// only the fullest computer.
type adviceGoal struct {
	FreeGiB float64 `json:"free_gib,omitempty"`
	Fullest bool    `json:"fullest,omitempty"`
}

type adviceOptions struct {
	Machine string
	Goal    adviceGoal
}

// advice is the whole answer.
type advice struct {
	ReadOnly     bool            `json:"read_only"`
	Source       string          `json:"source"`
	Summary      string          `json:"summary"`
	Goal         *adviceGoalOut  `json:"goal,omitempty"`
	Machines     []adviceMachine `json:"machines"`
	Steps        []adviceStep    `json:"steps"`
	LookFirst    []adviceStep    `json:"look_first"`
	MoreSteps    int             `json:"more_steps_not_listed,omitempty"`
	NotTouched   []string        `json:"not_touched"`
	HowToAct     string          `json:"how_to_act"`
	RefreshNotes []string        `json:"refresh_errors,omitempty"`

	totalPossible uint64
}

type adviceGoalOut struct {
	FreeGiB      float64 `json:"free_gib,omitempty"`
	Fullest      bool    `json:"fullest,omitempty"`
	PlannedBytes uint64  `json:"planned_bytes"`
	Planned      string  `json:"planned"`
	Met          bool    `json:"met"`
	Note         string  `json:"note"`
}

// adviceRisk is each kind's risk in one plain sentence.
var adviceRisk = map[string]string{
	CatGoCache:         "缓存删了会在下次编译时自动重新生成、重新下载，第一次会慢些。",
	CatNodePythonCache: "下载过的包删了，下次安装时会重新下载，第一次会慢些。",
	CatDockerUnused:    "只删没有任何容器用到的旧镜像和没在用的构建缓存，以后要用时重新构建或下载。",
	CatRustXcodeBuild:  "编译缓存和下载的依赖删了，下次编译时会重新生成、重新下载，第一次会慢些。",
	CatTmpLogs:         "旧日志和临时文件删了就没有了，找不回来；最近 7 天的系统日志会留着。",
	CatColdAgentSession: "对话先放进网盘、读回核对无误才删电脑上的那份，要用时可以拿回来；" +
		"最近 7 天用过、正打开着或还有程序在用的对话不动；网盘连不上时一个都不删。",
}

func adviceTier(cat string) int {
	switch cat {
	case CatColdAgentSession:
		return 1
	case CatTmpLogs:
		return 2
	}
	return 0
}

// fullnessRank orders computers fullest first by their last check.
func fullnessRank(level string) int {
	switch level {
	case HealthCritical:
		return 0
	case HealthWarning:
		return 1
	case HealthOK:
		return 2
	case HealthUnscanned:
		return 3
	}
	return 4 // never checked and not answering
}

// ago words how long ago t was, deterministically for now.
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	}
	return fmt.Sprintf("%d 天前", int(d.Hours()/24))
}

func adviceCallOf(tool string, args map[string]any) adviceCall {
	return adviceCall{Tool: tool, Arguments: args}
}

func adviceCallp(tool string, args map[string]any) *adviceCall {
	c := adviceCallOf(tool, args)
	return &c
}

// buildAdvice ranks what could free space on the computers st knows, from what is already
// cached. It changes nothing in st apart from loading cold indexes.
func buildAdvice(st *State, now time.Time, opt adviceOptions) (*advice, error) {
	var targets []*Machine
	if strings.TrimSpace(opt.Machine) != "" {
		m := st.machine(opt.Machine)
		if m == nil {
			return nil, fmt.Errorf("找不到电脑 %q（用 disklessly_machines_status 看有哪些）", opt.Machine)
		}
		targets = []*Machine{m}
	} else {
		for _, m := range st.Machines {
			if m.Enabled {
				targets = append(targets, m)
			}
		}
	}

	var machines []adviceMachine
	for _, m := range targets {
		machines = append(machines, adviceMachineOf(st, m, now))
	}
	slices.SortStableFunc(machines, func(a, b adviceMachine) int {
		return cmp.Or(cmp.Compare(a.rank, b.rank), -cmp.Compare(a.used, b.used), cmp.Compare(a.MachineID, b.MachineID))
	})
	if opt.Goal.Fullest && opt.Machine == "" && len(machines) > 1 {
		// The fullest checked computer (machines is sorted fullest first); the first one when none is checked.
		pick := 0
		for i, am := range machines {
			if am.total > 0 {
				pick = i
				break
			}
		}
		machines = machines[pick : pick+1]
	}

	adv := &advice{ReadOnly: true, Source: "上次检查的结果（没有重新检查）", Steps: []adviceStep{}, LookFirst: []adviceStep{}}
	var all, look []adviceStep
	for i := range machines {
		steps, lf := adviceSteps(st, &machines[i], now)
		all = append(all, steps...)
		look = append(look, lf...)
	}

	// What can be done now comes first, each part keeping its order (fullest computer first,
	// safest and biggest first within one); the page and this tool list the steps the same way.
	all, look = doableFirst(all), doableFirst(look)

	goalBytes := uint64(0)
	if opt.Goal.FreeGiB > 0 {
		goalBytes = uint64(math.Ceil(opt.Goal.FreeGiB * gib))
	}
	var planned uint64
	met := false
	for i, s := range all {
		if goalBytes > 0 && planned >= goalBytes {
			adv.MoreSteps = len(all) - i
			met = true
			break
		}
		s.Rank = len(adv.Steps) + 1
		adv.Steps = append(adv.Steps, s)
		if s.Possible {
			planned += s.FreesBytes
		}
	}
	if goalBytes > 0 && planned >= goalBytes {
		met = true
	}
	// 建议先看 comes last, apart, and only when the steps above do not already reach the goal.
	if !met {
		for i, s := range look {
			s.Rank = i + 1
			adv.LookFirst = append(adv.LookFirst, s)
		}
	}
	adv.totalPossible = planned

	perMachine := map[string]uint64{}
	for _, s := range adv.Steps {
		if s.Possible {
			perMachine[s.MachineID] += s.FreesBytes
		}
	}
	for i := range machines {
		am := &machines[i]
		if g := perMachine[am.MachineID]; g > 0 && am.total > 0 {
			am.FreeAfter = "大约 " + FormatBytes(min(am.total, am.avail+g))
		}
	}
	adv.Machines = machines
	if adv.Machines == nil {
		adv.Machines = []adviceMachine{}
	}

	if goalBytes > 0 || opt.Goal.Fullest {
		g := &adviceGoalOut{FreeGiB: opt.Goal.FreeGiB, Fullest: opt.Goal.Fullest, PlannedBytes: planned, Planned: FormatBytes(planned), Met: met}
		switch {
		case goalBytes == 0:
			g.Met = planned > 0
			g.Note = "只看最满的那台电脑。"
		case met:
			g.Note = fmt.Sprintf("按上面的步骤（现在就能做的）大约能腾出 %s，够 %s 了，后面的就不列了。", FormatBytes(planned), FormatBytes(goalBytes))
		default:
			g.Note = fmt.Sprintf("现在就能做的步骤一共大约只能腾出 %s，不到 %s：还可以看「建议先看」的，或者先解决那些现在还做不了的步骤。", FormatBytes(planned), FormatBytes(goalBytes))
		}
		adv.Goal = g
	}

	adv.NotTouched = adviceNotTouched(st, machines)
	adv.Summary = adviceSummary(adv, machines)
	adv.HowToAct = "这个工具只出主意，什么都没删、没搬。用户选了哪一步，就照那一步 call 里的工具和参数原样调用（那是会删除或搬走东西的写入工具，" +
		"Persona 会请用户确认，用户不同意就不做）；possible 为 false 的步骤先照 unblock 处理。用户没选的步骤不要做。做完一步，用工具回答里实测的腾出空间告诉用户真正腾出了多少。" +
		"「建议先看」(look_first) 的东西默认不删，只有用户点名要删才做。"
	return adv, nil
}

// doableFirst moves the steps that cannot be done now (possible false) after the others,
// keeping the order within each part.
func doableFirst(steps []adviceStep) []adviceStep {
	slices.SortStableFunc(steps, func(a, b adviceStep) int { return cmp.Compare(boolRank(!a.Possible), boolRank(!b.Possible)) })
	return steps
}

// adviceMachineOf is what the plan knows about computer m.
func adviceMachineOf(st *State, m *Machine, now time.Time) adviceMachine {
	ms := st.machineStatus(m)
	level := fullnessLevel(ms)
	am := adviceMachine{
		MachineID:   m.ID,
		MachineName: machineLabel(m),
		Fullness:    level,
		FullnessSay: healthLevelText(level),
		Reachable:   m.Reachable,
		Partial:     ms.Partial,
		RecentKept:  ms.Sessions.HotCount,
		m:           m,
		rank:        fullnessRank(level),
		used:        ms.UsedPercent,
		avail:       ms.AvailableBytes,
		total:       ms.TotalBytes,
	}
	if ms.TotalBytes > 0 {
		am.UsedPercent = math.Round(ms.UsedPercent*10) / 10
		am.FreeBytes = ms.AvailableBytes
		am.Free = FormatBytes(ms.AvailableBytes)
	}
	cat := st.cold(m.ID)
	am.Conversations = cat.stateCode()
	if ms.LastScannedAt != nil {
		t := *ms.LastScannedAt
		am.CheckedAt = &t
		am.Checked = ago(now, t) + "检查的"
		am.Stale = now.Sub(t) > adviceStaleAfter
		if am.Stale {
			am.Checked += "，数字可能已经变了；要最新的，用 refresh=true 重新检查（只看不删）"
		}
	} else {
		am.Checked = "还没检查过"
		am.Problem = "还没检查过「" + am.MachineName + "」，不知道能腾出多少。"
		am.NextCall = adviceCallp("disklessly_advise", map[string]any{"machine": m.ID, "refresh": true})
	}
	if !m.Reachable && m.LastError != "" {
		p := sshProblem(m, m.LastError)
		am.Problem = strings.TrimSpace("现在连不上「" + am.MachineName + "」（" + p.Title + "）。" + unreachableCauses)
		am.NextCall = adviceCallp("disklessly_advise", map[string]any{"machine": m.ID, "refresh": true})
	} else if !m.Reachable && ms.LastScannedAt != nil {
		am.Problem = "现在连不上「" + am.MachineName + "」，下面是它上次检查时的数。"
		am.NextCall = adviceCallp("disklessly_advise", map[string]any{"machine": m.ID, "refresh": true})
	}
	return am
}

// adviceSteps are computer am's steps, safest and biggest first, and its 建议先看 items.
func adviceSteps(st *State, am *adviceMachine, now time.Time) (steps, look []adviceStep) {
	m := am.m
	scan := st.Scans[m.ID]
	if scan == nil {
		return nil, nil
	}
	name := am.MachineName
	unreachable := !m.Reachable

	byKind := map[string][]ScanItem{}
	hasReview := map[string]bool{}
	for _, it := range scan.Items {
		if it.InUse || it.Bytes == 0 || it.Category == CatColdAgentSession {
			continue
		}
		if CheckSafeCleanTarget(it, scan.RemoteHome) != nil {
			continue
		}
		if it.Safety != SafetySafe {
			hasReview[it.Category] = true
			look = append(look, lookFirstStep(m, name, it))
			continue
		}
		byKind[it.Category] = append(byKind[it.Category], it)
	}
	for _, cid := range CategoryIDs {
		items := byKind[cid]
		if len(items) == 0 {
			continue
		}
		var total uint64
		var ids []string
		var list []adviceItem
		for _, it := range items {
			total += it.Bytes
			ids = append(ids, it.ID)
			list = append(list, adviceItem{ID: it.ID, Title: itemTitle(it), Bytes: it.Bytes, Size: FormatBytes(it.Bytes)})
		}
		s := adviceStep{
			MachineID: m.ID, MachineName: name, Action: adviceClean, Kind: cid, KindName: categoryName(cid),
			What:       fmt.Sprintf("在「%s」上删掉「%s」（%d 项，都是放心删的）", name, categoryName(cid), len(items)),
			Items:      list,
			FreesBytes: total,
			Frees:      "大约 " + FormatBytes(total),
			Risk:       adviceRisk[cid],
			Possible:   true,
			Call:       adviceCallOf("disklessly_clean", map[string]any{"machine": m.ID, "items": ids}),
			tier:       adviceTier(cid),
			pageTab:    tabItems,
		}
		if hasReview[cid] {
			// The kind's 删掉 row would take its 建议先看 items too: name the items' own rows.
			var titles []string
			for _, it := range items {
				titles = append(titles, clip(cmp.Or(itemTitle(it), it.ID), 200))
			}
			s.PageButtons = titles
			s.Page = "「" + homeTitle + "」页选「" + name + "」，在「能删什么」的「每一项」里逐项删：「" + strings.Join(titles, "」「") + "」（同一类里「建议先看」的那项不在这一步里）"
		} else {
			s.PageButtons = []string{"删掉：" + categoryName(cid)}
			s.Page = "「" + homeTitle + "」页选「" + name + "」，在「能删什么」里用「删掉：" + categoryName(cid) + "」那一行"
		}
		if scan.Partial {
			s.Frees += "（这台电脑上次缓存太多没算完，实际可能更多）"
		}
		if unreachable {
			s.Possible = false
			s.Blocked = "现在连不上「" + name + "」，删不了。"
			s.Unblock = "等它开机、连上网以后重新检查一次（只看不删），再做这一步。"
			s.UnblockCall = adviceCallp("disklessly_advise", map[string]any{"machine": m.ID, "refresh": true})
			s.short = "现在连不上这台电脑"
		}
		steps = append(steps, s)
	}
	if s, ok := moveStep(st, am, scan, now); ok {
		steps = append(steps, s)
	}
	slices.SortStableFunc(steps, func(a, b adviceStep) int {
		return cmp.Or(cmp.Compare(a.tier, b.tier), -cmp.Compare(a.FreesBytes, b.FreesBytes), cmp.Compare(a.Kind, b.Kind))
	})
	slices.SortStableFunc(look, func(a, b adviceStep) int {
		return cmp.Or(-cmp.Compare(a.FreesBytes, b.FreesBytes), cmp.Compare(a.Items[0].ID, b.Items[0].ID))
	})
	return steps, look
}

// lookFirstStep is one 建议先看 item: big, and slow to get back.
func lookFirstStep(m *Machine, name string, it ScanItem) adviceStep {
	title := clip(cmp.Or(itemTitle(it), it.ID), 200)
	s := adviceStep{
		MachineID: m.ID, MachineName: name, Action: adviceLookFirst, Kind: it.Category, KindName: categoryName(it.Category),
		What:        fmt.Sprintf("「%s」上的「%s」（建议先看：默认不删，删不删由你定）", name, title),
		Items:       []adviceItem{{ID: it.ID, Title: itemTitle(it), Bytes: it.Bytes, Size: FormatBytes(it.Bytes)}},
		FreesBytes:  it.Bytes,
		Frees:       "大约 " + FormatBytes(it.Bytes),
		Risk:        cmp.Or(itemReason(it), "删了以后下次用到时要重新下载，比较大、比较慢。"),
		Possible:    m.Reachable,
		Call:        adviceCallOf("disklessly_clean", map[string]any{"machine": m.ID, "items": []string{it.ID}}),
		Page:        "「" + homeTitle + "」页选「" + name + "」，在「能删什么」的「每一项」里用「" + title + "」那一行",
		PageButtons: []string{title},
		tier:        3,
		pageTab:     tabItems,
	}
	if !m.Reachable {
		s.Blocked = "现在连不上「" + name + "」，删不了。"
		s.Unblock = "等它开机、连上网以后重新检查一次（只看不删）。"
		s.UnblockCall = adviceCallp("disklessly_advise", map[string]any{"machine": m.ID, "refresh": true})
		s.short = "现在连不上这台电脑"
	}
	return s
}

// moveStep is moving computer am's week-old AI conversations to its network drive, with why it
// cannot be done now and what would make it possible.
func moveStep(st *State, am *adviceMachine, scan *Scan, now time.Time) (adviceStep, bool) {
	m := am.m
	name := am.MachineName
	var bytes uint64
	count := 0
	for _, it := range scan.Items {
		if it.Category == CatColdAgentSession && !it.InUse {
			bytes += it.Bytes
			count += it.SessionCnt
		}
	}
	movable := bytes > 0 || count > 0
	if !movable {
		bytes, count = scan.Sessions.ColdBytes, scan.Sessions.ColdCount
	}
	if bytes == 0 && count == 0 {
		return adviceStep{}, false
	}
	s := adviceStep{
		MachineID: m.ID, MachineName: name, Action: adviceMove, Kind: CatColdAgentSession, KindName: categoryName(CatColdAgentSession),
		What:        fmt.Sprintf("把「%s」上一周没动的 AI 对话（%d 个）搬到网盘", name, count),
		FreesBytes:  bytes,
		Frees:       "最多大约 " + FormatBytes(bytes) + "（还在用的会留着，实际可能少一些）",
		Risk:        adviceRisk[CatColdAgentSession],
		Possible:    true,
		Call:        adviceCallOf("disklessly_sessions_offload", map[string]any{"machine": m.ID}),
		PreviewCall: adviceCallp("disklessly_sessions_offload", map[string]any{"machine": m.ID, "dry_run": true}),
		Page:        "「" + sessionsTitle + "」页「" + name + "」那一行的「" + coldOffloadText + "」",
		PageButtons: []string{coldOffloadText},
		tier:        1,
		pageTab:     sessionsUI,
		count:       count,
	}
	cat := st.cold(m.ID)
	state := cat.stateCode()
	block := func(reason, unblock, short string, c *adviceCall) {
		s.Possible, s.Blocked, s.Unblock, s.short, s.UnblockCall = false, reason, unblock, short, c
	}
	refresh := adviceCallp("disklessly_advise", map[string]any{"machine": m.ID, "refresh": true})
	switch {
	case !m.Reachable:
		block("现在连不上「"+name+"」，搬不了。", "等它开机、连上网以后重新检查一次（只看不删），再做这一步。", "现在连不上这台电脑", refresh)
	case state != ColdReady && state != ColdUnchecked:
		adv := coldProblem(st, m, cat, state)
		var fix *adviceCall
		switch {
		case adv.brew:
			fix = adviceCallp("disklessly_cli_install", map[string]any{"machine": m.ID, "method": "brew"})
		case adv.copy.src != nil:
			fix = adviceCallp("disklessly_cli_install", map[string]any{"machine": m.ID, "method": "copy"})
		case state == ColdOffline:
			fix = refresh
		default:
			fix = adviceCallp("disklessly_sessions_status", map[string]any{"machine": m.ID, "refresh": true})
		}
		block(adv.Reason, adv.Next, coldStateLabel(state), fix)
		s.Command = adv.Command
		if fix != nil && fix.Tool == "disklessly_cli_install" {
			s.Unblock += "（disklessly_cli_install 会在这台电脑上装软件，也要用户确认。）"
		}
	case !movable:
		reason := cmp.Or(strings.TrimSpace(scan.Sessions.Note), "上次检查时这台电脑还不能搬对话。")
		block(reason, "重新检查一次（只看不删），看看现在能不能搬。", "上次检查时还不能搬", refresh)
	}
	return s, true
}

// adviceNotTouched is what no step touches.
func adviceNotTouched(st *State, machines []adviceMachine) []string {
	out := []string{
		"正在运行的服务和容器（Docker 只删没有任何容器用到的旧镜像和没在用的构建缓存）",
		"项目文件和项目里还没提交的改动",
		"最近 7 天用过、正打开着或还有程序在用的 AI 对话",
		"密码和密钥（~/.ssh、~/.gnupg、~/.persona 这些）",
		"最近 7 天的系统日志",
		"「建议先看」的东西，除非用户点名要删",
	}
	for _, am := range machines {
		if am.RecentKept > 0 {
			out = append(out, fmt.Sprintf("「%s」上最近一周用过的 %d 个 AI 对话", am.MachineName, am.RecentKept))
		}
		if scan := st.Scans[am.MachineID]; scan != nil {
			for _, it := range scan.Items {
				if it.InUse {
					out = append(out, fmt.Sprintf("「%s」上正在用的「%s」", am.MachineName, clip(cmp.Or(itemTitle(it), it.ID), 80)))
				}
			}
		}
	}
	return out
}

// adviceSummary is the answer in one or two plain sentences.
func adviceSummary(adv *advice, machines []adviceMachine) string {
	if len(machines) == 0 {
		return "还没有电脑。"
	}
	checked := 0
	for _, am := range machines {
		if am.CheckedAt != nil {
			checked++
		}
	}
	if checked == 0 {
		return "还没检查过这些电脑，不知道能腾出多少：先用 refresh=true 检查一次（只看不删）。"
	}
	if len(adv.Steps) == 0 {
		if len(adv.LookFirst) > 0 {
			return "放心删的都删过了；只剩「建议先看」的东西，删不删由用户决定。"
		}
		return "按上次检查的结果，现在没有能腾出来的东西。"
	}
	possible := 0
	for _, s := range adv.Steps {
		if s.Possible {
			possible++
		}
	}
	first := adv.Machines[0]
	lead := ""
	if first.total > 0 {
		lead = fmt.Sprintf("最满的是「%s」（用了 %.0f%%，还剩 %s）。", first.MachineName, first.UsedPercent, first.Free)
	}
	text := fmt.Sprintf("%s列了 %d 步，其中 %d 步现在就能做，一共大约能腾出 %s；都还没做，要做哪一步得用户先同意。", lead, len(adv.Steps), possible, FormatBytes(adv.totalPossible))
	if possible < len(adv.Steps) {
		text += "其余几步现在做不了，原因和办法写在各自那一步里。"
	}
	return text
}
