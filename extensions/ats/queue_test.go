package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustEnqueue(t *testing.T, q *Queue, in EnqueueInput) *Task {
	t.Helper()
	if in.Prompt == "" {
		in.Prompt = "Do the thing"
	}
	if in.Now.IsZero() {
		in.Now = testStart
	}
	res, err := q.Enqueue(in)
	if err != nil {
		t.Fatal(err)
	}
	return res.Task
}

func TestTasksAreReadOnlyUnlessTheyAskToWrite(t *testing.T) {
	q := newTestQueue(t, DefaultQueueConfig())
	if got := mustEnqueue(t, q, EnqueueInput{}); got.PermissionProfile != PermissionReadonly || got.State != StateQueued || got.Verdict != VerdictUnverified {
		t.Fatalf("defaults: %+v", got)
	}
	if got := mustEnqueue(t, q, EnqueueInput{PermissionProfile: PermissionDefault}); got.PermissionProfile != PermissionDefault {
		t.Fatalf("a writing task: %s", got.PermissionProfile)
	}
	if _, err := q.Enqueue(EnqueueInput{Prompt: "x", PermissionProfile: "bypass"}); err == nil {
		t.Fatal("an unknown permission profile was taken")
	}
	if _, err := q.Enqueue(EnqueueInput{Prompt: " "}); err == nil {
		t.Fatal("an empty prompt was taken")
	}
}

func TestNormalizeAgentType(t *testing.T) {
	for raw, want := range map[string]string{
		"": AgentClaude, "  ": AgentClaude, "Claude": AgentClaude,
		"codex": AgentCodex, "  CoDeX  ": AgentCodex, "custom_engine": "custom_engine",
	} {
		if got, err := NormalizeAgentType(raw); err != nil || got != want {
			t.Errorf("NormalizeAgentType(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"bad engine", "1codex", "codex;rm", strings.Repeat("a", 33)} {
		if _, err := NormalizeAgentType(raw); err == nil {
			t.Errorf("NormalizeAgentType(%q) accepted an unsupported engine", raw)
		}
	}
}

func TestSessionOptionsPersistAcrossLeasesAndRetries(t *testing.T) {
	dir := t.TempDir()
	q, err := NewQueue(dir, DefaultQueueConfig())
	if err != nil {
		t.Fatal(err)
	}
	const model, launcher = "engine-model", testMachine + "=local,other=remote"
	task := mustEnqueue(t, q, EnqueueInput{AgentType: AgentCodex, Model: " " + model + " ", Launcher: " " + launcher + " "})
	if task.AgentType != AgentCodex || task.Model != model || task.Launcher != launcher {
		t.Fatalf("new task: %+v", task)
	}

	q, err = NewQueue(dir, DefaultQueueConfig())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := q.GetTask(task.ID)
	if err != nil || stored.AgentType != AgentCodex || stored.Model != model || stored.Launcher != launcher {
		t.Fatalf("reloaded task: %+v %v", stored, err)
	}
	_, first, err := q.LeaseTask(task.ID, "dispatcher", testMachine, testStart)
	if err != nil || first.Harness != AgentCodex {
		t.Fatalf("first attempt: %+v %v", first, err)
	}
	after, err := q.CompleteAttempt(CompleteAttemptInput{TaskID: task.ID, FenceToken: first.FenceToken, WorkerID: "dispatcher",
		Outcome: StateFailed, ErrorClass: "infra_error", Message: "connection reset", Now: testStart.Add(time.Second)})
	if err != nil || after.State != StateQueued || after.NotBefore == nil {
		t.Fatalf("retryable failure: %+v %v", after, err)
	}
	leased, second, err := q.LeaseTask(task.ID, "dispatcher", testMachine, after.NotBefore.Add(time.Second))
	if err != nil || second.Harness != AgentCodex || leased.Model != model || leased.Launcher != launcher {
		t.Fatalf("second lease: %+v %v", leased, err)
	}
	got, _ := q.GetTask(task.ID)
	if len(got.Attempts) != 2 || got.Attempts[0].Harness != AgentCodex || got.Attempts[1].Harness != AgentCodex {
		t.Fatalf("stored attempt engines: %+v", got.Attempts)
	}
}

func TestConcurrentReplaysKeepOriginalSessionOverrides(t *testing.T) {
	q := newTestQueue(t, DefaultQueueConfig())
	task := mustEnqueue(t, q, EnqueueInput{IdempotencyKey: "session-overrides", Model: "first-model", Launcher: "first-launcher"})
	var replays sync.WaitGroup
	for range 8 {
		replays.Go(func() {
			got, err := q.Enqueue(EnqueueInput{
				IdempotencyKey: task.IdempotencyKey, Prompt: "Replay", Model: "replayed-model", Launcher: "replayed-launcher",
			})
			if err != nil || got.Created || got.Task.ID != task.ID || got.Task.Model != task.Model || got.Task.Launcher != task.Launcher {
				t.Errorf("replay: %+v, %v", got, err)
				return
			}
			got.Task.Model, got.Task.Launcher = "changed copy", "changed copy"
		})
	}
	replays.Wait()
	tasks, err := q.ListTasks(TaskFilter{})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("after concurrent replays: %d tasks, %v", len(tasks), err)
	}
	if tasks[0].Model != "first-model" || tasks[0].Launcher != "first-launcher" {
		t.Fatalf("stored overrides changed: %+v", tasks[0])
	}
}

func TestLegacyTaskWithoutAgentTypeUsesClaude(t *testing.T) {
	q := newTestQueue(t, DefaultQueueConfig())
	task := mustEnqueue(t, q, EnqueueInput{})
	q.mu.Lock()
	q.tasks[task.ID].AgentType = ""
	saveErr := q.saveLocked()
	q.mu.Unlock()
	if saveErr != nil {
		t.Fatal(saveErr)
	}
	raw, err := os.ReadFile(q.dataFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"agent_type"`, `"model"`, `"launcher"`} {
		if strings.Contains(string(raw), field) {
			t.Fatalf("legacy fixture still has %s: %s", field, raw)
		}
	}

	legacy, err := NewQueue(filepath.Dir(q.dataFile), DefaultQueueConfig())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := legacy.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v := viewTask(stored, promptNone); stored.AgentType != "" || v.AgentType != AgentClaude || v.Model != "" || v.Launcher != "" {
		t.Fatalf("legacy task view: %+v", v)
	}
	_, att, err := legacy.LeaseTask(task.ID, "dispatcher", testMachine, testStart)
	if err != nil || att.Harness != AgentClaude {
		t.Fatalf("legacy attempt: %+v %v", att, err)
	}
}

func TestALateReportOfAnOldAttemptIsRefused(t *testing.T) {
	cfg := DefaultQueueConfig()
	cfg.RetryBaseDelay = time.Second
	q := newTestQueue(t, cfg)
	task := mustEnqueue(t, q, EnqueueInput{})
	now := testStart

	_, first, err := q.LeaseTask(task.ID, "dispatcher", testMachine, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.LeaseTask(task.ID, "dispatcher", testMachine, now); err == nil {
		t.Fatal("a running task was leased again")
	}
	after, err := q.CompleteAttempt(CompleteAttemptInput{TaskID: task.ID, FenceToken: first.FenceToken, WorkerID: "dispatcher",
		Outcome: StateFailed, ErrorClass: "infra_error", Message: "connection reset", Now: now.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if after.State != StateQueued || after.NotBefore == nil || after.RetriesLeft != cfg.MaxRetries-1 {
		t.Fatalf("after a retryable failure: %+v", after)
	}
	if _, _, err := q.LeaseTask(task.ID, "dispatcher", testMachine, now.Add(time.Second)); err == nil {
		t.Fatal("the task was leased before its retry delay was over")
	}
	now = after.NotBefore.Add(time.Second)
	_, second, err := q.LeaseTask(task.ID, "dispatcher", testMachine, now)
	if err != nil {
		t.Fatal(err)
	}
	if second.FenceToken <= first.FenceToken {
		t.Fatalf("fence tokens: %d then %d", first.FenceToken, second.FenceToken)
	}

	// The first attempt reports late: refused, the second attempt is untouched.
	_, err = q.CompleteAttempt(CompleteAttemptInput{TaskID: task.ID, FenceToken: first.FenceToken, WorkerID: "dispatcher",
		Outcome: StateSucceeded, Verdict: VerdictPass, Now: now})
	if !errors.Is(err, ErrStaleAttempt) {
		t.Fatalf("a stale report: %v", err)
	}
	if got, _ := q.GetTask(task.ID); got.State != StateRunning || got.AttemptNo != 2 {
		t.Fatalf("after the stale report: %s attempt %d", got.State, got.AttemptNo)
	}

	zero := 0
	done, err := q.CompleteAttempt(CompleteAttemptInput{TaskID: task.ID, FenceToken: second.FenceToken, WorkerID: "dispatcher",
		Outcome: StateSucceeded, Verdict: VerdictPass, ExitCode: &zero, Now: now.Add(time.Second)})
	if err != nil || done.State != StateSucceeded || done.Verdict != VerdictPass || !done.IsTerminal() {
		t.Fatalf("the second attempt: %+v %v", done, err)
	}
	if _, err := q.CancelTask(task.ID, now); err == nil {
		t.Log("canceling a finished task is a no-op")
	}
	if got, _ := q.GetTask(task.ID); got.State != StateSucceeded {
		t.Fatalf("a finished task changed state: %s", got.State)
	}
}

func TestGetTaskClonesDirtyEvidence(t *testing.T) {
	q := newTestQueue(t, DefaultQueueConfig())
	task := mustEnqueue(t, q, EnqueueInput{})
	_, att, err := q.LeaseTask(task.ID, "dispatcher", testMachine, testStart)
	if err != nil {
		t.Fatal(err)
	}
	get := func() *Task {
		t.Helper()
		got, err := q.GetTask(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	check := func(field string, actual *bool, want bool) {
		t.Helper()
		if actual == nil {
			t.Fatalf("%s dirty is missing", field)
		}
		if *actual != want {
			t.Errorf("%s dirty = %t, want %t", field, *actual, want)
		}
	}

	activeDirty := true
	if _, err := q.UpdateExec(task.ID, att.FenceToken, testStart, func(e *Execution) {
		e.Dirty = &activeDirty
	}); err != nil {
		t.Fatal(err)
	}
	activeDirty = false
	active := get()
	check("execution", active.Exec.Dirty, true)
	*active.Exec.Dirty = false
	check("active attempt", active.Attempts[0].Dirty, true)
	*active.Attempts[0].Dirty = false
	stored := get()
	check("stored execution", stored.Exec.Dirty, true)
	check("stored active attempt", stored.Attempts[0].Dirty, true)

	completedDirty := false
	if _, err := q.CompleteAttempt(CompleteAttemptInput{
		TaskID: task.ID, FenceToken: att.FenceToken, WorkerID: att.WorkerID,
		Outcome: StateSucceeded, Dirty: &completedDirty, Now: testStart,
	}); err != nil {
		t.Fatal(err)
	}
	completedDirty = true
	done := get()
	check("completed attempt", done.Attempts[0].Dirty, false)
	*done.Attempts[0].Dirty = true
	check("report", done.Reports[0].Dirty, false)
	*done.Reports[0].Dirty = true
	stored = get()
	check("stored completed attempt", stored.Attempts[0].Dirty, false)
	check("stored report", stored.Reports[0].Dirty, false)
}

func TestQueueLoadsLegacyDirtyEvidence(t *testing.T) {
	q := newTestQueue(t, DefaultQueueConfig())
	legacy := `{"version":2,"tasks":[{"id":"task_legacy","exec":{"dirty":true},"attempts":[{"dirty":true},{}],"reports":[{"dirty":true},{}]}]}`
	if err := os.WriteFile(q.dataFile, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewQueue(filepath.Dir(q.dataFile), q.Config())
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.GetTask("task_legacy")
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec == nil || len(got.Attempts) != 2 || len(got.Reports) != 2 {
		t.Fatal("missing records after loading legacy queue")
	}
	for field, dirty := range map[string]*bool{"execution": got.Exec.Dirty, "attempt": got.Attempts[0].Dirty, "report": got.Reports[0].Dirty} {
		if dirty == nil || !*dirty {
			t.Errorf("legacy %s lost dirty = true", field)
		}
	}
	if got.Attempts[1].Dirty != nil || got.Reports[1].Dirty != nil {
		t.Error("missing legacy dirty field should remain unknown")
	}
}

func TestGetTaskClonesContinuationTime(t *testing.T) {
	q := newTestQueue(t, DefaultQueueConfig())
	task := mustEnqueue(t, q, EnqueueInput{})
	_, att, err := q.LeaseTask(task.ID, "worker-1", testMachine, testStart)
	if err != nil {
		t.Fatal(err)
	}
	continuedAt := testStart
	updated, err := q.UpdateExec(task.ID, att.FenceToken, testStart, func(e *Execution) {
		e.Phase = PhaseWorking
		e.ContinueCount, e.ContinuedAt, e.ContinuedErrorAt = 1, &continuedAt, "error-1"
	})
	if err != nil {
		t.Fatal(err)
	}
	*updated.Exec.ContinuedAt = testStart.Add(time.Minute)
	got, err := q.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e := got.Exec; e.ContinuedAt == nil || !e.ContinuedAt.Equal(testStart) || e.ContinueCount != 1 || e.ContinuedErrorAt != "error-1" {
		t.Fatalf("continuation state aliased a caller snapshot: %+v", e)
	}
}

func TestGatewayUnreachableRetryUsesAlternateCapacity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		alt      MachineSlot
		machine  string
		fill     bool
		wantFast bool
	}{
		{name: "free alternate", alt: MachineSlot{Name: "machine-2", Online: true, MaxSlots: 1}, wantFast: true},
		{name: "busy alternate", alt: MachineSlot{Name: "machine-2", Online: true, MaxSlots: 1}, fill: true, wantFast: true},
		{name: "offline alternate", alt: MachineSlot{Name: "machine-2", MaxSlots: 1}},
		{name: "degraded alternate", alt: MachineSlot{Name: "machine-2", Online: true, Degraded: true, MaxSlots: 1}},
		{name: "pinned to failing machine", alt: MachineSlot{Name: "machine-2", Online: true, MaxSlots: 1}, machine: "Primary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := newTestQueue(t, DefaultQueueConfig())
			if err := q.SetMachines([]MachineSlot{{Name: testMachine, Title: "Primary", Online: true, MaxSlots: 1}, tc.alt}); err != nil {
				t.Fatal(err)
			}
			if tc.fill {
				busy := mustEnqueue(t, q, EnqueueInput{Title: "occupy alternate"})
				if _, _, err := q.LeaseTask(busy.ID, "worker-2", tc.alt.Name, testStart); err != nil {
					t.Fatal(err)
				}
			}
			task := mustEnqueue(t, q, EnqueueInput{Machine: tc.machine})
			_, att, err := q.LeaseTask(task.ID, "worker-1", testMachine, testStart)
			if err != nil {
				t.Fatal(err)
			}
			got, err := q.CompleteAttempt(CompleteAttemptInput{
				TaskID: task.ID, FenceToken: att.FenceToken, WorkerID: att.WorkerID,
				Outcome: StateFailed, ErrorClass: "gateway_unreachable", Now: testStart,
			})
			if err != nil || got.State != StateQueued || got.FailureReason != "gateway_unreachable" {
				t.Fatalf("expected queued gateway retry: %+v, %v", got, err)
			}
			if tc.wantFast {
				if got.NotBefore != nil || got.RetriesLeft != task.RetriesLeft {
					t.Fatalf("expected immediate retry with no charge: %+v", got)
				}
			} else if got.NotBefore == nil || !got.NotBefore.After(testStart) || got.RetriesLeft != task.RetriesLeft-1 {
				t.Fatalf("expected a charged retry with backoff: %+v", got)
			}
		})
	}
}

func TestGatewayUnreachableRetriesAreBounded(t *testing.T) {
	q := newTestQueue(t, DefaultQueueConfig())
	if err := q.SetMachines([]MachineSlot{{Name: testMachine, Online: true, MaxSlots: 1}, {Name: "machine-2", Online: true, MaxSlots: 1}}); err != nil {
		t.Fatal(err)
	}
	zero := 0
	task := mustEnqueue(t, q, EnqueueInput{MaxRetries: &zero})
	for i := 1; i <= 4; i++ {
		_, att, err := q.LeaseTask(task.ID, "worker-1", testMachine, testStart)
		if err != nil {
			t.Fatal(err)
		}
		got, err := q.CompleteAttempt(CompleteAttemptInput{
			TaskID: task.ID, FenceToken: att.FenceToken, WorkerID: att.WorkerID,
			Outcome: StateFailed, ErrorClass: "gateway_unreachable", Now: testStart,
		})
		want := StateQueued
		if i == 4 {
			want = StateFailed
		}
		if err != nil || got.State != want || got.NotBefore != nil || got.RetriesLeft != 0 {
			t.Fatalf("attempt %d: want %s with no delay or charge, got %+v, %v", i, want, got, err)
		}
	}
}

func TestRetryDelayIsDeterministicAndGrows(t *testing.T) {
	base, most := 5*time.Second, 10*time.Minute
	d1 := ComputeRetryDelay("task_42", 1, base, most)
	if d1 != ComputeRetryDelay("task_42", 1, base, most) {
		t.Fatal("the delay of one attempt differs between calls")
	}
	d2, d3 := ComputeRetryDelay("task_42", 2, base, most), ComputeRetryDelay("task_42", 3, base, most)
	if d2 <= d1 || d3 <= d2 {
		t.Fatalf("delays do not grow: %v %v %v", d1, d2, d3)
	}
	if d := ComputeRetryDelay("task_42", 40, base, most); d > most {
		t.Fatalf("delay %v is over the cap", d)
	}
}

func TestAdmissionCaps(t *testing.T) {
	cfg := DefaultQueueConfig()
	cfg.MaxQueued = 2
	q := newTestQueue(t, cfg)
	mustEnqueue(t, q, EnqueueInput{IdempotencyKey: "k1"})
	mustEnqueue(t, q, EnqueueInput{IdempotencyKey: "k2"})
	// A replay of a task already admitted still answers at the cap.
	if replay, err := q.Enqueue(EnqueueInput{Prompt: "Do the thing", IdempotencyKey: "k1"}); err != nil || replay.Created {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	_, err := q.Enqueue(EnqueueInput{Prompt: "one more"})
	var adm *AdmissionError
	if !errors.As(err, &adm) || adm.Code != "too_many_queued" {
		t.Fatalf("over the queue cap: %v", err)
	}

	q = newTestQueue(t, DefaultQueueConfig())
	label := map[string]string{PersonaTodoLabel: "todo_1"}
	var last *Task
	for range 5 {
		last = mustEnqueue(t, q, EnqueueInput{Labels: label})
	}
	if _, err := q.Enqueue(EnqueueInput{Prompt: "sixth", Labels: label}); !errors.As(err, &adm) || adm.Code != "too_many_for_todo" {
		t.Fatalf("over the per-to-do cap: %v", err)
	}
	mustEnqueue(t, q, EnqueueInput{Labels: map[string]string{PersonaTodoLabel: "todo_2"}})
	// A finished task frees its place.
	if _, err := q.CancelTask(last.ID, testStart); err != nil {
		t.Fatal(err)
	}
	mustEnqueue(t, q, EnqueueInput{Labels: label})
}

func TestSchedulerOrdersByPriorityAndHoldsBackByQuota(t *testing.T) {
	now := testStart
	q := newTestQueue(t, DefaultQueueConfig())
	if err := q.SetMachines([]MachineSlot{{Name: testMachine, Online: true, MaxSlots: 4}}); err != nil {
		t.Fatal(err)
	}
	p3 := mustEnqueue(t, q, EnqueueInput{Title: "background", Priority: PriorityP3})
	p0 := mustEnqueue(t, q, EnqueueInput{Title: "urgent", Priority: PriorityP0})
	p2 := mustEnqueue(t, q, EnqueueInput{Title: "normal"})

	plan := q.PlanSchedule(now)
	if plan.AdmissionMode != AdmissionFull || len(plan.AdmittedCandidates) != 3 || plan.AdmittedCandidates[0].TaskID != p0.ID {
		t.Fatalf("with full quota: mode %s, %d admitted", plan.AdmissionMode, len(plan.AdmittedCandidates))
	}

	// One account of four is cooling down: three quarters remain, everything still goes.
	soon := now.Add(10 * time.Minute)
	accounts := func(cooling int) GenericGatewayAccounts {
		ga := GenericGatewayAccounts{Sources: []GenericGatewaySource{{ID: "pool", Name: "Pool", Type: "relay"}}}
		for i := range 4 {
			acc := GenericGatewayAccount{Source: "pool", AccountID: "acc_" + string(rune('1'+i)), Status: "active"}
			if i < cooling {
				acc.Status, acc.CooldownUntil = "cooldown", &soon
			}
			ga.Accounts = append(ga.Accounts, acc)
		}
		return ga
	}
	st := ParseGatewayAccounts(accounts(1), now)
	if st.ActiveAccounts != 3 || st.TotalAccounts != 4 || st.RemainingPct != 75 || st.Exhausted || st.CooldownUntil != nil {
		t.Fatalf("one of four cooling down: %+v", st)
	}
	_ = q.SetQuotaStatus(st)
	if plan := q.PlanSchedule(now); plan.AdmissionMode != AdmissionFull {
		t.Fatalf("mode with three of four accounts: %s", plan.AdmissionMode)
	}

	// Three of four: below the reserve line, only P0 and P1 go.
	_ = q.SetQuotaStatus(ParseGatewayAccounts(accounts(3), now))
	plan = q.PlanSchedule(now)
	if plan.AdmissionMode != AdmissionPriorityOnly || len(plan.AdmittedCandidates) != 1 || plan.AdmittedCandidates[0].TaskID != p0.ID {
		t.Fatalf("below the reserve line: mode %s, %d admitted", plan.AdmissionMode, len(plan.AdmittedCandidates))
	}

	// All four: nothing goes until the first one is back.
	st = ParseGatewayAccounts(accounts(4), now)
	if !st.Exhausted || st.CooldownUntil == nil {
		t.Fatalf("all cooling down: %+v", st)
	}
	_ = q.SetQuotaStatus(st)
	if leased, _, plan, err := q.DispatchNext(8, now); err != nil || len(leased) != 0 || plan.AdmissionMode != AdmissionPausedQuota {
		t.Fatalf("with the quota used up: %d leased, mode %s, %v", len(leased), plan.AdmissionMode, err)
	}
	// The next reading of the accounts, after the cooldown, opens the queue again.
	_ = q.SetQuotaStatus(ParseGatewayAccounts(accounts(0), soon.Add(time.Minute)))
	if leased, _, _, err := q.DispatchNext(8, soon.Add(time.Minute)); err != nil || len(leased) != 3 {
		t.Fatalf("after the cooldown: %d leased, %v", len(leased), err)
	}
	for _, id := range []string{p0.ID, p2.ID, p3.ID} {
		if got, _ := q.GetTask(id); got.State != StateRunning {
			t.Fatalf("%s is %s", got.Title, got.State)
		}
	}
}

func TestTasksWaitForAnOnlineComputerWithAFreeSlot(t *testing.T) {
	q := newTestQueue(t, DefaultQueueConfig())
	_ = q.SetMachines([]MachineSlot{{Name: "machine-1", Online: false, MaxSlots: 1}, {Name: "machine-2", Online: true, MaxSlots: 1}})
	pinned := mustEnqueue(t, q, EnqueueInput{Title: "pinned", Machine: "machine-1", Priority: PriorityP0})
	a := mustEnqueue(t, q, EnqueueInput{Title: "a"})
	b := mustEnqueue(t, q, EnqueueInput{Title: "b"})
	leased, attempts, _, err := q.DispatchNext(8, testStart)
	if err != nil || len(leased) != 1 || leased[0].ID != a.ID || attempts[0].Machine != "machine-2" {
		t.Fatalf("leased %d, %v", len(leased), err)
	}
	for _, id := range []string{pinned.ID, b.ID} {
		if got, _ := q.GetTask(id); got.State != StateQueued {
			t.Fatalf("%s is %s", got.Title, got.State)
		}
	}
}

func TestQueueSurvivesARestartAndRenewsLeases(t *testing.T) {
	dir := t.TempDir()
	q, err := NewQueue(dir, DefaultQueueConfig())
	if err != nil {
		t.Fatal(err)
	}
	_ = q.SetMachines([]MachineSlot{{Name: testMachine, Online: true, MaxSlots: 1}})
	task := mustEnqueue(t, q, EnqueueInput{Title: "kept"})
	_, att, err := q.LeaseTask(task.ID, "dispatcher", testMachine, testStart)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpdateExec(task.ID, att.FenceToken, testStart, func(e *Execution) {
		e.Phase, e.MachineID, e.SessionID = PhaseWorking, testMachine, "session-1"
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpdateExec(task.ID, att.FenceToken+1, testStart, func(e *Execution) { e.Phase = PhaseAccepting }); !errors.Is(err, ErrStaleAttempt) {
		t.Fatalf("an update with another fence token: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "queue-state.json"))
	if err != nil || !strings.Contains(string(raw), `"version": 2`) && !strings.Contains(string(raw), `"version":2`) {
		t.Fatalf("the stored queue has no version 2: %v", err)
	}

	again, err := NewQueue(dir, DefaultQueueConfig())
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.GetTask(task.ID)
	if err != nil || got.State != StateRunning || got.Exec == nil || got.Exec.SessionID != "session-1" {
		t.Fatalf("after the restart: %+v %v", got, err)
	}
	// Hours later the dispatcher renews before it reports: the attempt is still its own.
	late := testStart.Add(3 * time.Hour)
	if err := again.RenewLeases(late); err != nil {
		t.Fatal(err)
	}
	done, err := again.CompleteAttempt(CompleteAttemptInput{TaskID: task.ID, FenceToken: att.FenceToken, WorkerID: "dispatcher",
		Outcome: StateSucceeded, Verdict: VerdictUnverified, Now: late})
	if err != nil || done.State != StateSucceeded || done.Exec != nil {
		t.Fatalf("report after the restart: %+v %v", done, err)
	}

	// A file of a newer layout is not opened and not overwritten.
	newer := t.TempDir()
	if err := os.WriteFile(filepath.Join(newer, "queue-state.json"), []byte(`{"version":99,"tasks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewQueue(newer, DefaultQueueConfig()); err == nil {
		t.Fatal("a queue file of a newer version was opened")
	}
}

func TestPruneDropsOldFinishedTasksOnly(t *testing.T) {
	q := newTestQueue(t, DefaultQueueConfig())
	finish := func(title string, at time.Time) *Task {
		task := mustEnqueue(t, q, EnqueueInput{Title: title, Now: at})
		if _, err := q.CancelTask(task.ID, at); err != nil {
			t.Fatal(err)
		}
		return task
	}
	old := finish("old", testStart.Add(-40*24*time.Hour))
	recent := finish("recent", testStart.Add(-time.Hour))
	newest := finish("newest", testStart)
	open := mustEnqueue(t, q, EnqueueInput{Title: "open", Now: testStart.Add(-60 * 24 * time.Hour)})
	// Finished long ago, but its outcome has not reached the to-do yet.
	waiting := mustEnqueue(t, q, EnqueueInput{Title: "waiting", Labels: map[string]string{PersonaTodoLabel: "todo_1"}, Now: testStart.Add(-50 * 24 * time.Hour)})
	if _, err := q.CancelTask(waiting.ID, testStart.Add(-50*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	if n, err := q.Prune(testStart, 30*24*time.Hour, 500); err != nil || n != 1 {
		t.Fatalf("pruned by age: %d %v", n, err)
	}
	if _, err := q.GetTask(old.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("the old task: %v", err)
	}
	if n, err := q.Prune(testStart, 30*24*time.Hour, 1); err != nil || n != 1 {
		t.Fatalf("pruned by count: %d %v", n, err)
	}
	if _, err := q.GetTask(recent.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("the older of two finished tasks: %v", err)
	}
	for _, id := range []string{newest.ID, open.ID, waiting.ID} {
		if _, err := q.GetTask(id); err != nil {
			t.Fatalf("a task that must stay is gone: %v", err)
		}
	}
}

func TestPendingTodoDetachSkipsActiveAttemptAndRollsBackOnSaveError(t *testing.T) {
	q := newTestQueue(t, DefaultQueueConfig())
	task := mustEnqueue(t, q, EnqueueInput{Labels: map[string]string{PersonaTodoLabel: "todo_1"}})
	_, att, err := q.LeaseTask(task.ID, "worker", testMachine, testStart)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpdateExec(task.ID, att.FenceToken, testStart, func(e *Execution) {
		e.SessionID = "session-1"
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.SetAttemptTodoDetachPending(task.ID, att.AttemptNo, true); err != nil {
		t.Fatal(err)
	}
	if got := q.PendingTodoDetaches(); len(got) != 0 {
		t.Fatalf("active attempt eligible for detach: %+v", got)
	}
	if _, err := q.CancelTask(task.ID, testStart); err != nil {
		t.Fatal(err)
	}

	q.dataFile = filepath.Join(t.TempDir(), "missing", "queue.json")
	if err := q.SetAttemptTodoDetachPending(task.ID, att.AttemptNo, false); err == nil {
		t.Fatal("expected queue save to fail")
	}
	want := pendingTodoDetach{TaskID: task.ID, AttemptNo: att.AttemptNo, TodoID: "todo_1", MachineID: testMachine, SessionID: "session-1"}
	if got := q.PendingTodoDetaches(); len(got) != 1 || got[0] != want {
		t.Fatalf("pending detaches after save failure: %+v, want %+v", got, want)
	}
}

func TestOutcomeIsWrittenToTheTodoOnce(t *testing.T) {
	q := newTestQueue(t, DefaultQueueConfig())
	todos := newTodoServer(t)
	zero, none := 0, 0
	run := func(in EnqueueInput, done CompleteAttemptInput) *Task {
		task := mustEnqueue(t, q, in)
		_, att, err := q.LeaseTask(task.ID, "dispatcher", testMachine, testStart)
		if err != nil {
			t.Fatal(err)
		}
		done.TaskID, done.FenceToken, done.WorkerID, done.Now = task.ID, att.FenceToken, "dispatcher", testStart.Add(time.Minute)
		if _, err := q.CompleteAttempt(done); err != nil {
			t.Fatal(err)
		}
		return task
	}
	pass := run(EnqueueInput{Title: "Verify the release", AcceptanceCommand: "go test ./...", Labels: map[string]string{PersonaTodoLabel: "todo_ok"}},
		CompleteAttemptInput{Outcome: StateSucceeded, Verdict: VerdictPass, ExitCode: &zero, Message: "all 24 checks green",
			Artifacts: []Artifact{{Kind: "commit", Path: "0123456789abcdef0123456789abcdef01234567"}, {Kind: "branch", Path: "ats/example-1"}, {Kind: "session", Path: "machine-1/session-1"}}})
	gone := run(EnqueueInput{Labels: map[string]string{PersonaTodoLabel: "todo_missing"}, MaxRetries: &none},
		CompleteAttemptInput{Outcome: StateFailed, ErrorClass: "task_failed", Message: "failed check"})
	run(EnqueueInput{Title: "no to-do"}, CompleteAttemptInput{Outcome: StateSucceeded, Verdict: VerdictUnverified})

	writer := NewDecisionWriter(todos.srv.URL, "test-token", []string{"todos.write"}, todos.srv.Client())
	results, err := writer.SyncQueueOutcomes(context.Background(), q)
	if err != nil || len(results) != 2 {
		t.Fatalf("first sync: %+v %v", results, err)
	}
	written := todos.written()
	if len(written) != 1 || written[0].Choice != "通过验收" || written[0].Ref != "ats:"+pass.ID+":succeeded:pass" {
		t.Fatalf("decisions: %+v", written)
	}
	for _, want := range []string{"go test ./...", "ats/example-1", "提交 0123456"} {
		if !strings.Contains(written[0].Reason, want) {
			t.Errorf("the reason does not have %q: %s", want, written[0].Reason)
		}
	}
	if strings.Contains(written[0].Reason, "session-1") {
		t.Errorf("the reason names the session, which the to-do already has: %s", written[0].Reason)
	}
	if got, _ := q.GetTask(pass.ID); got.DecisionWrittenRef != written[0].Ref {
		t.Fatalf("the task does not remember its decision: %+v", got)
	}
	// The to-do is gone: skipped for good, not tried on every round.
	if got, _ := q.GetTask(gone.ID); got.DecisionSkipReason == "" {
		t.Fatal("a task whose to-do is gone is not marked")
	}
	if results, err := writer.SyncQueueOutcomes(context.Background(), q); err != nil || len(results) != 0 {
		t.Fatalf("second sync: %+v %v", results, err)
	}

	// Without the scope nothing is sent.
	bare := NewDecisionWriter(todos.srv.URL, "test-token", []string{"agents.read"}, todos.srv.Client())
	if bare.HasTodosWriteScope() {
		t.Fatal("a writer without the todos.write scope says it may write")
	}
}
