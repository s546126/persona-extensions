package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newTestWorker(t *testing.T, repos ...string) (*nodeWorker, chan acceptResult) {
	t.Helper()
	done := make(chan acceptResult, 4)
	w := newNodeWorker(t.TempDir(), repos, func(event string, params any) {
		if res, ok := params.(acceptResult); ok && event == "ats.accepted" {
			done <- res
		}
	})
	return w, done
}

func waitResult(t *testing.T, done chan acceptResult) acceptResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(20 * time.Second):
		t.Fatal("the acceptance command did not report")
		return acceptResult{}
	}
}

func TestToolPathPreservesExistingEntriesAndFindsGitAndAcceptanceTools(t *testing.T) {
	_, repo := newTestRepo(t)
	head := runGit(t, repo, "rev-parse", "HEAD")
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(gitPath, filepath.Join(localBin, "git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localBin, "ats-test-tool"), []byte("#!/bin/sh\necho found-tool\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := t.TempDir()
	t.Setenv("PATH", oldPath)
	t.Setenv("HOME", home)
	t.Setenv("PERSONA_TEST_TOKEN", "hidden")
	var path string
	for _, kv := range cleanEnv() {
		if strings.HasPrefix(kv, "PERSONA_") {
			t.Fatalf("Persona-injected variable was forwarded: %q", kv)
		}
		if value, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = value
		}
	}
	if dirs := filepath.SplitList(path); len(dirs) < 2 || dirs[0] != oldPath || dirs[len(dirs)-1] != localBin {
		t.Fatalf("PATH did not preserve precedence and include the user's bin: %q", path)
	}
	if got, err := git(t.Context(), repo, "rev-parse", "HEAD"); err != nil || got != head {
		t.Fatalf("git with augmented PATH: %q (%v)", got, err)
	}
	if got := runAcceptance(t.Context(), repo, "ats-test-tool"); got.ExitCode != 0 || strings.TrimSpace(got.OutputTail) != "found-tool" {
		t.Fatalf("acceptance with augmented PATH: %+v", got)
	}
	t.Setenv("PATH", strings.Join([]string{oldPath, localBin}, string(os.PathListSeparator)))
	for _, kv := range cleanEnv() {
		if value, ok := strings.CutPrefix(kv, "PATH="); ok && strings.Count(value, localBin) != 1 {
			t.Fatalf("PATH contains more than one user bin: %q", value)
		}
	}
}

func TestPrepareChecksTheRepositoryOutAtTheRef(t *testing.T) {
	root, repo := newTestRepo(t)
	w, _ := newTestWorker(t, root)
	first := runGit(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "next.txt"), []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "second")

	res, err := w.prepare(t.Context(), prepareParams{TaskID: "task_a", Attempt: 1, Repo: repo, Ref: first})
	if err != nil {
		t.Fatal(err)
	}
	if res.BaseSHA != first || res.Branch != "ats/task_a-1" || res.Path != filepath.Join(w.data, "worktrees", "task_a-1") {
		t.Fatalf("prepare: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(res.Path, "next.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the worktree is not at the ref the task named")
	}
	if got := runGit(t, res.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "ats/task_a-1" {
		t.Fatalf("the worktree is on %s", got)
	}
	// Asked again (the hub program restarted in between): the same worktree, not an error.
	again, err := w.prepare(t.Context(), prepareParams{TaskID: "task_a", Attempt: 1, Repo: repo, Ref: first})
	if err != nil || again != res {
		t.Fatalf("prepare again: %+v %v", again, err)
	}

	ro, err := w.prepare(t.Context(), prepareParams{TaskID: "task_b", Attempt: 2, Repo: repo, Readonly: true})
	if err != nil {
		t.Fatal(err)
	}
	if ro.Branch != "" || ro.BaseSHA != runGit(t, repo, "rev-parse", "HEAD") {
		t.Fatalf("read-only prepare: %+v", ro)
	}
}

func TestPrepareRefusesWhatTheAllowlistDoesNotCover(t *testing.T) {
	root, repo := newTestRepo(t)
	_, outside := newTestRepo(t)
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	w, _ := newTestWorker(t, root)
	for name, tc := range map[string]struct {
		p    prepareParams
		want string
	}{
		"another directory":     {prepareParams{TaskID: "t", Attempt: 1, Repo: outside}, "repo_not_allowed"},
		"a link leading out":    {prepareParams{TaskID: "t", Attempt: 1, Repo: link}, "repo_not_allowed"},
		"a relative path":       {prepareParams{TaskID: "t", Attempt: 1, Repo: "app"}, "repo:"},
		"a missing repository":  {prepareParams{TaskID: "t", Attempt: 1, Repo: filepath.Join(root, "none")}, "repo:"},
		"an unknown ref":        {prepareParams{TaskID: "t", Attempt: 1, Repo: repo, Ref: "no-such-branch"}, "ref_not_found"},
		"a ref that is a flag":  {prepareParams{TaskID: "t", Attempt: 1, Repo: repo, Ref: "--upload-pack=x"}, "ref must be"},
		"a task id with a path": {prepareParams{TaskID: "../t", Attempt: 1, Repo: repo}, "not valid"},
		"no attempt":            {prepareParams{TaskID: "t", Repo: repo}, "not valid"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := w.prepare(t.Context(), tc.p); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
	if entries, _ := os.ReadDir(filepath.Join(w.data, "worktrees")); len(entries) != 0 {
		t.Fatalf("a refused request left %d worktrees", len(entries))
	}

	empty, _ := newTestWorker(t)
	if _, err := empty.prepare(t.Context(), prepareParams{TaskID: "t", Attempt: 1, Repo: repo}); err == nil || !strings.Contains(err.Error(), "repo_not_allowed") {
		t.Fatalf("an empty allowlist allowed a repository: %v", err)
	}
}

func TestPrepareAndResolveCommitRejectUnsafeRefs(t *testing.T) {
	repo := t.TempDir()
	w, _ := newTestWorker(t, repo)
	for _, tc := range []struct{ name, ref string }{
		{"forced refspec", "+refs/heads/main:refs/heads/other"},
		{"refspec with destination", "main:other"},
		{"option", "--force"},
		{"force prefix", "+main"},
		{"negative refspec", "^main"},
		{"blank", " \t"},
		{"trailing newline", "main\n"},
		{"whitespace", "main\rother"},
		{"NUL", "main\x00other"},
		{"control character", "main\x7fother"},
		{"over the byte limit", strings.Repeat("x", 201)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := prepareParams{TaskID: "unsafe", Attempt: 1, Repo: repo, Ref: tc.ref}
			if _, err := w.prepare(t.Context(), p); err == nil || !strings.HasPrefix(err.Error(), "invalid_ref:") {
				t.Errorf("prepare with ref %q: got %v, want invalid_ref", tc.ref, err)
			}
			if _, err := resolveCommit(t.Context(), repo, tc.ref); err == nil || !strings.HasPrefix(err.Error(), "invalid_ref:") {
				t.Errorf("resolveCommit with ref %q: got %v, want invalid_ref", tc.ref, err)
			}
		})
	}
	if _, err := resolveCommit(t.Context(), repo, ""); err == nil || !strings.HasPrefix(err.Error(), "invalid_ref:") {
		t.Errorf("resolveCommit with an empty ref: got %v, want invalid_ref", err)
	}
}

func TestPrepareRPCRejectsExplicitlyEmptyRef(t *testing.T) {
	root, repo := newTestRepo(t)
	w, _ := newTestWorker(t, root)
	params, err := json.Marshal(map[string]any{"task_id": "unsafe", "attempt": 1, "repo": repo, "ref": ""})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.rpc(t.Context(), "prepare", params); err == nil || !strings.HasPrefix(err.Error(), "invalid_ref:") {
		t.Fatalf("ats.prepare with an empty ref: got %v, want invalid_ref", err)
	}
}

func TestResolveCommitAcceptsCommitishRefs(t *testing.T) {
	_, repo := newTestRepo(t)
	first := runGit(t, repo, "rev-parse", "HEAD")
	short := runGit(t, repo, "rev-parse", "--short", first)
	runGit(t, repo, "tag", "-a", "v1.0.0", "-m", "release", first)
	runGit(t, repo, "branch", "feature/foo", first)
	runGit(t, repo, "commit", "-qm", "second", "--allow-empty")
	head := runGit(t, repo, "rev-parse", "HEAD")
	for _, tc := range []struct{ ref, want string }{
		{"main", head}, {"HEAD", head}, {"refs/heads/main", head}, {"v1.0.0", first},
		{"feature/foo", first}, {first, first}, {short, first}, {"HEAD^", first}, {"HEAD~1", first}, {"HEAD@{0}", head},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			if got, err := resolveCommit(t.Context(), repo, tc.ref); err != nil || got != tc.want {
				t.Fatalf("resolveCommit(%q) = %q, %v, want %s", tc.ref, got, err, tc.want)
			}
		})
	}
}

func TestAcceptRunsTheCommandInTheWorktreeWithoutTheHubsEnvironment(t *testing.T) {
	t.Setenv("PERSONA_EXAMPLE_TOKEN", "not-for-the-command")
	root, repo := newTestRepo(t)
	w, done := newTestWorker(t, root)
	if err := w.accept(acceptParams{TaskID: "task_a", Attempt: 1, Command: "true"}); err == nil || !strings.Contains(err.Error(), "worktree_missing") {
		t.Fatalf("acceptance without a worktree: %v", err)
	}
	res, err := w.prepare(t.Context(), prepareParams{TaskID: "task_a", Attempt: 1, Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.accept(acceptParams{TaskID: "task_a", Attempt: 1, Command: " "}); err == nil {
		t.Fatal("an empty command was taken")
	}
	cmd := `echo "token=[${PERSONA_EXAMPLE_TOKEN:-}]"; pwd; echo dirty > note.txt; exit 7`
	if err := w.accept(acceptParams{TaskID: "task_a", Attempt: 1, Command: cmd}); err != nil {
		t.Fatal(err)
	}
	got := waitResult(t, done)
	if !got.Done || got.ExitCode != 7 || got.TimedOut || got.Dirty == nil || !*got.Dirty || got.HeadSHA != res.BaseSHA {
		t.Fatalf("result: %+v", got)
	}
	if !strings.Contains(got.OutputTail, "token=[]") || !strings.Contains(got.OutputTail, res.Path) {
		t.Fatalf("output: %s", got.OutputTail)
	}
	// The result is kept for a hub program that missed the event.
	status, err := w.acceptStatus("task_a", 1)
	if err != nil || !reflect.DeepEqual(status, got) {
		t.Fatalf("status: %+v %v", status, err)
	}
	// An attempt this program knows nothing of: its command did not survive.
	lost, err := w.acceptStatus("task_zz", 1)
	if err != nil || !lost.Done || lost.ExitCode != -1 || !strings.Contains(lost.Error, "lost") {
		t.Fatalf("status of an unknown attempt: %+v %v", lost, err)
	}
}

func TestAcceptReportsDirtyOnlyAfterInspection(t *testing.T) {
	clean := false
	for _, tc := range []struct {
		name, command string
		wantDirty     *bool
	}{
		{"clean", "true", &clean},
		{"inspection fails", "mv .git .git-moved", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, repo := newTestRepo(t)
			w, done := newTestWorker(t, root)
			if _, err := w.prepare(t.Context(), prepareParams{TaskID: "task_a", Attempt: 1, Repo: repo, Readonly: true}); err != nil {
				t.Fatal(err)
			}
			if err := w.accept(acceptParams{TaskID: "task_a", Attempt: 1, Command: tc.command}); err != nil {
				t.Fatal(err)
			}
			got := waitResult(t, done)
			wantError := tc.wantDirty == nil
			if !got.Done || got.ExitCode != 0 || (got.Error != "") != wantError || (wantError && !strings.Contains(got.Error, "check worktree:")) {
				t.Fatalf("acceptance result: %+v", got)
			}
			raw, err := os.ReadFile(w.resultFile("task_a-1"))
			if err != nil {
				t.Fatal(err)
			}
			assertDirtyJSON(t, raw, tc.wantDirty)
		})
	}
}

func TestAcceptTimeoutKillsEverythingTheCommandStarted(t *testing.T) {
	root, repo := newTestRepo(t)
	w, done := newTestWorker(t, root)
	res, err := w.prepare(t.Context(), prepareParams{TaskID: "task_a", Attempt: 1, Repo: repo, Readonly: true})
	if err != nil {
		t.Fatal(err)
	}
	// A child that would write a file after the timeout, were it left alive.
	cmd := `(sleep 3; echo late > late.txt) & echo started; sleep 30`
	start := time.Now()
	if err := w.accept(acceptParams{TaskID: "task_a", Attempt: 1, Command: cmd, TimeoutSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if st, err := w.acceptStatus("task_a", 1); err != nil || st.Done {
		t.Fatalf("status while running: %+v %v", st, err)
	}
	got := waitResult(t, done)
	if !got.TimedOut || got.ExitCode == 0 || time.Since(start) > 10*time.Second || !strings.Contains(got.OutputTail, "started") {
		t.Fatalf("result after %s: %+v", time.Since(start), got)
	}
	time.Sleep(3500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(res.Path, "late.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a process the command started outlived the timeout")
	}
}

func TestReleaseKeepsWorkAndDropsWhatIsEmpty(t *testing.T) {
	root, repo := newTestRepo(t)
	w, _ := newTestWorker(t, root)
	ctx := context.Background()
	prep := func(id string) prepareResult {
		res, err := w.prepare(ctx, prepareParams{TaskID: id, Attempt: 1, Repo: repo})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	branches := func() string { return runGit(t, repo, "branch", "--list", "ats/*", "--format=%(refname:short)") }

	// Nothing done: the worktree and the branch go.
	a := prep("task_a")
	res, err := w.release(ctx, releaseParams{TaskID: "task_a", Attempt: 1, Repo: repo, BaseSHA: a.BaseSHA})
	if err != nil || !res.Removed || res.Branch != "" || res.Dirty == nil || *res.Dirty || branches() != "" {
		t.Fatalf("release of an untouched attempt: %+v %v, branches %q", res, err, branches())
	}

	// A commit: the worktree goes, the branch stays.
	b := prep("task_b")
	if err := os.WriteFile(filepath.Join(b.Path, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, b.Path, "add", ".")
	runGit(t, b.Path, "commit", "-q", "-m", "work")
	head := runGit(t, b.Path, "rev-parse", "HEAD")
	res, err = w.release(ctx, releaseParams{TaskID: "task_b", Attempt: 1, Repo: repo, BaseSHA: b.BaseSHA})
	if err != nil || !res.Removed || res.Branch != "ats/task_b-1" || res.HeadSHA != head || res.Dirty == nil || *res.Dirty || branches() != "ats/task_b-1" {
		t.Fatalf("release of an attempt with a commit: %+v %v, branches %q", res, err, branches())
	}
	if _, err := os.Stat(b.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the worktree is still there")
	}

	// Work that is not committed: everything stays where it is.
	c := prep("task_c")
	if err := os.WriteFile(filepath.Join(c.Path, "draft.txt"), []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = w.release(ctx, releaseParams{TaskID: "task_c", Attempt: 1, Repo: repo, BaseSHA: c.BaseSHA})
	if err != nil || res.Removed || res.Kept != c.Path || res.Dirty == nil || !*res.Dirty {
		t.Fatalf("release of a dirty worktree: %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(c.Path, "draft.txt")); err != nil {
		t.Fatal("uncommitted work was removed")
	}

	// Released twice, or never prepared: nothing to do.
	if res, err := w.release(ctx, releaseParams{TaskID: "task_a", Attempt: 1, Repo: repo}); err != nil || !res.Removed || res.Dirty != nil {
		t.Fatalf("second release: %+v %v", res, err)
	}
}

func TestReleaseReportsCleanAfterAutoCommit(t *testing.T) {
	root, repo := newTestRepo(t)
	w, _ := newTestWorker(t, root)
	p, err := w.prepare(t.Context(), prepareParams{TaskID: "task_a", Attempt: 1, Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Path, "draft.txt"), []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := w.release(t.Context(), releaseParams{TaskID: "task_a", Attempt: 1, Repo: repo, BaseSHA: p.BaseSHA, CommitMessage: "Keep work"})
	if err != nil || !res.Removed || res.Branch != p.Branch || res.HeadSHA == "" || res.HeadSHA == p.BaseSHA {
		t.Fatalf("release after auto-commit: %+v (%v)", res, err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	clean := false
	assertDirtyJSON(t, raw, &clean)
}

func TestNodeRPCNamesItsMethods(t *testing.T) {
	root, repo := newTestRepo(t)
	w, _ := newTestWorker(t, root)
	out, err := w.rpc(t.Context(), "prepare", []byte(`{"task_id":"task_a","attempt":1,"repo":"`+repo+`","readonly":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if res, ok := out.(prepareResult); !ok || res.Path == "" {
		t.Fatalf("prepare over rpc: %#v", out)
	}
	if _, err := w.rpc(t.Context(), "run_anything", []byte(`{}`)); err == nil {
		t.Fatal("an unknown method was answered")
	}
}

func listenProbeGateway(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp4", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func closedProbeGateway(t *testing.T) string {
	t.Helper()
	l := listenProbeGateway(t)
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func writeLauncherSettings(t *testing.T, dir, filename, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProbeLauncherGatewaySettings(t *testing.T) {
	live := listenProbeGateway(t)
	defer live.Close()
	up, down := live.Addr().String(), closedProbeGateway(t)
	for _, tc := range []struct {
		name, launcher, env, settings, endpoint string
		reachable, skipped, canceled            bool
	}{
		{name: "anthropic first", launcher: "primary", env: `{"env":{"ANTHROPIC_BASE_URL":"http://` + up + `/v1","OPENAI_BASE_URL":"http://` + down + `"}}`, settings: `{"base_url":"http://` + down + `"}`, reachable: true, endpoint: up},
		{name: "openai before base_url", launcher: "primary", env: `{"env":{"OPENAI_BASE_URL":"http://` + up + `"},"base_url":"http://` + down + `"}`, reachable: true, endpoint: up},
		{name: "legacy settings", launcher: "primary", settings: `{"base_url":"http://` + up + `"}`, reachable: true, endpoint: up},
		{name: "connection refused", launcher: "primary", env: `{"base_url":"http://` + down + `"}`, endpoint: down},
		{name: "http default port", launcher: "primary", env: `{"base_url":"http://localhost/v1"}`, endpoint: "localhost:80", canceled: true},
		{name: "https default port", launcher: "primary", env: `{"base_url":"https://localhost/v1"}`, endpoint: "localhost:443", canceled: true},
		{name: "no custom URL", launcher: "primary", env: `{"env":{"OPENAI_BASE_URL":" "}}`, reachable: true, skipped: true},
		{name: "missing host", launcher: "primary", env: `{"base_url":"/v1"}`, reachable: true, skipped: true},
		{name: "missing files", launcher: "primary", reachable: true, skipped: true},
		{name: "blank launcher", launcher: "  ", reachable: true, skipped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "launcher-settings")
			if tc.env != "" {
				writeLauncherSettings(t, dir, "primary-env-settings.json", tc.env)
			}
			if tc.settings != "" {
				writeLauncherSettings(t, dir, "primary-settings.json", tc.settings)
			}
			nodes := &localNodes{worker: newNodeWorker(filepath.Join(root, "node-plugins", "ats", "data"), nil, nil)}
			ctx := t.Context()
			if tc.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var got probeLauncherResult
			if err := nodes.Call(ctx, testMachine, "ats.probe_launcher", probeLauncherParams{Launcher: tc.launcher}, &got); err != nil {
				t.Fatal(err)
			}
			if got.Reachable != tc.reachable || got.Skipped != tc.skipped || got.Endpoint != tc.endpoint || (got.Error != "") != !tc.reachable {
				t.Fatalf("probe = %+v, want reachable=%t skipped=%t endpoint=%q", got, tc.reachable, tc.skipped, tc.endpoint)
			}
		})
	}
}

func TestProbeLauncherSettingsSearchIsBoundedAndCanBeOverridden(t *testing.T) {
	live := listenProbeGateway(t)
	defer live.Close()
	root := t.TempDir()
	dir := filepath.Join(root, "launcher-settings")
	addr := live.Addr().String()
	writeLauncherSettings(t, dir, "primary-settings.json", `{"base_url":"http://`+addr+`"}`)
	w := newNodeWorker(filepath.Join(root, "a", "b", "c", "data"), nil, nil)
	check := func(want probeLauncherResult) {
		t.Helper()
		got, err := w.probeLauncher(t.Context(), probeLauncherParams{Launcher: "primary"})
		if err != nil || got != want {
			t.Fatalf("probe = %+v, %v, want %+v", got, err, want)
		}
	}
	check(probeLauncherResult{Reachable: true, Endpoint: addr})
	w.data = filepath.Join(w.data, "deeper")
	check(probeLauncherResult{Reachable: true, Skipped: true})
	w.launcherSettingsDir = dir
	check(probeLauncherResult{Reachable: true, Endpoint: addr})
}

func TestProbeLauncherRejectsUnsafeIDs(t *testing.T) {
	w, _ := newTestWorker(t)
	for _, launcher := range []string{"../primary", "primary/backup", `primary\backup`, "a..b", "bad name", " primary", strings.Repeat("a", 81)} {
		params, err := json.Marshal(map[string]string{"launcher": launcher})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.rpc(t.Context(), "probe_launcher", params); err == nil || !strings.Contains(err.Error(), "invalid launcher") {
			t.Errorf("launcher %q: expected validation error, got %v", launcher, err)
		}
	}
}

func TestPrepareFetchesMissingRefFromRemote(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(tmp, "origin.git")
	cloneA := filepath.Join(tmp, "cloneA")
	cloneB := filepath.Join(tmp, "cloneB")

	runGit(t, tmp, "init", "--bare", "-q", bare)
	runGit(t, tmp, "clone", "-q", bare, cloneA)
	if err := os.WriteFile(filepath.Join(cloneA, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, cloneA, "add", ".")
	runGit(t, cloneA, "commit", "-q", "-m", "init")
	runGit(t, cloneA, "push", "-q", "origin", "HEAD:main")

	runGit(t, tmp, "clone", "-q", bare, cloneB)

	// Push a new commit on an uncloned branch from cloneA so cloneB does not have it yet.
	runGit(t, cloneA, "checkout", "-q", "-b", "review/round-05")
	if err := os.WriteFile(filepath.Join(cloneA, "round5.txt"), []byte("round5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, cloneA, "add", ".")
	runGit(t, cloneA, "commit", "-q", "-m", "round 5")
	wantSHA := runGit(t, cloneA, "rev-parse", "HEAD")
	runGit(t, cloneA, "push", "-q", "origin", "review/round-05")

	w, _ := newTestWorker(t, tmp)
	res, err := w.prepare(t.Context(), prepareParams{TaskID: "task_fetch", Attempt: 1, Repo: cloneB, Ref: wantSHA, Readonly: true})
	if err != nil {
		t.Fatalf("prepare should auto-fetch missing ref %s: %v", wantSHA, err)
	}
	if res.BaseSHA != wantSHA {
		t.Fatalf("got BaseSHA %s, want %s", res.BaseSHA, wantSHA)
	}
	data, err := os.ReadFile(filepath.Join(res.Path, "round5.txt"))
	if err != nil || string(data) != "round5\n" {
		t.Fatalf("fetched worktree missing round5.txt: err=%v data=%q", err, string(data))
	}
}

func TestPrepareTargetedFetchKeepsRepositoryRefs(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	root, repo := newTestRepo(t)
	protected := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "branch", "other", protected)
	refs := runGit(t, repo, "show-ref")

	source, bare := filepath.Join(root, "source"), filepath.Join(root, "origin.git")
	runGit(t, root, "clone", "-q", repo, source)
	runGit(t, source, "commit", "-qm", "remote commit", "--allow-empty")
	want := runGit(t, source, "rev-parse", "HEAD")
	runGit(t, source, "branch", "feature/remote", want)
	runGit(t, source, "tag", "v1.0.0", want)
	runGit(t, root, "clone", "--bare", "-q", source, bare)
	runGit(t, repo, "remote", "add", "origin", bare)
	// 让常规 fetch --all 跳过 origin，以实际执行单个引用的 fetch。
	runGit(t, repo, "config", "remote.origin.skipFetchAll", "true")

	w, _ := newTestWorker(t, root)
	res, err := w.prepare(t.Context(), prepareParams{TaskID: "remote", Attempt: 1, Repo: repo, Ref: "refs/heads/feature/remote", Readonly: true})
	if err != nil || res.BaseSHA != want {
		t.Fatalf("prepare with remote ref: got %+v, %v, want %s", res, err, want)
	}
	if got := runGit(t, res.Path, "rev-parse", "HEAD"); got != want {
		t.Fatalf("worktree HEAD = %s, want %s", got, want)
	}
	if got := runGit(t, repo, "rev-parse", "FETCH_HEAD^{commit}"); got != want {
		t.Fatalf("FETCH_HEAD = %s, want %s", got, want)
	}
	if got := runGit(t, repo, "show-ref"); got != refs {
		t.Fatalf("targeted fetch changed refs:\n%s\nwant:\n%s", got, refs)
	}

	for _, ref := range []string{"+refs/heads/main:refs/heads/other", "main:other"} {
		p := prepareParams{TaskID: "unsafe", Attempt: 1, Repo: repo, Ref: ref}
		if _, err := w.prepare(t.Context(), p); err == nil || !strings.HasPrefix(err.Error(), "invalid_ref:") {
			t.Errorf("prepare with ref %q: got %v, want invalid_ref", ref, err)
		}
		if got := runGit(t, repo, "rev-parse", "refs/heads/other"); got != protected {
			t.Errorf("prepare with ref %q moved other from %s to %s", ref, protected, got)
		}
	}
	if _, err := os.Stat(w.worktree("unsafe-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected refs left a worktree: %v", err)
	}
}
