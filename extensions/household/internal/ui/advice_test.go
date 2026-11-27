package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/household/internal/rules"
)

func TestAdviceBlockAgreesWithTheNumbers(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	v := render(t, a, state{})
	checkView(t, "overview with advice", v, false)
	sec, ok := sectionTitled(v, "今天的建议")
	if !ok {
		t.Fatal("no 今天的建议")
	}
	if v.Sections[1].Title != "今天的建议" {
		t.Fatalf("section 1 is %q, want the advice right under the numbers", v.Sections[1].Title)
	}
	if len(sec.Items) > 5 {
		t.Fatalf("%d lines of advice", len(sec.Items))
	}
	sum := rules.Summarize(home(t, st), testToday)
	stats := overviewGrid(t, v).Stats
	want := []struct{ prefix, tab, stat string }{
		{fmt.Sprintf("先吃 %d 样", sum.Expiring), "tab:stock", stats[3].Value},
		{"可以做：", "tab:meal", ""},
		{fmt.Sprintf("该买 %d 样", sum.Expired+sum.Out+sum.Low+sum.Runout), "tab:buy", ""},
		{fmt.Sprintf("家务 %d 件", sum.ChoresDue), "tab:chore", stats[2].Value},
	}
	for i, w := range want {
		row := sec.Items[i]
		if !strings.HasPrefix(row.Title, w.prefix) || row.Subtitle == "" || row.Action == nil || row.Action.ID != w.tab {
			t.Fatalf("advice line %d %+v, want %q opening %s", i, row, w.prefix, w.tab)
		}
		if w.stat != "" && !strings.Contains(row.Title, " "+w.stat+" ") {
			t.Fatalf("line %q does not carry the number %s shown above", row.Title, w.stat)
		}
	}
	// The last line is the rest of the shopping list: with the 该买 items already on it, it is the 待买 number.
	last := sec.Items[len(sec.Items)-1]
	if !strings.HasPrefix(last.Title, "采购清单上") || last.Action == nil || last.Action.ID != "tab:buy" || last.Subtitle == "" {
		t.Fatalf("last advice line %+v, want the shopping list opening 采购", last)
	}
	// 该买 is the 补货 part of 需要处理.
	if !strings.Contains(stats[0].Detail, fmt.Sprintf("补货 %d", sum.Expired+sum.Out+sum.Low+sum.Runout)) {
		t.Fatalf("需要处理 detail %q", stats[0].Detail)
	}
	// The cook line is the first meal idea of 今天吃什么.
	meals, _ := sectionTitled(v, "今天吃什么")
	if sec.Items[1].Title != "可以做："+meals.Items[0].Title {
		t.Fatalf("cook %q, first meal idea %q", sec.Items[1].Title, meals.Items[0].Title)
	}
	// A line opens its tab.
	res := act(t, a, state{}, nil, sec.Items[2].Action.ID)
	if viewState(t, res.View).T != buyTab {
		t.Fatalf("tap on 该买 went to %+v", viewState(t, res.View))
	}
}

func TestAdviceBlockWhenNothingIsDue(t *testing.T) {
	st := newStore(t, false)
	a, _ := newApp(st)
	act(t, a, state{F: "new:item"}, map[string]string{"title": "盐"}, "save")
	if entityCount(t, st) != 1 {
		t.Fatal("the item was not saved")
	}
	ov := render(t, a, state{})
	sec, ok := sectionTitled(ov, "今天的建议")
	if !ok || len(sec.Items) != 1 || sec.Items[0].Type != wire.ComponentText || sec.Items[0].Text != "今天没有要特别注意的" {
		t.Fatalf("advice on a quiet household %+v", sec)
	}
}

func TestAdviceBlockNamesTheOpenShoppingList(t *testing.T) {
	st := newStore(t, false)
	a, _ := newApp(st)
	for _, n := range []string{"牛奶", "鸡蛋", "洗洁精", "卷纸"} {
		act(t, a, state{F: "new:buy"}, map[string]string{"title": n}, "save")
	}
	v := render(t, a, state{})
	checkView(t, "overview with a shopping list", v, false)
	if got := overviewGrid(t, v).Stats[1]; got.Label != "待买" || got.Value != "4" {
		t.Fatalf("待买 %+v", got)
	}
	sec, _ := sectionTitled(v, "今天的建议")
	if len(sec.Items) != 1 {
		t.Fatalf("advice %+v, want one line", sec.Items)
	}
	row := sec.Items[0]
	if row.Title != "采购清单上还有 4 样没买：牛奶、鸡蛋、洗洁精等" || row.Subtitle == "" || row.Action == nil || row.Action.ID != "tab:buy" {
		t.Fatalf("shopping line %+v", row)
	}
	if strings.Contains(fmt.Sprint(v.Sections), nothingToday) {
		t.Fatal("says there is nothing while 待买 is 4")
	}
}

func TestAdviceLineSaysOtherWhenRestockIsOnTheList(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	act(t, a, state{}, nil, "restock") // the 该买 items go on the list too
	act(t, a, state{F: "new:buy"}, map[string]string{"title": "卷纸"}, "save")
	sec, _ := sectionTitled(render(t, a, state{}), "今天的建议")
	last := sec.Items[len(sec.Items)-1]
	if !strings.HasPrefix(last.Title, "采购清单上另外还有 2 样没买：") {
		t.Fatalf("last line %q", last.Title)
	}
}
