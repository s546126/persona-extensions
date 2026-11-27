package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/model"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/overview"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/profile"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
)

func TestMigrateV04Phase0BackupAndRollbackByteExact(t *testing.T) {
	dir := t.TempDir()
	origState := []byte(`{"version":1,"entries":{"e1":{"id":"e1","text":"hello"}}}`)
	origOv := []byte(`{"version":1,"patterns":{"ov:1":{"id":"ov:1","text":"p1"}}}`)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), origState, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, overview.File), origOv, 0o600); err != nil {
		t.Fatal(err)
	}
	// profile-summary.json is intentionally absent before migration.

	now := time.Date(2026, 11, 27, 4, 30, 0, 0, time.UTC)
	ts, bdir, err := BackupV04(dir, now)
	if err != nil {
		t.Fatalf("BackupV04: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bdir, "SHA256SUMS")); err != nil {
		t.Fatalf("missing SHA256SUMS: %v", err)
	}

	// Mutate existing files and create profile-summary.json as migration would.
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"mutated":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, overview.File), []byte(`{"mutated":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, profile.FileName), []byte(`{"new":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Rollback must restore exact original bytes and remove newly created profile-summary.json.
	if err := RollbackV04(dir, ts); err != nil {
		t.Fatalf("RollbackV04: %v", err)
	}
	gotState, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	gotOv, _ := os.ReadFile(filepath.Join(dir, overview.File))
	if string(gotState) != string(origState) {
		t.Fatalf("state.json mismatch after rollback: got %q want %q", gotState, origState)
	}
	if string(gotOv) != string(origOv) {
		t.Fatalf("overview-state.json mismatch after rollback: got %q want %q", gotOv, origOv)
	}
	if _, err := os.Stat(filepath.Join(dir, profile.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent file %s to be removed on rollback, err=%v", profile.FileName, err)
	}

	// Tampering with backup file must cause RollbackV04 to fail WITHOUT touching live state.json.
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"live":"keep"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bdir, "state.json"), []byte(`{"corrupted":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RollbackV04(dir, ts); err == nil {
		t.Fatal("expected checksum mismatch error on tampered backup")
	}
	stillLive, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if string(stillLive) != `{"live":"keep"}` {
		t.Fatalf("failed rollback modified live state.json: got %q", stillLive)
	}
}

func TestMigrateV04Phase1DeterministicAndIdempotent(t *testing.T) {
	today := "2026-11-27"
	st := &store.State{
		ActiveDays: []string{today},
		Contacts: map[string]*entry.Contact{
			"person:hr": {Person: "person:hr", Name: "HR"},
		},
		Entries: map[string]*entry.Entry{
			// 1. S6 handwritten -> assistant_note
			"s6-1": {
				ID: "s6-1", Facet: entry.Interaction, Grain: entry.Action,
				Text: "长材料只给一句结论", Origin: entry.Handwritten, Status: entry.Stable, Last: today,
			},
			// 2a. S3-only persona claiming supervisor -> retired
			"u-sup": {
				ID: "u-sup", Facet: entry.Persona, Grain: entry.Action,
				Text: "用户担任 Persona 平台主管，负责统筹派活", Origin: entry.Behavior, Status: entry.Stable, Last: today,
				Evidence: []entry.Evidence{{Source: entry.SrcAgent, Ref: "agents/m/s1", Day: today, Quote: "我是平台主管"}},
			},
			// 2b. S3-only benign persona -> candidate
			"u-s3-other": {
				ID: "u-s3-other", Facet: entry.Persona, Grain: entry.Action,
				Text: "偏好简洁输出风格", Origin: entry.Behavior, Status: entry.Stable, Last: today,
				Evidence: []entry.Evidence{{Source: entry.SrcAgent, Ref: "agents/m/s2", Day: today, Quote: "简洁点"}},
			},
			// 2c. S1+S3 persona -> kept stable
			"u-s1": {
				ID: "u-s1", Facet: entry.Persona, Grain: entry.Action,
				Text: "关注系统长期可维护性", Origin: entry.Behavior, Status: entry.Stable, Last: today,
				Evidence: []entry.Evidence{
					{Source: entry.SrcAssistant, Ref: "chat/1", Day: today, Who: entry.WhoSelf, Quote: "要可维护"},
					{Source: entry.SrcAgent, Ref: "agents/m/s3", Day: today, Quote: "可维护"},
				},
			},
			// 3. Cron workflow -> retired
			"w-cron": {
				ID: "w-cron", Facet: entry.Workflow, Grain: entry.Action,
				Text: "每日导出用量数据：先采集再校验，把摘要文本发给用户", Origin: entry.Behavior, Status: entry.Stable, Last: today,
			},
			// 4a. Generic HR people noise -> dormant + ephemeral
			"p-hr": {
				ID: "p-hr", Facet: entry.People, Grain: entry.Action, Person: "person:hr",
				Text: "联系 HR 跟进面试反馈", Origin: entry.Behavior, Status: entry.Stable, Last: today,
			},
			// 4b. Old world status (>30d) -> dormant + ephemeral
			"w-status": {
				ID: "w-status", Facet: entry.World, Grain: entry.Action, Type: "status",
				Text: "PR #1234 正在排查中", Origin: entry.Behavior, Status: entry.Stable, Last: "2026-10-01",
			},
			// 6a. Misclassified emotion work rule -> cleared
			"i-emo-fp": {
				ID: "i-emo-fp", Facet: entry.Interaction, Grain: entry.Action,
				Text: "反复发问会让用户不满，嫌解释绕要求说人话", Origin: entry.Behavior, Status: entry.Stable, Last: today,
				Categories: []entry.Category{{Name: entry.CatEmotion, By: entry.ByModel}},
			},
			// 6b. Misclassified health work rule from health space -> cleared + space cleared
			"i-health-fp": {
				ID: "i-health-fp", Facet: entry.Interaction, Grain: entry.Action,
				Text: "要的是例行方案不要一次性导出", Origin: entry.Behavior, Status: entry.Stable, Last: today,
				OriginSpace: "health",
				Categories:  []entry.Category{{Name: entry.CatHealth, By: entry.BySpaceDefault}},
			},
			// 6c. User-tagged sensitivity must be preserved
			"i-user-sens": {
				ID: "i-user-sens", Facet: entry.Interaction, Grain: entry.Action,
				Text: "普通工作规范但用户手工标了敏感", Origin: entry.Behavior, Status: entry.Stable, Last: today,
				Categories: []entry.Category{{Name: entry.CatFinance, By: entry.ByUser}},
			},
			// 7. Ops runbook (tools/host) older than 60d -> tagged ops_runbook & decayed to dormant
			"i-ops-old": {
				ID: "i-ops-old", Facet: entry.Interaction, Grain: entry.Action,
				Text: "在 sj-wsl 上运行 scripts/ci/fast.sh 跑门禁，不要在 Mac 上跑 go test", Origin: entry.Behavior, Status: entry.Stable, Last: "2026-09-01",
			},
		},
		Graph: &store.Graph{
			Nodes: map[string]*store.Node{
				"n-task-1":   {ID: "n-task-1", Type: "task", Name: "修 bug", Status: "active"},
				"n-person-1": {ID: "n-person-1", Type: "person", Name: "person:张伟", Status: "active"},
				"n-person-2": {ID: "n-person-2", Type: "person", Name: "张伟", Status: "active"},
				"n-person-3": {ID: "n-person-3", Type: "person", Name: "刘旭钦", Status: "active"},
				"n-person-4": {ID: "n-person-4", Type: "person", Name: "旭钦", Status: "active"},
			},
			Assertions: map[string]*store.Assertion{
				"a1": {ID: "a1", S: "n-person-2", P: "about", O: "n-task-1", Status: "active"},
				"a2": {ID: "a2", S: "n-person-2", P: "collaborates_with", O: "n-person-3", Status: "active"},
				"a3": {ID: "a3", S: "n-person-3", P: "collaborates_with", O: "n-person-2", Status: "active"},
			},
		},
	}

	rep1, err := RunMigrateV04Phase1(context.Background(), st, today)
	if err != nil {
		t.Fatalf("Phase 1 run 1: %v", err)
	}
	if rep1.S6Downgraded != 1 || st.Entries["s6-1"].Origin != entry.OriginAssistantNote {
		t.Fatalf("S6 downgrade failed: rep=%+v origin=%s", rep1, st.Entries["s6-1"].Origin)
	}
	if rep1.S3PersonaRetired != 1 || st.Entries["u-sup"].Status != entry.Retired {
		t.Fatalf("S3 supervisor retire failed: rep=%+v status=%s", rep1, st.Entries["u-sup"].Status)
	}
	if rep1.S3PersonaDemoted != 1 || st.Entries["u-s3-other"].Status != entry.Candidate {
		t.Fatalf("S3 benign demote failed: rep=%+v status=%s", rep1, st.Entries["u-s3-other"].Status)
	}
	if st.Entries["u-s1"].Status != entry.Stable {
		t.Fatalf("S1+S3 persona should remain stable, got %s", st.Entries["u-s1"].Status)
	}
	if rep1.CronWorkflowRetired != 1 || st.Entries["w-cron"].Status != entry.Retired {
		t.Fatalf("Cron workflow retire failed: rep=%+v status=%s", rep1, st.Entries["w-cron"].Status)
	}
	if rep1.GenericPeopleDemoted != 1 || st.Entries["p-hr"].Status != entry.Dormant {
		t.Fatalf("Generic HR people demote failed: rep=%+v status=%s", rep1, st.Entries["p-hr"].Status)
	}
	if st.Entries["w-status"].Status != entry.Dormant {
		t.Fatalf("Old world status should decay to dormant, got %s", st.Entries["w-status"].Status)
	}
	if st.Entries["i-emo-fp"].IsSensitive() {
		t.Fatalf("i-emo-fp should be released from emotion, cats=%v", st.Entries["i-emo-fp"].Cats())
	}
	if st.Entries["i-health-fp"].IsSensitive() || st.Entries["i-health-fp"].OriginSpace != "" {
		t.Fatalf("i-health-fp should be released from health & space, cats=%v space=%q", st.Entries["i-health-fp"].Cats(), st.Entries["i-health-fp"].OriginSpace)
	}
	if !st.Entries["i-user-sens"].IsSensitive() {
		t.Fatalf("ByUser sensitivity must be preserved")
	}
	if st.Entries["i-ops-old"].MemoryClass != entry.ClassOpsRunbook || st.Entries["i-ops-old"].Status != entry.Dormant {
		t.Fatalf("i-ops-old expected ops_runbook+dormant, got class=%s status=%s", st.Entries["i-ops-old"].MemoryClass, st.Entries["i-ops-old"].Status)
	}
	if rep1.Graph == nil || rep1.Graph.ArchivedTasks != 1 || rep1.Graph.ArchivedAbout != 1 || rep1.Graph.TotalMerged < 2 || rep1.Graph.DedupedCollabEdges != 1 {
		t.Fatalf("unexpected graph report: %+v", rep1.Graph)
	}

	// Second run must be strictly idempotent (zero new mutations).
	rep2, err := RunMigrateV04Phase1(context.Background(), st, today)
	if err != nil {
		t.Fatalf("Phase 1 run 2: %v", err)
	}
	if rep2.S6Downgraded != 0 || rep2.S3PersonaRetired != 0 || rep2.S3PersonaDemoted != 0 ||
		rep2.CronWorkflowRetired != 0 || rep2.GenericPeopleDemoted != 0 ||
		rep2.OpsRunbookTagged != 0 || rep2.SensitiveWorkCleared != 0 ||
		rep2.Graph.TotalMerged != 0 || rep2.Graph.ArchivedTasks != 0 || rep2.Graph.ArchivedAbout != 0 {
		t.Fatalf("Phase 1 is not idempotent: run2=%+v graph=%+v", rep2, rep2.Graph)
	}
}

type stubRateModel struct {
	mu       sync.Mutex
	inFlight int
	maxSeen  int
	calls    int
	failWith error
	delay    time.Duration
	handler  func(r model.Request) (model.Response, error)
}

func (s *stubRateModel) Complete(ctx context.Context, r model.Request) (model.Response, error) {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.maxSeen {
		s.maxSeen = s.inFlight
	}
	s.calls++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.failWith != nil {
		return model.Response{}, s.failWith
	}
	if s.handler != nil {
		return s.handler(r)
	}
	return model.Response{Text: `{"items":[]}`}, nil
}

func TestMigrateV04RateLimiterAndAbortOnQuota(t *testing.T) {
	// 1. Concurrency cap <= 4 and hard clamping even if caller asks for 10/100.
	base := &stubRateModel{delay: 15 * time.Millisecond}
	lim := &model.Limiter{Concurrent: 10, PerMinute: 100, Window: 100 * time.Millisecond}
	g := newMigrateGuardedModel(base, lim, 20, 11)
	if g.limit.Concurrent > migrateV04MaxConcHard || g.limit.PerMinute > migrateV04MaxRPMHard {
		t.Fatalf("limiter not clamped: conc=%d rpm=%d", g.limit.Concurrent, g.limit.PerMinute)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = g.Complete(context.Background(), model.Request{Step: "test", Tier: model.Small})
		}()
	}
	wg.Wait()
	if base.maxSeen > migrateV04MaxConcHard {
		t.Fatalf("exceeded max concurrency %d: saw %d", migrateV04MaxConcHard, base.maxSeen)
	}

	// 2. Quota/auth error must abort immediately and prevent any subsequent call.
	var calls int32
	quotaBase := &stubRateModel{
		handler: func(r model.Request) (model.Response, error) {
			atomic.AddInt32(&calls, 1)
			return model.Response{}, errors.New("HTTP 429 rate_limit_exceeded")
		},
	}
	g2 := newMigrateGuardedModel(quotaBase, &model.Limiter{Concurrent: 2, PerMinute: 15}, 20, 11)
	_, err1 := g2.Complete(context.Background(), model.Request{Step: "s1", Tier: model.Small})
	if !errors.Is(err1, ErrMigrateQuotaOrAuth) {
		t.Fatalf("expected ErrMigrateQuotaOrAuth, got %v", err1)
	}
	_, err2 := g2.Complete(context.Background(), model.Request{Step: "s2", Tier: model.Small})
	if !errors.Is(err2, ErrMigrateQuotaOrAuth) {
		t.Fatalf("expected immediate abort on second call, got %v", err2)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("expected exactly 1 upstream call before abort, got %d", calls)
	}
}

func TestMigrateV04Phase2MockHaiku(t *testing.T) {
	today := "2026-11-27"
	st := &store.State{
		ActiveDays: []string{today},
		Entries: map[string]*entry.Entry{
			"p-sens-fp": {
				ID: "p-sens-fp", Facet: entry.Persona, Grain: entry.Action,
				Text: "工作上注重效率", Status: entry.Stable, Last: today, Count: 5,
				Categories: []entry.Category{{Name: entry.CatEmotion, By: entry.ByModel}},
			},
			"i-rule-ops": {
				ID: "i-rule-ops", Facet: entry.Interaction, Grain: entry.Class,
				Text: "发版前跑一遍远端构建节点检查", Status: entry.Stable, First: "2026-10-01", Last: today, Count: 8,
				Kinds: []string{entry.SrcAssistant, entry.SrcAsk}, ActiveDays: []string{"2026-10-01", "2026-11-01", "2026-11-20", "2026-11-25", today},
				Leverage: &entry.Leverage{Reach: entry.ReachEveryTurn, Horizon: entry.HorizonLasting},
				Evidence: []entry.Evidence{
					{Source: entry.SrcAssistant, Ref: "chat/1", Day: "2026-10-01", Who: entry.WhoSelf, Quote: "发版前跑远端构建节点"},
					{Source: entry.SrcAsk, Ref: "ask/1", Day: today, Who: entry.WhoSelf, Quote: "先跑远端构建节点检查"},
				},
			},
			"i-rule-univ": {
				ID: "i-rule-univ", Facet: entry.Interaction, Grain: entry.Class,
				Text: "先查原始证据再给结论，不要猜测", Status: entry.Stable, First: "2026-10-01", Last: today, Count: 8,
				Kinds: []string{entry.SrcAssistant, entry.SrcAsk}, ActiveDays: []string{"2026-10-01", "2026-11-01", "2026-11-20", "2026-11-25", today},
				Leverage: &entry.Leverage{Reach: entry.ReachEveryTurn, Horizon: entry.HorizonLasting},
				Evidence: []entry.Evidence{
					{Source: entry.SrcAssistant, Ref: "chat/2", Day: "2026-10-01", Who: entry.WhoSelf, Quote: "先查原始证据再给结论"},
					{Source: entry.SrcAsk, Ref: "ask/2", Day: today, Who: entry.WhoSelf, Quote: "不要猜测，先查证据"},
				},
			},
		},
		Graph: &store.Graph{
			Nodes: map[string]*store.Node{
				"n-sys-1": {ID: "n-sys-1", Type: "system", Name: "astergate", Status: "active"},
				"n-sys-2": {ID: "n-sys-2", Type: "system", Name: "aster", Status: "active"},
			},
			Assertions: map[string]*store.Assertion{},
		},
	}

	mockM := &stubRateModel{
		handler: func(r model.Request) (model.Response, error) {
			switch r.Step {
			case "migrate_v04_sens_audit":
				return model.Response{Text: `{"items":[{"id":"p-sens-fp","verdict":"false_positive","confidence":"high","reason":"work efficiency, not private emotion"}]}`}, nil
			case "migrate_v04_boundary_rules":
				return model.Response{Text: `{"items":[{"id":"i-rule-ops","memory_class":"ops_runbook","reason":"remote build host"},{"id":"i-rule-univ","memory_class":"universal_contract","reason":"general evidence discipline"}]}`}, nil
			default:
				// Tier 2 graph disambiguation
				return model.Response{Text: `{"pairs":[{"pair_id":0,"verdict":"same_entity","confidence":"high","reason":"short alias of astergate"}]}`}, nil
			}
		},
	}

	rep, err := RunMigrateV04Phase2(context.Background(), st, mockM, today, nil)
	if err != nil {
		t.Fatalf("Phase 2: %v", err)
	}
	if rep.SensitiveFalsePositives != 1 || st.Entries["p-sens-fp"].IsSensitive() {
		t.Fatalf("expected p-sens-fp sensitivity cleared, rep=%+v cats=%v", rep, st.Entries["p-sens-fp"].Cats())
	}
	if st.Entries["i-rule-ops"].MemoryClass != entry.ClassOpsRunbook || !slices.Contains(st.Entries["i-rule-ops"].Flags, entry.FlagOpsRunbook) {
		t.Fatalf("expected i-rule-ops tagged ops_runbook, got class=%s flags=%v", st.Entries["i-rule-ops"].MemoryClass, st.Entries["i-rule-ops"].Flags)
	}
	if st.Entries["i-rule-univ"].MemoryClass != entry.ClassUniversalContract {
		t.Fatalf("expected i-rule-univ tagged universal_contract, got %s", st.Entries["i-rule-univ"].MemoryClass)
	}
	if rep.HaikuCalls < 2 {
		t.Fatalf("expected at least 2 Haiku calls, got %d", rep.HaikuCalls)
	}
}

func TestReviewBundleGeneratesAll6ArtifactsAndColdstart(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(t.TempDir(), "bundle")
	today := "2026-11-27"

	st := &store.State{
		Version:    store.Version,
		ActiveDays: []string{"2026-11-10", "2026-11-20", today},
		Contacts: map[string]*entry.Contact{
			"person:lxq": {Person: "person:lxq", Name: "刘旭钦", Score: 50, First: "2026-11-01", Last: today},
		},
		Entries: map[string]*entry.Entry{
			"i-t1-1": {
				ID: "i-t1-1", Facet: entry.Interaction, Grain: entry.Class,
				Text: "事实断言必须先查原始证据再下结论", Origin: entry.Behavior, Status: entry.Stable,
				First: "2026-11-01", Last: today, Count: 6,
				ActiveDays: []string{"2026-11-01", "2026-11-10", "2026-11-20", today},
				Kinds:      []string{entry.SrcAssistant, entry.SrcAsk},
				Evidence:   []entry.Evidence{{Source: entry.SrcAssistant, Ref: "chat/1", Day: today, Who: entry.WhoSelf, Quote: "先查原始证据再说"}},
			},
			"s6-note": {
				ID: "s6-note", Facet: entry.Interaction, Grain: entry.Action,
				Text: "主助手反思笔记不应决定冷启动", Origin: entry.OriginAssistantNote, Status: entry.Stable,
				First: "2026-11-01", Last: today, Count: 1,
			},
			"p-health-real": {
				ID: "p-health-real", Facet: entry.Persona, Grain: entry.Action,
				Text: "近期体检提示静息心率偏高", Origin: entry.Behavior, Status: entry.Stable,
				First: "2026-11-20", Last: today, Count: 2,
				Categories: []entry.Category{{Name: entry.CatHealth, By: entry.ByModel}},
			},
		},
		Graph: &store.Graph{
			Nodes: map[string]*store.Node{
				"n-p1": {ID: "n-p1", Type: "person", Name: "刘旭钦", Status: "active"},
				"n-p2": {ID: "n-p2", Type: "person", Name: "旭钦", MergedInto: "n-p1", Status: "active"},
				"n-t1": {ID: "n-t1", Type: "task", Name: "一次性任务", Status: "archived"},
			},
			Assertions: map[string]*store.Assertion{
				"a1": {ID: "a1", S: "n-p1", P: "about", O: "n-t1", Status: "archived"},
			},
			Merges: []*store.MergeRec{
				{From: "n-p2", Into: "n-p1", At: today, Proposal: "auto-tier1-cn-short-v04"},
			},
		},
	}
	rawState, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), rawState, 0o600); err != nil {
		t.Fatal(err)
	}

	ovH, err := overview.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ovH.Update(func(s *overview.State) (bool, error) {
		s.Patterns["ov:pc1"] = &overview.Pattern{
			ID: "ov:pc1", Facet: string(entry.Persona), Kind: overview.KindProfileCore,
			Lifecycle: overview.Core, Rooted: true,
			Text:         "不要把研究管理方法论等同于已经是全职管理者",
			EvidenceIDs:  []string{"i-t1-1"},
			ScenesCount:  3,
			SourcesCount: 4,
			UniqueQuotes: 4,
			ActiveWeeks:  []string{"2026-W46", "2026-W47"},
		}
		s.Patterns["ov:p1"] = &overview.Pattern{
			ID: "ov:p1", Facet: string(entry.Judgement), Kind: overview.KindPrinciple,
			Lifecycle: overview.Core, Rooted: true,
			Text: "执行语境直接推进到底，设计语境先对齐方案",
			Poles: []overview.Pole{
				{When: "执行已定方向", Choice: "直接推进到底"},
				{When: "架构设计或协议变更", Choice: "先出方案对齐"},
			},
			EvidenceIDs:  []string{"i-t1-1"},
			ScenesCount:  3,
			SourcesCount: 5,
			UniqueQuotes: 4,
			ActiveWeeks:  []string{"2026-W46", "2026-W47"},
		}
		s.Patterns["ov:c1"] = &overview.Pattern{
			ID: "ov:c1", Facet: string(entry.Interaction), Kind: overview.KindContract,
			Lifecycle: overview.Core, Hard: true, Polarity: overview.PolarityCheck,
			Text:         "事实断言先查原始记录，不凭记忆猜测",
			EvidenceIDs:  []string{"i-t1-1"},
			ScenesCount:  3,
			SourcesCount: 6,
			UniqueQuotes: 5,
			ActiveWeeks:  []string{"2026-W46", "2026-W47"},
		}
		s.Patterns["ov:ops1"] = &overview.Pattern{
			ID: "ov:ops1", Facet: string(entry.Tools), Kind: overview.KindOpsRunbook,
			Lifecycle:    overview.Core,
			Text:         "在 sj-wsl 上运行 fast 门禁",
			EvidenceIDs:  []string{"i-t1-1"},
			ScenesCount:  2,
			SourcesCount: 3,
			UniqueQuotes: 3,
			ActiveWeeks:  []string{"2026-W46", "2026-W47"},
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	sum, err := GenerateReviewBundle(dir, outDir, "zh-Hans", 4096, today)
	if err != nil {
		t.Fatalf("GenerateReviewBundle: %v", err)
	}
	wantFiles := []string{
		"resident-v04.md",
		"principles-v04.md",
		"profile-v04.md",
		"entities-v04.md",
		"sensitive-v04.md",
		"coldstart-v04.md",
		"summary-v04.json",
	}
	for _, f := range wantFiles {
		b, err := os.ReadFile(filepath.Join(outDir, f))
		if err != nil || len(b) == 0 {
			t.Fatalf("missing or empty artifact %s: err=%v len=%d", f, err, len(b))
		}
	}
	if sum.ResidentLines == 0 || sum.ResidentSensitiveHits != 0 || sum.ResidentOpsHits != 0 || sum.ResidentS6Hits != 0 {
		t.Fatalf("unexpected resident summary: %+v", sum)
	}
	if sum.ResidentProfileLines == 0 || sum.ResidentPrincipleLines == 0 || sum.ResidentContractLines == 0 {
		t.Fatalf("expected all three resident sections non-empty: profile=%d principle=%d contract=%d",
			sum.ResidentProfileLines, sum.ResidentPrincipleLines, sum.ResidentContractLines)
	}
	if sum.ProfileCoreUsed != 1 || sum.PrinciplesUsed != 1 || sum.PrinciplesWithPoles != 1 || sum.ContractsUsed != 1 || sum.OpsRunbooksUsed != 1 {
		t.Fatalf("unexpected L1 kind counts: %+v", sum)
	}
	if sum.GraphMerges != 1 || sum.GraphTasksArchived != 1 || sum.GraphAboutArchived != 1 {
		t.Fatalf("unexpected graph summary: %+v", sum)
	}
	if sum.SensitiveByCat[entry.CatHealth] != 1 {
		t.Fatalf("expected 1 health sensitive entry, got %+v", sum.SensitiveByCat)
	}
	if sum.ColdstartLines == 0 || sum.ColdstartProfileLines == 0 || sum.ColdstartPrincipleLines == 0 || sum.ColdstartContractLines == 0 {
		t.Fatalf("expected non-empty 3-section coldstart standing output, got %+v", sum)
	}
	coldRaw, _ := os.ReadFile(filepath.Join(outDir, "coldstart-v04.md"))
	if !strings.Contains(string(coldRaw), "剔除全部 S6 笔记（共移除 1 条）") {
		t.Fatalf("coldstart report missing S6 strip evidence:\n%s", coldRaw)
	}
}

func TestMigrateV04DefaultsAndAtomicPhase2Save(t *testing.T) {
	if migrateV04DefaultMaxHaiku != 32 {
		t.Fatalf("migrateV04DefaultMaxHaiku = %d, want 32", migrateV04DefaultMaxHaiku)
	}
	if migrateV04DefaultMaxSonnet != 15 {
		t.Fatalf("migrateV04DefaultMaxSonnet = %d, want 15", migrateV04DefaultMaxSonnet)
	}
	if migrateV04DefaultSonnetModel != "claude-sonnet-5-5" {
		t.Fatalf("migrateV04DefaultSonnetModel = %q, want claude-sonnet-5-5", migrateV04DefaultSonnetModel)
	}

	dir := t.TempDir()
	today := "2026-11-27"
	st := &store.State{
		Version:    store.Version,
		ActiveDays: []string{today},
		Entries: map[string]*entry.Entry{
			"s6-1": {
				ID: "s6-1", Facet: entry.Interaction, Grain: entry.Action,
				Text: "长材料只给一句结论", Origin: entry.Handwritten, Status: entry.Stable, Last: today,
			},
			"p-sens": {
				ID: "p-sens", Facet: entry.Persona, Grain: entry.Action,
				Text: "工作上注重效率", Status: entry.Stable, Last: today, Count: 5,
				Categories: []entry.Category{{Name: entry.CatEmotion, By: entry.ByModel}},
			},
		},
	}
	origBytes, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, origBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// When Apply=true and MaxPhase=2 with a failing Phase 2 model, state.json must remain untouched on disk.
	failingModel := &stubRateModel{failWith: errors.New("HTTP 429 rate_limit_exceeded")}
	_, err = RunMigrateV04(context.Background(), MigrateV04Options{
		StateDir: dir,
		Apply:    true,
		MaxPhase: 2,
		Today:    today,
		Model:    failingModel,
	})
	if err == nil {
		t.Fatal("expected Phase 2 error")
	}
	afterFail, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterFail) != string(origBytes) {
		t.Fatalf("state.json was modified despite Phase 2 failure")
	}

	// When MaxPhase=1 (no Phase 2), Phase 1 must still save state.json to disk.
	_, err = RunMigrateV04(context.Background(), MigrateV04Options{
		StateDir: dir,
		Apply:    true,
		MaxPhase: 1,
		Today:    today,
	})
	if err != nil {
		t.Fatalf("MaxPhase=1 apply failed: %v", err)
	}
	reloaded, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.S.Entries["s6-1"].Origin != entry.OriginAssistantNote {
		t.Fatalf("expected Phase 1 save on MaxPhase=1, got origin=%s", reloaded.S.Entries["s6-1"].Origin)
	}
}
