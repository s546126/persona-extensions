package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The extension's HTTP API, which the hub serves at /api/v1/extensions/<id>/…:
//
//	GET  /tasks?state=&todo_id=&priority=&limit=   the queue, newest first, with what the scheduler is doing: each
//	     &after=&view=full                         task in short (taskBrief) unless view=full; next is the after
//	                                               of the next page when there is one
//	GET  /tasks/{id}[?view=full]                   one task: a long prompt is cut to its beginning and each attempt
//	                                               to what tells how it went; view=full has all of both
//	GET  /tasks/{id}/evidence[?attempt=N]          what shows whether the task is really done: the conclusion, then
//	                                               the last report with the end of its output; attempt=N has the
//	                                               whole report of that attempt
//	POST /tasks                                    queue a task for a to-do
//
// The assistant reaches it through the gated command of assistant/gate.json: reads pass, POST /tasks asks the
// user every time, everything else is refused before it gets here.

var todoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,120}$`)

const (
	maxPromptBytes     = 256 << 10
	maxAcceptanceBytes = 2 << 10
	maxBodyBytes       = 512 << 10
)

type apiError struct {
	Status int    `json:"-"`
	Code   string `json:"code"`
	Msg    string `json:"error"`
}

func (e *apiError) Error() string { return e.Msg }

func badRequest(code, format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: code, Msg: fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	var ae *apiError
	var adm *AdmissionError
	switch {
	case errors.As(err, &ae):
		writeJSON(w, ae.Status, ae)
	case errors.As(err, &adm):
		writeJSON(w, http.StatusTooManyRequests, &apiError{Code: adm.Code, Msg: adm.Message})
	case errors.Is(err, ErrBadCursor):
		writeJSON(w, http.StatusBadRequest, &apiError{Code: "invalid_after", Msg: "after must be the next of the page before"})
	case errors.Is(err, ErrTaskNotFound):
		writeJSON(w, http.StatusNotFound, &apiError{Code: "not_found", Msg: "no such task"})
	default:
		writeJSON(w, http.StatusBadRequest, &apiError{Code: "invalid", Msg: err.Error()})
	}
}

func (a *atsApp) q() *Queue {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.queue
}

func (a *atsApp) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tasks", a.listTasks)
	mux.HandleFunc("GET /tasks/{id}", a.getTask)
	mux.HandleFunc("GET /tasks/{id}/evidence", a.getEvidence)
	mux.HandleFunc("POST /tasks", a.postTask)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.q() == nil {
			writeJSON(w, http.StatusServiceUnavailable, &apiError{Code: "not_ready", Msg: "the queue is not ready"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// taskView is a task as the API shows it.
type taskView struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	State         string     `json:"state"`
	Verdict       string     `json:"verdict"`
	FailureReason string     `json:"failure_reason,omitempty"`
	TodoID        string     `json:"todo_id,omitempty"`
	Priority      string     `json:"priority"`
	Difficulty    string     `json:"difficulty"`
	ModelTier     string     `json:"model_tier"`
	Machine       string     `json:"machine,omitempty"`
	AgentType     string     `json:"agent_type"`
	Permission    string     `json:"permission_profile"`
	Repo          string     `json:"repo"`
	Ref           string     `json:"ref"`
	Acceptance    string     `json:"acceptance,omitempty"`
	AttemptNo     int        `json:"attempt_no"`
	RetriesLeft   int        `json:"retries_left"`
	Phase         string     `json:"phase,omitempty"`
	RunsOn        string     `json:"runs_on,omitempty"`
	SessionID     string     `json:"session_id,omitempty"`
	Launcher      string     `json:"launcher,omitempty"`
	Model         string     `json:"model,omitempty"`
	Worktree      string     `json:"worktree,omitempty"`
	Waiting       string     `json:"waiting,omitempty"`
	NotBefore     *time.Time `json:"not_before,omitempty"`
	SubmittedAt   time.Time  `json:"submitted_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Prompt        string     `json:"prompt,omitempty"`
	// PromptBytes is the length of the whole prompt, set when Prompt is only its beginning.
	PromptBytes int `json:"prompt_bytes,omitempty"`

	TodoAttachError string `json:"todo_attach_error,omitempty"`
}

// How much of the task an answer carries.
const (
	promptNone = iota // the list and the answer to POST /tasks
	promptHead        // GET /tasks/{id}: the beginning of a long prompt, and its length
	promptFull        // GET /tasks/{id}?view=full
)

// promptHeadRunes is how much of a prompt GET /tasks/{id} shows by default: enough to tell which task it is and
// what it asks for. The rest costs every reader of a task its whole brief, which the reader usually wrote.
const promptHeadRunes = 240

func viewTask(t *Task, prompt int) taskView {
	v := taskView{
		ID: t.ID, Title: t.Title, State: t.State, Verdict: t.Verdict, TodoID: t.TodoID(),
		Priority: t.Priority, Difficulty: t.Difficulty, ModelTier: t.ModelTier, Machine: t.Machine,
		AgentType: effectiveAgentType(t.AgentType), Permission: t.PermissionProfile, Repo: t.Workspace.Repo, Ref: t.Workspace.BaseSHA,
		AttemptNo: t.AttemptNo, RetriesLeft: t.RetriesLeft, SubmittedAt: t.SubmittedAt, FinishedAt: t.FinishedAt,
	}
	// How the previous attempt failed says nothing about the one running now.
	if t.State == StateFailed || t.State == StateQueued {
		v.FailureReason = t.FailureReason
	}
	if t.State == StateQueued {
		v.NotBefore = t.NotBefore
	}
	if t.Acceptance != nil {
		v.Acceptance = t.Acceptance.Command
	}
	if n := len(t.Attempts); n > 0 {
		v.TodoAttachError = t.Attempts[n-1].TodoAttachError
	}
	if e := t.Exec; e != nil {
		v.Phase, v.RunsOn, v.SessionID, v.Waiting = e.Phase, e.MachineID, e.SessionID, e.Waiting
		v.Launcher, v.Model, v.Worktree = e.Launcher, e.Model, e.Worktree
	} else {
		if n := len(t.Attempts); n > 0 {
			att := t.Attempts[n-1]
			v.Launcher, v.Model, v.SessionID, v.Worktree = att.Launcher, att.Model, att.SessionID, att.Worktree
		}
		if n := len(t.Reports); n > 0 {
			rep := t.Reports[n-1]
			if len(t.Attempts) == 0 || rep.AttemptNo == t.Attempts[len(t.Attempts)-1].AttemptNo {
				v.Launcher, v.Model = cmpOr(v.Launcher, rep.Launcher), cmpOr(v.Model, rep.Model)
				v.SessionID, v.Worktree = cmpOr(v.SessionID, rep.SessionID), cmpOr(v.Worktree, rep.Worktree)
			}
		}
	}
	v.Launcher, v.Model = cmpOr(v.Launcher, t.Launcher), cmpOr(v.Model, t.Model)
	switch prompt {
	case promptFull:
		v.Prompt = t.Prompt
	case promptHead:
		if v.Prompt = headRunes(t.Prompt, promptHeadRunes); v.Prompt != strings.TrimSpace(t.Prompt) {
			v.PromptBytes = len(t.Prompt)
		}
	}
	return v
}

// attemptView is an attempt as GET /tasks/{id} shows it by default: when it ran, where, and how it ended. The
// lease and fence bookkeeping, and the ids the task already gives, are in view=full.
type attemptView struct {
	AttemptNo  int        `json:"attempt_no"`
	State      string     `json:"state"`
	Machine    string     `json:"machine,omitempty"`
	ModelTier  string     `json:"model_tier,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Dirty      *bool      `json:"dirty,omitempty"`
	ErrorClass string     `json:"error_class,omitempty"`
	Verdict    string     `json:"verdict,omitempty"`
	Summary    string     `json:"summary,omitempty"`

	TodoAttachError string `json:"todo_attach_error,omitempty"`
}

func viewAttempts(atts []Attempt) []attemptView {
	out := make([]attemptView, 0, len(atts))
	for _, a := range atts {
		out = append(out, attemptView{AttemptNo: a.AttemptNo, State: a.State, Machine: a.Machine, ModelTier: a.ModelTier,
			StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, ExitCode: a.ExitCode, Dirty: a.Dirty, ErrorClass: a.ErrorClass,
			Verdict: a.Verdict, Summary: a.Summary, TodoAttachError: a.TodoAttachError})
	}
	return out
}

// taskBrief is a task as the list shows it by default: which task it is, how far along, and what it waits for.
// A field at its default is left out (priority p2, permission_profile readonly, agent_type claude, a first
// attempt), and so is the verdict of a task that has not succeeded, which says nothing yet. What the task was
// asked and where it runs (repo, ref, acceptance, difficulty, session) is in GET /tasks/{id}.
type taskBrief struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	State         string     `json:"state"`
	Verdict       string     `json:"verdict,omitempty"`
	FailureReason string     `json:"failure_reason,omitempty"`
	TodoID        string     `json:"todo_id,omitempty"`
	Priority      string     `json:"priority,omitempty"`
	Permission    string     `json:"permission_profile,omitempty"`
	AgentType     string     `json:"agent_type,omitempty"`
	AttemptNo     int        `json:"attempt_no,omitempty"`
	Phase         string     `json:"phase,omitempty"`
	RunsOn        string     `json:"runs_on,omitempty"`
	Waiting       string     `json:"waiting,omitempty"`
	NotBefore     *time.Time `json:"not_before,omitempty"`
	SubmittedAt   time.Time  `json:"submitted_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

// briefTask is the short form of v. withTodo is false for a list of one to-do's tasks, which all have its id.
func briefTask(v taskView, withTodo bool) taskBrief {
	b := taskBrief{ID: v.ID, Title: v.Title, State: v.State, FailureReason: v.FailureReason, Phase: v.Phase, RunsOn: v.RunsOn,
		Waiting: v.Waiting, NotBefore: v.NotBefore, SubmittedAt: v.SubmittedAt, FinishedAt: v.FinishedAt}
	if v.State == StateSucceeded || v.Verdict != VerdictUnverified {
		b.Verdict = v.Verdict
	}
	if withTodo {
		b.TodoID = v.TodoID
	}
	if v.Priority != PriorityP2 {
		b.Priority = v.Priority
	}
	if v.Permission != PermissionReadonly {
		b.Permission = v.Permission
	}
	if v.AgentType != AgentClaude {
		b.AgentType = v.AgentType
	}
	if v.AttemptNo > 1 {
		b.AttemptNo = v.AttemptNo
	}
	return b
}

func (a *atsApp) listTasks(w http.ResponseWriter, r *http.Request) {
	q := a.q()
	query := r.URL.Query()
	f := TaskFilter{TodoID: strings.TrimSpace(query.Get("todo_id")), Priority: query.Get("priority"), Limit: 20}
	if s := strings.TrimSpace(query.Get("state")); s != "" {
		for _, st := range strings.Split(s, ",") {
			st = strings.TrimSpace(st)
			if !matchesStateFilter(st, AllTaskStates) {
				writeError(w, badRequest("invalid_state", "state must be one of %s", strings.Join(AllTaskStates, ", ")))
				return
			}
			f.States = append(f.States, st)
		}
	}
	if s := query.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 200 {
			writeError(w, badRequest("invalid_limit", "limit must be 1 to 200"))
			return
		}
		f.Limit = n
	}
	full := false
	switch query.Get("view") {
	case "":
	case "full":
		full = true
	default:
		writeError(w, badRequest("invalid_view", "view must be full or left out"))
		return
	}
	f.After = strings.TrimSpace(query.Get("after"))
	limit := f.Limit
	f.Limit++ // one more, to know whether there is a next page
	tasks, err := q.ListTasks(f)
	if err != nil {
		writeError(w, err)
		return
	}
	out := map[string]any{}
	if len(tasks) > limit {
		tasks = tasks[:limit]
		out["next"] = taskCursor(tasks[limit-1])
	}
	views := make([]any, 0, len(tasks))
	for _, t := range tasks {
		if v := viewTask(t, promptNone); full {
			views = append(views, v)
		} else {
			views = append(views, briefTask(v, f.TodoID == ""))
		}
	}
	plan := q.PlanSchedule(a.now())
	out["tasks"] = views
	out["scheduler"] = map[string]any{
		"admission":           plan.AdmissionMode,
		"admission_label":     plan.AdmissionLabel,
		"quota_remaining_pct": plan.Quota.RemainingPct,
		"counts_by_state":     plan.CountsByState,
		"queued_by_priority":  plan.QueuedByPriority,
		"computers":           len(plan.Machines),
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *atsApp) getTask(w http.ResponseWriter, r *http.Request) {
	t, err := a.q().GetTask(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	switch r.URL.Query().Get("view") {
	case "":
		writeJSON(w, http.StatusOK, map[string]any{"task": viewTask(t, promptHead), "attempts": viewAttempts(t.Attempts)})
	case "full":
		writeJSON(w, http.StatusOK, map[string]any{"task": viewTask(t, promptFull), "attempts": t.Attempts})
	default:
		writeError(w, badRequest("invalid_view", "view must be full or left out"))
	}
}

// evidenceTailRunes is how much of a report's output GET /tasks/{id}/evidence shows: the end of an acceptance
// command's output is where it says what failed.
const evidenceTailRunes = 600

// evidenceView answers "is it really done", the conclusion first: whether the outcome is verified, the state and
// verdict, then the acceptance command, whether the outcome was written to the to-do, and the report behind it.
type evidenceView struct {
	Verified        bool            `json:"verified"`
	State           string          `json:"state"`
	Verdict         string          `json:"verdict"`
	FailureReason   string          `json:"failure_reason,omitempty"`
	Attempts        int             `json:"attempts"`
	TaskID          string          `json:"task_id"`
	TodoID          string          `json:"todo_id,omitempty"`
	Acceptance      string          `json:"acceptance,omitempty"`
	DecisionRef     string          `json:"decision_ref,omitempty"`
	DecisionSkipped string          `json:"decision_skipped,omitempty"`
	Launcher        string          `json:"launcher,omitempty"`
	Model           string          `json:"model,omitempty"`
	SessionID       string          `json:"session_id,omitempty"`
	Worktree        string          `json:"worktree,omitempty"`
	TodoAttachError string          `json:"todo_attach_error,omitempty"`
	LastReport      *reportView     `json:"last_report,omitempty"`
	Report          *EvidenceReport `json:"report,omitempty"` // attempt=N: that attempt's report as stored
}

// reportView is the last report as the evidence shows it by default: how the attempt ended, what its agent said
// and left behind, then the end of the output. The ids the answer already gives are left out.
type reportView struct {
	AttemptNo  int        `json:"attempt_no"`
	Outcome    string     `json:"outcome"`
	Verdict    string     `json:"verdict,omitempty"`
	ErrorClass string     `json:"error_class,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Dirty      *bool      `json:"dirty,omitempty"`
	Summary    string     `json:"summary,omitempty"`
	Artifacts  []Artifact `json:"artifacts,omitempty"`
	Message    string     `json:"message,omitempty"`
	// MessageBytes is the length of the whole message, set when Message is cut or left out.
	MessageBytes int       `json:"message_bytes,omitempty"`
	ReceivedAt   time.Time `json:"received_at"`

	TodoAttachError string `json:"todo_attach_error,omitempty"`
}

func viewReport(r EvidenceReport) *reportView {
	v := &reportView{AttemptNo: r.AttemptNo, Outcome: r.Outcome, Verdict: r.Verdict, ErrorClass: r.ErrorClass, ExitCode: r.ExitCode,
		Dirty: r.Dirty, Summary: r.Summary, Artifacts: r.Artifacts, ReceivedAt: r.ReceivedAt, TodoAttachError: r.TodoAttachError}
	// With no acceptance command the message of a success is the agent's whole reply, which the summary is taken from.
	if !(r.Outcome == StateSucceeded && r.ExitCode == nil && strings.TrimSpace(r.Summary) != "") {
		v.Message = outputEnd(r.Message, evidenceTailRunes)
	}
	if v.Message != strings.TrimSpace(r.Message) {
		v.MessageBytes = len(r.Message)
	}
	return v
}

// getEvidence answers "is it really done" (evidenceView). attempt=N answers with the whole report of that attempt
// in place of the last report's short form.
func (a *atsApp) getEvidence(w http.ResponseWriter, r *http.Request) {
	t, err := a.q().GetTask(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	out := evidenceView{
		Verified: t.State == StateSucceeded && t.Verdict == VerdictPass, State: t.State, Verdict: t.Verdict, FailureReason: t.FailureReason,
		Attempts: len(t.Attempts), TaskID: t.ID, TodoID: t.TodoID(), DecisionRef: t.DecisionWrittenRef,
	}
	if t.Acceptance != nil {
		out.Acceptance = t.Acceptance.Command
	}
	if out.DecisionRef == "" {
		out.DecisionSkipped = t.DecisionSkipReason
	}
	if s := r.URL.Query().Get("attempt"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			writeError(w, badRequest("invalid_attempt", "attempt must be the number of an attempt, from 1"))
			return
		}
		for i := len(t.Reports) - 1; i >= 0 && out.Report == nil; i-- {
			if t.Reports[i].AttemptNo == n {
				out.Report = &t.Reports[i]
			}
		}
		if out.Report == nil {
			writeJSON(w, http.StatusNotFound, &apiError{Code: "no_report", Msg: fmt.Sprintf("attempt %d of this task has no report (it has %d attempts)", n, len(t.Attempts))})
			return
		}
	} else {
		if len(t.Attempts) > 0 || len(t.Reports) > 0 {
			v := viewTask(t, promptNone)
			out.Launcher, out.Model, out.SessionID, out.Worktree = v.Launcher, v.Model, v.SessionID, v.Worktree
			out.TodoAttachError = v.TodoAttachError
		}
		if n := len(t.Reports); n > 0 {
			out.LastReport = viewReport(t.Reports[n-1])
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type enqueueRequest struct {
	TodoID            string            `json:"todo_id"`
	Title             string            `json:"title"`
	Prompt            string            `json:"prompt"`
	Repo              string            `json:"repo"`
	Ref               string            `json:"ref"`
	Priority          string            `json:"priority"`
	Difficulty        string            `json:"difficulty"`
	QoS               string            `json:"qos"`
	Pool              string            `json:"pool"`
	Machine           string            `json:"machine"`
	AgentType         string            `json:"agent_type"`
	Model             string            `json:"model"`
	Launcher          string            `json:"launcher"`
	Acceptance        string            `json:"acceptance"`
	PermissionProfile string            `json:"permission_profile"`
	Tags              []string          `json:"tags"`
	Labels            map[string]string `json:"labels"`
	IdempotencyKey    string            `json:"idempotency_key"`
}

func (a *atsApp) postTask(w http.ResponseWriter, r *http.Request) {
	// Who may queue: the owner's apps, and the assistant's gated command (which asked the user first). Other
	// callers the hub lets through to an extension's API (an answer-only token, a share) may read.
	switch r.Header.Get("X-Persona-Role") {
	case "", "full", "extension":
	default:
		writeJSON(w, http.StatusForbidden, &apiError{Code: "forbidden", Msg: "this caller may not queue tasks"})
		return
	}
	req := enqueueRequest{Ref: "HEAD"}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, badRequest("invalid_body", "the body must be a JSON object of the task's fields: %v", err))
		return
	}
	in, err := a.enqueueInput(req)
	if err != nil {
		writeError(w, err)
		return
	}
	res, err := a.q().Enqueue(in)
	if err != nil {
		writeError(w, err)
		return
	}
	a.mu.Lock()
	d := a.disp
	a.mu.Unlock()
	if d != nil {
		d.poke()
	}
	status := http.StatusCreated
	if !res.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"created": res.Created, "task": viewTask(res.Task, promptNone)})
}

// enqueueInput checks a request to queue a task. A task always belongs to a to-do, runs in a repository under a
// directory the settings allow, and is read-only unless it says otherwise.
func (a *atsApp) enqueueInput(req enqueueRequest) (EnqueueInput, error) {
	todo := strings.TrimSpace(req.TodoID)
	if !todoIDPattern.MatchString(todo) {
		return EnqueueInput{}, badRequest("invalid_todo_id", "todo_id is required: the id of the to-do the task is for")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return EnqueueInput{}, badRequest("invalid_prompt", "prompt is required: what the agent is to do")
	}
	if len(req.Prompt) > maxPromptBytes {
		return EnqueueInput{}, badRequest("invalid_prompt", "prompt is longer than %d bytes", maxPromptBytes)
	}
	if len(req.Acceptance) > maxAcceptanceBytes || strings.ContainsAny(req.Acceptance, "\x00") {
		return EnqueueInput{}, badRequest("invalid_acceptance", "acceptance must be one shell command of at most %d bytes", maxAcceptanceBytes)
	}
	repo, err := checkRepo(req.Repo, a.config().Repos)
	if err != nil {
		return EnqueueInput{}, err
	}
	if err := validateRef(req.Ref); err != nil {
		return EnqueueInput{}, badRequest("invalid_ref", invalidRefMessage)
	}
	switch strings.TrimSpace(req.QoS) {
	case "", "expedited", "standard", "bulk":
	default:
		return EnqueueInput{}, badRequest("invalid_qos", "qos must be expedited, standard or bulk")
	}
	for k := range req.Labels {
		if k == PersonaTodoLabel {
			return EnqueueInput{}, badRequest("invalid_labels", "the %s label is set from todo_id", PersonaTodoLabel)
		}
	}
	for _, tag := range req.Tags {
		if k, _, _ := strings.Cut(tag, "="); strings.TrimSpace(k) == PersonaTodoLabel {
			return EnqueueInput{}, badRequest("invalid_tags", "the %s label is set from todo_id", PersonaTodoLabel)
		}
	}
	return EnqueueInput{
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey), Title: strings.TrimSpace(req.Title), Prompt: req.Prompt,
		Repo: repo, BaseSHA: req.Ref, QoS: strings.TrimSpace(req.QoS), Pool: strings.TrimSpace(req.Pool),
		Priority: req.Priority, Difficulty: req.Difficulty, Machine: req.Machine, AgentType: req.AgentType,
		Model: req.Model, Launcher: req.Launcher,
		AcceptanceCommand: req.Acceptance, PermissionProfile: req.PermissionProfile, Labels: req.Labels, Tags: req.Tags,
		Source:      AssistantSource,
		ExternalRef: &ExternalRef{System: AssistantSource, ID: PersonaSystemID, Conversation: todo},
		Now:         a.now(),
	}, nil
}

// checkRepo is the hub program's part of the repository allowlist: the path as written must be under one of the
// allowed directories as written. The program on the computer checks again with the real paths (symlinks, the
// home directory), and its answer is the one that counts; this one refuses the plainly wrong early, with a
// message the assistant can show.
func checkRepo(raw string, allowed []string) (string, error) {
	repo := strings.TrimSpace(raw)
	if repo == "" || !(strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, "~/")) || strings.ContainsRune(repo, 0) {
		return "", badRequest("invalid_repo", "repo is required: the repository's absolute path on the computer (or ~/…)")
	}
	for _, seg := range strings.Split(repo, "/") {
		if seg == ".." {
			return "", badRequest("invalid_repo", "repo must not contain ..")
		}
	}
	repo = path.Clean(repo)
	if len(allowed) == 0 {
		return "", &apiError{Status: http.StatusForbidden, Code: "repos_not_configured",
			Msg: "no repository directory is allowed yet: the user adds the directories tasks may run in to the extension's settings (repos)"}
	}
	home := strings.HasPrefix(repo, "~/")
	undecided := false
	for _, root := range allowed {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		root = path.Clean(root)
		if strings.HasPrefix(root, "~") != home {
			undecided = true // one is written from the home directory, the other is not: only the computer can tell
			continue
		}
		if repo == root || strings.HasPrefix(repo, strings.TrimRight(root, "/")+"/") {
			return repo, nil
		}
	}
	if undecided {
		return repo, nil
	}
	return "", &apiError{Status: http.StatusForbidden, Code: "repo_not_allowed",
		Msg: "the repository is not under a directory in the extension's settings (repos)"}
}
