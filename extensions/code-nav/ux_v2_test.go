package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestSettingsStorePersistence(t *testing.T) {
	dir := t.TempDir()
	st := newSettingsStore(dir)
	if err := st.update(func(ps *persistedSettings) error {
		ps.UseHub = true
		ps.Configured = true
		ps.Machines[hubMachineID] = machineSettings{
			Roots:       []string{"~/code", "/tmp/work"},
			RootsSet:    true,
			IdleMinutes: 25,
			Languages: map[string]langOverride{
				"go":     {Path: "/custom/gopls"},
				"python": {Disabled: true},
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, settingsFileName))
	if err != nil {
		t.Fatalf("stat settings file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("settings file mode = %o, want 0600", mode)
	}

	reloaded := newSettingsStore(dir).get()
	if !reloaded.UseHub || !reloaded.Configured {
		t.Fatalf("reloaded flags: %+v", reloaded)
	}
	ms := reloaded.Machines[hubMachineID]
	if !ms.RootsSet || !slices.Equal(ms.Roots, []string{"~/code", "/tmp/work"}) || ms.IdleMinutes != 25 {
		t.Fatalf("reloaded hub machine settings: %+v", ms)
	}
	if ms.Languages["go"].Path != "/custom/gopls" || !ms.Languages["python"].Disabled {
		t.Fatalf("reloaded language overrides: %+v", ms.Languages)
	}

	if _, err := cleanRootPath("relative/dir"); err == nil {
		t.Error("relative root path should be rejected")
	}
	if got, err := cleanRootPath("  ~/code  "); err != nil || got != "~/code" {
		t.Errorf("cleanRootPath(~/code) = %q, %v", got, err)
	}
	if _, err := cleanLSPPath("/bin/ls\nrm -rf /"); err == nil {
		t.Error("control character in LSP path should be rejected")
	}
}

func TestOnboardingAndHubLocalFlow(t *testing.T) {
	dataDir := t.TempDir()
	codeRoot := t.TempDir()
	proj := writeRepo(t, codeRoot)

	app := &hubApp{
		store:   newSettingsStore(dataDir),
		baseCfg: config{Roots: []string{filepath.Join(t.TempDir(), "nonexistent")}},
	}
	t.Cleanup(app.cleanup)
	ctx := context.Background()

	// Fresh install: no machines enabled yet -> defaults straight to Setup onboarding guide.
	v, err := app.render(ctx, uiReq("", "", nil, 13))
	if err != nil {
		t.Fatal(err)
	}
	mustCheck(t, v)
	if v.Tabs == nil || v.Tabs.Value != "setup" {
		t.Fatalf("fresh install should open setup tab, got %+v", v.Tabs)
	}

	// Step 1: Enable Hub (local) right inside the onboarding page.
	res, err := app.action(ctx, uiReq(v.State, "toggle_hub", map[string]string{"hub_enabled": "true"}, 13))
	if err != nil || res.View == nil {
		t.Fatalf("toggle_hub: %v %+v", err, res)
	}
	mustCheck(t, res.View)
	v = res.View

	// Step 2: Add the real temp codeRoot and verify live repo probe finds 1 repository.
	res, err = app.action(ctx, uiReq(v.State, "add_root", map[string]string{"root_path": codeRoot}, 13))
	if err != nil || res.View == nil {
		t.Fatalf("add_root: %v %+v", err, res)
	}
	mustCheck(t, res.View)
	v = res.View
	if raw := rawOf(v); !strings.Contains(raw, "发现 1 个仓库") {
		t.Fatalf("expected live probe to report 1 repo: %s", raw)
	}

	// Adding a secret directory (e.g. ~/.ssh) is safely flagged as forbidden by probe.
	home := homeDir()
	if home != "" {
		sshDir := filepath.Join(home, ".ssh")
		if st, err := os.Stat(sshDir); err == nil && st.IsDir() {
			rp := probeOneRoot("~/.ssh", home, home)
			if rp.Status != "forbidden" {
				t.Fatalf("expected ~/.ssh to be forbidden, got %+v", rp)
			}
		}
	}

	// Step 3: Finish setup -> switches to repos tab and lists the Hub repository.
	res, err = app.action(ctx, uiReq(v.State, "finish_setup", nil, 13))
	if err != nil || res.View == nil {
		t.Fatalf("finish_setup: %v %+v", err, res)
	}
	mustCheck(t, res.View)
	v = res.View
	if v.Tabs == nil || v.Tabs.Value != "repos" || !strings.Contains(rawOf(v), `"repo":"`+proj+`"`) {
		t.Fatalf("expected repos tab with %s: %s", proj, rawOf(v))
	}

	// Reload a brand-new hubApp from the same DataDir: persisted Hub + root survives restart!
	app2 := &hubApp{store: newSettingsStore(dataDir)}
	t.Cleanup(app2.cleanup)
	v2, err := app2.render(ctx, uiReq("", "", nil, 13))
	if err != nil {
		t.Fatal(err)
	}
	mustCheck(t, v2)
	if !strings.Contains(rawOf(v2), `"repo":"`+proj+`"`) {
		t.Fatalf("persisted hub config not restored after restart: %s", rawOf(v2))
	}
}

func TestLanguageProbeAndConfigLifecycle(t *testing.T) {
	dataDir := t.TempDir()
	root := t.TempDir()
	proj := filepath.Join(root, "py")
	writeFiles(t, proj, map[string]string{
		"pyproject.toml": "[project]\nname = \"py\"\n",
		"app.py":         "def greet(name):\n    return 'hi ' + name\n\nprint(greet('you'))\n",
	})
	proj = realDir(t, proj)

	n := &node{}
	n.setup(config{Roots: []string{root}, Python: "/nonexistent/pyright", Gopls: "/nonexistent/gopls", IdleMinutes: 10})
	t.Cleanup(n.cleanup)

	app := &hubApp{
		machines: []string{"m1"},
		call:     direct(n),
		store:    newSettingsStore(dataDir),
	}
	ctx := context.Background()

	// Open LSP tab: Python shows invalid_path because initial config pointed to nonexistent binary.
	v, err := app.render(ctx, uiReq(extState(state{M: "m1", T: "lsp", E: "python"}), "", nil, 13))
	if err != nil {
		t.Fatal(err)
	}
	mustCheck(t, v)
	if !strings.Contains(rawOf(v), "路径无效") {
		t.Fatalf("expected invalid path badge for nonexistent python server: %s", rawOf(v))
	}

	// Save custom path pointing to our fake LSP test binary -> probe reports ready!
	res, err := app.action(ctx, uiReq(v.State, "save_lang", map[string]string{"lang": "python", "lsp_path": testBinary(t)}, 13))
	if err != nil || res.View == nil {
		t.Fatalf("save_lang: %v %+v", err, res)
	}
	mustCheck(t, res.View)
	if !strings.Contains(res.Toast, "已就绪") {
		t.Fatalf("expected ready toast, got %q", res.Toast)
	}

	// Open app.py -> LSP starts and returns outline symbol "greet".
	res, err = app.action(ctx, uiReq("", "repo", map[string]string{"repo": proj}, 13))
	if err != nil {
		t.Fatal(err)
	}
	res, err = app.action(ctx, uiReq(res.View.State, "open", map[string]string{"path": "app.py"}, 13))
	if err != nil {
		t.Fatal(err)
	}
	mustCheck(t, res.View)
	if !strings.Contains(rawOf(res.View), `"title":"greet"`) {
		t.Fatalf("expected outline from configured python server: %s", rawOf(res.View))
	}

	// Disable Python in LSP tab -> stops running server & disables Python navigation.
	res, err = app.action(ctx, uiReq(extState(state{M: "m1", T: "lsp", E: "python"}), "toggle_lang", map[string]string{"lang": "python", "enabled": "false"}, 13))
	if err != nil || res.View == nil {
		t.Fatalf("toggle_lang disable: %v", err)
	}
	mustCheck(t, res.View)
	if !strings.Contains(rawOf(res.View), "已禁用") {
		t.Fatalf("expected disabled badge: %s", rawOf(res.View))
	}

	// Opening app.py now reports no Python server instead of starting the disabled one.
	res, err = app.action(ctx, uiReq(extState(state{M: "m1", R: proj}), "open", map[string]string{"path": "app.py"}, 13))
	if err != nil {
		t.Fatal(err)
	}
	mustCheck(t, res.View)
	if strings.Contains(rawOf(res.View), `"title":"greet"`) {
		t.Fatalf("disabled python server should not produce symbols: %s", rawOf(res.View))
	}

	// Save idle eviction timeout = 20 minutes -> node reflects 20 minutes.
	res, err = app.action(ctx, uiReq(extState(state{M: "m1", T: "lsp"}), "save_idle", map[string]string{"idle_minutes": "20"}, 13))
	if err != nil || res.View == nil {
		t.Fatalf("save_idle: %v", err)
	}
	mustCheck(t, res.View)
	if !strings.Contains(rawOf(res.View), "20 分钟") {
		t.Fatalf("expected updated idle minutes: %s", rawOf(res.View))
	}
}

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestGitDiffSandboxAndTruncation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"secret.txt": "outside secret\n"})

	proj := filepath.Join(root, "gitapp")
	writeFiles(t, proj, map[string]string{
		"go.mod":     "module example.com/gitapp\n\ngo 1.22\n",
		"main.go":    "package main\n\nfunc main() {\n\tprintln(\"v1\")\n}\n",
		"staged.go":  "package main\n\nvar staged = 1\n",
		"branch.go":  "package main\n\nvar branch = 1\n",
		"deleted.go": "package main\n\nvar gone = 1\n",
	})
	runGitTest(t, proj, "init", "-b", "main", "-q")
	runGitTest(t, proj, "add", ".")
	runGitTest(t, proj, "commit", "-q", "-m", "initial")

	// Branch change on feat/work
	runGitTest(t, proj, "checkout", "-q", "-b", "feat/work")
	writeFiles(t, proj, map[string]string{
		"branch.go": "package main\n\nvar branch = 2\n",
	})
	if err := os.Remove(filepath.Join(proj, "deleted.go")); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, proj, "add", "-A")
	runGitTest(t, proj, "commit", "-q", "-m", "branch edit")

	// Staged change
	writeFiles(t, proj, map[string]string{"staged.go": "package main\n\nvar staged = 2\n"})
	runGitTest(t, proj, "add", "staged.go")

	// Unstaged change + untracked + secret .env + escaping symlink + huge file for truncation
	var big strings.Builder
	big.WriteString("package main\n")
	for i := range maxDiffLines + 200 {
		big.WriteString("// line " + strconv.Itoa(i) + "\n")
	}
	writeFiles(t, proj, map[string]string{
		"main.go": "package main\n\nfunc main() {\n\tprintln(\"v2\")\n}\n",
		"new.go":  "package main\n\nfunc NewFunc() {}\n",
		"big.go":  big.String(),
		".env":    "SECRET_TOKEN=topsecret\n",
	})
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(proj, "escape.go")); err != nil {
		t.Fatal(err)
	}

	statusBefore := runGitTest(t, proj, "status", "--porcelain=v1")

	n := &node{}
	n.setup(config{Roots: []string{root}, Gopls: "/nonexistent/gopls"})
	r, err := n.repoAt(realDir(t, proj))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Unstaged mode: sees main.go, new.go, big.go; NEVER sees .env or escape.go!
	st, err := n.diffStatus(ctx, r, diffModeUnstaged)
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsGit || st.Branch != "feat/work" {
		t.Fatalf("unstaged status header: %+v", st)
	}
	var paths []string
	for _, f := range st.Files {
		paths = append(paths, f.Path)
		if f.Path == ".env" || f.Path == "escape.go" {
			t.Fatalf("secret or escaping path leaked into diffStatus: %+v", f)
		}
	}
	if !slices.Contains(paths, "main.go") || !slices.Contains(paths, "new.go") {
		t.Fatalf("expected main.go and new.go in unstaged diff, got %v", paths)
	}

	// Staged mode: sees staged.go.
	stStaged, err := n.diffStatus(ctx, r, diffModeStaged)
	if err != nil {
		t.Fatal(err)
	}
	if len(stStaged.Files) != 1 || stStaged.Files[0].Path != "staged.go" {
		t.Fatalf("staged diff files: %+v", stStaged.Files)
	}

	// Branch mode (vs main): sees branch.go, deleted.go, staged.go, main.go.
	stBranch, err := n.diffStatus(ctx, r, diffModeBranch)
	if err != nil {
		t.Fatal(err)
	}
	if stBranch.NoBase || stBranch.BaseRef != "main" {
		t.Fatalf("branch diff base: %+v", stBranch)
	}
	var bpaths []string
	for _, f := range stBranch.Files {
		bpaths = append(bpaths, f.Path)
	}
	if !slices.Contains(bpaths, "branch.go") || !slices.Contains(bpaths, "deleted.go") {
		t.Fatalf("branch diff missing expected files: %v", bpaths)
	}

	// Sandbox refusals on diffFile.
	for _, bad := range []string{".env", "escape.go", "../outside/secret.txt", "/etc/passwd"} {
		if _, err := n.diffFile(ctx, r, diffModeUnstaged, bad); err == nil {
			t.Fatalf("diffFile(%q) should be refused", bad)
		}
	}

	// Diff file for main.go reports exact changed line 4.
	df, err := n.diffFile(ctx, r, diffModeUnstaged, "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(df.Hunks) != 1 || df.Hunks[0].JumpLine != 4 || df.AbsPath == "" {
		t.Fatalf("main.go diff hunks: %+v", df)
	}

	// Large untracked file big.go is truncated safely.
	dfBig, err := n.diffFile(ctx, r, diffModeUnstaged, "big.go")
	if err != nil {
		t.Fatal(err)
	}
	if !dfBig.Truncated || len(dfBig.Hunks) == 0 || len(dfBig.Hunks[0].Lines) > maxDiffLines {
		t.Fatalf("big.go should be truncated to <= %d lines: truncated=%v hunks=%d", maxDiffLines, dfBig.Truncated, len(dfBig.Hunks))
	}

	// Verify strictly read-only: git status is identical before and after.
	statusAfter := runGitTest(t, proj, "status", "--porcelain=v1")
	if statusBefore != statusAfter {
		t.Fatalf("git repository was mutated!\nbefore:\n%s\nafter:\n%s", statusBefore, statusAfter)
	}
}

// End-to-end test: Git Diff -> click modified line -> jump into source file -> real gopls Outline, Hover,
// UI 13 CodeComponent.navigate Go to Definition, Implementations, References, and Back/Forward stack.
func TestGitDiffJumpToSourceWithRealGopls(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	gopls := goplsPath()
	if _, err := exec.LookPath(gopls); err != nil {
		if os.Getenv("CODE_NAV_REQUIRE_GOPLS") == "1" {
			t.Fatalf("gopls is required (CODE_NAV_REQUIRE_GOPLS=1) and not installed at %s", gopls)
		}
		t.Skip("gopls is not installed")
	}

	root := t.TempDir()
	proj := writeRepo(t, root)
	runGitTest(t, proj, "init", "-b", "main", "-q")
	runGitTest(t, proj, "add", "go.mod", "README.md", "main.go", "util/util.go")
	runGitTest(t, proj, "commit", "-q", "-m", "initial")

	// Modify main.go line 7 to create a real unstaged diff calling util.Greet.
	updatedMain := "package main\n\nimport \"example.com/demo/util\"\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(util.Greet(\"diff-jump\"))\n}\n"
	if err := os.WriteFile(filepath.Join(proj, "main.go"), []byte(updatedMain), 0o644); err != nil {
		t.Fatal(err)
	}

	n := newNode(t, root)
	t.Cleanup(n.cleanup)
	app := &hubApp{machines: []string{"m1"}, call: direct(n)}
	ctx := context.Background()

	check := func(v *wire.View) string {
		t.Helper()
		mustCheck(t, v)
		raw, _ := json.Marshal(v)
		if err := exttest.CheckView(raw, false); err != nil {
			t.Fatalf("exttest.CheckView: %v: %s", err, raw)
		}
		return string(raw)
	}
	act := func(st, action string, values map[string]string) *wire.View {
		t.Helper()
		res, err := app.action(ctx, uiReq(st, action, values, 13))
		if err != nil || res.View == nil {
			t.Fatalf("%s: %v %+v", action, err, res)
		}
		return res.View
	}
	at := func(v *wire.View) state {
		var s state
		_ = json.Unmarshal([]byte(v.State), &s)
		return s
	}

	// Open repo -> enter Git Diff view.
	v := act("", "repo", map[string]string{"repo": proj})
	check(v)
	v = act(v.State, "diff_view", nil)
	raw := check(v)
	if !strings.Contains(raw, `"title":"main.go"`) || !strings.Contains(raw, `"diff-mode"`) {
		t.Fatalf("diff list missing main.go: %s", raw)
	}

	// Open main.go diff detail -> shows DiffComponent + clickable modified line 7.
	v = act(v.State, "diff_file", map[string]string{"path": "main.go"})
	raw = check(v)
	if !strings.Contains(raw, `"type":"diff"`) || !strings.Contains(raw, `"diff-open-src"`) || !strings.Contains(raw, `"line":"7"`) {
		t.Fatalf("diff file detail missing diff component or jump line 7: %s", raw)
	}

	// Click modified line 7 to jump directly into full source code!
	v = act(v.State, "diff_jump", map[string]string{"path": "main.go", "line": "7", "col": "2"})
	raw = check(v)
	s := at(v)
	if s.P != filepath.Join(proj, "main.go") || s.L != 7 {
		t.Fatalf("expected main.go:7 after diff_jump, got %+v", s)
	}
	// Verify line 7 highlight, UI 13 navigate field, and real gopls outline are all active together!
	if !strings.Contains(raw, `"highlight":{"start":7,"end":7}`) || !strings.Contains(raw, `"navigate":{"id":"nav"}`) || !strings.Contains(raw, `"sym:`) {
		t.Fatalf("source view after diff_jump missing highlight/navigate/outline: %s", raw)
	}

	// Use UI 13 CodeComponent.navigate to jump from Greet on line 7 to its definition in util/util.go:4.
	greetCol := strconv.Itoa(strings.Index("\tfmt.Println(util.Greet(\"diff-jump\"))", "Greet") + 1)
	v = act(v.State, "nav", map[string]string{"line": "7", "column": greetCol, "word": "Greet", "intent": "definition"})
	raw = check(v)
	s = at(v)
	if s.P != filepath.Join(proj, "util", "util.go") || s.L != 4 {
		t.Fatalf("UI 13 navigate to definition failed: %+v", s)
	}

	// Back via UI 13 nav returns cleanly to main.go:7.
	v = act(v.State, "nav", map[string]string{"intent": "back"})
	check(v)
	s = at(v)
	if s.P != filepath.Join(proj, "main.go") || s.L != 7 || len(s.N) != 1 {
		t.Fatalf("UI 13 navigate back failed: %+v", s)
	}
}

func extState(s state) string {
	b, _ := json.Marshal(s)
	return string(b)
}
