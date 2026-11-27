package entry

import (
	"slices"
	"testing"
)

func TestIsEphemeralAndDecay(t *testing.T) {
	ephemeralCases := []*Entry{
		{ID: "e1", Facet: World, Type: "status", Text: "Alpha 服务当前正常"},
		{ID: "e2", Facet: People, Part: PartFact, Text: "和陈明约 11-24 17:30 call 对齐上线"},
		{ID: "e3", Facet: World, Type: "fact", Text: "PR #1234 待 review 后合并"},
		{ID: "e4", Facet: World, Type: "fact", Text: "Issue #6100 正在排查中"},
		{ID: "e5", Facet: People, Part: PartFact, Text: "给林明远发了 TODO 清单等回复"},
		{ID: "e6", Facet: World, Type: "fact", Text: "发布流程卡在鉴权超时"},
		{ID: "e7", Facet: World, Predicate: "waiting_on", Text: "等对方确认接口字段"},
	}
	for _, e := range ephemeralCases {
		if !IsEphemeral(e) {
			t.Errorf("%s (%q): IsEphemeral = false, want true", e.ID, e.Text)
		}
	}

	durableCases := []*Entry{
		{ID: "d1", Facet: People, Part: PartFact, Type: "role", Text: "陈明负责 Alpha 网关核心研发"},
		{ID: "d2", Facet: World, Predicate: "owns", Text: "林明远负责支付服务架构"},
		{ID: "d3", Facet: Interaction, Text: "先查原始证据再给结论"},
		{ID: "d4", Facet: People, Part: PartCommitment, Text: "周五前交付接口文档", Commitment: &Commitment{From: "person:p1", To: WhoSelf, What: "接口文档", Due: "2026-11-20"}},
	}
	for _, e := range durableCases {
		if IsEphemeral(e) {
			t.Errorf("%s (%q): IsEphemeral = true, want false", e.ID, e.Text)
		}
	}

	today := "2026-11-27"
	es := map[string]*Entry{
		"ep-recent":  {ID: "ep-recent", Facet: World, Type: "status", Status: Stable, Last: "2026-11-20", Text: "服务刚切流"},
		"ep-15d":     {ID: "ep-15d", Facet: People, Part: PartFact, Status: Stable, Last: "2026-11-12", Text: "约 11-12 17:30 call"},
		"ep-35d":     {ID: "ep-35d", Facet: World, Type: "fact", Status: Stable, Last: "2026-10-20", Text: "PR #1234 卡在 CI"},
		"ep-pinned":  {ID: "ep-pinned", Facet: World, Type: "status", Status: Stable, Pinned: true, Last: "2026-10-01", Text: "置顶状态"},
		"ops-fresh":  {ID: "ops-fresh", Facet: Tools, Status: Stable, Last: "2026-11-15", Text: "host-a 跑门禁命令"},
		"ops-48d":    {ID: "ops-48d", Facet: Tools, Status: Stable, Last: "2026-10-10", Text: "host-b 旧端口规则"},
		"ops-65d":    {ID: "ops-65d", Facet: Interaction, Flags: []string{FlagOpsRunbook}, Status: Stable, Last: "2026-09-20", Text: "旧版 runner 规矩"},
		"durable-ok": {ID: "durable-ok", Facet: People, Part: PartFact, Status: Stable, Last: "2026-09-01", Text: "陈明负责 Alpha 架构"},
	}

	if EphemeralExpiredRecall(es["ep-recent"], today) {
		t.Error("7d ephemeral should remain in active recall")
	}
	if !EphemeralExpiredRecall(es["ep-15d"], today) || ShouldDormantEphemeral(es["ep-15d"], today) {
		t.Error("15d ephemeral should exit active recall (>=14d) but not yet dormant (<30d)")
	}
	if !ShouldDormantEphemeral(es["ep-35d"], today) {
		t.Error("35d ephemeral should be marked dormant (>=30d)")
	}
	if OpsRunbookWeight(es["ops-fresh"], today) != 1.0 {
		t.Errorf("12d ops weight = %v, want 1.0", OpsRunbookWeight(es["ops-fresh"], today))
	}
	if OpsRunbookWeight(es["ops-48d"], today) != 0.5 {
		t.Errorf("48d ops weight = %v, want 0.5", OpsRunbookWeight(es["ops-48d"], today))
	}
	if OpsRunbookWeight(es["ops-65d"], today) != 0.25 || !ShouldDormantOpsRunbook(es["ops-65d"], today) {
		t.Errorf("65d ops should demote to 0.25 and dormant")
	}

	rep := ApplyLifecycleDecay(es, today)
	if rep.EphemeralDormant != 1 || rep.EphemeralRecallExpired != 1 || rep.OpsDemoted != 1 || rep.OpsDormant != 1 {
		t.Fatalf("unexpected decay report: %+v", rep)
	}
	if es["ep-recent"].Status != Candidate || !slices.Contains(es["ep-recent"].Flags, FlagEphemeral) {
		t.Errorf("ep-recent: status=%s flags=%v", es["ep-recent"].Status, es["ep-recent"].Flags)
	}
	if es["ep-15d"].Status != Candidate {
		t.Errorf("ep-15d: status=%s, want candidate", es["ep-15d"].Status)
	}
	if es["ep-35d"].Status != Dormant {
		t.Errorf("ep-35d: status=%s, want dormant", es["ep-35d"].Status)
	}
	if es["ep-pinned"].Status != Stable {
		t.Errorf("pinned entry should stay stable, got %s", es["ep-pinned"].Status)
	}
	if es["ops-65d"].Status != Dormant {
		t.Errorf("ops-65d should become dormant, got %s", es["ops-65d"].Status)
	}
	if es["durable-ok"].Status != Stable {
		t.Errorf("durable people fact should stay stable, got %s", es["durable-ok"].Status)
	}
}

func TestGenericPeopleNoise(t *testing.T) {
	if !IsGenericPersonName("HR") || !IsGenericPersonName("person:对方") || IsGenericPersonName("陈明") || IsGenericPersonName("") {
		t.Error("IsGenericPersonName mismatch")
	}
	hrChatter := &Entry{ID: "p-hr", Facet: People, Part: PartFact, Text: "和 HR 沟通了招聘进度和简历筛选"}
	if !IsGenericPeopleNoise(hrChatter, "HR") || !IsGenericPeopleNoise(hrChatter, "") {
		t.Error("generic HR chatter should be filtered")
	}
	namedRole := &Entry{ID: "p-role", Facet: People, Part: PartFact, Type: "role", Person: "person:p1", Text: "担任研发团队 HRBP"}
	if IsGenericPeopleNoise(namedRole, "林明远") {
		t.Error("durable role on a concrete named person must not be dropped")
	}
}

func TestIsOpsRunbookEntryIntegratesBlock1Classifier(t *testing.T) {
	byClass := &Entry{ID: "c1", Facet: Interaction, MemoryClass: ClassOpsRunbook, Text: "generic text", Last: "2026-09-01"}
	if !IsOpsRunbookEntry(byClass) || !ShouldDormantOpsRunbook(byClass, "2026-11-27") {
		t.Fatalf("expected MemoryClass=ClassOpsRunbook to integrate with OpsRunbook decay")
	}
	byText := &Entry{ID: "c2", Facet: Interaction, Text: "在 build-wsl 上执行 go test 并检查 docker 容器", Last: "2026-10-05"}
	if !IsOpsRunbookEntry(byText) || OpsRunbookWeight(byText, "2026-11-27") != 0.5 {
		t.Fatalf("expected ClassifyOpsRunbook match to integrate with OpsRunbookWeight (0.5 at 53d)")
	}
}
