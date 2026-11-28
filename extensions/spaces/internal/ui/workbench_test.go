package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/spaces/internal/templates"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/store"
)

func TestWorkbenchOverviewFocusAndGraphAcrossVersions(t *testing.T) {
	st := storeWith(t, true, "subscription", "project", "finance", "media")
	a := newApp(st)
	var subSpace, projSpace, finSpace string
	_ = st.View(func(b *kernel.Base) error {
		subSpace = b.Spaces[0].ID
		projSpace = b.Spaces[1].ID
		finSpace = b.Spaces[2].ID
		return nil
	})

	// Subscription space at v10: responsive columns layout, upcoming renewals surfaced in 需要关注, doing items in 正在进行,
	// and relation graph button available.
	v := drawPage(t, a, state{S: subSpace}, 10)
	if v.Layout != wire.LayoutColumns {
		t.Fatalf("overview layout at v10 = %q, want columns", v.Layout)
	}
	upcoming := sectionTitled(t, v, "需要关注 · 3")
	if len(upcoming.Items) != 3 {
		t.Fatalf("upcoming items = %d, want 3", len(upcoming.Items))
	}
	if upcoming.Items[0].Title != "健身房月卡" || upcoming.Items[0].Badge == nil || upcoming.Items[0].Badge.Text != "3 天后" {
		t.Fatalf("first upcoming = %+v", upcoming.Items[0])
	}
	if _, ok := buttonTitled(v, "关系图"); !ok {
		t.Fatal("v10 overview with relations lacks 关系图 button")
	}

	// Open the space relation graph fold at v10 and lower it down to v1..v9: every version passes wire.CheckView.
	for _, ver := range []int{10, 8, 6, 5, 4, 3, 2, 1} {
		gv := drawPage(t, a, state{S: projSpace, F: "graph"}, ver)
		if ver >= 10 {
			graphs := componentsOf(gv, wire.ComponentGraph)
			if len(graphs) != 1 || len(graphs[0].Nodes) != 4 || len(graphs[0].Edges) == 0 {
				t.Fatalf("v%d graph = %+v", ver, graphs)
			}
		} else {
			if graphs := componentsOf(gv, wire.ComponentGraph); len(graphs) != 0 {
				t.Fatalf("v%d still has graph component", ver)
			}
			checkLowered(t, fmt.Sprintf("graph v%d", ver), gv, ver)
		}
	}

	// Finance space overview & board at v10, v8, v6, v5.
	for _, ver := range []int{10, 8, 6, 5} {
		fv := drawPage(t, a, state{S: finSpace}, ver)
		if ver >= 8 && fv.Layout != wire.LayoutColumns {
			t.Fatalf("finance v%d layout = %q", ver, fv.Layout)
		}
		if ver < 8 && fv.Layout != "" {
			t.Fatalf("finance v%d layout not lowered: %q", ver, fv.Layout)
		}
	}

	// Space-level quick capture adds an entity without switching tabs.
	qfold := drawPage(t, a, state{S: projSpace, F: "quick"}, 10)
	if !fieldNamedOK(qfold, "type") || !fieldNamedOK(qfold, "title") {
		t.Fatalf("quick fold fields = %v", fieldNamesOf(qfold))
	}
	res := act(t, a, state{S: projSpace, F: "quick"}, "quickadd", map[string]string{
		"type": "=task", "title": "首页速记任务",
	})
	if !strings.Contains(res.Toast, "已添加到「任务」") || decode(t, res.View).F != "" {
		t.Fatalf("quickadd result = %q / %+v", res.Toast, decode(t, res.View))
	}
	if _, ok := stored(t, st, entityID(t, st, "首页速记任务")); !ok {
		t.Fatal("quickadd entity not stored")
	}
}

func TestSpaceGraphStaysAcyclicOnMutualRelations(t *testing.T) {
	st := store.Open("")
	var sid string
	err := st.Update(func(b *kernel.Base, _ time.Time) error {
		sp, err := templates.Apply(b, "blank", "互连空间", false, testNow)
		if err != nil {
			return err
		}
		sid = sp.ID
		if err := sp.PutType(kernel.Type{Key: "a", Name: "甲"}, testNow); err != nil {
			return err
		}
		if err := sp.PutType(kernel.Type{
			Key: "b", Name: "乙",
			Props: []kernel.Prop{{Key: "to_a", Name: "到甲", Kind: kernel.KindRelation, Target: "a"}},
		}, testNow); err != nil {
			return err
		}
		// Add a mutual edge a -> b and a self-edge a -> a.
		return sp.PutType(kernel.Type{
			Key: "a", Name: "甲",
			Props: []kernel.Prop{
				{Key: "to_b", Name: "到乙", Kind: kernel.KindRelation, Target: "b"},
				{Key: "self", Name: "自己", Kind: kernel.KindRelation, Target: "a"},
			},
		}, testNow)
	})
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(st)
	v := drawPage(t, a, state{S: sid, F: "graph"}, 10)
	graphs := componentsOf(v, wire.ComponentGraph)
	if len(graphs) != 1 || len(graphs[0].Edges) != 1 {
		t.Fatalf("mutual graph should drop back-edge and self-edge, got %+v", graphs)
	}
}

func TestRowQuickActionsStatusToggleAndDelete(t *testing.T) {
	st := storeWith(t, true, "project", "travel")
	a := newApp(st)
	var projSpace, travelSpace string
	_ = st.View(func(b *kernel.Base) error {
		projSpace = b.Spaces[0].ID
		travelSpace = b.Spaces[1].ID
		return nil
	})

	// On UI >= 6, task board rows carry swipe advance action; on UI v5 they are stripped.
	v6 := drawPage(t, a, state{S: projSpace, T: "task"}, 6)
	if v6.Layout != "" {
		t.Fatalf("board at v6 should lower columns layout, got %q", v6.Layout)
	}
	v8 := drawPage(t, a, state{S: projSpace, T: "task"}, 8)
	if v8.Layout != wire.LayoutColumns {
		t.Fatalf("board at v8 layout = %q, want columns", v8.Layout)
	}
	taskID := entityID(t, st, "制定 12 周训练计划")
	var foundSwipe string
	for _, r := range componentsOf(v6, wire.ComponentRow) {
		if r.Title == "制定 12 周训练计划" {
			for _, ra := range r.Actions {
				if ra.Swipe {
					foundSwipe = ra.Action.ID
				}
			}
		}
	}
	if foundSwipe != "quickstatus:"+taskID+":进行中" {
		t.Fatalf("swipe action = %q, want quickstatus:%s:进行中", foundSwipe, taskID)
	}
	res := act(t, a, state{S: projSpace, T: "task"}, foundSwipe, nil)
	if !strings.Contains(res.Toast, "已改为「进行中」") {
		t.Fatalf("quickstatus toast = %q", res.Toast)
	}
	if got, _ := stored(t, st, taskID); got.Status != "进行中" {
		t.Fatalf("status after quickstatus = %q", got.Status)
	}

	// Flowless type with boolean property (travel pack.packed) gets one-swipe toggle.
	packID := entityID(t, st, "薄外套")
	tv := drawPage(t, a, state{S: travelSpace, T: "pack"}, 6)
	var toggleAct string
	for _, r := range componentsOf(tv, wire.ComponentRow) {
		if r.Title == "薄外套" {
			for _, ra := range r.Actions {
				if ra.Swipe {
					toggleAct = ra.Action.ID
				}
			}
		}
	}
	if toggleAct != "toggle:"+packID+":packed" {
		t.Fatalf("toggle swipe action = %q", toggleAct)
	}
	res = act(t, a, state{S: travelSpace, T: "pack"}, toggleAct, nil)
	if !strings.Contains(res.Toast, "已标记已装") {
		t.Fatalf("toggle toast = %q", res.Toast)
	}
	if got, _ := stored(t, st, packID); got.Values["packed"] != true {
		t.Fatalf("packed after toggle = %v", got.Values["packed"])
	}

	// Quick delete removes the record and keeps the user on the tab.
	res = act(t, a, state{S: travelSpace, T: "pack"}, "quickdelete:"+packID, nil)
	if !strings.Contains(res.Toast, "已删除「薄外套」") || res.Pop {
		t.Fatalf("quickdelete = %+v", res)
	}
	if _, ok := stored(t, st, packID); ok {
		t.Fatal("entity still present after quickdelete")
	}
}

func TestInlinePhaseFilterAndGroupAggregates(t *testing.T) {
	st := storeWith(t, true, "project", "finance")
	a := newApp(st)
	var projSpace, finSpace string
	_ = st.View(func(b *kernel.Base) error {
		projSpace = b.Spaces[0].ID
		finSpace = b.Spaces[1].ID
		return nil
	})

	// Task table view supports inline phase filter.
	res := act(t, a, state{S: projSpace, T: "task", V: 1}, "phase", map[string]string{"phase": "done"})
	if got := decode(t, res.View); got.P != "done" {
		t.Fatalf("phase state = %+v", got)
	}
	tbl := componentsOf(res.View, wire.ComponentTable)
	if len(tbl) != 1 || len(tbl[0].Rows) != 2 {
		t.Fatalf("done tasks in table = %+v, want 2 rows", tbl)
	}
	// Reset phase filter back to all.
	res = act(t, a, state{S: projSpace, T: "task", V: 1, P: "done"}, "phase:all", nil)
	if got := decode(t, res.View); got.P != "" {
		t.Fatalf("phase reset = %+v", got)
	}

	// Finance expense grouped by budget shows group aggregate sum in each group's footer!
	fv := drawPage(t, a, state{S: finSpace, T: "expense", V: 1}, 10)
	foodSec := sectionTitled(t, fv, "餐饮买菜 · 2")
	if !strings.Contains(foodSec.Footer, "221 元") {
		t.Fatalf("grouped footer = %q, want 221 元", foodSec.Footer)
	}
}

func TestQuickAddWithOptionalFieldsAndContextualLinkAdd(t *testing.T) {
	st := storeWith(t, true, "project")
	a := newApp(st)
	sid := firstSpace(t, st)
	goalID := entityID(t, st, "连续写满 12 篇周复盘")

	// Quick-add on task tab accepts title + optional priority/effort/goal in one submit.
	res := act(t, a, state{S: sid, T: "task", F: "new"}, "add", map[string]string{
		"title":      "整理复盘目录",
		"p_priority": "=高",
		"p_effort":   "3",
		"p_goal":     "=" + goalID,
	})
	if res.Toast != "已添加「整理复盘目录」" {
		t.Fatalf("quick add toast = %q", res.Toast)
	}
	added, ok := stored(t, st, entityID(t, st, "整理复盘目录"))
	if !ok || added.Values["priority"] != "高" || added.Values["effort"] != 3.0 {
		t.Fatalf("quick added task = %+v", added)
	}

	// Contextual linked creation from the goal entity page pre-binds task.goal = goalID and updates rollups.
	gv := drawPage(t, a, state{S: sid, E: goalID}, 10)
	in := sectionTitled(t, gv, "连到它的")
	foundNewTask := false
	for _, it := range in.Items {
		if it.Type == wire.ComponentButton && it.Title == "新建任务" && it.Action.ID == "fold:linknew:task:goal" {
			foundNewTask = true
		}
	}
	if !foundNewTask {
		t.Fatalf("goal backlinks lack 新建任务 button: %+v", in.Items)
	}
	fv := drawPage(t, a, state{S: sid, E: goalID, F: "linknew:task:goal"}, 10)
	if !fieldNamedOK(fv, "title") || fieldNamedOK(fv, "p_goal") {
		t.Fatalf("linknew fold fields = %v (goal should be pre-bound and omitted)", fieldNamesOf(fv))
	}
	res = act(t, a, state{S: sid, E: goalID, F: "linknew:task:goal"}, "linkadd:task:goal", map[string]string{
		"title": "写第 3 篇", "p_priority": "=中", "p_effort": "1.5",
	})
	if res.Toast != "已添加「写第 3 篇」" || decode(t, res.View).E != goalID || decode(t, res.View).F != "" {
		t.Fatalf("linkadd result = %q / %+v", res.Toast, decode(t, res.View))
	}
	child, ok := stored(t, st, entityID(t, st, "写第 3 篇"))
	if !ok || fmt.Sprint(child.Values["goal"]) != fmt.Sprint([]string{goalID}) || child.Values["effort"] != 1.5 {
		t.Fatalf("linked child = %+v", child)
	}
	// Parent goal facts immediately reflect the new task count (4 original + 2 added = 6).
	if facts := head2Facts(t, res.View); facts["任务数"] != "6" {
		t.Fatalf("updated parent rollup facts = %v, want 任务数=6", facts)
	}
}

func TestNativeV6DateAndMultichoiceFields(t *testing.T) {
	st, sid := kindsStore(t)
	a := newApp(st)
	item := entityID(t, st, "条目")
	one := entityID(t, st, "对象甲")
	two := entityID(t, st, "对象乙")

	// UI v6 renders native date and multichoice fields.
	v6 := drawPage(t, a, state{S: sid, E: item, F: "edit"}, 6)
	if f := fieldNamed(t, v6, "p_day"); f.Kind != wire.FieldKindDate {
		t.Fatalf("v6 date field = %+v", f)
	}
	if f := fieldNamed(t, v6, "p_many"); f.Kind != wire.FieldKindMultichoice || len(f.Choices) != 2 {
		t.Fatalf("v6 multichoice field = %+v", f)
	}

	// Submitting JSON array replaces the relation list in one save, and [] clears it.
	rawBoth, _ := json.Marshal([]string{one, two})
	res, err := a.Act(context.Background(), request(6, state{S: sid, E: item, F: "edit"}, map[string]string{
		"title": "条目", "p_many": string(rawBoth), "p_day": "2026-12-01",
	}, "update"))
	if err != nil {
		t.Fatal(err)
	}
	checkView(t, "v6 update", res.View, false)
	got, _ := stored(t, st, item)
	if fmt.Sprint(got.Values["many"]) != fmt.Sprint([]string{one, two}) || got.Values["day"] != "2026-12-01" {
		t.Fatalf("after v6 multichoice save = %+v", got.Values)
	}

	res, err = a.Act(context.Background(), request(6, state{S: sid, E: item, F: "edit"}, map[string]string{
		"title": "条目", "p_many": "[]",
	}, "update"))
	if err != nil {
		t.Fatal(err)
	}
	checkView(t, "v6 clear", res.View, false)
	if got, _ := stored(t, st, item); hasKey(got.Values, "many") {
		t.Fatalf("multichoice [] did not clear relation: %+v", got.Values)
	}
}

func TestEditorAutoKeyPresetsAndViewLensEditing(t *testing.T) {
	st := storeWith(t, false, "blank")
	a := newApp(st)
	sid := firstSpace(t, st)

	// Create type leaving key blank -> auto-generated key "film".
	res := act(t, a, state{S: sid, D: newSlot}, "typenew", map[string]string{"t_name": "电影", "t_key": ""})
	if res.Toast != "已新建「电影」" || decode(t, res.View).D != "film" {
		t.Fatalf("auto-key type = %q / %+v", res.Toast, decode(t, res.View))
	}
	// Second type with same Chinese word -> unique key "film1".
	res = act(t, a, state{S: sid, D: newSlot}, "typenew", map[string]string{"t_name": "电影", "t_key": ""})
	if decode(t, res.View).D != "film1" {
		t.Fatalf("collision auto-key = %+v", decode(t, res.View))
	}

	// One-click lifecycle preset on empty flow.
	res = act(t, a, state{S: sid, D: "film", F: "stage:+"}, "stagepreset:todo3", nil)
	if res.Toast != "已套用预设状态" {
		t.Fatalf("preset toast = %q", res.Toast)
	}
	if flow := mustType(t, st, sid, "film").Flow; len(flow) != 3 || flow[0].Name != "待办" || flow[2].Phase != kernel.Done {
		t.Fatalf("preset flow = %+v", flow)
	}

	// Add properties leaving key blank -> auto-generated keys "director" and "price".
	res = act(t, a, state{S: sid, D: "film", F: "new:text"}, "propnew:text", map[string]string{"p_name": "导演", "p_key": ""})
	if res.Toast != "已添加「导演」" {
		t.Fatalf("auto-key prop 1 = %q", res.Toast)
	}
	res = act(t, a, state{S: sid, D: "film", F: "new:number"}, "propnew:number", map[string]string{"p_name": "价格", "p_key": "", "o_unit": "元"})
	if res.Toast != "已添加「价格」" {
		t.Fatalf("auto-key prop 2 = %q", res.Toast)
	}
	ty := mustType(t, st, sid, "film")
	if len(ty.Props) != 2 || ty.Props[0].Key != "director" || ty.Props[1].Key != "price" {
		t.Fatalf("auto-key props = %+v", ty.Props)
	}

	// Create a view with primary filter and primary aggregate directly from the schema editor!
	res = act(t, a, state{S: sid, D: "film", F: "view:+"}, "viewsave:+", map[string]string{
		"v_name":   "在看清单",
		"v_kind":   "list",
		"v_group":  "=",
		"v_sort":   "=price",
		"v_desc":   "true",
		"v_wfield": "=phase",
		"v_wop":    "ne",
		"v_wval":   "done",
		"v_aop":    "=sum",
		"v_aof":    "=price",
	})
	if res.Toast != "已保存" {
		t.Fatalf("view save toast = %q", res.Toast)
	}
	views := mustType(t, st, sid, "film").Views
	if len(views) != 1 || len(views[0].Where) != 1 || views[0].Where[0].Field != "phase" || views[0].Where[0].Op != "ne" ||
		len(views[0].Agg) != 1 || views[0].Agg[0].Op != "sum" || views[0].Agg[0].Of != "price" {
		t.Fatalf("saved view lens = %+v", views)
	}
}

func TestCrowdedWorkbenchRespectsRowActionBudget(t *testing.T) {
	st := crowded(t, 120, false)
	a := newApp(st)
	sid := firstSpace(t, st)
	// Load 120 rows at v10: row actions must stop at wire.RowsWithActionsMost (100) and pass wire.CheckView!
	v := drawPage(t, a, state{S: sid, T: "item", N: 6}, 10)
	withActs := 0
	for _, r := range componentsOf(v, wire.ComponentRow) {
		if len(r.Actions) > 0 {
			withActs++
		}
	}
	if withActs > wire.RowsWithActionsMost {
		t.Fatalf("rows with actions = %d, exceeds %d", withActs, wire.RowsWithActionsMost)
	}
	if withActs != wire.RowsWithActionsMost {
		t.Fatalf("rows with actions = %d, want capped at %d", withActs, wire.RowsWithActionsMost)
	}
	// Verify every template works cleanly at v10 too.
	allSt := storeWith(t, true, templateKeys()...)
	allApp := newApp(allSt)
	var ids []string
	_ = allSt.View(func(b *kernel.Base) error {
		for _, sp := range b.Spaces {
			ids = append(ids, sp.ID)
		}
		return nil
	})
	for _, id := range ids {
		for _, ver := range []int{10, 8, 6} {
			drawPage(t, allApp, state{S: id}, ver)
		}
	}
	if !slices.Contains(templateKeys(), "finance") || !slices.Contains(templateKeys(), "media") {
		t.Fatal("missing new templates")
	}
}
