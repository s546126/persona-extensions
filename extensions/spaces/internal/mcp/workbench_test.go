package mcp

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/pluginkit/spaces/store"
)

func TestSchemaSummaryRelationsAndTemplateTypes(t *testing.T) {
	s := start(t, store.Open(""))
	defer s.finish()

	// Empty schema overview lists all 10 templates with their type names and sample flags.
	ov := asMap(t, s.done("schema", `{}`))
	tpls := asList(t, ov["templates"])
	foundFinance, foundMedia := false, false
	for _, item := range tpls {
		m := asMap(t, item)
		switch m["key"] {
		case "finance":
			foundFinance = true
			if got := asList(t, m["types"]); len(got) != 2 || got[0] != "预算" || got[1] != "账单" || m["has_samples"] != true {
				t.Errorf("finance template info = %v", m)
			}
		case "media":
			foundMedia = true
			if got := asList(t, m["types"]); len(got) != 3 || got[0] != "作品" || m["has_samples"] != true {
				t.Errorf("media template info = %v", m)
			}
		case "blank":
			if m["has_samples"] == true {
				t.Errorf("blank has_samples should be omitted/false: %v", m)
			}
		}
	}
	if !foundFinance || !foundMedia {
		t.Fatalf("missing finance (%v) or media (%v) in schema templates", foundFinance, foundMedia)
	}

	// Space schema includes summary counts and ontology relation edges.
	sp := asStr(t, get(t, s.done("space_write", `{"template":"project","samples":true}`), "id"))
	sch := asMap(t, s.done("schema", fmt.Sprintf(`{"space":%q}`, sp)))
	sum := asMap(t, sch["summary"])
	if sum["types"] != float64(4) || sum["total"] != float64(13) {
		t.Errorf("schema summary = %v", sum)
	}
	rels := asList(t, sch["relations"])
	if len(rels) != 3 {
		t.Errorf("schema relations = %v, want 3 edges (task.goal, task.blocked_by, note.about)", rels)
	}
}

func TestQueryByNamedView(t *testing.T) {
	s := start(t, store.Open(""))
	defer s.finish()

	sp := asStr(t, get(t, s.done("space_write", `{"template":"subscription","samples":true}`), "id"))

	// Query using named view "月付" inherits its where (cycle=月付, phase!=done), sort and agg.
	res := asMap(t, s.done("query", fmt.Sprintf(`{"space":%q,"type":"sub","view":"月付","brief":true}`, sp)))
	if res["total"] != float64(3) {
		t.Fatalf("monthly view total = %v, want 3", res["total"])
	}
	aggs := asList(t, res["aggs"])
	if len(aggs) != 2 || asMap(t, aggs[1])["value"] != float64(254) {
		t.Fatalf("monthly view aggs = %v, want sum=254", aggs)
	}

	// Extra where condition stacks on top of the view's lens.
	narrow := asMap(t, s.done("query", fmt.Sprintf(
		`{"space":%q,"type":"sub","view":"月付","where":[{"field":"category","op":"eq","value":"健康"}]}`, sp)))
	if narrow["total"] != float64(1) {
		t.Fatalf("stacked where total = %v, want 1", narrow["total"])
	}

	// Refusals: missing type or unknown view name lists available views.
	if msg := s.failed("query", fmt.Sprintf(`{"space":%q,"view":"月付"}`, sp)); !strings.Contains(msg, "指定类型") {
		t.Errorf("view without type error = %q", msg)
	}
	if msg := s.failed("query", fmt.Sprintf(`{"space":%q,"type":"sub","view":"不存在"}`, sp)); !strings.Contains(msg, "快续费了") {
		t.Errorf("unknown view error = %q, want available view list", msg)
	}
}

func TestEntityDeriveWithValuesAndAtomicRollback(t *testing.T) {
	s := start(t, store.Open(""))
	defer s.finish()

	sp := asStr(t, get(t, s.done("space_write", `{"template":"project","samples":true}`), "id"))
	ideas := asList(t, get(t, s.done("query", fmt.Sprintf(`{"space":%q,"type":"idea"}`, sp)), "entities"))
	var swimID string
	for _, it := range ideas {
		m := asMap(t, it)
		if m["title"] == "学会自由泳" {
			swimID = asStr(t, m["id"])
		}
	}

	// Derive a goal from the idea AND set due date + advance one step to 进行中 in a single atomic call.
	derived := asMap(t, s.done("entity_derive", fmt.Sprintf(
		`{"id":%q,"type":"goal","title":"夏天前游完一千米","status":"进行中","values":{"due":"2027-06-01"}}`, swimID)))
	ent := asMap(t, derived["entity"])
	if ent["status"] != "进行中" || ent["title"] != "夏天前游完一千米" {
		t.Fatalf("derived entity = %v", ent)
	}
	vals := asMap(t, ent["values"])
	if vals["area"] != "健康" || vals["due"] != "2027-06-01" {
		t.Fatalf("derived values = %v", vals)
	}
	if inh := asList(t, derived["inherited"]); !slices.Contains(inh, any("area")) {
		t.Fatalf("inherited = %v", inh)
	}

	// Derive with an invalid value rolls back the entire derive cleanly (no orphan entity created).
	before := get(t, s.done("query", fmt.Sprintf(`{"space":%q,"type":"goal"}`, sp)), "total")
	if msg := s.failed("entity_derive", fmt.Sprintf(
		`{"id":%q,"type":"goal","title":"无效目标","values":{"due":"not-a-date"}}`, swimID)); !strings.Contains(msg, "日期") {
		t.Fatalf("invalid derive error = %q", msg)
	}
	after := get(t, s.done("query", fmt.Sprintf(`{"space":%q,"type":"goal"}`, sp)), "total")
	if before != after {
		t.Fatalf("failed derive leaked entity: goals before=%v after=%v", before, after)
	}
}
