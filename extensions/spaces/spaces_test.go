package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestMain(m *testing.M) {
	// Never write the locator into the real config directory.
	dir, err := os.MkdirTemp("", "spaces-locator-")
	if err != nil {
		panic(err)
	}
	locatorFile = func() string { return filepath.Join(dir, "data-dir") }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newApp(t.TempDir()))))
}

func TestManifest(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Contributes struct {
			MCPServers []struct {
				Allow    []string `json:"allow"`
				Requires []string `json:"requires"`
			} `json:"mcp_servers"`
		} `json:"contributes"`
	}
	_ = json.Unmarshal(manifest, &raw)
	if len(raw.Contributes.MCPServers) != 1 {
		t.Fatal("one mcp server")
	}
	allow := raw.Contributes.MCPServers[0].Allow
	if !slices.Equal(allow, readOnlyTools) {
		t.Fatalf("allow %v, want the read-only tools %v", allow, readOnlyTools)
	}
	for _, tool := range mcpTools() {
		name := tool["name"].(string)
		ro := slices.Contains(readOnlyTools, name)
		if slices.Contains(allow, name) != ro {
			t.Fatalf("%s: allowed %v, read-only %v", name, slices.Contains(allow, name), ro)
		}
	}
	if !strings.Contains(usage, "mcp") {
		t.Fatal("--help prints mcp (requires)")
	}
	if _, err := os.Stat(filepath.Join("skills", "spaces", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

var t0 = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)

func mustSpace(t *testing.T, st *State, id string) *Space {
	t.Helper()
	sp, err := st.space(id)
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

func TestMultiSpaceIsolationAndTemplates(t *testing.T) {
	st := seedState(t0)
	var names []string
	for _, sp := range st.Spaces {
		names = append(names, sp.Name)
	}
	if !slices.Equal(names, []string{"薅羊毛空间", "LifeOS 家庭空间", "云电脑"}) || st.Active != "deals" {
		t.Fatalf("seed: %v active %s", names, st.Active)
	}
	for id, types := range map[string][]string{
		"deals":  {"deal", "merchant", "coupon"},
		"lifeos": {"inventory_item", "meal_plan", "chore", "location"},
		"cloud":  {"cloud_host", "runbook"},
	} {
		sp := mustSpace(t, st, id)
		for _, typ := range append(types, TypeFeed, TypeIdea, TypeGoal, TypeResource) {
			if _, ok := sp.objectType(typ); !ok {
				t.Fatalf("%s has no %s", id, typ)
			}
			if typ != TypeFeed && typ != TypeResource && typ != TypeIdea && typ != TypeGoal && sp.count(typ, false) == 0 {
				t.Fatalf("%s: no %s examples", id, typ)
			}
		}
		if sp.count(TypeFeed, false) == 0 {
			t.Fatalf("%s: the seed is in the feed", id)
		}
	}
	// No example leaks a real address.
	raw, _ := json.Marshal(st)
	if !strings.Contains(string(raw), `\u003chost-address\u003e`) {
		t.Fatal("the cloud examples use the <host-address> placeholder")
	}

	blank, err := st.createSpace(SpaceInput{Template: "blank", Name: "读书"}, "你", t0)
	if err != nil || st.Active != blank.ID || len(blank.customTypes()) != 0 || len(blank.Types) != 4 {
		t.Fatalf("blank: %+v %v", blank, err)
	}
	other, err := st.createSpace(SpaceInput{Template: "lifeos"}, "你", t0)
	if err != nil || other.ID != "lifeos-2" {
		t.Fatalf("a second lifeos: %+v %v", other, err)
	}
	if _, err := st.createSpace(SpaceInput{Template: "nope"}, "你", t0); err == nil {
		t.Fatal("an unknown template is refused")
	}
	// The same type key in two spaces, with different fields: neither sees the other's.
	if _, _, err := st.upsertType(blank, ObjectType{Key: "book", Label: "书", Fields: []FieldDef{{Key: "pages", Kind: KindNumber}}}, "你", t0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.upsertType(other, ObjectType{Key: "book", Label: "绘本", Fields: []FieldDef{{Key: "pages", Kind: KindText}}}, "你", t0); err != nil {
		t.Fatal(err)
	}
	bt, _ := blank.objectType("book")
	ot, _ := other.objectType("book")
	if bt.Fields[0].Kind != KindNumber || ot.Fields[0].Kind != KindText {
		t.Fatalf("schemas collided: %+v %+v", bt, ot)
	}
	if _, ok := mustSpace(t, st, "lifeos").objectType("book"); ok {
		t.Fatal("a type of one space shows in another")
	}
	e1, _, err := st.upsertEntity(EntityInput{SpaceID: blank.ID, Type: "book", Title: "A", Properties: map[string]any{"pages": "120"}}, "你", t0)
	if err != nil || e1.Properties["pages"] != 120.0 {
		t.Fatalf("number field: %+v %v", e1, err)
	}
	e2, _, err := st.upsertEntity(EntityInput{SpaceID: other.ID, Type: "book", Title: "B", Properties: map[string]any{"pages": "007"}}, "你", t0)
	if err != nil || e2.Properties["pages"] != "007" {
		t.Fatalf("text field keeps 007: %+v %v", e2, err)
	}
	if list, _ := blank.query(Query{Type: "book"}); len(list) != 1 || list[0].ID != e1.ID {
		t.Fatalf("isolation: %+v", list)
	}
	// A relation never crosses spaces.
	if _, _, err := st.upsertEntity(EntityInput{ID: e1.ID, Relations: []RelationInput{{Type: "relates_to", TargetID: e2.ID}}}, "你", t0); err == nil {
		t.Fatal("a relation to another space is refused")
	}

	// Switching and archiving.
	if _, err := st.switchSpace("cloud"); err != nil || st.activeSpace().ID != "cloud" {
		t.Fatal(err)
	}
	if _, err := st.archiveSpace("cloud", true, "你", t0); err != nil || st.Active == "cloud" {
		t.Fatalf("archive the active one: %v active %s", err, st.Active)
	}
	if _, err := st.switchSpace("cloud"); err == nil {
		t.Fatal("an archived space is not switched to")
	}
	if _, err := st.archiveSpace("cloud", false, "你", t0); err != nil {
		t.Fatal(err)
	}
	for _, sp := range st.liveSpaces() {
		if sp.ID != "deals" {
			if _, err := st.archiveSpace(sp.ID, true, "你", t0); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := st.archiveSpace("deals", true, "你", t0); err == nil {
		t.Fatal("the last open space stays")
	}
}

func TestStoreReloadsAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	a := openStore(dir, t.Logf)
	b := openStore(dir, t.Logf) // the MCP process
	if err := b.update(func(st *State, now time.Time) error {
		_, _, err := st.upsertEntity(EntityInput{Type: TypeIdea, Title: "从助手来的点子"}, mcpActor, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	a.read(func(st *State) {
		list, _ := st.activeSpace().query(Query{Type: TypeIdea, Keyword: "从助手来的"})
		found = len(list) == 1
	})
	if !found {
		t.Fatal("the hub process does not see the MCP process's write")
	}
	if a.generation() != b.generation() {
		t.Fatal("generations differ")
	}
	// A failed update changes nothing.
	before := a.generation()
	if err := a.update(func(st *State, now time.Time) error {
		st.Spaces[0].Name = "改坏了"
		return invalidf("no")
	}); err == nil {
		t.Fatal("want the error")
	}
	a.read(func(st *State) {
		if st.Spaces[0].Name == "改坏了" || st.Generation != before {
			t.Fatal("a failed update was kept")
		}
	})
	// A corrupt file is moved aside.
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := openStore(dir, t.Logf)
	c.read(func(st *State) {
		if len(st.Spaces) == 0 {
			t.Fatal("no state after a corrupt file")
		}
	})
	if _, err := os.Stat(filepath.Join(dir, stateFile+".corrupt")); err != nil {
		t.Fatal(err)
	}
}

func TestOntologySchemaAndEntityCRUD(t *testing.T) {
	st := seedState(t0)
	sp := mustSpace(t, st, "deals")
	pet := ObjectType{Key: "pet", Label: "宠物", Icon: "star", PrimaryField: "breed", Statuses: []string{"在家", "寄养"},
		Fields: []FieldDef{
			{Key: "breed", Label: "品种", Kind: KindText, Required: true},
			{Key: "age", Label: "年龄", Kind: KindNumber, Unit: "岁"},
			{Key: "neutered", Label: "已绝育", Kind: KindBoolean},
			{Key: "birthday", Label: "生日", Kind: KindDate},
			{Key: "size", Label: "体型", Kind: KindEnum, Options: []string{"小", "中", "大"}},
			{Key: "profile", Label: "档案", Kind: KindURL},
			{Key: "shop", Label: "常去店", Kind: KindRelation, TargetType: "merchant", RelationType: "uses"},
		}}
	if _, created, err := st.upsertType(sp, pet, "你", t0); err != nil || !created {
		t.Fatalf("register: %v", err)
	}
	for _, bad := range []ObjectType{
		{Key: "Bad"}, {Key: "all"}, {Key: "x", Icon: "unicorn"},
		{Key: "x", Fields: []FieldDef{{Key: "a", Kind: "money"}}},
		{Key: "x", Fields: []FieldDef{{Key: "a", Kind: KindEnum}}},
		{Key: "x", Fields: []FieldDef{{Key: "title"}}},
		{Key: "x", PrimaryField: "nope", Fields: []FieldDef{{Key: "a"}}},
		{Key: "x", Fields: []FieldDef{{Key: "a", Kind: KindRelation, TargetType: "ghost"}}},
	} {
		if _, _, err := st.upsertType(sp, bad, "你", t0); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	merchant, _ := sp.query(Query{Type: "merchant", Limit: 1})
	deal, _ := sp.query(Query{Type: "deal", Limit: 1})
	good := map[string]any{"breed": "007", "age": "3", "neutered": "true", "birthday": "2023-05-01T00:00:00Z", "size": "小",
		"profile": "https://example.com/pet", "shop": merchant[0].ID}
	e, created, err := st.upsertEntity(EntityInput{Type: "pet", Title: "团子", Tags: []string{"猫, 橘猫"}, Properties: good}, "你", t0)
	if err != nil || !created {
		t.Fatal(err)
	}
	if e.Properties["breed"] != "007" || e.Properties["age"] != 3.0 || e.Properties["neutered"] != true ||
		e.Properties["birthday"] != "2023-05-01" || e.Status != "在家" || !slices.Equal(e.Tags, []string{"猫", "橘猫"}) {
		t.Fatalf("normalized: %+v", e)
	}
	out, _ := sp.edges(e.ID)
	if len(out) != 1 || out[0].RelationType != "uses" || out[0].ToID != merchant[0].ID {
		t.Fatalf("a relation field keeps its edge: %+v", out)
	}
	for name, props := range map[string]map[string]any{
		"number":    {"breed": "x", "age": "three"},
		"boolean":   {"breed": "x", "neutered": "maybe"},
		"date":      {"breed": "x", "birthday": "05/01"},
		"enum":      {"breed": "x", "size": "巨大"},
		"url":       {"breed": "x", "profile": "ftp://x"},
		"relation":  {"breed": "x", "shop": deal[0].ID},
		"missing":   {"breed": "x", "shop": "e999999"},
		"unknown":   {"breed": "x", "color": "白"},
		"required":  {"age": 1},
		"not a map": {"breed": []any{"x"}},
	} {
		if _, _, err := st.upsertEntity(EntityInput{Type: "pet", Title: "坏", Properties: props}, "你", t0); err == nil {
			t.Fatalf("%s: accepted %v", name, props)
		}
	}
	if _, _, err := st.upsertEntity(EntityInput{Type: "pet", Title: "坏", Status: "流浪", Properties: map[string]any{"breed": "x"}}, "你", t0); err == nil {
		t.Fatal("a status outside the workflow")
	}
	// Update: merge, null removes, the status moves, the type stays.
	e, _, err = st.upsertEntity(EntityInput{ID: e.ID, Status: "寄养", Properties: map[string]any{"age": 4, "profile": nil, "shop": nil}}, "你", t0.Add(time.Minute))
	if err != nil || e.Properties["age"] != 4.0 || e.Properties["profile"] != nil || e.Properties["breed"] != "007" || e.Status != "寄养" {
		t.Fatalf("update: %+v %v", e, err)
	}
	if out, _ := sp.edges(e.ID); len(out) != 0 {
		t.Fatalf("clearing the field drops its edge: %+v", out)
	}
	if _, _, err := st.upsertEntity(EntityInput{ID: e.ID, Type: "deal"}, "你", t0); err == nil {
		t.Fatal("the type does not change")
	}
	// Relations and the graph.
	rel, err := st.addRelation(sp.ID, e.ID, "belongs_to", merchant[0].ID, "你", t0)
	if err != nil || rel.ID == "" {
		t.Fatal(err)
	}
	if _, err := st.addRelation(sp.ID, e.ID, "Bad Type", merchant[0].ID, "你", t0); err == nil {
		t.Fatal("relation types are snake_case")
	}
	_, in := sp.edges(merchant[0].ID)
	if !slices.ContainsFunc(in, func(r Relation) bool { return r.FromID == e.ID && r.RelationType == "belongs_to" }) {
		t.Fatalf("incoming edges: %+v", in)
	}
	// Queries.
	if list, _ := sp.query(Query{Type: "pet", Filters: map[string]any{"age": 4}}); len(list) != 1 {
		t.Fatal("filter by number")
	}
	if list, _ := sp.query(Query{Type: "pet", Filters: map[string]any{"neutered": "true"}}); len(list) != 1 {
		t.Fatal("filter by boolean")
	}
	if list, _ := sp.query(Query{Keyword: "橘猫"}); len(list) != 1 {
		t.Fatal("keyword matches tags")
	}
	if list, _ := sp.query(Query{Type: "dynamic"}); len(list) == 0 || list[0].Type != TypeFeed {
		t.Fatal("dynamic is the feed")
	}
	// Built-in types gain fields only.
	if _, _, err := st.upsertType(sp, ObjectType{Key: TypeGoal, Fields: []FieldDef{{Key: "owner", Kind: KindText}}}, "你", t0); err != nil {
		t.Fatal(err)
	}
	gt, _ := sp.objectType(TypeGoal)
	if !gt.Builtin || len(gt.Statuses) != 4 || gt.Fields[len(gt.Fields)-1].Key != "owner" {
		t.Fatalf("evolved goal: %+v", gt)
	}
	// Archive, then delete: the edges go with it.
	if a, err := st.deleteEntity(e.ID, true, "你", t0); err != nil || !a.Archived {
		t.Fatal(err)
	}
	if list, _ := sp.query(Query{Type: "pet"}); len(list) != 0 {
		t.Fatal("archived entities are hidden by default")
	}
	if list, _ := sp.query(Query{Type: "pet", IncludeArchived: true}); len(list) != 1 {
		t.Fatal("include_archived")
	}
	if _, err := st.deleteEntity(e.ID, false, "你", t0); err != nil {
		t.Fatal(err)
	}
	if _, ok := sp.entity(e.ID); ok || slices.ContainsFunc(sp.Relations, func(r Relation) bool { return r.FromID == e.ID || r.ToID == e.ID }) {
		t.Fatal("deleted with its edges")
	}
	// Every change is in the feed.
	feed := feedEntries(sp)
	var events []string
	for _, f := range feed[:6] {
		events = append(events, f.Properties["event_type"].(string))
	}
	for _, want := range []string{"deleted", "archived", "schema_upserted", "related"} {
		if !slices.Contains(events, want) {
			t.Fatalf("feed lacks %s: %v", want, events)
		}
	}
	// The feed stays bounded.
	for i := 0; i < feedLimit+20; i++ {
		st.log(sp, "note", "x", "你", "", "", t0)
	}
	if n := sp.count(TypeFeed, false); n != feedLimit {
		t.Fatalf("feed %d", n)
	}
}

func req(ui, size string, version int, s *uiState) *ext.UIRequest {
	r := &ext.UIRequest{}
	r.Contribution, r.Size, r.UIVersion, r.TZ = ui, size, version, "Asia/Shanghai"
	if s != nil {
		r.State = ext.EncodeState(*s)
	}
	return r
}

// checkView holds a view to the schema and to what an app of version ver can draw.
func checkView(t *testing.T, name string, v *wire.View, ver int, compact bool) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := exttest.CheckView(raw, compact); err != nil {
		t.Fatalf("%s (v%d): %v", name, ver, err)
	}
	if ver < 4 && v.Tabs != nil || ver < 3 && (v.Filter != nil || v.Search != nil) {
		t.Fatalf("%s (v%d): tabs, filter or search for an older app", name, ver)
	}
	for _, s := range v.Sections {
		if ver < 3 && s.More != nil {
			t.Fatalf("%s (v%d): more for an older app", name, ver)
		}
		for _, c := range s.Items {
			if wire.ComponentSince[c.Type] > ver {
				t.Fatalf("%s (v%d): a %s", name, ver, c.Type)
			}
			if ver < 3 && c.Type == wire.ComponentRow && c.Icon != "" {
				t.Fatalf("%s (v%d): a row icon", name, ver)
			}
		}
	}
}

func find(v *wire.View, pred func(wire.Component) bool) *wire.Component {
	for _, s := range v.Sections {
		for i := range s.Items {
			if pred(s.Items[i]) {
				return &s.Items[i]
			}
		}
	}
	return nil
}

func TestMuseViewsAndIdeaIncubation(t *testing.T) {
	a := newApp(t.TempDir())
	ctx := context.Background()
	var anyID, dealID string
	a.db().read(func(st *State) {
		sp := mustSpace(t, st, "deals")
		ideas, _ := sp.query(Query{Type: TypeIdea})
		deals, _ := sp.query(Query{Type: "deal"})
		anyID, dealID = ideas[0].ID, deals[0].ID
	})
	// A custom type registered from the UI, then shown in the library.
	r := req(pageUI, wire.SizeFull, 5, &uiState{P: pageType, Space: "deals"})
	r.Action, r.Values = "type-save", map[string]string{"key": "subscription", "label": "订阅", "icon": "calendar",
		"fields": "fee:月费:number:元!; renew:续费日:date; plan:套餐:enum:月付|年付", "statuses": "在用, 已退订"}
	if res, err := a.action(ctx, r); err != nil || !res.Pop {
		t.Fatalf("type-save: %+v %v", res, err)
	}
	states := map[string]uiState{
		"activity":       {Tab: tabActivity},
		"ideas":          {Tab: tabIdeas},
		"goals":          {Tab: tabGoals},
		"library":        {Tab: tabLibrary},
		"library-deal":   {Tab: tabLibrary, Type: "deal"},
		"library-custom": {Tab: tabLibrary, Type: "subscription"},
		"library-search": {Tab: tabLibrary, Q: "耳机"},
		"entity":         {P: pageEntity, ID: dealID},
		"idea":           {P: pageEntity, ID: anyID},
		"edit":           {P: pageEdit, ID: dealID},
		"spaces":         {P: pageSpaces},
		"type":           {P: pageType, Space: "deals", Type: "subscription"},
		"gone":           {P: pageEntity, ID: "e0"},
	}
	for ver := 1; ver <= 5; ver++ {
		for name, s := range states {
			s := s
			v, err := a.render(ctx, req(pageUI, wire.SizeFull, ver, &s))
			if err != nil {
				t.Fatal(err)
			}
			checkView(t, name, v, ver, false)
		}
		v, _ := a.card(ctx, req(cardUI, wire.SizeCompact, ver, nil))
		checkView(t, "card", v, ver, true)
		for _, f := range []string{"small", "medium", "large", "accessory_rectangular", "accessory_inline"} {
			v, _ := a.widget(ctx, req(widgetUI, f, ver, nil))
			checkView(t, "widget-"+f, v, ver, true)
		}
	}
	// The library's table takes its columns from the type's fields.
	v, _ := a.render(ctx, req(pageUI, wire.SizeFull, 5, &uiState{Tab: tabLibrary, Type: "deal"}))
	tbl := find(v, func(c wire.Component) bool { return c.Type == wire.ComponentTable })
	if tbl == nil || tbl.Columns[2].Title != "平台" || tbl.Columns[4].Title != "商家条目" {
		t.Fatalf("table: %+v", tbl)
	}
	if v.Tabs == nil || v.Filter == nil || len(v.Filter.Choices) < 5 || v.Search == nil {
		t.Fatal("tabs, the space switcher and the search box")
	}
	// The entity view shows deal -> belongs_to -> merchant.
	v, _ = a.render(ctx, req(pageUI, wire.SizeFull, 5, &uiState{P: pageEntity, ID: dealID}))
	if find(v, func(c wire.Component) bool { return strings.HasPrefix(c.Subtitle, "→ 属于 → 商家/渠道") }) == nil {
		t.Fatal("the deal's belongs_to edge")
	}
	// Incubation from the ideas tab.
	r = req(pageUI, wire.SizeFull, 5, &uiState{Tab: tabIdeas})
	r.Action, r.Values = "idea-add", map[string]string{"title": "做一个比价小工具", "summary": "自动比价", "priority": "高", "tags": "工具"}
	if res, err := a.action(ctx, r); err != nil || !strings.Contains(res.Toast, "记下了") {
		t.Fatalf("idea-add: %+v %v", res, err)
	}
	var ideaID string
	a.db().read(func(st *State) {
		list, _ := st.activeSpace().query(Query{Type: TypeIdea, Keyword: "比价小工具"})
		ideaID = list[0].ID
	})
	inbox := find(adapt(mainView(current(a), r, uiState{Tab: tabIdeas}), 5), func(c wire.Component) bool { return c.Action != nil && c.Action.ID == "idea-goal:"+ideaID })
	if inbox == nil || inbox.Title != "孵化为目标" {
		t.Fatal("a 孵化为目标 button for the new idea")
	}
	r.Action, r.Values = "idea-goal:"+ideaID, nil
	if res, err := a.action(ctx, r); err != nil || !strings.Contains(res.Toast, "孵化为目标") {
		t.Fatalf("incubate: %+v %v", res, err)
	}
	a.db().read(func(st *State) {
		sp := st.activeSpace()
		idea, _ := sp.entity(ideaID)
		goalID, _ := idea.Properties["incubated_target_id"].(string)
		goal, ok := sp.entity(goalID)
		if idea.Status != IdeaIncubated || !ok || goal.Type != TypeGoal || goal.Title != idea.Title || goal.Properties["priority"] != "高" {
			t.Fatalf("incubated: %+v %+v", idea, goal)
		}
		out, _ := sp.edges(ideaID)
		if !slices.ContainsFunc(out, func(r Relation) bool { return r.RelationType == "spawns" && r.ToID == goalID }) {
			t.Fatalf("spawns edge: %+v", out)
		}
		last := feedEntries(sp)[0]
		if last.Properties["event_type"] != "incubated" || last.Properties["target_id"] != goalID {
			t.Fatalf("feed: %+v", last.Properties)
		}
	})
	// Goals: +10% and done.
	var goalID string
	a.db().read(func(st *State) {
		idea, _ := st.activeSpace().entity(ideaID)
		goalID = idea.Properties["incubated_target_id"].(string)
	})
	r = req(pageUI, wire.SizeFull, 5, &uiState{Tab: tabGoals})
	r.Action = "goal-step:" + goalID
	if res, _ := a.action(ctx, r); !strings.Contains(res.Toast, "10%") {
		t.Fatalf("step: %q", res.Toast)
	}
	r.Action = "goal-done:" + goalID
	_, _ = a.action(ctx, r)
	a.db().read(func(st *State) {
		g, _ := st.activeSpace().entity(goalID)
		if g.Status != GoalAchieved || goalRatio(g) != 1 || g.Properties["current_value"] != 100.0 {
			t.Fatalf("done: %+v", g)
		}
	})
	// Library: add a custom entity, edit it, relate it, delete it.
	r = req(pageUI, wire.SizeFull, 5, &uiState{Tab: tabLibrary, Type: "subscription"})
	r.Action, r.Values = "entity-add", map[string]string{"title": "视频会员", "status": "在用", "f_fee": "25", "f_plan": "月付", "f_renew": ""}
	if res, _ := a.action(ctx, r); !strings.Contains(res.Toast, "新增了") {
		t.Fatalf("entity-add: %q", res.Toast)
	}
	r.Values = map[string]string{"title": "缺月费", "f_plan": "月付"}
	if res, _ := a.action(ctx, r); !strings.Contains(res.Toast, "没有保存") {
		t.Fatalf("a required field: %q", res.Toast)
	}
	var subID string
	a.db().read(func(st *State) {
		list, _ := st.activeSpace().query(Query{Type: "subscription"})
		subID = list[0].ID
	})
	r = req(pageUI, wire.SizeFull, 5, &uiState{P: pageEdit, ID: subID})
	r.Action, r.Values = "entity-save", map[string]string{"title": "视频会员（年）", "f_fee": "198", "f_plan": "年付", "status": "在用"}
	if res, _ := a.action(ctx, r); res == nil || !res.Pop {
		t.Fatalf("save pops: %+v", res)
	}
	r = req(pageUI, wire.SizeFull, 2, &uiState{P: pageEdit, ID: subID})
	r.Action = "entity-save"
	if res, _ := a.action(ctx, r); res == nil || res.View == nil || res.Pop {
		t.Fatalf("an older app gets the detail: %+v", res)
	}
	r = req(pageUI, wire.SizeFull, 5, &uiState{P: pageEntity, ID: subID})
	r.Action, r.Values = "relate", map[string]string{"rel_type": "supports", "rel_target": goalID}
	_, _ = a.action(ctx, r)
	r.Action, r.Values = "delete:"+subID, nil
	if res, _ := a.action(ctx, r); res == nil || !res.Pop {
		t.Fatalf("delete pops: %+v", res)
	}
	// Spaces: create from a template, then switch with the filter.
	r = req(pageUI, wire.SizeFull, 5, &uiState{P: pageSpaces})
	r.Action, r.Values = "space-create", map[string]string{"template": "cloud", "name": "实验室", "icon": "-"}
	if res, _ := a.action(ctx, r); res == nil || !res.Pop {
		t.Fatalf("space-create: %+v", res)
	}
	v, _ = a.render(ctx, req(pageUI, wire.SizeFull, 5, nil))
	if v.Title != "实验室" {
		t.Fatalf("the new space is active: %q", v.Title)
	}
	r = req(pageUI, wire.SizeFull, 5, nil)
	r.Action, r.Values = "space", map[string]string{"space": "lifeos"}
	res, _ := a.action(ctx, r)
	if res.View.Title != "LifeOS 家庭空间" {
		t.Fatalf("switch: %q", res.View.Title)
	}
	r.Values = map[string]string{"space": newSpace}
	if res, _ := a.action(ctx, r); res.Push == nil {
		t.Fatal("+ 创建空间 opens the spaces page")
	}
	// An older app: tabs by segmented (v2) and buttons (v1).
	r = req(pageUI, wire.SizeFull, 1, nil)
	r.Action = "tab:goals"
	res, _ = a.action(ctx, r)
	if find(res.View, func(c wire.Component) bool { return c.Type == wire.ComponentProgress }) == nil {
		t.Fatal("v1 tab button goes to the goals")
	}
	// Post to the activity stream.
	r = req(pageUI, wire.SizeFull, 5, nil)
	r.Action, r.Values = "post", map[string]string{"note": "今天清点了冰箱"}
	res, _ = a.action(ctx, r)
	if find(res.View, func(c wire.Component) bool { return c.Title == "今天清点了冰箱" }) == nil {
		t.Fatal("the note is in the stream")
	}
}

// current is the app's current state, for drawing directly.
func current(a *app) *State {
	var out *State
	a.db().read(func(st *State) { out = st })
	return out
}

func TestShortcuts(t *testing.T) {
	a := newApp(t.TempDir())
	ctx := context.Background()
	run := func(id string, values map[string]string) (wire.ShortcutResult, error) {
		raw, _ := json.Marshal(wire.ShortcutRunParams{Contribution: id, Values: values, UIVersion: 3})
		res, err := a.shortcut(ctx, "run", raw)
		if err != nil {
			return wire.ShortcutResult{}, err
		}
		return res.(wire.ShortcutResult), nil
	}
	res, err := run(captureShortcut, map[string]string{"text": "周末去看看二手相机"})
	if err != nil || !strings.Contains(res.Text, "薅羊毛空间") {
		t.Fatalf("capture: %+v %v", res, err)
	}
	if err := wire.Validate("ShortcutResult", res); err != nil {
		t.Fatal(err)
	}
	if _, err := run(captureShortcut, map[string]string{}); err == nil {
		t.Fatal("an empty idea")
	}
	res, err = run(progressShortcut, map[string]string{"goal": "省钱", "value": "500"})
	if err != nil || !strings.Contains(res.Text, "已达成") {
		t.Fatalf("progress: %+v %v", res, err)
	}
	if _, err := run(progressShortcut, map[string]string{"goal": "不存在的目标", "value": "1"}); err == nil {
		t.Fatal("no such goal")
	}
	if _, err := run(progressShortcut, map[string]string{"goal": "省钱", "value": "x"}); err == nil {
		t.Fatal("not a number")
	}
}

// mcpClient talks to an mcpServer over pipes, as the assistant does over stdio.
type mcpClient struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Scanner
	id  int
}

func (c *mcpClient) call(method string, params any) map[string]any {
	c.t.Helper()
	c.id++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
	if !c.out.Scan() {
		c.t.Fatal("no answer")
	}
	var resp map[string]any
	if err := json.Unmarshal(c.out.Bytes(), &resp); err != nil {
		c.t.Fatal(err)
	}
	if resp["id"] != float64(c.id) {
		c.t.Fatalf("answer to %v, not %d", resp["id"], c.id)
	}
	return resp
}

// tool calls a tool and decodes its structured result; isErr says whether an error is wanted.
func (c *mcpClient) tool(name string, args any, isErr bool) map[string]any {
	c.t.Helper()
	resp := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		c.t.Fatalf("%s: %v", name, resp["error"])
	}
	if (res["isError"] == true) != isErr {
		c.t.Fatalf("%s: isError %v: %v", name, res["isError"], res["content"])
	}
	out, _ := res["structuredContent"].(map[string]any)
	return out
}

func TestMCPToolsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	hub := newApp(dir) // the hub's process, on the same directory
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := &mcpServer{store: openStore(dir, t.Logf)}
	done := make(chan error, 1)
	go func() { done <- srv.serve(context.Background(), inR, outW); _ = outW.Close() }()
	c := &mcpClient{t: t, in: inW, out: bufio.NewScanner(outR)}
	c.out.Buffer(make([]byte, 0, 1<<20), 16<<20)

	init := c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"}})
	if init["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "spaces" {
		t.Fatalf("initialize: %v", init)
	}
	_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	if c.call("ping", nil)["error"] != nil {
		t.Fatal("ping")
	}
	if c.call("nope", nil)["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatal("an unknown method")
	}
	tools := c.call("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, x := range tools {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	want := []string{"space_list", "space_query", "space_schema_get", "space_create", "space_schema_upsert", "space_entity_upsert", "space_entity_delete"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools %v", names)
	}

	list := c.tool("space_list", map[string]any{}, false)
	if list["active"] != "deals" || len(list["spaces"].([]any)) != 3 {
		t.Fatalf("space_list: %v", list)
	}
	q := c.tool("space_query", map[string]any{"space_id": "deals", "type": "deal", "keyword": "耳机"}, false)
	ents := q["entities"].([]any)
	if q["total"] != float64(1) {
		t.Fatalf("space_query: %v", q)
	}
	deal := ents[0].(map[string]any)
	edges := deal["relations"].(map[string]any)["out"].([]any)
	if len(edges) != 1 || edges[0].(map[string]any)["relation_type"] != "belongs_to" {
		t.Fatalf("the deal's edges: %v", deal["relations"])
	}
	if q := c.tool("space_query", map[string]any{"type": "coupon", "filters": map[string]any{"code": "007"}}, false); q["total"] != float64(1) {
		t.Fatalf("filters keep 007 a string: %v", q)
	}
	if q := c.tool("space_query", map[string]any{"type": "dynamic", "limit": 3}, false); q["type"] != "feed" || q["returned"] != float64(3) {
		t.Fatalf("dynamic: %v", q)
	}
	c.tool("space_query", map[string]any{"type": "ghost"}, true)
	c.tool("space_query", map[string]any{"bogus": 1}, true)
	schema := c.tool("space_schema_get", map[string]any{"space_id": "lifeos"}, false)
	if len(schema["types"].([]any)) != 8 || len(schema["views"].([]any)) < 4 {
		t.Fatalf("schema: %d types", len(schema["types"].([]any)))
	}

	created := c.tool("space_create", map[string]any{"template": "blank", "name": "读书空间", "space_id": "books"}, false)
	if created["active"] != "books" {
		t.Fatalf("space_create: %v", created)
	}
	c.tool("space_create", map[string]any{"template": "galaxy"}, true)
	up := c.tool("space_schema_upsert", map[string]any{"space_id": "books", "key": "book", "label": "书", "icon": "doc",
		"primary_field": "author", "statuses": []string{"想读", "在读", "读完"},
		"fields": []map[string]any{{"key": "author", "label": "作者", "kind": "text", "required": true},
			{"key": "pages", "label": "页数", "kind": "number", "unit": "页"}, {"key": "isbn", "label": "ISBN", "kind": "text"}}}, false)
	if up["created"] != true {
		t.Fatalf("schema_upsert: %v", up)
	}
	c.tool("space_schema_upsert", map[string]any{"space_id": "books", "key": "Bad Key"}, true)
	book := c.tool("space_entity_upsert", map[string]any{"space_id": "books", "type": "book", "title": "示例书",
		"properties": map[string]any{"author": "某作者", "pages": 320, "isbn": "0071"}}, false)
	be := book["entity"].(map[string]any)
	if be["properties"].(map[string]any)["isbn"] != "0071" || be["status"] != "想读" {
		t.Fatalf("entity_upsert: %v", be)
	}
	c.tool("space_entity_upsert", map[string]any{"space_id": "books", "type": "book", "title": "x", "properties": map[string]any{"pages": "many"}}, true)
	idea := c.tool("space_entity_upsert", map[string]any{"space_id": "books", "type": "idea", "title": "一年读 24 本书",
		"relations": []map[string]any{{"type": "relates_to", "target_id": be["id"]}}, "incubate_to_goal": true}, false)
	goal := idea["goal"].(map[string]any)
	if goal["type"] != "goal" || idea["entity"].(map[string]any)["status"] != "incubated" {
		t.Fatalf("incubate_to_goal: %v", idea)
	}
	upd := c.tool("space_entity_upsert", map[string]any{"id": be["id"], "status": "在读", "properties": map[string]any{"pages": nil}}, false)
	if _, has := upd["entity"].(map[string]any)["properties"].(map[string]any)["pages"]; has {
		t.Fatal("null removes a property")
	}
	c.tool("space_entity_delete", map[string]any{"id": be["id"], "archive": true}, false)
	del := c.tool("space_entity_delete", map[string]any{"id": be["id"]}, false)
	if del["deleted"] != true {
		t.Fatalf("delete: %v", del)
	}
	c.tool("space_entity_delete", map[string]any{"id": be["id"]}, true)
	c.tool("space_create", map[string]any{"action": "switch", "space_id": "deals"}, false)
	c.tool("space_create", map[string]any{"action": "archive", "space_id": "books"}, false)
	if l := c.tool("space_list", map[string]any{"include_archived": true}, false); len(l["spaces"].([]any)) != 4 {
		t.Fatal("include_archived lists the archived space")
	}
	if resp := c.call("tools/call", map[string]any{"name": "nope"}); resp["error"] == nil {
		t.Fatal("an unknown tool")
	}

	// The hub's process sees what the assistant wrote, on its next render.
	v, _ := hub.render(context.Background(), req(pageUI, wire.SizeFull, 5, &uiState{Tab: tabActivity}))
	if v.Title != "薅羊毛空间" {
		t.Fatalf("the hub's view: %q", v.Title)
	}
	hub.db().read(func(st *State) {
		sp := mustSpace(t, st, "books")
		if !sp.Archived || sp.count(TypeGoal, false) != 1 {
			t.Fatalf("hub state: %+v", sp)
		}
		last := feedEntries(sp)
		if !slices.ContainsFunc(last, func(e *Entity) bool { return e.Properties["actor"] == mcpActor }) {
			t.Fatal("the assistant's writes are in the feed as 助手")
		}
	})
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPreviewServer(t *testing.T) {
	a := newApp(t.TempDir())
	srv := httptest.NewServer(a.previewHandler())
	defer srv.Close()
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	get := func(path string) string {
		t.Helper()
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, b)
		}
		return string(b)
	}
	home := get("/")
	for _, want := range []string{"薅羊毛空间", "LifeOS 家庭空间", "云电脑", "创建空间", "主要聊天", "动态", "点子", "目标", "资源库", "总动态"} {
		if !strings.Contains(home, want) {
			t.Fatalf("home lacks %s", want)
		}
	}
	for path, want := range map[string]string{
		"/?tab=ideas":                "孵化为目标",
		"/?tab=goals":                "平均达成率",
		"/?tab=library":              "实体总数",
		"/?tab=library&type=deal":    "merchant_name: text",
		"/?tab=chat":                 "mcp__spaces__space_query",
		"/?tab=library&space=lifeos": "家庭库存",
	} {
		if !strings.Contains(get(path), want) {
			t.Fatalf("%s lacks %s", path, want)
		}
	}
	post := func(form url.Values) string {
		t.Helper()
		resp, err := client.PostForm(srv.URL+"/action", form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("action: %d", resp.StatusCode)
		}
		return resp.Header.Get("Location")
	}
	loc := post(url.Values{"op": {"idea-add"}, "title": {"<script>x</script>"}, "back_tab": {"ideas"}})
	if !strings.Contains(loc, "tab=ideas") {
		t.Fatalf("redirect %s", loc)
	}
	page := get("/?tab=ideas")
	if strings.Contains(page, "<script>x</script>") || !strings.Contains(page, "&lt;script&gt;") {
		t.Fatal("escaped")
	}
	post(url.Values{"op": {"type-upsert"}, "key": {"book"}, "label": {"书"}, "icon": {"doc"}, "fields": {"author:作者:text!"}, "statuses": {"想读, 读完"}})
	post(url.Values{"op": {"entity-add"}, "type": {"book"}, "title": {"示例书"}, "f_author": {"某人"}})
	if !strings.Contains(get("/?tab=library&type=book"), "示例书") {
		t.Fatal("the new entity")
	}
	if loc := post(url.Values{"op": {"goal-add"}, "title": {""}}); !strings.Contains(loc, "flash=") {
		t.Fatalf("an error is flashed: %s", loc)
	}
	post(url.Values{"op": {"create-space"}, "template": {"blank"}, "name": {"新空间"}})
	var state struct {
		Active string           `json:"active"`
		Spaces []map[string]any `json:"spaces"`
	}
	if err := json.Unmarshal([]byte(get("/api/state")), &state); err != nil || len(state.Spaces) != 4 || state.Active != "space" {
		t.Fatalf("state: %+v %v", state, err)
	}
	if !strings.Contains(get("/api/schema?space=deals"), `"relation_types"`) {
		t.Fatal("schema")
	}
}

func TestFieldSpec(t *testing.T) {
	fields, err := parseFieldSpec("a:甲:number:元!; b:乙:enum:x|y\nc:丙:relation:merchant:belongs_to")
	if err != nil || len(fields) != 3 || !fields[0].Required || fields[0].Unit != "元" || len(fields[1].Options) != 2 || fields[2].RelationType != "belongs_to" {
		t.Fatalf("%+v %v", fields, err)
	}
	again, _ := parseFieldSpec(fieldSpec(fields))
	if fieldSpec(again) != fieldSpec(fields) {
		t.Fatal("round trip")
	}
	if _, err := parseFieldSpec(" ; "); err == nil {
		t.Fatal("no fields")
	}
}
