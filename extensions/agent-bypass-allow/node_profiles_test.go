package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// Assistant profiles live beside the extension's node-plugins directory. The
// node program must follow new and removed channels without a config_dirs edit.
func TestNodeDiscoversAssistantProfiles(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home", "me")
	t.Setenv("HOME", home)
	// On macOS the synthetic root is below /private/var/folders. Keep system
	// protection elsewhere, but do not reject scratch deletions inside this root.
	oldSys := systemDirs
	systemDirs = nil
	for _, s := range oldSys {
		if r := resolve(s, true); !within(root, r) {
			systemDirs = append(systemDirs, s, r)
		}
	}
	t.Cleanup(func() { systemDirs = oldSys })
	personaHome := filepath.Join(root, "state", "persona")
	data := filepath.Join(personaHome, "node-plugins", "agent-bypass-allow", "data")
	profiles := filepath.Join(personaHome, "profiles")
	first := filepath.Join(profiles, "first-channel")
	second := filepath.Join(profiles, "second-channel")
	for _, dir := range []string{filepath.Join(home, ".claude"), data, first} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	n := &node{bin: "/opt/extension/0.2.30/bin/agent-bypass-allow"}
	in := &ext.Init{}
	in.Extension = &wire.ExtensionInfo{ID: currentExtensionID, DataDir: data}
	in.InitializeParams.Config = json.RawMessage(`{"machines":["m1"]}`)
	ctx, cancel := context.WithCancel(context.Background())
	finished := false
	t.Cleanup(func() {
		if !finished {
			cancel()
			n.cleanup()
		}
	})
	if _, err := n.init(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	firstSettings := filepath.Join(first, "settings.json")
	if !hasHook(firstSettings) {
		t.Fatalf("existing assistant profile has no hook: %s", firstSettings)
	}

	// The installed profile uses the same policy: an eligible bypass request is
	// allowed, while the identical request outside bypass remains untouched.
	req := request(env{config: first, data: data, project: t.TempDir()}, "bypassPermissions", "Bash", "ls")
	raw, _ := json.Marshal(req)
	var out bytes.Buffer
	runHook(hookArgs{ConfigDir: first, Data: data}, bytes.NewReader(raw), &out, time.Now())
	if strings.TrimSpace(out.String()) != allowOutput {
		t.Fatalf("eligible assistant request: %q", out.String())
	}
	deletionProject := filepath.Join(root, "code", "proj")
	if err := os.MkdirAll(filepath.Join(deletionProject, "view", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	deletion := request(env{config: first, data: data, project: deletionProject}, "bypassPermissions", "Bash", "cd view && rm -f sub/*")
	raw, _ = json.Marshal(deletion)
	var deletionInput hookInput
	_ = json.Unmarshal(raw, &deletionInput)
	if why, _ := refuse(hookArgs{ConfigDir: first, Data: data}, deletionInput, time.Now()); why != "" {
		t.Fatalf("synthetic deletion used by the real-Claude reproduction: %s", why)
	}
	req["permission_mode"] = "default"
	raw, _ = json.Marshal(req)
	out.Reset()
	runHook(hookArgs{ConfigDir: first, Data: data}, bytes.NewReader(raw), &out, time.Now())
	if out.Len() != 0 {
		t.Fatalf("non-bypass assistant request was answered: %q", out.String())
	}
	pre := request(env{config: first, data: data, project: deletionProject}, "bypassPermissions", "Bash", "lark-cli im +messages-send --as user --text hello")
	pre["hook_event_name"] = "PreToolUse"
	raw, _ = json.Marshal(pre)
	out.Reset()
	runHook(hookArgs{ConfigDir: first, Data: data}, bytes.NewReader(raw), &out, time.Now())
	if out.Len() != 0 {
		t.Fatalf("PreToolUse Lark write was answered by the PermissionRequest hook: %q", out.String())
	}

	if err := os.MkdirAll(second, 0o700); err != nil {
		t.Fatal(err)
	}
	n.refresh() // the heartbeat's periodic reconciliation
	if !hasHook(filepath.Join(second, "settings.json")) {
		t.Fatal("new assistant profile did not receive the hook")
	}

	n.bin = "/opt/extension/0.2.31/bin/agent-bypass-allow"
	n.refresh() // an upgrade replaces every discovered entry's executable
	for _, dir := range []string{first, second} {
		body, err := os.ReadFile(filepath.Join(dir, "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), n.bin) || strings.Contains(string(body), "/0.2.30/") {
			t.Fatalf("upgrade did not replace %s: %s", dir, body)
		}
	}

	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	n.refresh()
	for _, state := range n.dirs {
		if state.Dir == first {
			t.Fatalf("removed assistant profile is still tracked: %+v", state)
		}
	}
	if !hasHook(filepath.Join(second, "settings.json")) {
		t.Fatal("reconciling a removed profile dropped another profile's hook")
	}

	// Persona may seed a brand-new profile after the extension's last refresh.
	// Shutdown must still find and remove that owned entry.
	third := filepath.Join(profiles, "third-channel")
	if err := os.MkdirAll(third, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := installHookWith(filepath.Join(third, "settings.json"), n.matcher, hookCommand("/opt/extension/seed/agent-bypass-allow", third, data)); err != nil {
		t.Fatal(err)
	}

	cancel()
	n.cleanup()
	finished = true
	for _, dir := range []string{filepath.Join(home, ".claude"), second, third} {
		if hasHook(filepath.Join(dir, "settings.json")) {
			t.Errorf("cleanup left an owned entry in %s", dir)
		}
	}
}

func TestNodeProfileDiscoveryDedupesAndSkipsLinks(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "me")
	t.Setenv("HOME", home)
	personaHome := filepath.Join(root, "state", "persona")
	data := filepath.Join(personaHome, "node-plugins", "agent-bypass-allow", "data")
	profile := filepath.Join(personaHome, "profiles", "channel")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{filepath.Join(home, ".claude"), data, profile, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(personaHome, "profiles", "linked-channel")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(personaHome, "profiles", "not-a-directory"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]any{"machines": []string{"m1"}, "config_dirs": []string{"~/.claude", profile}})
	n := &node{bin: "/opt/extension/bin/agent-bypass-allow"}
	in := &ext.Init{}
	in.Extension = &wire.ExtensionInfo{ID: currentExtensionID, DataDir: data}
	in.InitializeParams.Config = config
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		n.cleanup()
	})
	if _, err := n.init(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	if len(n.dirs) != 2 {
		t.Fatalf("explicit and discovered profiles were not deduplicated: %+v", n.dirs)
	}
	if n.dirs[1].Dir != profile || !n.dirs[1].Installed {
		t.Fatalf("explicit profile lost its status: %+v", n.dirs)
	}
	if _, err := os.Stat(filepath.Join(outside, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("discovery followed a profile symlink outside its root: %v", err)
	}
}
