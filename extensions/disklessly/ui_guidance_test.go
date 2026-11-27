package main

import (
	"cmp"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// rowSays is what a computer's row on the AI 对话 page says about its state: the row's own
// detail and, for a computer that is not ready, the plain line drawn right under it.
func rowSays(items []wire.Component) string {
	if len(items) == 0 {
		return ""
	}
	s := items[0].Detail
	if len(items) > 1 && items[1].Type == wire.ComponentText {
		s += items[1].Text
	}
	return s
}

// unreachableErrors are the SSH failures a computer can show, one per cause.
var unreachableErrors = []struct{ name, err, command string }{
	{"timeout", "SSH 连接 build-box 失败：ssh: connect to host build-box port 22: Connection timed out", "ssh -o BatchMode=yes -o ConnectTimeout=10 build-box true"},
	{"login refused", "SSH 连接 build-box 失败：dev@build-box: Permission denied (publickey).", "ssh-copy-id build-box"},
	{"host key", "SSH 连接 build-box 失败：Host key verification failed.", "ssh build-box"},
	{"connection refused", "SSH 连接 build-box 失败：ssh: connect to host build-box port 22: Connection refused", "ssh -o BatchMode=yes -o ConnectTimeout=10 build-box true"},
	{"port forward", "SSH 连接 build-box 失败：ssh: connect to host 127.0.0.1 port 2222: Connection refused", "ssh -o BatchMode=yes -o ConnectTimeout=10 build-box true"},
	{"unknown name", "SSH 连接 build-box 失败：ssh: Could not resolve hostname build-box: Name or service not known", "ssh -G build-box | grep -i '^hostname'"},
	{"network", "SSH 连接 build-box 失败：ssh: connect to host build-box port 22: No route to host", "ssh -o BatchMode=yes -o ConnectTimeout=10 build-box true"},
	{"ssh config", "SSH 连接 build-box 失败：Bad owner or permissions on /etc/ssh/ssh_config.d/20-local.conf", "sudo chown root:root /etc/ssh/ssh_config.d/20-local.conf && sudo chmod 644 /etc/ssh/ssh_config.d/20-local.conf"},
	{"other", "SSH 连接 build-box 失败：kex_exchange_identification: read: Connection reset by peer", "ssh -o BatchMode=yes -o ConnectTimeout=10 build-box true"},
}

// leadsPlainly checks the items drawn for a computer that is not ready: a plain line comes
// first (no command in it), and each command comes after a line saying where to run it.
func leadsPlainly(t *testing.T, what string, items []wire.Component, wantCommand string) {
	t.Helper()
	first := -1
	for i, c := range items {
		if c.Type == wire.ComponentText && strings.Contains(c.Text, "「build-box」") {
			first = i
			break
		}
	}
	if first < 0 {
		t.Fatalf("%s: no plain line naming the computer: %+v", what, items)
	}
	codeAt := -1
	for i, c := range items {
		if c.Type != wire.ComponentCode {
			continue
		}
		codeAt = i
		if i <= first {
			t.Errorf("%s: a command comes before the plain line", what)
		}
		if intro := items[i-1]; intro.Type != wire.ComponentText || intro.Text == items[first].Text {
			t.Errorf("%s: command %q has no line of its own introducing it (%+v)", what, c.Text, intro)
		}
		if wantCommand != "" && c.Text != wantCommand {
			t.Errorf("%s: command changed: %q, want %q", what, c.Text, wantCommand)
		}
	}
	if wantCommand != "" && codeAt < 0 {
		t.Errorf("%s: command %q dropped", what, wantCommand)
	}
	lead := items[first].Text
	for _, w := range []string{"ssh -", "ssh build", "ssh-copy-id", "disklessly backend", "disklessly --", "disklessly doctor", "disklessly init", "brew tap", "curl ", "sudo "} {
		if strings.Contains(lead, w) {
			t.Errorf("%s: the plain line carries a command (%q): %q", what, w, lead)
		}
	}
	if !strings.Contains(lead, "点「") {
		t.Errorf("%s: the plain line names no button to tap first: %q", what, lead)
	}
}

// A computer that cannot be reached says what happened and what to do before the command, on
// both pages, and the command stays exact.
func TestUnreachableComputerLeadsWithPlainWords(t *testing.T) {
	noSandbox(t)
	ctx := context.Background()
	for _, c := range unreachableErrors {
		f := newUploadFleet(t)
		if err := f.a.db().update(func(st *State, now time.Time) error {
			m := st.machine("build-box")
			m.LastError, m.Reachable = c.err, false
			cc := *st.cold("build-box")
			cc.State, cc.Error = ColdOffline, c.err
			st.setCold("build-box", &cc)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		want := c.command
		leadsPlainly(t, "AI 对话 "+c.name, itemsOf(f.view(t, "build-box"), "build-box"), want)
		if c.name != "ssh config" {
			lead := rowSays(itemsOf(f.view(t, "build-box"), "build-box"))
			if c.name == "timeout" || c.name == "network" || c.name == "other" {
				for _, w := range []string{"关机或睡眠", "VPN", "地址或登录方式变了"} {
					if !strings.Contains(lead, w) {
						t.Errorf("%s: everyday cause %q not said: %q", c.name, w, lead)
					}
				}
			}
		}
		for _, tab := range []string{tabFleet, tabItems} {
			res, err := f.a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(page{Machine: "build-box"}), Action: "tab", Values: map[string]string{"tab": tab}}})
			if err != nil {
				t.Fatal(err)
			}
			var items []wire.Component
			for _, s := range res.View.Sections {
				if tab == tabItems || s.Title == "build-box" {
					items = append(items, s.Items...)
					if tab == tabItems {
						break
					}
				}
			}
			leadsPlainly(t, "硬盘空间 "+tab+" "+c.name, items, want)
		}
	}
}

// Each AI 对话 state whose fix has a command leads with plain words and the button to tap.
func TestNotReadyCommandsAreSecondary(t *testing.T) {
	noSandbox(t)
	down := false
	states := []struct {
		name, command string
		set           func(c *ColdCatalog)
	}{
		{"no tool, Homebrew", brewInstallCmd, func(c *ColdCatalog) { c.State, c.Brew, c.BrewKnown = ColdNoCLI, "/opt/homebrew/bin/brew", true }},
		{"no tool, no Homebrew", brewSetupComment + "\n" + brewSetupCommand + "\n" + brewLinuxShellEnv + "\n" + brewInstallCmd,
			func(c *ColdCatalog) { c.State, c.Brew, c.BrewKnown = ColdNoCLI, "", true }},
		{"no drive", noBackendCommand, func(c *ColdCatalog) { c.State = ColdNoBackend }},
		{"drive down", "disklessly doctor", func(c *ColdCatalog) { c.State, c.Backend.Reachable, c.Backend.ID = ColdBackendDown, &down, "nas-drive" }},
		{"drive without repo", "disklessly backend list\ndisklessly backend use <后端 ID>   # 换到已经在用的那个网盘\ndisklessly init                    # 或者：在当前网盘里新建存放目录",
			func(c *ColdCatalog) {
				c.State, c.Backend.Reachable, c.Backend.Error = ColdBackendDown, &down, "repo.json is not in the folder"
			}},
		{"unreadable", "disklessly --json sessions status --check-segments", func(c *ColdCatalog) { c.State, c.Error = ColdUnreadable, "disklessly 没有正常结束" }},
	}
	g := newUploadFleet(t)
	for _, s := range states {
		if err := g.a.db().update(func(st *State, now time.Time) error {
			c := *st.cold("build-box")
			c.Error, c.Backend.Error = "", ""
			s.set(&c)
			st.setCold("build-box", &c)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, m := range []string{"", "build-box"} {
			leadsPlainly(t, "AI 对话 "+s.name, itemsOf(g.view(t, m), "build-box"), s.command)
		}
	}
}

// fiveComputers is a 硬盘空间 page state with five computers, four checked and one unreachable.
func fiveComputers(t *testing.T) *app {
	t.Helper()
	a := newTestApp(t)
	if err := a.db().update(func(st *State, now time.Time) error {
		st.ensureDefaults(now)
		st.Scans["local"] = ParseProbeOutput(st.machine("local"), sampleProbeLocal, now)
		for _, id := range []string{"build-box", "nas-box", "studio-mac", "offline-box"} {
			m, err := st.upsertMachine(MachineInput{ID: id, Host: id, Name: id, Source: "manual"}, now)
			if err != nil {
				return err
			}
			if id == "offline-box" {
				m.LastError, m.Reachable = unreachableErrors[0].err, false
				continue
			}
			st.Scans[id] = ParseProbeOutput(m, sampleProbeLocal, now)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

// The 硬盘空间 page draws charts for the selected computer only: every other one is a row with its
// bar and its buttons, so every action stays one tap away; old page states still open it.
func TestDiskPageCompactForManyComputers(t *testing.T) {
	noSandbox(t)
	a := fiveComputers(t)
	ctx := context.Background()
	for _, sel := range []string{"", "nas-box"} {
		v, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, State: ext.EncodeState(page{Machine: sel})}})
		if err != nil {
			t.Fatal(err)
		}
		checkStrictView(t, v, false)
		assertMobileReadability360px(t, "five computers "+sel, v)
		checkTapNames(t, "five computers "+sel, v)
		want := cmp.Or(sel, "local")
		for _, id := range []string{"local", "build-box", "nas-box", "studio-mac"} {
			charts := findComponentByKey(v, "fleet-cats-"+id) != nil
			if charts != (id == want) {
				t.Errorf("selected %q: %s drawn with charts %v", sel, id, charts)
			}
			for _, key := range []string{"mach-", "prog-", "btn-scan-", "btn-plan-", "btn-clean-"} {
				if findComponentByKey(v, key+id) == nil {
					t.Errorf("selected %q: %s lost %s", sel, id, key)
				}
			}
		}
		// 41: the 帮我腾空间 block's 看全部步骤 row came in 0.5.3.
		if n := sectionItemCount(v.Sections); n > 41 {
			t.Errorf("selected %q: %d components for five computers", sel, n)
		}
	}
	// Saved states of earlier versions: the old sessions tab, a computer no longer there.
	for _, st := range []map[string]string{{"t": "sessions", "sq": "checkout"}, {"m": "gone-box"}, {"m": "build-box", "t": "fleet", "x": "scan-offline-box"}} {
		v, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, State: ext.EncodeState(st)}})
		if err != nil || v == nil {
			t.Fatalf("state %s: %v", st, err)
		}
		checkStrictView(t, v, false)
	}
}
