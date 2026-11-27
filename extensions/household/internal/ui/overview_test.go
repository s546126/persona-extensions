package ui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/household/internal/model"
	"github.com/nwverse/persona-extensions/extensions/household/internal/ops"
	"github.com/nwverse/persona-extensions/extensions/household/internal/rules"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/store"
)

func TestOverviewWithNoRecordsOffersSamplesAndAdd(t *testing.T) {
	a, _ := newApp(newStore(t, false))
	v := render(t, a, state{})
	checkView(t, "empty overview", v, false)
	items := v.Sections[0].Items
	if len(items) != 1 || items[0].Type != wire.ComponentEmpty {
		t.Fatalf("empty overview first section %+v, want one empty component", items)
	}
	empty := items[0]
	if empty.Button == nil || empty.Button.Title != "载入示例" {
		t.Fatalf("empty button %+v, want 载入示例", empty.Button)
	}
	if empty.Button.Action.ID != "load" || empty.Button.Action.Confirm == "" {
		t.Fatalf("载入示例 action %+v, want load with a confirm", empty.Button.Action)
	}
	add, ok := buttonTitled(v, "添加物品")
	if !ok {
		t.Fatal("no 添加物品 button on the empty overview")
	}
	if add.Action == nil || add.Action.Push == nil || add.Action.Push.State == "" {
		t.Fatalf("添加物品 is not a push to the form: %+v", add.Action)
	}
	if hasAction(v, "restock") {
		t.Fatal("empty overview offers 一键补货")
	}
}

func TestOverviewWithSamplesShowsEverySection(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	v := render(t, a, state{})
	checkView(t, "sample overview", v, false)

	for _, title := range []string{"需要补货", "先用掉", "到期家务", "今天吃什么"} {
		if _, ok := sectionTitled(v, title); !ok {
			t.Fatalf("no section %q", title)
		}
	}
	stats := overviewGrid(t, v)
	if stats.Type != wire.ComponentStatGrid || len(stats.Stats) != 4 {
		t.Fatalf("first component %+v, want a stat grid of four", stats)
	}
	wantLabels := []string{"需要处理", "待买", "家务到期", "即将过期"}
	for i, s := range stats.Stats {
		if s.Label != wantLabels[i] {
			t.Fatalf("stat %d label %q, want %q", i, s.Label, wantLabels[i])
		}
	}
}

func TestOverviewStatsMatchTheRules(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	h := home(t, st)
	sum := rules.Summarize(h, testToday)
	stats := overviewGrid(t, render(t, a, state{})).Stats
	got := map[string]string{}
	for _, s := range stats {
		got[s.Label] = s.Value
	}
	if got["需要处理"] != strconv.Itoa(sum.Alerts) || got["待买"] != strconv.Itoa(sum.OpenBuys) ||
		got["家务到期"] != strconv.Itoa(sum.ChoresDue) || got["即将过期"] != strconv.Itoa(sum.Expiring) {
		t.Fatalf("stats %v do not match summary %+v", got, sum)
	}
}

func TestRestockSectionRowsAreTheBuyableFindings(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	h := home(t, st)
	v := render(t, a, state{})
	sec, ok := sectionTitled(v, "需要补货")
	if !ok {
		t.Fatal("no 需要补货 section")
	}
	var rows []wire.Component
	for _, it := range sec.Items {
		if it.Type == wire.ComponentRow {
			rows = append(rows, it)
		}
	}
	want := 0
	for _, f := range rules.Findings(h, testToday) {
		if f.Kind.Buyable() {
			want++
		}
	}
	if len(rows) != want || want == 0 {
		t.Fatalf("%d rows for 需要补货, want %d (> 0)", len(rows), want)
	}
	for _, r := range rows {
		if r.Badge == nil || r.Badge.Text == "" {
			t.Fatalf("row %q has no kind badge", r.Title)
		}
	}
}

func TestRestockButtonShowsItsCountAndIsHiddenAtZero(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	h := home(t, st)
	want := len(rules.Restock(h, testToday))
	if want == 0 {
		t.Fatal("samples need no restock: the test has nothing to count")
	}
	b, ok := buttonByAction(render(t, a, state{}), "restock")
	if !ok {
		t.Fatal("no 一键补货 button")
	}
	if !strings.Contains(b.Title, strconv.Itoa(want)) {
		t.Fatalf("button %q does not show the %d items it will create", b.Title, want)
	}

	act(t, a, state{}, nil, "restock")
	if hasAction(render(t, a, state{}), "restock") {
		t.Fatal("一键补货 is still offered after every need has a purchase")
	}
}

func TestExpiringSectionUsesDaysUntilExpiry(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	sec, ok := sectionTitled(render(t, a, state{}), "先用掉")
	if !ok {
		t.Fatal("no 先用掉 section")
	}
	for _, it := range sec.Items {
		if it.Type != wire.ComponentRow {
			continue
		}
		if it.Badge == nil || it.Badge.Text != "即将过期" || it.Badge.Tone != wire.ToneCaution {
			t.Fatalf("expiring row %q badge %+v, want 即将过期 caution", it.Title, it.Badge)
		}
		if !strings.Contains(it.Subtitle, "过期") {
			t.Fatalf("expiring row %q subtitle %q does not say when", it.Title, it.Subtitle)
		}
	}
}

func TestChoreRowsCompleteFromTheRowAndOpenTheForm(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	sec, ok := sectionTitled(render(t, a, state{}), "到期家务")
	if !ok {
		t.Fatal("no 到期家务 section")
	}
	rows := 0
	for _, it := range sec.Items {
		if it.Type != wire.ComponentRow {
			continue
		}
		rows++
		if len(it.Actions) != 1 || it.Actions[0].Title != "完成" || !strings.HasPrefix(it.Actions[0].Action.ID, "chore:") {
			t.Fatalf("chore row %q actions %+v, want 完成 as chore:<id>", it.Title, it.Actions)
		}
		if it.Action == nil || it.Action.Push == nil || it.Action.Push.UI != PageUI {
			t.Fatalf("chore row %q does not open its form: %+v", it.Title, it.Action)
		}
	}
	if rows == 0 {
		t.Fatal("no chore due in the samples")
	}
}

func TestChoreRowBadgeSaysOverdueDays(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	sec, _ := sectionTitled(render(t, a, state{}), "到期家务")
	seenOverdue := false
	for _, it := range sec.Items {
		if it.Type != wire.ComponentRow {
			continue
		}
		if it.Badge == nil {
			t.Fatalf("chore row %q has no badge", it.Title)
		}
		if strings.HasPrefix(it.Badge.Text, "逾期") {
			seenOverdue = true
			if it.Badge.Tone != wire.ToneDanger {
				t.Fatalf("overdue chore tone %q, want danger", it.Badge.Tone)
			}
		}
	}
	if !seenOverdue {
		t.Fatal("samples have an overdue chore; no row says 逾期")
	}
}

func TestMealIdeasAreTopThreeWithPlanActions(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	h := home(t, st)
	ideas := rules.MealIdeas(h, testToday, 3)
	sec, ok := sectionTitled(render(t, a, state{}), "今天吃什么")
	if !ok {
		t.Fatal("no 今天吃什么 section")
	}
	var rows []wire.Component
	for _, it := range sec.Items {
		if it.Type == wire.ComponentRow {
			rows = append(rows, it)
		}
	}
	if len(rows) != len(ideas) {
		t.Fatalf("%d meal rows, want %d", len(rows), len(ideas))
	}
	for i, r := range rows {
		if r.Title != ideas[i].Recipe.Title {
			t.Fatalf("row %d title %q, want %q", i, r.Title, ideas[i].Recipe.Title)
		}
		if len(r.Actions) != 1 || r.Actions[0].Title != "排到今晚" {
			t.Fatalf("row %q actions %+v, want 排到今晚", r.Title, r.Actions)
		}
		if want := "plan:" + ideas[i].Recipe.ID; r.Actions[0].Action.ID != want {
			t.Fatalf("row %q plan action %q, want %q", r.Title, r.Actions[0].Action.ID, want)
		}
	}
}

func TestMealSectionSaysWhatIsMissingOrToUseFirst(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	h := home(t, st)
	ideas := rules.MealIdeas(h, testToday, 3)
	sec, _ := sectionTitled(render(t, a, state{}), "今天吃什么")
	for i, it := range sec.Items {
		if it.Type != wire.ComponentRow || i >= len(ideas) {
			continue
		}
		if len(ideas[i].Missing) > 0 && !strings.Contains(it.Subtitle, "缺：") {
			t.Fatalf("row %q subtitle %q does not list what is missing", it.Title, it.Subtitle)
		}
		if len(ideas[i].UseFirst) > 0 && !strings.Contains(it.Subtitle, "先用：") {
			t.Fatalf("row %q subtitle %q does not list what to use first", it.Title, it.Subtitle)
		}
	}
}

func TestOverviewAlwaysOffersAddItem(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	v := render(t, a, state{})
	if _, ok := buttonTitled(v, "添加物品"); !ok {
		t.Fatal("no 添加物品 on a populated overview")
	}
	checkView(t, "populated overview", v, false)
}

func TestEmptyBlocksFoldIntoOneLine(t *testing.T) {
	st := newStore(t, false)
	a, _ := newApp(st)
	// One record that needs nothing: a fine item with no quantity, no expiry and no threshold.
	act(t, a, state{F: "new:item"}, map[string]string{"title": "牙刷", "p_category": "=日用"}, "save")
	v := render(t, a, state{})
	checkView(t, "quiet overview", v, false)
	for _, title := range []string{"需要补货", "先用掉", "到期家务"} {
		if sec, ok := sectionTitled(v, title); ok {
			t.Fatalf("empty %s shown: %+v", title, sec)
		}
	}
	// Nothing at all: the advice says so once, and the meal block keeps its next step.
	advice, _ := sectionTitled(v, "今天的建议")
	if len(advice.Items) != 1 || advice.Items[0].Text != nothingToday || advice.Footer != "" {
		t.Fatalf("advice %+v", advice)
	}
	if meals, ok := sectionTitled(v, "今天吃什么"); !ok || !strings.Contains(meals.Items[0].Text, "「餐食」") {
		t.Fatalf("meal block %+v", meals)
	}

	// A chore due: its block shows, the two empty ones are one line under the advice.
	act(t, a, state{F: "new:chore"}, map[string]string{"title": "倒垃圾"}, "save")
	v = render(t, a, state{})
	checkView(t, "overview with one chore", v, false)
	if sec, ok := sectionTitled(v, "到期家务"); !ok || sec.Items[0].Type != wire.ComponentRow {
		t.Fatalf("chore block %+v", sec)
	}
	for _, title := range []string{"需要补货", "先用掉"} {
		if _, ok := sectionTitled(v, title); ok {
			t.Fatalf("empty %s shown", title)
		}
	}
	advice, _ = sectionTitled(v, "今天的建议")
	if advice.Footer != "库存里没有缺的，没有快过期的。" {
		t.Fatalf("folded line %q", advice.Footer)
	}
	var titles []string
	for _, s := range v.Sections {
		titles = append(titles, s.Title)
	}
	want := []string{"", "今天的建议", "到期家务", "今天吃什么", addTitle}
	if fmt.Sprint(titles) != fmt.Sprint(want) {
		t.Fatalf("sections %q, want %q", titles, want)
	}
}

func TestEveryWayToAddHasItsSentenceAndButton(t *testing.T) {
	for _, samples := range []bool{false, true} {
		a, _ := newApp(newStore(t, samples))
		v := render(t, a, state{})
		sec, ok := sectionTitled(v, addTitle)
		if !ok || v.Sections[len(v.Sections)-1].Title != addTitle {
			t.Fatalf("samples %v: 记新东西 is not the last section", samples)
		}
		var got []string
		for _, c := range sec.Items {
			switch c.Type {
			case wire.ComponentRow:
				if c.Subtitle == "" || c.Action != nil {
					t.Fatalf("way %+v: wants its sentence and no tap", c)
				}
				got = append(got, c.Title)
			case wire.ComponentButton:
				got = append(got, "["+c.Title+"]")
			}
		}
		want := []string{"记一样", "[添加物品]", "自己粘贴一份清单", "[粘贴清单]", "让助手帮我记"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("samples %v: ways %q, want %q", samples, got, want)
		}
		if !strings.Contains(sec.Items[2].Subtitle, "不经过助手") || !strings.Contains(sec.Items[4].Subtitle, "发给助手") {
			t.Fatalf("paste and assistant are not told apart: %+v", sec.Items)
		}
		// No section holds a lone button.
		for _, s := range v.Sections {
			if len(s.Items) == 1 && s.Items[0].Type == wire.ComponentButton {
				t.Fatalf("samples %v: lone button %q", samples, s.Items[0].Title)
			}
		}
	}
}

// buttonTitled is the first button with the title, anywhere on the view.
func buttonTitled(v *wire.View, title string) (wire.Component, bool) {
	for _, c := range components(v) {
		if c.Type == wire.ComponentButton && c.Title == title {
			return c, true
		}
	}
	return wire.Component{}, false
}

// buttonByAction is the first button whose action has the id.
func buttonByAction(v *wire.View, id string) (wire.Component, bool) {
	for _, c := range components(v) {
		if c.Type == wire.ComponentButton && c.Action != nil && c.Action.ID == id {
			return c, true
		}
	}
	return wire.Component{}, false
}

// overviewGrid is the stat grid of the overview's first section.
func overviewGrid(t *testing.T, v *wire.View) wire.Component {
	t.Helper()
	for _, c := range v.Sections[0].Items {
		if c.Type == wire.ComponentStatGrid {
			return c
		}
	}
	t.Fatalf("no stat grid in the first section: %+v", v.Sections[0].Items)
	return wire.Component{}
}

// TestOverviewWithRecordsSaysWhatItIsForInOneLine keeps a short intro on the overview with records: one secondary
// line, then the numbers, so the numbers stay at the top of a phone screen.
func TestOverviewWithRecordsSaysWhatItIsForInOneLine(t *testing.T) {
	a, _ := newApp(newStore(t, true))
	v := render(t, a, state{})
	items := v.Sections[0].Items
	if len(items) != 2 || items[0].Type != wire.ComponentText || items[1].Type != wire.ComponentStatGrid {
		t.Fatalf("first section %+v, want the intro line then the stat grid", items)
	}
	intro := items[0]
	if intro.Text != overviewIntro || intro.Style != wire.TextStyleSecondary {
		t.Fatalf("intro %+v, want %q in the secondary style", intro, overviewIntro)
	}
	if n := utf8.RuneCountInString(intro.Text); n > 26 || strings.Contains(intro.Text, "\n") {
		t.Fatalf("intro %q is %d characters, want one short line", intro.Text, n)
	}
	for _, w := range []string{"有什么", "买", "家务", "提醒"} {
		if !strings.Contains(intro.Text, w) {
			t.Errorf("intro %q does not mention %q", intro.Text, w)
		}
	}
}

// TestOverviewNumbersSayWhatTheyCount checks 待买 and 需要处理 explain themselves: 待买 is the open shopping list,
// 需要处理 is the items to restock plus the chores due (the two parts in its detail add up to it), and the caption
// under the grid tells the two apart.
func TestOverviewNumbersSayWhatTheyCount(t *testing.T) {
	st := newStore(t, true)
	a, _ := newApp(st)
	v := render(t, a, state{})
	sum := rules.Summarize(home(t, st), testToday)
	stats := map[string]wire.Stat{}
	for _, s := range overviewGrid(t, v).Stats {
		stats[s.Label] = s
	}
	buy := sum.Expired + sum.Out + sum.Low + sum.Runout
	if buy+sum.ChoresDue != sum.Alerts {
		t.Fatalf("restock %d + chores %d != alerts %d", buy, sum.ChoresDue, sum.Alerts)
	}
	want := fmt.Sprintf("补货 %d · 家务 %d", buy, sum.ChoresDue)
	if got := stats["需要处理"].Detail; got != want {
		t.Fatalf("需要处理 detail %q, want %q", got, want)
	}
	if got := stats["待买"].Detail; !strings.Contains(got, "采购清单") {
		t.Fatalf("待买 detail %q, want it to name the shopping list", got)
	}
	if f := v.Sections[0].Footer; f != statsCaption || !strings.Contains(f, "待买") || !strings.Contains(f, "需要处理") {
		t.Fatalf("stat caption %q, want %q", f, statsCaption)
	}
}

// TestShoppingListAloneIsNotSomethingToHandle is the case a first-time user asks about: entries on the shopping list
// and nothing low, expiring or due. 待买 counts them, 需要处理 stays 0 and its detail says why.
func TestShoppingListAloneIsNotSomethingToHandle(t *testing.T) {
	st := store.Open("")
	err := st.Update(func(b *kernel.Base, _ time.Time) error {
		for _, n := range []string{"洗衣液", "抽纸", "鸡蛋", "酱油"} {
			if _, err := ops.Save(b, kernel.EntityInput{Type: model.TBuy, Title: &n}, testNow, testLoc()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := newApp(st)
	v := render(t, a, state{})
	checkView(t, "shopping-only overview", v, false)
	got := map[string]wire.Stat{}
	for _, s := range overviewGrid(t, v).Stats {
		got[s.Label] = s
	}
	if got["待买"].Value != "4" || got["需要处理"].Value != "0" || got["需要处理"].Detail != "补货 0 · 家务 0" {
		t.Fatalf("stats %+v, want 待买 4 and 需要处理 0 (补货 0 · 家务 0)", got)
	}
}
