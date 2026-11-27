package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// The dispatcher runs the queue. Every round it takes the ready tasks the scheduler admits, and moves each
// running attempt one step along:
//
//	preparing  the computer's program checks the repository out into a worktree (ats.prepare)
//	working    a Persona agent session works in the worktree; the task is needs_input while the session waits
//	           for the user (a question, an approval) or ended its turn saying it is blocked
//	accepting  the computer's program runs the acceptance command (ats.accept); its exit code is the verdict
//
// and then records the outcome, removes the worktree and writes the outcome to the task's to-do. The queue
// records each running attempt (Task.Exec), so a restart picks it up where it was.

const (
	dispatchInterval        = 15 * time.Second
	refreshInterval         = 60 * time.Second
	stablePolls             = 2
	pruneMaxAge             = 30 * 24 * time.Hour
	pruneKeep               = 500
	lostSessionAfter        = 2 * time.Minute
	blockedMarker           = "ATS_BLOCKED:"
	waitingInSession        = "Agent 在会话里等你回答或批准"
	nodeCallTimeout         = 25 * time.Second
	maxReadonlyPlanDeclines = 3
	launcherProbeTimeout    = 5 * time.Second
	launcherProbeTTL        = 30 * time.Second
	breakerCooldown         = 3 * time.Minute
	gatewayStartupWindow    = 30 * time.Second
	autoContinueGrace       = 45 * time.Second
	maxAutoContinues        = 2
	autoContinuePrompt      = "The response stream dropped mid-response. Continue in this session, keeping the work you already verified. Finish the remaining reasoning and actions, then provide the complete final answer."
)

type launcherProbeState struct {
	reachable bool
	until     time.Time
	err       string
}

type dispatcher struct {
	q      *Queue
	api    hubAPI
	nodes  nodeCaller
	writer *DecisionWriter
	cfg    func() ExtConfig
	now    func() time.Time
	// refresh reads what the hub knows that the scheduler needs (quota); called once a minute.
	refresh func(ctx context.Context)
	// changed tells the apps the queue page's data changed.
	changed func()

	mu           sync.Mutex
	accepted     map[string]acceptResult // acceptance results the node program reported, by attempt key
	planDeclines map[string]int          // count of ExitPlanMode auto-declines per attempt key
	lastRefresh  time.Time
	wake         chan struct{}

	probeMu    sync.Mutex
	probeCache map[string]launcherProbeState
}

func newDispatcher(q *Queue, api hubAPI, nodes nodeCaller, writer *DecisionWriter, cfg func() ExtConfig, now func() time.Time) *dispatcher {
	return &dispatcher{q: q, api: api, nodes: nodes, writer: writer, cfg: cfg, now: now,
		accepted: map[string]acceptResult{}, planDeclines: map[string]int{}, wake: make(chan struct{}, 1),
		probeCache: map[string]launcherProbeState{}}
}

// run is the background loop; it ends with ctx.
func (d *dispatcher) run(ctx context.Context) {
	t := time.NewTicker(dispatchInterval)
	defer t.Stop()
	for {
		d.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wake:
		}
	}
}

// poke asks for a round now (a task was queued, an acceptance command finished).
func (d *dispatcher) poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// onAccepted takes an acceptance result the node program sent.
func (d *dispatcher) onAccepted(res acceptResult) {
	key, err := attemptKey(res.TaskID, res.Attempt)
	if err != nil {
		return
	}
	res.Done = true
	d.mu.Lock()
	d.accepted[key] = res
	d.mu.Unlock()
	d.poke()
}

func (d *dispatcher) tick(ctx context.Context) {
	now := d.now()
	_ = d.q.RenewLeases(now)

	d.mu.Lock()
	slow := now.Sub(d.lastRefresh) >= refreshInterval
	if slow {
		d.lastRefresh = now
	}
	d.mu.Unlock()
	if slow {
		d.refreshMachines(ctx)
		if d.refresh != nil {
			d.refresh(ctx)
		}
		_, _ = d.q.Prune(now, pruneMaxAge, pruneKeep)
	}

	d.retryPendingTodoDetaches(ctx)
	moved := false
	for _, t := range d.q.ActiveTasks() {
		if d.advance(ctx, t) {
			moved = true
		}
	}
	leased, _, _, err := d.q.DispatchNext(8, d.now())
	if err == nil {
		for _, t := range leased {
			d.advance(ctx, t)
			moved = true
		}
	}
	if d.writer != nil && d.writer.HasTodosWriteScope() {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _ = d.writer.SyncQueueOutcomes(wctx, d.q)
		cancel()
	}
	if moved && d.changed != nil {
		d.changed()
	}
}

// refreshMachines sets the scheduler's slots from the hub's list of computers: the ones the extension's program
// is switched on for, each with the configured number of tasks at a time.
func (d *dispatcher) refreshMachines(ctx context.Context) {
	now := d.now()
	d.probeMu.Lock()
	for key, st := range d.probeCache {
		if !now.Before(st.until) {
			delete(d.probeCache, key)
		}
	}
	d.probeMu.Unlock()

	cfg := d.cfg()
	cctx, cancelList := context.WithTimeout(ctx, 10*time.Second)
	machines, err := d.api.Machines(cctx)
	cancelList()
	if err != nil {
		return // keep the last known slots: a hub that is briefly unreachable must not look like no computers
	}
	on := map[string]bool{}
	for _, id := range cfg.Machines {
		on[id] = true
	}
	var slots []MachineSlot
	for _, m := range machines {
		if !on[m.ID] {
			continue
		}
		slots = append(slots, MachineSlot{Name: m.ID, Title: m.Name, Online: m.Online, MaxSlots: cfg.parallel()})
	}
	pctx, cancelProbes := context.WithTimeout(ctx, 10*time.Second)
	defer cancelProbes()
	var probes sync.WaitGroup
	for i, slot := range slots {
		if !slot.Online {
			continue
		}
		candidates := cfg.launchersFor(slot.Name)
		if len(candidates) == 0 {
			continue
		}
		probes.Go(func() {
			_, slots[i].Degraded, _ = d.resolveHealthyLauncher(pctx, slot.Name, candidates, true)
		})
	}
	probes.Wait()
	_ = d.q.SetMachines(slots)
}

// advance moves one running attempt a step along; it reports whether the task changed.
func (d *dispatcher) advance(ctx context.Context, t *Task) bool {
	if len(t.Attempts) == 0 {
		return false
	}
	att := t.Attempts[len(t.Attempts)-1]
	e := t.Exec
	if e == nil || e.Phase == "" {
		var err error
		t, err = d.q.UpdateExec(t.ID, t.FenceToken, d.now(), func(e *Execution) {
			e.MachineID, e.Phase = att.Machine, PhasePreparing
		})
		if err != nil {
			return false
		}
		e = t.Exec
	}
	switch e.Phase {
	case PhasePreparing:
		return d.prepare(ctx, t, att)
	case PhaseWorking:
		return d.watch(ctx, t, att)
	case PhaseAccepting:
		return d.collect(ctx, t, att)
	}
	return false
}

func (d *dispatcher) nodeCall(ctx context.Context, machine, method string, params, out any) error {
	cctx, cancel := context.WithTimeout(ctx, nodeCallTimeout)
	defer cancel()
	return d.nodes.Call(cctx, machine, method, params, out)
}

func (d *dispatcher) resolveHealthyLauncher(ctx context.Context, machineID string, candidates []string, allowProbe bool) (chosen string, allUnreachable bool, detail string) {
	if len(candidates) == 0 {
		return "", false, ""
	}
	var failures []string
	for _, launcher := range candidates {
		st := d.launcherProbe(ctx, machineID, launcher, allowProbe)
		if st.reachable {
			return launcher, false, ""
		}
		failures = append(failures, launcher+": "+st.err)
	}
	return "", true, strings.Join(failures, "；")
}

func (d *dispatcher) launcherProbe(ctx context.Context, machineID, launcher string, allowProbe bool) launcherProbeState {
	key := machineID + "\x00" + launcher
	d.probeMu.Lock()
	st := d.probeCache[key]
	d.probeMu.Unlock()
	if d.now().Before(st.until) {
		return st
	}
	if !allowProbe || ctx.Err() != nil {
		return launcherProbeState{reachable: true}
	}
	pctx, cancel := context.WithTimeout(ctx, launcherProbeTimeout)
	var res probeLauncherResult
	err := d.nodeCall(pctx, machineID, "ats.probe_launcher", probeLauncherParams{Launcher: launcher}, &res)
	cancel()

	now := d.now()
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	// 探测期间若已发生会话网关错误，仍须等待熔断期结束。
	if current := d.probeCache[key]; !current.reachable && now.Before(current.until) {
		return current
	}
	// 旧版节点可能缺少此方法。RPC 的其他错误也交由 ats.prepare 处理。
	if err != nil || (res.Reachable && res.Skipped) {
		return launcherProbeState{reachable: true}
	}
	st = launcherProbeState{reachable: res.Reachable, until: now.Add(launcherProbeTTL)}
	if !res.Reachable {
		st.until = now.Add(breakerCooldown)
		st.err = strings.TrimSpace(res.Endpoint + " " + res.Error)
	}
	d.probeCache[key] = st
	return st
}

func (d *dispatcher) invalidateProbe(machineID, launcher, detail string) {
	if launcher == "" {
		return
	}
	st := launcherProbeState{until: d.now().Add(breakerCooldown), err: detail}
	d.probeMu.Lock()
	d.probeCache[machineID+"\x00"+launcher] = st
	d.probeMu.Unlock()
}

// prepare checks the worktree out and starts the agent session in it.
func (d *dispatcher) prepare(ctx context.Context, t *Task, att Attempt) bool {
	e := t.Exec
	cfg := d.cfg()
	agentType := effectiveAgentType(att.Harness, t.AgentType)
	candidates := launchersForSpec(t.Launcher, e.MachineID)
	if len(candidates) == 0 {
		if agentType == AgentClaude {
			candidates = cfg.launchersFor(e.MachineID)
		} else {
			// 只指定引擎时，Hub 仍可能选用默认的 Claude 启动器。
			candidates = []string{agentType}
		}
	}
	launcher, allUnreachable, detail := d.resolveHealthyLauncher(ctx, e.MachineID, candidates, len(candidates) > 0 && (agentType == AgentClaude || strings.TrimSpace(t.Launcher) != ""))
	if allUnreachable {
		d.finish(ctx, t, att, outcome{state: StateFailed, errClass: "gateway_unreachable", message: "所有候选启动器网关不可达：" + detail})
		return true
	}
	if launcher == "" {
		launcher = launcherForSpec(t.Launcher, e.MachineID)
	}
	readonly := t.PermissionProfile != PermissionDefault
	prev := lastFailedReport(t)
	// A writable retry goes on from what the earlier attempt committed, when that commit is on this computer.
	resume := ""
	if !readonly && prev != nil {
		resume = resumeCommit(t, e.MachineID)
	}
	var ws prepareResult
	call := func(ref string) error {
		ws = prepareResult{}
		return d.nodeCall(ctx, e.MachineID, "ats.prepare", prepareParams{
			TaskID: t.ID, Attempt: att.AttemptNo, Repo: t.Workspace.Repo, Ref: ref, Readonly: readonly,
		}, &ws)
	}
	var err error
	if resume != "" {
		if err = call(resume); err != nil && strings.Contains(err.Error(), "ref_not_found") {
			resume = "" // the earlier attempt's branch was deleted since: start from the base again
			err = call(t.Workspace.BaseSHA)
		}
	} else {
		err = call(t.Workspace.BaseSHA)
	}
	if err != nil {
		// What the task asks for is wrong (not what the computer is doing right now): another attempt would
		// fail the same way.
		msg := err.Error()
		permanent := strings.Contains(msg, "repo_not_allowed") || strings.Contains(msg, "ref_not_found") || strings.Contains(msg, "repo:")
		d.finish(ctx, t, att, outcome{state: StateFailed, errClass: classIf(permanent, "rejected", "infra_error"), message: "准备工作目录失败：" + msg})
		return true
	}
	updated, err := d.q.UpdateExec(t.ID, t.FenceToken, d.now(), func(e *Execution) {
		e.Worktree, e.Branch, e.BaseSHA = ws.Path, ws.Branch, ws.BaseSHA
	})
	if err != nil {
		// The attempt is no longer active (for example canceled during ats.prepare): release the worktree
		// and untouched branch without starting an agent session.
		t.Exec.Worktree, t.Exec.Branch, t.Exec.BaseSHA = ws.Path, ws.Branch, ws.BaseSHA
		d.cleanup(context.WithoutCancel(ctx), t, att.AttemptNo, true)
		return false
	}
	t = updated
	e = t.Exec

	mode := cfg.readonlyMode()
	if !readonly {
		mode = cfg.writeMode()
	}
	model := strings.TrimSpace(t.Model)
	if model == "" && agentType == AgentClaude {
		model = cfg.model(cmpOr(att.ModelTier, t.ModelTier))
	}
	cctx, cancel := context.WithTimeout(ctx, sessionStartTimeout(agentType))
	sid, err := d.api.CreateSession(cctx, sessionRequest{
		MachineID: e.MachineID, Cwd: ws.Path, AgentType: agentType, Launcher: launcher, Prompt: buildPrompt(t, ws, readonly, prev, resume != ""),
		PermissionMode: mode, Model: model, TabName: "ATS · " + firstLinePreview(t.Title, 40), Async: true,
	})
	cancel()
	if err != nil {
		d.finish(ctx, t, att, outcome{state: StateFailed, errClass: "infra_error", message: "启动 Agent 会话失败：" + err.Error()})
		return true
	}
	if _, err := d.q.UpdateExec(t.ID, t.FenceToken, d.now(), func(e *Execution) {
		e.Phase, e.Worktree, e.Branch, e.BaseSHA, e.SessionID = PhaseWorking, ws.Path, ws.Branch, ws.BaseSHA, sid
		e.Launcher, e.Model = launcher, model
	}); err != nil {
		// The attempt is no longer active (for example canceled while CreateSession was in flight):
		// interrupt the live session and release the worktree and branch instead of leaking them.
		t.Exec.Worktree, t.Exec.Branch, t.Exec.BaseSHA, t.Exec.SessionID = ws.Path, ws.Branch, ws.BaseSHA, sid
		d.cleanup(context.WithoutCancel(ctx), t, att.AttemptNo, true)
		return false
	}
	if todo := t.TodoID(); todo != "" {
		actx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := d.api.AttachTodo(actx, todo, e.MachineID, sid)
		cancel()
		if err != nil {
			warning := firstLinePreview(err.Error(), 500)
			slog.Warn("ATS could not attach session to to-do", "task_id", t.ID, "attempt", att.AttemptNo,
				"todo_id", todo, "machine_id", e.MachineID, "session_id", sid, "error", warning)
			if _, err := d.q.UpdateExec(t.ID, t.FenceToken, d.now(), func(e *Execution) {
				e.TodoAttachError = warning
			}); err != nil {
				slog.Warn("ATS could not save to-do attachment error", "task_id", t.ID, "attempt", att.AttemptNo, "error", err)
			}
		} else if cur, err := d.q.GetTask(t.ID); err == nil && (cur.FenceToken != t.FenceToken || !isActiveState(cur.State)) {
			// A cancel can finish before the hub applies the attachment.
			if !d.detachTodoSession(ctx, t.ID, att.AttemptNo, todo, e.MachineID, sid) {
				_ = d.q.SetAttemptTodoDetachPending(t.ID, att.AttemptNo, true)
			}
		}
	}
	return true
}

// lastFailedReport is the report of the task's latest attempt when that attempt failed: what a new attempt is
// told about.
func lastFailedReport(t *Task) *EvidenceReport {
	if len(t.Reports) == 0 {
		return nil
	}
	rep := t.Reports[len(t.Reports)-1]
	if strings.EqualFold(strings.TrimSpace(rep.Outcome), StateSucceeded) {
		return nil
	}
	return &rep
}

// resumeCommit is the commit the task's earlier attempts left on machine: the latest report that has one, of an
// attempt that ran there (a commit made on another computer is not in this one's repository).
func resumeCommit(t *Task, machine string) string {
	for i := len(t.Reports) - 1; i >= 0; i-- {
		rep := t.Reports[i]
		sha := ""
		for _, a := range rep.Artifacts {
			if a.Kind == "commit" && strings.TrimSpace(a.Path) != "" {
				sha = strings.TrimSpace(a.Path)
			}
		}
		if sha == "" {
			continue
		}
		for _, att := range t.Attempts {
			if att.AttemptNo == rep.AttemptNo && att.Machine == machine {
				return sha
			}
		}
		return ""
	}
	return ""
}

// errorClassInWords says an error class to the agent of the next attempt.
func errorClassInWords(class string) string {
	switch class {
	case "task_failed":
		return "the work did not pass (the acceptance command failed, or the agent's turn ended in an error)"
	case "timeout":
		return "it ran out of time"
	case "infra_error":
		return "the computer or the agent session failed, not necessarily the work"
	case "rate_limited":
		return "the model service's rate or usage limit was reached"
	case "gateway_unreachable", "gateway_error":
		return "the inference gateway was unavailable"
	case "stream_interrupted":
		return "the agent's response stream was interrupted"
	case "lost":
		return "the agent session disappeared"
	case "config_error":
		return "the configured model was not available"
	case "auth_expired":
		return "the agent was not signed in"
	case "":
		return "it failed"
	}
	return "it failed (" + class + ")"
}

const (
	// promptFailureRunes is how much of the earlier attempt's report a retry is told: its first line (how the
	// acceptance command ended) and the end of the output. The agent can run the command again for the rest.
	promptFailureRunes = 800
	summaryRunes       = 1200
)

// summaryMarker is the line the agent is asked to put before its closing summary.
const summaryMarker = "Summary:"

// agentSummary is what is kept of the agent's last reply: the part after its last summary marker when it wrote
// one, else the reply's beginning (a reply states its conclusion first more often than last).
func agentSummary(reply string) string {
	reply = strings.TrimSpace(reply)
	lines := strings.Split(reply, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimLeft(strings.TrimSpace(lines[i]), "#*_ ")
		rest, ok := strings.CutPrefix(line, summaryMarker)
		if !ok {
			continue
		}
		rest = strings.TrimLeft(rest, "*_ ")
		if sum := strings.TrimSpace(strings.Join(append([]string{rest}, lines[i+1:]...), "\n")); sum != "" {
			return headRunes(sum, summaryRunes)
		}
		break
	}
	return headRunes(reply, summaryRunes)
}

// headRunes is the beginning of s, at most max runes of it, line breaks kept.
func headRunes(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return strings.TrimSpace(string(r[:max])) + "…"
}

// tailRunes is the end of s, at most max runes of it, line breaks kept.
func tailRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return "…" + strings.TrimSpace(string(r[len(r)-max:]))
}

// buildPrompt is what the agent is told: the task, the rules of running unattended and, for a retry, how the
// earlier attempt ended (prev) and whether the worktree goes on from its commit (resumed).
func buildPrompt(t *Task, ws prepareResult, readonly bool, prev *EvidenceReport, resumed bool) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(t.Prompt))
	b.WriteString("\n\n---\n")
	fmt.Fprintf(&b, "You are running unattended as queued task %s, in a git worktree of its own checked out at commit %s.\n", t.ID, ws.BaseSHA)
	if readonly {
		b.WriteString("- This task is read-only: do not change, create or delete files, and do not run commands that do. Investigate and report.\n")
		b.WriteString("- This is an unattended read-only inspection task: do not write plan files or call ExitPlanMode; run read-only inspection commands directly and output your final report.\n")
	} else {
		fmt.Fprintf(&b, "- Work only inside this worktree. Commit your work on the current branch (%s) if you can; changes left uncommitted are committed to it for you when you finish. Do not push, and do not touch other branches or worktrees.\n", ws.Branch)
	}
	b.WriteString("- Nobody is watching: avoid commands that need approval when a file edit does the job.\n")
	if t.Acceptance != nil && strings.TrimSpace(t.Acceptance.Command) != "" {
		fmt.Fprintf(&b, "- When you finish, this command is run in the worktree and must exit 0 for the task to pass: %s\n", strings.TrimSpace(t.Acceptance.Command))
	}
	fmt.Fprintf(&b, "- If you cannot go on without the user, end your reply with one line: %s <what you need>\n", blockedMarker)
	fmt.Fprintf(&b, "- End your reply with a line \"%s\" followed by at most five lines: what you did, what you found, and anything left open. That part is what the user reads first.\n", summaryMarker)
	if prev != nil {
		attempt := prev.AttemptNo + 1
		if n := len(t.Attempts); n > 0 {
			attempt = t.Attempts[n-1].AttemptNo
		}
		fmt.Fprintf(&b, "\n---\nThis is attempt %d of this task. Attempt %d did not succeed: %s.\n", attempt, prev.AttemptNo, errorClassInWords(prev.ErrorClass))
		msg := outputEnd(prev.Message, promptFailureRunes)
		if msg != "" {
			fmt.Fprintf(&b, "How it ended (the end of the output):\n%s\n", indentBlock(msg))
		}
		// A turn that ended in an error reports the agent's reply as its message: not said twice.
		if sum := strings.TrimSpace(prev.Summary); sum != "" && !strings.HasSuffix(msg, sum) {
			fmt.Fprintf(&b, "What that attempt's agent said at its end:\n%s\n", indentBlock(sum))
		}
		switch {
		case resumed:
			fmt.Fprintf(&b, "Your worktree starts from that work (commit %s): look at what is there and continue from it; do not start over.\n", ws.BaseSHA)
		case readonly:
		default:
			b.WriteString("Your worktree starts again from the base commit: none of that attempt's changes are in it.\n")
		}
	}
	return b.String()
}

func indentBlock(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

// watch reads the session's state: waiting for the user, still working, or done with its turn.
func (d *dispatcher) watch(ctx context.Context, t *Task, att Attempt) bool {
	e := t.Exec
	now := d.now()
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	s, err := d.api.Session(cctx, e.MachineID, e.SessionID)
	cancel()
	if err != nil {
		return false // the hub is briefly unreachable: look again next round
	}
	if s == nil {
		if now.Sub(e.PhaseSince) > lostSessionAfter {
			d.finish(ctx, t, att, outcome{state: StateFailed, errClass: "lost", message: "Agent 会话不见了（被关闭或电脑重装）"})
			return true
		}
		return false
	}
	if model := strings.TrimSpace(s.Model); model != "" && model != e.Model {
		updated, err := d.q.UpdateExec(t.ID, t.FenceToken, now, func(e *Execution) { e.Model = model })
		if err != nil {
			return false
		}
		t = updated
		e = t.Exec
	}
	elapsed := now.Sub(e.PhaseSince)
	timeout := d.cfg().taskTimeout()
	working := s.State == "working" || s.BackgroundWorking
	resumed := working && e.ContinuedAt != nil && s.UpdatedAt != e.ContinuedErrorAt
	// Hub 可能先报会话恢复，再清除上轮的错误文本。
	continuationWorking := resumed || (working && e.ContinueCount > 0 && e.ContinuedAt == nil)
	switch {
	case s.StartError != "":
		d.finish(ctx, t, att, outcome{state: StateFailed, errClass: "infra_error", message: "Agent 会话没有启动：" + s.StartError})
		return true
	case s.Starting:
		return false
	case s.TurnError != "" && !continuationWorking:
		if e.ContinuedAt != nil && now.Sub(*e.ContinuedAt) < autoContinueGrace && s.UpdatedAt == e.ContinuedErrorAt {
			if elapsed >= timeout {
				d.timeoutAttempt(ctx, t, att, timeout)
				return true
			}
			return false
		}
		d.handleTurnError(ctx, t, att, s, now, timeout-elapsed)
		return true
	case s.State == "needs_you" || s.PendingInteractions > 0:
		if t.PermissionProfile != PermissionDefault {
			if declined, changed := d.tryDeclineReadonlyPlan(ctx, t, att, e); declined {
				return changed
			}
		}
		why, known := d.waitingFor(ctx, e)
		if !known && e.Waiting != "" && e.JudgedUpdate == "" {
			return false // the hub did not say what the session asks: keep what was known
		}
		return d.setWaiting(t, why, "")
	case working:
		if elapsed >= timeout {
			d.timeoutAttempt(ctx, t, att, timeout)
			return true
		}
		if e.Waiting != "" || e.Stable != 0 || resumed {
			_, err := d.q.UpdateExec(t.ID, t.FenceToken, now, func(e *Execution) {
				e.Waiting, e.Stable, e.SeenUpdate = "", 0, ""
				if resumed {
					e.ContinuedAt, e.ContinuedErrorAt = nil, ""
				}
			})
			return err == nil
		}
		return false
	case s.State == "unread" || s.State == "idle":
		if s.UpdatedAt == e.JudgedUpdate && e.JudgedUpdate != "" {
			return false // the turn that said it is blocked: nothing new since
		}
		// The hub shows a finished turn for a moment between an agent's steps: take it only when two rounds
		// in a row see the same one.
		stable := 1
		if s.UpdatedAt == e.SeenUpdate {
			stable = e.Stable + 1
		}
		if stable < stablePolls {
			_, _ = d.q.UpdateExec(t.ID, t.FenceToken, now, func(e *Execution) { e.SeenUpdate, e.Stable = s.UpdatedAt, stable })
			return false
		}
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		reply, err := d.api.LastReply(rctx, e.MachineID, e.SessionID)
		cancel()
		if err != nil {
			return false
		}
		if need, blocked := blockedReason(reply); blocked {
			return d.setWaiting(t, "Agent 说它卡住了："+need, s.UpdatedAt)
		}
		return d.startAcceptance(ctx, t, att, reply)
	}
	return false // offline, unknown: the computer is away; the session is still there when it is back
}

func (d *dispatcher) handleTurnError(ctx context.Context, t *Task, att Attempt, s *hubSession, now time.Time, remaining time.Duration) {
	e := t.Exec
	class := turnErrorClass(s.TurnErrorKind, s.TurnError)
	text := strings.TrimRight(strings.TrimSpace(s.TurnError), "。.")
	msg := "Agent 这一轮出错：" + s.TurnError
	switch class {
	case "config_error":
		msg = "模型不可用：" + text + "。到扩展设置里检查「模型」一栏后再重试。"
	case "auth_expired":
		msg = "Agent 的登录失效了：" + text + "。在那台电脑上重新登录后再重试。"
	}
	mayContinue := class == "stream_interrupted" ||
		(class == "gateway_error" && (now.Sub(e.PhaseSince) >= gatewayStartupWindow || e.ContinueCount > 0))
	if mayContinue && e.ContinueCount < maxAutoContinues && remaining > 0 {
		mctx, cancel := context.WithTimeout(ctx, min(10*time.Second, remaining))
		err := d.api.SendMessage(mctx, e.MachineID, e.SessionID, autoContinuePrompt)
		cancel()
		if err == nil {
			_, err = d.q.UpdateExec(t.ID, t.FenceToken, now, func(e *Execution) {
				e.ContinueCount++
				e.ContinuedAt, e.ContinuedErrorAt = &now, s.UpdatedAt
				e.SeenUpdate, e.Stable = "", 0
			})
			if err != nil {
				// 已发送续跑消息。状态保存失败时结束本次尝试，避免重复发送。
				d.interruptSession(context.WithoutCancel(ctx), e)
				if !errors.Is(err, ErrStaleAttempt) && !errors.Is(err, ErrTaskNotFound) {
					d.finish(ctx, t, att, outcome{state: StateFailed, errClass: "infra_error", message: "保存会话续跑状态失败：" + err.Error()})
				}
			}
			return
		}
		msg += "。自动续跑失败：" + err.Error()
	}
	if class == "gateway_error" {
		d.invalidateProbe(e.MachineID, e.Launcher, s.TurnError)
	}
	d.finish(ctx, t, att, outcome{state: StateFailed, errClass: class, message: msg})
}

func (d *dispatcher) interruptSession(ctx context.Context, e *Execution) {
	ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = d.api.Interrupt(ictx, e.MachineID, e.SessionID)
}

func (d *dispatcher) timeoutAttempt(ctx context.Context, t *Task, att Attempt, timeout time.Duration) {
	d.interruptSession(ctx, t.Exec)
	d.finish(ctx, t, att, outcome{state: StateFailed, errClass: "timeout", message: fmt.Sprintf("Agent 超过 %d 分钟没有做完，已打断", int(timeout.Minutes()))})
}

// blockedReason finds the line the agent ends a reply with when it cannot go on.
func blockedReason(reply string) (string, bool) {
	lines := strings.Split(strings.TrimSpace(reply), "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-3; i-- {
		line := strings.Trim(strings.TrimSpace(lines[i]), "`*")
		if rest, ok := strings.CutPrefix(line, blockedMarker); ok {
			return firstLinePreview(strings.TrimSpace(rest), 200), true
		}
	}
	return "", false
}

// turnErrorClass sorts a failed turn by what another attempt could do about it. kind is the hub's own reading
// of the error (transient | user | usage_limit, often empty); the text decides the rest. Whole phrases only:
// "limit" or "auth" alone are in too many unrelated messages.
func turnErrorClass(kind, text string) string {
	s := strings.ToLower(text)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(s, w) {
				return true
			}
		}
		return false
	}
	switch {
	case kind == "usage_limit":
		return "rate_limited"
	case has("selected model", "invalid model", "model_not_found", "unknown model") ||
		has("model") && has("not exist", "not found", "have access", "no access", "not available", "not supported"):
		return "config_error" // the configured model is not there: every attempt fails the same way
	case has("401", "authentication", "not logged in", "login", "log in", "credential", "api key", "unauthorized"):
		return "auth_expired"
	case has("rate limit", "rate_limit", "ratelimit", "429", "quota", "usage limit", "too many requests"):
		return "rate_limited"
	case has("mid-response", "connection lost", "unexpected eof", "stream closed", "stream ended", "response.failed", "before its last event"):
		return "stream_interrupted"
	case has("502", "503", "504", "bad gateway", "gateway timeout", "service unavailable", "inference gateway",
		"connection refused", "connection reset", "no route to host", "dial tcp", "i/o timeout"):
		return "gateway_error"
	case has("overloaded"):
		return "rate_limited"
	}
	return "task_failed"
}

// waitingFor says what the session waits for the user with, from the hub's pending cards; known is false when
// the hub could not be asked.
func (d *dispatcher) waitingFor(ctx context.Context, e *Execution) (why string, known bool) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ixs, err := d.api.Interactions(cctx)
	if err != nil {
		return waitingInSession, false
	}
	for _, ix := range ixs {
		if ix.SessionID != e.SessionID || (ix.MachineID != "" && ix.MachineID != e.MachineID) {
			continue
		}
		if text := waitingText(ix); text != "" {
			return text, true
		}
	}
	return waitingInSession, true
}

// tryDeclineReadonlyPlan auto-declines ExitPlanMode prompts on unattended read-only tasks up to
// maxReadonlyPlanDeclines times per attempt so Claude Code outputs its report directly without stalling.
func (d *dispatcher) tryDeclineReadonlyPlan(ctx context.Context, t *Task, att Attempt, e *Execution) (declined, changed bool) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ixs, err := d.api.Interactions(cctx)
	cancel()
	if err != nil {
		return false, false
	}
	var target *hubInteraction
	for i := range ixs {
		ix := &ixs[i]
		if ix.SessionID != e.SessionID || (ix.MachineID != "" && ix.MachineID != e.MachineID) {
			continue
		}
		if ix.Kind == "plan" || ix.ToolName == "ExitPlanMode" {
			target = ix
			break
		}
	}
	if target == nil {
		return false, false
	}
	key, err := attemptKey(t.ID, att.AttemptNo)
	if err != nil {
		return false, false
	}
	d.mu.Lock()
	if d.planDeclines == nil {
		d.planDeclines = map[string]int{}
	}
	count := d.planDeclines[key]
	if count >= maxReadonlyPlanDeclines {
		d.mu.Unlock()
		return false, false
	}
	d.planDeclines[key] = count + 1
	d.mu.Unlock()

	no := false
	rctx, rcancel := context.WithTimeout(ctx, 10*time.Second)
	err = d.api.ResolveInteraction(rctx, target.ID, interactionResolution{
		Approve:  &no,
		Feedback: "This is an unattended read-only ATS task. Stay in plan mode; do not exit plan mode or write plan files. Output your final report directly.",
	})
	rcancel()
	if err != nil {
		return false, false
	}
	if e.Waiting != "" || e.JudgedUpdate != "" || e.Stable != 0 || e.SeenUpdate != "" {
		_, uerr := d.q.UpdateExec(t.ID, t.FenceToken, d.now(), func(ex *Execution) {
			ex.Waiting, ex.JudgedUpdate, ex.Stable, ex.SeenUpdate = "", "", 0, ""
		})
		return true, uerr == nil
	}
	return true, false
}

// waitingText is one line for a pending card: what to approve, or what is asked.
func waitingText(ix hubInteraction) string {
	what := firstLinePreview(ix.Summary, 120)
	switch ix.Kind {
	case "plan":
		return "Agent 提交了计划，等你批准"
	case "permission":
		tool := strings.TrimSpace(ix.ToolName)
		switch {
		case what == "" && tool == "":
			return ""
		case what == "":
			return "Agent 等你批准使用 " + tool
		case tool == "" || tool == "Bash":
			return "Agent 等你批准运行：" + what
		}
		return "Agent 等你批准 " + tool + "：" + what
	case "question":
		if len(ix.Questions) > 0 && strings.TrimSpace(ix.Questions[0].Question) != "" {
			what = firstLinePreview(ix.Questions[0].Question, 120)
		}
		if what == "" {
			return ""
		}
		return "Agent 在问你：" + what
	}
	return ""
}

func (d *dispatcher) setWaiting(t *Task, why, judged string) bool {
	if t.Exec.Waiting == why && t.Exec.JudgedUpdate == judged {
		return false
	}
	_, err := d.q.UpdateExec(t.ID, t.FenceToken, d.now(), func(e *Execution) {
		e.Waiting, e.JudgedUpdate, e.Stable = why, judged, 0
	})
	return err == nil
}

// startAcceptance: the agent is done. With no acceptance command the task ends unverified; with one, the
// computer's program runs it.
func (d *dispatcher) startAcceptance(ctx context.Context, t *Task, att Attempt, reply string) bool {
	t.Exec.LastReply, t.Exec.FullReply = agentSummary(reply), reply
	if t.Acceptance == nil || strings.TrimSpace(t.Acceptance.Command) == "" {
		d.finish(ctx, t, att, outcome{state: StateSucceeded, verdict: VerdictUnverified, message: reply, summary: agentSummary(reply), fullReply: reply})
		return true
	}
	key, _ := attemptKey(t.ID, att.AttemptNo)
	d.mu.Lock()
	delete(d.accepted, key)
	d.mu.Unlock()
	err := d.nodeCall(ctx, t.Exec.MachineID, "ats.accept", acceptParams{
		TaskID: t.ID, Attempt: att.AttemptNo, Command: t.Acceptance.Command, TimeoutSeconds: int(d.cfg().acceptTimeout().Seconds()),
	}, nil)
	if err != nil {
		d.finish(ctx, t, att, outcome{state: StateFailed, errClass: "infra_error", message: "验收命令没有跑起来：" + err.Error()})
		return true
	}
	_, err = d.q.UpdateExec(t.ID, t.FenceToken, d.now(), func(e *Execution) {
		e.Phase, e.Waiting, e.LastReply = PhaseAccepting, "", agentSummary(reply)
		e.FullReply = reply
	})
	return err == nil
}

// collect takes the acceptance command's result: the event the node program sent, or its answer when asked.
func (d *dispatcher) collect(ctx context.Context, t *Task, att Attempt) bool {
	e := t.Exec
	key, _ := attemptKey(t.ID, att.AttemptNo)
	d.mu.Lock()
	res, ok := d.accepted[key]
	delete(d.accepted, key)
	d.mu.Unlock()
	if !ok {
		if err := d.nodeCall(ctx, e.MachineID, "ats.accept_status", acceptParams{TaskID: t.ID, Attempt: att.AttemptNo}, &res); err != nil || !res.Done {
			if d.now().Sub(e.PhaseSince) > d.cfg().acceptTimeout()+5*time.Minute {
				d.finish(ctx, t, att, outcome{state: StateFailed, errClass: "timeout", message: "等不到验收命令的结果（电脑离线或程序重启）"})
				return true
			}
			return false
		}
	}
	code := res.ExitCode
	out := outcome{exitCode: &code, summary: e.LastReply, fullReply: e.FullReply, headSHA: res.HeadSHA, dirty: cloneBoolPtr(res.Dirty)}
	tail := strings.TrimSpace(res.OutputTail)
	switch {
	case res.TimedOut:
		out.state, out.verdict, out.errClass = StateFailed, VerdictFail, "timeout"
		out.message = fmt.Sprintf("验收命令超时（%d 秒）", res.DurationMS/1000)
	case res.Error != "":
		out.state, out.verdict, out.errClass = StateFailed, VerdictUnverified, "infra_error"
		out.message = "验收命令没有跑完：" + res.Error
	case res.ExitCode == 0:
		out.state, out.verdict = StateSucceeded, VerdictPass
		out.message = "验收命令退出码 0"
	default:
		out.state, out.verdict, out.errClass = StateFailed, VerdictFail, "task_failed"
		out.message = fmt.Sprintf("验收命令退出码 %d", res.ExitCode)
	}
	if tail != "" {
		out.message += "\n" + tail
	}
	if res.HeadSHA != "" {
		out.artifacts = append(out.artifacts, Artifact{Kind: "commit", Path: res.HeadSHA})
	}
	d.finish(ctx, t, att, out)
	return true
}

type outcome struct {
	state     string // succeeded | failed
	verdict   string
	errClass  string
	exitCode  *int
	message   string
	summary   string // the end of the agent's last reply
	fullReply string
	headSHA   string
	dirty     *bool
	artifacts []Artifact
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func classIf(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// finish ends the attempt: releases what it holds on the computer, records the outcome (the queue decides
// between another attempt and a final state) and writes a final outcome to the to-do.
func (d *dispatcher) finish(ctx context.Context, t *Task, att Attempt, out outcome) {
	if key, err := attemptKey(t.ID, att.AttemptNo); err == nil {
		d.mu.Lock()
		delete(d.planDeclines, key)
		d.mu.Unlock()
	}
	arts, releasedHeadSHA, relDirty := d.cleanup(ctx, t, att.AttemptNo, false)
	todoDetachPending := false
	if out.state != StateSucceeded && t.Exec != nil {
		todoDetachPending = !d.detachTodoSession(ctx, t.ID, att.AttemptNo, t.TodoID(), t.Exec.MachineID, t.Exec.SessionID)
	}
	out.headSHA = cmpOr(releasedHeadSHA, out.headSHA)
	// 验收发现的修改需要保留为证据，即使释放时已自动提交。
	if relDirty != nil && (out.dirty == nil || *relDirty) {
		out.dirty = boolPtr(*relDirty)
	}
	out.artifacts = mergeCommit(out.artifacts, releasedHeadSHA, t.Exec)
	if t.Exec != nil && t.Exec.SessionID != "" {
		arts = append(arts, Artifact{Kind: "session", Path: t.Exec.MachineID + "/" + t.Exec.SessionID})
	}
	if out.verdict == "" {
		out.verdict = VerdictUnverified
	}
	var retryable *bool
	switch out.errClass {
	case "rejected", "config_error", "auth_expired": // another attempt would fail the same way
		no := false
		retryable = &no
	}
	now := d.now()
	_ = d.q.RenewLeases(now)
	in := CompleteAttemptInput{
		TaskID: t.ID, AttemptNo: att.AttemptNo, FenceToken: att.FenceToken, WorkerID: att.WorkerID,
		Outcome: out.state, Verdict: out.verdict, ErrorClass: out.errClass, ExitCode: out.exitCode,
		Message: out.message, Summary: out.summary, FullReply: out.fullReply, HeadSHA: out.headSHA, Dirty: out.dirty,
		Artifacts: append(out.artifacts, arts...), Retryable: retryable, Now: now,
		TodoDetachPending: todoDetachPending,
	}
	if e := t.Exec; e != nil {
		in.Launcher, in.Model, in.SessionID, in.Worktree, in.BaseSHA = e.Launcher, e.Model, e.SessionID, e.Worktree, e.BaseSHA
		in.FullReply = cmpOr(in.FullReply, e.FullReply)
		in.Summary = cmpOr(in.Summary, e.LastReply)
	}
	done, err := d.q.CompleteAttempt(in)
	if err != nil || !done.IsTerminal() || d.writer == nil || !d.writer.HasTodosWriteScope() {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, _ = d.writer.WriteTaskOutcome(wctx, d.q, done.ID)
	cancel()
}

func (d *dispatcher) detachTodoSession(ctx context.Context, taskID string, attempt int, todoID, machineID, sessionID string) bool {
	if todoID == "" || machineID == "" || sessionID == "" {
		return true
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := d.api.DetachTodo(dctx, todoID, machineID, sessionID); err != nil {
		slog.Warn("ATS could not detach failed session from to-do", "task_id", taskID, "attempt", attempt,
			"todo_id", todoID, "machine_id", machineID, "session_id", sessionID, "error", err)
		return false
	}
	return true
}

func (d *dispatcher) retryPendingTodoDetaches(ctx context.Context) {
	for _, item := range d.q.PendingTodoDetaches() {
		if d.detachTodoSession(ctx, item.TaskID, item.AttemptNo, item.TodoID, item.MachineID, item.SessionID) {
			_ = d.q.SetAttemptTodoDetachPending(item.TaskID, item.AttemptNo, false)
		}
	}
}

// mergeCommit 优先采用 release 读取的最终 HEAD。只有偏离起点的提交进入证据。
func mergeCommit(own []Artifact, releasedHeadSHA string, e *Execution) []Artifact {
	base := ""
	if e != nil {
		base = e.BaseSHA
	}
	var out []Artifact
	for _, a := range own {
		if a.Kind == "commit" && (releasedHeadSHA != "" || (base != "" && a.Path == base)) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// releaseCommitMessage is the message of the commit ATS makes of work an attempt left uncommitted.
func releaseCommitMessage(t *Task, attempt int) string {
	prefix := "ATS " + t.ID + ": "
	room := 72 - len([]rune(prefix))
	title := firstLinePreview(t.Title, 200)
	if r := []rune(title); len(r) > room {
		title = string(r[:max(room-1, 0)]) + "…"
	}
	return fmt.Sprintf("%s%s\n\nUncommitted work of attempt %d, committed by ATS.", prefix, title, attempt)
}

// cleanup releases what an attempt holds on its computer: the worktree (kept when it has uncommitted work that
// could not be committed) and, when the attempt is being canceled, the agent's turn. An attempt of a writable
// task that ends by itself has its uncommitted work committed to its branch. It returns what stays behind as
// evidence.
func (d *dispatcher) cleanup(ctx context.Context, t *Task, attempt int, interrupt bool) (arts []Artifact, headSHA string, dirty *bool) {
	e := t.Exec
	if e == nil || e.MachineID == "" {
		return nil, "", nil
	}
	if interrupt && e.SessionID != "" {
		d.interruptSession(ctx, e)
	}
	if e.Worktree == "" {
		return nil, "", nil
	}
	msg := ""
	if !interrupt && t.PermissionProfile == PermissionDefault {
		msg = releaseCommitMessage(t, attempt)
	}
	var rel releaseResult
	if err := d.nodeCall(ctx, e.MachineID, "ats.release", releaseParams{
		TaskID: t.ID, Attempt: attempt, Repo: t.Workspace.Repo, BaseSHA: e.BaseSHA, CommitMessage: msg,
		Readonly: t.PermissionProfile != PermissionDefault,
	}, &rel); err != nil {
		slog.Warn("ATS could not release worktree", "task_id", t.ID, "attempt", attempt, "error", err)
		return []Artifact{{Kind: "worktree", Path: e.Worktree}}, "", nil
	}
	headSHA = strings.TrimSpace(rel.HeadSHA)
	if headSHA != "" && headSHA != e.BaseSHA {
		arts = append(arts, Artifact{Kind: "commit", Path: headSHA})
	}
	if rel.Branch != "" {
		arts = append(arts, Artifact{Kind: "branch", Path: rel.Branch})
	}
	if rel.Kept != "" {
		arts = append(arts, Artifact{Kind: "worktree", Path: rel.Kept})
	}
	return arts, headSHA, rel.Dirty
}

// cancel cancels a task from the queue page: the queue first, then what its attempt holds.
func (d *dispatcher) cancel(ctx context.Context, taskID string) (*Task, error) {
	before, t, canceled, err := d.q.cancelTaskWithSnapshot(taskID, d.now())
	if err != nil {
		return nil, err
	}
	if !canceled {
		return t, nil
	}
	if before.Exec != nil && len(before.Attempts) > 0 {
		attemptNo := before.Attempts[len(before.Attempts)-1].AttemptNo
		d.cleanup(ctx, before, attemptNo, true)
		if !d.detachTodoSession(ctx, before.ID, attemptNo, before.TodoID(), before.Exec.MachineID, before.Exec.SessionID) {
			_ = d.q.SetAttemptTodoDetachPending(before.ID, attemptNo, true)
		}
	}
	if d.writer != nil && d.writer.HasTodosWriteScope() {
		_, _ = d.writer.WriteTaskOutcome(ctx, d.q, t.ID)
	}
	return t, nil
}
