package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

// pageRig is a queue with tasks put into the states the pages show, without a dispatcher: each helper moves a
// task the way the dispatcher's calls would.
type pageRig struct {
	t *testing.T
	q *Queue
	a *atsApp
}

func newPageRig(t *testing.T) *pageRig {
	t.Helper()
	a := newTestApp(t)
	return &pageRig{t: t, q: a.queue, a: a}
}

func (p *pageRig) enqueue(in EnqueueInput) *Task {
	p.t.Helper()
	if in.Labels == nil {
		in.Labels = map[string]string{PersonaTodoLabel: "todo_1"}
	}
	return mustEnqueue(p.t, p.q, in)
}

// lease starts the task's next attempt in the given phase; waiting makes it wait for the user.
func (p *pageRig) lease(id, phase, waiting string) *Attempt {
	p.t.Helper()
	_, att, err := p.q.LeaseTask(id, "dispatcher", testMachine, testStart)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.q.UpdateExec(id, att.FenceToken, testStart, func(e *Execution) {
		e.MachineID, e.Phase, e.Waiting = testMachine, phase, waiting
		e.Branch, e.Worktree, e.SessionID = "ats/"+id+"-1", "/data/worktrees/"+id+"-1", "session-1"
	}); err != nil {
		p.t.Fatal(err)
	}
	return att
}

func (p *pageRig) complete(id string, att *Attempt, done CompleteAttemptInput) *Task {
	p.t.Helper()
	done.TaskID, done.FenceToken, done.WorkerID, done.Now = id, att.FenceToken, "dispatcher", testStart.Add(time.Minute)
	task, err := p.q.CompleteAttempt(done)
	if err != nil {
		p.t.Fatal(err)
	}
	return task
}

// checked marshals a view after holding it to the SDK's rules for a page.
func checked(t *testing.T, view *wire.View) string {
	t.Helper()
	if err := wire.CheckView(view); err != nil {
		t.Fatalf("wire.CheckView failed: %v", err)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if err := exttest.CheckView(raw, false); err != nil {
		t.Fatalf("not a valid view: %v\n%s", err, raw)
	}
	return string(raw)
}

func sectionTitles(v *wire.View) []string {
	var out []string
	for _, s := range v.Sections {
		out = append(out, s.Title)
	}
	return out
}

func findRow(t *testing.T, v *wire.View, key string) wire.Component {
	t.Helper()
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Key == key {
				return c
			}
		}
	}
	t.Fatalf("the view has no component %q", key)
	return wire.Component{}
}

func sectionOf(v *wire.View, key string) string {
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Key == key {
				return s.Title
			}
		}
	}
	return ""
}

func TestQueuePagePutsWhatNeedsTheUserFirstAndATapNeverChangesATask(t *testing.T) {
	p := newPageRig(t)
	zero := 0
	none := 0
	one := 1

	waiting := p.enqueue(EnqueueInput{Title: "Waiting task", Repo: "/srv/code/app", PermissionProfile: PermissionDefault, Priority: PriorityP0})
	p.lease(waiting.ID, PhaseWorking, "Agent 等你批准运行：go test ./...")
	queued := p.enqueue(EnqueueInput{Title: "Queued task", Repo: "/srv/code/site"})
	failed := p.enqueue(EnqueueInput{Title: "Failed task", Repo: "/srv/code/app", MaxRetries: &none})
	p.q.SetMachines([]MachineSlot{{Name: testMachine, Title: "Studio", Online: true, MaxSlots: 4}})
	p.complete(failed.ID, p.lease(failed.ID, PhaseWorking, ""), CompleteAttemptInput{Outcome: StateFailed, ErrorClass: "config_error",
		Message: "Agent 这一轮出错：There's an issue with the selected model (example-model). It may not exist or you may not have access to it."})
	retrying := p.enqueue(EnqueueInput{Title: "Retrying task", Repo: "/srv/code/app", AcceptanceCommand: "go test ./...", MaxRetries: &one})
	p.complete(retrying.ID, p.lease(retrying.ID, PhaseAccepting, ""), CompleteAttemptInput{Outcome: StateFailed, Verdict: VerdictFail, ErrorClass: "task_failed", ExitCode: &one, Message: "验收命令退出码 1\nFAIL"})
	passed := p.enqueue(EnqueueInput{Title: "Passed task", Repo: "/srv/code/app", AcceptanceCommand: "go test ./..."})
	p.complete(passed.ID, p.lease(passed.ID, PhaseAccepting, ""), CompleteAttemptInput{Outcome: StateSucceeded, Verdict: VerdictPass, ExitCode: &zero, Message: "验收命令退出码 0\nok"})

	view := buildQueueView(p.q, p.a.config(), testStart.Add(2*time.Minute), "UTC")
	page := checked(t, view)

	// Tasks first, the one that waits for the user before all; the numbers and the computers below.
	if got, want := strings.Join(sectionTitles(view), "|"), "|现在怎么样|等你处理|正在做和排着的|最近做完的|数一数|干活的电脑"; got != want {
		t.Fatalf("sections: %s, want %s", got, want)
	}
	if got := sectionOf(view, "active-"+waiting.ID); got != "等你处理" {
		t.Errorf("the task that waits for the user is under %q", got)
	}
	if got := sectionOf(view, "active-"+queued.ID); got != "正在做和排着的" {
		t.Errorf("the queued task is under %q", got)
	}
	if got := sectionOf(view, "active-"+retrying.ID); got != "正在做和排着的" {
		t.Errorf("the task that waits for its retry is under %q", got)
	}
	if row := findRow(t, view, "active-"+waiting.ID); row.Subtitle != "它想运行一条命令，等你同意：go test ./..." {
		t.Errorf("the row of the waiting task does not say what it waits for: %q", row.Subtitle)
	}

	// A tap opens the task's page; canceling and retrying are in the row's menu, each asking first.
	for _, task := range []*Task{waiting, queued, retrying, failed, passed} {
		key := "active-" + task.ID
		if task.ID == failed.ID || task.ID == passed.ID {
			key = "term-" + task.ID
		}
		row := findRow(t, view, key)
		if row.Action == nil || row.Action.ID != "open:"+task.ID || row.Action.Confirm != "" || row.Action.Push == nil {
			t.Fatalf("a tap on %q does not just open its page: %+v", task.Title, row.Action)
		}
		var st taskPage
		if err := json.Unmarshal([]byte(row.Action.Push.State), &st); err != nil || st.Task != task.ID {
			t.Errorf("the pushed page of %q is for %q (%v)", task.Title, st.Task, err)
		}
		for _, ra := range row.Actions {
			if ra.Action.Confirm == "" {
				t.Errorf("%q: the row action %q does not ask first", task.Title, ra.Title)
			}
		}
	}
	if acts := findRow(t, view, "active-"+waiting.ID).Actions; len(acts) != 1 || acts[0].Action.ID != "cancel:"+waiting.ID || acts[0].Role != wire.ButtonRoleDestructive {
		t.Errorf("the running row's menu: %+v", acts)
	}
	if acts := findRow(t, view, "term-"+failed.ID).Actions; len(acts) != 1 || acts[0].Action.ID != "retry:"+failed.ID {
		t.Errorf("the failed row's menu: %+v", acts)
	}
	if acts := findRow(t, view, "term-"+passed.ID).Actions; len(acts) != 0 {
		t.Errorf("a passed task offers %+v", acts)
	}

	// Rows speak plainly: no to-do id, no tier arrow, no raw error class, and the reason of a failure whole.
	for _, bad := range []string{"todo_1", "→", "config_error", "task_failed", "点按", "P0", "P2", "Agent", "调度", "准入", "出队", "fast", "standard", "medium", "100%"} {
		if strings.Contains(page, bad) {
			t.Errorf("the page has %q", bad)
		}
	}
	if sub := findRow(t, view, "term-"+failed.ID).Subtitle; !strings.Contains(sub, "选的模型用不了，试了 1 次") || !strings.Contains(sub, "app") {
		t.Errorf("the failed row: %q", sub)
	}
	if sub := findRow(t, view, "active-"+retrying.ID).Subtitle; !strings.Contains(sub, "第 1 次没做成（检查没通过），") || !strings.Contains(sub, "自动再试") {
		t.Errorf("the row of the task that waits for its retry: %q", sub)
	}
	if sub := findRow(t, view, "active-"+queued.ID).Subtitle; sub != "普通 · 只看不改 · 项目 site" {
		t.Errorf("the queued row: %q", sub)
	}

	// Without a quota account there is no made-up quota, and nothing is held back; the status line says so.
	if strings.Contains(page, "额度") {
		t.Error("the page shows a quota with no quota account")
	}
	if got := findRow(t, view, "status").Text; !strings.HasPrefix(got, "正常接活") {
		t.Errorf("the status line: %q", got)
	}
	// Low quota: the line says only urgent work is done, and the table of difficulties is back.
	reset := testStart.Add(3 * time.Hour)
	if err := p.q.SetQuotaStatus(QuotaStatus{Provider: "test", RemainingPct: 20, ResetAt: &reset, CheckedAt: testStart}); err != nil {
		t.Fatal(err)
	}
	held := buildQueueView(p.q, p.a.config(), testStart.Add(2*time.Minute), "UTC")
	raw := checked(t, held)
	if got := findRow(t, held, "status").Text; !strings.Contains(got, "额度快用完了（还剩 20%）") || !strings.Contains(got, "只做紧急和重要的事") || !strings.Contains(got, "13:00") {
		t.Errorf("the status line with low quota: %q", got)
	}
	if !strings.Contains(raw, "额度不够时，不同难度的事还做不做") || !strings.Contains(raw, "只做紧急和重要的") {
		t.Error("the table of difficulties is missing when quota holds them back")
	}
	// Used up: the line says when it comes back.
	if err := p.q.SetQuotaStatus(QuotaStatus{Provider: "test", RemainingPct: 0, Exhausted: true, ResetAt: &reset, CheckedAt: testStart}); err != nil {
		t.Fatal(err)
	}
	if got := findRow(t, buildQueueView(p.q, p.a.config(), testStart.Add(2*time.Minute), "UTC"), "status").Text; !strings.HasPrefix(got, "额度用完了，大约 13:00 恢复") {
		t.Errorf("the status line with no quota left: %q", got)
	}
	// No computer on: the line says so before anything about quota.
	if err := p.q.SetMachines([]MachineSlot{{Name: testMachine, Title: "Studio", Online: false, MaxSlots: 4}}); err != nil {
		t.Fatal(err)
	}
	off := buildQueueView(p.q, p.a.config(), testStart.Add(2*time.Minute), "UTC")
	if got := findRow(t, off, "status").Text; !strings.HasPrefix(got, "没有电脑在线") {
		t.Errorf("the status line with the computer off: %q", got)
	}
	if row := findRow(t, off, "machine-"+testMachine); row.Badge == nil || row.Badge.Text != "离线" || !strings.Contains(row.Subtitle, "打开它上面的 Persona") {
		t.Errorf("the offline computer: %+v", row)
	}
}

func TestQueuePageBeforeSetupShowsOnlyWhatIsLeftToDo(t *testing.T) {
	a := newATSApp(newTestQueue(t, DefaultQueueConfig()))
	view := buildQueueView(a.queue, a.config(), testStart, "")
	raw := checked(t, view)
	if got, want := strings.Join(sectionTitles(view), "|"), "|第 1 步：选一台干活的电脑|第 2 步：告诉它能动哪个文件夹|第 3 步：保存"; got != want {
		t.Fatalf("sections: %s, want %s", got, want)
	}
	if row := findRow(t, view, "setup-machines"); row.Type != wire.ComponentText || !strings.Contains(row.Text, "刷新") {
		t.Errorf("without a list of computers: %+v", row)
	}
	if row := findRow(t, view, "setup-repo"); row.Type != wire.ComponentField || row.Name != "repo" {
		t.Errorf("the folder field: %+v", row)
	}
	if save := findRow(t, view, "setup-save"); save.Action == nil || save.Action.ID != actSetupSave {
		t.Errorf("the save button: %+v", save)
	}
	for _, bad := range []string{"100%", "数一数", "排着", "能动的文件夹", "现在怎么样"} {
		if strings.Contains(raw, bad) {
			t.Errorf("before setup the page shows %q", bad)
		}
	}

	// One step done: it is marked done, the other is not; tasks already handed over are mentioned.
	cfg := a.config()
	cfg.Machines = []string{testMachine}
	mustEnqueue(t, a.queue, EnqueueInput{Title: "Early task", Repo: "/srv/code/app", Labels: map[string]string{PersonaTodoLabel: "todo_1"}})
	view = buildQueueView(a.queue, cfg, testStart, "")
	checked(t, view)
	if row := findRow(t, view, "setup-machines"); row.Badge.Text != "做好了" {
		t.Errorf("the chosen computer is not marked done: %+v", row.Badge)
	}
	if row := findRow(t, view, "setup-repo"); row.Type != wire.ComponentField {
		t.Errorf("the folders are marked done: %+v", row)
	}
	if next := findRow(t, view, "setup-live").Text; !strings.Contains(next, "之前交给它的事还有 1 件没结束（排着 1 件）") {
		t.Errorf("the task already handed over is not mentioned: %q", next)
	}

	// Set up: the checklist is gone, the computers list the allowed folders.
	cfg.Repos = []string{"~/code", "~/src"}
	view = buildQueueView(a.queue, cfg, testStart, "")
	checked(t, view)
	if got, want := strings.Join(sectionTitles(view), "|"), "|现在怎么样|正在做和排着的|最近做完的|数一数|干活的电脑"; got != want {
		t.Fatalf("sections when set up: %s, want %s", got, want)
	}
	if got := findRow(t, view, "repos").Text; got != "能动的文件夹：~/code、~/src" {
		t.Errorf("the allowed folders: %q", got)
	}
}

// Clearing the computers or the folders after tasks were handed over does not stop the dispatcher: the page
// keeps every task that has not ended in sight, with its cancel action, under the checklist.
func TestQueuePageBeforeSetupKeepsLiveWorkInSight(t *testing.T) {
	p := newPageRig(t)
	none := 0
	running := p.enqueue(EnqueueInput{Title: "Running task", Repo: "/srv/code/app"})
	p.lease(running.ID, PhaseWorking, "")
	waiting := p.enqueue(EnqueueInput{Title: "Waiting task", Repo: "/srv/code/app"})
	p.lease(waiting.ID, PhaseWorking, "Agent 在问你：用哪个分支？")
	queued := p.enqueue(EnqueueInput{Title: "Queued task", Repo: "/srv/code/app"})
	finished := p.enqueue(EnqueueInput{Title: "Finished task", Repo: "/srv/code/app", MaxRetries: &none})
	p.complete(finished.ID, p.lease(finished.ID, PhaseWorking, ""), CompleteAttemptInput{Outcome: StateFailed, ErrorClass: "infra_error"})
	canceled := p.enqueue(EnqueueInput{Title: "Canceled task", Repo: "/srv/code/app"})
	if _, err := p.q.CancelTask(canceled.ID, testStart); err != nil {
		t.Fatal(err)
	}

	for name, cfg := range map[string]ExtConfig{
		"no computers": {Repos: []string{"/srv/code"}},
		"no folders":   {Machines: []string{testMachine}},
		"nothing":      {},
	} {
		view := buildQueueView(p.q, cfg, testStart.Add(2*time.Minute), "UTC")
		raw := checked(t, view)
		if got, want := strings.Join(sectionTitles(view), "|"), "|第 1 步：选一台干活的电脑|第 2 步：告诉它能动哪个文件夹|第 3 步：保存|等你处理|正在做和排着的"; got != want {
			t.Fatalf("%s: sections %s, want %s", name, got, want)
		}
		if got := sectionOf(view, "active-"+waiting.ID); got != "等你处理" {
			t.Errorf("%s: the waiting task is under %q", name, got)
		}
		for _, task := range []*Task{running, waiting, queued} {
			row := findRow(t, view, "active-"+task.ID)
			if len(row.Actions) != 1 || row.Actions[0].Action.ID != "cancel:"+task.ID || row.Actions[0].Action.Confirm == "" {
				t.Errorf("%s: %q cannot be canceled from the page: %+v", name, task.Title, row.Actions)
			}
		}
		// Ended tasks are neither listed nor counted; the count says what is being done.
		for _, gone := range []string{finished.ID, canceled.ID} {
			if strings.Contains(raw, "term-"+gone) || strings.Contains(raw, "active-"+gone) {
				t.Errorf("%s: an ended task is listed before setup", name)
			}
		}
		if next := findRow(t, view, "setup-live").Text; !strings.Contains(next, "之前交给它的事还有 3 件没结束（正在做 1 件、等你处理 1 件、排着 1 件），就在下面，不想做了可以取消。") {
			t.Errorf("%s: the count of live work: %q", name, next)
		}
		for _, bad := range []string{"100%", "数一数", "能动的文件夹"} {
			if strings.Contains(raw, bad) {
				t.Errorf("%s: before setup the page shows %q", name, bad)
			}
		}
	}

	// With only ended tasks, nothing is said about work left.
	q := newTestQueue(t, DefaultQueueConfig())
	old := mustEnqueue(t, q, EnqueueInput{Title: "Old task", Repo: "/srv/code/app", Labels: map[string]string{PersonaTodoLabel: "todo_1"}})
	if _, err := q.CancelTask(old.ID, testStart); err != nil {
		t.Fatal(err)
	}
	view := buildQueueView(q, ExtConfig{}, testStart, "UTC")
	checked(t, view)
	if got := strings.Join(sectionTitles(view), "|"); got != "|第 1 步：选一台干活的电脑|第 2 步：告诉它能动哪个文件夹|第 3 步：保存" {
		t.Errorf("only ended tasks: sections %s", got)
	}
	if raw := checked(t, view); strings.Contains(raw, "没结束") {
		t.Errorf("only ended tasks: %s", raw)
	}
}

// A gateway that lists a source but no account gives the placeholder of 100% under the source's name: the page
// shows no quota for it. Measured quota is shown.
func TestQueuePageShowsNoQuotaWithoutAnAccount(t *testing.T) {
	a := newTestApp(t)
	page := func() string {
		t.Helper()
		return checked(t, buildQueueView(a.queue, a.config(), testStart, "UTC"))
	}
	for name, ga := range map[string]GenericGatewayAccounts{
		"a source, no accounts": {Sources: []GenericGatewaySource{{ID: "gw", Name: "Gateway", Type: "gateway"}}},
		"nothing":               {},
	} {
		if err := a.queue.SetQuotaStatus(ParseGatewayAccounts(ga, testStart)); err != nil {
			t.Fatal(err)
		}
		if raw := page(); strings.Contains(raw, "额度还剩") || strings.Contains(raw, "100%") {
			t.Errorf("%s: the page shows a quota: %s", name, raw)
		}
	}
	used, reset := 0.4, testStart.Add(time.Hour)
	if err := a.queue.SetQuotaStatus(ParseGatewayAccounts(GenericGatewayAccounts{
		Sources:  []GenericGatewaySource{{ID: "gw", Name: "Gateway"}},
		Accounts: []GenericGatewayAccount{{Source: "gw", AccountID: "a1", Status: "active", Windows: []GenericGatewayWindow{{UsedRatio: &used, ResetsAt: &reset}}}},
	}, testStart)); err != nil {
		t.Fatal(err)
	}
	if raw := page(); !strings.Contains(raw, "额度还剩") || !strings.Contains(raw, "60%") {
		t.Errorf("a measured account: the page shows no quota: %s", raw)
	}
}

// What the pages tell the user to change must be what the settings are called.
func TestNextStepsNameRealSettings(t *testing.T) {
	var m struct {
		Configuration struct {
			Properties map[string]struct {
				Title string `json:"title"`
			} `json:"properties"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	titles := map[string]string{}
	for k, p := range m.Configuration.Properties {
		titles[k] = p.Title
	}
	for _, c := range []struct{ class, verdict, setting, quoted string }{
		{"timeout", VerdictFail, "acceptance_timeout_minutes", "检查最多等多久"},
		{"timeout", "", "task_timeout_minutes", "一件事最多做多久"},
		{"config_error", "", "model_standard", "用哪个模型"},
	} {
		next := failureNextStep(c.class, c.verdict)
		if !strings.Contains(next, "「"+c.quoted) || !strings.Contains(titles[c.setting], c.quoted) || !strings.Contains(next, settingsPath) {
			t.Errorf("%s/%s: %q does not name the setting %q", c.class, c.verdict, next, titles[c.setting])
		}
	}
	// The agent program's own sign-in on that computer, not Persona's.
	if next := failureNextStep("auth_expired", ""); !strings.Contains(next, "那台电脑上干活用的程序") || !strings.Contains(next, "不是 Persona 的登录") {
		t.Errorf("auth_expired: %q", next)
	}
	// A check that took too long is not answered by "try a faster check" next to the button that reruns it.
	if next := failureNextStep("timeout", VerdictFail); !strings.Contains(next, "重新交给它做") {
		t.Errorf("check timeout: %q", next)
	}
}

func TestManifestNameMatchesWhatThePagesSay(t *testing.T) {
	var m struct {
		Name        string `json:"name"`
		Contributes struct {
			UI []struct{ ID, Title string } `json:"ui"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != extName {
		t.Errorf("the pages send users to 设置 › 扩展 › %s, but the extension is called %q", extName, m.Name)
	}
	if len(m.Contributes.UI) != 2 {
		t.Errorf("the ui contributions: %+v", m.Contributes.UI)
	}
	// The hub's settings page lists every UI contribution by its title: the home page carries the extension's
	// name, the Overview card the name and 「 · 概况」, so the two switches never read as a duplicate.
	want := map[string]string{queueUI: extName, cardUI: extName + " · 概况"}
	seen := map[string]string{}
	for _, u := range m.Contributes.UI {
		if u.Title != want[u.ID] {
			t.Errorf("the title of %s: %q, want %q", u.ID, u.Title, want[u.ID])
		}
		if other, ok := seen[u.Title]; ok {
			t.Errorf("%s and %s are both titled %q", other, u.ID, u.Title)
		}
		seen[u.Title] = u.ID
	}
	// Not every task has a check: neither the intro nor the description promises one.
	var d struct{ Description string }
	if err := json.Unmarshal(manifest, &d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Description, "交给它时说了怎么检查的，它做完还会自己检查一遍") {
		t.Errorf("the description does not say the check is only there when one was given: %q", d.Description)
	}
	if strings.Contains(homeIntro, "检查") || strings.Count(homeIntro, "\n") > 1 {
		t.Errorf("the intro promises a check or runs past two lines: %q", homeIntro)
	}
	// The state that waits for the user has one name everywhere.
	for _, src := range []string{"panel.go", "detail.go", "words.go", "persona-extension.json"} {
		if strings.Contains(string(mustRead(t, src)), "等你回话") {
			t.Errorf("%s calls the waiting state by another name", src)
		}
	}
}

func TestPageWordsHaveNoJargon(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{"", "它在等你回答问题或点同意"},
		{waitingInSession, "它在等你回答问题或点同意"},
		{"Agent 等你批准运行：go test ./...", "它想运行一条命令，等你同意：go test ./..."},
		{"Agent 等你批准使用 WebFetch", "它想上网查资料，等你同意"},
		{"Agent 等你批准 Edit：main.go", "它想改文件，等你同意：main.go"},
		{"Agent 在问你：用哪个分支？", "它在问你：用哪个分支？"},
		{"Agent 说它卡住了：没有权限", "它卡住了，需要你帮忙：没有权限"},
		{"Agent 提交了计划，等你批准", "它写好了打算怎么做，等你同意"},
	} {
		if got := waitingWords(c.raw); got != c.want {
			t.Errorf("waitingWords(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
	for p, want := range map[string]string{PriorityP0: "紧急", PriorityP1: "重要", PriorityP2: "普通", PriorityP3: "不急", "": "普通"} {
		if got := priorityWords(p); got != want {
			t.Errorf("priorityWords(%q) = %q, want %q", p, got, want)
		}
	}
	for _, class := range []string{"config_error", "auth_expired", "rate_limited", "infra_error", "timeout", "lost", "rejected", "task_failed", "", "something_new"} {
		for _, verdict := range []string{"", VerdictFail} {
			for _, s := range []string{pageFailureWords(class, verdict), failureNextStep(class, verdict)} {
				if s == "" || hasLatin(strings.NewReplacer("ATS", "", "Persona", "").Replace(s)) {
					t.Errorf("%s/%s: %q", class, verdict, s)
				}
			}
		}
	}

	// Errors of the buttons: known ones in Chinese with a next step, unknown ones in Chinese with the raw text after.
	for _, c := range []struct {
		err          error
		words, extra string
	}{
		{ErrTaskNotFound, "找不到这个任务了，可能已经被清掉。刷新一下看看。", ""},
		{fmt.Errorf("only failed or canceled tasks can be retried (current state: running)"), "这个任务还没结束，不能再试一次。等它做完，或者先取消。", ""},
		{&AdmissionError{Code: "too_many_queued", MaxQueued: 100, Message: "too_many_queued (429): queue has reached max_queued=100"}, "排着的事已经满了（最多 100 件），等前面的做完一些再试。", ""},
		{errors.New("disk full"), "出了点问题，没办成。刷新一下再试。", "disk full"},
	} {
		if words, extra := actionErrorWords(c.err); words != c.words || extra != c.extra {
			t.Errorf("actionErrorWords(%v) = %q, %q", c.err, words, extra)
		}
	}
	if got := actionErrorToast("没能取消", errors.New("disk full")); got != "没能取消：出了点问题，没办成。刷新一下再试。（原话：disk full）" {
		t.Errorf("the toast of an unknown error: %q", got)
	}
}

func TestTaskPageShowsWhatATaskIsDoingAndItsEvidence(t *testing.T) {
	p := newPageRig(t)
	zero, one, none := 0, 1, 0
	if err := p.q.SetMachines([]MachineSlot{{Name: testMachine, Title: "Studio", Online: true, MaxSlots: 4}}); err != nil {
		t.Fatal(err)
	}
	at := testStart.Add(2 * time.Minute)
	page := func(id string) (*wire.View, string) {
		t.Helper()
		v := buildTaskView(p.q, id, at, "UTC")
		return v, checked(t, v)
	}
	has := func(what, raw string, wants ...string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(raw, w) {
				t.Errorf("%s: the page does not have %q", what, w)
			}
		}
	}
	button := func(v *wire.View, key string) *wire.Action {
		for _, s := range v.Sections {
			for _, c := range s.Items {
				if c.Key == key && c.Type == wire.ComponentButton {
					return c.Action
				}
			}
		}
		return nil
	}

	// Running: what it is doing, on which computer; it can be canceled, after asking; the page draws again.
	running := p.enqueue(EnqueueInput{Title: "Running task", Prompt: "Add the function", Repo: "/srv/code/app", AcceptanceCommand: "go test ./...", PermissionProfile: PermissionDefault, Priority: PriorityP1})
	p.lease(running.ID, PhaseWorking, "")
	v, raw := page(running.ID)
	has("running", raw, "Running task", "现在怎么样", "ATS 正在做（在 Studio 上，第 1 次", "接下来：不用做什么", "Add the function", "go test ./...", "ats/"+running.ID+"-1", "会改文件", "重要", "/srv/code/app")
	// Ids and the model tier are only in the technical details, the last section.
	if last := v.Sections[len(v.Sections)-1]; last.Title != "技术细节" {
		t.Errorf("the last section is %q", last.Title)
	}
	for _, s := range v.Sections[:len(v.Sections)-1] {
		part, _ := json.Marshal(s)
		for _, bad := range []string{"todo_1", "\"" + running.ID + "\"", "standard", "medium", "HEAD", "退出码", "Agent", "验收"} {
			if strings.Contains(string(part), bad) {
				t.Errorf("running: section %q has %q", s.Title, bad)
			}
		}
	}
	has("running technical details", raw, "todo_1", "medium / standard", "当时最新的版本")
	if first := v.Sections[0]; first.Title != "现在怎么样" || first.Items[1].Key != "now" {
		t.Errorf("the page does not lead with how the task stands: %+v", first)
	}
	if v.RefreshAfter == 0 {
		t.Error("the page of a running task is not drawn again")
	}
	if a := button(v, "cancel"); a == nil || a.ID != "cancel:"+running.ID || a.Confirm == "" {
		t.Errorf("the cancel button of a running task: %+v", a)
	}
	if button(v, "retry") != nil {
		t.Error("a running task offers a retry")
	}
	if pageTask(&ext.UIRequest{UIParams: wire.UIParams{State: v.State}}) != running.ID {
		t.Errorf("the page does not keep its task in its state: %q", v.State)
	}

	// Needs input: what it waits for, and where to answer.
	waiting := p.enqueue(EnqueueInput{Title: "Waiting task", Repo: "/srv/code/app"})
	p.lease(waiting.ID, PhaseWorking, "Agent 等你批准运行：go test ./...")
	_, raw = page(waiting.ID)
	has("needs input", raw, "等你处理", "它想运行一条命令，等你同意：go test ./...", "在里面 ATS 的对话里回答它的问题或点同意")

	// Failed acceptance: the exit code and the end of the output, the agent's summary, what is left, the attempts.
	failed := p.enqueue(EnqueueInput{Title: "Failed task", Repo: "/srv/code/app", AcceptanceCommand: "go test ./...", PermissionProfile: PermissionDefault, MaxRetries: &none})
	p.complete(failed.ID, p.lease(failed.ID, PhaseAccepting, ""), CompleteAttemptInput{Outcome: StateFailed, Verdict: VerdictFail, ErrorClass: "task_failed", ExitCode: &one,
		Message: "验收命令退出码 1\n--- FAIL: TestGreet\nFAIL example.com/demo", Summary: "I changed greet.go.\nThe test still fails.",
		Artifacts: []Artifact{{Kind: "commit", Path: "0123456789abcdef0123456789abcdef01234567"}, {Kind: "branch", Path: "ats/example-1"}, {Kind: "session", Path: "machine-1/session-1"}}})
	v, raw = page(failed.ID)
	has("failed", raw, "没做成：检查没通过，一共试了 1 次。", "接下来：", "第 1 次检查：没通过，命令报错了。", `--- FAIL: TestGreet\nFAIL example.com/demo`, "它说它做了什么", `I changed greet.go.\nThe test still fails.`,
		"ats/example-1", "0123456", "做过几次", "第 1 次", "1（0 才算通过）")
	for _, bad := range []string{"0123456789abcdef", "session-1", "退出码"} {
		if strings.Contains(raw, bad) {
			t.Errorf("failed: the page has %q", bad)
		}
	}
	if v.RefreshAfter != 0 {
		t.Error("the page of a finished task is drawn again")
	}
	if a := button(v, "retry"); a == nil || a.ID != "retry:"+failed.ID {
		t.Errorf("the retry button of a failed task: %+v", a)
	}
	if button(v, "cancel") != nil {
		t.Error("a failed task offers a cancel")
	}
	out := findRow(t, v, "accept-output")
	if out.Type != wire.ComponentCode || out.Text != "--- FAIL: TestGreet\nFAIL example.com/demo" {
		t.Errorf("the acceptance output: %+v", out)
	}

	// Failed before acceptance: the message that stopped it, whole, and no exit code made up.
	broken := p.enqueue(EnqueueInput{Title: "Broken task", Repo: "/srv/code/app", AcceptanceCommand: "go test ./...", MaxRetries: &none})
	long := "Agent 这一轮出错：There's an issue with the selected model (example-model). It may not exist or you may not have access to it. Run /model to pick a different model."
	p.complete(broken.ID, p.lease(broken.ID, PhaseWorking, ""), CompleteAttemptInput{Outcome: StateFailed, ErrorClass: "config_error", Message: long})
	_, raw = page(broken.ID)
	has("config error", raw, "选的模型用不了", "设置 › 扩展 › "+extName, "没检查：在检查之前就停下了。")
	// The raw error is kept, but only in the technical details at the bottom.
	if got := sectionOf(v2(page(broken.ID)), "raw-message"); got != "技术细节" {
		t.Errorf("config error: the raw message is under %q", got)
	}
	if got := findRow(t, v2(page(broken.ID)), "raw-message").Text; got != long {
		t.Errorf("config error: the raw message: %q", got)
	}
	if strings.Contains(findRow(t, v2(page(broken.ID)), "result").Text, "Run /model") {
		t.Error("config error: the English error leads the page")
	}
	if strings.Contains(raw, "（0 才算通过）") {
		t.Error("config error: the page shows an exit code of a command that never ran")
	}

	// Passed: nothing to press.
	passed := p.enqueue(EnqueueInput{Title: "Passed task", Repo: "/srv/code/app", AcceptanceCommand: "go test ./..."})
	p.complete(passed.ID, p.lease(passed.ID, PhaseAccepting, ""), CompleteAttemptInput{Outcome: StateSucceeded, Verdict: VerdictPass, ExitCode: &zero, Message: "验收命令退出码 0\nok  example.com/demo", Summary: "Added Farewell and its test."})
	v, raw = page(passed.ID)
	has("passed", raw, "做成了，检查也通过了。", "第 1 次检查：通过了。", "ok  example.com/demo", "Added Farewell and its test.")
	if button(v, "retry") != nil || button(v, "cancel") != nil {
		t.Error("a passed task has buttons")
	}

	// No acceptance command: the page says what that means.
	plain := p.enqueue(EnqueueInput{Title: "Plain task", Repo: "/srv/code/app"})
	_, raw = page(plain.ID)
	has("no acceptance", raw, "没设检查方法", "排着队")

	// A task that is gone: an empty page, not an error.
	v, raw = page("task_gone")
	has("unknown", raw, "找不到这件事")
	if len(v.Sections) != 1 || v.Sections[0].Items[0].Type != wire.ComponentEmpty {
		t.Errorf("the page of an unknown task: %s", raw)
	}
}

func TestTaskPageButtonsCancelAndRetryAndStayOnThePage(t *testing.T) {
	r := newRig(t)
	a := newATSApp(r.q)
	a.cfg, a.disp = r.cfg, r.d
	a.nowFunc = func() time.Time { return r.now }

	task := r.enqueue(EnqueueInput{Title: "Working task"})
	r.tick()
	state := ext.EncodeState(taskPage{Task: task.ID})
	req := func(action string) *ext.UIRequest {
		return &ext.UIRequest{UIParams: wire.UIParams{Contribution: queueUI, Action: action, State: state}}
	}

	// The queue's rows open the page: render with the pushed state draws the task, not the queue.
	view, err := a.render(t.Context(), req(""))
	if err != nil || view.State != state || !strings.Contains(checked(t, view), "取消，不做了") {
		t.Fatalf("the task's page: %+v %v", view, err)
	}
	// An app that does not push pages itself asks with the row's action id.
	res, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: queueUI, Action: "open:" + task.ID}})
	if err != nil || res.Push == nil || res.Push.State != state || res.View != nil {
		t.Fatalf("open: %+v %v", res, err)
	}
	if got := r.task(task.ID); got.State != StateRunning {
		t.Fatalf("opening the page changed the task: %s", got.State)
	}

	// Cancel from the page goes through the dispatcher and draws the page again, now with a retry.
	res, err = a.action(context.Background(), req("cancel:"+task.ID))
	if err != nil || !strings.Contains(res.Toast, "已取消") || len(r.hub.interrupts) != 1 || r.task(task.ID).State != StateCanceled {
		t.Fatalf("cancel: %+v %v, interrupts %v", res, err, r.hub.interrupts)
	}
	if res.View == nil || res.View.State != state {
		t.Fatalf("after cancel the answer is not the task's page: %+v", res.View)
	}
	raw := checked(t, res.View)
	if !strings.Contains(raw, "retry:"+task.ID) || strings.Contains(raw, "cancel:"+task.ID) || strings.Contains(raw, "正在做和排着的") {
		t.Errorf("the page after cancel: %s", raw)
	}

	res, err = a.action(context.Background(), req("retry:"+task.ID))
	if err != nil || !strings.Contains(res.Toast, "已经重新交给它做") || r.task(task.ID).State != StateQueued {
		t.Fatalf("retry: %+v %v", res, err)
	}
	if raw := checked(t, res.View); !strings.Contains(raw, "cancel:"+task.ID) || !strings.Contains(raw, "排着队") {
		t.Errorf("the page after retry: %s", raw)
	}

}

func TestCancelOfATaskThatHasEndedSaysSo(t *testing.T) {
	p := newPageRig(t)
	task := p.enqueue(EnqueueInput{Title: "Done task", Repo: "/srv/code/app"})
	p.complete(task.ID, p.lease(task.ID, PhaseWorking, ""), CompleteAttemptInput{Outcome: StateSucceeded, Verdict: VerdictUnverified})
	res, err := p.a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: queueUI, Action: "cancel:" + task.ID}})
	if err != nil || res.Toast != "「Done task」已经结束了，不用再取消。" || mustTask(t, p.q, task.ID).State != StateSucceeded {
		t.Errorf("cancel of an ended task: %+v %v", res, err)
	}
}

func TestFailureWords(t *testing.T) {
	for _, c := range []struct{ class, verdict, want string }{
		{"config_error", "", "模型或配置不可用"},
		{"auth_expired", "", "Agent 登录失效"},
		{"rate_limited", "", "额度或限流"},
		{"infra_error", "", "电脑或环境出错"},
		{"timeout", "", "超时"},
		{"timeout", VerdictFail, "验收命令超时"},
		{"task_failed", VerdictFail, "没通过验收"},
		{"task_failed", VerdictUnverified, "Agent 出错"},
		{"", "", "Agent 出错"},
		{"lost", "", "会话丢失"},
		{"rejected", "", "任务本身有问题（仓库或 ref 不对）"},
		{"something_new", "", "出错（something_new）"},
	} {
		if got := failureWords(c.class, c.verdict); got != c.want {
			t.Errorf("failureWords(%q, %q) = %q, want %q", c.class, c.verdict, got, c.want)
		}
	}
	if got := repoName("/srv/code/app/"); got != "app" {
		t.Errorf("repoName: %q", got)
	}
	if got := oneLine(" a\n\n b\tc ", 10); got != "a b c" {
		t.Errorf("oneLine: %q", got)
	}
	if head, tail := acceptanceOutput("验收命令退出码 1\nline 1\nline 2\n"); head != "验收命令退出码 1" || tail != "line 1\nline 2" {
		t.Errorf("acceptanceOutput: %q %q", head, tail)
	}
}

func TestOutcomeDecisionSaysWhatHappenedInPlainWords(t *testing.T) {
	p := newPageRig(t)
	zero, one, none := 0, 1, 0
	if err := p.q.SetMachines([]MachineSlot{{Name: testMachine, Title: "Studio", Online: true, MaxSlots: 8}}); err != nil {
		t.Fatal(err)
	}
	decision := func(id string) OutcomeDecisionPayload {
		t.Helper()
		task, err := p.q.GetTask(id)
		if err != nil {
			t.Fatal(err)
		}
		d, ok := BuildOutcomeDecision(task)
		if !ok {
			t.Fatalf("no decision for %s (%s)", id, task.State)
		}
		return d
	}

	passed := p.enqueue(EnqueueInput{Title: "Add Farewell and its test", Repo: "/srv/code/app", AcceptanceCommand: "go test ./...", PermissionProfile: PermissionDefault})
	p.complete(passed.ID, p.lease(passed.ID, PhaseAccepting, ""), CompleteAttemptInput{Outcome: StateSucceeded, Verdict: VerdictPass, ExitCode: &zero,
		Message: "验收命令退出码 0\nok", Summary: "Added Farewell\nand a test for it.",
		Artifacts: []Artifact{{Kind: "commit", Path: "0123456789abcdef0123456789abcdef01234567"}, {Kind: "branch", Path: "ats/example-1"}, {Kind: "session", Path: "machine-1/session-1"}}})
	d := decision(passed.ID)
	if d.Question != "ATS 任务「Add Farewell and its test」的结果" || d.Choice != "通过验收" || d.Ref != "ats:"+passed.ID+":succeeded:pass" {
		t.Errorf("passed: %+v", d)
	}
	if want := "Agent 的总结: Added Farewell and a test for it.；验收: `go test ./...` 退出码 0；产物: 分支 ats/example-1 · 提交 0123456；任务 " + passed.ID; d.Reason != want {
		t.Errorf("passed reason:\n got %s\nwant %s", d.Reason, want)
	}

	// Failed before the acceptance command ran: the command is not listed as if it had; the message is.
	broken := p.enqueue(EnqueueInput{Title: strings.Repeat("长", 50), Repo: "/srv/code/app", AcceptanceCommand: "go test ./...", MaxRetries: &none})
	p.complete(broken.ID, p.lease(broken.ID, PhaseWorking, ""), CompleteAttemptInput{Outcome: StateFailed, ErrorClass: "config_error", Message: "Agent 这一轮出错：the model is not available\nmore"})
	d = decision(broken.ID)
	if d.Choice != "失败：模型或配置不可用（试了 1 次）" || d.Ref != "ats:"+broken.ID+":failed" {
		t.Errorf("config error: %+v", d)
	}
	if d.Question != "ATS 任务「"+strings.Repeat("长", 40)+"…」的结果" {
		t.Errorf("a long title is not cut: %s", d.Question)
	}
	if want := "原因: Agent 这一轮出错：the model is not available；任务 " + broken.ID; d.Reason != want {
		t.Errorf("config error reason:\n got %s\nwant %s", d.Reason, want)
	}

	// Failed acceptance after a retry: the attempts are counted, the ref keeps its attempt.
	twice := p.enqueue(EnqueueInput{Title: "Fix the test", Repo: "/srv/code/app", AcceptanceCommand: "go test ./...", MaxRetries: &one})
	p.complete(twice.ID, p.lease(twice.ID, PhaseAccepting, ""), CompleteAttemptInput{Outcome: StateFailed, Verdict: VerdictFail, ErrorClass: "task_failed", ExitCode: &one, Message: "验收命令退出码 1\nFAIL"})
	if _, ok := BuildOutcomeDecision(mustTask(t, p.q, twice.ID)); ok {
		t.Fatal("a task that waits for its retry has a decision")
	}
	_, att, err := p.q.LeaseTask(twice.ID, "dispatcher", testMachine, testStart.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.q.CompleteAttempt(CompleteAttemptInput{TaskID: twice.ID, FenceToken: att.FenceToken, WorkerID: "dispatcher", Now: testStart.Add(time.Hour + time.Minute),
		Outcome: StateFailed, Verdict: VerdictFail, ErrorClass: "task_failed", ExitCode: &one, Message: "验收命令退出码 1\nFAIL"}); err != nil {
		t.Fatal(err)
	}
	d = decision(twice.ID)
	if d.Choice != "失败：没通过验收（试了 2 次）" || d.Ref != "ats:"+twice.ID+":2:failed" || !strings.HasPrefix(d.Reason, "验收: `go test ./...` 退出码 1；") {
		t.Errorf("failed twice: %+v", d)
	}

	// No acceptance command, canceled, and a long summary held to the cap with whole characters.
	plain := p.enqueue(EnqueueInput{Title: "Look around", Repo: "/srv/code/app"})
	p.complete(plain.ID, p.lease(plain.ID, PhaseWorking, ""), CompleteAttemptInput{Outcome: StateSucceeded, Verdict: VerdictUnverified, Message: "the reply", Summary: strings.Repeat("字", 3000)})
	d = decision(plain.ID)
	if d.Choice != "跑完了，没有验收命令" || strings.Contains(d.Reason, "验收") {
		t.Errorf("unverified: %+v", d)
	}
	if n := len([]rune(d.Reason)); n > decisionReasonMost || !strings.HasSuffix(d.Reason, "任务 "+plain.ID) || !strings.Contains(d.Reason, strings.Repeat("字", 399)+"…") {
		t.Errorf("a long summary: %d characters, ends %q", n, d.Reason[max(0, len(d.Reason)-40):])
	}
	canceled := p.enqueue(EnqueueInput{Title: "Never mind", Repo: "/srv/code/app"})
	if _, err := p.q.CancelTask(canceled.ID, testStart); err != nil {
		t.Fatal(err)
	}
	if d = decision(canceled.ID); d.Choice != "已取消" || d.Reason != "任务 "+canceled.ID {
		t.Errorf("canceled: %+v", d)
	}
}

// hasLatin says whether a text has a Latin letter: the pages name things in Chinese, besides the names ATS and
// Persona and what the user or the agent wrote.
func hasLatin(s string) bool {
	return strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
}

func v2(v *wire.View, _ string) *wire.View { return v }

func mustTask(t *testing.T, q *Queue, id string) *Task {
	t.Helper()
	task, err := q.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestGateCardSaysWhatApprovingDoes(t *testing.T) {
	var gate struct {
		Actions []struct{ Pattern, What string } `json:"actions"`
	}
	if err := json.Unmarshal(mustRead(t, "assistant/gate.json"), &gate); err != nil {
		t.Fatal(err)
	}
	if len(gate.Actions) != 1 || !strings.Contains(gate.Actions[0].What, "无人值守") || !strings.Contains(gate.Actions[0].What, "你的电脑") {
		t.Errorf("the card's wording: %+v", gate.Actions)
	}
	skill := string(mustRead(t, "skills/ats-todo/SKILL.md"))
	for _, want := range []string{"确认卡上只有这条命令的原文", "config_error", "接着上一次尝试的提交"} {
		if !strings.Contains(skill, want) {
			t.Errorf("the skill does not have %q", want)
		}
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestTaskPageValidatesWithLongPromptSummaryAndFailureMessage(t *testing.T) {
	p := newPageRig(t)
	none := 0
	longTitle := strings.Repeat("题", 2500)
	task := p.enqueue(EnqueueInput{
		Title:      longTitle,
		Prompt:     strings.Repeat("说", 2500),
		Repo:       "/srv/code/app",
		MaxRetries: &none,
	})
	p.complete(task.ID, p.lease(task.ID, PhaseWorking, ""), CompleteAttemptInput{
		Outcome:    StateFailed,
		ErrorClass: "infra_error",
		Message:    strings.Repeat("错", 2500),
		Summary:    strings.Repeat("结", 2500),
	})
	if err := p.q.markDecisionWritten(task.ID, "ats:"+task.ID+":failed"); err != nil {
		t.Fatal(err)
	}

	// Also queue an active task with a >2000-rune title so both activeRow and terminalRow are validated.
	active := p.enqueue(EnqueueInput{
		Title:  longTitle,
		Prompt: "Active task",
		Repo:   "/srv/code/app",
	})
	p.lease(active.ID, PhaseWorking, "")

	at := testStart.Add(2 * time.Minute)
	view := buildTaskView(p.q, task.ID, at, "UTC")
	checked(t, view)
	checked(t, buildQueueView(p.q, p.a.config(), at, "UTC"))

	for _, key := range []string{"result", "summary", "prompt", "raw-message"} {
		comp := findRow(t, view, key)
		if n := utf8.RuneCountInString(comp.Text); n > detailTextMost {
			t.Errorf("%s text length = %d runes, want <= %d", key, n, detailTextMost)
		}
	}
	if res := findRow(t, view, "result"); !strings.Contains(res.Text, "结果已经写回待办。") {
		t.Errorf("result text dropped writeback status line: %q", res.Text[max(0, len(res.Text)-80):])
	}

	// Verify skipped-writeback suffix is also preserved within detailTextMost.
	skipped := p.enqueue(EnqueueInput{
		Title:      longTitle,
		Prompt:     "Short prompt",
		Repo:       "/srv/code/app",
		MaxRetries: &none,
	})
	p.complete(skipped.ID, p.lease(skipped.ID, PhaseWorking, ""), CompleteAttemptInput{
		Outcome:    StateFailed,
		ErrorClass: "infra_error",
		Message:    strings.Repeat("错", 2500),
	})
	if err := p.q.markDecisionSkipped(skipped.ID, "http_404"); err != nil {
		t.Fatal(err)
	}
	skippedView := buildTaskView(p.q, skipped.ID, at, "UTC")
	checked(t, skippedView)
	if res := findRow(t, skippedView, "result"); !strings.Contains(res.Text, "结果没能写回待办") {
		t.Errorf("result text dropped skipped-writeback status line: %q", res.Text[max(0, len(res.Text)-80):])
	}

	// Boundary checks for truncateRunes.
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{"abc", 0, ""},
		{"abc", -1, ""},
		{"abc", 1, "a"},
		{"abc", 3, "abc"},
		{"abcd", 3, "ab…"},
	} {
		if got := truncateRunes(tc.in, tc.max); got != tc.want {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
	if got := truncateRunes(strings.Repeat("a", 2500), 2000); utf8.RuneCountInString(got) > 2000 {
		t.Errorf("truncateRunes(..., 2000) returned %d runes", utf8.RuneCountInString(got))
	}
}

func TestLauncherForSpec(t *testing.T) {
	cases := []struct {
		name     string
		launcher string
		machine  string
		want     string
	}{
		{name: "empty", launcher: "", machine: "workstation", want: ""},
		{name: "blank", launcher: " \n ; , ", machine: "workstation", want: ""},
		{name: "single", launcher: "default-claude", machine: "laptop", want: "default-claude"},
		{name: "single_trimmed", launcher: "  default-claude  ", machine: "laptop", want: "default-claude"},
		{name: "override_match", launcher: "default-claude,workstation=local-claude", machine: "workstation", want: "local-claude"},
		{name: "override_case_insensitive", launcher: "default-claude ; workstation = local-claude", machine: " WORKSTATION ", want: "local-claude"},
		{name: "override_fallback", launcher: "default-claude,workstation=local-claude", machine: "server", want: "default-claude"},
		{name: "explicit_only_unlisted", launcher: "workstation=local-claude\nserver=default-claude", machine: "laptop", want: ""},
		{name: "explicit_only_listed", launcher: "workstation=local-claude\nserver=default-claude", machine: "server", want: "default-claude"},
		{name: "first_machine_candidate", launcher: "default-a|default-b,workstation=primary|backup", machine: "workstation", want: "primary"},
		{name: "first_default_candidate", launcher: "default-a|default-b,workstation=primary|backup", machine: "server", want: "default-a"},
		{name: "empty_override_falls_back", launcher: "default-a,workstation=| ", machine: "workstation", want: "default-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := launcherForSpec(tc.launcher, tc.machine); got != tc.want {
				t.Fatalf("launcherForSpec(%q, %q) = %q, want %q", tc.launcher, tc.machine, got, tc.want)
			}
			cfg := ExtConfig{Launcher: tc.launcher}
			if got := cfg.launcherFor(tc.machine); got != tc.want {
				t.Fatalf("launcherFor(%q) = %q, want %q", tc.machine, got, tc.want)
			}
		})
	}
}

func TestLaunchersForSpecOrdersAndDeduplicatesCandidates(t *testing.T) {
	cfg := ExtConfig{Launcher: " default-a | backup ; workstation = primary | backup | primary \nother=ignored,WORKSTATION= || local , default-b | default-a "}
	for _, tc := range []struct {
		machine string
		want    []string
	}{
		{" Workstation ", []string{"primary", "backup", "local", "default-a", "default-b"}},
		{"other", []string{"ignored", "default-a", "backup", "default-b"}},
		{"unlisted", []string{"default-a", "backup", "default-b"}},
	} {
		if got := cfg.launchersFor(tc.machine); !slices.Equal(got, tc.want) {
			t.Errorf("launchers for %q = %v, want %v", tc.machine, got, tc.want)
		}
	}
	if got := launchersForSpec(" | ; =invalid,other=ignored", ""); len(got) != 0 {
		t.Fatalf("no candidates expected, got %v", got)
	}
}

func TestBuildPromptReadonlyNoExitPlanMode(t *testing.T) {
	task := &Task{ID: "task_99", Prompt: "inspect repo"}
	ws := prepareResult{Path: "/tmp/wt", BaseSHA: "abc1234"}
	got := buildPrompt(task, ws, true, nil, false)
	if !strings.Contains(got, "ExitPlanMode") {
		t.Fatalf("expected readonly buildPrompt to forbid ExitPlanMode, got:\n%s", got)
	}
}
