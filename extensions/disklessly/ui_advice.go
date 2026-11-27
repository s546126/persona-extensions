package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The 「帮我腾空间」 block of 硬盘空间 shows the first steps of disklessly_advise in plain words:
// each with its gain, its risk and the existing button that does it. Its rows only open the
// page that has that button; it adds no button that deletes or moves anything.

const (
	adviceTitle = "帮我腾空间"
	// adviceShown is how many steps the block lists: a few lines, so the summary above stays in sight.
	adviceShown = 3
	// assistantRowTitle is the row that says what to tell the Persona assistant.
	assistantRowTitle = "让助手帮我腾空间"
	// assistantPrompt is the sentence to say; the quick prompt of the manifest carries it too.
	assistantPrompt = "帮我腾点空间"
	// adviceAllTitle is the pushed page that lists every step, and the row that opens it.
	adviceAllTitle = "帮我腾空间：全部步骤"
	adviceAllRow   = "看全部步骤"
	// adviceAllMost bounds the steps and the 建议先看 items the page lists (wire.ItemsMost).
	adviceAllMost = 120
	// sessionsAdviceShown is how many computers the AI 对话 page's block names.
	sessionsAdviceShown = 3
)

// adviceSection is the block: at most adviceShown step rows and the row about the assistant;
// what else there is goes in the footer, which costs no component.
func (d draw) adviceSection() wire.Section {
	sec := wire.Section{Key: "advice", Title: adviceTitle}
	adv, err := buildAdvice(d.st, d.now, adviceOptions{})
	var items []wire.Component
	switch {
	case err != nil:
		// buildAdvice fails only for an unknown machine, never for all of them.
	case adviceUnchecked(adv):
		items = append(items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary,
			Text: "检查过以后，这里会按先后列出先删什么、能腾出多少、有什么风险。"})
	case len(adv.Steps) == 0:
		text := "按上次检查的结果，现在没有能腾出来的东西。"
		if len(adv.LookFirst) > 0 {
			text = "放心删的都删过了，只剩「建议先看」的，删不删你自己定（在「" + planText + "」里看）。"
		}
		items = append(items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: text})
	default:
		for _, s := range adv.Steps[:min(len(adv.Steps), adviceShown)] {
			items = append(items, d.adviceRow(s))
		}
		var more []string
		if n := len(adv.Steps) - adviceShown; n > 0 {
			more = append(more, fmt.Sprintf("还有 %d 步，点「%s」能看到", n, adviceAllRow))
		}
		if n := len(adv.LookFirst); n > 0 {
			var b uint64
			for _, s := range adv.LookFirst {
				b += s.FreesBytes
			}
			more = append(more, fmt.Sprintf("另有 %d 项「建议先看」的（%s）默认不删，在「%s」里自己决定", n, FormatBytes(b), planText))
		}
		if len(more) > 0 {
			sec.Footer = clip(strings.Join(more, "；")+"。", 300)
		}
		if len(adv.Steps) > adviceShown || len(adv.LookFirst) > 0 {
			items = append(items, adviceAllLink(len(adv.Steps), len(adv.LookFirst)))
		}
	}
	sec.Items = append(items, d.assistantRow())
	return sec
}

// adviceAllLink is the row that opens the page listing every step; an app that does not push
// pages calls advice-all, which draws that page in place.
func adviceAllLink(steps, look int) wire.Component {
	sub := fmt.Sprintf("一共 %d 步", steps)
	if look > 0 {
		sub += fmt.Sprintf("，另有 %d 项「建议先看」", look)
	}
	return wire.Component{
		Type:     wire.ComponentRow,
		Key:      "steps-all",
		Icon:     "list",
		Title:    adviceAllRow,
		Subtitle: sub + "；只是列出来，不会删除或搬走任何东西。",
		Action:   &wire.Action{ID: "advice-all", Push: &wire.Push{Title: adviceAllTitle, State: ext.EncodeState(page{Mode: modeAdvice})}},
	}
}

// adviceAllView is the read-only page of every step and every 建议先看 item, in order. Its rows
// open the page that has each step's button, as the block's rows do.
func (d draw) adviceAllView(p page) *wire.View {
	v := &wire.View{Title: adviceAllTitle, State: ext.EncodeState(p)}
	adv, err := buildAdvice(d.st, d.now, adviceOptions{})
	intro := wire.Section{Items: []wire.Component{{Type: wire.ComponentText, Style: wire.TextStyleSecondary,
		Text: "这里只列出来，不会删除或搬走任何东西。点一步会打开能做这一步的地方，真要删或搬的时候还会先问你。"}}}
	if !d.v3() {
		intro.Items = append(intro.Items, wire.Component{Type: wire.ComponentButton, Title: "返回", Action: &wire.Action{ID: "cancel"}})
	}
	v.Sections = []wire.Section{intro}
	if err != nil || adviceUnchecked(adv) || len(adv.Steps)+len(adv.LookFirst) == 0 {
		v.Sections = append(v.Sections, wire.Section{Items: []wire.Component{{Type: wire.ComponentText, Style: wire.TextStyleSecondary,
			Text: "现在没有能列出来的步骤：检查过电脑以后再来看。"}}})
		return d.fitVersion(v)
	}
	if n := len(adv.Steps); n > 0 {
		// The steps that can be done now, then (under their own heading, numbers going on) the
		// ones that cannot: buildAdvice already put them in that order.
		shown := adv.Steps[:min(n, adviceAllMost)]
		now := 0
		for _, s := range adv.Steps {
			if s.Possible {
				now++
			}
		}
		sec := wire.Section{Key: "advice-all-steps", Title: fmt.Sprintf("按先后的 %d 步", n)}
		if now < n {
			sec.Title = fmt.Sprintf("按先后的 %d 步：现在就能做的 %d 步", n, now)
		}
		later := wire.Section{Key: "advice-all-later", Title: fmt.Sprintf("现在还做不了的 %d 步", n-now),
			Footer: "这些要等那台电脑连得上，或者先照每一步写的办法处理好，才能做。"}
		for _, s := range shown {
			if s.Possible {
				sec.Items = append(sec.Items, d.adviceRow(s))
			} else {
				later.Items = append(later.Items, d.adviceRow(s))
			}
		}
		last := &sec
		if len(later.Items) > 0 {
			last = &later
		}
		if n > adviceAllMost {
			last.Footer = fmt.Sprintf("只列出前 %d 步；其余的问 Persona 助手。", adviceAllMost)
		}
		if len(sec.Items) > 0 {
			v.Sections = append(v.Sections, sec)
		}
		if len(later.Items) > 0 {
			v.Sections = append(v.Sections, later)
		}
	}
	if n := len(adv.LookFirst); n > 0 {
		sec := wire.Section{Key: "advice-all-look", Title: fmt.Sprintf("「建议先看」的 %d 项", n),
			Footer: "这些默认不删：比较大、删了要很久才能拿回来，删不删你自己定。"}
		for _, s := range adv.LookFirst[:min(n, adviceAllMost)] {
			sec.Items = append(sec.Items, d.lookFirstRow(s))
		}
		v.Sections = append(v.Sections, sec)
	}
	v.Sections = append(v.Sections, wire.Section{Items: []wire.Component{d.assistantRow()}})
	return d.fitVersion(v)
}

// lookFirstRow is one 建议先看 item: what, how much and why to look first; tapping it opens the
// computer's 先看看会删什么.
func (d draw) lookFirstRow(s adviceStep) wire.Component {
	title := s.KindName
	if len(s.Items) > 0 {
		title = cmp.Or(strings.TrimSpace(s.Items[0].Title), s.Items[0].ID)
	}
	where := "在「" + planText + "」里自己决定删不删。"
	if !s.Possible {
		where = "现在还做不了：" + d.stepShort(s) + "。"
	}
	return wire.Component{
		Type:     wire.ComponentRow,
		Key:      boundedKey(fmt.Sprintf("look-%d-", s.Rank), s.MachineID),
		Title:    clip(fmt.Sprintf("%d. %s：「%s」", s.Rank, s.MachineName, title), 200),
		Subtitle: clip(strings.TrimRight(s.Risk, "。")+"。"+where, 400),
		Badge:    &wire.Badge{Text: FormatBytes(s.FreesBytes), Tone: wire.ToneCaution},
		Action:   &wire.Action{ID: "open-plan", Values: map[string]string{"m": s.MachineID}},
	}
}

// assistantRow says what to tell the Persona assistant.
func (d draw) assistantRow() wire.Component {
	return wire.Component{
		Type:     wire.ComponentRow,
		Key:      "advice-assistant",
		Icon:     "chat",
		Title:    assistantRowTitle,
		Subtitle: "对 Persona 助手说「" + assistantPrompt + "」（消息框的快捷提示里也有）。它会照这个顺序讲每一步腾出多少、有什么风险，问你做哪一步；每删一样、每搬一次都要你点头。",
	}
}

// sessionsAdviceSection is the AI 对话 page's short block: which computers (of m, or all) have
// week-old conversations to move and what that frees, from the same plan as 帮我腾空间, and how
// to find an old conversation through the assistant. Read-only: its rows only pick a computer.
func (d draw) sessionsAdviceSection(m *Machine) wire.Section {
	sec := wire.Section{Key: "sessions-advice", Title: adviceTitle}
	opt := adviceOptions{}
	if m != nil {
		opt.Machine = m.ID
	}
	var moves []adviceStep
	if adv, err := buildAdvice(d.st, d.now, opt); err == nil {
		for _, s := range adv.Steps {
			if s.Action == adviceMove {
				moves = append(moves, s)
			}
		}
	}
	// The ones that can be done now first.
	slices.SortStableFunc(moves, func(a, b adviceStep) int { return cmp.Compare(boolRank(!a.Possible), boolRank(!b.Possible)) })
	for _, s := range moves[:min(len(moves), sessionsAdviceShown)] {
		title := fmt.Sprintf("%s：一周没动的对话", s.MachineName)
		if s.count > 0 {
			title = fmt.Sprintf("%s：%d 个一周没动的对话", s.MachineName, s.count)
		}
		badge := &wire.Badge{Text: "+" + FormatBytes(s.FreesBytes), Tone: wire.TonePositive}
		sub := "搬到网盘最多腾出 " + FormatBytes(s.FreesBytes) + "，还在用的会留着。在下面它那一行点「" + d.moveButton(s.MachineID) + "」。"
		if !s.Possible {
			badge.Tone = wire.ToneNeutral
			sub = "现在还搬不了：" + d.stepShort(s) + "。处理办法在下面它那一行。"
		}
		row := wire.Component{Type: wire.ComponentRow, Key: boundedKey("move-", s.MachineID), Title: clip(title, 200), Subtitle: clip(sub, 400), Badge: badge}
		if m == nil {
			row.Action = &wire.Action{ID: "machine", Values: map[string]string{"machine": s.MachineID}}
		}
		sec.Items = append(sec.Items, row)
	}
	if n := len(moves) - sessionsAdviceShown; n > 0 {
		sec.Footer = fmt.Sprintf("还有 %d 台电脑也有能搬的对话，在下面各自那一行。", n)
	}
	sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: assistantFindHint})
	return sec
}

// stepShort is why step s cannot be done now, in the pages' words: a conversation move blocked
// by the computer's state names it as the AI 对话 row's badge does.
func (d draw) stepShort(s adviceStep) string {
	if m := d.st.machine(s.MachineID); s.Action == adviceMove && m != nil && m.Reachable {
		if cat := d.st.cold(m.ID); cat.stateCode() != ColdReady {
			return d.pageLabel(m, cat)
		}
	}
	return strings.TrimPrefix(s.short, "现在")
}

// boolRank orders false before true.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// adviceUnchecked reports that no computer of adv was checked yet.
func adviceUnchecked(adv *advice) bool {
	for _, am := range adv.Machines {
		if am.CheckedAt != nil {
			return false
		}
	}
	return true
}

// adviceRow is one step: what, how much, the risk, and where its existing button is. Tapping it
// opens that page (the computer's 能删什么, or its AI 对话), never the deletion itself.
func (d draw) adviceRow(s adviceStep) wire.Component {
	m := d.st.machine(s.MachineID)
	title := fmt.Sprintf("%d. %s：删掉「%s」", s.Rank, s.MachineName, s.KindName)
	if s.Action == adviceMove {
		title = fmt.Sprintf("%d. %s：把%s搬到网盘", s.Rank, s.MachineName, s.KindName)
	}
	badge := &wire.Badge{Text: "+" + FormatBytes(s.FreesBytes), Tone: wire.TonePositive}
	var where string
	switch {
	case !s.Possible:
		badge.Tone = wire.ToneNeutral
		where = "现在还做不了：" + d.stepShort(s) + "。"
		if s.pageTab == sessionsUI {
			where += "到「" + sessionsTitle + "」页看怎么处理。"
		} else {
			where += "等它开机联网后重新检查。"
		}
	case m != nil && d.busy(m):
		where = "这台电脑正在忙别的，忙完再做。"
	case s.pageTab == sessionsUI:
		where = "在「" + sessionsTitle + "」页这台电脑那一行用「" + d.moveButton(s.MachineID) + "」。"
	case len(s.PageButtons) == 1:
		where = "在「能删什么」里用「" + s.PageButtons[0] + "」那一行。"
	default:
		where = "在「能删什么」的「每一项」里逐项删（同类里「建议先看」的那项不在这一步）。"
	}
	act := &wire.Action{ID: "select-machine", Values: map[string]string{"m": s.MachineID, "t": tabItems}}
	if s.pageTab == sessionsUI {
		act = &wire.Action{ID: "open-sessions", Push: &wire.Push{UI: sessionsUI, Title: sessionsTitle, State: ext.EncodeState(sessPage{Machine: s.MachineID})}}
	}
	return wire.Component{
		Type: wire.ComponentRow,
		// No icon: a check mark reads as done; the number says the order.
		Key:      boundedKey(fmt.Sprintf("advice-%d-", s.Rank), s.MachineID),
		Title:    clip(title, 200),
		Subtitle: clip(s.Risk+where, 400),
		Badge:    badge,
		Action:   act,
	}
}

// moveButton is the AI 对话 page's upload button of machine id as it is drawn now: 重试 after a
// failed upload.
func (d draw) moveButton(id string) string {
	if j, ok := d.jobs[id]; ok && !j.running() && j.Kind == jobOffload && j.Err != "" {
		return coldRetryText
	}
	return coldOffloadText
}
