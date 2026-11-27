package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const queueUI = "queue"

// cardUI is the card on the Overview page; a tap on it opens queueUI (persona-extension.json "opens").
const cardUI = "queue-card"

// cardTitle is the card's title (persona-extension.json) and its header on Overview. The hub's settings page
// lists every UI contribution by its title, so the card's says 「概况」 to tell it from the home page's.
const cardTitle = extName + " · 概况"

// ExtConfig is the extension's settings (configuration in persona-extension.json). Both programs read it: the
// hub program all of it, the program on a computer the repository allowlist.
type ExtConfig struct {
	Machines               []string `json:"machines"`
	Repos                  []string `json:"repos"`
	ParallelPerMachine     int      `json:"parallel_per_machine"`
	ReadonlyPermissionMode string   `json:"readonly_permission_mode"`
	WritePermissionMode    string   `json:"write_permission_mode"`
	TaskTimeoutMinutes     int      `json:"task_timeout_minutes"`
	AcceptTimeoutMinutes   int      `json:"acceptance_timeout_minutes"`
	MaxQueued              int      `json:"max_queued"`
	MaxRetries             int      `json:"max_retries"`
	ReserveHeadroomPct     int      `json:"reserve_headroom_pct"`
	Launcher               string   `json:"launcher"`
	ModelFast              string   `json:"model_fast"`
	ModelStandard          string   `json:"model_standard"`
	ModelStrong            string   `json:"model_strong"`
}

func defaultExtConfig() ExtConfig {
	return ExtConfig{
		ParallelPerMachine: 1, ReadonlyPermissionMode: "plan", WritePermissionMode: "acceptEdits",
		TaskTimeoutMinutes: 120, AcceptTimeoutMinutes: 10,
		MaxQueued: 100, MaxRetries: 2, ReserveHeadroomPct: 30,
	}
}

func (c ExtConfig) parallel() int { return min(max(c.ParallelPerMachine, 1), 4) }

func (c ExtConfig) readonlyMode() string {
	if c.ReadonlyPermissionMode == "bypassPermissions" {
		return "bypassPermissions"
	}
	return "plan"
}

// writeMode is the permission mode of the session of a task that may write.
func (c ExtConfig) writeMode() string {
	switch c.WritePermissionMode {
	case "default", "acceptEdits", "auto", "bypassPermissions":
		return c.WritePermissionMode
	}
	return "acceptEdits"
}

func (c ExtConfig) taskTimeout() time.Duration {
	m := c.TaskTimeoutMinutes
	if m <= 0 {
		m = 120
	}
	return time.Duration(min(m, 24*60)) * time.Minute
}

func (c ExtConfig) acceptTimeout() time.Duration {
	m := c.AcceptTimeoutMinutes
	if m <= 0 {
		return defaultAcceptTime
	}
	return min(time.Duration(m)*time.Minute, maxAcceptTime)
}

// model is the model a session of this tier asks for; empty leaves the agent's own default.
func (c ExtConfig) model(tier string) string {
	switch tier {
	case TierFast:
		return strings.TrimSpace(c.ModelFast)
	case TierStrong:
		return strings.TrimSpace(c.ModelStrong)
	}
	return strings.TrimSpace(c.ModelStandard)
}

func (c ExtConfig) launcherFor(machineID string) string {
	return launcherForSpec(c.Launcher, machineID)
}

func (c ExtConfig) launchersFor(machineID string) []string {
	return launchersForSpec(c.Launcher, machineID)
}

func launcherForSpec(spec, machineID string) string {
	if candidates := launchersForSpec(spec, machineID); len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

func launchersForSpec(spec, machineID string) []string {
	target := strings.TrimSpace(machineID)
	parts := strings.FieldsFunc(spec, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n'
	})
	var machine, defaults []string
	for _, entry := range parts {
		if k, v, ok := strings.Cut(entry, "="); ok {
			if target != "" && strings.EqualFold(strings.TrimSpace(k), target) {
				machine = append(machine, v)
			}
		} else {
			defaults = append(defaults, entry)
		}
	}
	var candidates []string
	seen := map[string]bool{}
	for _, entry := range append(machine, defaults...) {
		for _, name := range strings.Split(entry, "|") {
			name = strings.TrimSpace(name)
			if name != "" && !seen[name] {
				seen[name] = true
				candidates = append(candidates, name)
			}
		}
	}
	return candidates
}

func (c ExtConfig) queueConfig() QueueConfig {
	q := DefaultQueueConfig()
	if c.MaxQueued > 0 {
		q.MaxQueued = c.MaxQueued
	}
	if c.MaxRetries >= 0 {
		q.MaxRetries = c.MaxRetries
	}
	if c.ReserveHeadroomPct > 0 {
		q.ReserveHeadroomPct = float64(c.ReserveHeadroomPct)
	}
	return q
}

// atsApp is the hub program: the queue, its dispatcher, the queue page and the HTTP API the assistant calls.
type atsApp struct {
	mu      sync.Mutex
	id      string
	cfg     ExtConfig
	queue   *Queue
	writer  *DecisionWriter
	disp    *dispatcher
	hub     *ext.Hub
	hubURL  string
	token   string
	nowFunc func() time.Time
	// api and nodes are set by tests; init makes the real ones.
	api   hubAPI
	nodes nodeCaller
	// draft is what the user picked on the home page before setup and has not saved in the settings yet.
	draft setupDraft
}

func newATSApp(q *Queue) *atsApp {
	return &atsApp{id: "ats", cfg: defaultExtConfig(), queue: q, nowFunc: func() time.Time { return time.Now().UTC() }}
}

func (a *atsApp) now() time.Time { return a.nowFunc() }

func (a *atsApp) config() ExtConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *atsApp) program(id, version string) ext.Program {
	a.id = id
	return ext.Program{
		ID:       id,
		Version:  version,
		Manifest: manifest,
		Init:     a.init,
		HTTP:     a.handler(),
		UI:       map[string]ext.Page{queueUI: {Render: a.render, Action: a.action}, cardUI: {Render: a.render}},
		Namespaces: map[string]ext.Handler{
			"assistant": a.assistant,
			"node":      a.nodeEvent,
		},
	}
}

func (a *atsApp) init(ctx context.Context, in *ext.Init, hub *ext.Hub) (map[string]any, error) {
	c := defaultExtConfig()
	if err := in.Config(&c); err != nil {
		return nil, err
	}
	dataDir := strings.TrimSpace(in.Info().DataDir)
	if dataDir == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		dataDir = filepath.Join(dir, "persona-ats")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg, a.hub, a.hubURL, a.token = c, hub, strings.TrimRight(in.Hub.URL, "/"), in.Hub.Token
	if a.queue == nil {
		q, err := NewQueue(dataDir, c.queueConfig())
		if err != nil {
			return nil, err
		}
		a.queue = q
	} else {
		a.queue.UpdateConfig(c.queueConfig())
	}
	if a.writer == nil {
		a.writer = NewDecisionWriter(in.Hub.URL, in.Hub.Token, in.Hub.Scopes, &http.Client{Timeout: 10 * time.Second})
	} else {
		a.writer.Configure(in.Hub.URL, in.Hub.Token, in.Hub.Scopes)
	}
	if a.disp != nil {
		a.disp.poke()
		return nil, nil
	}
	// Saving the settings restarts the program. Once it is up, ask the apps to draw the pages again, so that a
	// home page left open on its setup steps becomes the usual one without waiting for its next refresh.
	if hub != nil {
		go func() {
			time.Sleep(time.Second) // after the hub has its answer to initialize
			_ = hub.Invalidate(queueUI)
			_ = hub.Invalidate(cardUI)
		}()
	}
	if a.api == nil {
		a.api = newHubClient(in.Hub.URL, in.Hub.Token)
	}
	if a.nodes == nil {
		a.nodes = rpcNodes{hub: hub}
	}
	a.disp = a.newDispatcher()
	go a.disp.run(context.Background()) // ends with the process: the hub stops the program by closing stdin
	return nil, nil
}

func (a *atsApp) newDispatcher() *dispatcher {
	d := newDispatcher(a.queue, a.api, a.nodes, a.writer, a.config, a.now)
	d.refresh = a.refreshQuota
	d.changed = func() {
		a.mu.Lock()
		hub := a.hub
		a.mu.Unlock()
		if hub != nil {
			_ = hub.Invalidate(queueUI)
			_ = hub.Invalidate(cardUI)
		}
	}
	return d
}

// refreshQuota reads the usage windows of the hub's gateway accounts, which decide what the scheduler admits.
func (a *atsApp) refreshQuota(ctx context.Context) {
	a.mu.Lock()
	hub, q := a.hub, a.queue
	a.mu.Unlock()
	if hub == nil || q == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var ga GenericGatewayAccounts
	if err := hub.Call(cctx, "usage.gateway_accounts", map[string]any{}, &ga); err == nil && (len(ga.Accounts) > 0 || len(ga.Sources) > 0) {
		_ = q.SetQuotaStatus(ParseGatewayAccounts(ga, a.now()))
	}
}

// assistant answers assistant.credentials: what the assistant's gated command needs to reach this program's HTTP
// API. The token is the extension's own, whose scopes include calling this extension and nothing wider of the
// API; the gate keeps it out of the agent's sight.
func (a *atsApp) assistant(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if method != "credentials" {
		return nil, ext.UnknownMethod("assistant." + method)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hubURL == "" || a.token == "" {
		return nil, errors.New("the hub gave no API address or token (permissions.hub not granted)")
	}
	return map[string]any{"env": map[string]string{
		"ATS_BASE":  a.hubURL + "/api/v1/extensions/" + a.id,
		"ATS_TOKEN": a.token,
	}}, nil
}

// nodeEvent takes what the program on a computer sends: the result of an acceptance command.
func (a *atsApp) nodeEvent(ctx context.Context, method string, params json.RawMessage) (any, error) {
	var ev struct {
		Event  string          `json:"event"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(params, &ev); err != nil {
		return nil, err
	}
	a.mu.Lock()
	d := a.disp
	a.mu.Unlock()
	if ev.Event == "ats.accepted" && d != nil {
		var res acceptResult
		if err := json.Unmarshal(ev.Params, &res); err == nil {
			d.onAccepted(res)
		}
	}
	return map[string]any{}, nil
}

func (a *atsApp) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	switch r.Contribution {
	case "", queueUI, cardUI:
	default:
		return nil, ext.NoContribution(r.Contribution)
	}
	q := a.q()
	if q == nil {
		return nil, errors.New(notReadyWords)
	}
	if r.Contribution == cardUI {
		return buildCardView(q, a.config(), a.now(), r.TZ), nil
	}
	if id := pageTask(r); id != "" {
		return buildTaskView(q, id, a.now(), r.TZ), nil
	}
	return a.homeView(ctx, q, a.now(), r.TZ), nil
}

// homeView is the home page; before setup it reads the computers the setup steps offer.
func (a *atsApp) homeView(ctx context.Context, q *Queue, now time.Time, tz string) *wire.View {
	cfg := a.config()
	var info setupInfo
	if !setUp(cfg) {
		info = a.setupInfo(ctx)
	}
	return buildHomeView(q, cfg, info, now, tz)
}

func setUp(cfg ExtConfig) bool { return len(cfg.Machines) > 0 && len(cfg.Repos) > 0 }

// notReadyWords is what the app shows when a page is asked for before the program has opened its queue.
const notReadyWords = "ATS 还在启动，过几秒刷新一下。"

func (a *atsApp) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	if r.Contribution != "" && r.Contribution != queueUI {
		return nil, ext.NoContribution(r.Contribution)
	}
	q := a.q()
	if q == nil {
		return nil, errors.New(notReadyWords)
	}
	a.mu.Lock()
	d := a.disp
	a.mu.Unlock()
	now := a.now()
	kind, target, _ := strings.Cut(strings.TrimSpace(r.Action), ":")

	if res, ok := a.setupAction(kind, target, r.Values); ok {
		if res.OpenURL == "" {
			res.View = a.homeView(ctx, q, now, r.TZ)
		}
		return res, nil
	}

	var toast string
	switch kind {
	case "open":
		// An app that does not open pages itself asks for the task's page this way.
		title := "任务"
		if t, err := q.GetTask(target); err == nil {
			title = t.Title
		}
		return &wire.ActionResult{Push: taskPush(target, title)}, nil
	case "retry":
		if t, err := q.RetryTask(target, now); err != nil {
			toast = actionErrorToast("没能再试一次", err)
		} else {
			toast = "已经重新交给它做：" + toastTitle(t)
		}
	case "cancel":
		var t *Task
		var err error
		if d != nil {
			t, err = d.cancel(ctx, target)
		} else {
			t, err = q.CancelTask(target, now)
		}
		switch {
		case err != nil:
			toast = actionErrorToast("没能取消", err)
		case t != nil && t.State != StateCanceled && t.State != StateCancelled:
			// The task ended between drawing the page and the tap: nothing was canceled.
			toast = "「" + toastTitle(t) + "」已经结束了，不用再取消。"
		default:
			toast = "已取消：" + toastTitle(t)
		}
	}
	if d != nil {
		d.poke()
	}
	// The action came from a task's own page: draw that page again, not the queue.
	if id := pageTask(r); id != "" {
		return &wire.ActionResult{View: buildTaskView(q, id, now, r.TZ), Toast: toast}, nil
	}
	return &wire.ActionResult{View: a.homeView(ctx, q, now, r.TZ), Toast: toast}, nil
}

func toastTitle(t *Task) string {
	if t == nil {
		return "这个任务"
	}
	return cmpOr(firstLinePreview(t.Title, 40), "这个任务")
}

// actionErrorToast says in Chinese why a button did not work; an error it does not know follows, as it came.
func actionErrorToast(what string, err error) string {
	words, detail := actionErrorWords(err)
	toast := what + "：" + words
	if detail != "" {
		toast += "（原话：" + firstLinePreview(detail, 200) + "）"
	}
	return toast
}

func viewLocation(tz string) *time.Location {
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	return time.UTC
}

// homeIntro is the one line at the top of the home page: what this is and how to hand it work.
const homeIntro = "它在你选的电脑上把待办做完，再把结果写回待办。交给它：在待办里跟助手说「把这个交给 ATS 做」。"

// buildQueueView is the home page. Before the extension is set up it shows what is left to set up, and, when
// tasks handed over earlier have not ended (the settings were cleared after they were queued), those tasks with
// their cancel actions: the dispatcher keeps running them, so the page never hides them. After setup, what the
// user acts on comes first: one line on how things stand, the tasks that wait for the user, what is being done
// and queued, what ended; the counts and the computers are below.
func buildQueueView(q *Queue, cfg ExtConfig, now time.Time, tz string) *wire.View {
	return buildHomeView(q, cfg, setupInfo{}, now, tz)
}

// buildHomeView is buildQueueView with what the setup steps offer before setup (info).
func buildHomeView(q *Queue, cfg ExtConfig, info setupInfo, now time.Time, tz string) *wire.View {
	loc := viewLocation(tz)
	plan := q.PlanSchedule(now)
	quota := plan.Quota
	view := &wire.View{Title: extName, RefreshAfter: 30}
	view.Sections = append(view.Sections, wire.Section{Items: []wire.Component{
		{Type: wire.ComponentText, Key: "intro", Style: wire.TextStyleSecondary, Text: homeIntro},
	}})

	names := map[string]string{}
	online := 0
	for _, m := range plan.Machines {
		if m.Title != "" {
			names[m.Name] = m.Title
		}
		if m.Online {
			online++
		}
	}
	tasks, _ := q.ListTasks(TaskFilter{Limit: 60})
	var needs, open, done []wire.Component
	for _, t := range tasks {
		switch {
		case t.IsTerminal():
			done = append(done, terminalRow(t, loc))
		case t.State == StateNeedsInput:
			needs = append(needs, activeRow(t, names, loc))
		default:
			open = append(open, activeRow(t, names, loc))
		}
	}
	needsSection := wire.Section{Title: "等你处理", Items: needs,
		Footer: "怎么处理：打开这件事对应的待办，在里面 ATS 的对话里回答它的问题或点同意，它就会接着做。"}

	if !setUp(cfg) {
		for _, m := range plan.Machines {
			if info.names == nil {
				info.names = map[string]string{}
			}
			if _, ok := info.names[m.Name]; !ok && m.Title != "" {
				info.names[m.Name] = m.Title
			}
		}
		view.Sections = append(view.Sections, setupSections(cfg, info)...)
		running, waiting := plan.CountsByState[StateRunning], plan.CountsByState[StateNeedsInput]
		if note := liveWorkNote(running, waiting, plan.CountsByState[StateQueued]); note != "" {
			view.Sections[len(view.Sections)-1].Items = append(view.Sections[len(view.Sections)-1].Items,
				wire.Component{Type: wire.ComponentText, Key: "setup-live", Style: wire.TextStyleSecondary, Text: note})
		}
		if len(needs) > 0 {
			view.Sections = append(view.Sections, needsSection)
		}
		if len(open) > 0 {
			view.Sections = append(view.Sections, wire.Section{Title: "正在做和排着的", Items: open})
		}
		return view
	}

	text, tone := statusLine(plan, online, now, loc)
	view.Sections = append(view.Sections, wire.Section{Title: "现在怎么样", Items: []wire.Component{
		{Type: wire.ComponentText, Key: "status", Text: text, Tone: tone},
	}})
	if len(needs) > 0 {
		view.Sections = append(view.Sections, needsSection)
	}
	if len(open) == 0 && len(needs) == 0 {
		open = []wire.Component{{Type: wire.ComponentEmpty, Title: "现在没有要做的事",
			Text: "在任意一个待办里跟助手说「把这个交给 ATS 做」。助手会先跟你说要做什么、在哪个项目、会不会改文件，你同意后，这件事就会出现在这里。"}}
	}
	if len(open) > 0 { // with tasks waiting for the user above, an empty placeholder only pushes the rest down
		view.Sections = append(view.Sections, wire.Section{Title: "正在做和排着的", Items: open})
	}
	if len(done) == 0 {
		done = []wire.Component{{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: "还没有做完的事。"}}
	}
	view.Sections = append(view.Sections, wire.Section{Title: "最近做完的", Items: done})

	queued := plan.CountsByState[StateQueued]
	stats := []wire.Stat{
		{Label: "排着", Value: fmt.Sprintf("%d 件", queued), Detail: fmt.Sprintf("紧急 %d · 重要 %d · 普通 %d · 不急 %d；最多排 %d 件",
			plan.QueuedByPriority[PriorityP0], plan.QueuedByPriority[PriorityP1], plan.QueuedByPriority[PriorityP2], plan.QueuedByPriority[PriorityP3], q.Config().MaxQueued)},
		{Label: "正在做", Value: fmt.Sprintf("%d 件", plan.CountsByState[StateRunning]+plan.CountsByState[StateLeased])},
		{Label: "等你处理", Value: fmt.Sprintf("%d 件", plan.CountsByState[StateNeedsInput])},
		{Label: "做完了", Value: fmt.Sprintf("%d 件", plan.CountsByState[StateSucceeded])},
		{Label: "没做成", Value: fmt.Sprintf("%d 件", plan.CountsByState[StateFailed])},
	}
	if hasQuotaAccount(quota) {
		quotaTone := wire.TonePositive
		if quota.Exhausted || quota.RemainingPct <= 10 {
			quotaTone = wire.ToneDanger
		} else if quota.RemainingPct < plan.ReserveHeadroomPct {
			quotaTone = wire.ToneCaution
		}
		detail := fmt.Sprintf("低于 %.0f%% 时只做紧急和重要的", plan.ReserveHeadroomPct)
		if at := quotaBackAt(quota, now); at != nil {
			detail = at.In(loc).Format("15:04") + " 恢复；" + detail
		}
		stats = append(stats, wire.Stat{Label: "额度还剩", Value: fmt.Sprintf("%.0f%%", quota.RemainingPct), Detail: detail, Tone: quotaTone})
	}
	overview := []wire.Component{{Type: wire.ComponentStatGrid, Stats: stats}}
	// The table of difficulties says something only when one is held back: with every one admitted in full it
	// repeats the status line three times.
	if hasQuotaAccount(quota) {
		tiers := [][]string{
			tierRow("简单的", quota, TierFast, plan.ReserveHeadroomPct),
			tierRow("一般的", quota, TierStandard, plan.ReserveHeadroomPct),
			tierRow("难的", quota, TierStrong, plan.ReserveHeadroomPct),
		}
		if slices.ContainsFunc(tiers, func(r []string) bool { return r[2] != tierAdmitted }) {
			overview = append(overview, wire.Component{Type: wire.ComponentTable, Title: "额度不够时，不同难度的事还做不做", Columns: []wire.TableColumn{
				{Title: "难度"}, {Title: "额度还剩", Align: "end"}, {Title: "现在"},
			}, Rows: tiers})
		}
	}
	view.Sections = append(view.Sections, wire.Section{Title: "数一数", Items: overview})
	view.Sections = append(view.Sections, machinesSection(plan.Machines, cfg))
	return view
}

// liveWorkNote says, before setup, how many tasks handed over earlier have not ended; "" when none.
func liveWorkNote(running, waiting, queued int) string {
	live := running + waiting + queued
	if live == 0 {
		return ""
	}
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{{running, "正在做"}, {waiting, "等你处理"}, {queued, "排着"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d 件", p.what, p.n))
		}
	}
	return fmt.Sprintf("之前交给它的事还有 %d 件没结束（%s），就在下面，不想做了可以取消。", live, strings.Join(parts, "、"))
}

// statusLine is the one line on how things stand: taking work, low on quota, out of quota, or no computer.
func statusLine(plan SchedulePlan, online int, now time.Time, loc *time.Location) (string, string) {
	quota := plan.Quota
	back := ""
	if at := quotaBackAt(quota, now); at != nil {
		back = at.In(loc).Format("15:04")
	}
	switch {
	case len(plan.Machines) == 0:
		return "还没连上你选的电脑：过一会儿刷新看看。要是一直这样，看看那台电脑开着没有、登录着 Persona 没有。", wire.ToneCaution
	case online == 0:
		return "没有电脑在线：你选的电脑都没开，或者没登录 Persona。事情会先排着，打开那台电脑上的 Persona 就会接着做。", wire.ToneDanger
	case plan.AdmissionMode == AdmissionPausedQuota:
		if back != "" {
			return "额度用完了，大约 " + back + " 恢复。事情会先排着，额度恢复后自动接着做。", wire.ToneDanger
		}
		return "额度用完了。事情会先排着，额度恢复后自动接着做。", wire.ToneDanger
	case plan.AdmissionMode == AdmissionPriorityOnly:
		line := fmt.Sprintf("额度快用完了（还剩 %.0f%%），现在只做紧急和重要的事，其余的先排着", quota.RemainingPct)
		if back != "" {
			line += "，大约 " + back + " 额度恢复后再做"
		}
		return line + "。", wire.ToneCaution
	}
	return "正常接活：交给它的事会按先急后缓的顺序做。", wire.TonePositive
}

// buildCardView is the card on the Overview page: whether ATS is ready, a few counts, and the one thing to do
// next. It says the same as the home page in fewer words, and a tap opens the home page.
func buildCardView(q *Queue, cfg ExtConfig, now time.Time, tz string) *wire.View {
	loc := viewLocation(tz)
	plan := q.PlanSchedule(now)
	counts := plan.CountsByState
	waiting, doing, queued, failed := counts[StateNeedsInput], counts[StateRunning]+counts[StateLeased], counts[StateQueued], counts[StateFailed]
	view := &wire.View{Title: cardTitle, RefreshAfter: 30}

	if !setUp(cfg) {
		items := []wire.Component{
			{Type: wire.ComponentText, Key: "card-setup", Text: "ATS 还没准备好：" + setupMissingWords(len(cfg.Machines) > 0, len(cfg.Repos) > 0), Tone: wire.ToneCaution},
			{Type: wire.ComponentText, Key: "card-next", Text: "下一步：点开这里，在页面上选好电脑和文件夹。"},
		}
		if live := waiting + doing + queued; live > 0 {
			items = append(items, wire.Component{Type: wire.ComponentText, Key: "card-live", Style: wire.TextStyleSecondary,
				Text: fmt.Sprintf("之前交给它的事还有 %d 件没结束，点开可以看，也可以取消。", live)})
		}
		view.Sections = []wire.Section{{Items: items}}
		return view
	}

	count := func(label string, n int, tone string) wire.Stat {
		if n == 0 {
			tone = ""
		}
		return wire.Stat{Label: label, Value: fmt.Sprintf("%d 件", n), Tone: tone}
	}
	stats := []wire.Stat{count("等你处理", waiting, wire.ToneDanger), count("正在做", doing, ""), count("排着", queued, "")}
	if failed > 0 {
		stats = append(stats, count("没做成", failed, wire.ToneDanger))
	}

	online := 0
	for _, m := range plan.Machines {
		if m.Online {
			online++
		}
	}
	var next, tone string
	switch {
	case waiting > 0:
		title := "一件事"
		what := "它在等你回答问题或点同意"
		if ts, _ := q.ListTasks(TaskFilter{States: []string{StateNeedsInput}, Limit: 1}); len(ts) > 0 {
			title = "「" + cmpOr(firstLinePreview(ts[0].Title, 30), "没有标题的事") + "」"
			if ts[0].Exec != nil {
				what = waitingWords(ts[0].Exec.Waiting)
			}
		}
		next = truncateRunes(title+"等你处理："+what, 120) + "\n打开这件事对应的待办，在里面 ATS 的对话里回答它的问题或点同意。"
		tone = wire.ToneDanger
	case online == 0, hasQuotaAccount(plan.Quota) && (plan.AdmissionMode == AdmissionPausedQuota || plan.AdmissionMode == AdmissionPriorityOnly):
		// The home page's own line, with the time the quota comes back; no quota is spoken of that was not measured.
		next, tone = statusLine(plan, online, now, loc)
	case doing+queued > 0:
		next, tone = "没有要你处理的事，它在接着做。", wire.TonePositive
	default:
		next, tone = "没有要你处理的事。想交给它做事：在任意一个待办里跟助手说「把这个交给 ATS 做」。", wire.TonePositive
	}
	view.Sections = []wire.Section{{Items: []wire.Component{
		{Type: wire.ComponentStatGrid, Key: "card-counts", Stats: stats},
		{Type: wire.ComponentText, Key: "card-next", Text: next, Tone: tone},
	}}}
	return view
}

// hasQuotaAccount says whether the quota was measured on a real account. Without accounts the queue holds a
// placeholder of 100% (DefaultQuotaStatus, also what a gateway source with no accounts gives, under the source's
// name), which the page must not show as if it were measured. The placeholder is told by its values: the
// provider's name alone does not tell it apart. An account that reports no window and no reset looks the same,
// and has no measured quota either.
func hasQuotaAccount(quota QuotaStatus) bool {
	if quota.Provider == "" || quota.Provider == DefaultQuotaStatus().Provider {
		return false
	}
	if quota.Exhausted || quota.RemainingPct < 100 || quota.ResetAt != nil || quota.CooldownUntil != nil || quota.TotalAccounts > 1 {
		return true
	}
	for _, v := range quota.TierRemainingPct {
		if v < 100 {
			return true
		}
	}
	return false
}

// quotaBackAt is when the quota comes back: the end of a cool-down still running at now, or the reset.
func quotaBackAt(quota QuotaStatus, now time.Time) *time.Time {
	if quota.CooldownUntil != nil && quota.CooldownUntil.After(now) {
		return quota.CooldownUntil
	}
	return quota.ResetAt
}

// machinesSection lists the chosen computers, whether each is on and how busy, and the allowed folders.
func machinesSection(machines []MachineSlot, cfg ExtConfig) wire.Section {
	var items []wire.Component
	for _, m := range machines {
		name := cmpOr(m.Title, m.Name)
		row := wire.Component{Type: wire.ComponentRow, Key: "machine-" + m.Name, Title: truncateRunes(name, detailTextMost)}
		if m.Online {
			row.Badge = &wire.Badge{Text: "在线", Tone: wire.TonePositive}
			row.Subtitle = fmt.Sprintf("正在做 %d 件，最多同时做 %d 件", m.RunningSlots, m.MaxSlots)
		} else {
			row.Badge = &wire.Badge{Text: "离线", Tone: wire.ToneDanger}
			row.Subtitle = "这台电脑没开或没登录 Persona。打开它上面的 Persona，事情才会在这台电脑上接着做。"
		}
		items = append(items, row)
		if len(items) >= 50 {
			break
		}
	}
	if len(items) == 0 {
		items = append(items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary,
			Text: "还没连上你选的电脑，过一会儿刷新看看。"})
	}
	items = append(items, wire.Component{Type: wire.ComponentText, Key: "repos", Style: wire.TextStyleSecondary,
		Text: truncateRunes("能动的文件夹："+strings.Join(cfg.Repos, "、"), detailTextMost)})
	return wire.Section{Title: "干活的电脑", Items: items, Footer: "想换电脑或文件夹：到「" + settingsPath + "」里改。"}
}

const tierAdmitted = "照常做"

func tierRow(difficulty string, quota QuotaStatus, key string, reserve float64) []string {
	rem := tierRemaining(quota, key)
	status := tierAdmitted
	switch {
	case quota.Exhausted || rem <= 5:
		status = "先停下"
	case rem < reserve:
		status = "只做紧急和重要的"
	}
	return []string{difficulty, fmt.Sprintf("%.0f%%", rem), status}
}

// taskMeta is what every row says of its task, in a few words: how urgent, whether it may change files, which
// project. The to-do's id and the model tier are on the task's page, under its technical details.
func taskMeta(t *Task) []string {
	meta := []string{priorityWords(t.Priority), permissionWords(t.PermissionProfile)}
	if name := repoName(t.Workspace.Repo); name != "" {
		meta = append(meta, "项目 "+name)
	}
	return meta
}

// openTask is a row's tap: the task's page, on top of the queue. Canceling and retrying are on that page and in
// the row's menu, so that a tap never changes a task.
func openTask(t *Task) *wire.Action {
	return &wire.Action{ID: "open:" + t.ID, Push: taskPush(t.ID, t.Title)}
}

func cancelAction(t *Task) wire.Action {
	return wire.Action{ID: "cancel:" + t.ID, Confirm: fmt.Sprintf("不做「%s」了？取消后它会马上停下。", cmpOr(firstLinePreview(t.Title, 40), "这件事"))}
}

func retryAction(t *Task) wire.Action {
	return wire.Action{ID: "retry:" + t.ID, Confirm: fmt.Sprintf("把「%s」重新交给它做一次？", cmpOr(firstLinePreview(t.Title, 40), "这件事"))}
}

func activeRow(t *Task, machines map[string]string, loc *time.Location) wire.Component {
	meta := taskMeta(t)
	badge := &wire.Badge{Text: "排着队", Tone: wire.ToneCaution}
	if t.NotBefore != nil && t.State == StateQueued {
		badge = &wire.Badge{Text: "等着再试", Tone: wire.ToneCaution}
		meta = append(meta, fmt.Sprintf("第 %d 次没做成（%s），%s 自动再试", max(t.AttemptNo, 1), pageFailureWords(lastErrorClass(t), lastVerdict(t)), t.NotBefore.In(loc).Format("15:04")))
	}
	subtitle := ""
	if e := t.Exec; e != nil && isActiveState(t.State) {
		name := cmpOr(machines[e.MachineID], e.MachineID)
		meta = append(meta, "在 "+name+" 上", attemptCount(t.AttemptNo))
		switch {
		case t.State == StateNeedsInput:
			badge = &wire.Badge{Text: "等你处理", Tone: wire.ToneDanger}
			// What it waits for is the line the user reads; the rest is on the task's page.
			subtitle = waitingWords(e.Waiting)
		default:
			badge = &wire.Badge{Text: phaseWords(e.Phase), Tone: wire.TonePositive}
		}
	}
	if subtitle == "" {
		subtitle = strings.Join(meta, " · ")
	}
	return wire.Component{
		Type: wire.ComponentRow, Key: "active-" + t.ID, Title: cmpOr(truncateRunes(t.Title, detailTextMost), "（没有标题）"), Subtitle: truncateRunes(subtitle, detailTextMost), Badge: badge,
		Action:  openTask(t),
		Actions: []wire.RowAction{{Title: "取消", Role: wire.ButtonRoleDestructive, Swipe: true, Action: cancelAction(t)}},
	}
}

// lastVerdict is the verdict of the task's newest report (the task's own verdict is set only when it ends).
func lastVerdict(t *Task) string {
	if r := lastReport(t); r != nil {
		return r.Verdict
	}
	return t.Verdict
}

func terminalRow(t *Task, loc *time.Location) wire.Component {
	meta := taskMeta(t)
	var badge *wire.Badge
	var actions []wire.RowAction
	switch t.State {
	case StateSucceeded:
		if t.Verdict == VerdictPass {
			badge = &wire.Badge{Text: "做成了", Tone: wire.TonePositive}
		} else {
			badge = &wire.Badge{Text: "做完了（没设检查）", Tone: wire.ToneCaution}
		}
	case StateFailed:
		badge = &wire.Badge{Text: "没做成", Tone: wire.ToneDanger}
		meta = append(meta, fmt.Sprintf("%s，试了 %d 次", pageFailureWords(lastErrorClass(t), t.Verdict), max(t.AttemptNo, 1)))
		actions = []wire.RowAction{{Title: "再试一次", Swipe: true, Action: retryAction(t)}}
	default:
		badge = &wire.Badge{Text: pageOutcomeWords(t.State, t.Verdict, ""), Tone: wire.ToneCaution}
		actions = []wire.RowAction{{Title: "再试一次", Swipe: true, Action: retryAction(t)}}
	}
	if t.FinishedAt != nil {
		meta = append(meta, t.FinishedAt.In(loc).Format("01-02 15:04"))
	}
	return wire.Component{Type: wire.ComponentRow, Key: "term-" + t.ID, Title: cmpOr(truncateRunes(t.Title, detailTextMost), "（没有标题）"), Subtitle: truncateRunes(strings.Join(meta, " · "), detailTextMost), Badge: badge,
		Action: openTask(t), Actions: actions}
}
