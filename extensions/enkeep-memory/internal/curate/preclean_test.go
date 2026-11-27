package curate

import (
	"testing"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/entry"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/kbsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/norm"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/relay"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/schema"
	srcs "github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/sources"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/triage"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/verify"
)

func TestPrecleanUnitsStripsBoilerplateAndMultiSessionTemplates(t *testing.T) {
	tplLine := "请严格按照占位派活模板执行，完成后汇报结果和验证命令。"
	units := []Unit{
		{
			Ref: "agents/m1/s1", Entity: srcs.KindAgents, Day: "2026-11-20",
			Lines: []norm.Line{
				{N: 1, Speaker: srcs.SpeakerPersona, Body: "<persona-context>背景</persona-context>\nKeep hub credentials private."},
				{N: 2, Speaker: srcs.SpeakerPersona, Body: tplLine},
				{N: 3, Speaker: norm.Me, Body: "这次先只改占位模块的超时参数"},
			},
		},
		{
			Ref: "agents/m1/s2", Entity: srcs.KindAgents, Day: "2026-11-20",
			Lines: []norm.Line{
				{N: 1, Speaker: srcs.SpeakerPersona, Body: tplLine},
			},
		},
		{
			Ref: "agents/m1/s3", Entity: srcs.KindAgents, Day: "2026-11-20",
			Lines: []norm.Line{
				{N: 1, Speaker: norm.Me, Body: tplLine},
				{N: 2, Speaker: norm.Me, Body: relay.RelayedMark + "别动线上配置，先跑单测"},
			},
		},
		{
			Ref: "chats/persona-c1", Entity: "chats", Day: "2026-11-20",
			Lines: []norm.Line{
				{N: 1, Speaker: norm.Me, Body: "主对话里的占位要求"},
			},
		},
	}

	got, stats := PrecleanUnits(units, nil)
	if stats.BoilerplateLines != 1 {
		t.Errorf("BoilerplateLines = %d, want 1", stats.BoilerplateLines)
	}
	if stats.TemplateLines != 3 {
		t.Errorf("TemplateLines = %d, want 3", stats.TemplateLines)
	}
	if stats.S3UnitsDropped != 1 {
		t.Errorf("S3UnitsDropped = %d, want 1 (s2 became empty)", stats.S3UnitsDropped)
	}
	if len(got) != 3 {
		t.Fatalf("remaining units = %d, want 3", len(got))
	}
	if len(got[0].Lines) != 1 || got[0].Lines[0].Body != "这次先只改占位模块的超时参数" {
		t.Errorf("s1 lines = %+v", got[0].Lines)
	}
	if len(got[1].Lines) != 1 || got[1].Lines[0].Body != relay.RelayedMark+"别动线上配置，先跑单测" {
		t.Errorf("s3 lines = %+v", got[1].Lines)
	}
}

func TestS3ScopingAllowsToolsAndWhoSelfInteractionJudgementOnly(t *testing.T) {
	c := &Curator{Cfg: Config{Language: "zh-Hans"}}
	u := &Unit{
		Ref:    "agents/m1/s1",
		Entity: srcs.KindAgents,
		Meta:   kbsrc.Meta{Kind: "agent", Name: "agent-s1"},
		Day:    "2026-11-20",
		Lines: []norm.Line{
			{N: 1, Speaker: norm.Me, Body: "在占位项目里别自动合并，先给我看结果"},
			{N: 2, Speaker: srcs.SpeakerAgent, Body: "工具 host-a 跑构建需要先设置环境变量"},
		},
		Who: map[int]string{1: entry.WhoSelf},
	}
	w := &verify.Window{Chat: u.Ref, Day: u.Day, Kind: "agent", All: map[int]norm.Line{1: u.Lines[0], 2: u.Lines[1]}, Shown: map[int]bool{1: true, 2: true}}

	// Workflow, Persona, People are disallowed from S3.
	for _, facet := range []string{string(entry.Workflow), string(entry.Persona), string(entry.People)} {
		r := c.checkObservation(w, u, nil, triage.TierOwn, schema.Observation{
			Facet: facet, Text: "用户在占位项目里习惯先看结果", Domain: entry.Work, Confidence: "high",
			Choice: "先看结果", Evidence: []schema.Evidence{{N: 1, Quote: "先给我看结果"}},
			Impact: schema.ObsImpact{Moment: "dispatch", Action: "先汇报再合并", Object: "占位项目"},
		})
		if r.rej != obsFacetNotAllowed {
			t.Errorf("S3 facet %s: rej = %q, want %q", facet, r.rej, obsFacetNotAllowed)
		}
	}

	// Tools from agent report is allowed.
	rTool := c.checkObservation(w, u, nil, triage.TierOwn, schema.Observation{
		Facet: string(entry.Tools), Object: "host-a", Text: "在 host-a 上跑构建需要先设置环境变量",
		Domain: entry.Work, Confidence: "high", Evidence: []schema.Evidence{{N: 2, Quote: "工具 host-a 跑构建需要先设置环境变量"}},
		Impact: schema.ObsImpact{Moment: "use_object", Action: "构建前设置环境变量", Object: "host-a"},
	})
	if rTool.rej != "" {
		t.Errorf("S3 tools rejected: %q", rTool.rej)
	}

	// Interaction from WhoSelf is allowed.
	rInt := c.checkObservation(w, u, nil, triage.TierOwn, schema.Observation{
		Facet: string(entry.Interaction), Text: "在占位项目里不要自动合并，先给用户看结果",
		Situation: "占位项目改动完成", Choice: "先给用户看结果再合并", Domain: entry.Work, Confidence: "high",
		Evidence: []schema.Evidence{{N: 1, Quote: "在占位项目里别自动合并，先给我看结果"}},
		Impact:   schema.ObsImpact{Moment: "dispatch", Action: "合并前先汇报结果", Object: "占位项目"},
	})
	if rInt.rej != "" {
		t.Errorf("S3 WhoSelf interaction rejected: %q", rInt.rej)
	}

	// Interaction from agent line (not WhoSelf) is rejected.
	rAgentInt := c.checkObservation(w, u, nil, triage.TierOwn, schema.Observation{
		Facet: string(entry.Interaction), Text: "在 host-a 上构建前设置环境变量",
		Situation: "构建", Choice: "先设置环境变量", Domain: entry.Work, Confidence: "high",
		Evidence: []schema.Evidence{{N: 2, Quote: "工具 host-a 跑构建需要先设置环境变量"}},
		Impact:   schema.ObsImpact{Moment: "dispatch", Action: "先设置环境变量", Object: "host-a"},
	})
	if rAgentInt.rej == "" {
		t.Errorf("S3 non-WhoSelf interaction should be rejected")
	}

	_ = store.State{}
}

func TestPrecleanStripsCronAndScheduledPrompts(t *testing.T) {
	units := []Unit{
		{
			Ref: "chats/persona-cron-1", Entity: "chats", Day: "2026-11-20",
			Meta: kbsrc.Meta{Kind: "scheduled", Name: "每日用量导出"},
			Lines: []norm.Line{
				{N: 1, Speaker: norm.Me, Body: "每日导出用量数据：先采集再校验，把摘要文本发给用户"},
			},
		},
		{
			Ref: "chats/persona-mixed", Entity: "chats", Day: "2026-11-20",
			Meta: kbsrc.Meta{Kind: "assistant", Name: "主对话"},
			Lines: []norm.Line{
				{N: 1, Speaker: norm.Me, Body: "每日导出用量数据：先采集再校验，把摘要文本发给用户"},
				{N: 2, Speaker: norm.Me, Body: "0 8 * * * 执行定时巡检脚本，无异常则静默"},
				{N: 3, Speaker: norm.Me, Body: "帮我把这篇设计文档的摘要发给我看看"},
			},
		},
		{
			Ref: "agents/m1/s-cron", Entity: srcs.KindAgents, Day: "2026-11-20",
			Lines: []norm.Line{
				{N: 1, Speaker: srcs.SpeakerPersona, Body: "定时任务触发：批量巡检全部仓库并把结果推送给用户"},
			},
		},
	}

	got, stats := PrecleanUnits(units, nil)
	if stats.CronUnitsDropped != 2 {
		t.Errorf("CronUnitsDropped = %d, want 2", stats.CronUnitsDropped)
	}
	if stats.CronLines != 4 {
		t.Errorf("CronLines = %d, want 4", stats.CronLines)
	}
	if len(got) != 1 || got[0].Ref != "chats/persona-mixed" {
		t.Fatalf("remaining units = %+v, want only chats/persona-mixed", got)
	}
	if len(got[0].Lines) != 1 || got[0].Lines[0].Body != "帮我把这篇设计文档的摘要发给我看看" {
		t.Errorf("human line should survive cron preclean: %+v", got[0].Lines)
	}
}

func TestS3S4AuthorityAndCronObserveGates(t *testing.T) {
	c, _ := newCurator(t, nil, "shadow")
	// 1. S3 AI supervisor self-description must be rejected from persona.
	uS3 := &Unit{
		Ref: "agents/m1/sup-1", Entity: srcs.KindAgents,
		Meta: kbsrc.Meta{Kind: "agent", Name: "supervisor-session"}, Day: "2026-11-20",
		Lines: []norm.Line{
			{N: 1, Speaker: srcs.SpeakerAgent, Body: "我担任占位平台主管和网关统筹，负责派活与验收。"},
			{N: 2, Speaker: norm.Me, Body: "我看看当前架构方案"},
			{N: 3, Speaker: norm.Me, Body: "别再把运维脚本写进全局规范，改放到工具手册里"},
			{N: 4, Speaker: norm.Me, Body: relay.RelayedMark + "别再把运维脚本写进全局规范"},
		},
		Who: map[int]string{2: entry.WhoSelf, 3: entry.WhoSelf, 4: entry.WhoSelf},
	}
	wS3 := &verify.Window{
		Chat: uS3.Ref, Day: uS3.Day, Kind: "agent",
		All:   map[int]norm.Line{1: uS3.Lines[0], 2: uS3.Lines[1], 3: uS3.Lines[2], 4: uS3.Lines[3]},
		Shown: map[int]bool{1: true, 2: true, 3: true, 4: true},
	}
	rSup := c.checkObservation(wS3, uS3, nil, triage.TierOwn, schema.Observation{
		Facet: string(entry.Persona), Type: "role", Text: "用户担任占位平台主管与网关统筹",
		Domain: entry.Work, Confidence: "high",
		Evidence: []schema.Evidence{{N: 1, Quote: "担任占位平台主管和网关统筹"}},
		Impact:   schema.ObsImpact{Moment: "every_turn", Action: "以平台主管视角汇报"},
	})
	if rSup.rej != obsFacetNotAllowed {
		t.Errorf("S3 supervisor self-description to persona: rej = %q, want %q", rSup.rej, obsFacetNotAllowed)
	}

	// 2. S3 ordinary judgement (not direct user correction) is rejected; direct WhoSelf correction is allowed.
	rOrdJ := c.checkObservation(wS3, uS3, nil, triage.TierOwn, schema.Observation{
		Facet: string(entry.Judgement), Type: "review", Text: "用户倾向先看架构方案",
		Situation: "新需求评审", Choice: "先看架构方案", Domain: entry.Work, Confidence: "high",
		Evidence: []schema.Evidence{{N: 2, Quote: "我看看当前架构方案"}},
		Impact:   schema.ObsImpact{Moment: "ask_user", Action: "先给架构方案"},
	})
	if rOrdJ.rej != obsFacetNotAllowed {
		t.Errorf("S3 non-correction judgement: rej = %q, want %q", rOrdJ.rej, obsFacetNotAllowed)
	}
	rRelayedJ := c.checkObservation(wS3, uS3, nil, triage.TierOwn, schema.Observation{
		Facet: string(entry.Judgement), Type: "correction", Text: "运维脚本放到工具手册里而不是全局规范",
		Situation: "沉淀运维脚本", Choice: "放到工具手册里", Rejected: "写进全局规范", Domain: entry.Work, Confidence: "high",
		Evidence: []schema.Evidence{{N: 4, Quote: "别再把运维脚本写进全局规范"}},
		Impact:   schema.ObsImpact{Moment: "dispatch", Action: "按手册分流", Object: "运维脚本"},
	})
	if rRelayedJ.rej != obsFacetNotAllowed {
		t.Errorf("S3 relayed judgement must be rejected: rej = %q, want %q", rRelayedJ.rej, obsFacetNotAllowed)
	}
	rCorrJ := c.checkObservation(wS3, uS3, nil, triage.TierOwn, schema.Observation{
		Facet: string(entry.Judgement), Type: "correction", Text: "运维脚本放到工具手册里而不是全局规范",
		Situation: "沉淀运维脚本", Choice: "放到工具手册里", Rejected: "写进全局规范", Domain: entry.Work, Confidence: "high",
		Evidence: []schema.Evidence{{N: 3, Quote: "别再把运维脚本写进全局规范，改放到工具手册里"}},
		Impact:   schema.ObsImpact{Moment: "dispatch", Action: "按手册分流", Object: "运维脚本"},
	})
	if rCorrJ.rej != "" {
		t.Errorf("S3 direct WhoSelf correction judgement should pass, got rej=%q", rCorrJ.rej)
	}

	// 3. Cron export script is blocked from writing workflow even if checkObservation is called directly.
	uCron := &Unit{
		Ref: "chats/persona-export", Entity: "chats",
		Meta: kbsrc.Meta{Kind: "assistant", Name: "用量导出"}, Day: "2026-11-20",
		Lines: []norm.Line{
			{N: 1, Speaker: norm.Me, Body: "每日导出用量数据：先采集再校验，把摘要文本发给用户"},
		},
		Who: map[int]string{1: entry.WhoSelf},
	}
	wCron := &verify.Window{Chat: uCron.Ref, Day: uCron.Day, Kind: "assistant", All: map[int]norm.Line{1: uCron.Lines[0]}, Shown: map[int]bool{1: true}}
	rCron := c.checkObservation(wCron, uCron, nil, triage.TierOwn, schema.Observation{
		Facet: string(entry.Workflow), Type: "export", Text: "每日导出用量数据先采集再校验并把摘要发给用户",
		Situation: "每日用量导出", Choice: "先采集再校验把摘要发给用户", Domain: entry.Work, Confidence: "high",
		Evidence: []schema.Evidence{{N: 1, Quote: "每日导出用量数据：先采集再校验，把摘要文本发给用户"}},
		Impact:   schema.ObsImpact{Moment: "task_start", Action: "按采集校验流程执行", Object: "用量导出"},
	})
	if rCron.rej != obsCronPrompt {
		t.Errorf("cron script to workflow: rej = %q, want %q", rCron.rej, obsCronPrompt)
	}

	// 4. Curator.Observe hard guard rejects pure S3/S4 entries for persona and workflow.
	c.St.Lock()
	defer c.St.Unlock()
	if id := c.Observe(&entry.Entry{
		Facet: entry.Persona, Text: "用户担任平台主管",
		Evidence: []entry.Evidence{{Source: entry.SrcAgent, Ref: "agents/m1/s1", Day: "2026-11-20", Quote: "担任平台主管"}},
	}, "test"); id != "" {
		t.Errorf("Observe accepted S3-only persona entry: %q", id)
	}
	if id := c.Observe(&entry.Entry{
		Facet: entry.Workflow, Text: "自动化巡检步骤",
		Evidence: []entry.Evidence{{Source: entry.SrcTools, Ref: "segments/s1", Day: "2026-11-20", Quote: "先采集再校验"}},
	}, "test"); id != "" {
		t.Errorf("Observe accepted S4-only workflow entry: %q", id)
	}
}
