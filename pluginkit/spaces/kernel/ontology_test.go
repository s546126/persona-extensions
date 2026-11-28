package kernel

import (
	"slices"
	"testing"
	"time"
)

func TestWorldOntologyHelpers(t *testing.T) {
	w, sp := sampleWorld(t)

	edges := w.SchemaEdges()
	if len(edges) != 4 {
		t.Fatalf("SchemaEdges = %+v, want 4 edges", edges)
	}
	if edges[0].FromType != "goal" || edges[0].PropKey != "picks" || edges[0].ToType != "task" || !edges[0].Many {
		t.Errorf("first edge = %+v", edges[0])
	}
	back := w.BacklinkProps("goal")
	if len(back) != 2 || back[0].FromType != "task" || back[0].PropKey != "goal" || back[1].FromType != "note" || back[1].PropKey != "about" {
		t.Errorf("BacklinkProps(goal) = %+v", back)
	}
	if got := w.BacklinkProps(""); got != nil {
		t.Errorf("BacklinkProps(empty) = %+v", got)
	}

	sum := w.Summary()
	if sum.Types != 3 || sum.Total != len(sp.Entities) || sum.Todo != 3 || sum.Doing != 2 || sum.Done != 1 || sum.Dropped != 1 {
		t.Errorf("Summary = %+v", sum)
	}
	if sum.PercentDone == nil || *sum.PercentDone < 0.16 || *sum.PercentDone > 0.17 {
		t.Errorf("PercentDone = %v, want 1/6", sum.PercentDone)
	}
	if sum.Relations == 0 {
		t.Errorf("Relations = 0, want > 0")
	}

	// Nil world safety.
	var nilWorld *World
	if nilWorld.SchemaEdges() != nil || nilWorld.Upcoming("2026-11-10", 7) != nil || nilWorld.Summary().Total != 0 {
		t.Errorf("nil world helpers returned non-empty values")
	}
}

func TestWorldUpcoming(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	sp := &Space{
		ID: "s1", Name: "日程",
		Types: []*Type{
			{
				Key: "task", Name: "任务",
				Flow: []Stage{
					{Name: "待办", Phase: Todo},
					{Name: "进行中", Phase: Doing},
					{Name: "完成", Phase: Done},
					{Name: "取消", Phase: Dropped},
				},
				Props: []Prop{
					{Key: "start", Name: "开始", Kind: KindDate},
					{Key: "due", Name: "截止", Kind: KindDate},
				},
			},
			{
				Key: "log", Name: "记录",
				Props: []Prop{
					{Key: "date", Name: "日期", Kind: KindDate},
				},
			},
		},
		Entities: []*Entity{
			{ID: "e1", Type: "task", Title: "逾期两天", Status: "进行中", Values: map[string]any{"due": "2026-11-08"}, Created: now, Updated: now},
			{ID: "e2", Type: "task", Title: "今天截止", Status: "待办", Values: map[string]any{"due": "2026-11-10"}, Created: now, Updated: now},
			{ID: "e3", Type: "task", Title: "三天后", Status: "待办", Values: map[string]any{"start": "2026-11-13", "due": "2026-11-15"}, Created: now, Updated: now},
			{ID: "e4", Type: "task", Title: "已完成不算", Status: "完成", Values: map[string]any{"due": "2026-11-10"}, Created: now, Updated: now},
			{ID: "e5", Type: "task", Title: "已取消不算", Status: "取消", Values: map[string]any{"due": "2026-11-11"}, Created: now, Updated: now},
			{ID: "e6", Type: "task", Title: "太久远逾期", Status: "待办", Values: map[string]any{"due": "2026-09-01"}, Created: now, Updated: now},
			{ID: "e7", Type: "task", Title: "超出窗口", Status: "待办", Values: map[string]any{"due": "2026-12-01"}, Created: now, Updated: now},
			{ID: "e8", Type: "log", Title: "过去打卡不算逾期", Values: map[string]any{"date": "2026-11-08"}, Created: now, Updated: now},
			{ID: "e9", Type: "log", Title: "明天提醒", Values: map[string]any{"date": "2026-11-11"}, Created: now, Updated: now},
		},
	}
	b := &Base{Format: 1, Spaces: []*Space{sp}}
	if err := b.Check(); err != nil {
		t.Fatal(err)
	}
	w := Open(sp)

	got := w.Upcoming("2026-11-10", 7)
	var ids []string
	var days []int
	for _, it := range got {
		ids = append(ids, it.Entity.ID)
		days = append(days, it.Days)
	}
	wantIDs := []string{"e1", "e2", "e9", "e3"}
	wantDays := []int{-2, 0, 1, 3}
	if !slices.Equal(ids, wantIDs) || !slices.Equal(days, wantDays) {
		t.Fatalf("Upcoming = ids %v days %v, want %v %v", ids, days, wantIDs, wantDays)
	}
	// Multi-date entity picks its earliest qualifying date (start on +3 rather than due on +5).
	if got[3].Prop.Key != "start" || got[3].Date != "2026-11-13" {
		t.Errorf("multi-date entity picked %+v, want start 2026-11-13", got[3])
	}
	if w.Upcoming("bad-date", 7) != nil || w.Upcoming("2026-11-10", -1) != nil {
		t.Errorf("invalid arguments to Upcoming should return nil")
	}
}

func TestMatchTextMatchesStatusEnumAndRelations(t *testing.T) {
	w, _ := sampleWorld(t)
	// Match enum value ("工作" on g1).
	if got := runIDs(t, w, Query{Type: "goal", Text: "工作"}); !slices.Equal(got, []string{"g1"}) {
		t.Errorf("enum search = %v, want [g1]", got)
	}
	// Match status ("进行" on t2).
	if got := runIDs(t, w, Query{Type: "task", Text: "进行"}); !slices.Equal(got, []string{"t2"}) {
		t.Errorf("status search = %v, want [t2]", got)
	}
	// Match linked record title ("目标一" on tasks linked to g1).
	if got := runIDs(t, w, Query{Type: "task", Text: "目标一"}); !slices.Equal(got, []string{"t1", "t2", "t3"}) {
		t.Errorf("relation title search = %v, want [t1 t2 t3]", got)
	}
}
