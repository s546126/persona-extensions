package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// A hooks.json as another tool leaves it in a Codex home: hooks on several events, one on UserPromptSubmit.
const otherCodexHooks = `{
  "hooks": {
    "SessionStart": [{"matcher": null, "hooks": [{"type": "command", "command": "other-tool session", "timeout": 10}]}],
    "UserPromptSubmit": [{"matcher": null, "hooks": [{"type": "command", "command": "other-tool prompt && echo ok", "timeout": 10}]}]
  }
}
`

func userPromptGroups(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return f.Hooks["UserPromptSubmit"]
}

// The entry goes at the end of Codex's UserPromptSubmit list, beside the hooks already there, with a copy of the
// file kept; putting it in again changes nothing, and taking it out leaves the file as the other tool had it.
func TestCodexHooksFile(t *testing.T) {
	dir := t.TempDir()
	p := codexHooksPath(dir)
	if err := os.WriteFile(p, []byte(otherCodexHooks), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := codexHookCommand("/opt/ebd bin", "/data/it's")
	if !strings.Contains(cmd, " --agent codex ") || !isOurCommand(cmd) {
		t.Fatalf("command %q", cmd)
	}
	if changed, err := installHook(p, cmd); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if b, err := os.ReadFile(p + settingsBackupSuffix); err != nil || string(b) != otherCodexHooks {
		t.Fatalf("backup: %v %q", err, b)
	}
	groups := userPromptGroups(t, p)
	if len(groups) != 2 || !strings.Contains(fmt.Sprint(groups[0]), "other-tool prompt && echo ok") || ourGroupIndex(p) != 1 {
		t.Fatalf("ours goes last: %v", groups)
	}
	if changed, err := installHook(p, cmd); err != nil || changed {
		t.Fatal("again: ", changed, err)
	}
	if changed, err := uninstallHook(p); err != nil || !changed || hasHook(p) {
		t.Fatal(changed, err)
	}
	var before, after any
	b, _ := os.ReadFile(p)
	if json.Unmarshal([]byte(otherCodexHooks), &before) != nil || json.Unmarshal(b, &after) != nil ||
		fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("after uninstall:\n%s", b)
	}
	if changed, err := uninstallHook(p); err != nil || changed {
		t.Fatal("uninstall again: ", changed, err)
	}
}

// The review Codex keeps in config.toml is read for our slot only, by the path Codex keys it with.
func TestCodexReviewed(t *testing.T) {
	dir := t.TempDir()
	p := codexHooksPath(dir)
	if err := os.WriteFile(p, []byte(otherCodexHooks), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installHook(p, codexHookCommand("/opt/ebd", "/data")); err != nil {
		t.Fatal(err)
	}
	toml := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if codexReviewed(dir, p) {
		t.Fatal("no config.toml: not reviewed")
	}
	other := fmt.Sprintf("model = \"x\"\n\n[hooks.state.%q]\nenabled = true\ntrusted_hash = \"sha256:aa\"\n", p+":user_prompt_submit:0:0")
	toml(other)
	if codexReviewed(dir, p) {
		t.Fatal("the other tool's review is not ours")
	}
	toml(other + fmt.Sprintf("\n[hooks.state.%q]\nenabled = true\n", p+":user_prompt_submit:1:0"))
	if codexReviewed(dir, p) {
		t.Fatal("a slot without a trusted_hash is not reviewed")
	}
	toml(other + fmt.Sprintf("\n[hooks.state.%q]\nenabled = true\ntrusted_hash = \"sha256:bb\"\n\n[features]\nx = 1\n", p+":user_prompt_submit:1:0"))
	if !codexReviewed(dir, p) {
		t.Fatal("our slot has a review")
	}
	if _, err := uninstallHook(p); err != nil || codexReviewed(dir, p) {
		t.Fatal("no entry, no review", err)
	}
}

// The node program puts the entry into each Codex home that exists, passes a missing default home over, shows a
// missing configured one, and takes the entries out again on cleanup.
func TestNodeInstallsCodex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	n := &node{bin: "/opt/ebd", data: filepath.Join(home, "data")}
	if err := os.MkdirAll(n.data, 0o700); err != nil {
		t.Fatal(err)
	}
	n.beat()
	n.install(nil)
	n.installCodex(config{}.codexDirs())
	if len(n.dirs) != 0 {
		t.Fatalf("no ~/.codex: nothing to show: %+v", n.dirs)
	}
	if got := (config{CodexDirs: []string{}}).codexDirs(); len(got) != 0 {
		t.Fatalf("an empty list turns Codex off: %v", got)
	}
	codex := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codex, 0o700); err != nil {
		t.Fatal(err)
	}
	n.installCodex([]string{defaultCodexDir, filepath.Join(home, "missing")})
	st, err := n.status("0.1.0", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Dirs) != 2 || st.Dirs[0].Agent != agentCodex || !st.Dirs[0].Installed || st.Dirs[0].Reviewed || st.Dirs[1].Error == "" {
		t.Fatalf("%+v", st.Dirs)
	}
	hooks := filepath.Join(codex, "hooks.json")
	if !hasHook(hooks) {
		t.Fatal("entry in ~/.codex/hooks.json")
	}
	if b, _ := os.ReadFile(hooks); !strings.Contains(string(b), "--agent codex") {
		t.Fatalf("%s", b)
	}
	toml := fmt.Sprintf("[hooks.state.%q]\ntrusted_hash = \"sha256:cc\"\n", codexHooksPath(codex)+":user_prompt_submit:0:0")
	if err := os.WriteFile(filepath.Join(codex, "config.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ = n.status("0.1.0", time.Now()); !st.Dirs[0].Reviewed {
		t.Fatalf("reviewed once Codex has it on record: %+v", st.Dirs)
	}
	n.cleanup()
	if hasHook(hooks) {
		t.Fatal("cleanup takes the entry out")
	}
	if b, _ := os.ReadFile(filepath.Join(codex, "config.toml")); string(b) != toml {
		t.Fatal("config.toml is never written")
	}
}

// A Codex rollout as codex-cli 0.171 writes it: the session's meta, Codex's own developer and environment messages,
// and then the user's prompt.
const codexRolloutStart = `{"type":"session_meta","payload":{"session_id":"s"}}
{"type":"event_msg","payload":{"type":"task_started"}}
{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"You are Codex"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/w</cwd>\n</environment_context>"}]}}
`

func TestCodexUserTurn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout.jsonl")
	if hasCodexUserTurn("") || hasCodexUserTurn(p) {
		t.Fatal("no transcript: no turn")
	}
	if err := os.WriteFile(p, []byte(codexRolloutStart), 0o600); err != nil {
		t.Fatal(err)
	}
	if hasCodexUserTurn(p) {
		t.Fatal("Codex's own environment message is not the user's")
	}
	turn := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"帮我看看"}]}}` + "\n"
	if err := os.WriteFile(p, []byte(codexRolloutStart+turn), 0o600); err != nil {
		t.Fatal(err)
	}
	if !hasCodexUserTurn(p) {
		t.Fatal("the user's prompt is a turn")
	}
	if err := os.WriteFile(p, []byte(codexRolloutStart+`{"type":"event_msg","payload":{"type":"user_message","message":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !hasCodexUserTurn(p) {
		t.Fatal("a user_message event is a turn")
	}
}

// codexOutputFields are what Codex's user-prompt-submit.command.output schema allows (additionalProperties false).
var codexOutputFields = []string{"continue", "decision", "hookSpecificOutput", "reason", "stopReason", "suppressOutput", "systemMessage"}

// The hook, as Codex runs it: Codex's input, Codex's answer, the first prompt of a Persona session let through and a
// later one held back, and nothing at all for Persona's assistant or the extension's own model call.
func TestCodexHook(t *testing.T) {
	data := computer(t, options{Mode: "block"})
	m := &fakeModel{verdict: `{"equivalent": true, "score": 90}`}
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte(codexRolloutStart), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(env map[string]string, session, prompt string) (map[string]json.RawMessage, hookOut) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"session_id": session, "turn_id": "t1", "transcript_path": rollout, "cwd": "/w",
			"hook_event_name": "UserPromptSubmit", "model": "gpt-x", "permission_mode": "default", "prompt": prompt})
		var out bytes.Buffer
		runHook(hookArgs{Data: data, Agent: agentCodex, ask: m.ask, getenv: func(k string) string { return env[k] }},
			bytes.NewReader(b), &out, time.Now())
		var fields map[string]json.RawMessage
		var o hookOut
		if s := bytes.TrimSpace(out.Bytes()); len(s) > 0 {
			if err := json.Unmarshal(s, &fields); err != nil {
				t.Fatalf("%q: %v", s, err)
			}
			_ = json.Unmarshal(s, &o)
		}
		for k, v := range fields {
			if !slices.Contains(codexOutputFields, k) {
				t.Errorf("Codex refuses the field %q", k)
			}
			if k == "hookSpecificOutput" {
				var spec map[string]json.RawMessage
				_ = json.Unmarshal(v, &spec)
				for sk := range spec {
					if sk != "hookEventName" && sk != "additionalContext" {
						t.Errorf("Codex refuses hookSpecificOutput.%s", sk)
					}
				}
			}
		}
		return fields, o
	}
	if _, o := run(nil, "c1", "帮我修一下那个测试"); o.Decision != "block" || o.Reason == "" {
		t.Fatalf("held back: %+v", o)
	}
	persona := map[string]string{"PERSONA_HOME": "/home/me/.persona"}
	if _, o := run(persona, "c2", "把日志级别调成 debug"); o.Decision != "" || o.Specific.Event != "UserPromptSubmit" || o.Specific.Context == "" {
		t.Fatalf("a Persona session's first prompt goes through with the English: %+v", o)
	}
	turn := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}` + "\n"
	if err := os.WriteFile(rollout, []byte(codexRolloutStart+turn), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, o := run(persona, "c2", "继续"); o.Decision != "block" {
		t.Fatalf("a later prompt in a Persona session: %+v", o)
	}
	for _, env := range []map[string]string{{"PERSONA_ASSISTANT": "1"}, {internalEnv: "1"}} {
		if fields, _ := run(env, "c3", "继续"); len(fields) != 0 {
			t.Fatalf("%v: says nothing: %v", env, fields)
		}
	}
	if err := os.Remove(filepath.Join(data, heartbeatFile)); err != nil {
		t.Fatal(err)
	}
	if fields, _ := run(nil, "c4", "继续"); len(fields) != 0 {
		t.Fatalf("no heartbeat: says nothing: %v", fields)
	}
}

// Codex's rows on the page hold to wire.CheckView, and an entry Codex has not reviewed says so.
func TestPageCodexRows(t *testing.T) {
	long := strings.Repeat("/very/long/codex/home", 200)
	app := &hubApp{machines: []string{"m1"}, call: fakeNodes(map[string]any{"m1": status{Mode: "block", Dirs: []dirState{
		{Agent: agentClaude, Dir: "~/.claude", Settings: "/home/me/.claude/settings.json", Installed: true},
		{Agent: agentCodex, Dir: "~/.codex", Settings: "/home/me/.codex/hooks.json", Installed: true},
		{Agent: agentCodex, Dir: "~/work/.codex", Settings: "/home/me/work/.codex/hooks.json", Installed: true, Reviewed: true},
		{Agent: agentCodex, Dir: long, Error: "no such directory"},
	}}})}
	for _, locale := range []string{"zh-Hans", "en", "ja"} {
		r := &ext.UIRequest{}
		r.Contribution, r.Size, r.UIVersion, r.Locale = logUI, wire.SizeFull, 3, locale
		v, err := app.render(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("%s: %v", locale, err)
		}
		items, tx := v.Sections[0].Items, texts(locale)
		if !strings.HasPrefix(items[0].Title, "Claude Code · ") || !strings.HasPrefix(items[1].Title, "Codex · ") ||
			items[1].Badge.Text != tx.needsReview || items[2].Badge.Text != tx.installed || items[3].Badge.Tone != wire.ToneCaution {
			t.Fatalf("%s: %+v", locale, items[:4])
		}
	}
}
