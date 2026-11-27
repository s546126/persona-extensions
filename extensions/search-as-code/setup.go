package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/nwverse/persona/sdk/go/wire"
)

// What the page and the card say when a search read no chat at all. Zero counts there look like a broken
// extension to someone who opens it for the first time, so instead they say which of the real situations it is,
// what is missing, where to set it (a page cannot link to the hub's settings, so the place is spelled out) and
// what happens next.

// settingsPlace is where the hub draws this extension's settings; machinesSetting and sessionDirsSetting are the
// titles of its settings there (the manifest's configuration).
const (
	settingsPlace      = "设置 › 扩展 › " + pageTitle
	machinesSetting    = "要搜哪些电脑"
	sessionDirsSetting = "其他聊天记录文件夹（一般不用填）"
)

// readiness is why a result read no chat at all, or readyToShow when it read some.
type readiness int

const (
	readyToShow      readiness = iota
	noComputerPicked           // (a) no computer picked, and nothing to read on the hub's own
	picksNotSearched           // computers picked, but this result did not go to them (none was run since)
	noPickAnswered             // (b) computers picked, none of them online or answering, nothing synced
	picksHaveNoChats           // (c) the picked computers answered and really have no chats
)

// readinessOf classifies res: picked is whether any computer is picked in the setting now.
func readinessOf(res *ExecutionResult, picked bool) readiness {
	if res == nil || res.ScannedSessions > 0 || len(res.Hits) > 0 {
		return readyToShow
	}
	switch {
	case !picked:
		return noComputerPicked
	case len(res.Machines) == 0:
		return picksNotSearched
	}
	remote, answered := 0, 0
	for _, m := range res.Machines {
		switch m.Status {
		case machineLocal:
		case machineOnline:
			remote++
			answered++
		default:
			remote++
		}
	}
	if remote > 0 && answered == 0 {
		return noPickAnswered
	}
	return picksHaveNoChats
}

// machineNames lists the computers of runs with one of the statuses, at most three and then how many more.
func machineNames(runs []MachineRun, statuses ...string) (names string, n int) {
	var out []string
	for _, m := range runs {
		for _, s := range statuses {
			if m.Status == s {
				n++
				if len(out) < 3 {
					out = append(out, "「"+clipRunes(machineLabel(m.ID, m.Name), 20)+"」")
				}
			}
		}
	}
	names = strings.Join(out, "、")
	if n > len(out) {
		names += fmt.Sprintf("等 %d 台电脑", n)
	}
	return names, n
}

// afterSearchText says what the 「再找一次」 button does with picked computers: the page never searches them
// by itself; a search waits up to nodeSearchTimeout for each computer. lead starts the sentence.
func afterSearchText(lead string) string {
	return fmt.Sprintf("这个页面不会自己重新搜：%s点下面的「再找一次」（或者在上面的搜索框里搜几个词），才会去选的电脑上搜，一般几秒就有结果，每台电脑最多等 %d 秒。", lead, int(nodeSearchTimeout.Seconds()))
}

// syncText says what the background sync does: once shortly after the extension (re)starts, then every
// nodeSyncInterval, for the picked computers that are online.
func syncText() string {
	return fmt.Sprintf("另外，选的电脑在线时，它的聊天会自动同步一份精简副本过来（设置保存后不久同步第一次，之后每 %d 分钟一次），那台电脑关机以后也能搜到。", int(nodeSyncInterval.Minutes()))
}

// setupSections replace the counts, the quick searches and the empty list of a result that read no chat.
func (d draw) setupSections(res *ExecutionResult, r readiness) []wire.Section {
	text := func(key, s string) wire.Component {
		return wire.Component{Type: wire.ComponentText, Key: key, Text: s}
	}
	again := &wire.EmptyButton{Title: "再找一次", Action: wire.Action{ID: "reset"}}
	var sec wire.Section
	switch r {
	case noComputerPicked:
		// A first-time user reads this, so it is four short lines in the order they act on: what is missing,
		// where to set it, what to press afterwards, and that nothing is broken. The rest is one footer line.
		sec = wire.Section{Title: "还没设置好", Items: []wire.Component{
			{
				Type:   wire.ComponentEmpty,
				Key:    "setup-empty",
				Icon:   "settings",
				Title:  "还没选要搜哪台电脑",
				Detail: "运行 Persona 的这台电脑上没有聊天记录，要选一台你平时用 Claude Code 或 Codex 的电脑。",
			},
			text("setup-where", "去「"+settingsPlace+"」的「"+machinesSetting+"」里选上，然后保存。"),
			text("setup-after", "选好后回到这里，点「再找一次」。"),
			text("setup-ok", "这不是出错了，只是还没设置。"),
			{Type: wire.ComponentButton, Key: "setup-again", Title: "再找一次", Action: &wire.Action{ID: "reset"}},
		}, Footer: fmt.Sprintf("选的电脑要开着并连着 Persona；之后每 %d 分钟会自动同步一份精简副本，它关机后也能搜。聊天记录其实在这台电脑的别的文件夹里的话，不用选电脑，到「搜哪些地方」填上文件夹。", int(nodeSyncInterval.Minutes()))}
	case picksNotSearched:
		n := 0
		if d.fleet != nil {
			n = d.fleet.selectedCount()
		}
		sec = wire.Section{Title: "还没去选的电脑上搜过", Items: []wire.Component{{
			Type:   wire.ComponentEmpty,
			Key:    "setup-empty",
			Icon:   "search",
			Title:  fmt.Sprintf("已经选了 %d 台电脑，还没去那里搜", n),
			Detail: "运行 Persona 的这台电脑上没有聊天记录；现在看到的只是这台电脑的情况，还不是选的电脑上的。" + afterSearchText(""),
			Button: again,
		}}}
	case noPickAnswered:
		names, _ := machineNames(res.Machines, machineUnavailable, machineCached)
		sec = wire.Section{Title: "选的电脑都没连上", Items: []wire.Component{
			{
				Type:   wire.ComponentEmpty,
				Key:    "setup-empty",
				Icon:   "link",
				Title:  "这次一台电脑都没搜到",
				Detail: "去" + names + "上搜的时候，它们都不在线或者没有回应，之前也还没同步过它们的聊天，所以什么都没搜到。这不代表那里没有聊天记录。",
				Button: again,
			},
			text("setup-fix", "怎么办：把这些电脑打开，确认它连着 Persona（在 Persona 的电脑列表里显示为在线），再点「再找一次」。要换别的电脑，到「"+settingsPlace+"」的「"+machinesSetting+"」里改。"),
			text("setup-sync", syncText()),
		}}
	default: // picksHaveNoChats
		where := "你选的电脑上"
		if names, n := machineNames(res.Machines, machineOnline); n > 0 {
			where = names + "都回应了，但它们上面"
		}
		detail := where + "和运行 Persona 的这台电脑上，都没有找到 Claude Code 或 Codex 的聊天记录（默认看 ~/.claude/projects 和 ~/.codex/sessions）。"
		if names, n := machineNames(res.Machines, machineUnavailable); n > 0 {
			detail += "另外" + names + "没连上，没有搜到。"
		}
		sec = wire.Section{Title: "选的电脑上还没有聊天记录", Items: []wire.Component{
			{
				Type:   wire.ComponentEmpty,
				Key:    "setup-empty",
				Icon:   "folder",
				Title:  "确实还没有聊天记录",
				Detail: detail,
				Button: again,
			},
			text("setup-fix", "在你常用的电脑上用 Claude Code 或 Codex 聊几次，再回来点「再找一次」。聊天记录放在别的文件夹的话，到「"+settingsPlace+"」的「"+sessionDirsSetting+"」里填上；要换别的电脑，在同一页的「"+machinesSetting+"」里改。"),
		}}
	}
	return []wire.Section{sec}
}

// cardSetupText is the card's line when the stored run (or none) read no chat, else "", and whether the card
// still says what it is for above it (not when nothing is set up: the line says it all). picked is whether a
// computer is picked now, selected how many; foldersEmpty whether every chat folder of the hub's computer is
// missing or empty. The card never runs a search, so this goes by the stored run and those folders only.
func cardSetupText(res *ExecutionResult, picked, foldersEmpty bool, selected int) (line string, intro bool) {
	if res != nil && (res.ScannedSessions > 0 || len(res.Hits) > 0) {
		return "", false
	}
	switch {
	case !picked && foldersEmpty:
		return "还没设置好：还没选要搜哪台电脑，这台电脑上也没有聊天记录。到「" + settingsPlace + "」的「" + machinesSetting + "」里选一台电脑，再点开这里，点「再找一次」。", false
	case !picked:
		return "", false
	case res == nil || len(res.Machines) == 0:
		if !foldersEmpty {
			return "", false
		}
		return fmt.Sprintf("已经选了 %d 台电脑，还没去那里搜过：点开这里，点「再找一次」。", selected), true
	}
	switch readinessOf(res, true) {
	case noPickAnswered:
		return "上次搜的时候，选的电脑都没连上，什么都没搜到。等它们在线后点开这里，点「再找一次」。", true
	case picksHaveNoChats:
		return "上次搜的时候，选的电脑都回应了，但上面还没有 Claude Code 或 Codex 的聊天记录。", true
	}
	return "", false
}

// chatFoldersEmpty reports whether every chat folder of this computer (the default ones, the configured ones and
// the text indexes of chats moved to the cloud) is missing or empty. It reads only those folders' own entries,
// never the chats, so the card can draw it every time Overview opens.
func chatFoldersEmpty(st *State) bool {
	for _, dir := range append(activeSessionDirs(st), activeColdTextDirs(st)...) {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return false
		}
	}
	return true
}
