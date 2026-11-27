package main

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// failureWords says why an attempt failed in the user's words. The error class is what the queue decides on;
// the user reads this instead. A failed acceptance and an agent that broke off share the class task_failed and
// are told apart by the verdict.
func failureWords(class, verdict string) string {
	switch strings.TrimSpace(class) {
	case "config_error":
		return "模型或配置不可用"
	case "auth_expired":
		return "Agent 登录失效"
	case "rate_limited":
		return "额度或限流"
	case "infra_error":
		return "电脑或环境出错"
	case "timeout":
		if verdict == VerdictFail {
			return "验收命令超时"
		}
		return "超时"
	case "lost":
		return "会话丢失"
	case "rejected":
		return "任务本身有问题（仓库或 ref 不对）"
	case "", "task_failed":
		if verdict == VerdictFail {
			return "没通过验收"
		}
		return "Agent 出错"
	}
	return "出错（" + class + "）"
}

// outcomeWords is one attempt's or one finished task's result in a few words.
func outcomeWords(state, verdict, class string) string {
	switch state {
	case StateSucceeded:
		if verdict == VerdictPass || verdict == "passed" {
			return "通过验收"
		}
		return "跑完了，没有验收命令"
	case StateFailed:
		return failureWords(class, verdict)
	case StateCanceled, StateCancelled:
		return "已取消"
	case StateExpired:
		return "已过期"
	case StateNeedsInput:
		return "等你处理"
	case StateRunning, StateLeased:
		return "在跑"
	case StateQueued:
		return "排队中"
	}
	return state
}

// phaseWords says what a running attempt is doing.
func phaseWords(phase string) string {
	switch phase {
	case PhaseWorking:
		return "ATS 在做"
	case PhaseAccepting:
		return "在检查结果"
	}
	return "在准备"
}

// priorityWords is a task's priority as the pages say it.
func priorityWords(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case PriorityP0:
		return "紧急"
	case PriorityP1:
		return "重要"
	case PriorityP3:
		return "不急"
	}
	return "普通"
}

// permissionWords says whether a task may change files.
func permissionWords(profile string) string {
	if profile == PermissionDefault {
		return "会改文件"
	}
	return "只看不改"
}

// repoName is the last element of a repository's path: enough to tell repositories apart in a row.
func repoName(repo string) string {
	repo = strings.TrimRight(strings.TrimSpace(strings.ReplaceAll(repo, "\\", "/")), "/")
	if repo == "" {
		return ""
	}
	return path.Base(repo)
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// oneLine folds a text of several lines into one, cut to maxRunes.
func oneLine(s string, maxRunes int) string {
	return truncateRunes(strings.Join(strings.Fields(s), " "), maxRunes)
}

// lastReport is the task's newest evidence report, or nil.
func lastReport(t *Task) *EvidenceReport {
	if t == nil || len(t.Reports) == 0 {
		return nil
	}
	return &t.Reports[len(t.Reports)-1]
}

// lastErrorClass is the error class of the task's last failure: the task's own, or its last report's.
func lastErrorClass(t *Task) string {
	if c := strings.TrimSpace(t.FailureReason); c != "" {
		return c
	}
	if r := lastReport(t); r != nil {
		return strings.TrimSpace(r.ErrorClass)
	}
	return ""
}

// acceptanceOutput splits a report's message into its first line and the output tail under it. The dispatcher
// writes "验收命令退出码 N\n<tail>" for an acceptance command that ran.
func acceptanceOutput(message string) (head, tail string) {
	head, tail, _ = strings.Cut(strings.TrimSpace(message), "\n")
	return strings.TrimSpace(head), strings.TrimSpace(tail)
}

// outputEnd is a report's message in at most about max runes: its first line, which says how the attempt or its
// acceptance command ended, and the end of the output under it.
func outputEnd(message string, max int) string {
	message = strings.TrimSpace(message)
	if utf8.RuneCountInString(message) <= max {
		return message
	}
	head, tail := acceptanceOutput(message)
	if tail == "" {
		return tailRunes(head, max)
	}
	head = headRunes(head, max/4)
	return head + "\n" + tailRunes(tail, max-utf8.RuneCountInString(head))
}

func attemptCount(n int) string { return fmt.Sprintf("第 %d 次", max(n, 1)) }

// The words below are for the pages only. failureWords and outcomeWords above also go into the decision written
// to the to-do, so the pages use their own variants and leave those strings alone.

// extName is the extension's name as the settings list shows it (persona-extension.json "name"). Pages name the
// settings in these words; the setup page also opens them (setup.go).
const extName = "ATS 自动做待办"

// settingsPath is where the user changes this extension's settings.
const settingsPath = "设置 › 扩展 › " + extName

// pageFailureWords says why an attempt did not work, for the pages.
func pageFailureWords(class, verdict string) string {
	switch strings.TrimSpace(class) {
	case "config_error":
		return "选的模型用不了"
	case "auth_expired":
		return "那台电脑上干活用的程序要重新登录"
	case "rate_limited":
		return "额度不够或被限速了"
	case "infra_error":
		return "电脑上出了问题"
	case "timeout":
		if verdict == VerdictFail {
			return "检查太久没跑完"
		}
		return "做太久没做完，被停下了"
	case "lost":
		return "做到一半断开了"
	case "rejected":
		return "项目文件夹或版本不对"
	case "", "task_failed":
		if verdict == VerdictFail {
			return "检查没通过"
		}
		return "做到一半出错停下了"
	}
	return "出了点问题"
}

// failureNextStep is what the user can do about a failure of that kind.
func failureNextStep(class, verdict string) string {
	switch strings.TrimSpace(class) {
	case "config_error":
		return "到「" + settingsPath + "」里看看「用哪个模型」那几项，空着就用默认的；改好后点「再试一次」。"
	case "auth_expired":
		return "那台电脑上干活用的程序（交给它的事就是由它来做的）登录过期了。到那台电脑上把这个程序重新登录一下（不是 Persona 的登录），然后点「再试一次」。"
	case "rate_limited":
		return "等额度恢复后再点「再试一次」。"
	case "infra_error":
		return "看看那台电脑是不是开着、项目文件夹还在不在，然后点「再试一次」。"
	case "timeout":
		if verdict == VerdictFail {
			return "检查用的命令跑得太久。到「" + settingsPath + "」里把「检查最多等多久」调长，再点「再试一次」；要是这条命令本身会卡住，就跟助手说换一个检查方法，重新交给它做。"
		}
		return "把这件事拆小一点再交给它，或者在「" + settingsPath + "」里把「一件事最多做多久」调长。"
	case "lost":
		return "点「再试一次」重新开始。"
	case "rejected":
		return "看看项目在不在允许的文件夹里、版本写得对不对，然后让助手重新交给它。"
	case "", "task_failed":
		if verdict == VerdictFail {
			return "看下面「怎么检查的」里命令输出了什么，跟助手说要怎么改，再点「再试一次」。"
		}
		return "看看下面它说了什么，再点「再试一次」。"
	}
	return "看看最下面「技术细节」里的原话，再点「再试一次」。"
}

// pageOutcomeWords is one attempt's or one finished task's result, for the pages.
func pageOutcomeWords(state, verdict, class string) string {
	switch state {
	case StateSucceeded:
		if verdict == VerdictPass || verdict == "passed" {
			return "做成了，检查通过"
		}
		return "做完了（没设检查）"
	case StateFailed:
		return "没做成：" + pageFailureWords(class, verdict)
	case StateCanceled, StateCancelled:
		return "已取消"
	case StateExpired:
		return "过期了，没做"
	case StateNeedsInput:
		return "等你处理"
	case StateRunning, StateLeased:
		return "在做"
	case StateQueued:
		return "排着队"
	}
	return "不清楚"
}

// toolWords names a tool an agent asks to use, in the user's words.
func toolWords(tool string) string {
	switch strings.TrimSpace(tool) {
	case "", "Bash":
		return "运行一条命令"
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return "改文件"
	case "Read", "Glob", "Grep", "LS":
		return "看文件"
	case "WebFetch", "WebSearch":
		return "上网查资料"
	}
	return "用一个工具"
}

// waitingWords is what a task waits for, for the pages. The dispatcher writes these lines for the API as well
// ("Agent 等你批准运行：…"), so the pages reword them here rather than at the source.
func waitingWords(raw string) string {
	raw = strings.TrimSpace(raw)
	cut := func(prefix string) (string, bool) {
		rest, ok := strings.CutPrefix(raw, prefix)
		return strings.TrimSpace(rest), ok
	}
	if raw == "" || raw == waitingInSession {
		return "它在等你回答问题或点同意"
	}
	if rest, ok := cut("Agent 等你批准运行："); ok {
		return "它想运行一条命令，等你同意：" + rest
	}
	if rest, ok := cut("Agent 等你批准使用 "); ok {
		return "它想" + toolWords(rest) + "，等你同意"
	}
	if rest, ok := cut("Agent 等你批准 "); ok {
		tool, what, _ := strings.Cut(rest, "：")
		if what = strings.TrimSpace(what); what != "" {
			return "它想" + toolWords(tool) + "，等你同意：" + what
		}
		return "它想" + toolWords(tool) + "，等你同意"
	}
	if rest, ok := cut("Agent 在问你："); ok {
		return "它在问你：" + rest
	}
	if rest, ok := cut("Agent 说它卡住了："); ok {
		return "它卡住了，需要你帮忙：" + rest
	}
	if strings.HasPrefix(raw, "Agent 提交了计划") {
		return "它写好了打算怎么做，等你同意"
	}
	if rest, ok := cut("Agent "); ok {
		return "它" + rest
	}
	return raw
}

// actionErrorWords turns an error of a page's button into a Chinese sentence with what to do next. The second
// value is the raw error, for errors this does not know, to show after the sentence.
func actionErrorWords(err error) (words, detail string) {
	var adm *AdmissionError
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, ErrTaskNotFound):
		return "找不到这个任务了，可能已经被清掉。刷新一下看看。", ""
	case errors.Is(err, ErrStaleAttempt):
		return "这个任务刚刚有了变化，刷新一下再试。", ""
	case errors.As(err, &adm) && adm.Code == "too_many_queued":
		return fmt.Sprintf("排着的事已经满了（最多 %d 件），等前面的做完一些再试。", adm.MaxQueued), ""
	case strings.HasPrefix(err.Error(), "only failed or canceled tasks can be retried"):
		return "这个任务还没结束，不能再试一次。等它做完，或者先取消。", ""
	}
	return "出了点问题，没办成。刷新一下再试。", err.Error()
}

// setupMissingWords says which of the two setup steps is still left, for the Overview card.
func setupMissingWords(haveMachines, haveRepos bool) string {
	switch {
	case !haveMachines && !haveRepos:
		return "还没选干活的电脑，也还没告诉它能动哪些文件夹。"
	case !haveMachines:
		return "还没选干活的电脑。"
	case !haveRepos:
		return "还没告诉它能动哪些文件夹。"
	}
	return ""
}
