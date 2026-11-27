package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
	"github.com/nwverse/persona-extensions/extensions/household/internal/ops"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
)

// firstEmpty is the view's first empty state.
func firstEmpty(t *testing.T, v *wire.View) wire.Component {
	t.Helper()
	for _, c := range components(v) {
		if c.Type == wire.ComponentEmpty {
			return c
		}
	}
	t.Fatalf("no empty state in %+v", v.Sections)
	return wire.Component{}
}

func TestEmptyCardSaysWhatItIsForAndTheFirstStep(t *testing.T) {
	a, _ := newApp(newStore(t, false))
	e := firstEmpty(t, renderCard(t, a))
	if e.Title != welcomeTitle {
		t.Errorf("card title %q", e.Title)
	}
	// what it keeps, where to tap (the card opens the page) and the assistant as the other way in
	for _, w := range []string{"家里有什么", "该买什么", "家务", "点开这里添加第一件物品", "跟助手说"} {
		if !strings.Contains(e.Detail, w) {
			t.Errorf("card detail has no %q: %s", w, e.Detail)
		}
	}
}

func TestEmptyOverviewSaysWhatItIsForAndOffersTheFirstSteps(t *testing.T) {
	a, _ := newApp(newStore(t, false))
	v := render(t, a, state{})
	checkView(t, "empty overview", v, false)
	e := firstEmpty(t, v)
	if e.Title != welcomeTitle || e.Button == nil || e.Button.Action.ID != "load" {
		t.Fatalf("empty state %+v", e)
	}
	for _, w := range []string{"家里有什么", "快过期", "家务", "吃什么", "「添加物品」", "示例", "跟助手说"} {
		if !strings.Contains(e.Detail, w) {
			t.Errorf("overview detail has no %q: %s", w, e.Detail)
		}
	}
	// the button the words name is on the page
	add := false
	for _, c := range components(v) {
		add = add || (c.Type == wire.ComponentButton && c.Title == "添加物品")
	}
	if !add {
		t.Fatal("no 添加物品 button")
	}
}

func TestOverviewMealHintNamesTheTabWithTheButton(t *testing.T) {
	st := newStore(t, false)
	if err := st.Update(func(b *kernel.Base, _ time.Time) error {
		title := "牛奶"
		_, err := ops.Save(b, kernel.EntityInput{Type: model.TItem, Title: &title}, testNow, testLoc())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	a, _ := newApp(st)
	sec, ok := sectionTitled(render(t, a, state{}), "今天吃什么")
	if !ok || len(sec.Items) != 1 || !strings.Contains(sec.Items[0].Text, "「餐食」") {
		t.Fatalf("meal section %+v", sec)
	}
	// the 餐食 tab has the button
	if !hasAction(render(t, a, state{T: mealTab}), "add:"+model.TRecipe) {
		t.Fatalf("the 餐食 tab has no add-recipe action: %v", actionIDs(render(t, a, state{T: mealTab})))
	}
}
