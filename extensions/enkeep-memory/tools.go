package main

// The connector "enkeep"'s tools. Reads (tools.list, capability tools): how far the reruns got, the seeds, what a
// resettle or a seed import would do. Writes (actions.list, capability actions, run by actions.do once the hub's gate let
// them through): queue days to be digested again with the current daily prompt, apply a resettle, import seeds, mark
// entries wrong. A rerun is a one-off operation, not a setting: it reads the user's own sources again inside the
// extension, sends nothing anywhere and deletes nothing; what it finds goes through the same thresholds and review as
// any day's, and each day runs once per prompt, so asking twice does nothing more. rerun_range (the setting) works as
// before. enkeep_resettle settles every entry's status again on the real active days (curate/resettle.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/curate"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	exits "github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/export"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/recall"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
	"github.com/nwverse/persona/sdk/go/ext"
)

// toolWait is how long a tool that changes the state waits for the store's lock before it says a pass is running
// (or, enkeep_mark_wrong, queues its marks): the hub gives up on a tool call after a while, a daily pass holds the
// lock for tens of minutes.
var toolWait = 5 * time.Second // a variable for tests

// busy turns curate.ErrBusy into what the caller is told: which pass runs, and to call again when it is over.
func (a *app) busy(err error) error {
	if !errors.Is(err, curate.ErrBusy) {
		return err
	}
	what := "a curation pass"
	if r := a.runningNow(); r != "" {
		what = r
	}
	return fmt.Errorf("busy: %s is running and holds the state; call again when it is over (enkeep_rerun_status shows it)", what)
}

// runningNow is the pass this process runs, "" when none.
func (a *app) runningNow() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// connectorID is the manifest's connector id. The hub keeps only the tools whose names start with it and "_"
// (connector.owned), so the tools are enkeep_*.
const connectorID = "enkeep"

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// The connector's tools, split as the hub reads them (Persona #13532/#13586, docs/extensions/extensions.md "Memory
// sources"): readTools are tools.list's, read-only, answered by tools.call; writeTools are actions.list's, run by
// actions.do only once the hub's gate let them through (a card on the user's phone unless a connector_gate rule
// allows it). Since the extension declares memory_sources, the hub does not offer the read tools in the assistant's
// conversations (it reads memory.entries itself and recalls with decisions_query); coding Agents get them through
// agent_tools. There is no recall tool: the entries reach every scene through memory.entries only.
var readTools = []tool{
	{Name: "enkeep_rerun_status", Description: "Show how far enkeep memory's reruns got: the days queued (by enkeep_rerun " +
		"or the rerun_range setting), which are done with the current prompt, which are waiting, what is running now, " +
		"and whether the passes wait for the model or the assistant's computer; also the model route (calls per model, " +
		"fallbacks and why) and what memory.entries answers the hub (entries, active, standing, tagged, time). Read-only.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
	{Name: "enkeep_resettle_preview", Description: "Report what settling every enkeep memory entry's status (candidate, " +
		"stable, shifting, dormant, retired) again as of today would change, counting only the days the extraction really " +
		"read with the model: the counts before and after and the stable entries per facet. Read-only: enkeep_resettle " +
		"(a write, confirmed on the phone) applies it.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
	{Name: "enkeep_seed_status", Description: "Show enkeep memory's seeds: whether seeds are on as a source, the seed " +
		"entries by source and status (a candidate waits for the user's own records to bear it out), by facet, how many " +
		"reach the exits now, the curator's own entries a seed confirmed, and every import written. Read-only.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
	{Name: "enkeep_seed_preview", Description: "Preview importing the user's own profile and rules confirmed elsewhere " +
		"(another memory, a notes file, a profile platform) as enkeep memory seeds: how many items would be new, merged " +
		"with an entry the curator has, updated, the same as before, or skipped and why. Writes nothing; enkeep_seed_import " +
		"(a write, confirmed on the phone) imports the same arguments. " + seedFormat,
		InputSchema: seedSchema()},
	{Name: "enkeep_overview_status", Description: "Show enkeep memory's periodic cross-event overview: its patterns per " +
		"facet by lifecycle (core_stable, scoped, candidate, drifting, era_closed, retired), the last weekly and monthly " +
		"passes (model calls, what they made or moved), the forward checks (the patterns predicting a week's real choices " +
		"against the scattered entries and no memory), the last blind spot check, the weekly report's push, and how many " +
		"overview entries memory.entries answers. Optional from/to (YYYY-MM-DD) also report per-contract violation " +
		"recurrences (including unabsorbed Fast-Path hot-slot entries) and rate per 100 ask/decision cards in that window; " +
		"optional query filters contracts by substring or ID. Read-only.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"from":  map[string]any{"type": "string", "pattern": `^\d{4}-\d{2}-\d{2}$`, "description": "optional window start YYYY-MM-DD for per-contract violation readings"},
			"to":    map[string]any{"type": "string", "pattern": `^\d{4}-\d{2}-\d{2}$`, "description": "optional window end YYYY-MM-DD for per-contract violation readings"},
			"query": map[string]any{"type": "string", "description": "optional substring or contract ID filter for violation readings"},
		}}},
}

var writeTools = []tool{
	{Name: "enkeep_rerun", Description: "Queue past days to be read again with enkeep memory's current extraction prompt " +
		"(for example after the model was down or the prompt changed). A write: the hub confirms it on the user's phone " +
		"unless its rules allow it. Days run one at a time in the background, each at most once per prompt version; days " +
		"already read with the current prompt are skipped. At most 31 days per call, only days before today. force: true " +
		"reads the days again even if already read with the current prompt (for a day that lost a unit to a model error, " +
		"or after a switch changed what is kept); units read before reuse their stored answers, which are checked again " +
		"without a model call, and only what was not read calls the model. Returns what was queued; enkeep_rerun_status " +
		"(coding Agents) shows progress.",
		InputSchema: map[string]any{"type": "object", "required": []string{"from", "to"}, "properties": map[string]any{
			"from":  map[string]any{"type": "string", "pattern": `^\d{4}-\d{2}-\d{2}$`, "description": "first day, YYYY-MM-DD"},
			"to":    map[string]any{"type": "string", "pattern": `^\d{4}-\d{2}-\d{2}$`, "description": "last day, YYYY-MM-DD, before today"},
			"force": map[string]any{"type": "boolean", "default": false, "description": "read the days again even if already read with the current prompt"},
		}}},
	{Name: "enkeep_resettle", Description: "Settle every enkeep memory entry's status (candidate, stable, shifting, dormant, " +
		"retired) again as of today, counting only the days the extraction really read with the model, and apply it. A " +
		"write: the hub confirms it on the user's phone unless its rules allow it. Use it to repair statuses after reruns " +
		"(enkeep_resettle_preview says what it would change). Idempotent: a second call moves nothing. Deletes nothing.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
	{Name: "enkeep_seed_import", Description: "Import the user's own profile and rules confirmed elsewhere (another " +
		"memory, a notes file, a profile platform) into enkeep memory as seeds. A write: the hub confirms it on the user's " +
		"phone unless its rules allow it; enkeep_seed_preview (coding Agents) previews the same arguments. Items in a " +
		"category (health, finance, relationships, emotion, legal_id; by category, keywords and a model's reading) are " +
		"imported tagged and used by scene; items with a credential or not confirmed at the source are never imported. New " +
		"seeds start as candidates and reach the exits only once the user's own records bear them out. " + seedFormat,
		InputSchema: seedSchema()},
	{Name: "enkeep_mark_wrong", Description: "Mark enkeep memory entries wrong, for example an entry in the context that " +
		"is not the user's rule (a task prompt, a relay or a test prompt taken for the user's words). A write: the hub " +
		"confirms it on the user's phone unless its rules allow it. The entries leave every exit and memory.entries at once " +
		"and stay as counter-examples, recorded as marked by the assistant with the reason; the user can mark them right " +
		"on the enkeep memory page to bring them back. Give entry ids as the context or decisions_query shows them " +
		"(memory-extension:<id>, or the bare id). Deletes nothing.",
		InputSchema: map[string]any{"type": "object", "required": []string{"ids", "reason"}, "properties": map[string]any{
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "1 to 20 entry ids (memory-extension:<id> or <id>)"},
			"reason": map[string]any{"type": "string", "description": "why the entries are wrong (at most 300 characters)"},
		}}},
	{Name: "enkeep_overview_run", Description: "Run a pass of enkeep memory's periodic cross-event overview now, in the " +
		"background: weekly (the week's cross-event synthesis, period YYYY-Www), monthly (consolidation and drift, period " +
		"YYYY-MM), backtest (patterns built only from what came before the week YYYY-Www, checked against its real choices; " +
		"the kept patterns are not changed), audit (a model's blind spot check of up to 30 used patterns, kept for the user " +
		"to go over), or report (send the weekly report of week YYYY-Www again, its short summary to the user's phone, when " +
		"its push failed). A write: the hub confirms it on the user's phone unless its rules allow it. Uses Claude models " +
		"only, within the extension's shared limit of calls. enkeep_overview_status (coding Agents) shows the result.",
		InputSchema: map[string]any{"type": "object", "required": []string{"kind"}, "properties": map[string]any{
			"kind":   map[string]any{"type": "string", "enum": []string{"weekly", "monthly", "backtest", "audit", "report"}},
			"period": map[string]any{"type": "string", "pattern": `^\d{4}-(W\d{2}|\d{2})$`, "description": "YYYY-Www for weekly, backtest and report; YYYY-MM for monthly; not used by audit"},
		}}},
}

// overviewArgs checks enkeep_overview_run's arguments: the pass's kind and its period.
func overviewArgs(raw json.RawMessage) (kind, period string, err error) {
	var in struct {
		Kind   string `json:"kind"`
		Period string `json:"period"`
	}
	if err := strictDecode(raw, &in); err != nil {
		return "", "", err
	}
	period = strings.TrimSpace(in.Period)
	week := func() error {
		if _, err := curate.WeekDays(period); err != nil || !strings.Contains(period, "-W") {
			return errors.New("period is a week, YYYY-Www")
		}
		return nil
	}
	switch in.Kind {
	case "weekly":
		return kindOverview, period, week()
	case "backtest":
		return kindBacktest, period, week()
	case "report":
		return "report", period, week()
	case "monthly":
		if _, err := time.Parse("2006-01", period); err != nil {
			return "", "", errors.New("period is a month, YYYY-MM")
		}
		return kindOverviewMonth, period, nil
	case "audit":
		return kindAudit, time.Now().UTC().Format("2006-01-02T15:04"), nil
	}
	return "", "", errors.New("kind is weekly, monthly, backtest, audit or report")
}

// seedFormat is the seeds' general format, in both seed tools' descriptions.
const seedFormat = "Arguments: a source name and items {id, kind (rule | guardrail | preference | fact), text, confirmed " +
	"(YYYY-MM-DD), and optionally facet, hard, category, scope (global | project:<name>), domain (work | life), effective, " +
	"expiring (recent | role), sensitive}, at most 500."

// seedSchema is both seed tools' input schema.
func seedSchema() map[string]any {
	return map[string]any{"type": "object", "required": []string{"source", "items"}, "properties": map[string]any{
		"source": map[string]any{"type": "string", "description": "a short name of the source, without spaces or slashes (e.g. enkeep)"},
		"items": map[string]any{"type": "array", "description": "at most 500 items", "items": map[string]any{"type": "object",
			"required": []string{"id", "kind", "text", "confirmed"}, "properties": map[string]any{
				"id": map[string]any{"type": "string"}, "kind": map[string]any{"type": "string", "enum": []string{"rule", "guardrail", "preference", "fact"}},
				"text": map[string]any{"type": "string"}, "confirmed": map[string]any{"type": "string"},
				"facet": map[string]any{"type": "string", "enum": []string{"interaction", "tools", "judgement", "workflow", "world", "persona"}},
				"hard":  map[string]any{"type": "boolean"}, "category": map[string]any{"type": "string"},
				"scope": map[string]any{"type": "string"}, "domain": map[string]any{"type": "string", "enum": []string{"work", "life"}},
				"effective": map[string]any{"type": "string"}, "expiring": map[string]any{"type": "string", "enum": []string{"recent", "role"}},
				"sensitive": map[string]any{"type": "string"},
			}}},
	}}
}

// allTools are the read tools, then the writes.
func allTools() []tool { return append(slices.Clone(readTools), writeTools...) }

func listed(list []tool, name string) bool {
	return slices.ContainsFunc(list, func(t tool) bool { return t.Name == name })
}

func (a *app) connector(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var in struct {
		Contribution string          `json:"contribution"`
		Name         string          `json:"name"`
		Args         json.RawMessage `json:"args"`
		RequestID    string          `json:"request_id"`
	}
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		if err := dec.Decode(&in); err != nil {
			return nil, &ext.Error{Code: "invalid", Message: "invalid connector parameters"}
		}
	}
	if in.Contribution != "" && in.Contribution != connectorID {
		return nil, ext.NoContribution(in.Contribution)
	}
	switch method {
	case "status":
		return map[string]any{"state": "ok", "account": "enkeep 记忆"}, nil
	case "tools.list":
		return map[string]any{"tools": readTools}, nil
	case "actions.list":
		return map[string]any{"tools": writeTools}, nil
	case "tools.call":
		if !listed(readTools, in.Name) {
			if listed(writeTools, in.Name) {
				return nil, &ext.Error{Code: "invalid", Message: in.Name + " is a write: the hub runs it through its gate (actions.do)"}
			}
			return nil, &ext.Error{Code: "invalid", Message: "unknown tool " + in.Name}
		}
		text, err := a.callTool(ctx, in.Name, in.Args)
		if err != nil {
			return nil, &ext.Error{Code: "invalid", Message: err.Error()}
		}
		return map[string]any{"text": text}, nil
	case "actions.describe":
		if !listed(writeTools, in.Name) {
			return nil, &ext.Error{Code: "invalid", Message: "unknown action " + in.Name}
		}
		summary, err := a.describe(in.Name, in.Args)
		if err != nil {
			return nil, &ext.Error{Code: "invalid", Message: err.Error()}
		}
		return map[string]any{"action": map[string]any{"name": in.Name, "visible": cardVisible}, "summary": summary}, nil
	case "actions.do":
		if !listed(writeTools, in.Name) {
			return nil, &ext.Error{Code: "invalid", Message: "unknown action " + in.Name}
		}
		text, err := a.doOnce(in.RequestID, func() (string, error) { return a.callTool(ctx, in.Name, in.Args) })
		if err != nil {
			return nil, &ext.Error{Code: "invalid", Message: err.Error()}
		}
		return map[string]any{"text": text}, nil
	}
	return nil, ext.UnknownMethod("connector." + method)
}

// cardVisible is who sees what a write does (the card's "谁会看到"): it stays in the user's own memory.
const cardVisible = "只有你：只改你的 enkeep 记忆，不发给任何人"

// doneMost is how many actions.do answers are kept by request id: the hub never sends one again after a timeout, but
// the protocol says a repeated id answers the first result.
const doneMost = 64

type doneWrite struct {
	id, text string
	err      error
}

// doOnce runs a write once per request id (an empty id always runs) and answers a repeated id with its first result.
func (a *app) doOnce(id string, run func() (string, error)) (string, error) {
	if id != "" {
		a.mu.Lock()
		for _, d := range a.done {
			if d.id == id {
				a.mu.Unlock()
				return d.text, d.err
			}
		}
		a.mu.Unlock()
	}
	text, err := run()
	if id != "" {
		a.mu.Lock()
		a.done = append(a.done, doneWrite{id, text, err})
		if len(a.done) > doneMost {
			a.done = a.done[len(a.done)-doneMost:]
		}
		a.mu.Unlock()
	}
	return text, err
}

// describe is a write's card: what it will do, in the user's words, from its checked arguments and the pages'
// snapshot (no network, never the store's lock: a run holds it for tens of minutes). An error refuses the call before
// the user is asked.
func (a *app) describe(name string, raw json.RawMessage) (string, error) {
	switch name {
	case "enkeep_rerun":
		var in struct {
			From, To string
			Force    bool
		}
		if err := strictDecode(raw, &in); err != nil {
			return "", err
		}
		from, to := strings.TrimSpace(in.From), strings.TrimSpace(in.To)
		f, err1 := time.Parse(time.DateOnly, from)
		t, err2 := time.Parse(time.DateOnly, to)
		if err1 != nil || err2 != nil || t.Before(f) {
			return "", errors.New("from and to are days, YYYY-MM-DD, from not after to")
		}
		n := int(t.Sub(f).Hours()/24) + 1
		if n > 31 {
			return "", errors.New("at most 31 days per call")
		}
		s := fmt.Sprintf("enkeep 记忆：用当前提示词重读 %s 到 %s 共 %d 天", from, to, n)
		if in.Force {
			s += "（包括已经用当前提示词读过的天）"
		} else {
			s += "（已经用当前提示词读过的天跳过）"
		}
		return s + "。在扩展里重读你自己的来源，一天一天在后台跑；不发给任何人，不删数据。", nil
	case "enkeep_resettle":
		if err := strictDecode(raw, &struct{}{}); err != nil {
			return "", err
		}
		return "enkeep 记忆：按今天重算所有条目的状态（候选、稳定、在变、较早、退役）并保存，只数模型真正读过的日子。" +
			"不删数据；再算一次不会再动。", nil
	case "enkeep_seed_import":
		in, err := seedArgs(raw)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "enkeep 记忆：从「%s」导入 %d 条种子。新种子先是候选，你自己的记录印证后才会用上；带凭据或来源没确认过的不导入。", in.Source, len(in.Items))
		for i, it := range in.Items {
			if i == 5 {
				fmt.Fprintf(&b, "\n…另外 %d 条", len(in.Items)-5)
				break
			}
			fmt.Fprintf(&b, "\n- %s", cut(it.Text, 80))
		}
		return b.String(), nil
	case "enkeep_overview_run":
		kind, period, err := overviewArgs(raw)
		if err != nil {
			return "", err
		}
		switch kind {
		case kindOverview:
			return "enkeep 记忆：现在跑 " + period + " 的周级跨事总览（从你自己的记录里找跨项目、跨来源反复出现的取舍，只用 Claude 模型，大约 20 次调用）。不发给任何人，不删数据。", nil
		case kindOverviewMonth:
			return "enkeep 记忆：现在跑 " + period + " 的月度巩固（合并重复的模式、重新核对反例、标出在变和已结束的）。不发给任何人，不删数据。", nil
		case kindBacktest:
			return "enkeep 记忆：用 " + period + " 之前的记录建一份临时模式，检验它能不能预测这一周你的真实取舍（和散点条目、不带记忆对比）。不改现有模式，不发给任何人。", nil
		case kindAudit:
			return "enkeep 记忆：让模型盲判抽查最多 30 条在用的跨事模式，结果留给你在插件页上核对。不改模式，不发给任何人。", nil
		}
		return "enkeep 记忆：把 " + period + " 的周报重新推送到你的手机（短摘要，全文在插件页「定期总览」）。", nil
	case "enkeep_mark_wrong":
		ids, reason, err := markArgs(raw)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "enkeep 记忆：把这 %d 条标成错的（理由：%s）。它们马上不再出现在任何地方，作为反例留着，记成助手标的；你可以在 enkeep 记忆页上改回。不删数据。", len(ids), reason)
		var st *store.State
		if s := a.c.Live.Snapshot(); s != nil {
			st = s.State
		}
		for _, id := range ids {
			if st != nil && st.Entries[id] != nil {
				fmt.Fprintf(&b, "\n- %s：%s", id, cut(recall.Line(st.Entries[id]), 80))
				continue
			}
			fmt.Fprintf(&b, "\n- %s", id)
		}
		return b.String(), nil
	}
	return "", errors.New("unknown action " + name)
}

type seedIn struct {
	Source string            `json:"source"`
	Items  []curate.SeedItem `json:"items"`
}

// seedArgs are both seed tools' arguments, checked.
func seedArgs(raw json.RawMessage) (seedIn, error) {
	var in seedIn
	if err := strictDecode(raw, &in); err != nil {
		return in, err
	}
	f := curate.SeedFormat{Source: in.Source, Items: in.Items}
	if err := curate.CheckSeeds(&f); err != nil {
		return in, err
	}
	in.Source, in.Items = f.Source, f.Items
	return in, nil
}

// markArgs are enkeep_mark_wrong's arguments, checked: the ids without their "memory-extension:" or "id:" prefix.
func markArgs(raw json.RawMessage) ([]string, string, error) {
	var in struct {
		IDs    []string `json:"ids"`
		Reason string   `json:"reason"`
	}
	if err := strictDecode(raw, &in); err != nil {
		return nil, "", err
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.IDs) == 0 || len(in.IDs) > 20 {
		return nil, "", errors.New("ids: 1 to 20 entry ids")
	}
	if in.Reason == "" || len([]rune(in.Reason)) > 300 {
		return nil, "", errors.New("reason: 1 to 300 characters")
	}
	for i, id := range in.IDs {
		id = strings.TrimSpace(id)
		id = strings.TrimPrefix(id, "memory-extension:")
		in.IDs[i] = strings.TrimPrefix(id, "id:")
	}
	return in.IDs, in.Reason, nil
}

func strictDecode(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func (a *app) callTool(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	switch name {
	case "enkeep_mark_wrong":
		return a.markWrongTool(ctx, raw)
	case "enkeep_seed_import", "enkeep_seed_preview":
		in, err := seedArgs(raw)
		if err != nil {
			return "", err
		}
		apply := name == "enkeep_seed_import"
		if apply && !entry.On(a.seedSwitches(), entry.K21Seeds) {
			return "", errors.New("seeds are switched off as a source (K21 on the enkeep memory page): nothing imported")
		}
		r, err := a.c.ImportSeeds(ctx, curate.SeedFormat{Source: in.Source, Items: in.Items}, apply, toolWait)
		if err != nil {
			return "", a.busy(err)
		}
		return r.Text(), nil
	case "enkeep_seed_status":
		if err := strictDecode(raw, &struct{}{}); err != nil {
			return "", err
		}
		return a.c.Seeds(exits.Usable).Text(), nil
	case "enkeep_overview_status":
		var in struct {
			From  string `json:"from"`
			To    string `json:"to"`
			Query string `json:"query"`
		}
		if err := strictDecode(raw, &in); err != nil {
			return "", err
		}
		return a.overviewStatusRange(strings.TrimSpace(in.From), strings.TrimSpace(in.To), strings.TrimSpace(in.Query)), nil
	case "enkeep_overview_run":
		kind, period, err := overviewArgs(raw)
		if err != nil {
			return "", err
		}
		if kind == "report" {
			body, err := a.weeklySend(ctx, period, time.Now(), true)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("The weekly report of %s was sent (%d characters): %s", period, len([]rune(body)), body), nil
		}
		if a.ov == nil {
			return "", errors.New("the overview is off: its state could not be read (see the hub's log)")
		}
		if !a.start(context.WithoutCancel(ctx), kind, period) {
			return "", a.busy(curate.ErrBusy)
		}
		return fmt.Sprintf("Started %s %s in the background; enkeep_overview_status shows the result when it is done.", kind, period), nil
	case "enkeep_rerun":
		var in struct {
			From, To string
			Force    bool
		}
		if err := strictDecode(raw, &in); err != nil {
			return "", err
		}
		from, to, today := strings.TrimSpace(in.From), strings.TrimSpace(in.To), a.localDay()
		r, err := a.c.QueueRerunWithin(from, to, today, in.Force, toolWait)
		if errors.Is(err, curate.ErrBusy) {
			return a.queueRerun(from, to, today, in.Force)
		}
		if err != nil {
			return "", err
		}
		var b strings.Builder
		if r.Force {
			fmt.Fprintf(&b, "Queued %d day(s) with force to read again with the current prompt, even if already read with it", len(r.Queued))
		} else {
			fmt.Fprintf(&b, "Queued %d day(s) to read again with the current prompt", len(r.Queued))
		}
		if len(r.Queued) > 0 {
			fmt.Fprintf(&b, ": %s", span(r.Queued))
		}
		b.WriteString(".")
		if len(r.Already) > 0 {
			if r.Force {
				fmt.Fprintf(&b, " Already queued with force, skipped: %s.", span(r.Already))
			} else {
				fmt.Fprintf(&b, " Already done or queued, skipped: %s.", span(r.Already))
			}
		}
		b.WriteString(" They run one at a time in the background; enkeep_rerun_status shows progress.")
		return b.String(), nil
	case "enkeep_rerun_status":
		if err := strictDecode(raw, &struct{}{}); err != nil {
			return "", err
		}
		p := a.c.RerunStatus() // the pages' snapshot: never waits for a run's lock
		var b strings.Builder
		fmt.Fprintf(&b, "Prompt %s: %d day(s) done, %d waiting.", p.Prompt, len(p.Done), len(p.Waiting))
		fmt.Fprintf(&b, " Rerun progress: %d day(s) done, %d left, %d new usable entr(ies) resting on the days read since the batch was queued.",
			len(p.Done), len(p.Waiting)+len(p.Forced), p.Gain)
		if len(p.Waiting) > 0 {
			fmt.Fprintf(&b, " Waiting: %s.", span(p.Waiting))
		}
		if len(p.Forced) > 0 {
			fmt.Fprintf(&b, " Queued with force (read again although already read with this prompt), waiting: %s.", span(p.Forced))
		}
		if pendN, pendF := a.pendingRerunDays(); len(pendN) > 0 || len(pendF) > 0 {
			if len(pendN) > 0 {
				fmt.Fprintf(&b, " Pending queue (joins after current run): %s.", span(pendN))
			}
			if len(pendF) > 0 {
				fmt.Fprintf(&b, " Pending force queue (joins at front after current run): %s.", span(pendF))
			}
		}
		if pr := p.Running; pr != nil {
			fmt.Fprintf(&b, " Running now: %s %s", pr.Kind, pr.Period)
			if pr.Stage != "" {
				fmt.Fprintf(&b, ", %s %d/%d", pr.Stage, pr.Done, pr.Total)
			}
			fmt.Fprintf(&b, ", since %s.", pr.Started.UTC().Format(time.RFC3339))
		} else if running := a.runningNow(); running != "" {
			fmt.Fprintf(&b, " Running now: %s.", running)
		}
		if !p.At.IsZero() {
			fmt.Fprintf(&b, " (State as of %s.)", p.At.UTC().Format(time.RFC3339))
		}
		if p.Paused != "" {
			fmt.Fprintf(&b, " Paused: %s.", p.Paused)
		}
		for _, l := range p.History {
			b.WriteString("\nSession history: " + l + ".")
		}
		if l := a.recallCounts(); l != "" {
			b.WriteString("\n" + l)
		}
		b.WriteString("\n" + a.modelText())
		b.WriteString("\nMemory source (" + a.memoryReading() + ").")
		return b.String(), nil
	case "enkeep_resettle", "enkeep_resettle_preview":
		if err := strictDecode(raw, &struct{}{}); err != nil {
			return "", err
		}
		r, err := a.c.ResettleWithin(a.localDay(), name == "enkeep_resettle", toolWait)
		if err != nil {
			return "", a.busy(err)
		}
		return resettleText(r), nil
	}
	return "", errors.New("unknown tool " + name)
}

// seedSwitches are the switches as the pages' snapshot has them.
func (a *app) seedSwitches() map[string]entry.SwitchSetting {
	st, _, done := a.c.Read()
	defer done()
	return st.Switches
}

// modelText says the model route and what it counted (docs/model-routes.md).
func (a *app) modelText() string {
	rt := a.route
	if rt == nil {
		return "Model route: off (model_route purpose): every call on the model of the extension's purpose in the hub's model settings."
	}
	var b strings.Builder
	primary := strings.Join(rt.Primary, " → ")
	if primary == "" {
		primary = "no primary model"
	}
	fmt.Fprintf(&b, "Model route: %s; fallback above %d estimated tokens %s, else %s.", primary, rt.LongAbove, rt.Long, rt.Short)
	if rt.Stats != nil {
		snap := rt.Stats.Snapshot()
		b.WriteString("\n" + snap.Summary())
		today := a.localDay()
		yesterday := today
		if t, err := time.Parse("2006-01-02", today); err == nil {
			yesterday = t.AddDate(0, 0, -1).Format("2006-01-02")
		}
		lines := snap.CostLines(today)
		b.WriteString("\n" + strings.Join(lines, "\n"))
		b.WriteString("\n" + snap.CostLines(yesterday)[0])
	}
	out, waiting, minute := model.Shared().Stats()
	fmt.Fprintf(&b, "\nCall limit: at most %d at once and %d a minute (reruns wait behind the daily and weekly passes); now %d out, %d waiting, %d in the last minute.",
		model.LimitConcurrent, model.LimitPerMinute, out, waiting, minute)
	if l := a.c.LedgerState(); l != "" {
		b.WriteString("\nLedger (assistant.entries.list): " + l + ".")
	}
	return b.String()
}

// localDay is the local day in the curator's time zone.
func (a *app) localDay() string {
	now := time.Now
	if a.c.Now != nil {
		now = a.c.Now
	}
	return now().In(a.c.Loc).Format("2006-01-02")
}

// span says a list of days briefly: "2026-10-29..2026-11-13 (16)" when they follow each other, else the list.
func span(days []string) string {
	if len(days) == 0 {
		return ""
	}
	consecutive := true
	for i := 1; i < len(days); i++ {
		a, _ := time.Parse("2006-01-02", days[i-1])
		if a.AddDate(0, 0, 1).Format("2006-01-02") != days[i] {
			consecutive = false
			break
		}
	}
	if consecutive && len(days) > 2 {
		return fmt.Sprintf("%s..%s (%d)", days[0], days[len(days)-1], len(days))
	}
	return strings.Join(days, ", ")
}

// resettleText says a resettle's result: the five statuses before and after, the stable entries per facet, and what
// moved.
func resettleText(r curate.ResettleReport) string {
	var b strings.Builder
	if r.Applied {
		fmt.Fprintf(&b, "Applied as of %s: %d entr(ies) changed status.", r.AsOf, r.Changed())
	} else {
		fmt.Fprintf(&b, "Dry run as of %s (nothing changed): %d entr(ies) would change status.", r.AsOf, r.Changed())
	}
	fmt.Fprintf(&b, " Active days: %d", r.ActiveDays)
	if len(r.DroppedDays) > 0 {
		fmt.Fprintf(&b, ", not counted (no model output): %s", span(r.DroppedDays))
	}
	b.WriteString(".\nStatus (before → after):")
	for _, s := range []entry.Status{entry.Stable, entry.Candidate, entry.Dormant, entry.Retired, entry.Shifting} {
		fmt.Fprintf(&b, " %s %d → %d;", s, r.Before[s], r.After[s])
	}
	b.WriteString("\nStable per facet (before → after):")
	for _, f := range entry.Facets {
		fmt.Fprintf(&b, " %s %d → %d;", f, r.StableBefore[f], r.StableAfter[f])
	}
	if len(r.Moves) > 0 {
		moves := make([]string, 0, len(r.Moves))
		for m, n := range r.Moves {
			moves = append(moves, fmt.Sprintf("%s %d", m, n))
		}
		sort.Strings(moves)
		b.WriteString("\nMoves: " + strings.Join(moves, ", ") + ".")
	}
	if !r.Applied && r.Changed()+len(r.DroppedDays) > 0 {
		b.WriteString("\nCall again with dry_run false to apply.")
	}
	return b.String()
}

// recallLog is the counts' file beside the state (store.RecallLog), opened on first use when init did not.
func (a *app) recallLog() *store.RecallLog {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.recalls == nil {
		a.recalls = store.OpenRecall(a.c.St.Dir, nil)
	}
	return a.recalls
}

// recallCounts says enkeep_recall's counts of the last 7 days, for enkeep_rerun_status: "" when it was never called.
func (a *app) recallCounts() string {
	keys, days := a.recallLog().Days(7)
	if len(keys) == 0 {
		return ""
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d call(s), %d empty", k, days[i].Calls, days[i].Empty)
	}
	return "enkeep_recall: " + strings.Join(parts, "; ") + "."
}

// markWrongTool answers enkeep_mark_wrong: each entry marked wrong by the assistant with the reason, then the exits
// compiled at once without the entries (as the page's "不对" does) and the state saved.
func (a *app) markWrongTool(ctx context.Context, raw json.RawMessage) (string, error) {
	ids, reason, err := markArgs(raw)
	if err != nil {
		return "", err
	}
	in := struct {
		IDs    []string
		Reason string
	}{ids, reason}
	if !a.c.St.LockWithin(toolWait) {
		return a.queueMarks(in.IDs, in.Reason)
	}
	defer a.c.St.Unlock()
	var marked, missing []string
	for _, id := range in.IDs {
		e, err := a.c.MarkWrongBy(id, "assistant", in.Reason)
		if err != nil {
			missing = append(missing, id)
			continue
		}
		marked = append(marked, fmt.Sprintf("%s (%s)", id, cut(recall.Line(e), 60)))
	}
	if len(marked) > 0 {
		a.withdraw(ctx)
		if err := a.c.St.Save(); err != nil {
			return "", err
		}
		a.c.Publish()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Marked wrong: %d entr(ies)", len(marked))
	if len(marked) > 0 {
		b.WriteString(": " + strings.Join(marked, "; "))
	}
	b.WriteString(".")
	if len(missing) > 0 {
		fmt.Fprintf(&b, " Not found: %s.", strings.Join(missing, ", "))
	}
	if len(marked) > 0 {
		b.WriteString(" They are out of the exits now (the assistant's block follows the hub's own schedule); the user can mark them right on the enkeep memory page.")
	}
	return b.String(), nil
}

func cut(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
