package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// diskItemsOf is what the 硬盘空间 page draws for computer id: its section on 所有电脑, or every
// section of its own tab.
func diskItemsOf(v *wire.View, title string, own bool) []wire.Component {
	var out []wire.Component
	for _, s := range v.Sections {
		if own || s.Title == title {
			out = append(out, s.Items...)
		}
	}
	return out
}

// checkButtons are the titles of the buttons in items that check computer id again: scan on the
// 硬盘空间 page, cold-refresh on AI 对话; an empty state's button counts too.
func checkButtons(items []wire.Component, id string) []string {
	var out []string
	for _, c := range items {
		if c.Type == wire.ComponentButton && c.Action != nil && (c.Action.ID == "scan" || c.Action.ID == "cold-refresh") && c.Action.Values["m"] == id {
			out = append(out, c.Title)
		}
		if c.Button != nil && c.Button.Action.ID == "scan" && c.Button.Action.Values["m"] == id {
			out = append(out, c.Button.Title)
		}
	}
	return out
}

func componentsText(items []wire.Component) string {
	var b strings.Builder
	for _, c := range items {
		if c.Type == wire.ComponentCode {
			continue
		}
		b.WriteString(strings.Join([]string{c.Title, c.Subtitle, c.Detail, c.Text, c.Caption}, "\n"))
		if c.Badge != nil {
			b.WriteString("\n" + c.Badge.Text)
		}
		for _, bd := range c.Badges {
			b.WriteString("\n" + bd.Text)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// A computer in the same situation gets the same state words and the same check button on both
// pages: 检查 before the first check, 重新检查 after a check that reached it, 重试 after one that
// could not; 连不上这台电脑 when SSH fails, 这台电脑连不上网盘 when only its network drive does
// not answer, and neither page ever says the old 现在连不上 or a bare 网盘连不上.
func TestBothPagesAgreeOnStatesAndButtons(t *testing.T) {
	reach, down := true, false
	type situation struct {
		name    string
		set     func(st *State, m *Machine, c *ColdCatalog)
		button  string // the check button on both pages
		offline bool   // both pages say 连不上这台电脑
		badge   string // the AI 对话 row's badge
	}
	situations := []situation{{
		name: "never checked",
		set: func(st *State, m *Machine, c *ColdCatalog) {
			delete(st.Scans, m.ID)
			m.LastError, m.Reachable = "", true
			*c = ColdCatalog{MachineID: m.ID}
		},
		button: "检查 build-box", badge: coldStateLabel(ColdUnchecked),
	}}
	for _, r := range []struct {
		state string
		set   func(c *ColdCatalog)
	}{
		{ColdReady, func(c *ColdCatalog) { c.Backend.Reachable = &reach }},
		{ColdNoCLI, func(c *ColdCatalog) { c.Brew, c.BrewKnown = "", true }},
		{ColdCLIOld, func(c *ColdCatalog) { c.Brew, c.BrewKnown = "/opt/homebrew/bin/brew", true }},
		{ColdNoBackend, func(c *ColdCatalog) {}},
		{ColdBackendDown, func(c *ColdCatalog) { c.Backend.Reachable, c.Backend.ID = &down, "nas-drive" }},
	} {
		situations = append(situations, situation{
			name: "checked, " + r.state,
			set: func(st *State, m *Machine, c *ColdCatalog) {
				m.LastError, m.Reachable = "", true
				c.State, c.Error = r.state, ""
				r.set(c)
			},
			button: "重新检查 build-box", badge: coldStatePageLabel(r.state),
		})
	}
	for _, cause := range unreachableErrors {
		situations = append(situations, situation{
			name: "unreachable, " + cause.name,
			set: func(st *State, m *Machine, c *ColdCatalog) {
				m.LastError, m.Reachable = cause.err, false
				c.State, c.Error = ColdOffline, cause.err
			},
			button: "重试 build-box", offline: true, badge: offlineText,
		})
	}
	for _, s := range situations {
		a := mixedFleet(t)
		if err := a.db().update(func(st *State, now time.Time) error {
			m := st.machine("build-box")
			c := *st.cold("build-box")
			c.Backend.Error = ""
			s.set(st, m, &c)
			st.setCold("build-box", &c)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		pages := map[string][]wire.Component{
			"硬盘空间 所有电脑":       diskItemsOf(diskView(t, a, "build-box", tabFleet), "build-box", false),
			"硬盘空间 能删什么":       diskItemsOf(diskView(t, a, "build-box", tabItems), "", true),
			"AI 对话 build-box": itemsOf(sessView(t, a, "build-box"), "build-box"),
			"AI 对话 全部电脑":      itemsOf(sessView(t, a, ""), "build-box"),
		}
		for page, items := range pages {
			what := s.name + " / " + page
			if len(items) == 0 {
				t.Fatalf("%s: nothing drawn for build-box", what)
			}
			got := checkButtons(items, "build-box")
			if len(got) == 0 {
				t.Errorf("%s: no check button", what)
			}
			for _, b := range got {
				if b != s.button {
					t.Errorf("%s: check button %q, want %q", what, b, s.button)
				}
			}
			txt := componentsText(items)
			if says := strings.Contains(txt, offlineText); says != s.offline {
				t.Errorf("%s: says %s = %v, want %v:\n%s", what, offlineText, says, s.offline, txt)
			}
			for _, old := range []string{"现在连不上", "「网盘连不上」"} {
				if strings.Contains(txt, old) {
					t.Errorf("%s: still says %s:\n%s", what, old, txt)
				}
			}
			if strings.HasPrefix(page, "AI 对话") {
				if b := items[0].Badge; b == nil || b.Text != s.badge {
					t.Errorf("%s: badge %+v, want %q", what, b, s.badge)
				}
				if s.badge == backendDownText && !strings.Contains(txt, "能连上「build-box」，但它连不上自己用的网盘") {
					t.Errorf("%s: does not say the computer answers:\n%s", what, txt)
				}
			}
		}
	}

	// The badge and the summary count are the same words; MCP keeps its state_label.
	a := mixedFleet(t)
	v := sessView(t, a, "")
	if !strings.Contains(visibleText(v), "1 台"+noRepoText) || strings.Contains(visibleText(v), "网盘连不上 ·") {
		t.Errorf("summary:\n%s", visibleText(v))
	}
	if coldStateLabel(ColdBackendDown) != "网盘连不上" {
		t.Errorf("MCP state label changed: %q", coldStateLabel(ColdBackendDown))
	}
}

// When one page's check is older than the other's, the row says what the newer one found.
func TestPagesSayWhenTheOtherFoundItUnreachable(t *testing.T) {
	a := mixedFleet(t)
	down := false
	if err := a.db().update(func(st *State, now time.Time) error {
		c := *st.cold("studio-mac")
		c.Backend.Error, c.Backend.Reachable = "dial tcp: i/o timeout", &down
		c.CheckedAt = now.Add(-5 * time.Hour)
		st.setCold("studio-mac", &c)
		m := st.machine("studio-mac")
		m.LastError, m.Reachable = unreachableErrors[0].err, false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	setJob(a, "studio-mac", jobScan, unreachableErrors[0].err)
	row := itemsOf(sessView(t, a, "studio-mac"), "studio-mac")[0]
	if !strings.Contains(row.Subtitle, "5 小时前检查") || !strings.Contains(row.Subtitle, "「硬盘空间」页检查时连不上这台电脑") {
		t.Errorf("AI 对话 row: %q", row.Subtitle)
	}
	disk := diskItemsOf(diskView(t, a, "studio-mac", tabFleet), "studio-mac", false)
	if !strings.Contains(disk[0].Subtitle, "检查时连不上这台电脑") {
		t.Errorf("硬盘空间 row: %q", disk[0].Subtitle)
	}

	// The AI 对话 check reached it after the 硬盘空间 check failed: that line says so.
	b := mixedFleet(t)
	if err := b.db().update(func(st *State, now time.Time) error {
		m := st.machine("build-box")
		m.LastError, m.Reachable = unreachableErrors[0].err, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	txt := componentsText(diskItemsOf(diskView(t, b, "build-box", tabFleet), "build-box", false))
	// The newest check reached it: 重新检查, as on AI 对话.
	if !strings.Contains(txt, "不过「AI 对话」页刚刚检查时连上了它。") || !strings.Contains(txt, "点「重新检查 build-box」") {
		t.Errorf("硬盘空间 after a later AI 对话 check:\n%s", txt)
	}
}

// nestedBracket returns the first bracket opened inside another one in s, or "".
func nestedBracket(s string) string {
	depth := 0
	for i, r := range s {
		switch r {
		case '（', '(':
			depth++
			if depth > 1 {
				return s[max(0, i-30):min(len(s), i+30)]
			}
		case '）', ')':
			depth = max(0, depth-1)
		}
	}
	return ""
}

// A computer without the companion tool: each fact once, in short sentences, no bracket inside a
// bracket, and copying is ruled out once with its reason; the same for the copy confirm.
func TestInstallGuidanceSaysEachFactOnce(t *testing.T) {
	noSandbox(t)
	type variant struct {
		name  string
		setup func(st *State)
	}
	homebrewCLI := ColdCLI{Version: "disklessly 0.3.0", Path: "/home/linuxbrew/.linuxbrew/bin/disklessly",
		Real: "/home/linuxbrew/.linuxbrew/Cellar/disklessly-cli/0.3.0/bin/disklessly", Home: "/home/linuxbrew/.linuxbrew/Cellar/disklessly-cli/0.3.0", KB: 9000}
	ownDir := ColdCLI{Version: "disklessly 0.3.0", Path: "/home/dev/.local/bin/disklessly",
		Real: "/home/dev/.local/opt/disklessly-0.3.0-linux-arm64/bin/disklessly", Home: "/home/dev/.local/opt/disklessly-0.3.0-linux-arm64", KB: 9000}
	source := func(cli ColdCLI) func(st *State) {
		return func(st *State) {
			c := *st.cold("nas-box")
			c.State, c.Platform, c.CLI = ColdReady, "linux-arm64", cli
			st.setCold("nas-box", &c)
			st.machine("nas-box").Platform = "linux-arm64"
		}
	}
	variants := []variant{
		{"no source", func(st *State) {}},
		{"source not self-contained", source(homebrewCLI)},
		{"source", source(ownDir)},
		{"platform unknown", func(st *State) {
			c := *st.cold("build-box")
			c.Platform = ""
			st.setCold("build-box", &c)
			st.machine("build-box").Platform = ""
		}},
	}
	for _, state := range []string{ColdNoCLI, ColdCLIOld} {
		for _, brewKnown := range []bool{true, false} {
			for _, vr := range variants {
				what := fmt.Sprintf("%s, Homebrew known %v, %s", state, brewKnown, vr.name)
				a := mixedFleet(t)
				if err := a.db().update(func(st *State, now time.Time) error {
					c := *st.cold("build-box")
					c.State, c.Brew, c.BrewKnown = state, "", brewKnown
					st.setCold("build-box", &c)
					vr.setup(st)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				items := itemsOf(sessView(t, a, "build-box"), "build-box")
				lead := rowSays(items)
				var adv coldAdvice
				if err := a.db().read(func(st *State) {
					adv = coldProblem(st, st.machine("build-box"), st.cold("build-box"), state)
				}); err != nil {
					t.Fatal(err)
				}
				for _, txt := range []string{lead, adv.Reason, adv.Next, adv.Lead} {
					if n := nestedBracket(txt); n != "" {
						t.Errorf("%s: bracket inside a bracket at %q in %q", what, n, txt)
					}
					if strings.Contains(txt, "没法复制") || strings.Contains(txt, "不能复制") || strings.Count(txt, "不能从别的电脑复制过来") > 1 {
						t.Errorf("%s: says it cannot copy more than once: %q", what, txt)
					}
				}
				copyBtn := buttonIn(items, "从 ")
				if vr.name == "source" {
					if copyBtn == nil {
						t.Fatalf("%s: no copy button", what)
					}
					if n := nestedBracket(copyBtn.Action.Confirm); n != "" {
						t.Errorf("%s: confirm nests brackets at %q", what, n)
					}
					continue
				}
				if copyBtn != nil {
					t.Fatalf("%s: offers a copy", what)
				}
				if strings.Count(lead, "不能从别的电脑复制过来：") != 1 {
					t.Errorf("%s: copying is not ruled out once with its reason: %q", what, lead)
				}
			}
		}
	}
	// No visible text on either page puts a bracket inside a bracket.
	for name, v := range allViews(t, mixedFleet(t)) {
		for _, line := range strings.Split(visibleText(v), "\n") {
			if n := nestedBracket(line); n != "" {
				t.Errorf("%s: bracket inside a bracket at %q", name, n)
			}
		}
	}
}

// A computer that is not ready: the plain line, then the buttons it names and 详情, then the
// command block (after its one-line introduction) last, on both pages.
func TestNotReadyButtonsComeBeforeCommands(t *testing.T) {
	check := func(what string, items []wire.Component) {
		t.Helper()
		lead, firstButton, lastButton, intro, code, details := -1, -1, -1, -1, -1, -1
		for i, c := range items {
			switch {
			case c.Type == wire.ComponentButton && c.Title == "详情":
				details = i
			case c.Type == wire.ComponentText && lead < 0 && c.Style == "":
				lead = i
			case (c.Type == wire.ComponentButton || c.Type == wire.ComponentLink) && c.Title != "详情" && c.Title != "收起详情":
				if firstButton < 0 {
					firstButton = i
				}
				lastButton = i
			case c.Type == wire.ComponentCode && code < 0:
				code = i
			}
		}
		if code > 0 && items[code-1].Type == wire.ComponentText {
			intro = code - 1
		}
		if lead < 0 || firstButton < 0 || code < 0 {
			t.Fatalf("%s: lead %d, buttons %d, code %d:\n%s", what, lead, firstButton, code, componentsText(items))
		}
		if !(lead < firstButton && lastButton < intro && intro < code) || (details >= 0 && !(lastButton < details && details < intro)) {
			t.Errorf("%s: order lead %d, buttons %d-%d, 详情 %d, intro %d, code %d of %d:\n%s", what, lead, firstButton, lastButton, details, intro, code, len(items), componentsText(items))
		}
	}
	a := mixedFleet(t)
	for _, id := range []string{"build-box", "nas-box", "studio-mac", "offline-box"} {
		check("AI 对话 "+id, itemsOf(sessView(t, a, id), id))
		check("AI 对话 全部电脑 "+id, itemsOf(sessView(t, a, ""), id))
	}
	check("硬盘空间 所有电脑 offline-box", diskItemsOf(diskView(t, a, "offline-box", tabFleet), "offline-box", false))
	check("硬盘空间 能删什么 offline-box", diskItemsOf(diskView(t, a, "offline-box", tabItems), "", true)[1:])
	for _, cause := range unreachableErrors {
		b := mixedFleet(t)
		if err := b.db().update(func(st *State, now time.Time) error {
			m := st.machine("build-box")
			m.LastError, m.Reachable = cause.err, false
			c := *st.cold("build-box")
			c.State, c.Error = ColdOffline, cause.err
			st.setCold("build-box", &c)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		check("AI 对话 "+cause.name, itemsOf(sessView(t, b, "build-box"), "build-box"))
		check("硬盘空间 "+cause.name, diskItemsOf(diskView(t, b, "build-box", tabFleet), "build-box", false))
	}
}

func TestFirstMessageWithoutContentMakesNoTitle(t *testing.T) {
	for _, p := range []string{"reply OK", "Reply with OK.", "ok", "OK!", "hi", "hello", "test", "testing 123", "继续", "继续吧", "好的，继续",
		"测试一下", "嗯", "/clear", "/compact", "/review   ", "？？", "...", "deploy", "修复", "42", "say hi", "thank you", "  ok  \n"} {
		if promptHasContent(oneLine(p)) || promptTitle(p) != "" {
			t.Errorf("%q makes a title", p)
		}
	}
	for _, p := range []string{"fix tests", "Please fix the checkout flow", "帮我看看为什么 nightly 构建总是失败", "构建失败", "继续上次的数据库迁移",
		"/review fix the flaky upload test", "test the upload retry", "部署到 staging", "長い会話のタイトル"} {
		if !promptHasContent(oneLine(p)) || promptTitle(p) == "" {
			t.Errorf("%q makes no title", p)
		}
	}

	// On the page: trivial first messages read as untitled conversations with their time, the
	// message itself in the detail; real ones that repeat carry their date and time; search
	// still finds them by their first message.
	a := newTestApp(t)
	var entries []map[string]any
	prompts := []string{"reply OK", "reply OK", "reply OK", "reply OK", "reply OK", "reply OK", "reply OK", "hi", "继续", "/clear",
		"帮我看看为什么构建总是失败", "帮我看看为什么构建总是失败"}
	for i, p := range prompts {
		entries = append(entries, map[string]any{"id": fmt.Sprintf("e%011d", i), "local": true, "tool": "claude",
			"path": fmt.Sprintf("/home/dev/.claude/projects/x/%d.jsonl", i), "sessionId": fmt.Sprintf("%08d-0000-4000-8000-000000000000", i),
			"project": "shop", "mtime": fmt.Sprintf("2026-11-%02dT%02d:%02d:00Z", 1+i%3, 8+i, i*4),
			"bytes": 4096, "state": "archived", "firstPrompt": p, "segment": "seg-a"})
	}
	a.runner = coldListRunner(entries)
	refreshAll(t, a)
	rows := conversationRows(sessionsSearch(t, a, "", ""))
	if len(rows) != len(prompts) {
		t.Fatalf("%d rows", len(rows))
	}
	titles := map[string]int{}
	shown := map[string]int{}
	for _, r := range rows {
		titles[r.Title]++
		shown[r.Title+"|"+r.Subtitle]++
		switch {
		case strings.HasPrefix(r.Title, untitledText):
			if !strings.Contains(r.Detail, "第一句话：") {
				t.Errorf("untitled row hides its first message: %+v", r)
			}
		case r.Title == "帮我看看为什么构建总是失败":
			if !clockRE.MatchString(r.Subtitle) {
				t.Errorf("a title from the first message has no date and time: %q", r.Subtitle)
			}
		default:
			t.Errorf("title %q", r.Title)
		}
	}
	for title, n := range titles {
		if n > 1 && title != "帮我看看为什么构建总是失败" {
			t.Errorf("%d rows titled %q", n, title)
		}
	}
	for k, n := range shown {
		if n > 1 {
			t.Errorf("%d rows read the same: %q", n, k)
		}
	}
	if n := len(conversationRows(sessionsSearch(t, a, "", "reply OK"))); n != 7 {
		t.Errorf("search reply OK: %d rows", n)
	}
	if n := len(conversationRows(sessionsSearch(t, a, "", "继续"))); n != 1 {
		t.Errorf("search 继续: %d rows", n)
	}
}

// coldListRunner answers a cold refresh with entries on every computer.
func coldListRunner(entries []map[string]any) Runner {
	return func(ctx context.Context, m *Machine, s string, d time.Duration) (string, error) {
		if strings.Contains(s, "sessions list --state all") {
			return coldRefreshOut(coldListJSON(entries...), coldStatusJSON), nil
		}
		return fakeRunner(ctx, m, s, d)
	}
}

// refreshAll checks every computer on the AI 对话 page and waits for it.
func refreshAll(t *testing.T, a *app) {
	t.Helper()
	if _, err := a.actionSessions(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Action: "cold-refresh", Values: map[string]string{"m": ""}}}); err != nil {
		t.Fatal(err)
	}
	a.waitJobs()
}
