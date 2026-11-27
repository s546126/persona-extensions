// Package relay tells a user-role text that is not the user's own words (0.2.0-beta.37, docs/sources.md "Who said
// it"): a task prompt Persona or the assistant sent an agent, a supervisor's or the assistant's relay, the context the
// hub puts in front of a prompt, a test prompt, and material pasted in. In agent sessions and the assistant's
// conversation the model role "user" is whoever typed into the session, and before the hub marked automated inputs
// (proto.Item.Source, Injected, From) those all looked like the user; entries such as "the user wants only OK as the
// answer" came from them. The rules look at the structure of the text only, never at any one user's names or
// projects.
package relay

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// What a user-role text is when it is not the user's words.
const (
	None    = ""        // the user's own
	Persona = "persona" // a task prompt, a relay, hub context
	Test    = "test"    // a probe: "reply only OK, call no tool"
)

// markers make a whole text Persona's: the hub's context block, its task-handoff boilerplate and result-file note, a
// cross-session message, a relay marked as such.
var markers = []string{
	"<persona-context", "<jarvis-context", "Persona task handoffs:", "<cross-session-message", "[Cross-session",
	"（Persona：任务的结果", "<task-notification",
	"经主对话转", "主对话转达", "主管转达", "（转自主管", "relayed by the supervisor", "relayed from the assistant",
	// The platform's task-brief boilerplate (0.2.0-beta.52): the credential note, the supervisor's report tool and
	// acceptance list, the result-file line. A brief carrying them is the supervisor's, not the user's habit.
	"Keep hub credentials private", "report_to_supervisor", "handoff_receipt", "done_when", "file: <",
}

// probe: a short text that asks for a fixed answer or no tool calls, the shape of a deployment or smoke test.
var (
	probeAnswer = regexp.MustCompile(`(?i)(只|仅|只要)(需要)?(回复|回答|回)|reply (only )?with|respond (only )?with|answer (only )?with|just (reply|say|answer)|reply only`)
	probeTools  = regexp.MustCompile(`(?i)不要?(调用|使用|用)(任何)?工具|别(调用|用)工具|(do not|don't) (call|use) (any )?tools?|without (calling|using) (any )?tools?`)
	probeTest   = regexp.MustCompile(`(?i)(部署|上线|冒烟|连通|联通)?(验证|测试|自检)|smoke|ping|health ?check|deploy(ment)? (check|test|verification)`)
)

const probeMost = 120 // runes: a probe is a line or two

// Classify says what a user-role text is: None when it is the user's own words.
func Classify(text string) string {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return Persona
		}
	}
	own, _ := SplitPaste(text)
	t := strings.TrimSpace(own)
	if t != "" && utf8.RuneCountInString(t) <= probeMost {
		a, tl, ts := probeAnswer.MatchString(t), probeTools.MatchString(t), probeTest.MatchString(t)
		if (a && tl) || (a && ts) || (tl && ts) {
			return Test
		}
	}
	return None
}

var pasted = regexp.MustCompile(`(?s)<pasted_content\b[^>]*>.*?</pasted_content\b[^>]*>`)

// SplitPaste takes out the blocks the client marks as pasted (<pasted_content …>…</pasted_content …>): what is left is
// what the user typed; pasted says how many characters were pasted. A pasted block is material, not the user's words,
// even when the user pasted it.
func SplitPaste(text string) (own string, pastedRunes int) {
	own = pasted.ReplaceAllStringFunc(text, func(m string) string {
		pastedRunes += utf8.RuneCountInString(m)
		return " "
	})
	return strings.TrimSpace(own), pastedRunes
}

// PasteMark is the short mark left where pasted material was taken out.
func PasteMark(n int) string { return fmt.Sprintf("[粘贴材料 %d 字]", n) }

var (
	// boilerplateBlocks are platform-injected XML/tag blocks stripped before text reaches a model.
	boilerplateBlocks = regexp.MustCompile(`(?s)<(persona-context|jarvis-context|system-reminder|cross-session-message|task-notification|persona-supervision)\b[^>]*>.*?</(persona-context|jarvis-context|system-reminder|cross-session-message|task-notification|persona-supervision)>`)
	// boilerplateResultBlock is the multi-line Persona file-delivery footer appended to task prompts.
	boilerplateResultBlock = regexp.MustCompile(`(?s)（Persona：任务的结果是要交给用户的文件时.*?）`)
	// boilerplateLineMarkers identify single platform boilerplate lines (handoff, credential, scratch, file template).
	boilerplateLineMarkers = []string{
		"Persona task handoffs:",
		"Keep hub credentials private",
		"临时文件和草稿请放在仓库工作区外",
		"这是 Persona 的任务通知",
		"这是 Persona 的督办消息",
		"这是 Persona 的收件箱通知",
		"不是用户发的消息",
		"not a message from the user",
		"（Persona 叫醒你",
		"（Persona 叫你接着做）",
	}
	boilerplateFileTemplate = regexp.MustCompile(`^\s*file:\s*<[^>]+>\s*$`)
)

// StripBoilerplate deterministically removes Persona platform system prompts, injected context blocks, and task
// handoff footers before text is sent to L0 extraction models. Substantive user or task text is preserved.
func StripBoilerplate(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	s := boilerplateBlocks.ReplaceAllString(text, "\n")
	s = boilerplateResultBlock.ReplaceAllString(s, "\n")
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if boilerplateFileTemplate.MatchString(t) {
			continue
		}
		drop := false
		for _, m := range boilerplateLineMarkers {
			if strings.Contains(t, m) {
				drop = true
				break
			}
		}
		if drop {
			continue
		}
		kept = append(kept, t)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// Who a line is by, in three values (design v2 §4.2): the user's own words, the user's words relayed verbatim by
// Persona or an agent and verified against what the user really said, or anyone else (an agent, Persona, a probe,
// pasted material). What cannot be verified is never guessed: it is WhoAgent.
const (
	WhoUser    = "user"
	WhoRelayed = "relayed"
	WhoAgent   = "agent"
)

// RelayedMark starts the body of a line that carries the user's words relayed verbatim and verified: the line keeps
// the user's speaker, so every check that reads the user's own lines takes it, and the mark keeps it told apart.
const RelayedMark = "[转达原话] "

// Three is who a line is by, from its speaker label and body: the user's label (norm.Me, passed in as me) with
// RelayedMark is relayed, without it the user's own, anything else an agent's.
func Three(speaker, body, me string) string {
	switch {
	case speaker != me:
		return WhoAgent
	case strings.HasPrefix(body, RelayedMark):
		return WhoRelayed
	}
	return WhoUser
}

// quoteHead is a heading or a lead-in line that opens a block of the user's words in a task prompt or a relay.
var quoteHead = regexp.MustCompile(`(?i)^\s*(#{1,6}\s*)?(\*\*)?\s*(用户|他|她|本人)?(的)?原话|^\s*(#{1,6}\s*)?(the )?user'?s (own |exact )?words|^\s*(#{1,6}\s*)?verbatim`)

var (
	heading    = regexp.MustCompile(`^\s*#{1,6}\s`)
	bullet     = regexp.MustCompile(`^\s*([-*•·]|\d+[.、)）])\s*`)
	stampLead  = regexp.MustCompile(`^\s*(\d{4}-)?\d{1,2}-\d{1,2}\s*\d{1,2}:\d{2}\s*[，,：:]?\s*`)
	bracketed  = regexp.MustCompile(`[「“"]([^」”"\n]{2,400})[」”"]`)
	inlineSaid = regexp.MustCompile(`(?:原话|用户(?:说|写道|回复|答|原话是?))\s*[：:]?\s*[「“"]([^」”"\n]{2,400})[」”"]`)
)

// quotesMost bounds what one text gives: a relay carries a few of the user's sentences, not a transcript.
const quotesMost = 20

// Quotes are the user's words a task prompt or a relay says it carries verbatim: the lines of a "用户原话" (the
// user's words) block, the bracketed parts of them when there are any, and anything bracketed after "原话" or "用户说"
// elsewhere. They are claims only: a caller keeps one as the user's words when it finds them in what the user really
// said (the day's own lines, or the hub's ledger), never on the relay's word alone.
func Quotes(text string) []string {
	var out []string
	add := func(q string) {
		q = strings.TrimSpace(q)
		if utf8.RuneCountInString(q) < 2 || len(out) >= quotesMost {
			return
		}
		if r := []rune(q); len(r) > 400 {
			q = string(r[:400])
		}
		for _, x := range out {
			if x == q {
				return
			}
		}
		out = append(out, q)
	}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		if !quoteHead.MatchString(lines[i]) {
			continue
		}
		// The heading line itself may carry the words: "用户原话：「…」".
		for _, m := range bracketed.FindAllStringSubmatch(lines[i], -1) {
			add(m[1])
		}
		for j := i + 1; j < len(lines) && j <= i+20; j++ {
			l := lines[j]
			if heading.MatchString(l) || strings.HasPrefix(strings.TrimSpace(l), "<") {
				break
			}
			l = stampLead.ReplaceAllString(bullet.ReplaceAllString(l, ""), "")
			if strings.TrimSpace(l) == "" {
				continue
			}
			if ms := bracketed.FindAllStringSubmatch(l, -1); len(ms) > 0 {
				for _, m := range ms {
					add(m[1])
				}
				continue
			}
			t := strings.TrimSpace(l)
			if strings.HasSuffix(t, "：") || strings.HasSuffix(t, ":") {
				continue // a lead-in ("the user settled two points:"), not the words
			}
			add(t)
		}
	}
	for _, m := range inlineSaid.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	return out
}
