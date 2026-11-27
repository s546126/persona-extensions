package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/wire"
)

func statesByName(t *testing.T) map[string]pageState {
	t.Helper()
	out := map[string]pageState{}
	for _, s := range pageStates(t) {
		out[s.name] = s
	}
	return out
}

// hasStats reports whether a view shows a stat grid (the counts that read as broken when they are all zero).
func hasStats(v *wire.View) bool {
	for _, sec := range v.Sections {
		for _, it := range sec.Items {
			if it.Type == wire.ComponentStatGrid {
				return true
			}
		}
	}
	return false
}

// When a search read no chat at all, the page says which situation it is, in place of zero counts, under the
// intro: (a) no computer picked, computers picked but not searched yet, (b) none answered, (c) no chats there.
func TestFirstOpenSaysWhatToSetUp(t *testing.T) {
	states := statesByName(t)
	cases := []struct {
		state string
		want  []string
		not   []string
	}{
		{"01-empty-results", []string{"还没设置好", "还没选要搜哪台电脑", "「设置 › 扩展 › AI 聊天记录搜索」", "「要搜哪些电脑」", "再找一次", "这不是出错了", "每 15 分钟", "「搜哪些地方」"}, []string{"没连上"}},
		{"08-picked-not-searched-results", []string{"已经选了 2 台电脑，还没去那里搜", "再找一次", "不会自己重新搜"}, []string{"还没选要搜哪台电脑"}},
		{"09-picked-none-answered-results", []string{"选的电脑都没连上", "「Laptop 2」、「desktop」", "这不代表那里没有聊天记录", "再找一次"}, []string{"还没选要搜哪台电脑", "确实还没有聊天记录"}},
		{"10-picked-no-chats-results", []string{"确实还没有聊天记录", "「Laptop 2」、「desktop」都回应了", "「其他聊天记录文件夹（一般不用填）」"}, []string{"还没选要搜哪台电脑", "没连上"}},
	}
	for _, c := range cases {
		s, ok := states[c.state]
		if !ok || s.view == nil {
			t.Fatalf("%s: missing", c.state)
		}
		if hasStats(s.view) {
			t.Errorf("%s: shows counts:\n%s", c.state, s.text)
		}
		if s.view.Sections[0].Title != "这是做什么的" {
			t.Errorf("%s: intro is not first:\n%s", c.state, s.text)
		}
		for _, w := range c.want {
			if !strings.Contains(s.text, w) {
				t.Errorf("%s: lacks %q:\n%s", c.state, w, s.text)
			}
		}
		for _, w := range append(c.not, "找到：0", "搜了：0", "还没找到任何聊天记录", "常用搜索（点一下就搜）") {
			if strings.Contains(s.text, w) {
				t.Errorf("%s: has %q:\n%s", c.state, w, s.text)
			}
		}
	}
	// With chats there, the result is drawn as before: counts, quick searches, hits.
	for _, name := range []string{"02-idle-results", "03-results", "07-machines-results"} {
		s := states[name]
		if !hasStats(s.view) || strings.Contains(s.text, "还没设置好") || strings.Contains(s.text, "选的电脑上还没有聊天记录") {
			t.Errorf("%s: not the plain result:\n%s", name, s.text)
		}
	}
}

// With no computer picked, a first-time user reads four short lines in the order they act on them (what is
// missing, where to choose it, what to press, that nothing is broken); the rest is one secondary footer line.
func TestNothingPickedIsShort(t *testing.T) {
	s := statesByName(t)["01-empty-results"]
	var sec *wire.Section
	for i := range s.view.Sections {
		if s.view.Sections[i].Title == "还没设置好" {
			sec = &s.view.Sections[i]
		}
	}
	if sec == nil {
		t.Fatalf("no setup section:\n%s", s.text)
	}
	var lines []string
	for _, it := range sec.Items {
		switch it.Type {
		case wire.ComponentEmpty:
			lines = append(lines, it.Title, it.Detail)
		case wire.ComponentText:
			if it.Style == wire.TextStyleSecondary || it.Style == wire.TextStyleCaption {
				t.Errorf("secondary text %q belongs in the footer", it.Text)
			}
			lines = append(lines, it.Text)
		}
	}
	order := []string{"还没选要搜哪台电脑", "「设置 › 扩展 › AI 聊天记录搜索」的「要搜哪些电脑」", "点「再找一次」", "这不是出错了"}
	at := 0
	for _, w := range order {
		for at < len(lines) && !strings.Contains(lines[at], w) {
			at++
		}
		if at == len(lines) {
			t.Fatalf("%q missing or out of order in %q", w, lines)
		}
	}
	if len(lines) > 5 {
		t.Errorf("%d lines, want at most 5: %q", len(lines), lines)
	}
	for _, l := range lines {
		if n := len([]rune(l)); n > 60 {
			t.Errorf("line of %d characters, want a short one: %q", n, l)
		}
	}
	if !strings.Contains(sec.Footer, "每 15 分钟") || !strings.Contains(sec.Footer, "「搜哪些地方」") {
		t.Errorf("footer %q", sec.Footer)
	}
	if i := slices.IndexFunc(sec.Items, func(c wire.Component) bool { return c.Type == wire.ComponentButton }); i < 0 || sec.Items[i].Title != "再找一次" {
		t.Errorf("no 再找一次 button: %+v", sec.Items)
	}
}

// The card says the same, without running a search.
func TestCardSaysWhatToSetUp(t *testing.T) {
	states := statesByName(t)
	for name, want := range map[string]string{
		"01-empty-card":                "还没设置好：还没选要搜哪台电脑",
		"08-picked-not-searched-card":  "已经选了 2 台电脑，还没去那里搜过",
		"09-picked-none-answered-card": "选的电脑都没连上",
		"10-picked-no-chats-card":      "上面还没有 Claude Code 或 Codex 的聊天记录",
	} {
		s := states[name]
		if !strings.Contains(s.text, want) || strings.Contains(s.text, "最近的报错") {
			t.Errorf("%s: want %q and no quick searches:\n%s", name, want, s.text)
		}
	}
	if s := states["01-empty-card"]; !strings.Contains(s.text, "「设置 › 扩展 › AI 聊天记录搜索」的「要搜哪些电脑」") {
		t.Errorf("01-empty-card: no settings place:\n%s", s.text)
	}
	for _, name := range []string{"02-idle-card", "03-results-card", "07-machines-card"} {
		if s := states[name]; strings.Contains(s.text, "还没") || !strings.Contains(s.text, "最近的报错") {
			t.Errorf("%s: not the plain card:\n%s", name, s.text)
		}
	}
}

func TestReadinessOf(t *testing.T) {
	run := func(st ...string) []MachineRun {
		var out []MachineRun
		for _, s := range st {
			out = append(out, MachineRun{Status: s})
		}
		return out
	}
	for _, c := range []struct {
		name   string
		res    *ExecutionResult
		picked bool
		want   readiness
	}{
		{"chats here", &ExecutionResult{ScannedSessions: 3}, false, readyToShow},
		{"no run", nil, true, readyToShow},
		{"(a) nothing picked", &ExecutionResult{}, false, noComputerPicked},
		{"picked, run before", &ExecutionResult{}, true, picksNotSearched},
		{"(b) none answered", &ExecutionResult{Machines: run(machineLocal, machineUnavailable, machineUnavailable)}, true, noPickAnswered},
		{"(c) answered", &ExecutionResult{Machines: run(machineLocal, machineOnline)}, true, picksHaveNoChats},
		{"(c) one answered, one not", &ExecutionResult{Machines: run(machineLocal, machineOnline, machineUnavailable)}, true, picksHaveNoChats},
		{"(c) only the hub's own picked", &ExecutionResult{Machines: run(machineLocal)}, true, picksHaveNoChats},
		{"repository hits only", &ExecutionResult{Hits: []*SearchHit{{}}}, false, readyToShow},
	} {
		if got := readinessOf(c.res, c.picked); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestChatFoldersEmpty(t *testing.T) {
	dir := t.TempDir()
	st := &State{SessionDirs: []string{dir, filepath.Join(dir, "missing")}}
	if !chatFoldersEmpty(st) {
		t.Fatal("empty and missing folders")
	}
	if err := os.Mkdir(filepath.Join(dir, "-work-app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if chatFoldersEmpty(st) {
		t.Fatal("a folder with a project in it")
	}
}

// The ids the hub, the assistant's gate, the skill loader and MCP clients address stay as they were; the
// manifest schema has no display field for skills, assistant contributions or MCP servers, so none is added.
func TestContributionIDsUnchanged(t *testing.T) {
	var m struct {
		Contributes map[string][]map[string]any `json:"contributes"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	var got []string
	for point, items := range m.Contributes {
		for _, it := range items {
			got = append(got, point+"/"+it["id"].(string))
			for _, k := range []string{"title", "name", "description"} {
				if _, ok := it[k]; ok && point != "ui" {
					t.Errorf("%s/%s: %s is not a field of this point", point, it["id"], k)
				}
			}
		}
	}
	slices.Sort(got)
	want := []string{"assistant/sessions", "mcp_servers/search-as-code", "skills/search-as-code", "ui/search-as-code", "ui/search-as-code-card"}
	if !slices.Equal(got, want) {
		t.Errorf("contributions %v, want %v", got, want)
	}
	if pageUI != "search-as-code" || cardUI != "search-as-code-card" {
		t.Errorf("ui ids %q %q", pageUI, cardUI)
	}
	for dir, name := range map[string]string{"search-as-code": "search-as-code", "sac-remote": "sac-remote"} {
		raw, err := os.ReadFile(filepath.Join("skills", dir, "SKILL.md"))
		if err != nil || !strings.Contains(string(raw), "\nname: "+name+"\n") {
			t.Errorf("skill %s: name changed (%v)", dir, err)
		}
	}
	var gate struct {
		Command string `json:"command"`
		Tool    struct {
			Name string `json:"name"`
		} `json:"tool"`
	}
	raw, err := os.ReadFile(filepath.Join("assistant", "gate.json"))
	if err != nil || json.Unmarshal(raw, &gate) != nil || gate.Command != "sac" || gate.Tool.Name != "sac" {
		t.Errorf("gate command or tool name changed: %+v %v", gate, err)
	}
}

// The confirmation for switching a computer on says what runs there and what it does and does not do.
func TestComputerConfirmSaysWhatHappens(t *testing.T) {
	var m struct {
		Configuration struct {
			Properties map[string]struct {
				Confirm string `json:"x-confirm"`
			} `json:"properties"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	c := m.Configuration.Properties["machines"].Confirm
	for _, want := range []string{"小程序", "你自己的账号权限", "~/.claude/projects", "~/.codex/sessions", "只读", "不会改", "不会删", "不连外网", "运行 Persona 的电脑", "280", "每 15 分钟", "恢复", "先问你"} {
		if !strings.Contains(c, want) {
			t.Errorf("confirm lacks %q: %s", want, c)
		}
	}
}
