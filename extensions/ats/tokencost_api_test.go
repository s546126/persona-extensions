package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// What an assistant pays, in bytes and estimated tokens, to read and queue ATS tasks through the skill and the
// HTTP API. The fixture goes through the real queue and handler, and every scenario with a key is held to its
// budget in tokenBudgets (tokencost_budget_test.go).
//
//	go test -run TestTokenCost -v .
//
// ATS_TOKCOST_DUMP=<dir> also writes every measured answer to <dir>/<scenario>.json.

// estTokens is the shared estimate, not a tokenizer: ascii bytes / 4 plus 1.5 per non-ascii rune, rounded up.
func estTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return int(math.Ceil(float64(ascii)/4 + float64(other)*1.5))
}

// tokPrompt is a deterministic prompt of about 1.5 KB, Chinese and English mixed, the way users brief an agent.
func tokPrompt(n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "目标：修复任务 #%d 里登录页在 session 过期后出现的空指针（nil pointer dereference），并补上回归测试。\n\n", n)
	b.WriteString("背景：用户反馈在 Safari 和 Chrome 上，token 过期后刷新 /login 会直接 500。日志里能看到 `internal/auth/session.go:142` panic。\n")
	b.WriteString("范围：只改 internal/auth 和 web/login 两个目录；不要动数据库 schema，不要升级依赖，也不要改 CI 配置。\n")
	b.WriteString("Steps:\n1. Reproduce with `go test ./internal/auth -run TestExpiredSession` (write the test first if it does not exist).\n")
	b.WriteString("2. Fix the nil check in Session.Refresh and make the handler return 401 with a JSON error instead of panicking.\n")
	b.WriteString("3. 前端在收到 401 时跳转到登录页并保留 return_to 参数；加一个 Playwright 用例覆盖这条路径。\n")
	b.WriteString("4. Run the whole suite: `make check` must exit 0. Keep the diff small and explain every change in the commit message.\n\n")
	b.WriteString("完成标准：make check 通过；新测试在修复前失败、修复后通过；没有无关改动。做完在最后一条回复里写 Summary: 加上改了什么、怎么验证的。\n")
	b.WriteString("注意：这个仓库的测试依赖本地 Postgres（docker compose up -d db），如果起不来就说明原因，不要跳过测试。\n")
	for b.Len() < 1500 {
		b.WriteString("补充：保持现有的错误文案（中文），日志用英文 structured fields，例如 `user_id`, `session_id`, `reason`。\n")
	}
	return b.String()
}

// tokTail is the end of an acceptance command's output, about 2 KB.
func tokTail(failing bool) string {
	var b strings.Builder
	pkgs := []string{"internal/auth", "internal/auth/oauth", "internal/store", "internal/store/pg", "web/login", "web/api", "cmd/server", "internal/mail"}
	for i := 0; b.Len() < 1700; i++ {
		p := pkgs[i%len(pkgs)]
		fmt.Fprintf(&b, "ok  \tgithub.com/example/app/%s\t%d.%03ds\n", p, 1+i%3, (i*137)%1000)
	}
	if failing {
		b.WriteString("--- FAIL: TestExpiredSessionRedirect (0.04s)\n    login_test.go:88: expected status 401, got 500\n    login_test.go:89: body: {\"error\":\"internal\"}\nFAIL\nFAIL\tgithub.com/example/app/web/login\t0.412s\nFAIL\nmake: *** [Makefile:23: check] Error 1\n")
	} else {
		b.WriteString("PASS\nok  \tgithub.com/example/app/web/login\t0.398s\nlint: 0 issues.\nall checks passed\n")
	}
	return b.String()
}

const tokSummary = "改了 internal/auth/session.go：Refresh 在 session 已过期时返回 ErrExpired 而不是对 nil 解引用；" +
	"login handler 把 ErrExpired 映射成 401 和 JSON 错误 {\"error\":\"session_expired\"}。前端在 401 时跳转 /login?return_to=…。\n" +
	"Tests: added TestExpiredSession (unit) and login.spec.ts \"expired session redirects\" (Playwright); both failed before the fix and pass now.\n" +
	"验证：make check 全部通过（8 个包，lint 0 issues）。没有改 schema、依赖和 CI。"

type tokRow struct {
	key   string // its budget in tokenBudgets; "" for a row that is only logged
	name  string
	calls int
	text  string
	note  string
}

func tokRaw(t *testing.T, a *atsApp, method, path, body string) string {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Persona-Role", "extension")
	rec := httptest.NewRecorder()
	a.handler().ServeHTTP(rec, req)
	return rec.Body.String()
}

func tokSHA(n int) string {
	return fmt.Sprintf("%08x", 0x9e3779b1*uint32(n+1)) + strings.Repeat(fmt.Sprintf("%x", n%16), 32)
}

func tokSession(n int) string {
	return fmt.Sprintf("7c9e6679-7425-40de-944b-%012x", 0xe07fc1f90ae7+n)
}

// tokFixture builds the queue the scenarios read: the to-do todo_7f3a9c with three tasks (one finished on its
// third attempt, one running, one queued) and 22 tasks of other to-dos in every state.
func tokFixture(t *testing.T) (a *atsApp, mainID, postBody string) {
	t.Helper()
	a = newTestApp(t)
	q := a.queue
	clock := testStart
	a.nowFunc = func() time.Time { return clock }
	tick := func(d time.Duration) time.Time { clock = clock.Add(d); return clock }
	exit := func(n int) *int { return &n }
	const todo = "todo_7f3a9c"

	post := func(n int, title, perm, acceptance string) string {
		tick(time.Minute)
		body, _ := json.Marshal(map[string]any{
			"todo_id": todo, "title": title, "prompt": tokPrompt(n), "repo": "~/code/app", "acceptance": acceptance,
			"permission_profile": perm, "priority": "p1", "difficulty": "medium",
		})
		var res struct{ Task taskView }
		if err := json.Unmarshal([]byte(tokRaw(t, a, "POST", "/tasks", string(body))), &res); err != nil || res.Task.ID == "" {
			t.Fatalf("post %d: %v", n, err)
		}
		return res.Task.ID
	}
	run := func(id string, n int) (fence int) {
		_, att, err := q.LeaseTask(id, "dispatcher", testMachine, tick(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.UpdateExec(id, att.FenceToken, clock, func(e *Execution) {
			e.Phase, e.MachineID, e.SessionID = PhaseWorking, testMachine, tokSession(n*10+att.AttemptNo)
			e.Worktree = fmt.Sprintf("/home/me/.ats/worktrees/%s-%d", id, att.AttemptNo)
			e.Branch, e.BaseSHA = fmt.Sprintf("ats/%s-%d", id, att.AttemptNo), tokSHA(0)
		}); err != nil {
			t.Fatal(err)
		}
		return att.FenceToken
	}
	finish := func(id string, fence, n int, pass bool) {
		in := CompleteAttemptInput{TaskID: id, FenceToken: fence, WorkerID: "dispatcher", Summary: tokSummary, Now: tick(3 * time.Minute), // inside the 5-minute lease
			Artifacts: []Artifact{{Kind: "commit", Path: tokSHA(n*10 + fence)}, {Kind: "branch", Path: fmt.Sprintf("ats/%s-%d", id, fence)},
				{Kind: "session", Path: testMachine + "/" + tokSession(n*10+fence)}}}
		if pass {
			in.Outcome, in.Verdict, in.ExitCode, in.Message = StateSucceeded, VerdictPass, exit(0), "验收命令退出码 0\n"+tokTail(false)
		} else {
			in.Outcome, in.Verdict, in.ErrorClass, in.ExitCode, in.Message = StateFailed, VerdictFail, "task_failed", exit(1), "验收命令退出码 1\n"+tokTail(true)
		}
		if _, err := q.CompleteAttempt(in); err != nil {
			t.Fatal(err)
		}
	}

	// Other to-dos first, so the to-do's own tasks are the newest.
	states := []string{StateSucceeded, StateFailed, StateQueued, StateRunning, StateNeedsInput, StateSucceeded, StateQueued, StateFailed, StateSucceeded, StateRunning, StateSucceeded}
	for i := range 22 {
		tick(time.Minute)
		id := mustEnqueue(t, q, EnqueueInput{Title: fmt.Sprintf("整理第 %d 批日志告警并给出 root cause", i+1), Prompt: tokPrompt(100 + i),
			Repo: "~/code/app", AcceptanceCommand: "make check", Priority: []string{"p1", "p2", "p2", "p3"}[i%4],
			Labels: map[string]string{PersonaTodoLabel: fmt.Sprintf("todo_%06x", 0xa1b2c3+i)}, Now: clock}).ID
		switch states[i%len(states)] {
		case StateSucceeded:
			finish(id, run(id, 100+i), 100+i, true)
			_ = q.markDecisionWritten(id, BuildDecisionRef(id, 1, StateSucceeded, VerdictPass))
		case StateFailed: // retried once, then out of retries
			for range 3 {
				finish(id, run(id, 100+i), 100+i, false)
			}
			_ = q.markDecisionWritten(id, BuildDecisionRef(id, 3, StateFailed, VerdictFail))
		case StateRunning:
			run(id, 100+i)
		case StateNeedsInput:
			f := run(id, 100+i)
			if _, err := q.UpdateExec(id, f, clock, func(e *Execution) {
				e.Waiting = "Agent 在问：要不要同时修改 staging 环境的配置？（approve: edit config/staging.yaml）"
			}); err != nil {
				t.Fatal(err)
			}
		case StateQueued:
			if i%2 == 0 { // waiting for a retry
				finish(id, run(id, 100+i), 100+i, false)
			}
		}
	}

	mainID = post(1, "修复登录页 session 过期后的空指针", PermissionDefault, "make check")
	for _, pass := range []bool{false, false, true} {
		finish(mainID, run(mainID, 1), 1, pass)
	}
	_ = q.markDecisionWritten(mainID, BuildDecisionRef(mainID, 3, StateSucceeded, VerdictPass))
	run(post(2, "给 /login 加 Playwright 回归用例", PermissionDefault, "npx playwright test login"), 2)
	post(4, "复查 auth 模块其他 nil 解引用", PermissionReadonly, "go vet ./internal/auth/...")

	tick(time.Minute)
	postBody = fmt.Sprintf(`{"todo_id":%q,"title":"调查 /login 偶发 502","prompt":%q,"repo":"~/code/app","acceptance":"make check","priority":"p1","difficulty":"medium"}`, todo, tokPrompt(3))
	return a, mainID, postBody
}

// tokFieldShare is how many bytes each top-level key of the objects takes (`"key":value,`), summed.
func tokFieldShare(objs []json.RawMessage) map[string]int {
	out := map[string]int{}
	for _, raw := range objs {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		for k, v := range m {
			out[k] += len(k) + 3 + len(v) + 1
		}
	}
	return out
}

func tokShareTable(title string, share map[string]int) string {
	keys := make([]string, 0, len(share))
	total := 0
	for k, v := range share {
		keys = append(keys, k)
		total += v
	}
	sort.Slice(keys, func(i, j int) bool {
		if share[keys[i]] != share[keys[j]] {
			return share[keys[i]] > share[keys[j]]
		}
		return keys[i] < keys[j]
	})
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s (total %d bytes)\n\n| field | bytes | share |\n| --- | ---: | ---: |\n", title, total)
	for _, k := range keys {
		fmt.Fprintf(&b, "| %s | %d | %.1f%% |\n", k, share[k], 100*float64(share[k])/float64(max(total, 1)))
	}
	return b.String()
}

func TestTokenCostAPI(t *testing.T) {
	a, mainID, postBody := tokFixture(t)

	skill, err := os.ReadFile("skills/ats-todo/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	desc, body := skillParts(t)
	gateRaw, err := os.ReadFile("assistant/gate.json")
	if err != nil {
		t.Fatal(err)
	}
	var gate struct {
		Tool struct{ Name, Description string } `json:"tool"`
	}
	if err := json.Unmarshal(gateRaw, &gate); err != nil {
		t.Fatal(err)
	}

	listTodo := tokRaw(t, a, "GET", "/tasks?todo_id=todo_7f3a9c", "")
	listAll := tokRaw(t, a, "GET", "/tasks", "")
	listFull := tokRaw(t, a, "GET", "/tasks?view=full", "")
	var page struct{ Next string }
	_ = json.Unmarshal([]byte(listAll), &page)
	listPage2 := tokRaw(t, a, "GET", "/tasks?after="+page.Next, "")
	getOne := tokRaw(t, a, "GET", "/tasks/"+mainID, "")
	getFull := tokRaw(t, a, "GET", "/tasks/"+mainID+"?view=full", "")
	evidence := tokRaw(t, a, "GET", "/tasks/"+mainID+"/evidence", "")
	evidenceOne := tokRaw(t, a, "GET", "/tasks/"+mainID+"/evidence?attempt=2", "")
	posted := tokRaw(t, a, "POST", "/tasks", postBody)
	errRepo := tokRaw(t, a, "POST", "/tasks", strings.Replace(postBody, "~/code/app", "/srv/other/app", 1))
	errMissing := tokRaw(t, a, "GET", "/tasks/task_999", "")

	var counts struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	_ = json.Unmarshal([]byte(listAll), &counts)
	var todoCounts struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	_ = json.Unmarshal([]byte(listTodo), &todoCounts)

	rows := []tokRow{
		{"skill.desc", "skill frontmatter description (always in context)", 0, desc, ""},
		{"gate.desc", "gate tool description (ats_api)", 0, gate.Tool.Description, ""},
		{"skill.body", "SKILL.md body (without frontmatter)", 1, body, "read when the skill is used"},
		{"", "SKILL.md full file", 1, string(skill), "frontmatter included"},
		{"list.todo", "GET /tasks?todo_id= (to-do tasks)", 1, listTodo, fmt.Sprintf("%d tasks", len(todoCounts.Tasks))},
		{"list.all", "GET /tasks default", 1, listAll, fmt.Sprintf("%d of 25 tasks", len(counts.Tasks))},
		{"list.full", "GET /tasks?view=full", 1, listFull, "every field of 20 tasks"},
		{"list.page2", "GET /tasks?after= (second page)", 1, listPage2, "the last 5 of 25 tasks"},
		{"task.get", "GET /tasks/{id} (3 attempts)", 1, getOne, ""},
		{"task.full", "GET /tasks/{id}?view=full (3 attempts)", 1, getFull, "the whole prompt, every attempt field"},
		{"evidence", "GET /tasks/{id}/evidence", 1, evidence, ""},
		{"evidence.attempt", "GET /tasks/{id}/evidence?attempt=2", 1, evidenceOne, "one attempt's whole report"},
		{"", "POST /tasks request body", 0, postBody, "what the assistant writes"},
		{"post.resp", "POST /tasks response", 1, posted, ""},
		{"err.repo", "error: repo_not_allowed", 1, errRepo, ""},
		{"err.missing", "error: not_found", 1, errMissing, ""},
		{"composite", "composite: how far along / really done", 4, body + listTodo + getOne + evidence, "skill body + list + get + evidence"},
		{"flow.list_evidence", "flow: list then evidence", 3, body + listTodo + evidence, "skill body + list + evidence"},
	}

	var b strings.Builder
	b.WriteString("\n| key | scenario | calls | bytes | est_tokens | notes |\n| --- | --- | ---: | ---: | ---: | --- |\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %s |\n", r.key, r.name, r.calls, len(r.text), estTokens(r.text), r.note)
	}

	// Where the bytes go.
	b.WriteString(tokShareTable("GET /tasks default: per task field bytes", tokFieldShare(counts.Tasks)))
	var one struct {
		Task     json.RawMessage   `json:"task"`
		Attempts []json.RawMessage `json:"attempts"`
	}
	_ = json.Unmarshal([]byte(getOne), &one)
	b.WriteString(tokShareTable("GET /tasks/{id}: task field bytes", tokFieldShare([]json.RawMessage{one.Task})))
	b.WriteString(tokShareTable("GET /tasks/{id}: attempts field bytes", tokFieldShare(one.Attempts)))
	var ev struct {
		LastReport json.RawMessage `json:"last_report"`
	}
	_ = json.Unmarshal([]byte(evidence), &ev)
	b.WriteString(tokShareTable("GET /tasks/{id}/evidence: top-level bytes", tokFieldShare([]json.RawMessage{json.RawMessage(evidence)})))
	b.WriteString(tokShareTable("GET /tasks/{id}/evidence: last_report bytes", tokFieldShare([]json.RawMessage{ev.LastReport})))
	t.Log(b.String())

	if dir := os.Getenv("ATS_TOKCOST_DUMP"); dir != "" {
		for name, body := range map[string]string{"list_todo": listTodo, "list_all": listAll, "get": getOne, "evidence": evidence,
			"post": posted, "post_body": postBody, "err_repo": errRepo, "err_missing": errMissing} {
			if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The fixture is what it claims to be.
	if !strings.HasSuffix(page.Next, "-task_6") || strings.Contains(listPage2, `"next"`) || strings.Count(listPage2, `"id":`) != 5 {
		t.Fatalf("fixture: pages\n%s\n%s", listAll, listPage2)
	}
	if len(todoCounts.Tasks) != 3 || len(counts.Tasks) != 20 || len(one.Attempts) != 3 || !strings.Contains(evidence, `"verified":true`) ||
		!strings.Contains(posted, `"created":true`) || !strings.Contains(errRepo, "repo_not_allowed") || !strings.Contains(errMissing, "not_found") {
		t.Fatalf("fixture: %d to-do tasks, %d listed, %d attempts\n%s\n%s\n%s", len(todoCounts.Tasks), len(counts.Tasks), len(one.Attempts), evidence, posted, errRepo)
	}
	measured := map[string]string{}
	for _, r := range rows {
		if r.key != "" {
			measured[r.key] = r.text
		}
	}
	checkBudgets(t, apiBudgetKeys, measured, testTempRoot(t))
}
