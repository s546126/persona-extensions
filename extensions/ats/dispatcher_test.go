package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func artifactKinds(t *Task) []string {
	var kinds []string
	if len(t.Reports) == 0 {
		return nil
	}
	for _, a := range t.Reports[len(t.Reports)-1].Artifacts {
		kinds = append(kinds, a.Kind)
	}
	return kinds
}

type todoAttachTestHub struct {
	*fakeHub
	client *hubClient
}

func (h todoAttachTestHub) AttachTodo(ctx context.Context, todo, machine, session string) error {
	return h.client.AttachTodo(ctx, todo, machine, session)
}

func (h todoAttachTestHub) DetachTodo(ctx context.Context, todo, machine, session string) error {
	return h.client.DetachTodo(ctx, todo, machine, session)
}

func TestTodoAttachOutcomeIsVisibleAfterQueueReload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		warn   bool
	}{
		{"conflict", http.StatusConflict, true},
		{"attached", http.StatusNoContent, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodPut || req.URL.Path != "/api/v1/todos/todo_1/agents/"+testMachine+"/session-1" {
					t.Errorf("attach request: %s %s", req.Method, req.URL.Path)
				}
				w.WriteHeader(tc.status)
				if tc.warn {
					_, _ = w.Write([]byte(`{"code":"todo_closed","error":"the to-do is already complete"}`))
				}
			}))
			defer srv.Close()
			r.d.api = todoAttachTestHub{fakeHub: r.hub, client: newHubClient(srv.URL, "test-token")}
			task := r.enqueue(EnqueueInput{Title: "Record the attachment result"})
			check := func(terminal bool) {
				t.Helper()
				q, err := NewQueue(filepath.Dir(r.q.dataFile), r.cfg.queueConfig())
				if err != nil {
					t.Fatal(err)
				}
				h := (&atsApp{queue: q}).handler()
				get := func(path string) map[string]json.RawMessage {
					t.Helper()
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
					body := map[string]json.RawMessage{}
					if err := json.Unmarshal(rec.Body.Bytes(), &body); rec.Code != http.StatusOK || err != nil {
						t.Fatalf("GET %s: %d %s (%v)", path, rec.Code, rec.Body, err)
					}
					return body
				}
				path := "/tasks/" + task.ID
				view, evidence := get(path), get(path+"/evidence")
				var attempts []map[string]json.RawMessage
				if err := json.Unmarshal(view["attempts"], &attempts); err != nil || len(attempts) != 1 {
					t.Fatalf("attempts: %s (%v)", view["attempts"], err)
				}
				warnings := []json.RawMessage{attempts[0]["todo_attach_error"], evidence["todo_attach_error"]}
				if terminal {
					var report map[string]json.RawMessage
					if err := json.Unmarshal(evidence["last_report"], &report); err != nil {
						t.Fatal(err)
					}
					warnings = append(warnings, report["todo_attach_error"])
				}
				for _, warning := range warnings {
					if tc.warn && !strings.Contains(string(warning), "409") || !tc.warn && warning != nil {
						t.Fatalf("todo_attach_error = %s (want warning = %v)", warning, tc.warn)
					}
				}
			}
			r.tick()
			check(false)
			r.hub.turnEnds("session-1", "u1", "Finished the review.")
			r.tick()
			r.tick()
			if got := r.task(task.ID); got.State != StateSucceeded {
				t.Fatalf("the review did not finish: %s", got.State)
			}
			check(true)
		})
	}
}

func TestReadonlyWithoutAcceptanceStillReportsAndKeepsSourceChanges(t *testing.T) {
	for _, stage := range []string{"untracked", "modified", "committed", "tool configuration"} {
		t.Run(stage, func(t *testing.T) {
			r := newRig(t)
			task := r.enqueue(EnqueueInput{Title: "Inspect source"})
			r.tick()
			wt := r.task(task.ID).Exec.Worktree
			runGit(t, wt, "config", "status.showUntrackedFiles", "no")
			name := "README.md"
			switch stage {
			case "untracked":
				name = "new-source.go"
			case "tool configuration":
				name = ".serena/project.yml"
				if err := os.Mkdir(filepath.Join(wt, ".serena"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(wt, name), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if stage == "committed" {
				runGit(t, wt, "add", name)
				runGit(t, wt, "commit", "-qm", "unexpected change")
			}
			r.hub.turnEnds("session-1", "u1", "Finished the review.")
			r.tick()
			r.tick()
			got := r.task(task.ID)
			if len(got.Reports) != 1 || got.Reports[0].Dirty == nil || !*got.Reports[0].Dirty || !slices.Contains(artifactKinds(got), "worktree") {
				t.Fatalf("missing dirty worktree evidence: %+v", got.Reports)
			}
			if _, err := os.Stat(filepath.Join(wt, name)); err != nil {
				t.Fatalf("worktree did not retain the work: %v", err)
			}
		})
	}
}

func TestDispatcherPreservesInspectionWhenReleaseFails(t *testing.T) {
	for _, tc := range []struct {
		name, acceptance string
		wantDirty        *bool
	}{
		{"clean acceptance", "true", boolPtr(false)},
		{"dirty acceptance", "echo work > draft.txt", boolPtr(true)},
		{"no acceptance", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.nodes.fail["ats.release"] = errors.New("node unavailable")
			task := r.enqueue(EnqueueInput{PermissionProfile: PermissionDefault, AcceptanceCommand: tc.acceptance})
			r.tick()
			r.hub.turnEnds("session-1", "u1", "Finished.")
			r.tick()
			r.tick()
			if tc.acceptance != "" {
				r.waitAccepted(task.ID, 1)
				r.tick()
			}
			got := r.task(task.ID)
			if got.State != StateSucceeded || len(got.Attempts) != 1 || len(got.Reports) != 1 {
				t.Fatalf("task did not finish: state %s, attempts %d, reports %d", got.State, len(got.Attempts), len(got.Reports))
			}
			if !slices.Contains(artifactKinds(got), "worktree") {
				t.Fatal("release failure did not retain the worktree artifact")
			}
			if !reflect.DeepEqual(got.Attempts[0].Dirty, tc.wantDirty) || !reflect.DeepEqual(got.Reports[0].Dirty, tc.wantDirty) {
				t.Fatalf("dirty evidence did not reflect the acceptance result for %q", tc.name)
			}
		})
	}
}

func TestWritingTaskRunsInItsOwnWorktreeAndPassesAcceptance(t *testing.T) {
	r := newRig(t)
	task := r.enqueue(EnqueueInput{Title: "Add the marker", PermissionProfile: PermissionDefault, AcceptanceCommand: "test -f done.txt && echo checked"})

	r.tick()
	got := r.task(task.ID)
	if got.State != StateRunning || got.Exec == nil || got.Exec.Phase != PhaseWorking {
		t.Fatalf("after the first round: state %s exec %+v", got.State, got.Exec)
	}
	if len(r.hub.created) != 1 {
		t.Fatalf("sessions started: %d", len(r.hub.created))
	}
	req := r.hub.created[0]
	wt := got.Exec.Worktree
	if req.Cwd != wt || req.MachineID != testMachine || req.PermissionMode != "acceptEdits" || !req.Async || req.AgentType != AgentClaude || req.Launcher != "" {
		t.Fatalf("session request: %+v", req)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].Harness != AgentClaude {
		t.Fatalf("attempts: %+v", got.Attempts)
	}
	if !strings.HasPrefix(wt, r.worker.data) || got.Exec.Branch != "ats/"+task.ID+"-1" {
		t.Fatalf("worktree %s branch %s", wt, got.Exec.Branch)
	}
	for _, want := range []string{"Do the thing", got.Exec.Branch, "Do not push", "test -f done.txt", blockedMarker} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("the prompt does not have %q:\n%s", want, req.Prompt)
		}
	}
	if !slices.Equal(r.hub.attached, []string{"todo_1/session-1"}) {
		t.Fatalf("attached: %v", r.hub.attached)
	}

	// The agent works: a commit on the task's branch.
	if err := os.WriteFile(filepath.Join(wt, "done.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, wt, "add", ".")
	runGit(t, wt, "commit", "-q", "-m", "work")
	head := runGit(t, wt, "rev-parse", "HEAD")

	// The hub shows a finished turn for a moment between the agent's steps: one sighting is not the end.
	r.hub.turnEnds("session-1", "u1", "half way")
	r.tick()
	r.hub.set("session-1", func(s *hubSession) { s.State = "working" })
	r.tick()
	if got := r.task(task.ID); got.Exec.Phase != PhaseWorking || got.State != StateRunning {
		t.Fatalf("a flicker ended the turn: %+v", got.Exec)
	}

	r.hub.turnEnds("session-1", "u2", "Added done.txt and committed.")
	r.tick()
	if got := r.task(task.ID); got.Exec.Phase != PhaseWorking {
		t.Fatalf("one sighting of the finished turn was taken: %+v", got.Exec)
	}
	r.tick()
	if got := r.task(task.ID); got.Exec == nil || got.Exec.Phase != PhaseAccepting {
		t.Fatalf("acceptance did not start: %+v", got)
	}
	r.waitAccepted(task.ID, 1)
	r.tick()

	got = r.task(task.ID)
	if got.State != StateSucceeded || got.Verdict != VerdictPass || got.Exec != nil {
		t.Fatalf("final: state %s verdict %s exec %+v", got.State, got.Verdict, got.Exec)
	}
	rep := got.Reports[len(got.Reports)-1]
	if rep.ExitCode == nil || *rep.ExitCode != 0 || !strings.Contains(rep.Message, "checked") {
		t.Fatalf("report: %+v", rep)
	}
	kinds := artifactKinds(got)
	for _, want := range []string{"commit", "branch", "session"} {
		if !slices.Contains(kinds, want) {
			t.Errorf("artifacts %v lack %s", kinds, want)
		}
	}
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the worktree is still there: %v", err)
	}
	if sha := runGit(t, r.repo, "rev-parse", "ats/"+task.ID+"-1"); sha != head {
		t.Fatalf("the task's branch is at %s, the work at %s", sha, head)
	}
	if sha := runGit(t, r.repo, "rev-parse", "main"); sha == head {
		t.Fatal("the task moved the repository's own branch")
	}

	written := r.todos.written()
	if len(written) != 1 || written[0].Choice != "通过验收" || written[0].Ref != "ats:"+task.ID+":succeeded:pass" {
		t.Fatalf("decisions: %+v", written)
	}
	if r.todos.paths[0] != "/api/v1/todos/todo_1/decisions" {
		t.Fatalf("decision path %s", r.todos.paths[0])
	}
	r.tick()
	if n := len(r.todos.written()); n != 1 {
		t.Fatalf("the outcome was written %d times", n)
	}
}

func TestWritingTaskRecordsReleasedHead(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	for _, tc := range []struct {
		name, acceptance string
		pendingEdits     bool
		afterAcceptance  string
	}{
		{name: "agent commit without acceptance"},
		{name: "release commits without acceptance", pendingEdits: true},
		{name: "release commits after acceptance", acceptance: "test -f work.txt", pendingEdits: true},
		{name: "release has no head", acceptance: "test -f work.txt", afterAcceptance: "remove worktree"},
		{name: "release finds the base", acceptance: "test -f work.txt", afterAcceptance: "restore base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			task := r.enqueue(EnqueueInput{Title: "Record work", PermissionProfile: PermissionDefault, AcceptanceCommand: tc.acceptance})
			r.tick()
			e := r.task(task.ID).Exec
			if e == nil || e.Phase != PhaseWorking {
				t.Fatalf("worktree was not prepared: %+v", e)
			}
			if err := os.WriteFile(filepath.Join(e.Worktree, "work.txt"), []byte("work\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGit(t, e.Worktree, "add", "work.txt")
			runGit(t, e.Worktree, "commit", "-qm", "agent work")
			agentHead := runGit(t, e.Worktree, "rev-parse", "HEAD")
			if tc.pendingEdits {
				if err := os.WriteFile(filepath.Join(e.Worktree, "pending.txt"), []byte("pending\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			r.hub.turnEnds(e.SessionID, "u1", "Changed the files.")
			r.tick()
			r.tick()
			wantVerdict := VerdictUnverified
			if tc.acceptance != "" {
				wantVerdict = VerdictPass
				r.waitAccepted(task.ID, 1)
				accepted, err := r.worker.acceptStatus(task.ID, 1)
				if err != nil || accepted.HeadSHA != agentHead || accepted.ExitCode != 0 {
					t.Fatalf("acceptance: %+v (%v), want commit %s and exit 0", accepted, err, agentHead)
				}
				switch tc.afterAcceptance {
				case "remove worktree":
					runGit(t, r.repo, "worktree", "remove", e.Worktree)
				case "restore base":
					runGit(t, e.Worktree, "reset", "--hard", e.BaseSHA)
				}
				r.tick()
			}

			head := e.BaseSHA
			if tc.afterAcceptance != "restore base" {
				head = runGit(t, r.repo, "rev-parse", e.Branch)
			}
			if tc.pendingEdits {
				if parent := runGit(t, r.repo, "rev-parse", head+"^"); parent != agentHead {
					t.Fatalf("release commit parent = %s, want %s", parent, agentHead)
				}
				if got := runGit(t, r.repo, "show", head+":pending.txt"); got != "pending" {
					t.Fatalf("release commit contains %q in pending.txt", got)
				}
			} else if tc.afterAcceptance != "restore base" && head != agentHead {
				t.Fatalf("branch head = %s, want %s", head, agentHead)
			}

			q, err := NewQueue(filepath.Dir(r.q.dataFile), r.cfg.queueConfig())
			if err != nil {
				t.Fatal(err)
			}
			got, err := q.GetTask(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != StateSucceeded || got.Verdict != wantVerdict || len(got.Attempts) != 1 || len(got.Reports) != 1 {
				t.Fatalf("reloaded task: state %s, verdict %s, attempts %d, reports %d", got.State, got.Verdict, len(got.Attempts), len(got.Reports))
			}
			report := got.Reports[0]
			if got.Attempts[0].HeadSHA != head || report.HeadSHA != head {
				t.Errorf("persisted head_sha = %q in attempt and %q in report, want %s", got.Attempts[0].HeadSHA, report.HeadSHA, head)
			}
			wantDirty := tc.pendingEdits && tc.acceptance != ""
			if !reflect.DeepEqual(got.Attempts[0].Dirty, &wantDirty) || !reflect.DeepEqual(report.Dirty, &wantDirty) {
				t.Errorf("attempt and report should both have dirty = %t", wantDirty)
			}
			var commits []string
			for _, a := range report.Artifacts {
				if a.Kind == "commit" {
					commits = append(commits, a.Path)
				}
			}
			var wantCommits []string
			if head != e.BaseSHA {
				wantCommits = []string{head}
			}
			if !slices.Equal(commits, wantCommits) {
				t.Errorf("commit artifacts = %v, want %v", commits, wantCommits)
			}
		})
	}
}

func TestReadonlyTaskRunsInPlanModeAndEndsUnverifiedWithoutAcceptance(t *testing.T) {
	r := newRig(t)
	r.cfg.ModelStandard = "example-model"
	task := r.enqueue(EnqueueInput{Title: "Look into it"})
	r.tick()
	req := r.hub.created[0]
	if req.PermissionMode != "plan" || req.Model != "example-model" || !strings.Contains(req.Prompt, "read-only") {
		t.Fatalf("session request: %+v", req)
	}
	wt := r.task(task.ID).Exec.Worktree
	if branch := runGit(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); branch != "HEAD" {
		t.Fatalf("a read-only task got branch %s", branch)
	}
	r.hub.turnEnds("session-1", "u1", "Found the cause.")
	r.tick()
	r.tick()
	got := r.task(task.ID)
	if got.State != StateSucceeded || got.Verdict != VerdictUnverified {
		t.Fatalf("final: %s %s", got.State, got.Verdict)
	}
	if w := r.todos.written(); len(w) != 1 || w[0].Choice != "跑完了，没有验收命令" {
		t.Fatalf("decisions: %+v", w)
	}
	if out := runGit(t, r.repo, "branch", "--list", "ats/*"); out != "" {
		t.Fatalf("branches left: %s", out)
	}
}

func TestSelectedEngineAndPermissionModeReachSessionCreation(t *testing.T) {
	for name, tc := range map[string]struct {
		permission string
		wantMode   string
	}{
		"read only": {PermissionReadonly, "plan"},
		"writable":  {PermissionDefault, "acceptEdits"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			task := r.enqueue(EnqueueInput{AgentType: AgentCodex, PermissionProfile: tc.permission})
			r.tick()
			if len(r.hub.created) != 1 {
				t.Fatalf("sessions started: %d", len(r.hub.created))
			}
			req := r.hub.created[0]
			if req.AgentType != AgentCodex || req.Launcher != AgentCodex || req.PermissionMode != tc.wantMode || req.MachineID != testMachine {
				t.Fatalf("session request: %+v", req)
			}
			got := r.task(task.ID)
			if len(got.Attempts) != 1 || got.Attempts[0].Harness != AgentCodex {
				t.Fatalf("attempts: %+v", got.Attempts)
			}
		})
	}
}

func TestTaskSessionOverridesReachSessionCreation(t *testing.T) {
	for _, tc := range []struct {
		name, agentType, difficulty, model, launcher string
		wantModel, wantLauncher                      string
	}{
		{"agy engine default", "agy", DifficultyMedium, "", "", "", "agy"},
		{"codex engine default", AgentCodex, DifficultyHard, "", "", "", AgentCodex},
		{"claude explicit overrides", AgentClaude, DifficultyHard, " task-model ", " task-launcher ", "task-model", "task-launcher"},
		{"agy explicit model", "agy", DifficultyMedium, "agy-model", "", "agy-model", "agy"},
		{"codex launcher mapped to machine", AgentCodex, DifficultyMedium, "", "other=elsewhere\n MACHINE-1 = task-launcher ", "", "task-launcher"},
		{"agy launcher map misses machine", "agy", DifficultyMedium, "", "other=elsewhere", "", "agy"},
		{"claude launcher map misses machine", AgentClaude, DifficultyHard, "", "other=elsewhere", "claude-strong", "claude-machine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.cfg.ModelStandard, r.cfg.ModelStrong = "claude-standard", "claude-strong"
			r.cfg.Launcher = "claude-default; " + testMachine + "=claude-machine"
			a := newATSApp(r.q)
			a.cfg = r.cfg
			a.nowFunc = func() time.Time { return r.now }
			body := map[string]string{
				"todo_id": "todo_1", "prompt": "Inspect the code", "repo": r.repo,
				"agent_type": tc.agentType, "difficulty": tc.difficulty,
			}
			if tc.model != "" {
				body["model"] = tc.model
			}
			if tc.launcher != "" {
				body["launcher"] = tc.launcher
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			ans := call(t, a, "POST", "/tasks", string(raw))
			if ans.Status != http.StatusCreated {
				t.Fatalf("enqueue: %d %v", ans.Status, ans.Body)
			}

			r.tick()
			if len(r.hub.created) != 1 {
				t.Fatalf("sessions started: %d", len(r.hub.created))
			}
			req := r.hub.created[0]
			if req.AgentType != tc.agentType || req.Model != tc.wantModel || req.Launcher != tc.wantLauncher {
				t.Fatalf("session engine %q, model %q, launcher %q; want %q, %q, %q",
					req.AgentType, req.Model, req.Launcher, tc.agentType, tc.wantModel, tc.wantLauncher)
			}
			detail := call(t, a, "GET", "/tasks/"+taskOf(t, ans)["id"].(string), "")
			view := taskOf(t, detail)
			if view["launcher"] != tc.wantLauncher ||
				tc.wantModel != "" && view["model"] != tc.wantModel ||
				tc.wantModel == "" && view["model"] != nil {
				t.Fatalf("running task: %v", view)
			}
		})
	}
}

func TestReadonlyPermissionModeReachesSessionCreation(t *testing.T) {
	for _, tc := range []struct {
		name, setting, profile, writeMode, wantMode string
	}{
		{"empty setting", "", PermissionReadonly, "auto", "plan"},
		{"explicit plan", "plan", PermissionReadonly, "auto", "plan"},
		{"explicit bypass", "bypassPermissions", PermissionReadonly, "auto", "bypassPermissions"},
		{"invalid setting", "bypassPermissions ", PermissionReadonly, "auto", "plan"},
		{"write mode stays independent", "bypassPermissions", PermissionDefault, "auto", "auto"},
		{"default write mode stays independent", "bypassPermissions", PermissionDefault, "", "acceptEdits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			raw, err := json.Marshal(map[string]string{
				"readonly_permission_mode": tc.setting, "write_permission_mode": tc.writeMode,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &r.cfg); err != nil {
				t.Fatal(err)
			}
			r.enqueue(EnqueueInput{PermissionProfile: tc.profile})
			r.tick()
			if len(r.hub.created) != 1 {
				t.Fatalf("sessions started: %d", len(r.hub.created))
			}
			req := r.hub.created[0]
			if req.PermissionMode != tc.wantMode {
				t.Fatalf("permission mode = %q, want %q", req.PermissionMode, tc.wantMode)
			}
			if tc.profile == PermissionReadonly {
				if !strings.Contains(req.Prompt, "do not change, create or delete files") || !strings.Contains(req.Prompt, "ExitPlanMode") {
					t.Fatalf("read-only prompt: %s", req.Prompt)
				}
				if branch := runGit(t, req.Cwd, "rev-parse", "--abbrev-ref", "HEAD"); branch != "HEAD" {
					t.Fatalf("read-only task has a branch: %q", branch)
				}
			}
		})
	}
}

func TestSynchronousEngineStartGetsTheLongerBudget(t *testing.T) {
	c := newHubClient("http://example.com", "token")
	if got := sessionStartTimeout(AgentClaude); got != 30*time.Second {
		t.Fatalf("Claude start timeout: %s", got)
	}
	if got := sessionStartTimeout(AgentCodex); got <= 100*time.Second {
		t.Fatalf("Codex start timeout %s does not cover the hub's startup budget", got)
	}
	if c.sessionClientFor(AgentClaude) != c.client || c.sessionClientFor(AgentCodex) != c.syncStartClient {
		t.Fatal("the engine did not select its session-start HTTP client")
	}
	if c.syncStartClient.Timeout != sessionStartTimeout(AgentCodex) || c.client.Timeout != 20*time.Second {
		t.Fatalf("HTTP timeouts: default %s synchronous %s", c.client.Timeout, c.syncStartClient.Timeout)
	}
}

func TestSessionCreationRejectionDoesNotChangeEngineOrPermission(t *testing.T) {
	r := newRig(t)
	zero := 0
	r.hub.createErr = errors.New("unsupported engine and permission combination")
	task := r.enqueue(EnqueueInput{AgentType: AgentCodex, PermissionProfile: PermissionDefault, MaxRetries: &zero})
	r.tick()
	got := r.task(task.ID)
	if got.State != StateFailed || got.FailureReason != "infra_error" || got.Exec != nil {
		t.Fatalf("after refusal: %+v", got)
	}
	if len(r.hub.requests) != 1 || r.hub.requests[0].AgentType != AgentCodex || r.hub.requests[0].Launcher != AgentCodex || r.hub.requests[0].PermissionMode != "acceptEdits" {
		t.Fatalf("session requests: %+v", r.hub.requests)
	}
	if len(r.hub.created) != 0 || len(r.hub.attached) != 0 {
		t.Fatalf("a refused session was used: created %+v attached %+v", r.hub.created, r.hub.attached)
	}
}

func TestSessionWaitingForTheUserIsNeedsInput(t *testing.T) {
	r := newRig(t)
	task := r.enqueue(EnqueueInput{})
	r.tick()

	r.hub.set("session-1", func(s *hubSession) { s.State, s.PendingInteractions = "needs_you", 1 })
	r.tick()
	if got := r.task(task.ID); got.State != StateNeedsInput || got.Exec.Waiting == "" {
		t.Fatalf("a session waiting for approval: %s %+v", got.State, got.Exec)
	}
	if c := r.q.PlanSchedule(r.now).CountsByState; c[StateNeedsInput] != 1 || c[StateRunning] != 0 {
		t.Fatalf("a task waiting for the user is counted as waiting, not running: %v", c)
	}
	// Needing the user is not running late: the lease is kept, the task is not given to another attempt.
	r.now = r.now.Add(3 * time.Hour)
	r.tick()
	if got := r.task(task.ID); got.State != StateNeedsInput || got.AttemptNo != 1 {
		t.Fatalf("after a long wait: %s attempt %d", got.State, got.AttemptNo)
	}

	r.hub.set("session-1", func(s *hubSession) { s.State, s.PendingInteractions = "working", 0 })
	r.now = r.now.Add(-3 * time.Hour) // the agent's own time has not run out
	r.tick()
	if got := r.task(task.ID); got.State != StateRunning || got.Exec.Waiting != "" {
		t.Fatalf("after the user answered: %s %+v", got.State, got.Exec)
	}

	// The agent ends its turn saying it is blocked: needs input, judged once.
	r.hub.turnEnds("session-1", "u5", "I looked around.\n\nATS_BLOCKED: which environment should I check?")
	r.tick()
	r.tick()
	got := r.task(task.ID)
	if got.State != StateNeedsInput || !strings.Contains(got.Exec.Waiting, "which environment") {
		t.Fatalf("a blocked reply: %s %+v", got.State, got.Exec)
	}
	r.tick()
	r.tick()
	if got := r.task(task.ID); got.State != StateNeedsInput {
		t.Fatalf("the blocked turn was judged again: %s", got.State)
	}

	// The user answers in the session; the agent goes on and finishes.
	r.hub.set("session-1", func(s *hubSession) { s.State, s.UpdatedAt = "working", "u6" })
	r.tick()
	r.hub.turnEnds("session-1", "u7", "Checked staging. All fine.")
	r.tick()
	r.tick()
	if got := r.task(task.ID); got.State != StateSucceeded {
		t.Fatalf("final: %s", got.State)
	}
}

func TestFailedAcceptanceIsRetriedAsANewAttemptThenFails(t *testing.T) {
	r := newRig(t)
	one := 1
	task := r.enqueue(EnqueueInput{AcceptanceCommand: "echo broken >&2; exit 3", MaxRetries: &one})

	for attempt := 1; attempt <= 2; attempt++ {
		r.tick()
		got := r.task(task.ID)
		if got.AttemptNo != attempt || got.Exec == nil || got.Exec.Phase != PhaseWorking {
			t.Fatalf("attempt %d did not start: state %s attempt %d", attempt, got.State, got.AttemptNo)
		}
		sid := got.Exec.SessionID
		r.hub.turnEnds(sid, "u1", "done")
		r.tick()
		r.tick()
		r.waitAccepted(task.ID, attempt)
		r.tick()
		got = r.task(task.ID)
		if attempt == 1 {
			if got.State != StateQueued || got.NotBefore == nil || got.FailureReason != "task_failed" {
				t.Fatalf("after the first failure: %s not_before %v reason %s", got.State, got.NotBefore, got.FailureReason)
			}
			if len(r.todos.written()) != 0 {
				t.Fatal("an attempt that will be retried was written to the to-do")
			}
			r.now = got.NotBefore.Add(time.Second)
		}
	}
	got := r.task(task.ID)
	if got.State != StateFailed || got.Verdict != VerdictFail || len(got.Attempts) != 2 {
		t.Fatalf("final: %s %s attempts %d", got.State, got.Verdict, len(got.Attempts))
	}
	rep := got.Reports[len(got.Reports)-1]
	if rep.ExitCode == nil || *rep.ExitCode != 3 || !strings.Contains(rep.Message, "broken") {
		t.Fatalf("report: %+v", rep)
	}
	if len(r.hub.created) != 2 || r.hub.created[0].Cwd == r.hub.created[1].Cwd {
		t.Fatalf("attempts shared a worktree: %+v", r.hub.created)
	}
	if want := []string{"todo_1/session-1", "todo_1/session-2"}; !slices.Equal(r.hub.detached, want) {
		t.Fatalf("detached %v, want %v", r.hub.detached, want)
	}
	if w := r.todos.written(); len(w) != 1 || w[0].Choice != "失败：没通过验收（试了 2 次）" {
		t.Fatalf("decisions: %+v", w)
	}
}

func TestRetryKeepsTheSuccessfulSessionAttached(t *testing.T) {
	r := newRig(t)
	one := 1
	task := r.enqueue(EnqueueInput{MaxRetries: &one, AcceptanceCommand: "true"})
	r.tick()
	r.hub.set("session-1", func(s *hubSession) { s.StartError = "agent failed to start" })
	r.tick()
	failed := r.task(task.ID)
	if failed.State != StateQueued || failed.NotBefore == nil {
		t.Fatalf("after attempt failure: state %s, retry at %v", failed.State, failed.NotBefore)
	}
	if want := []string{"todo_1/session-1"}; !slices.Equal(r.hub.detached, want) {
		t.Fatalf("detached %v before retry, want %v", r.hub.detached, want)
	}

	r.now = *failed.NotBefore
	r.tick()
	r.hub.turnEnds("session-2", "u1", "Done.")
	r.tick()
	r.tick()
	r.waitAccepted(task.ID, 2)
	r.tick()
	got := r.task(task.ID)
	if got.State != StateSucceeded || got.Verdict != VerdictPass || len(got.Attempts) != 2 || len(got.Reports) != 2 {
		t.Fatalf("retry result: state %s, verdict %s, attempts %d, reports %d", got.State, got.Verdict, len(got.Attempts), len(got.Reports))
	}
	if want := []string{"todo_1/session-1", "todo_1/session-2"}; !slices.Equal(r.hub.attached, want) {
		t.Fatalf("attached %v, want %v", r.hub.attached, want)
	}
	if want := []string{"todo_1/session-1"}; !slices.Equal(r.hub.detached, want) {
		t.Fatalf("detached %v after retry, want %v", r.hub.detached, want)
	}
	rep := got.Reports[0]
	if got.Attempts[0].SessionID != "session-1" || rep.SessionID != "session-1" {
		t.Fatalf("failed session missing from history: attempt %q, report %q", got.Attempts[0].SessionID, rep.SessionID)
	}
	if !slices.Contains(rep.Artifacts, Artifact{Kind: "session", Path: testMachine + "/session-1"}) {
		t.Fatalf("failed session missing from evidence: %v", rep.Artifacts)
	}
}

func TestTodoDetachRetriesWhileTheNextAttemptIsActive(t *testing.T) {
	r := newRig(t)
	one := 1
	task := r.enqueue(EnqueueInput{MaxRetries: &one})
	r.tick()
	r.hub.detachErr = errors.New("temporary hub failure")
	r.hub.set("session-1", func(s *hubSession) { s.StartError = "agent failed to start" })
	r.tick()
	failed := r.task(task.ID)
	if failed.State != StateQueued || failed.NotBefore == nil || !failed.Attempts[0].TodoDetachPending {
		t.Fatalf("after detach failure: state %s, retry at %v, attempt %+v", failed.State, failed.NotBefore, failed.Attempts[0])
	}

	q, err := NewQueue(filepath.Dir(r.q.dataFile), r.cfg.queueConfig())
	if err != nil {
		t.Fatal(err)
	}
	r.q = q
	r.start(r.worker.data)
	r.now = *failed.NotBefore
	r.tick()
	running := r.task(task.ID)
	if running.State != StateRunning || running.AttemptNo != 2 || !running.Attempts[0].TodoDetachPending {
		t.Fatalf("after reload and retry: state %s, attempt %d, detach pending %v", running.State, running.AttemptNo, running.Attempts[0].TodoDetachPending)
	}

	r.hub.detachErr = nil
	r.tick()
	if want := []string{"todo_1/session-1", "todo_1/session-1", "todo_1/session-1"}; !slices.Equal(r.hub.detached, want) {
		t.Fatalf("detach calls %v, want %v", r.hub.detached, want)
	}
	if got := r.task(task.ID); got.State != StateRunning || got.Attempts[0].TodoDetachPending {
		t.Fatalf("after detach retry: state %s, detach pending %v", got.State, got.Attempts[0].TodoDetachPending)
	}
}

func TestRepositoryOutsideTheAllowlistFailsWithoutRetry(t *testing.T) {
	r := newRig(t)
	_, other := newTestRepo(t)
	task := r.enqueue(EnqueueInput{Repo: other})
	r.tick()
	got := r.task(task.ID)
	if got.State != StateFailed || got.FailureReason != "rejected" || got.RetriesLeft != got.MaxRetries {
		t.Fatalf("state %s reason %s retries left %d", got.State, got.FailureReason, got.RetriesLeft)
	}
	if len(r.hub.created) != 0 {
		t.Fatal("a session was started for a repository that is not allowed")
	}
	if !strings.Contains(got.Reports[0].Message, "repo_not_allowed") {
		t.Fatalf("report: %s", got.Reports[0].Message)
	}
}

func TestRestartPicksTheAttemptUpWhereItWas(t *testing.T) {
	r := newRig(t)
	task := r.enqueue(EnqueueInput{AcceptanceCommand: "true"})
	r.tick()

	// Simulate a queue written before agent_type existed: the active attempt has no engine fields either.
	r.q.mu.Lock()
	stored := r.q.tasks[task.ID]
	stored.AgentType = ""
	stored.Attempts[len(stored.Attempts)-1].Harness = ""
	saveErr := r.q.saveLocked()
	r.q.mu.Unlock()
	if saveErr != nil {
		t.Fatal(saveErr)
	}
	dir, nodeData := filepath.Dir(r.q.dataFile), r.worker.data

	// Both programs restart, long after the lease would have run out.
	q, err := NewQueue(dir, r.cfg.queueConfig())
	if err != nil {
		t.Fatal(err)
	}
	r.q = q
	r.start(nodeData)
	r.now = r.now.Add(2 * time.Hour)
	r.cfg.TaskTimeoutMinutes = 600
	r.tick()
	got := r.task(task.ID)
	if got.State != StateRunning || got.AttemptNo != 1 || got.Exec.SessionID != "session-1" {
		t.Fatalf("after the restart: %s attempt %d exec %+v", got.State, got.AttemptNo, got.Exec)
	}
	if got.AgentType != "" || got.Attempts[0].Harness != "" || viewTask(got, promptNone).AgentType != AgentClaude {
		t.Fatalf("legacy engine fields changed after the restart: %+v", got)
	}
	if len(r.hub.created) != 1 {
		t.Fatalf("the restart started %d sessions", len(r.hub.created))
	}
	r.hub.turnEnds("session-1", "u1", "done")
	r.tick()
	r.tick()
	r.waitAccepted(task.ID, 1)
	r.tick()
	if got := r.task(task.ID); got.State != StateSucceeded || got.Verdict != VerdictPass {
		t.Fatalf("final: %s %s", got.State, got.Verdict)
	}
}

func TestCancelInterruptsTheSessionAndRemovesTheWorktree(t *testing.T) {
	r := newRig(t)
	task := r.enqueue(EnqueueInput{})
	r.tick()
	wt := r.task(task.ID).Exec.Worktree
	got, err := r.d.cancel(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCanceled || !slices.Equal(r.hub.interrupts, []string{"session-1"}) {
		t.Fatalf("state %s interrupts %v", got.State, r.hub.interrupts)
	}
	if want := []string{"todo_1/session-1"}; !slices.Equal(r.hub.detached, want) {
		t.Fatalf("detached %v, want %v", r.hub.detached, want)
	}
	if got.Attempts[0].SessionID != "session-1" {
		t.Fatalf("canceled session missing from history: %+v", got.Attempts[0])
	}
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the worktree is still there: %v", err)
	}
	if w := r.todos.written(); len(w) != 1 || w[0].Choice != "已取消" {
		t.Fatalf("decisions: %+v", w)
	}
	r.tick()
	if len(r.hub.created) != 1 {
		t.Fatal("a canceled task ran again")
	}
}

func TestCanceledTodoDetachSurvivesPruningUntilRetried(t *testing.T) {
	r := newRig(t)
	task := r.enqueue(EnqueueInput{})
	r.tick()
	r.hub.detachErr = errors.New("temporary hub failure")
	if got, err := r.d.cancel(t.Context(), task.ID); err != nil || got.State != StateCanceled {
		t.Fatalf("cancel: task %+v, error %v", got, err)
	}
	if got := r.task(task.ID); !got.Attempts[0].TodoDetachPending || got.DecisionWrittenRef == "" {
		t.Fatalf("after cancel: detach pending %v, decision ref %q", got.Attempts[0].TodoDetachPending, got.DecisionWrittenRef)
	}

	r.now = r.now.Add(pruneMaxAge)
	r.tick()
	if got := r.task(task.ID); !got.Attempts[0].TodoDetachPending {
		t.Fatal("pruning dropped the pending detach")
	}
	r.hub.detachErr = nil
	r.tick()
	if got := r.task(task.ID); got.Attempts[0].TodoDetachPending || got.Attempts[0].SessionID != "session-1" {
		t.Fatalf("attempt after detach retry: %+v", got.Attempts[0])
	}
}

func TestAgentRunningTooLongIsInterruptedAndTimesOut(t *testing.T) {
	r := newRig(t)
	zero := 0
	r.cfg.TaskTimeoutMinutes = 30
	task := r.enqueue(EnqueueInput{MaxRetries: &zero})
	r.tick()
	r.now = r.now.Add(31 * time.Minute)
	r.tick()
	got := r.task(task.ID)
	if got.State != StateFailed || got.FailureReason != "timeout" || len(r.hub.interrupts) != 1 {
		t.Fatalf("state %s reason %s interrupts %v", got.State, got.FailureReason, r.hub.interrupts)
	}
}

func TestWatchContinuesInterruptedTurnInSameSession(t *testing.T) {
	r := newRig(t)
	task := r.enqueue(EnqueueInput{})
	r.tick()
	original := r.task(task.ID)
	if _, err := r.q.UpdateExec(original.ID, original.FenceToken, r.now, func(e *Execution) {
		e.SeenUpdate, e.Stable = "partial", 1
	}); err != nil {
		t.Fatal(err)
	}
	r.hub.turnFails(original.Exec.SessionID, "error-1", "Server error mid-response")
	r.tick()
	if len(r.hub.messages) != 1 || r.hub.messages[0].MachineID != original.Exec.MachineID || r.hub.messages[0].SessionID != original.Exec.SessionID || r.hub.messages[0].Text == "" {
		t.Fatalf("expected a continuation message to the original session, got %+v", r.hub.messages)
	}
	got := r.task(task.ID)
	if got.State != StateRunning || got.Exec == nil || got.Exec.SessionID != original.Exec.SessionID || got.Exec.Worktree != original.Exec.Worktree || got.RetriesLeft != original.RetriesLeft {
		t.Fatalf("interrupted session or worktree was replaced: %+v", got)
	}
	if e := got.Exec; e.ContinueCount != 1 || e.ContinuedAt == nil || !e.ContinuedAt.Equal(r.now) || e.ContinuedErrorAt != "error-1" ||
		e.SeenUpdate != "" || e.Stable != 0 || !e.PhaseSince.Equal(original.Exec.PhaseSince) {
		t.Fatalf("unexpected continuation state: %+v", e)
	}
	reloaded, err := NewQueue(filepath.Dir(r.q.dataFile), r.cfg.queueConfig())
	if err != nil {
		t.Fatal(err)
	}
	r.q = reloaded
	r.start(r.worker.data)
	r.tick()
	r.tick()
	if got := r.task(task.ID); got.State != StateRunning || len(r.hub.messages) != 1 {
		t.Fatalf("stale turn error triggered another continuation: state=%s messages=%+v", got.State, r.hub.messages)
	}
	r.hub.set(original.Exec.SessionID, func(s *hubSession) {
		s.State, s.UpdatedAt = "working", "working-1"
	})
	r.tick()
	r.tick()
	if e := r.task(task.ID).Exec; e == nil || e.ContinuedAt != nil || e.ContinuedErrorAt != "" || e.ContinueCount != 1 {
		t.Fatalf("continuation status did not clear on working update: %+v", e)
	}
	if len(r.hub.messages) != 1 {
		t.Fatalf("a working session received a duplicate continuation: %+v", r.hub.messages)
	}
	r.hub.set(original.Exec.SessionID, func(s *hubSession) { s.TurnError = "" })
	r.hub.turnEnds(original.Exec.SessionID, "complete", "Completed the remaining inspection.")
	r.tick()
	r.tick()
	if got := r.task(task.ID); got.State != StateSucceeded || got.AttemptNo != 1 || len(r.hub.created) != 1 {
		t.Fatalf("expected the original attempt to finish successfully, got %+v", got)
	}
}

func TestWatchTripsBreakerOnStartupGatewayError(t *testing.T) {
	r := newRig(t)
	live := listenProbeGateway(t)
	defer live.Close()
	r.worker.launcherSettingsDir = t.TempDir()
	writeLauncherSettings(t, r.worker.launcherSettingsDir, "primary-settings.json", `{"base_url":"http://`+live.Addr().String()+`"}`)
	r.cfg.Launcher = "primary|backup"
	task := r.enqueue(EnqueueInput{})
	r.tick()
	if got := r.task(task.ID); got.Exec == nil || got.Exec.Launcher != "primary" {
		t.Fatalf("expected the primary launcher to start, got %+v", got)
	}
	r.hub.turnFails("session-1", "error-1", "HTTP 503 Service Unavailable")
	r.tick()
	got := r.task(task.ID)
	if got.State != StateQueued || got.FailureReason != "gateway_error" || got.NotBefore == nil || len(r.hub.messages) != 0 {
		t.Fatalf("startup gateway error should end the attempt: %+v messages=%+v", got, r.hub.messages)
	}
	r.now = *got.NotBefore
	r.tick()
	if got := r.task(task.ID); got.Exec == nil || got.Exec.Launcher != "backup" || got.AttemptNo != 2 {
		t.Fatalf("expected the retry to use the backup launcher, got %+v", got)
	}
}

func TestWatchStopsContinuingAStaleErrorAfterTwoMessages(t *testing.T) {
	r := newRig(t)
	zero := 0
	task := r.enqueue(EnqueueInput{MaxRetries: &zero})
	r.tick()
	r.hub.turnFails("session-1", "error-1", "stream closed")
	r.tick()
	r.now = r.now.Add(45*time.Second - dispatchInterval)
	r.tick()
	if e := r.task(task.ID).Exec; e == nil || e.ContinueCount != 2 || len(r.hub.messages) != 2 {
		t.Fatalf("expected a second continuation only after grace: exec=%+v messages=%+v", e, r.hub.messages)
	}
	r.now = r.now.Add(45*time.Second - dispatchInterval)
	r.tick()
	if got := r.task(task.ID); got.State != StateFailed || got.FailureReason != "stream_interrupted" || len(r.hub.messages) != 2 {
		t.Fatalf("expected a terminal interruption after two messages: %+v messages=%+v", got, r.hub.messages)
	}
}

func TestWatchContinuesLateGatewayErrorsAndTripsBreakerAfterExhaustion(t *testing.T) {
	r := newRig(t)
	r.cfg.Launcher = "primary|backup"
	zero := 0
	task := r.enqueue(EnqueueInput{MaxRetries: &zero})
	r.tick()
	r.now = r.now.Add(dispatchInterval)
	r.hub.turnFails("session-1", "error-1", "HTTP 502")
	r.tick()
	if e := r.task(task.ID).Exec; e == nil || e.ContinueCount != 1 || len(r.hub.messages) != 1 {
		t.Fatalf("expected gateway error at 30 seconds to continue: exec=%+v messages=%+v", e, r.hub.messages)
	}
	r.hub.set("session-1", func(s *hubSession) {
		s.BackgroundWorking, s.UpdatedAt = true, "resumed"
	})
	r.tick()
	if e := r.task(task.ID).Exec; e == nil || e.ContinuedAt != nil || e.ContinuedErrorAt != "" {
		t.Fatalf("background working did not clear the grace state: %+v", e)
	}
	r.hub.turnFails("session-1", "error-2", "HTTP 504")
	r.tick()
	if e := r.task(task.ID).Exec; e == nil || e.ContinueCount != 2 || e.ContinuedErrorAt != "error-2" {
		t.Fatalf("expected a second continuation: %+v", e)
	}
	r.hub.turnFails("session-1", "error-3", "connection reset")
	r.tick()
	if got := r.task(task.ID); got.State != StateFailed || got.FailureReason != "gateway_error" || len(r.hub.messages) != 2 {
		t.Fatalf("new gateway failure should end the attempt: %+v messages=%+v", got, r.hub.messages)
	}
	if chosen, _, _ := r.d.resolveHealthyLauncher(t.Context(), testMachine, []string{"primary", "backup"}, false); chosen != "backup" {
		t.Fatalf("expected exhausted gateway to be bypassed, got %q", chosen)
	}
}

func TestWatchFailedContinuationUsesOrdinaryRetry(t *testing.T) {
	for _, tc := range []struct {
		class, failure, wantLauncher string
	}{
		{"stream_interrupted", "unexpected EOF", "primary"},
		{"gateway_error", "HTTP 504", "backup"},
	} {
		t.Run(tc.class, func(t *testing.T) {
			r := newRig(t)
			r.cfg.Launcher = "primary|backup"
			r.hub.sendErr = errors.New("message rejected")
			task := r.enqueue(EnqueueInput{})
			r.tick()
			r.now = r.now.Add(dispatchInterval)
			r.hub.turnFails("session-1", "error-1", tc.failure)
			r.tick()
			got := r.task(task.ID)
			if got.State != StateQueued || got.FailureReason != tc.class || got.NotBefore == nil ||
				got.RetriesLeft != task.RetriesLeft-1 || len(r.hub.messages) != 1 || !strings.Contains(got.Reports[0].Message, "message rejected") {
				t.Fatalf("expected a charged retry of %s after send failed, got %+v messages=%+v", tc.class, got, r.hub.messages)
			}
			if chosen, _, _ := r.d.resolveHealthyLauncher(t.Context(), testMachine, []string{"primary", "backup"}, false); chosen != tc.wantLauncher {
				t.Fatalf("expected launcher %q, got %q", tc.wantLauncher, chosen)
			}
		})
	}
}

func TestWatchContinuationRespectsTaskDeadline(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "stale error"
		if resume {
			name = "working"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.cfg.TaskTimeoutMinutes = 1
			zero := 0
			task := r.enqueue(EnqueueInput{MaxRetries: &zero})
			r.tick()
			r.now = r.now.Add(2 * dispatchInterval)
			r.hub.turnFails("session-1", "error-1", "stream ended")
			r.tick()
			if e := r.task(task.ID).Exec; e == nil || e.ContinueCount != 1 {
				t.Fatalf("expected continuation before deadline, got %+v", e)
			}
			if resume {
				r.hub.set("session-1", func(s *hubSession) {
					s.State, s.UpdatedAt, s.TurnError = "working", "resumed", ""
				})
			}
			r.tick()
			if got := r.task(task.ID); got.State != StateFailed || got.FailureReason != "timeout" ||
				!slices.Equal(r.hub.interrupts, []string{"session-1"}) || len(r.hub.messages) != 1 {
				t.Fatalf("continuation extended the deadline: %+v interrupts=%v messages=%+v", got, r.hub.interrupts, r.hub.messages)
			}
		})
	}
}

func TestWatchDoesNotContinueAnErrorAtTheTaskDeadline(t *testing.T) {
	r := newRig(t)
	r.cfg.TaskTimeoutMinutes = 1
	zero := 0
	task := r.enqueue(EnqueueInput{MaxRetries: &zero})
	r.tick()
	r.now = r.now.Add(time.Minute - dispatchInterval)
	r.hub.turnFails("session-1", "error-1", "unexpected EOF")
	r.tick()
	if got := r.task(task.ID); got.State != StateFailed || len(r.hub.messages) != 0 {
		t.Fatalf("sent a continuation at the deadline: %+v messages=%+v", got, r.hub.messages)
	}
}

func TestWatchDoesNotResendWhenContinuationStateCannotBeSaved(t *testing.T) {
	r := newRig(t)
	zero := 0
	task := r.enqueue(EnqueueInput{MaxRetries: &zero})
	r.tick()
	r.hub.turnFails("session-1", "error-1", "Server error mid-response")
	savedFile := r.q.dataFile
	r.q.dataFile = filepath.Join(t.TempDir(), "missing", "queue.json")
	current := r.task(task.ID)
	r.d.watch(t.Context(), current, current.Attempts[0])
	r.q.dataFile = savedFile
	r.tick()
	if got := r.task(task.ID); got.State != StateFailed || got.FailureReason != "infra_error" ||
		!slices.Equal(r.hub.interrupts, []string{"session-1"}) || len(r.hub.messages) != 1 {
		t.Fatalf("continuation send was not contained after save error: %+v interrupts=%v messages=%+v", got, r.hub.interrupts, r.hub.messages)
	}
}

func TestHubClientSendMessagePostsToTheExistingSession(t *testing.T) {
	const text = "Resume this turn."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.EscapedPath() != "/api/v1/sessions/machine%2Fone/session%2Fone/messages" {
			t.Errorf("unexpected continuation request: %s %s", req.Method, req.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || len(body) != 1 || body["text"] != text {
			t.Errorf("continuation body: %v, %v", body, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := newHubClient(srv.URL, "test-token").SendMessage(t.Context(), "machine/one", "session/one", text); err != nil {
		t.Fatal(err)
	}
}

func TestHubClientDetachTodo(t *testing.T) {
	const path = "/api/v1/todos/todo%2Fone/agents/machine%2Fone/session%2Fone"
	for _, status := range []int{http.StatusOK, http.StatusNoContent, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodDelete || req.URL.EscapedPath() != path {
					t.Errorf("detach request: %s %s", req.Method, req.URL.EscapedPath())
				}
				w.WriteHeader(status)
				if status == http.StatusInternalServerError {
					_, _ = w.Write([]byte(`{"code":"internal_error","error":"could not unlink\nextra details"}`))
				}
			}))
			defer srv.Close()
			err := newHubClient(srv.URL, "test-token").DetachTodo(t.Context(), "todo/one", "machine/one", "session/one")
			if status != http.StatusInternalServerError {
				if err != nil {
					t.Fatalf("HTTP %d: %v", status, err)
				}
				return
			}
			var apiErr *hubAPIError
			if !errors.As(err, &apiErr) || apiErr.Status != status {
				t.Fatalf("expected typed HTTP %d error, got %v", status, err)
			}
			if want := "DELETE " + path + ": HTTP 500 (internal_error) could not unlink"; err.Error() != want {
				t.Fatalf("error %q, want %q", err, want)
			}
		})
	}
}

func TestSessionErrorsEndTheAttempt(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(s *hubSession)
		reason string
	}{
		"start error": {func(s *hubSession) { s.StartError = "no agent installed" }, "infra_error"},
		"rate limit":  {func(s *hubSession) { s.TurnError, s.TurnErrorKind = "You've hit your limit", "usage_limit" }, "rate_limited"},
		"signed out":  {func(s *hubSession) { s.TurnError, s.TurnErrorKind = "Not logged in · Please run /login", "user" }, "auth_expired"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			task := r.enqueue(EnqueueInput{})
			r.tick()
			r.hub.set("session-1", tc.change)
			r.tick()
			if got := r.task(task.ID); got.FailureReason != tc.reason || got.Exec != nil {
				t.Fatalf("state %s reason %s", got.State, got.FailureReason)
			}
		})
	}
}

type nodeCallFunc func(context.Context, string, string, any, any) error

func (f nodeCallFunc) Call(ctx context.Context, machine, method string, params, out any) error {
	return f(ctx, machine, method, params, out)
}

func TestResolveHealthyLauncherCachesByMachineAndExpiresOnSchedule(t *testing.T) {
	now := testStart
	firstAvailable := false
	var calls []string
	nodes := nodeCallFunc(func(ctx context.Context, machine, method string, params, out any) error {
		deadline, ok := ctx.Deadline()
		if method != "ats.probe_launcher" || !ok || time.Until(deadline) > 5*time.Second {
			t.Fatalf("unexpected probe RPC or deadline: %q, %v", method, deadline)
		}
		launcher := params.(probeLauncherParams).Launcher
		calls = append(calls, machine+"/"+launcher)
		res := out.(*probeLauncherResult)
		*res = probeLauncherResult{Endpoint: "localhost:443"}
		if launcher == "backup" || firstAvailable || machine != testMachine {
			res.Reachable = true
		} else {
			res.Error = "connection refused"
		}
		return nil
	})
	d := newDispatcher(nil, nil, nodes, nil, nil, func() time.Time { return now })
	candidates := []string{"primary", "backup"}
	check := func(machine string, allowProbe bool, want string, wantCalls int) {
		t.Helper()
		got, all, detail := d.resolveHealthyLauncher(t.Context(), machine, candidates, allowProbe)
		if got != want || all || detail != "" || len(calls) != wantCalls {
			t.Fatalf("launcher for %s: chosen=%q unreachable=%t detail=%q calls=%v, want %q after %d calls", machine, got, all, detail, calls, want, wantCalls)
		}
	}
	if got, all, detail := d.resolveHealthyLauncher(t.Context(), testMachine, nil, true); got != "" || all || detail != "" || len(calls) != 0 {
		t.Fatalf("empty candidates: chosen=%q unreachable=%t detail=%q calls=%v", got, all, detail, calls)
	}
	check(testMachine, false, "primary", 0)
	check(testMachine, true, "backup", 2)
	now = testStart.Add(30*time.Second - time.Nanosecond)
	check(testMachine, true, "backup", 2)
	now = testStart.Add(30 * time.Second)
	check(testMachine, true, "backup", 3)
	check("machine-2", true, "primary", 4)
	firstAvailable = true
	now = testStart.Add(3*time.Minute - time.Nanosecond)
	check(testMachine, false, "backup", 4)
	now = testStart.Add(3 * time.Minute)
	check(testMachine, true, "primary", 5)
	if want := []string{testMachine + "/primary", testMachine + "/backup", testMachine + "/backup", "machine-2/primary", testMachine + "/primary"}; !slices.Equal(calls, want) {
		t.Fatalf("probe order: %v, want %v", calls, want)
	}
}

func TestResolveHealthyLauncherDoesNotCacheSkippedProbesOrRPCErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "native launcher"},
		{name: "older node", err: errors.New("unknown method ats.probe_launcher")},
		{name: "method not found", err: errors.New("method not found")},
		{name: "node offline", err: errors.New("node disconnected")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			nodes := nodeCallFunc(func(_ context.Context, _, _ string, params, out any) error {
				calls = append(calls, params.(probeLauncherParams).Launcher)
				if tc.err != nil {
					return tc.err
				}
				*out.(*probeLauncherResult) = probeLauncherResult{Reachable: true, Skipped: true}
				return nil
			})
			d := newDispatcher(nil, nil, nodes, nil, nil, func() time.Time { return testStart })
			for range 2 {
				got, all, detail := d.resolveHealthyLauncher(t.Context(), testMachine, []string{"primary", "backup"}, true)
				if got != "primary" || all || detail != "" {
					t.Fatalf("expected primary, got chosen=%q unreachable=%t detail=%q", got, all, detail)
				}
			}
			if !slices.Equal(calls, []string{"primary", "primary"}) {
				t.Fatalf("expected two uncached primary probes, got %v", calls)
			}
		})
	}
}

func TestResolveHealthyLauncherPreservesBreakerOpenedDuringProbe(t *testing.T) {
	var d *dispatcher
	nodes := nodeCallFunc(func(_ context.Context, machine, _ string, params, out any) error {
		if launcher := params.(probeLauncherParams).Launcher; launcher == "primary" {
			d.invalidateProbe(machine, launcher, "HTTP 503")
		}
		*out.(*probeLauncherResult) = probeLauncherResult{Reachable: true}
		return nil
	})
	d = newDispatcher(nil, nil, nodes, nil, nil, func() time.Time { return testStart })
	if got, _, _ := d.resolveHealthyLauncher(t.Context(), testMachine, []string{"primary", "backup"}, true); got != "backup" {
		t.Fatalf("late TCP success closed the primary breaker: %q", got)
	}
}

func TestDispatcherRefreshAvoidsDegradedMachineAndRecognizesRecovery(t *testing.T) {
	r := newRig(t)
	const alternate = "machine-2"
	r.cfg.Machines = []string{testMachine, alternate, "offline"}
	r.hub.machines = append(r.hub.machines, hubMachine{ID: alternate, Online: true}, hubMachine{ID: "offline"}, hubMachine{ID: "disabled", Online: true})
	r.cfg.Launcher = "primary," + alternate + "=primary|native"
	r.worker.launcherSettingsDir = t.TempDir()
	writeLauncherSettings(t, r.worker.launcherSettingsDir, "primary-settings.json", `{"base_url":"http://`+closedProbeGateway(t)+`"}`)
	r.d.refreshMachines(t.Context())
	slots := r.q.Machines()
	if len(slots) != 3 || slots[0].Name != testMachine || !slots[0].Degraded || slots[1].Degraded || slots[2].Degraded ||
		!slices.Equal(r.nodes.calls, []string{"ats.probe_launcher", "ats.probe_launcher", "ats.probe_launcher"}) {
		t.Fatalf("expected only %s degraded, slots=%+v calls=%v", testMachine, slots, r.nodes.calls)
	}
	task := r.enqueue(EnqueueInput{})
	r.tick()
	if got := r.task(task.ID); got.Exec == nil || got.Exec.MachineID != alternate || got.Exec.Launcher != "native" {
		t.Fatalf("expected the healthy machine and its fallback, got %+v", got)
	}
	live := listenProbeGateway(t)
	defer live.Close()
	writeLauncherSettings(t, r.worker.launcherSettingsDir, "primary-settings.json", `{"base_url":"http://`+live.Addr().String()+`"}`)
	r.now = r.now.Add(3 * time.Minute)
	r.d.refreshMachines(t.Context())
	if got := r.q.Machines()[0]; got.Degraded {
		t.Fatalf("recovered machine is still degraded: %+v", got)
	}
}

func TestDispatcherRefreshProbesOnlineMachinesIndependently(t *testing.T) {
	r := newRig(t)
	const alternate = "machine-2"
	r.cfg.Launcher = "primary"
	r.cfg.Machines = []string{testMachine, alternate}
	r.hub.machines = append(r.hub.machines, hubMachine{ID: alternate, Online: true})
	alternateProbed := make(chan struct{})
	r.d.nodes = nodeCallFunc(func(ctx context.Context, machine, _ string, _, out any) error {
		if machine == testMachine {
			select {
			case <-alternateProbed:
			case <-ctx.Done():
				return ctx.Err()
			}
		} else {
			close(alternateProbed)
		}
		*out.(*probeLauncherResult) = probeLauncherResult{Error: "connection refused"}
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	r.d.refreshMachines(ctx)
	slots := r.q.Machines()
	if len(slots) != 2 || !slots[0].Degraded || !slots[1].Degraded {
		t.Fatalf("slow probe prevented independent probes: %+v", slots)
	}
}

func TestDispatcherProbesExplicitLaunchersForNonClaudeEngines(t *testing.T) {
	for _, tc := range []struct {
		launcher  string
		want      string
		wantCalls []string
	}{
		{want: AgentCodex, wantCalls: []string{"ats.prepare"}},
		{launcher: "primary|backup", want: "backup", wantCalls: []string{"ats.probe_launcher", "ats.probe_launcher", "ats.prepare"}},
	} {
		t.Run(tc.want, func(t *testing.T) {
			r := newRig(t)
			r.worker.launcherSettingsDir = t.TempDir()
			writeLauncherSettings(t, r.worker.launcherSettingsDir, "primary-settings.json", `{"base_url":"http://`+closedProbeGateway(t)+`"}`)
			r.enqueue(EnqueueInput{AgentType: AgentCodex, Launcher: tc.launcher})
			r.tick()
			if len(r.hub.created) != 1 || r.hub.created[0].Launcher != tc.want || !slices.Equal(r.nodes.calls, tc.wantCalls) {
				t.Fatalf("expected launcher %q after %v, sessions=%+v RPCs=%v", tc.want, tc.wantCalls, r.hub.created, r.nodes.calls)
			}
		})
	}
}

func TestDispatcherReroutesUnreachableLaunchersBeforePreparing(t *testing.T) {
	r := newRig(t)
	const alternate = "machine-2"
	r.cfg.Machines = []string{testMachine, alternate}
	r.hub.machines = append(r.hub.machines, hubMachine{ID: alternate, Online: true})
	r.worker.launcherSettingsDir = t.TempDir()
	addr := closedProbeGateway(t)
	for _, launcher := range []string{"primary", "backup"} {
		writeLauncherSettings(t, r.worker.launcherSettingsDir, launcher+"-settings.json", `{"base_url":"http://`+addr+`"}`)
	}
	task := r.enqueue(EnqueueInput{Launcher: testMachine + "=primary|backup," + alternate + "=native"})
	r.tick()
	got := r.task(task.ID)
	if got.State != StateQueued || got.FailureReason != "gateway_unreachable" || got.NotBefore != nil || got.RetriesLeft != task.RetriesLeft {
		t.Fatalf("expected a retry ready to reroute with its original budget, got %+v", got)
	}
	if got.Attempts[0].Machine != testMachine || got.Attempts[0].Worktree != "" || len(r.hub.created) != 0 ||
		!slices.Equal(r.nodes.calls, []string{"ats.probe_launcher", "ats.probe_launcher"}) {
		t.Fatalf("failed machine created work: attempts=%+v, sessions=%+v, RPCs=%v", got.Attempts, r.hub.created, r.nodes.calls)
	}
	if _, err := os.Stat(filepath.Join(r.worker.data, "worktrees")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected worktree directory: %v", err)
	}
	if msg := got.Reports[0].Message; !strings.Contains(msg, "primary") || !strings.Contains(msg, "backup") || !strings.Contains(msg, addr) {
		t.Fatalf("unreachable launcher details: %q", msg)
	}
	r.tick()
	got = r.task(task.ID)
	if got.State != StateRunning || got.AttemptNo != 2 || got.Exec.MachineID != alternate || got.Exec.Launcher != "native" || got.RetriesLeft != task.RetriesLeft {
		t.Fatalf("next tick did not start on the alternate machine: %+v", got)
	}
}

func TestNothingRunsWithoutAComputerOrOverItsSlots(t *testing.T) {
	r := newRig(t)
	a := r.enqueue(EnqueueInput{Title: "first", Priority: PriorityP0})
	b := r.enqueue(EnqueueInput{Title: "second"})
	r.tick()
	if ta, tb := r.task(a.ID), r.task(b.ID); ta.State != StateRunning || tb.State != StateQueued {
		t.Fatalf("one slot: first %s second %s", ta.State, tb.State)
	}

	r2 := newRig(t)
	r2.cfg.Machines = nil
	c := r2.enqueue(EnqueueInput{})
	r2.now = r2.now.Add(2 * refreshInterval)
	r2.tick()
	if got := r2.task(c.ID); got.State != StateQueued || len(r2.hub.created) != 0 {
		t.Fatalf("with no computer selected: %s", got.State)
	}
}

func TestBlockedReason(t *testing.T) {
	for reply, want := range map[string]string{
		"All done.": "",
		"Work.\nATS_BLOCKED: need the staging URL":    "need the staging URL",
		"Work.\n\n`ATS_BLOCKED: need access`\n":       "need access",
		"ATS_BLOCKED: early\n1\n2\n3\n4\nfinished ok": "",
	} {
		got, blocked := blockedReason(reply)
		if got != want || blocked != (want != "") {
			t.Errorf("%q: %q %v", reply, got, blocked)
		}
	}
}

func TestCreateSessionFailureCleansUpBranchAndRecordsNoPhantomArtifacts(t *testing.T) {
	r := newRig(t)
	one := 1
	r.hub.createErr = errors.New("hub unavailable")
	task := r.enqueue(EnqueueInput{Title: "Writable task", PermissionProfile: PermissionDefault, MaxRetries: &one})

	r.tick()
	got := r.task(task.ID)
	if got.State != StateQueued || got.NotBefore == nil || got.FailureReason != "infra_error" {
		t.Fatalf("after CreateSession failure: state %s not_before %v reason %s", got.State, got.NotBefore, got.FailureReason)
	}
	if branches := runGit(t, r.repo, "branch", "--list", "ats/*"); branches != "" {
		t.Fatalf("leaked untouched branch after CreateSession failure: %q", branches)
	}
	key, err := attemptKey(task.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.worker.worktree(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leaked worktree after CreateSession failure: %v", err)
	}
	if kinds := artifactKinds(got); len(kinds) != 0 {
		t.Fatalf("fabricated artifacts after CreateSession failure: %+v", got.Reports[0].Artifacts)
	}

	// Retry after CreateSession failure must start fresh from base, not pretend to resume a phantom commit.
	r.hub.createErr = nil
	r.now = got.NotBefore.Add(time.Second)
	r.tick()
	if len(r.hub.created) != 1 {
		t.Fatalf("retry sessions started: %d", len(r.hub.created))
	}
	if strings.Contains(r.hub.created[0].Prompt, "continue from it") {
		t.Fatalf("retry prompt falsely claimed to resume a commit:\n%s", r.hub.created[0].Prompt)
	}
}

func TestCancelDuringCreateSessionInterruptsSessionAndReleasesWorktree(t *testing.T) {
	r := newRig(t)
	task := r.enqueue(EnqueueInput{Title: "Cancel during start", PermissionProfile: PermissionDefault})
	var wt string
	r.hub.onCreateSession = func(req sessionRequest) {
		wt = req.Cwd
		if _, err := r.d.cancel(context.Background(), task.ID); err != nil {
			t.Errorf("cancel during CreateSession: %v", err)
		}
	}

	r.tick()
	got := r.task(task.ID)
	if got.State != StateCanceled || got.Exec != nil {
		t.Fatalf("after cancel during CreateSession: state %s exec %+v", got.State, got.Exec)
	}
	if !slices.Equal(r.hub.interrupts, []string{"session-1"}) {
		t.Fatalf("leaked live session (interrupts=%v, want [session-1])", r.hub.interrupts)
	}
	if len(r.hub.attached) != 0 {
		t.Fatalf("attached canceled session to to-do: %v", r.hub.attached)
	}
	if wt == "" {
		t.Fatal("CreateSession was not invoked with a worktree path")
	}
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leaked worktree %s: %v", wt, err)
	}
	if branches := runGit(t, r.repo, "branch", "--list", "ats/*"); branches != "" {
		t.Fatalf("leaked branch: %q", branches)
	}
}

func TestCancelDuringAttachDetachesSessionAfterAttachReturns(t *testing.T) {
	r := newRig(t)
	task := r.enqueue(EnqueueInput{})
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseAttach := sync.OnceFunc(func() { close(release) })
	var linked atomic.Bool
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if want := "/api/v1/todos/todo_1/agents/" + testMachine + "/session-1"; req.URL.Path != want {
			t.Errorf("request path %q, want %q", req.URL.Path, want)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch req.Method {
		case http.MethodPut:
			close(started)
			select {
			case <-release:
				linked.Store(true)
				w.WriteHeader(http.StatusNoContent)
			case <-req.Context().Done():
			}
		case http.MethodDelete:
			deletes.Add(1)
			if linked.Swap(false) {
				w.WriteHeader(http.StatusNoContent)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			t.Errorf("unexpected method %s", req.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	r.d.api = todoAttachTestHub{fakeHub: r.hub, client: newHubClient(srv.URL, "test-token")}
	go func() {
		defer close(done)
		r.d.tick(t.Context())
	}()
	defer func() {
		releaseAttach()
		<-done
	}()
	select {
	case <-started:
	case <-done:
		t.Fatal("dispatcher returned before starting the attachment")
	}
	cctx, cancel := context.WithCancel(t.Context())
	cancel()
	canceled, err := r.d.cancel(cctx, task.ID)
	if err != nil || canceled.State != StateCanceled {
		t.Fatalf("cancel during attachment: task %+v, error %v", canceled, err)
	}
	releaseAttach()
	<-done

	if linked.Load() || deletes.Load() != 2 {
		t.Fatalf("after attachment returned: linked %v, DELETE requests %d, want false and 2", linked.Load(), deletes.Load())
	}
}

func TestCancelDuringPrepareReleasesWorktreeWithoutStartingSession(t *testing.T) {
	r := newRig(t)
	task := r.enqueue(EnqueueInput{Title: "Cancel during prepare", PermissionProfile: PermissionDefault})
	r.nodes.afterPrepare = func() {
		if _, err := r.d.cancel(context.Background(), task.ID); err != nil {
			t.Errorf("cancel during prepare: %v", err)
		}
	}

	r.tick()
	got := r.task(task.ID)
	if got.State != StateCanceled || got.Exec != nil {
		t.Fatalf("after cancel during prepare: state %s exec %+v", got.State, got.Exec)
	}
	if len(r.hub.requests) != 0 || len(r.hub.created) != 0 {
		t.Fatalf("started session for canceled task: requests=%d created=%d", len(r.hub.requests), len(r.hub.created))
	}
	key, err := attemptKey(task.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.worker.worktree(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leaked worktree after cancel during prepare: %v", err)
	}
	if branches := runGit(t, r.repo, "branch", "--list", "ats/*"); branches != "" {
		t.Fatalf("leaked branch after cancel during prepare: %q", branches)
	}
}

func TestCancelAlreadyTerminalTaskIsNoop(t *testing.T) {
	r := newRig(t)
	task := r.enqueue(EnqueueInput{Title: "Already finished"})
	r.tick()
	r.hub.turnEnds("session-1", "u1", "Done.")
	r.tick()
	r.tick()
	if got := r.task(task.ID); got.State != StateSucceeded {
		t.Fatalf("expected succeeded, got %s", got.State)
	}
	callsBefore := len(r.nodes.calls)
	interruptsBefore := len(r.hub.interrupts)
	got, err := r.d.cancel(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateSucceeded {
		t.Fatalf("terminal state changed to %s", got.State)
	}
	if len(r.nodes.calls) != callsBefore || len(r.hub.interrupts) != interruptsBefore {
		t.Fatalf("cancel on terminal task ran cleanup: node calls %d->%d interrupts %d->%d",
			callsBefore, len(r.nodes.calls), interruptsBefore, len(r.hub.interrupts))
	}
}

func TestUpdateExecRollsBackInMemoryStateOnSaveError(t *testing.T) {
	r := newRig(t)
	task := r.enqueue(EnqueueInput{Title: "Save error rollback"})
	_, att, err := r.q.LeaseTask(task.ID, "dispatcher", testMachine, r.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.q.UpdateExec(task.ID, att.FenceToken, r.now, func(e *Execution) {
		e.MachineID, e.Phase = testMachine, PhasePreparing
	}); err != nil {
		t.Fatal(err)
	}

	// Point dataFile at a nonexistent parent directory so saveLocked fails deterministically.
	origFile := r.q.dataFile
	r.q.mu.Lock()
	r.q.dataFile = filepath.Join(t.TempDir(), "missing", "sub", "queue.json")
	r.q.mu.Unlock()
	_, err = r.q.UpdateExec(task.ID, att.FenceToken, r.now, func(e *Execution) {
		e.Phase, e.SessionID = PhaseWorking, "session-unsaved"
	})
	r.q.mu.Lock()
	r.q.dataFile = origFile
	r.q.mu.Unlock()
	if err == nil {
		t.Fatal("expected UpdateExec to fail when queue file cannot be saved")
	}

	got := r.task(task.ID)
	if got.Exec == nil || got.Exec.Phase != PhasePreparing || got.Exec.SessionID != "" {
		t.Fatalf("UpdateExec left dirty in-memory state after save failure: %+v", got.Exec)
	}
}

func TestReadonlyTaskAutoDeclinesExitPlanMode(t *testing.T) {
	t.Run("readonly bypass auto-declines plan exit up to limit and succeeds", func(t *testing.T) {
		r := newRig(t)
		r.cfg.ReadonlyPermissionMode = "bypassPermissions"
		task := r.enqueue(EnqueueInput{Title: "Readonly plan guard"})
		r.tick()

		for i := 1; i <= maxReadonlyPlanDeclines; i++ {
			ixID := "ix_plan_" + string(rune('0'+i))
			r.hub.mu.Lock()
			r.hub.interactions = []hubInteraction{{
				ID: ixID, MachineID: testMachine, SessionID: "session-1",
				Kind: "plan", ToolName: "ExitPlanMode",
			}}
			r.hub.sessions["session-1"].State = "needs_you"
			r.hub.sessions["session-1"].PendingInteractions = 1
			r.hub.mu.Unlock()

			r.tick()
			got := r.task(task.ID)
			if got.State != StateRunning {
				t.Fatalf("decline %d moved task to %s (%+v)", i, got.State, got.Exec)
			}
		}

		r.hub.mu.Lock()
		resolvedCount := len(r.hub.resolved)
		firstRes := r.hub.resolved[0]
		r.hub.mu.Unlock()
		if resolvedCount != maxReadonlyPlanDeclines {
			t.Fatalf("resolved %d interactions, want %d", resolvedCount, maxReadonlyPlanDeclines)
		}
		if firstRes.Res.Approve == nil || *firstRes.Res.Approve {
			t.Fatalf("expected approve=false, got %+v", firstRes.Res)
		}

		// Exceeding maxReadonlyPlanDeclines falls back to needs_input.
		r.hub.mu.Lock()
		r.hub.interactions = []hubInteraction{{
			ID: "ix_plan_over", MachineID: testMachine, SessionID: "session-1",
			Kind: "plan", ToolName: "ExitPlanMode",
		}}
		r.hub.sessions["session-1"].State = "needs_you"
		r.hub.sessions["session-1"].PendingInteractions = 1
		r.hub.mu.Unlock()

		r.tick()
		if got := r.task(task.ID); got.State != StateNeedsInput || got.Exec.Waiting != "Agent 提交了计划，等你批准" {
			t.Fatalf("after limit: state=%s waiting=%q", got.State, got.Exec.Waiting)
		}

		// Manual approval resumes and finishes cleanly.
		r.hub.turnEnds("session-1", "u2", "Done after staying in plan mode.")
		r.tick()
		r.tick()
		if got := r.task(task.ID); got.State != StateSucceeded {
			t.Fatalf("final state: %s", got.State)
		}
	})

	t.Run("writable task with plan card enters needs_input without auto-decline", func(t *testing.T) {
		r := newRig(t)
		task := r.enqueue(EnqueueInput{Title: "Writable plan", PermissionProfile: PermissionDefault})
		r.tick()

		r.hub.mu.Lock()
		r.hub.interactions = []hubInteraction{{
			ID: "ix_write_plan", MachineID: testMachine, SessionID: "session-1",
			Kind: "plan", ToolName: "ExitPlanMode",
		}}
		r.hub.sessions["session-1"].State = "needs_you"
		r.hub.sessions["session-1"].PendingInteractions = 1
		r.hub.mu.Unlock()

		r.tick()
		got := r.task(task.ID)
		if got.State != StateNeedsInput || got.Exec.Waiting != "Agent 提交了计划，等你批准" {
			t.Fatalf("writable task: state=%s waiting=%q", got.State, got.Exec.Waiting)
		}
		r.hub.mu.Lock()
		resolvedCount := len(r.hub.resolved)
		r.hub.mu.Unlock()
		if resolvedCount != 0 {
			t.Fatalf("writable task should not auto-decline ExitPlanMode, got %d resolves", resolvedCount)
		}
	})
}
