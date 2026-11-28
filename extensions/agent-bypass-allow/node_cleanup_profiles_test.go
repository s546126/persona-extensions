package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func profileCleanupNode(t *testing.T, personaHome string, configDirs []string) (*node, string, context.CancelFunc) {
	t.Helper()
	root := filepath.Dir(filepath.Dir(personaHome))
	home := filepath.Join(root, "home", "me")
	t.Setenv("HOME", home)
	data := filepath.Join(personaHome, "node-plugins", "agent-bypass-allow", "data")
	for _, dir := range []string{filepath.Join(home, ".claude"), data} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config, _ := json.Marshal(map[string]any{"machines": []string{"m1"}, "config_dirs": configDirs})
	n := &node{bin: "/opt/extension/bin/agent-bypass-allow"}
	in := &ext.Init{}
	in.Extension = &wire.ExtensionInfo{ID: currentExtensionID, DataDir: data}
	in.InitializeParams.Config = config
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := n.init(ctx, in, nil); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		n.cleanup()
	})
	return n, data, cancel
}

func TestNodeCleanupBackfillsPreviouslyMissingExplicitProfile(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "me")
	t.Setenv("HOME", home)
	personaHome := filepath.Join(root, "state", "persona")
	profile := filepath.Join(personaHome, "profiles", "channel")
	n, data, cancel := profileCleanupNode(t, personaHome, []string{"~/.claude", profile})
	if len(n.dirs) != 2 || n.dirs[1].Settings != "" {
		t.Fatalf("missing explicit directory did not leave the expected placeholder: %+v", n.dirs)
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(profile, "settings.json")
	if _, err := installHookWith(path, n.matcher, hookCommand("/opt/extension/seed", profile, data)); err != nil {
		t.Fatal(err)
	}
	cancel()
	n.cleanup()
	if hasHook(path) {
		t.Fatal("cleanup skipped a profile that was missing at the last refresh")
	}
}

func TestNodeCleanupDoesNotFollowReplacedProfileLink(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "me")
	t.Setenv("HOME", home)
	personaHome := filepath.Join(root, "state", "persona")
	profile := filepath.Join(personaHome, "profiles", "channel")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	n, data, cancel := profileCleanupNode(t, personaHome, []string{"~/.claude"})
	path := filepath.Join(profile, "settings.json")
	if !hasHook(path) {
		t.Fatal("profile was not installed before the replacement")
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(profile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, profile); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(outside, "settings.json")
	if _, err := installHookWith(outsidePath, n.matcher, hookCommand("/opt/extension/outside", outside, data)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(outsidePath)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	n.cleanup()
	after, err := os.ReadFile(outsidePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("cleanup followed a replaced profile symlink outside the profiles root")
	}
	if !hasHook(outsidePath) {
		t.Fatal("test control lost the outside hook")
	}
}

func TestNodeAutomaticProfileRejectsSettingsLink(t *testing.T) {
	root := t.TempDir()
	personaHome := filepath.Join(root, "state", "persona")
	profile := filepath.Join(personaHome, "profiles", "channel")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.json")
	const before = "{\n  \"theme\": \"dark\"\n}\n"
	if err := os.WriteFile(outside, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(profile, "settings.json")); err != nil {
		t.Fatal(err)
	}
	n, _, _ := profileCleanupNode(t, personaHome, []string{"~/.claude"})
	after, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Fatal("installation followed an automatic profile's settings symlink outside the profiles root")
	}
	if len(n.dirs) != 2 || n.dirs[1].Installed || n.dirs[1].Error == "" {
		t.Fatalf("rejected settings symlink did not leave an error status: %+v", n.dirs)
	}
}

func TestNodeExplicitProfileKeepsSettingsLink(t *testing.T) {
	root := t.TempDir()
	personaHome := filepath.Join(root, "state", "persona")
	profile := filepath.Join(personaHome, "profiles", "channel")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.json")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(profile, "settings.json")); err != nil {
		t.Fatal(err)
	}
	profileCleanupNode(t, personaHome, []string{"~/.claude", profile})
	if !hasHook(outside) {
		t.Fatal("an explicit config_dir no longer follows its settings symlink")
	}
}

func TestNodeCleanupDoesNotFollowSettingsLink(t *testing.T) {
	root := t.TempDir()
	personaHome := filepath.Join(root, "state", "persona")
	profile := filepath.Join(personaHome, "profiles", "channel")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.json")
	if _, err := installHookWith(outside, hookMatcher, hookCommand("/opt/extension/seed", profile, filepath.Join(personaHome, "node-plugins", "agent-bypass-allow", "data"))); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(profile, "settings.json")); err != nil {
		t.Fatal(err)
	}
	n, _, cancel := profileCleanupNode(t, personaHome, []string{"~/.claude"})
	before, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	n.cleanup()
	after, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("cleanup followed an automatic profile's settings symlink")
	}
}

func TestNodeCleanupRetainsAutomaticProfileAfterDiscoveryError(t *testing.T) {
	root := t.TempDir()
	personaHome := filepath.Join(root, "state", "persona")
	profiles := filepath.Join(personaHome, "profiles")
	profile := filepath.Join(profiles, "channel")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	n, _, cancel := profileCleanupNode(t, personaHome, []string{"~/.claude"})
	settings := filepath.Join(profile, "settings.json")
	if !hasHook(settings) {
		t.Fatal("automatic profile hook was not installed")
	}
	if err := os.Chmod(profiles, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(profiles, 0o700) })
	if _, err := os.ReadDir(profiles); err == nil {
		t.Fatal("test setup requires profile discovery to fail")
	}

	n.refresh()
	cancel()
	n.cleanup()
	if err := os.Chmod(profiles, 0o700); err != nil {
		t.Fatal(err)
	}
	if hasHook(settings) {
		t.Fatal("cleanup lost an automatic profile after discovery failed")
	}
}

func TestNodeCleanupRestoredExplicitDirAfterRefresh(t *testing.T) {
	root := t.TempDir()
	personaHome := filepath.Join(root, "state", "persona")
	home := filepath.Join(root, "home", "me")
	config := filepath.Join(home, ".claude")
	n, _, cancel := profileCleanupNode(t, personaHome, []string{"~/.claude"})
	settings := filepath.Join(config, "settings.json")
	if !hasHook(settings) {
		t.Fatal("explicit directory hook was not installed")
	}
	moved := filepath.Join(home, ".claude-away")
	if err := os.Rename(config, moved); err != nil {
		t.Fatal(err)
	}
	n.refresh()
	if err := os.Rename(moved, config); err != nil {
		t.Fatal(err)
	}

	cancel()
	n.cleanup()
	if hasHook(settings) {
		t.Fatal("cleanup lost an explicit directory after a transient disappearance")
	}
}

func TestNodeCleanupRestoredAgyDirAfterRefresh(t *testing.T) {
	e := newAgyTestEnv(t, `{"toolPermission":"always-proceed"}`)
	config, err := json.Marshal(map[string]any{"machines": []string{"m1"}, "agy_dirs": []string{e.gemini}})
	if err != nil {
		t.Fatal(err)
	}
	n := &node{bin: "/opt/bin/" + currentExtensionID}
	in := &ext.Init{}
	in.Extension = &wire.ExtensionInfo{ID: currentExtensionID, DataDir: e.data}
	in.InitializeParams.Config = config
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
	hooks := agyHooksPath(e.gemini)
	if !hasAgyHook(hooks) {
		t.Fatal("agy hook was not installed")
	}
	moved := e.gemini + "-away"
	if err := os.Rename(e.gemini, moved); err != nil {
		t.Fatal(err)
	}
	n.refresh()
	if err := os.Rename(moved, e.gemini); err != nil {
		t.Fatal(err)
	}

	cancel()
	n.cleanup()
	finished = true
	if hasAgyHook(hooks) {
		t.Fatal("cleanup lost an agy directory after a transient disappearance")
	}
}

func TestNodeCleanupDualUseProfileRemovesBothHooks(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "me")
	t.Setenv("HOME", home)
	personaHome := filepath.Join(root, "state", "persona")
	data := filepath.Join(personaHome, "node-plugins", "agent-bypass-allow", "data")
	profile := filepath.Join(personaHome, "profiles", "channel")
	for _, dir := range []string{filepath.Join(home, ".claude"), data} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config, err := json.Marshal(map[string]any{
		"machines":    []string{"m1"},
		"config_dirs": []string{"~/.claude"},
		"agy_dirs":    []string{profile},
	})
	if err != nil {
		t.Fatal(err)
	}
	n := &node{bin: "/opt/bin/" + currentExtensionID}
	in := &ext.Init{}
	in.Extension = &wire.ExtensionInfo{ID: currentExtensionID, DataDir: data}
	in.InitializeParams.Config = config
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

	settings := filepath.Join(profile, "settings.json")
	hooks := agyHooksPath(profile)
	if err := os.MkdirAll(filepath.Dir(hooks), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := installHookWith(settings, n.matcher, hookCommand(n.bin, profile, data)); err != nil {
		t.Fatal(err)
	}
	if _, err := installAgyHookWith(hooks, n.agyMatcher, agyHookCommand(n.bin, profile, data)); err != nil {
		t.Fatal(err)
	}
	if !hasHook(settings) || !hasAgyHook(hooks) {
		t.Fatal("test setup did not install both hooks")
	}

	cancel()
	n.cleanup()
	finished = true
	if hasAgyHook(hooks) {
		t.Fatal("cleanup left the agy hook in a dual-use assistant profile")
	}
	if hasHook(settings) {
		t.Fatal("cleanup left the Claude hook in a dual-use assistant profile")
	}
}
