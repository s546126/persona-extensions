package rules

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
)

// adviceHome is mealHome with a low item, one running out, one on the shopping list, two entries of the list for
// nothing that needs buying (one closed), and two chores.
func adviceHome() *model.Home {
	h := mealHome()
	h.Items = append(h.Items,
		model.Item{ID: "yog", Title: "酸奶", Qty: 2, HasQty: true, Expires: today},
		model.Item{ID: "salt", Title: "盐", Qty: 1, HasQty: true, Min: 1, HasMin: true, Unit: "袋"},
		model.Item{ID: "dia", Title: "纸尿裤", Qty: 20, HasQty: true, Daily: 5, Counted: today, Unit: "片", Category: model.CategoryBaby},
	)
	h.Buys = []model.Buy{
		{ID: "b1", Title: "大米", Status: model.BuyTodo, ItemID: "rice"},
		{ID: "b2", Title: "卷纸", Status: model.BuyTodo, Amount: 2, Unit: "提"},
		{ID: "b3", Title: "洗洁精", Status: model.BuyDone},
		{ID: "b4", Title: "酱油", Status: model.BuyTodo},
	}
	h.Chores = []model.Chore{
		{ID: "c1", Title: "拖地", Next: today.Add(-3)},
		{ID: "c2", Title: "倒垃圾", Next: today},
		{ID: "c3", Title: "擦窗", Next: today.Add(5)},
	}
	return h
}

func tipKinds(a Advice) []string {
	var out []string
	for _, t := range a.Tips {
		out = append(out, t.Kind+":"+t.Title)
	}
	return out
}

func TestAdviceRanking(t *testing.T) {
	a := Advise(adviceHome(), today)
	want := []string{
		"eat_first:酸奶", "eat_first:纯牛奶", // today, then tomorrow
		"cook:煎蛋", "cook:番茄炒蛋", "cook:牛奶粥", // the first three meal ideas
		"chore:拖地", "chore:倒垃圾", // most overdue first
		"restock:老豆腐", "restock:盐", "restock:纸尿裤", // not on the list, in severity order
		"restock:大米",                 // on the list, last of the restock
		"shopping:卷纸", "shopping:酱油", // the rest of the list, in its order; the closed entry is left out
	}
	if got := tipKinds(a); !reflect.DeepEqual(got, want) {
		t.Fatalf("advice\n got %v\nwant %v", got, want)
	}
	for i, tip := range a.Tips {
		if tip.Rank != i+1 || tip.Reason == "" {
			t.Fatalf("tip %d %+v", i, tip)
		}
	}
	byTitle := map[string]Tip{}
	for _, tip := range a.Tips {
		byTitle[tip.Kind+tip.Title] = tip
	}
	if r := byTitle["restock盐"].Reason; !strings.Contains(r, "约剩 1袋") || !strings.Contains(r, "低库存线 1袋") {
		t.Fatalf("salt reason %q", r)
	}
	if tip := byTitle["restock纸尿裤"]; tip.Days != 4 || !strings.Contains(tip.Reason, "每天 5片") {
		t.Fatalf("diaper %+v", tip)
	}
	if tip := byTitle["restock大米"]; !tip.OnList || tip.BuyID != "b1" || !strings.Contains(tip.Reason, "已经在采购清单上") {
		t.Fatalf("rice %+v", tip)
	}
	if tip := byTitle["shopping卷纸"]; !tip.OnList || tip.BuyID != "b2" || tip.ItemID != "" || tip.Reason != "要买 2提，在采购清单上，还没买" {
		t.Fatalf("shopping %+v", tip)
	}
	if tip := byTitle["shopping酱油"]; tip.BuyID != "b4" || tip.Reason != "在采购清单上，还没买" {
		t.Fatalf("shopping %+v", tip)
	}
	if tip := byTitle["cook煎蛋"]; tip.RecipeID != "r_milkegg" || !strings.Contains(tip.Reason, "用得上快过期的纯牛奶") {
		t.Fatalf("cook %+v", tip)
	}
	if tip := byTitle["chore拖地"]; tip.ChoreID != "c1" || tip.Reason != "已经逾期 3 天" {
		t.Fatalf("chore %+v", tip)
	}
}

func TestAdviceIsDeterministic(t *testing.T) {
	first := Advise(adviceHome(), today)
	for range 20 {
		if again := Advise(adviceHome(), today); !reflect.DeepEqual(first, again) {
			t.Fatal("same records and day gave other advice")
		}
	}
	if other := Advise(adviceHome(), today.Add(1)); reflect.DeepEqual(tipKinds(first), tipKinds(other)) {
		t.Fatal("the advice does not move with the day")
	}
}

func TestAdviceAgreesWithSummaryAndMeals(t *testing.T) {
	h := adviceHome()
	a := Advise(h, today)
	sum := Summarize(h, today)
	count := map[string]int{}
	for _, tip := range a.Tips {
		count[tip.Kind]++
	}
	if count[AdviceEatFirst] != sum.Expiring || count[AdviceChore] != sum.ChoresDue ||
		count[AdviceRestock] != sum.Expired+sum.Out+sum.Low+sum.Runout || count[AdviceRestock]+count[AdviceChore] != sum.Alerts {
		t.Fatalf("counts %v, summary %+v", count, sum)
	}
	// Every open purchase is named: as a restock tip on the list, or as a shopping tip.
	listed := count[AdviceShopping]
	for _, tip := range a.Tips {
		if tip.Kind == AdviceRestock && tip.OnList {
			listed++
		}
	}
	if listed != sum.OpenBuys {
		t.Fatalf("%d open purchases named, 待买 is %d", listed, sum.OpenBuys)
	}
	var cook []string
	for _, tip := range a.Tips {
		if tip.Kind == AdviceCook {
			cook = append(cook, tip.Title)
		}
	}
	if want := ideaTitles(MealIdeas(h, today, AdviceMeals)); !reflect.DeepEqual(cook, want) {
		t.Fatalf("cook %v, meal ideas %v", cook, want)
	}
	if !reflect.DeepEqual(a.Summary, sum) {
		t.Fatal("summary differs")
	}
}

func TestAdviceOnAnEmptyHome(t *testing.T) {
	if a := Advise(&model.Home{}, today); len(a.Tips) != 0 {
		t.Fatalf("advice on nothing: %+v", a.Tips)
	}
}

func TestAdviceNamesTheShoppingListWhenNothingElseIsDue(t *testing.T) {
	h := &model.Home{Buys: []model.Buy{
		{ID: "b1", Title: "牛奶", Status: model.BuyTodo},
		{ID: "b2", Title: "鸡蛋", Status: model.BuyTodo},
		{ID: "b3", Title: "面包", Status: model.BuySkip},
	}}
	a := Advise(h, today)
	if got, want := tipKinds(a), []string{"shopping:牛奶", "shopping:鸡蛋"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("advice %v, want %v", got, want)
	}
	if a.Summary.OpenBuys != 2 || a.Summary.Alerts != 0 {
		t.Fatalf("summary %+v", a.Summary)
	}
}
