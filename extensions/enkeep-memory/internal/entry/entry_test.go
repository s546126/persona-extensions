package entry

import "testing"

func TestIsTrueUserOwned(t *testing.T) {
	cases := []struct {
		name string
		e    *Entry
		want bool
	}{
		{"nil", nil, false},
		{"self_report", &Entry{Origin: SelfReport}, true},
		{"onboarding", &Entry{Origin: FromOnboarding}, true},
		{"pinned", &Entry{Origin: Behavior, Pinned: true}, true},
		{"user_edit", &Entry{Origin: Behavior, User: &UserEdit{Text: "user revised rule"}}, true},
		{"blank_user_edit", &Entry{Origin: Behavior, User: &UserEdit{Text: "   "}}, false},
		{"s6_assistant_note", &Entry{Origin: OriginAssistantNote, Status: Stable}, false},
		{"s6_legacy_handwritten", &Entry{Origin: Handwritten, Status: Stable}, false},
		{"behavior", &Entry{Origin: Behavior, Status: Stable}, false},
		{"seed", &Entry{Origin: Seed, Status: Stable}, false},
		{"s6_with_explicit_pin", &Entry{Origin: OriginAssistantNote, Pinned: true}, true},
		{"s6_with_user_edit", &Entry{Origin: OriginAssistantNote, User: &UserEdit{Text: "edited by user"}}, true},
	}
	for _, tc := range cases {
		if got := IsTrueUserOwned(tc.e); got != tc.want {
			t.Errorf("%s: IsTrueUserOwned = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClassifyOpsRunbook(t *testing.T) {
	opsCases := []struct {
		name string
		e    *Entry
	}{
		{"tools_facet", &Entry{Facet: Tools, Text: "use jump host"}},
		{"explicit_class", &Entry{Facet: Interaction, MemoryClass: ClassOpsRunbook, Text: "any text"}},
		{"object_scope", &Entry{Facet: Interaction, Scopes: []Scope{{Kind: ScopeObject, Name: "host-a"}}, Text: "prefer alias"}},
		{"wsl_gate_host", &Entry{Facet: Interaction, Text: "在 host-wsl 上跑 fast 门禁，不要在本机跑单测"}},
		{"herdr_codex_runtime", &Entry{Facet: Interaction, Text: "herdr 启动失败时改用 Codex 执行"}},
		{"platform_tool_names", &Entry{Facet: Workflow, Text: "派活使用 agent_start，不要使用 task_plan"}},
		{"vps_deployment", &Entry{Facet: Workflow, Text: "把 email-relay 服务部署到 VPS 上"}},
		{"cloudflare_key", &Entry{Facet: Interaction, Text: "在 Cloudflare 后台不要点击轮换密钥"}},
		{"cli_git_pr", &Entry{Facet: Workflow, Text: "推分支后用 gh pr merge --squash 合入 origin/main"}},
		{"draft_pr_merge_script", &Entry{Facet: Workflow, Text: "提 PR 先开草稿 PR，门禁通过后转正式并用合并脚本合并"}},
		{"draft_pr_squash_en", &Entry{Facet: Interaction, Text: "Always open a draft PR first and finish with squash merge"}},
		{"quota_routing", &Entry{Facet: Interaction, Text: "API 限流或额度超过 90% 时自动分流"}},
	}
	for _, tc := range opsCases {
		if !ClassifyOpsRunbook(tc.e) {
			t.Errorf("ops case %s: ClassifyOpsRunbook = false, want true (%+v)", tc.name, tc.e)
		}
	}

	universalCases := []struct {
		name string
		e    *Entry
	}{
		{"nil", nil},
		{"verify_first", &Entry{Facet: Interaction, Text: "下结论前先查原始证据，不要凭空猜测"}},
		{"context_split", &Entry{Facet: Judgement, Text: "执行语境直接推进到底，设计语境先出方案对齐"}},
		{"concise_summary", &Entry{Facet: Interaction, Text: "长材料对话先给一句核心结论，不要贴大段原文"}},
		{"plain_language", &Entry{Facet: Interaction, Text: "用大白话解释专业概念并给明确建议，不要只抛选择题"}},
		{"model_tier_principle", &Entry{Facet: Judgement, Text: "按任务复杂度分级选择模型，不无脑上最强模型"}},
		{"world_fact_not_ops", &Entry{Facet: World, Text: "项目在 host-wsl 上有构建节点"}},
		{"people_not_ops", &Entry{Facet: People, Text: "同事负责维护 docker 镜像"}},
		{"routine_export_methodology", &Entry{Facet: Interaction, Text: "需要的是例行方案，而不是一次性导出"}},
	}
	for _, tc := range universalCases {
		if ClassifyOpsRunbook(tc.e) {
			t.Errorf("universal case %s: ClassifyOpsRunbook = true, want false (%+v)", tc.name, tc.e)
		}
	}
}
