package mcp

import (
	"reflect"
	"strings"
	"testing"
)

func TestAdviseAgreesWithStatusAndMealSuggest(t *testing.T) {
	s := start(t, seeded(t, true))
	defer s.finish()
	adv := s.done("household_advise", `{}`)
	status := s.done("household_status", `{}`)
	if !reflect.DeepEqual(get(t, adv, "summary"), get(t, status, "summary")) {
		t.Fatalf("advice summary %v, status %v", get(t, adv, "summary"), get(t, status, "summary"))
	}
	var cook []string
	count := map[string]int{}
	for _, tip := range get(t, adv, "tips").([]any) {
		k := asStr(t, get(t, tip, "kind"))
		count[k]++
		if k == "cook" {
			cook = append(cook, asStr(t, get(t, tip, "title")))
		}
	}
	var ideas []string
	for _, i := range get(t, s.done("household_meal_suggest", `{"limit":3}`), "ideas").([]any) {
		ideas = append(ideas, asStr(t, get(t, i, "recipe", "title")))
	}
	if !reflect.DeepEqual(cook, ideas) {
		t.Fatalf("cook %v, meal_suggest %v", cook, ideas)
	}
	sum := asMap(t, get(t, status, "summary"))
	if float64(count["restock"]+count["chore"]) != sum["alerts"] || float64(count["eat_first"]) != sum["expiring"] {
		t.Fatalf("counts %v, summary %v", count, sum)
	}
	says := get(t, adv, "says").([]any)
	if len(says) != len(get(t, adv, "tips").([]any)) || !strings.HasPrefix(asStr(t, says[0]), "1. 先吃「生菜」") {
		t.Fatalf("says %v", says)
	}
	// Same records, same advice; reading changes nothing.
	if again := s.done("household_advise", `{}`); !reflect.DeepEqual(adv, again) {
		t.Fatal("advice changed between two calls")
	}
	if msg := s.failed("household_advise", `{"x":1}`); msg == "" {
		t.Fatal("unknown argument accepted")
	}
}

func TestAdviseOnEmptyStore(t *testing.T) {
	s := start(t, seeded(t, false))
	defer s.finish()
	adv := s.done("household_advise", `{}`)
	if len(get(t, adv, "tips").([]any)) != 0 || get(t, adv, "next") == "" {
		t.Fatalf("empty advice %v", adv)
	}
}

func TestAdviseNamesTheOpenShoppingList(t *testing.T) {
	s := start(t, seeded(t, false))
	defer s.finish()
	for _, title := range []string{"牛奶", "鸡蛋"} {
		s.done("household_save", `{"type":"buy","title":"`+title+`"}`)
	}
	adv := s.done("household_advise", `{}`)
	tips := get(t, adv, "tips").([]any)
	if len(tips) != 2 || asStr(t, get(t, tips[0], "kind")) != "shopping" || asStr(t, get(t, tips[0], "buy_id")) == "" {
		t.Fatalf("tips %v", tips)
	}
	says := get(t, adv, "says").([]any)
	if asStr(t, says[0]) != "1. 待买「牛奶」：在采购清单上，还没买" {
		t.Fatalf("says %v", says)
	}
	if asMap(t, get(t, adv, "summary"))["open_buys"] != float64(2) {
		t.Fatalf("summary %v", get(t, adv, "summary"))
	}
}
