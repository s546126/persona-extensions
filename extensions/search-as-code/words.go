package main

import (
	"cmp"
	"strings"
	"unicode"
)

// Plain words for the page. The values behind them (roles, harness ids, error kinds, locations, group fields) are
// part of the MCP and API output and stay as they are; only what the page shows is translated here.

// pageTitle is the page's title; it matches the manifest's display name. cardTitle is the overview card's: the
// hub lists every UI contribution by its title among the extension's switches, so the card's must differ from
// the page's, and it is also the card's header on Overview.
const (
	pageTitle = "AI 聊天记录搜索"
	cardTitle = pageTitle + " · 概况"
)

// roleLabel names who said a snippet.
func roleLabel(role string) string {
	switch role {
	case "User":
		return "你"
	case "Assistant":
		return "助手"
	case "Error":
		return "报错"
	case "File":
		return "文件"
	case "Tool":
		return "工具输出"
	default:
		return cmp.Or(role, "内容")
	}
}

// harnessLabel names the coding tool a session came from.
func harnessLabel(h string) string {
	switch h {
	case "claude-code", "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "file":
		return "代码文件"
	case "":
		return "未知工具"
	default:
		return h
	}
}

// errorKindLabel says what kind of error a turn holds.
func errorKindLabel(kind string) string {
	switch kind {
	case "panic":
		return "程序崩溃"
	case "timeout":
		return "超时"
	case "quota_or_ratelimit":
		return "额度或限流"
	case "auth_or_permission":
		return "没有权限"
	case "command_or_test_failure":
		return "命令或测试失败"
	default:
		return "报错"
	}
}

// locationLabel is the badge of a session that is not a local file any more.
func locationLabel(loc string) string {
	switch loc {
	case locationArchived:
		return "已移到云端"
	case locationUnconfirmed:
		return "可能没传完"
	default:
		return loc
	}
}

// groupFieldLabel names what a result is grouped by.
func groupFieldLabel(field string) string {
	switch field {
	case "project":
		return "项目"
	case "harness":
		return "工具"
	case "model":
		return "模型"
	case "role":
		return "说话的人"
	case "error_kind":
		return "报错类型"
	case "machine":
		return "电脑"
	case "file":
		return "文件"
	case "branch":
		return "分支"
	default:
		return field
	}
}

// groupKeyLabel shows one group's key in plain words where it is a known value.
func groupKeyLabel(field, key string) string {
	switch field {
	case "harness":
		return harnessLabel(key)
	case "role":
		return roleLabel(key)
	case "error_kind":
		return errorKindLabel(key)
	case "machine":
		return machineTitle(key)
	default:
		return key
	}
}

// machineTitle is a computer's name as the page shows it: the hub's own computer is "本机（主电脑）".
func machineTitle(name string) string {
	return strings.Replace(name, "（hub）", "（主电脑）", 1)
}

// resultTitle names what a result is: the typed words, the saved search, or the recent chats.
func resultTitle(res *ExecutionResult, recipe *Recipe) string {
	if recipe != nil {
		return "常用搜索 · " + recipe.Name
	}
	q := strings.TrimSpace(res.Program.Query)
	switch {
	case q != "" && looksLikePipelineCode(q):
		return "按你写的步骤搜到的结果"
	case q != "":
		return "「" + clipRunes(q, 40) + "」的搜索结果"
	case res.Program.Scope == "errors":
		return "最近出现报错的聊天"
	default:
		return "最近的聊天"
	}
}

// hasHan reports whether s holds Chinese text, i.e. was written for the user rather than passed through.
func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// restoreFailure turns a failed restore's message into a cause plus what to do. Messages this extension writes
// already say both; a raw one from the restore tool or the connection goes behind a plain sentence.
func restoreFailure(msg string) string {
	const next = "没能恢复。稍后再试一次；一直不行的话，在那台电脑的终端运行 disklessly sessions status 看看。"
	msg = strings.TrimSpace(msg)
	switch {
	case msg == "":
		return next
	case strings.HasPrefix(msg, "恢复失败："):
		return next + "（详情：" + clipRunes(strings.TrimPrefix(msg, "恢复失败："), 200) + "）"
	case !hasHan(msg):
		return next + "（详情：" + clipRunes(msg, 200) + "）"
	default:
		return msg
	}
}

// actionFailure says why a page action did not work and what to do next; the cause stays as detail.
func actionFailure(action string, err error) string {
	cause := err.Error()
	switch action {
	case "search", "pivot-query", "scope":
		return "没法按这些词搜索：" + cause + "。去掉里面带冒号的特殊写法（比如 regex:）再试，或者换几个普通的词。"
	case "run-code":
		return "这些搜索步骤有问题，没能运行：" + cause + "。改一下再试，或者回到「结果」直接输入几个词搜索。"
	case "save-recipe":
		return "没有保存：" + cause + "。改好后再点保存。"
	case "delete-recipe":
		return "没有删除：" + cause + "。刷新页面看看它还在不在。"
	case "run-recipe", "rerun-history":
		return cause + "。可能已经被删掉了，刷新页面后再选一次。"
	default:
		if !hasHan(cause) {
			return "这个操作没有完成，刷新页面后再试。（详情：" + cause + "）"
		}
		return cause
	}
}

// dedupLabel names what dedup(by=…) compares.
func dedupLabel(by string) string {
	switch by {
	case "session", "":
		return "聊天"
	case "file":
		return "文件"
	case "title":
		return "标题"
	default:
		return by
	}
}

// rankLabel names how rank(by=…) orders.
func rankLabel(by string) string {
	switch by {
	case "freshness":
		return "时间（新的在前）"
	case "errors_first":
		return "报错在前、再按相关程度"
	default:
		return "相关程度和新旧"
	}
}
