package entry

import (
	"slices"
	"testing"
)

// Categories: the legacy single label reads as a category; anyone adds, only the user removes, and a category the
// user removed is not added back but by the user; a space tags its default; the resident exits take a category only
// with K22 on, and a domain space's content only as interaction or persona. Synthetic entries only.
func TestCategories(t *testing.T) {
	e := &Entry{Facet: Persona, Sensitive: SensFamily}
	if !slices.Equal(e.Cats(), []string{CatRelationships}) || !e.IsSensitive() {
		t.Fatalf("legacy label: %v", e.Cats())
	}
	if !e.AddCategory(CatLegalID, ByModel) || e.AddCategory("legal_id", ByModel) || e.AddCategory("gossip", ByModel) {
		t.Fatal("add")
	}
	if e.RemoveCategory(CatRelationships, ByModel) || !e.RemoveCategory(CatRelationships, ByUser) || e.Sensitive != "" {
		t.Fatalf("remove: %+v", e)
	}
	if e.AddCategory(SensFamily, ByModel) || !e.AddCategory(SensFamily, ByUser) {
		t.Fatal("a category the user removed came back by the model, or not by the user")
	}
	if !slices.Equal(e.Cats(), []string{CatLegalID, CatRelationships}) {
		t.Fatalf("cats: %v", e.Cats())
	}
	sw := map[string]SwitchSetting{K22Resident + "." + CatLegalID: {Value: "on", By: "user"}}
	if e.Resident(sw) {
		t.Fatal("relationships still off")
	}
	sw[K22Resident+"."+CatRelationships] = SwitchSetting{Value: "on", By: "user"}
	if !e.Resident(sw) {
		t.Fatal("both on")
	}

	// Payload-First (0.4.0 §1.6): restricted-space facts (World/People/Persona) still inherit space defaults and
	// confinement, while de-shelled work-methodology rules (Interaction/Tools/Workflow/Judgement) do not.
	wFact := &Entry{Facet: World, Text: "占位体检记录存放在健康空间"}
	wFact.TagSpace("health")
	if wFact.OriginSpace != "health" || !slices.Equal(wFact.Cats(), []string{CatHealth}) || wFact.Categories[0].By != BySpaceDefault {
		t.Fatalf("world space: %+v", wFact)
	}
	if wFact.Resident(map[string]SwitchSetting{K22Resident + "." + CatHealth: {Value: "on", By: "user"}}) {
		t.Fatal("a restricted space's world fact reached the resident exits")
	}

	wGeneric := &Entry{Facet: Workflow, Text: "需要例行方案，不要一次性导出"}
	wGeneric.TagSpace("health")
	if wGeneric.OriginSpace != "" || wGeneric.IsSensitive() || !wGeneric.Resident(nil) {
		t.Fatalf("de-shelled workflow rule should not inherit health space: %+v", wGeneric)
	}

	// A work-methodology entry whose own payload contains genuine medical disclosure stays tagged and blocked.
	wMedical := &Entry{Facet: Workflow, Text: "占位健康空间里的复诊流程"}
	wMedical.TagSpace("health")
	if wMedical.OriginSpace != "health" || !slices.Equal(wMedical.Cats(), []string{CatHealth}) || wMedical.Resident(nil) {
		t.Fatalf("medical workflow payload must stay tagged and blocked: %+v", wMedical)
	}

	// Historical OriginSpace on a de-shelled general interaction rule does not block Resident.
	p := &Entry{Facet: Interaction, Text: "汇报只给覆盖天数和核心结论", OriginSpace: "health"}
	if !p.Resident(nil) || p.IsSensitive() {
		t.Fatalf("de-shelled general rule with OriginSpace should reach resident: %+v", p)
	}
	self := &Entry{Facet: Persona, Origin: SelfReport}
	self.AddCategory(CatHealth, ByModel)
	if !self.Resident(nil) {
		t.Fatal("what the user wrote about themself goes out")
	}
	k := &Entry{Facet: Persona}
	k.MergeCategories(wFact)
	if !slices.Equal(k.Cats(), []string{CatHealth}) || k.OriginSpace != "health" {
		t.Fatalf("merge non-work: %+v", k)
	}

	// MergeCategories must not unconditionally contaminate a work-methodology entry from a sensitive sighting.
	iRule := &Entry{Facet: Interaction, Text: "要的是例行方案，不要一次性导出"}
	iRule.MergeCategories(wFact)
	if iRule.IsSensitive() || iRule.OriginSpace != "" {
		t.Fatalf("work-method entry contaminated by MergeCategories: %+v", iRule)
	}
	// Explicit user-tagged categories (ByUser) are still preserved.
	userTagged := &Entry{Facet: Interaction, Categories: []Category{{Name: CatEmotion, By: ByUser}}}
	iRule.MergeCategories(userTagged)
	if !slices.Equal(iRule.Cats(), []string{CatEmotion}) {
		t.Fatalf("explicit ByUser category not preserved: %+v", iRule)
	}
}

func TestPayloadCategories(t *testing.T) {
	// Sanitized false-positive regression cases (0.4.0 §1.6): work methodology & AI interaction tone must return empty.
	nonSensitive := []struct {
		name  string
		facet Facet
		text  string
	}{
		{"repeated_questions_dissatisfied", Interaction, "反复发问会让用户不满，已明确的前提直接推进"},
		{"convoluted_explanation_plain_words", Interaction, "嫌 AI 解释太绕，要求直接说人话给结论"},
		{"routine_plan_not_one_off_export", Interaction, "要的是例行方案，不要一次性导出"},
		{"health_chat_reporting_style", Workflow, "数据汇总汇报只给覆盖天数和核心结论"},
		{"model_token_cost_awareness", Judgement, "不无脑上最强模型，按复杂度控制 Token 调用成本和项目预算"},
		{"api_quota_balance_in_tools", Tools, "先检查 Claude API 可用额度与账号余额再派发批量任务"},
		{"open_source_license_and_api_token", Tools, "遵守 MIT 开源协议，API Token 放环境变量不提交仓库"},
		{"public_team_org_division", World, "成员甲负责平台架构与网关模块，成员乙负责移动端发布协作"},
		{"object_oriented_programming", Workflow, "修改核心对象结构时保持向后兼容"},
	}
	for _, tc := range nonSensitive {
		if got := PayloadCategories(tc.facet, tc.text); len(got) != 0 {
			t.Errorf("%s (%s): PayloadCategories = %v, want empty", tc.name, tc.facet, got)
		}
	}

	// Genuine personal privacy payloads must hit 100% accurately across all 5 categories.
	sensitive := []struct {
		name  string
		facet Facet
		text  string
		want  string
	}{
		{"health_exam_metric", Persona, "近期体检显示血糖偏高且静息心率异常，正在服药观察", CatHealth},
		{"health_diagnosis_en", Persona, "Doctor diagnosed mild asthma and adjusted medication dosage", CatHealth},
		{"emotion_insomnia_anxiety", Persona, "近期因个人低谷持续失眠、重度焦虑并开始心理咨询", CatEmotion},
		{"emotion_burnout_en", Persona, "Experiencing severe anxiety and burnout, seeing a therapist weekly", CatEmotion},
		{"finance_salary_mortgage", Persona, "当前年薪调整后准备提前还房贷并调整家庭存款配置", CatFinance},
		{"finance_bank_en", Persona, "Personal savings and mortgage down payment plan for next year", CatFinance},
		{"legal_passport_visa", Persona, "护照与签证下个月到期，同时在处理一起私人合同仲裁", CatLegalID},
		{"legal_ssn_en", Persona, "Need to renew passport and prepare documents for the lawsuit", CatLegalID},
		{"relationships_family", Persona, "和伴侣在子女教育与父母同住问题上有分歧", CatRelationships},
		{"relationships_en", Persona, "Discussing childcare schedule with wife and parents", CatRelationships},
		{"legal_hukouben", Persona, "需要准备户口本和身份证办理落户手续", CatLegalID},
		{"relationships_ex_girlfriend", Persona, "提到前女友和前任伴侣的旧事", CatRelationships},
		{"legal_us_ead_card", Persona, "美国工卡审批通过，实体工卡下周寄出", CatLegalID},
		{"emotion_anxiety_disorder", Persona, "近期确诊焦虑症并持续接受心理咨询", CatEmotion},
	}
	for _, tc := range sensitive {
		got := PayloadCategories(tc.facet, tc.text)
		if !slices.Contains(got, tc.want) {
			t.Errorf("%s: PayloadCategories = %v, want to contain %q", tc.name, got, tc.want)
		}
	}

	// Cross-word Chinese false positives & Impact.Action exclusion regressions (v0.4.0 rehearsal round 1).
	rehearsalFP := []struct {
		name  string
		entry *Entry
	}{
		{"dictation_summary_no_hukou", &Entry{Facet: Workflow, Text: "用户口述总结后再整理为结构化要点"}},
		{"current_task_no_ex_partner", &Entry{Facet: World, Text: "同事甲当前任务进展同步到周报"}},
		{"manual_checkpoint_no_work_card", &Entry{Facet: Judgement, Text: "agent 人工卡点是伪需求，能自动化就不要人工审批"}},
		{"meeting_impact_action_no_dating", &Entry{
			Facet:  People,
			Text:   "同事乙约 15 分钟会议对齐接口细节",
			Impact: Impact{Moment: "reply_person", Action: "提醒用户记住这项约会承诺", Object: "约会安排"},
		}},
		{"pace_impact_action_no_anxiety", &Entry{
			Facet:  People,
			Text:   "同事丙自认推进节奏偏慢，希望拆细里程碑",
			Impact: Impact{Moment: "reply_person", Action: "把他的节奏焦虑当作背景参考", Object: "焦虑情绪"},
		}},
	}
	for _, tc := range rehearsalFP {
		if got := PayloadCategories(tc.entry.Facet, tc.entry.PayloadTexts()...); len(got) != 0 {
			t.Errorf("%s: PayloadCategories(PayloadTexts) = %v, want empty", tc.name, got)
		}
	}

	// Insomnia maps solely to health (not emotion) so sensitive-v04.md and profile-v04.md stay consistent.
	if got := PayloadCategories(Persona, "用户这几天失眠，晚上睡不着"); !slices.Equal(got, []string{CatHealth}) {
		t.Errorf("insomnia PayloadCategories = %v, want [health]", got)
	}

	// Rehearsal round 2: 6 cross-word / ambiguous false positives + PayloadTexts work-method boundary.
	rehearsalRound2FP := []struct {
		name  string
		entry *Entry
	}{
		{"api_diagnosis_endpoint", &Entry{Facet: Workflow, Text: "调用 hub API 或诊断端点排查问题"}},
		{"auto_diagnose_issue", &Entry{Facet: Interaction, Text: "自动诊断问题并明确诊断边界"}},
		{"service_side_effect", &Entry{Facet: Judgement, Text: "避免对线上服务产生脏数据或副作用"}},
		{"decide_investment_priority", &Entry{Facet: Judgement, Text: "在多项工作之间决定投入优先级时先看杠杆"}},
		{"add_account_auth", &Entry{Facet: Workflow, Text: "先设置 mentor 或加入账号授权再分配权限"}},
		{"interview_complained_about", &Entry{Facet: World, Text: "候选人因拒绝面试被告状，后续被告知流程取消"}},
		{"ai_passport_product", &Entry{Facet: World, Text: "排查某 AI Passport 登录回调异常"}},
		{"domestic_visa_payment", &Entry{Facet: Workflow, Text: "测试国内 visa 付款渠道与个人资料目录 bank、visa 配置"}},
		{"work_rejected_family_object_health", &Entry{
			Facet:    Interaction,
			Text:     "回答问题时直接切入核心结论",
			Rejected: "反面举例：只讲母亲年龄而不给结论",
			Object:   "心率回补待办",
		}},
	}
	for _, tc := range rehearsalRound2FP {
		if got := PayloadCategories(tc.entry.Facet, tc.entry.PayloadTexts()...); len(got) != 0 {
			t.Errorf("%s: PayloadCategories(PayloadTexts) = %v, want empty", tc.name, got)
		}
	}

	// Tightened true positives must still hit accurately.
	rehearsalRound2TP := []struct {
		name  string
		facet Facet
		text  string
		want  string
	}{
		{"clinical_diagnosis_health", Persona, "医院确诊为轻度脂肪肝，医生建议观察药物副作用", CatHealth},
		{"fund_dca_finance", Persona, "每月定投指数基金并等工资入账后自动分账", CatFinance},
		{"lawsuit_defendant_legal", Persona, "因私人纠纷成为被告，正在准备应诉材料", CatLegalID},
		{"us_visa_renewal_legal", Persona, "Preparing H1B visa renewal and US passport photo", CatLegalID},
		{"renew_the_passport_legal", Workflow, "renew the passport before the trip", CatLegalID},
	}
	for _, tc := range rehearsalRound2TP {
		if got := PayloadCategories(tc.facet, tc.text); !slices.Contains(got, tc.want) {
			t.Errorf("%s: PayloadCategories = %v, want to contain %q", tc.name, got, tc.want)
		}
	}

	// Ops runbook entries never inherit non-user sensitive tags via TagSpace or MergeCategories.
	opsEntry := &Entry{Facet: Tools, Text: "在专用节点跑心率回补脚本 run.sh"}
	opsEntry.TagSpace("health")
	if opsEntry.IsSensitive() || opsEntry.OriginSpace != "" {
		t.Fatalf("ops runbook must not inherit space sensitivity: %+v", opsEntry)
	}
	sensSrc := &Entry{
		Facet:       Persona,
		Text:        "体检静息心率偏高",
		OriginSpace: "health",
		Categories:  []Category{{Name: CatHealth, By: ByModel}},
	}
	opsEntry.MergeCategories(sensSrc)
	if opsEntry.IsSensitive() || opsEntry.OriginSpace != "" {
		t.Fatalf("ops runbook must not inherit non-user sensitivity via MergeCategories: %+v", opsEntry)
	}
}
