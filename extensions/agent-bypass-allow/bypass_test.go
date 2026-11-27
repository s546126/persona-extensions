package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestManifest(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
}

func TestConformance(t *testing.T) {
	app := &hubApp{call: fakeNodes(nil)}
	exttest.Check(t, manifest, exttest.Serve(t, app.program(currentExtensionID, version(t))), json.RawMessage(`{"machines":["m1"]}`))
}

func version(t *testing.T) string {
	var m struct{ Version string }
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	return m.Version
}

// env is a computer: a config directory, the node program's data directory with a fresh heartbeat, a project.
type env struct{ config, data, project string }

func newEnv(t *testing.T) env {
	t.Helper()
	root := t.TempDir()
	e := env{config: filepath.Join(root, "claude"), data: filepath.Join(root, "data"), project: filepath.Join(root, "work", "proj")}
	for _, d := range []string{e.config, e.data, e.project} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(e.data, heartbeatFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e env) hook(t *testing.T, req map[string]any, now time.Time) string {
	t.Helper()
	b, _ := json.Marshal(req)
	var out bytes.Buffer
	runHook(hookArgs{ConfigDir: e.config, Data: e.data}, bytes.NewReader(b), &out, now)
	return out.String()
}

func request(e env, mode, tool, cmd string) map[string]any {
	return map[string]any{"hook_event_name": "PermissionRequest", "session_id": "93bed2c0-0000-4000-8000-000000000000",
		"cwd": e.project, "permission_mode": mode, "tool_name": tool,
		"tool_input": map[string]any{"command": cmd, "description": "remove the views"}, "permission_suggestions": []any{}}
}

func writeJSON(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHookAllowsOnlyItsToolsInBypass(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	cmd := "cd view && ls sub/*; make"
	if got := e.hook(t, request(e, "bypassPermissions", "Bash", cmd), now); strings.TrimSpace(got) != allowOutput {
		t.Fatalf("bypass + Bash: %q", got)
	}
	for _, c := range []struct{ name, mode, tool string }{
		{"default mode", "default", "Bash"},
		{"accept edits", "acceptEdits", "Bash"},
		{"auto mode", "auto", "Bash"},
		{"plan mode", "plan", "Bash"},
		{"no mode", "", "Bash"},
		{"Write", "bypassPermissions", "Write"},
		{"Edit", "bypassPermissions", "Edit"},
		{"NotebookEdit", "bypassPermissions", "NotebookEdit"},
		{"Workflow", "bypassPermissions", "Workflow"},
		{"WebFetch", "bypassPermissions", "WebFetch"},
		{"an MCP tool", "bypassPermissions", "mcp__x__run"},
	} {
		if got := e.hook(t, request(e, c.mode, c.tool, cmd), now); got != "" {
			t.Errorf("%s: answered %q, should stay silent", c.name, got)
		}
	}
	if got := e.hook(t, request(e, "bypassPermissions", "Bash", "  "), now); got != "" {
		t.Errorf("an empty command: %q", got)
	}
	var out bytes.Buffer
	runHook(hookArgs{ConfigDir: e.config, Data: e.data}, strings.NewReader("not json"), &out, now)
	if out.Len() != 0 {
		t.Errorf("bad input: %q", out.String())
	}
	if n := len(readAudit(e.data, 100)); n != 1 {
		t.Errorf("audit has %d entries, want the one allow", n)
	}
}

func TestHookNeedsTheNodeProgram(t *testing.T) {
	e := newEnv(t)
	req := request(e, "bypassPermissions", "Bash", "ls \"$t\"")
	if got := e.hook(t, req, time.Now().Add(heartbeatFresh+time.Second)); got != "" {
		t.Errorf("stale heartbeat: %q", got)
	}
	if err := os.Remove(filepath.Join(e.data, heartbeatFile)); err != nil {
		t.Fatal(err)
	}
	if got := e.hook(t, req, time.Now()); got != "" {
		t.Errorf("no heartbeat: %q", got)
	}
}

func TestHookLeavesTheUsersRules(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	writeJSON(t, filepath.Join(e.config, "settings.json"), `{"permissions":{"ask":["Bash(git push:*)","Read(./secrets/**)"],"deny":["Bash(curl *)"]}}`)
	writeJSON(t, filepath.Join(filepath.Dir(e.project), ".claude", "settings.example.json"), `{"permissions":{"ask":["Bash(touch:*)"]}}`)
	for _, cmd := range []string{"git push origin HEAD", "cd x && git push", "touch y.tmp", "X=1 curl https://example.com"} {
		if got := e.hook(t, request(e, "bypassPermissions", "Bash", cmd), now); got != "" {
			t.Errorf("%q matches a rule of the user's, but the hook answered %q", cmd, got)
		}
	}
	if got := e.hook(t, request(e, "bypassPermissions", "Bash", "cd view && ls sub/*"), now); got == "" {
		t.Error("a command no rule is about: should be allowed")
	}

	all := newEnv(t)
	writeJSON(t, filepath.Join(all.project, ".claude", "settings.json"), `{"permissions":{"ask":["Bash"]}}`)
	if got := all.hook(t, request(all, "bypassPermissions", "Bash", "ls a/*"), now); got != "" {
		t.Errorf("ask: [Bash] covers every command: %q", got)
	}
}

func TestRuleLiteral(t *testing.T) {
	for _, c := range []struct {
		rule, lit string
		all       bool
	}{
		{"Bash", "", true},
		{"Bash()", "", true},
		{"Bash(*)", "", true},
		{"Bash(:*)", "", true},
		{"Bash(git push:*)", "git push", false},
		{"Bash(git push *)", "git push", false},
		{"Bash(npm run test)", "npm run test", false},
		{"Bash(rm -rf /tmp/*/cache)", "rm -rf /tmp/", false},
	} {
		lit, all := ruleLiteral(c.rule)
		if lit != c.lit || all != c.all {
			t.Errorf("%s: %q %v, want %q %v", c.rule, lit, all, c.lit, c.all)
		}
	}
	if isBashRule("BashOutput") || isBashRule("Read(./x)") || !isBashRule(" Bash(ls) ") {
		t.Error("isBashRule")
	}
}

// Claude Code 2.1.318 asks for each "asks" command here under ask: [Bash(touch:*), Bash(rm -rf:*)], measured in a
// bypass-mode session; the git push ones are the same shapes under the other rule.
func TestMatchingRule(t *testing.T) {
	rules := []string{"Bash(git push:*)", "Bash(rm -rf:*)", "Bash(touch:*)"}
	for _, c := range []struct{ cmd, rule string }{
		{"git push origin HEAD", "Bash(git push:*)"},
		{"git  push --force", "Bash(git push:*)"},
		{"git\tpush", "Bash(git push:*)"},
		{"'git' push", "Bash(git push:*)"},
		{"git 'push' origin main", "Bash(git push:*)"},
		{"git \\\n  push", "Bash(git push:*)"},
		{"g\\it push", "Bash(git push:*)"},
		{`"rm" -rf /x`, "Bash(rm -rf:*)"},
		{"rm  -rf /x", "Bash(rm -rf:*)"},
		{"cd x &&\n  rm \t-rf y", "Bash(rm -rf:*)"},
		{"touch\ta3.tmp", "Bash(touch:*)"},
		{"touch \\\n  a4.tmp", "Bash(touch:*)"},
		// Flags in another order or split: Claude Code does not ask for these either.
		{"rm -fr d", ""},
		{"rm -r -f d", ""},
		{"git pull", ""},
		{"cd view && rm -f sub/*", ""},
		{"echo 'git' && push", ""},
	} {
		if got := matchingRule(rules, "Bash", c.cmd); got != c.rule {
			t.Errorf("matchingRule(%q) = %q, want %q", c.cmd, got, c.rule)
		}
	}
}

func TestRedact(t *testing.T) {
	for _, c := range []struct{ in, gone string }{
		{"GITHUB_TOKEN=ghp_TEST00000000daf86f70 gh pr list", "ghp_TEST00000000daf86f70"},
		{`curl -H "Authorization: Bearer sk-live-abcdefghijk" https://api.example.com`, "sk-live-abcdefghijk"},
		{"mysql --password=hunter22 -u me", "hunter22"},
		{"git clone https://me:s3cretpass@example.com/r.git", "s3cretpass"},
		{"echo 0123456789abcdef0123456789abcdef0123", "0123456789abcdef0123456789abcdef0123"},
		{"export API_KEY='a b c'; run", "a b c"},
		{"mysql -h db -u root -pS3cretpw app", "S3cretpw"},
		{"mysqldump -u root -p'S3cret pw' app > a.sql", "S3cret pw"},
		{"sshpass -p hunter22 ssh me@example.com", "hunter22"},
		{"sshpass -phunter22 ssh me@example.com", "hunter22"},
		{"curl -u me:s3cretpass https://example.com", "s3cretpass"},
		{"curl -sS --user 'me:s3cretpass' https://example.com", "s3cretpass"},
		{"curl -fsSL -ume:s3cretpass https://example.com", "s3cretpass"},
	} {
		got := redact(c.in)
		if strings.Contains(got, c.gone) {
			t.Errorf("redact(%q) = %q, still has %q", c.in, got, c.gone)
		}
	}
	for _, ordinary := range []string{"cd view && rm -f sub/*; ls", "mkdir -p build && cp -pr a b", "mysql -p app", "docker run -u 1000:1000 img", "curl -u me https://example.com"} {
		if got := redact(ordinary); got != ordinary {
			t.Errorf("an ordinary command changed: %q", got)
		}
	}
	long := redact(strings.Repeat("echo hi; ", 100))
	if n := len([]rune(long)); n != commandShown+1 || !strings.HasSuffix(long, "…") {
		t.Errorf("a long command is cut to %d runes and an ellipsis: %d %q", commandShown, n, long[len(long)-10:])
	}
	if got := redact("python3 - <<EOF\nprint(1)\nEOF"); strings.Contains(got, "\n") {
		t.Errorf("one line in the log: %q", got)
	}
}

func TestAuditEntry(t *testing.T) {
	e := newEnv(t)
	req := request(e, "bypassPermissions", "Bash", "TOKEN=abcdefgh12345678 make -C \"$t\"")
	req["agent_id"] = "a1b2"
	e.hook(t, req, time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	got := readAudit(e.data, 10)
	if len(got) != 1 {
		t.Fatalf("%d entries", len(got))
	}
	a := got[0]
	if a.At != "2026-11-05T10:00:00Z" || a.Agent != "a1b2" || a.Cwd != e.project || a.ConfigDir != e.config || strings.Contains(a.Command, "abcdefgh12345678") || a.Length != 35 {
		t.Errorf("entry: %+v", a)
	}
	raw, _ := os.ReadFile(filepath.Join(e.data, auditFile))
	if strings.Contains(string(raw), "abcdefgh12345678") {
		t.Error("the secret reached the file")
	}
	if st, _ := os.Stat(filepath.Join(e.data, auditFile)); st.Mode().Perm() != 0o600 {
		t.Errorf("audit file mode %v", st.Mode().Perm())
	}
}

func TestAuditRotates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, auditFile)
	big := auditEntry{At: "2026-11-05T10:00:00Z", Session: "s", Command: strings.Repeat("x", 1000)}
	for range auditKeep/1000 + 10 {
		appendAudit(p, big)
	}
	appendAudit(p, auditEntry{At: "2026-11-05T11:00:00Z", Session: "s", Command: "newest"})
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatal("no rotated file")
	}
	got := readAudit(dir, 3)
	if len(got) != 3 || got[0].Command != "newest" {
		t.Fatalf("newest first across both files: %+v", got[0])
	}
}

const userSettings = `{
    "model": "opus",
    "hooks": {
        "PermissionRequest": [
            {"matcher": "*", "hooks": [{"type": "command", "command": "other-tool hook"}]}
        ],
        "Stop": [{"hooks": [{"type": "command", "command": "notify && done"}]}]
    },
    "env": {"A": "1"}
}
`

func TestInstallKeepsEverythingElse(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	writeJSON(t, p, userSettings)
	cmd := hookCommand("/opt/x/bin/"+currentExtensionID, dir, "/data/it's")
	if changed, err := installHook(p, cmd); err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if strings.Index(s, `"model"`) > strings.Index(s, `"hooks"`) || strings.Index(s, `"hooks"`) > strings.Index(s, `"env"`) {
		t.Errorf("order of the keys changed:\n%s", s)
	}
	if !strings.Contains(s, "other-tool hook") || !strings.Contains(s, "notify && done") || !strings.Contains(s, "\n    \"model\"") {
		t.Errorf("other hooks or the indent changed:\n%s", s)
	}
	if !hasHook(p) {
		t.Fatal("no entry after install")
	}
	if changed, err := installHook(p, cmd); err != nil || changed {
		t.Errorf("a second install should not write: %v %v", changed, err)
	}
	// An upgrade: the program moved, the entry follows.
	if changed, _ := installHook(p, hookCommand("/opt/y/bin/"+currentExtensionID, dir, "/data")); !changed {
		t.Error("a new command should replace the old entry")
	}
	b, _ = os.ReadFile(p)
	if strings.Count(string(b), tagFlag) != 1 || strings.Contains(string(b), "/opt/x/") {
		t.Errorf("one entry, the new one:\n%s", b)
	}
	if _, err := os.Stat(p + settingsBackupSuffix); err != nil {
		t.Error("no backup")
	}
	if changed, err := uninstallHook(p); err != nil || !changed {
		t.Fatalf("uninstall: %v %v", changed, err)
	}
	var before, after any
	b, _ = os.ReadFile(p)
	_ = json.Unmarshal([]byte(userSettings), &before)
	_ = json.Unmarshal(b, &after)
	if !jsonEqual(before, after) {
		t.Errorf("after uninstall the settings differ from the user's:\n%s", b)
	}
	if changed, _ := uninstallHook(p); changed {
		t.Error("a second uninstall should not write")
	}
}

func TestInstallIntoNothing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if changed, _ := uninstallHook(p); changed {
		t.Error("uninstall created a file")
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("uninstall created a file")
	}
	if _, err := installHook(p, hookCommand("/b", dir, "/d")); err != nil {
		t.Fatal(err)
	}
	if _, err := uninstallHook(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.TrimSpace(string(b)) != "{}" {
		t.Errorf("hooks and PermissionRequest should go when empty: %s", b)
	}
	writeJSON(t, p, "[1, 2]")
	if _, err := installHook(p, "x "+tagFlag); err == nil {
		t.Error("a file that is not an object should be left alone")
	}
}

func TestUninstallKeepsTheUsersHookInOurGroup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	cmd := hookCommand("/b", dir, "/d")
	writeJSON(t, p, `{"hooks":{"PermissionRequest":[{"matcher":"Bash","hooks":[{"type":"command","command":"`+strings.ReplaceAll(cmd, `'`, `'`)+`"},{"type":"command","command":"mine"}]}]}}`)
	if _, err := uninstallHook(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"mine"`) || strings.Contains(string(b), tagFlag) {
		t.Errorf("%s", b)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestNodeInstallsAndCleansUp(t *testing.T) {
	e := newEnv(t)
	t.Setenv("HOME", filepath.Dir(e.config))
	writeJSON(t, filepath.Join(e.config, "settings.json"), userSettings)
	n := &node{bin: "/opt/bin/claude-bypass-allow"}
	in := &ext.Init{}
	in.Extension = &wire.ExtensionInfo{ID: "claude-bypass-allow", DataDir: e.data}
	in.InitializeParams.Config = json.RawMessage(`{"machines":["m1"],"config_dirs":["~/claude","~/missing","relative/dir"]}`)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := n.init(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.config, "settings.json")
	if !hasHook(p) {
		t.Fatal("no entry after init")
	}
	if !alive(filepath.Join(e.data, heartbeatFile), time.Now()) {
		t.Fatal("no heartbeat")
	}
	h := n.program("claude-bypass-allow", "0.1.0").Namespaces["bypass"]
	raw, err := h(ctx, "status", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := raw.(status)
	if len(st.Dirs) != 3 || !st.Dirs[0].Installed || st.Dirs[1].Error == "" || st.Dirs[2].Error == "" {
		t.Errorf("dirs: %+v", st.Dirs)
	}
	if _, err := h(ctx, "nope", nil); err == nil {
		t.Error("an unknown method")
	}
	// Real shutdown: ext.Main has returned, so the run context is cancelled before cleanup runs; cleanup joins
	// the ticker goroutine there and must not leave a heartbeat it could resurrect.
	cancel()
	n.cleanup()
	if hasHook(p) {
		t.Error("entry left after cleanup")
	}
	if _, err := os.Stat(filepath.Join(e.data, heartbeatFile)); err == nil {
		t.Error("heartbeat left after cleanup")
	}
}

// A node program the hub stops before it initialized has no ticker goroutine: cleanup's WaitGroup join must
// return immediately rather than block.
func TestCleanupBeforeInitDoesNotBlock(t *testing.T) {
	done := make(chan struct{})
	go func() {
		(&node{}).cleanup()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup blocked joining a ticker that never started")
	}
}

func fakeNodes(by map[string]any) nodeCaller {
	return func(_ context.Context, machine string, out any) error {
		v, ok := by[machine]
		if !ok {
			return errors.New("machine " + machine + " is offline")
		}
		b, _ := json.Marshal(v)
		return json.Unmarshal(b, out)
	}
}

func TestPage(t *testing.T) {
	app := &hubApp{machines: []string{"m1", "m2"}, call: fakeNodes(map[string]any{
		"m1": status{Version: "0.1.0", Dirs: []dirState{{Dir: "~/.claude", Settings: "/home/me/.claude/settings.json", Installed: true}},
			Recent: []auditEntry{{At: "2026-11-05T10:00:00Z", Session: "93bed2c0-0000", Agent: "a1", Cwd: "/w/proj", Command: "cd view && rm -f sub/*"}}},
	})}
	for _, locale := range []string{"zh-Hans", "en", "ja"} {
		r := &ext.UIRequest{}
		r.Contribution, r.Size, r.UIVersion, r.Locale, r.TZ = logUI, wire.SizeFull, 3, locale, "Asia/Shanghai"
		v, err := app.render(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(v)
		if err := exttest.CheckView(raw, false); err != nil {
			t.Fatalf("%s: %v", locale, err)
		}
		if len(v.Sections) != 3 || v.Sections[0].Title != "m1" || v.Sections[1].Items[0].Badge == nil {
			t.Fatalf("%s: %s", locale, raw)
		}
		if !strings.Contains(string(raw), "18:00:00") || !strings.Contains(string(raw), "cd view") {
			t.Errorf("%s: the entry, in the user's time zone: %s", locale, raw)
		}
	}
	empty := &hubApp{call: fakeNodes(nil)}
	r := &ext.UIRequest{}
	r.Contribution, r.Size, r.UIVersion = logUI, wire.SizeFull, 3
	v, _ := empty.render(context.Background(), r)
	if len(v.Sections) != 1 || v.Sections[0].Items[0].Type != wire.ComponentEmpty {
		t.Errorf("no machine: %+v", v)
	}
	if v.Sections[0].Items[0].Detail == "" || v.Sections[0].Items[0].Text != "" {
		t.Errorf("empty component must use Detail (not Text): %+v", v.Sections[0].Items[0])
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("empty view CheckView: %v", err)
	}
}

func TestViewCheckEdgeCases(t *testing.T) {
	// 1. Empty state across all locales: Detail populated, Text empty, passes wire.CheckView.
	empty := &hubApp{call: fakeNodes(nil)}
	for _, locale := range []string{"zh-Hans", "en", "ja"} {
		r := &ext.UIRequest{}
		r.Contribution, r.Size, r.UIVersion, r.Locale = logUI, wire.SizeFull, 3, locale
		v, err := empty.render(context.Background(), r)
		if err != nil {
			t.Fatalf("%s: %v", locale, err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("%s empty CheckView: %v", locale, err)
		}
		raw, _ := json.Marshal(v)
		if err := exttest.CheckView(raw, false); err != nil {
			t.Fatalf("%s empty exttest.CheckView: %v", locale, err)
		}
		item := v.Sections[0].Items[0]
		if item.Type != wire.ComponentEmpty || item.Detail == "" || item.Text != "" {
			t.Fatalf("%s empty item: %+v", locale, item)
		}
	}

	// 2. 12 machines x (2 dirs + 30 recent = 32 items each => 385 raw items) must stay <= wire.ItemsMost (300).
	machines := make([]string, 12)
	by := make(map[string]any, len(machines))
	recent := make([]auditEntry, recentShown)
	for i := range recent {
		recent[i] = auditEntry{At: "2026-11-05T10:00:00Z", Session: "93bed2c0-0000", Cwd: "proj", Command: fmt.Sprintf("go test ./pkg%d", i)}
	}
	for i := range machines {
		m := fmt.Sprintf("m%02d", i+1)
		machines[i] = m
		by[m] = status{
			Version: "0.2.36",
			Dirs: []dirState{
				{Dir: "~/.claude", Settings: "~/.claude/settings.json", Installed: true},
				{Dir: "~/.gemini", Settings: "~/.gemini/config/hooks.json", Installed: true},
			},
			Recent: recent,
		}
	}
	app := &hubApp{machines: machines, call: fakeNodes(by)}
	r := &ext.UIRequest{}
	r.Contribution, r.Size, r.UIVersion, r.Locale, r.TZ = logUI, wire.SizeFull, 3, "en", "UTC"
	v, err := app.render(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("12 machines CheckView: %v", err)
	}
	raw, _ := json.Marshal(v)
	if err := exttest.CheckView(raw, false); err != nil {
		t.Fatalf("12 machines exttest.CheckView: %v", err)
	}
	totalItems := 0
	for _, s := range v.Sections {
		totalItems += len(s.Items)
	}
	if totalItems > wire.ItemsMost {
		t.Fatalf("totalItems = %d, exceeds wire.ItemsMost (%d)", totalItems, wire.ItemsMost)
	}

	// 3. Extreme machine count (> wire.ItemsMost) still passes wire.CheckView.
	many := make([]string, wire.ItemsMost+20)
	for i := range many {
		many[i] = fmt.Sprintf("node-%03d", i)
	}
	hugeApp := &hubApp{machines: many, call: fakeNodes(nil)}
	vHuge, err := hugeApp.render(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckView(vHuge); err != nil {
		t.Fatalf("320 machines CheckView: %v", err)
	}

	// 4. Empty Command audit rows, empty Tool, and overly long Settings / Note / Command still pass wire.CheckView.
	dir := t.TempDir()
	auditPath := filepath.Join(dir, auditFile)
	if err := os.WriteFile(auditPath, []byte(
		`{"at":"2026-11-05T10:00:00Z","session_id":"93bed2c0-0000","tool":"Read","command":""}`+"\n"+
			`{"at":"2026-11-05T10:01:00Z","session_id":"93bed2c0-0000","command":"   "}`+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	fromDisk := readAudit(dir, 10)
	if len(fromDisk) != 2 || fromDisk[0].Command != "-" || fromDisk[1].Command != "Read" {
		t.Fatalf("readAudit empty command fallback: %+v", fromDisk)
	}

	edgeApp := &hubApp{
		machines: []string{"m1"},
		call: fakeNodes(map[string]any{
			"m1": status{
				Version: "0.2.36",
				Dirs: []dirState{{
					Dir:       "~/.claude",
					Settings:  "~/" + strings.Repeat("a", 2500) + "/settings.json",
					Installed: true,
				}},
				Recent: []auditEntry{
					{At: "2026-11-05T10:00:00Z", Session: "93bed2c0-0000", Command: ""},
					{At: "2026-11-05T10:01:00Z", Session: "93bed2c0-0000", Tool: "Grep", Command: "   "},
					{At: "2026-11-05T10:02:00Z", Session: "93bed2c0-0000", Command: strings.Repeat("x", 3000), Note: strings.Repeat("n", 3000)},
				},
			},
		}),
	}
	vEdge, err := edgeApp.render(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckView(vEdge); err != nil {
		t.Fatalf("edge view CheckView: %v", err)
	}
	rawEdge, _ := json.Marshal(vEdge)
	if err := exttest.CheckView(rawEdge, false); err != nil {
		t.Fatalf("edge view exttest.CheckView: %v", err)
	}
	if got := vEdge.Sections[0].Items[1].Title; got != "-" {
		t.Errorf("empty command fallback title = %q, want -", got)
	}
	if got := vEdge.Sections[0].Items[2].Title; got != "Grep" {
		t.Errorf("empty command with tool fallback title = %q, want Grep", got)
	}
	if len([]rune(vEdge.Sections[0].Items[0].Subtitle)) > 201 {
		t.Errorf("long settings subtitle not truncated: %d runes", len([]rune(vEdge.Sections[0].Items[0].Subtitle)))
	}
}

func TestErrorAndPathSanitization(t *testing.T) {
	e := newEnv(t)
	home := filepath.Dir(e.config)
	t.Setenv("HOME", home)
	// Invalid JSON object in settings.json must not leak full host path in error or dirState.Error.
	badSettings := filepath.Join(e.config, "settings.json")
	writeJSON(t, badSettings, "[1, 2]")
	if _, err := installHook(badSettings, hookCommand("/opt/bin/x", e.config, e.data)); err == nil {
		t.Fatal("expected error for non-object settings.json")
	} else if strings.Contains(err.Error(), home) || strings.Contains(err.Error(), badSettings) {
		t.Errorf("settings error leaked host path: %q", err.Error())
	}

	// Valid settings in second config dir + audit entry with full host paths.
	okDir := filepath.Join(home, "claude-ok")
	if err := os.MkdirAll(okDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(okDir, "settings.json"), userSettings)
	appendAudit(filepath.Join(e.data, auditFile), auditEntry{
		At:        "2026-11-05T10:00:00Z",
		Session:   "93bed2c0-0000",
		Cwd:       e.project,
		ConfigDir: okDir,
		Tool:      "Bash",
		Command:   "go test ./...",
	})

	n := &node{bin: "/opt/bin/" + currentExtensionID}
	in := &ext.Init{}
	in.Extension = &wire.ExtensionInfo{ID: currentExtensionID, DataDir: e.data}
	in.InitializeParams.Config = json.RawMessage(`{"machines":["m1"],"config_dirs":["~/claude","~/claude-ok"]}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		n.cleanup()
	}()
	if _, err := n.init(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	h := n.program(currentExtensionID, "0.2.36").Namespaces["bypass"]
	rawSt, err := h(ctx, "status", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := rawSt.(status)
	if len(st.Dirs) != 2 {
		t.Fatalf("dirs = %+v", st.Dirs)
	}
	if st.Dirs[0].Error == "" || strings.Contains(st.Dirs[0].Error, home) {
		t.Errorf("dirState.Error leaked host path or empty: %q", st.Dirs[0].Error)
	}
	if !st.Dirs[1].Installed || st.Dirs[1].Settings != "~/claude-ok/settings.json" {
		t.Errorf("dirState.Settings not folded to ~: %+v", st.Dirs[1])
	}
	if len(st.Recent) != 1 || st.Recent[0].ConfigDir != "" || st.Recent[0].Cwd != filepath.Base(e.project) {
		t.Errorf("RPC audit entry not minimized: %+v", st.Recent)
	}

	// Init failure must not leak full host path either.
	badData := filepath.Join(home, "not-a-dir")
	if err := os.WriteFile(badData, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	badNode := &node{bin: "/opt/bin/" + currentExtensionID}
	badIn := &ext.Init{}
	badIn.Extension = &wire.ExtensionInfo{ID: currentExtensionID, DataDir: badData}
	badIn.InitializeParams.Config = json.RawMessage(`{}`)
	if _, err := badNode.init(ctx, badIn, nil); err == nil {
		t.Fatal("expected init error when data dir is a regular file")
	} else if strings.Contains(err.Error(), home) || strings.Contains(err.Error(), badData) {
		t.Errorf("node.init error leaked host path: %q", err.Error())
	}

	// Hub UI must sanitize cross-node RPC errors, legacy raw dir errors, and legacy absolute settings paths across locales.
	app := &hubApp{
		machines: []string{"m-err", "m-timeout", "m-legacy"},
		call: func(_ context.Context, machine string, out any) error {
			switch machine {
			case "m-err":
				return errors.New("dial tcp 192.0.2.10:8443: open /home/secretuser/.persona/node.sock: internal failure")
			case "m-timeout":
				return context.DeadlineExceeded
			default:
				b, _ := json.Marshal(status{
					Version: "0.2.35",
					Dirs: []dirState{
						{Dir: "~/.claude", Settings: "/home/secretuser/.claude/settings.json", Installed: true},
						{Dir: "/srv/app", Settings: "/srv/app/private/settings.json", Installed: true},
						{Dir: "~/.broken", Error: "/home/secretuser/.broken/settings.json is not a JSON object, left alone: invalid character"},
					},
				})
				return json.Unmarshal(b, out)
			}
		},
	}
	for _, locale := range []string{"zh-Hans", "en", "ja"} {
		r := &ext.UIRequest{}
		r.Contribution, r.Size, r.UIVersion, r.Locale = logUI, wire.SizeFull, 3, locale
		v, err := app.render(context.Background(), r)
		if err != nil {
			t.Fatalf("%s: %v", locale, err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("%s CheckView: %v", locale, err)
		}
		raw, _ := json.Marshal(v)
		s := string(raw)
		for _, leaked := range []string{"192.0.2.10", "secretuser", "/home/", "/srv/app/private", "invalid character"} {
			if strings.Contains(s, leaked) {
				t.Errorf("%s view leaked %q in %s", locale, leaked, s)
			}
		}
		if got := v.Sections[2].Items[0].Subtitle; got != "~/.claude/settings.json" {
			t.Errorf("%s folded legacy home path = %q, want ~/.claude/settings.json", locale, got)
		}
		if got := v.Sections[2].Items[1].Subtitle; got != "settings.json" {
			t.Errorf("%s collapsed non-home path = %q, want settings.json", locale, got)
		}
	}
}

func TestAtomicWritesBackupSymlinkAndConcurrentAudit(t *testing.T) {
	// 1. Existing data dir with 0755 is tightened to 0700 by node.init, and options.json is written 0600 atomically.
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	cfgDir := filepath.Join(root, "claude")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	n := &node{bin: "/opt/bin/" + currentExtensionID}
	in := &ext.Init{}
	in.Extension = &wire.ExtensionInfo{ID: currentExtensionID, DataDir: dataDir}
	in.InitializeParams.Config = json.RawMessage(`{"config_dirs":["~/claude"]}`)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := n.init(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	n.cleanup()

	if st, err := os.Stat(dataDir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("dataDir mode = %v (err=%v), want 0700", st.Mode().Perm(), err)
	}
	if st, err := os.Stat(filepath.Join(dataDir, optionsFile)); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("options.json mode = %v (err=%v), want 0600", st.Mode().Perm(), err)
	}

	// 2. Pre-planted symlink at settings backup path must be atomically replaced (0600), never followed.
	settingsFile := filepath.Join(cfgDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(userSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(root, "decoy.txt")
	if err := os.WriteFile(decoy, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	bak := settingsFile + settingsBackupSuffix
	_ = os.Remove(bak)
	if err := os.Symlink(decoy, bak); err != nil {
		t.Fatal(err)
	}
	if changed, err := installHook(settingsFile, hookCommand("/opt/bin/x", cfgDir, dataDir)); err != nil || !changed {
		t.Fatalf("installHook with symlink backup: changed=%v err=%v", changed, err)
	}
	decoyBytes, err := os.ReadFile(decoy)
	if err != nil || string(decoyBytes) != "untouched" {
		t.Fatalf("symlink backup target was modified: %q (err=%v)", decoyBytes, err)
	}
	bakInfo, err := os.Lstat(bak)
	if err != nil {
		t.Fatal(err)
	}
	if bakInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatal("backup path is still a symlink")
	}
	if bakInfo.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %v, want 0600", bakInfo.Mode().Perm())
	}

	// 3. Concurrent appendAudit across rotation boundary under -race keeps valid JSONL without corruption.
	auditDir := t.TempDir()
	auditPath := filepath.Join(auditDir, auditFile)
	// Pre-fill just below auditKeep so concurrent writers immediately contend on rotation.
	seed := auditEntry{At: "2026-11-05T09:00:00Z", Session: "seed", Command: strings.Repeat("s", 1000)}
	for range auditKeep/1000 - 2 {
		appendAudit(auditPath, seed)
	}
	const workers, perWorker = 8, 12
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := range perWorker {
				appendAudit(auditPath, auditEntry{
					At:      "2026-11-05T10:00:00Z",
					Session: fmt.Sprintf("w%d-%d", id, j),
					Command: fmt.Sprintf("cmd-%d-%d %s", id, j, strings.Repeat("x", 400)),
				})
			}
		}(w)
	}
	wg.Wait()
	if _, err := os.Stat(auditPath + ".1"); err != nil {
		t.Fatalf("expected rotated audit file: %v", err)
	}
	seen := map[string]bool{}
	for _, name := range []string{auditFile + ".1", auditFile} {
		b, err := os.ReadFile(filepath.Join(auditDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			var entry auditEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("corrupted JSONL line in %s: %v (%q)", name, err, line)
			}
			if strings.HasPrefix(entry.Session, "w") {
				seen[entry.Session] = true
			}
		}
	}
	if len(seen) != workers*perWorker {
		t.Errorf("concurrent rotation lost records: got %d/%d unique worker entries", len(seen), workers*perWorker)
	}
}

// An allow tells Persona's node (hook.decided) with the input Claude Code gave the hook; a refusal tells it nothing.
func TestHookTellsTheNode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".persona"), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(home, ".persona", "node.sock"))
	if err != nil {
		t.Skip("no unix socket here:", err)
	}
	defer ln.Close()
	got := make(chan []byte, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			_, _ = c.Write([]byte("{\"ok\":true}\n"))
			c.Close()
			got <- line
		}
	}()
	e := newEnv(t)
	e.hook(t, request(e, "default", "Bash", "ls a/*"), time.Now())
	req := request(e, "bypassPermissions", "Bash", "cd view && ls sub/*")
	if out := e.hook(t, req, time.Now()); out == "" {
		t.Fatal("not allowed")
	}
	select {
	case line := <-got:
		var m struct {
			Kind  string          `json:"kind"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(line, &m); err != nil || m.Kind != "hook.decided" {
			t.Fatalf("%s", line)
		}
		want, _ := json.Marshal(req)
		if !bytes.Equal(m.Input, want) {
			t.Fatalf("the input as Claude Code gave it:\n%s\n%s", m.Input, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the node heard nothing")
	}
	select {
	case line := <-got:
		t.Fatalf("a refusal told the node: %s", line)
	default:
	}
}

func TestHookWithoutANode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	e := newEnv(t)
	start := time.Now()
	if out := e.hook(t, request(e, "bypassPermissions", "Bash", "ls a/*"), time.Now()); out == "" || time.Since(start) > nodeWait {
		t.Fatalf("no node: %q after %v", out, time.Since(start))
	}
}
