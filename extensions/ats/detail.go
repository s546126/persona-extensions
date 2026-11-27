package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// taskPage is the state of a task's page: the page the queue pushes when a row is tapped.
type taskPage struct {
	Task string `json:"task,omitempty"`
}

func taskPush(id, title string) *wire.Push {
	return &wire.Push{State: ext.EncodeState(taskPage{Task: id}), Title: firstLinePreview(title, 40)}
}

// pageTask is the task whose page a render or an action belongs to; empty on the queue page.
func pageTask(r *ext.UIRequest) string {
	var st taskPage
	if err := r.DecodeState(&st); err != nil || len(st.Task) > 128 {
		return ""
	}
	return strings.TrimSpace(st.Task)
}

const (
	detailTextMost = 2000 // a text on the page, in characters
	detailCodeMost = 6000 // the output tail on the page, in characters: its end is what tells
)

// buildTaskView is one task's page. It leads with where the task stands and what the user can do next, then
// the evidence in plain words (what the agent said it did, how the result was checked, where the changes are),
// what was asked, every attempt, and last, in small print, the technical details: ids, the model tier, the
// commit it started from, the exit code and the raw error.
func buildTaskView(q *Queue, id string, now time.Time, tz string) *wire.View {
	loc := viewLocation(tz)
	state := ext.EncodeState(taskPage{Task: id})
	t, err := q.GetTask(id)
	if err != nil || t == nil {
		return &wire.View{Title: "任务", State: state, Sections: []wire.Section{{Items: []wire.Component{
			{Type: wire.ComponentEmpty, Title: "找不到这件事", Text: "它可能已经被清掉了。回到「" + extName + "」首页看看现在有哪些事。"},
		}}}}
	}
	view := &wire.View{Title: firstLinePreview(t.Title, 40), State: state}
	if !t.IsTerminal() {
		view.RefreshAfter = 30
	}
	plan := q.PlanSchedule(now)
	machines := map[string]string{}
	online := 0
	for _, m := range plan.Machines {
		if m.Title != "" {
			machines[m.Name] = m.Title
		}
		if m.Online {
			online++
		}
	}
	machineName := func(id string) string { return cmpOr(machines[id], id) }
	clock := func(at time.Time) string { return at.In(loc).Format("01-02 15:04") }
	machine := t.Machine
	if t.Exec != nil && t.Exec.MachineID != "" {
		machine = t.Exec.MachineID
	} else if n := len(t.Attempts); n > 0 && t.Attempts[n-1].Machine != "" {
		machine = t.Attempts[n-1].Machine
	}
	on := ""
	if machine != "" {
		on = "在 " + machineName(machine) + " 上"
	}
	rep := lastReport(t)

	// First: the task, how it stands, and what to do next.
	head := []wire.Component{{Type: wire.ComponentHeader, Key: "task", Title: cmpOr(truncateRunes(t.Title, detailTextMost), "（没有标题）"), Badges: taskBadges(t)}}
	stand, next, tone := taskStanding(t, rep, plan, online, on, loc)
	key := "now"
	if t.IsTerminal() {
		key = "result"
	}
	text := truncateRunes(stand, detailTextMost/2)
	if next != "" {
		text += "\n接下来：" + next
	}
	if t.IsTerminal() {
		switch {
		case t.DecisionWrittenRef != "":
			text += "\n结果已经写回待办。"
		case t.DecisionSkipReason != "":
			text += "\n结果没能写回待办（待办已经不在了，或者没有权限）。"
		}
	}
	head = append(head, wire.Component{Type: wire.ComponentText, Key: key, Text: truncateRunes(text, detailTextMost), Tone: tone})
	switch {
	case !t.IsTerminal():
		a := cancelAction(t)
		head = append(head, wire.Component{Type: wire.ComponentButton, Key: "cancel", Title: "取消，不做了", Role: wire.ButtonRoleDestructive, Action: &a})
	case t.State != StateSucceeded:
		a := retryAction(t)
		head = append(head, wire.Component{Type: wire.ComponentButton, Key: "retry", Title: "再试一次", Action: &a})
	}
	view.Sections = append(view.Sections, wire.Section{Title: "现在怎么样", Items: head})

	if rep != nil && strings.TrimSpace(rep.Summary) != "" {
		view.Sections = append(view.Sections, wire.Section{Title: "它说它做了什么", Items: []wire.Component{
			{Type: wire.ComponentText, Key: "summary", Text: truncateRunes(rep.Summary, detailTextMost)},
		}})
	}

	// How the result is checked: the command, and what it answered when it ran.
	var accept []wire.Component
	if t.Acceptance != nil && strings.TrimSpace(t.Acceptance.Command) != "" {
		accept = append(accept, wire.Component{Type: wire.ComponentText, Key: "accept-how", Style: wire.TextStyleSecondary, Text: "做完以后，在项目里运行下面这条命令，没报错就算通过："})
		accept = append(accept, wire.Component{Type: wire.ComponentCode, Key: "accept-command", Language: "sh", Text: truncateRunes(strings.TrimSpace(t.Acceptance.Command), detailTextMost)})
		switch {
		case rep != nil && rep.ExitCode != nil:
			_, tail := acceptanceOutput(rep.Message)
			line, tone := attemptCount(rep.AttemptNo)+"检查：通过了。", wire.TonePositive
			switch {
			case rep.ErrorClass == "timeout":
				line, tone = attemptCount(rep.AttemptNo)+"检查：太久没跑完，算没通过。", wire.ToneDanger
			case *rep.ExitCode != 0 || rep.Verdict != VerdictPass:
				line, tone = attemptCount(rep.AttemptNo)+"检查：没通过，命令报错了。", wire.ToneDanger
			}
			accept = append(accept, wire.Component{Type: wire.ComponentText, Key: "accept-exit", Text: line, Tone: tone})
			if tail != "" {
				accept = append(accept, wire.Component{Type: wire.ComponentText, Key: "accept-output-label", Style: wire.TextStyleSecondary, Text: "命令最后输出的内容："})
				accept = append(accept, wire.Component{Type: wire.ComponentCode, Key: "accept-output", Text: tailRunes(tail, detailCodeMost)})
			}
		case t.IsTerminal():
			accept = append(accept, wire.Component{Type: wire.ComponentText, Key: "accept-exit", Style: wire.TextStyleSecondary, Text: "没检查：在检查之前就停下了。"})
		default:
			accept = append(accept, wire.Component{Type: wire.ComponentText, Key: "accept-exit", Style: wire.TextStyleSecondary, Text: "还没到检查这一步。"})
		}
	} else {
		accept = append(accept, wire.Component{Type: wire.ComponentText, Key: "accept-none", Style: wire.TextStyleSecondary, Text: "这件事没设检查方法：做完只能算「做完了」，没法确认做得对不对。"})
	}
	view.Sections = append(view.Sections, wire.Section{Title: "怎么检查的", Items: accept})

	if rows := artifactRows(t, rep); len(rows) > 0 {
		footer := "这些都在干活的那台电脑上，没有上传到别处。"
		if machine != "" {
			footer = "这些都在 " + machineName(machine) + " 上的项目里，没有上传到别处。"
		}
		view.Sections = append(view.Sections, wire.Section{Title: "改动在哪", Items: []wire.Component{
			{Type: wire.ComponentTable, Key: "artifacts", Columns: []wire.TableColumn{{Title: "是什么"}, {Title: "在哪"}}, Rows: rows},
		}, Footer: truncateRunes(footer, detailTextMost)})
	}

	if prompt := strings.TrimSpace(t.Prompt); prompt != "" {
		view.Sections = append(view.Sections, wire.Section{Title: "交给它的事", Items: []wire.Component{
			{Type: wire.ComponentText, Key: "prompt", Text: truncateRunes(prompt, detailTextMost)},
		}})
	}

	// The facts, in words.
	facts := [][]string{
		{"有多急", priorityWords(t.Priority)},
		{"会不会改文件", permissionWords(t.PermissionProfile)},
	}
	if t.Workspace.Repo != "" {
		facts = append(facts, []string{"项目", truncateRunes(t.Workspace.Repo, detailTextMost)})
	}
	if machine != "" {
		facts = append(facts, []string{"电脑", truncateRunes(machineName(machine), detailTextMost)})
	}
	if !t.SubmittedAt.IsZero() {
		facts = append(facts, []string{"交给它的时间", clock(t.SubmittedAt)})
	}
	if t.FinishedAt != nil {
		facts = append(facts, []string{"结束的时间", clock(*t.FinishedAt)})
	}
	view.Sections = append(view.Sections, wire.Section{Title: "基本情况", Items: []wire.Component{
		{Type: wire.ComponentTable, Key: "facts", Columns: []wire.TableColumn{{Title: "项"}, {Title: "内容"}}, Rows: facts},
	}})

	if len(t.Attempts) > 0 {
		attempts := t.Attempts
		if len(attempts) > 100 {
			attempts = attempts[len(attempts)-100:]
		}
		rows := make([][]string, 0, len(attempts))
		for _, a := range attempts {
			at := ""
			switch {
			case a.FinishedAt != nil:
				at = clock(*a.FinishedAt)
			case a.StartedAt != nil:
				at = clock(*a.StartedAt) + " 开始"
			}
			state := a.State
			if isActiveState(state) && a.AttemptNo == t.AttemptNo {
				state = t.State // the task knows that its attempt waits for the user
			}
			rows = append(rows, []string{attemptCount(a.AttemptNo), pageOutcomeWords(state, a.Verdict, a.ErrorClass), machineName(a.Machine), at})
		}
		view.Sections = append(view.Sections, wire.Section{Title: "做过几次", Items: []wire.Component{
			{Type: wire.ComponentTable, Key: "attempts", Columns: []wire.TableColumn{{Title: "第几次"}, {Title: "结果"}, {Title: "电脑"}, {Title: "时间", Align: "end"}}, Rows: rows},
		}})
	}

	view.Sections = append(view.Sections, technicalSection(t, rep, machine))
	return view
}

// taskStanding says in a sentence where a task stands and what the user can do next, with the tone to draw it in.
func taskStanding(t *Task, rep *EvidenceReport, plan SchedulePlan, online int, on string, loc *time.Location) (stand, next, tone string) {
	e := t.Exec
	since := ""
	if e != nil && !e.PhaseSince.IsZero() {
		since = "，从 " + e.PhaseSince.In(loc).Format("15:04") + " 开始"
	}
	switch {
	case t.State == StateNeedsInput:
		waiting := ""
		if e != nil {
			waiting = e.Waiting
		}
		return waitingWords(waiting), "打开这件事对应的待办，在里面 ATS 的对话里回答它的问题或点同意，它就会接着做。", wire.ToneDanger
	case e != nil && isActiveState(t.State):
		where := strings.TrimSpace(on)
		if where != "" {
			where += "，"
		}
		switch e.Phase {
		case PhaseWorking:
			stand = "ATS 正在做（" + where + attemptCount(t.AttemptNo) + since + "）。"
		case PhaseAccepting:
			stand = "做完了，正在检查结果（" + where + attemptCount(t.AttemptNo) + since + "）。"
		default:
			stand = "正在准备项目（" + where + attemptCount(t.AttemptNo) + since + "）。"
		}
		if t.Acceptance != nil && strings.TrimSpace(t.Acceptance.Command) != "" && e.Phase != PhaseAccepting {
			return stand, "不用做什么。做完它会用下面的命令检查一遍，再把结果写回待办。", ""
		}
		return stand, "不用做什么。做完它会把结果写回待办。", ""
	case t.State == StateQueued && t.NotBefore != nil:
		left := "这是最后一次自动再试。"
		if t.RetriesLeft > 0 {
			left = fmt.Sprintf("这次之后还能自动再试 %d 次。", t.RetriesLeft)
		}
		return fmt.Sprintf("第 %d 次没做成（%s），%s 会自动再试。%s", max(t.AttemptNo, 1),
				pageFailureWords(lastErrorClass(t), lastVerdict(t)), t.NotBefore.In(loc).Format("15:04"), left),
			"不用做什么，到时间它会自己再试。不想再试了就点「取消，不做了」。", wire.ToneCaution
	case t.State == StateQueued:
		switch {
		case len(plan.Machines) == 0 || online == 0:
			return "排着队，但现在没有电脑在线。", "打开你选的那台电脑，登录 Persona，它就会开始做。", wire.ToneCaution
		case plan.AdmissionMode == AdmissionPausedQuota:
			return "排着队：额度用完了，等额度恢复后开始。", "不用做什么，额度恢复后会自动开始。", wire.ToneCaution
		case plan.AdmissionMode == AdmissionPriorityOnly && t.Priority != PriorityP0 && t.Priority != PriorityP1:
			return "排着队：额度快用完了，现在先做紧急和重要的事。", "不用做什么，额度恢复后会接着做。", wire.ToneCaution
		}
		return "排着队，等电脑有空就开始。", "不用做什么，开始以后这一页会更新。", ""
	case t.State == StateSucceeded && t.Verdict == VerdictPass:
		return "做成了，检查也通过了。", "看看下面它做了什么、改动在哪。", wire.TonePositive
	case t.State == StateSucceeded:
		return "做完了。这件事没设检查方法，所以没法确认做得对不对。", "看看下面它说它做了什么，自己确认一下。", wire.ToneCaution
	case t.State == StateFailed:
		class, verdict := lastErrorClass(t), t.Verdict
		return fmt.Sprintf("没做成：%s，一共试了 %d 次。", pageFailureWords(class, verdict), max(t.AttemptNo, 1)), failureNextStep(class, verdict), wire.ToneDanger
	case t.State == StateCanceled || t.State == StateCancelled:
		return "已取消，没有接着做。", "想重新做的话，点「再试一次」。", ""
	case t.State == StateExpired:
		return "过期了：排了太久，没开始做。", "还想做的话，点「再试一次」。", ""
	}
	return pageOutcomeWords(t.State, t.Verdict, lastErrorClass(t)) + "。", "", ""
}

// technicalSection is the small print at the bottom of a task's page: what someone who looks into a problem asks
// for. The user does not need it.
func technicalSection(t *Task, rep *EvidenceReport, machine string) wire.Section {
	rows := [][]string{{"任务编号", t.ID}}
	if todo := t.TodoID(); todo != "" {
		rows = append(rows, []string{"待办编号", todo})
	}
	rows = append(rows, []string{"状态", t.State})
	if t.Difficulty != "" || t.ModelTier != "" {
		rows = append(rows, []string{"难度 / 模型档", strings.Trim(t.Difficulty+" / "+t.ModelTier, " /")})
	}
	if t.AgentType != "" {
		rows = append(rows, []string{"用的程序", t.AgentType})
	}
	if t.Workspace.Repo != "" {
		rows = append(rows, []string{"从哪个版本开始", baseWords(t.Workspace.BaseSHA)})
	}
	if machine != "" {
		rows = append(rows, []string{"电脑编号", machine})
	}
	if e := t.Exec; e != nil && isActiveState(t.State) {
		if e.Worktree != "" {
			rows = append(rows, []string{"正在用的项目副本", e.Worktree})
		}
		if e.SessionID != "" {
			rows = append(rows, []string{"对话编号", e.SessionID})
		}
	}
	if rep != nil {
		if c := strings.TrimSpace(rep.ErrorClass); c != "" {
			rows = append(rows, []string{"出错类别", c})
		}
		if rep.ExitCode != nil {
			rows = append(rows, []string{"检查命令的返回值", fmt.Sprintf("%d（0 才算通过）", *rep.ExitCode)})
		}
	}
	for i := range rows {
		rows[i][1] = truncateRunes(rows[i][1], 500)
	}
	items := []wire.Component{
		{Type: wire.ComponentText, Key: "tech-note", Style: wire.TextStyleCaption, Text: "出问题时给懂行的人看的，平时不用管。"},
		{Type: wire.ComponentTable, Key: "tech", Columns: []wire.TableColumn{{Title: "项"}, {Title: "内容"}}, Rows: rows},
	}
	if rep != nil && rep.ExitCode == nil && rep.Outcome != StateSucceeded {
		if msg := strings.TrimSpace(rep.Message); msg != "" {
			items = append(items, wire.Component{Type: wire.ComponentText, Key: "raw-message-label", Style: wire.TextStyleCaption, Text: "出错时的原话："})
			items = append(items, wire.Component{Type: wire.ComponentCode, Key: "raw-message", Text: truncateRunes(msg, detailTextMost)})
		}
	}
	return wire.Section{Title: "技术细节", Items: items}
}

func taskBadges(t *Task) []wire.Badge {
	switch t.State {
	case StateSucceeded:
		if t.Verdict == VerdictPass {
			return []wire.Badge{{Text: "做成了", Tone: wire.TonePositive}}
		}
		return []wire.Badge{{Text: "做完了（没设检查）", Tone: wire.ToneCaution}}
	case StateFailed:
		return []wire.Badge{{Text: "没做成", Tone: wire.ToneDanger}, {Text: pageFailureWords(lastErrorClass(t), t.Verdict), Tone: wire.ToneDanger}}
	case StateNeedsInput:
		return []wire.Badge{{Text: "等你处理", Tone: wire.ToneDanger}}
	case StateQueued:
		if t.NotBefore != nil {
			return []wire.Badge{{Text: "等着再试", Tone: wire.ToneCaution}}
		}
		return []wire.Badge{{Text: "排着队", Tone: wire.ToneCaution}}
	case StateRunning, StateLeased:
		phase := ""
		if t.Exec != nil {
			phase = t.Exec.Phase
		}
		return []wire.Badge{{Text: phaseWords(phase), Tone: wire.TonePositive}}
	}
	return []wire.Badge{{Text: pageOutcomeWords(t.State, t.Verdict, ""), Tone: wire.ToneCaution}}
}

// artifactRows lists what the task left on its computer, in words: of a finished attempt what its report names,
// of a running one the branch it works on. Paths of working copies and session ids are technical details.
func artifactRows(t *Task, rep *EvidenceReport) [][]string {
	var rows [][]string
	if e := t.Exec; e != nil && isActiveState(t.State) {
		if e.Branch != "" {
			rows = append(rows, []string{"存改动的分支（正在用）", e.Branch})
		}
		return rows
	}
	if rep == nil {
		return nil
	}
	for _, a := range rep.Artifacts {
		p := strings.TrimSpace(a.Path)
		if p == "" {
			continue
		}
		switch a.Kind {
		case "branch":
			rows = append(rows, []string{"存改动的分支", p})
		case "commit":
			rows = append(rows, []string{"保存的那一版（编号）", shortSHA(p)})
		case "worktree":
			rows = append(rows, []string{"留下的项目副本（里面有没保存的改动）", p})
		case "session":
			rows = append(rows, []string{"ATS 的对话记录", "在这件事对应的待办里"})
		default:
			rows = append(rows, []string{"别的东西", p})
		}
	}
	for i := range rows {
		rows[i][1] = truncateRunes(rows[i][1], 500)
	}
	return rows
}

// baseWords names the commit a task started from: a short id, or the newest commit when none was named.
func baseWords(sha string) string {
	if sha = strings.TrimSpace(sha); sha == "" || strings.EqualFold(sha, "HEAD") {
		return "当时最新的版本"
	}
	return shortSHA(sha)
}
