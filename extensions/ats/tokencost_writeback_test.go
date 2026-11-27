package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Token cost of what the extension writes back to the to-do and of what it hands the worker agent, measured with
// estTokens. The rows with a key are held to their budgets in tokenBudgets (tokencost_budget_test.go); the
// evidence rows are only logged, because their worktree artifacts are temporary paths.

// wbPrompt is a realistic task prompt of about 1.5KB, Chinese and English mixed.
func wbPrompt() string {
	var b strings.Builder
	b.WriteString("修复 todo 列表在分页时重复返回同一条任务的问题。\n\n")
	b.WriteString("背景：用户按 limit=20 连续翻页时，第二页的第一条和第一页的最后一条是同一个任务，")
	b.WriteString("因为排序只按 updated_at，而多个任务的 updated_at 相同。\n\n")
	b.WriteString("Requirements:\n")
	b.WriteString("1. Sort by (updated_at DESC, id DESC) so the order is total and stable across pages.\n")
	b.WriteString("2. Add an opaque cursor (base64 of the last row's updated_at and id) and accept it as `cursor` on GET /tasks.\n")
	b.WriteString("3. Keep the old `offset` parameter working for one release; log a deprecation warning when it is used.\n")
	b.WriteString("4. 不要改动 evidence 接口的返回格式，也不要动 writeback 的 ref 规则。\n")
	b.WriteString("5. Add tests: two tasks with the same updated_at must appear exactly once across two pages of size 1.\n\n")
	b.WriteString("相关文件：api.go（listTasks）、queue.go（ListTasks / TaskFilter）、api_test.go。\n")
	b.WriteString("完成后在回复末尾用 Summary: 写清楚改了什么、测试结果、还有什么没做。\n\n")
	b.WriteString("Notes from the reviewer of the previous attempt at this (closed PR):\n")
	b.WriteString("- the cursor must not leak internal ids beyond what GET /tasks already returns;\n")
	b.WriteString("- the page size cap stays at 100; an invalid cursor is a 400 with code bad_cursor, not a 500;\n")
	b.WriteString("- keep the JSON field order of taskView unchanged, the Persona app parses it positionally in one place.\n")
	b.WriteString("验收：go test ./... 全部通过，并且 go vet 没有新的警告。")
	return b.String()
}

// wbCIScript prints about 2KB of test output, Chinese and English mixed, and exits with its first argument.
func wbCIScript() string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	for i := 1; i <= 14; i++ {
		fmt.Fprintf(&b, "echo '=== RUN   TestListTasksCursorPage%02d'\n", i)
		fmt.Fprintf(&b, "echo '--- PASS: TestListTasksCursorPage%02d (0.0%ds) 分页游标第 %d 页'\n", i, i%10, i)
	}
	b.WriteString("echo '=== RUN   TestListTasksSameUpdatedAt'\n")
	b.WriteString("echo '    api_test.go:212: page 2 repeats task_7 (also last of page 1): 排序不稳定'\n")
	b.WriteString("echo '    api_test.go:213: want ids [task_9 task_8] then [task_7 task_6], got [task_9 task_7] then [task_7 task_6]'\n")
	b.WriteString("echo '--- FAIL: TestListTasksSameUpdatedAt (0.01s)'\n")
	b.WriteString("echo 'FAIL'\n")
	b.WriteString("echo 'FAIL	github.com/nwverse/persona-extensions/extensions/ats	1.234s'\n")
	b.WriteString("exit \"$1\"\n")
	return b.String()
}

// wbReply is the agent's last reply of attempt n: some narration and a five-line summary.
func wbReply(n int) string {
	return fmt.Sprintf(`我先读了 api.go 的 listTasks 和 queue.go 的 ListTasks，确认排序只按 updated_at。
Then I changed the sort to (updated_at DESC, id DESC), added an opaque base64 cursor and kept offset with a deprecation log.
I ran go test ./... and go vet ./...; details are below.

Summary:
- 第 %d 次尝试：排序改成 (updated_at DESC, id DESC)，GET /tasks 支持 cursor，offset 仍可用并打印弃用警告。
- 新增 TestListTasksSameUpdatedAt 和 14 个分页测试。
- go vet 无新警告；TestListTasksSameUpdatedAt 仍失败：同一 updated_at 下第二页重复 task_7。
- 怀疑 queue.ListTasks 在加锁前复制了切片，排序发生在复制之前。
- 未做：bad_cursor 的 400 错误码测试。`, n)
}

type wbRow struct {
	key         string // its budget in tokenBudgets; "" for a row that is only logged
	name        string
	text        string
	bytes, toks int
	note        string
}

// wbDecisionText is what a reader of the to-do sees of a decision: its question, choice and reason.
func wbDecisionText(p OutcomeDecisionPayload) string {
	return p.Question + "\n" + p.Choice + "\n" + p.Reason
}

// wbRig is a rig whose repository has the CI script committed.
func wbRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	if err := os.WriteFile(filepath.Join(r.repo, "ci.sh"), []byte(wbCIScript()), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, r.repo, "add", ".")
	runGit(t, r.repo, "commit", "-q", "-m", "ci")
	return r
}

// wbRunAttempt runs one attempt from its start to the acceptance result.
func wbRunAttempt(t *testing.T, r *rig, taskID string, n int, withAcceptance bool) {
	t.Helper()
	r.tick()
	got := r.task(taskID)
	if got.Exec == nil || got.Exec.Phase != PhaseWorking {
		t.Fatalf("attempt %d did not start: %s %+v", n, got.State, got.Exec)
	}
	if err := os.WriteFile(filepath.Join(got.Exec.Worktree, fmt.Sprintf("work%d.txt", n)), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.hub.turnEnds(got.Exec.SessionID, fmt.Sprintf("u%d", n), wbReply(n))
	r.tick()
	r.tick()
	if withAcceptance {
		r.waitAccepted(taskID, n)
		r.tick()
	}
}

func TestTokenCostWorker(t *testing.T) {
	prompt := wbPrompt()
	title := "修复列表分页重复返回同一条"
	var rows []wbRow
	var roots []string // the rigs' temporary directories, which no budgeted text may contain
	addKey := func(key, name, s, note string) {
		rows = append(rows, wbRow{key, name, s, len(s), estTokens(s), note})
	}
	add := func(name, s, note string) { addKey("", name, s, note) }
	rigFor := func() *rig {
		r := wbRig(t)
		roots = append(roots, r.root)
		return r
	}
	add("fixture: user prompt", prompt, "what the user/assistant wrote; not overhead")
	add("fixture: acceptance output", strings.TrimSpace(func() string {
		// the script's output, as the node worker would capture it
		var lines []string
		for _, l := range strings.Split(wbCIScript(), "\n") {
			if s, ok := strings.CutPrefix(l, "echo '"); ok {
				lines = append(lines, strings.TrimSuffix(s, "'"))
			}
		}
		return strings.Join(lines, "\n")
	}()), "printed by the acceptance command")

	// 1. Success, verdict pass, first attempt.
	{
		r := rigFor()
		task := r.enqueue(EnqueueInput{Title: title, Prompt: prompt, PermissionProfile: PermissionDefault, AcceptanceCommand: "sh ci.sh 0"})
		wbRunAttempt(t, r, task.ID, 1, true)
		got := r.task(task.ID)
		w := r.todos.written()
		if got.State != StateSucceeded || got.Verdict != VerdictPass || len(w) != 1 {
			t.Fatalf("pass: state %s verdict %s decisions %d", got.State, got.Verdict, len(w))
		}
		body, _ := json.Marshal(w[0])
		addKey("wb.pass", "write-back: success pass (visible text)", wbDecisionText(w[0]), "1 decision")
		add("write-back: success pass (POST body)", string(body), "")
		ev, _ := json.Marshal(got.Reports[len(got.Reports)-1])
		add("evidence last_report: success pass", string(ev), "for duplication with write-back")
		t.Logf("SAMPLE success pass decision:\n%s\nref=%s", wbDecisionText(w[0]), w[0].Ref)
	}

	// 2. Success with no acceptance command: unverified.
	{
		r := rigFor()
		task := r.enqueue(EnqueueInput{Title: title, Prompt: prompt, PermissionProfile: PermissionDefault})
		wbRunAttempt(t, r, task.ID, 1, false)
		got := r.task(task.ID)
		w := r.todos.written()
		if got.State != StateSucceeded || got.Verdict != VerdictUnverified || len(w) != 1 {
			t.Fatalf("unverified: state %s verdict %s decisions %d", got.State, got.Verdict, len(w))
		}
		addKey("wb.unverified", "write-back: success unverified (visible text)", wbDecisionText(w[0]), "1 decision")
		rep := got.Reports[len(got.Reports)-1]
		ev, _ := json.Marshal(rep)
		add("evidence last_report: success unverified", string(ev), "message = whole reply, summary = its tail again")
		add("  of which report.message (whole reply)", rep.Message, "")
		add("  of which report.summary", rep.Summary, "duplicate of the end of message")
		t.Logf("SAMPLE success unverified decision:\n%s", wbDecisionText(w[0]))
	}

	// 3. Three attempts, all fail the acceptance command: final failure.
	var prompts []string
	{
		r := rigFor()
		task := r.enqueue(EnqueueInput{Title: title, Prompt: prompt, PermissionProfile: PermissionDefault, AcceptanceCommand: "sh ci.sh 1"})
		for n := 1; n <= 3; n++ {
			wbRunAttempt(t, r, task.ID, n, true)
			got := r.task(task.ID)
			if n < 3 {
				if got.State != StateQueued || got.NotBefore == nil {
					t.Fatalf("after attempt %d: %s", n, got.State)
				}
				if w := r.todos.written(); len(w) != 0 {
					t.Fatalf("a decision was written before the task ended: %+v", w)
				}
				r.now = got.NotBefore.Add(1)
				r.now = r.now.Add(dispatchInterval)
			}
		}
		got := r.task(task.ID)
		w := r.todos.written()
		if got.State != StateFailed || got.AttemptNo != 3 || len(w) != 1 || len(r.hub.created) != 3 {
			t.Fatalf("final failure: state %s attempt %d decisions %d sessions %d", got.State, got.AttemptNo, len(w), len(r.hub.created))
		}
		addKey("wb.fail", "write-back: final failure after 3 attempts (visible text)", wbDecisionText(w[0]), "1 decision")
		body, _ := json.Marshal(w[0])
		add("write-back: final failure (POST body)", string(body), "")
		var all, refs []string
		for _, d := range w {
			all, refs = append(all, wbDecisionText(d)), append(refs, d.Ref)
		}
		add("write-back: total over 3-attempt task", strings.Join(all, ""), fmt.Sprintf("%d decision(s) written; refs: %v", len(w), refs))
		// A repeated sync writes nothing more.
		r.tick()
		r.tick()
		if n := len(r.todos.written()); n != 1 {
			t.Fatalf("re-sync wrote %d decisions", n)
		}
		rep := got.Reports[len(got.Reports)-1]
		ev, _ := json.Marshal(rep)
		add("evidence last_report: final failure", string(ev), "")
		allRep, _ := json.Marshal(got.Reports)
		add("all 3 reports (task.reports JSON)", string(allRep), "")
		t.Logf("SAMPLE final failure decision:\n%s\nref=%s", wbDecisionText(w[0]), w[0].Ref)

		for _, req := range r.hub.created {
			prompts = append(prompts, req.Prompt)
		}
	}
	userPrompt := strings.TrimSpace(prompt)
	for i, p := range prompts {
		if !strings.HasPrefix(p, userPrompt) {
			t.Fatalf("prompt %d does not start with the user's prompt", i+1)
		}
		over := strings.TrimPrefix(p, userPrompt)
		key, name := "prompt.first", "worker prompt: first attempt overhead"
		if i > 0 {
			key, name = fmt.Sprintf("prompt.retry%d", i+1), fmt.Sprintf("worker prompt: retry (attempt %d) overhead", i+1)
		}
		addKey(key, name, over, fmt.Sprintf("full prompt %d B / ~%d tok", len(p), estTokens(p)))
		if i > 0 {
			rules, retry, _ := strings.Cut(over, "\n\n---\nThis is attempt")
			_ = rules
			add(fmt.Sprintf("  of which retry block (attempt %d)", i+1), "\n\n---\nThis is attempt"+retry, "")
		} else {
			add("  of which fixed rules block", over, "repeated verbatim on every attempt")
		}
		t.Logf("SAMPLE worker prompt overhead, attempt %d:\n%s", i+1, over)
	}
	if !strings.Contains(prompts[1], "This is attempt 2") || !strings.Contains(prompts[2], "This is attempt 3") {
		t.Fatal("the retries' prompts do not say which attempt they are")
	}

	// 4. Cancel while running.
	{
		r := rigFor()
		task := r.enqueue(EnqueueInput{Title: title, Prompt: prompt, PermissionProfile: PermissionDefault, AcceptanceCommand: "sh ci.sh 0"})
		r.tick()
		if _, err := r.d.cancel(context.Background(), task.ID); err != nil {
			t.Fatal(err)
		}
		w := r.todos.written()
		if len(w) != 1 {
			t.Fatalf("cancel decisions: %d", len(w))
		}
		addKey("wb.cancel", "write-back: cancel (visible text)", wbDecisionText(w[0]), "1 decision")
		t.Logf("SAMPLE cancel decision:\n%s\nref=%s", wbDecisionText(w[0]), w[0].Ref)
	}

	var b strings.Builder
	b.WriteString("\n| key | scenario | bytes | est_tokens | note |\n|---|---|---:|---:|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %s |\n", r.key, r.name, r.bytes, r.toks, r.note)
	}
	t.Log(b.String())
	measured := map[string]string{}
	for _, r := range rows {
		if r.key != "" {
			if _, dup := measured[r.key]; dup {
				t.Errorf("%s is measured twice", r.key)
			}
			measured[r.key] = r.text
		}
	}
	// testTempRoot also covers the rigs' worktrees, which are in a t.TempDir of their own, not under r.root.
	checkBudgets(t, workerBudgetKeys, measured, append(roots, testTempRoot(t))...)
}
