package main

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// clockRE matches a time of day such as 08:00.
var clockRE = regexp.MustCompile(`\d{1,2}:\d{2}`)

func sectionByKey(v *wire.View, key string) *wire.Section {
	for i := range v.Sections {
		if v.Sections[i].Key == key {
			return &v.Sections[i]
		}
	}
	return nil
}

func statDetail(v *wire.View, label string) string {
	for _, s := range v.Sections {
		for _, c := range s.Items {
			for _, st := range c.Stats {
				if st.Label == label {
					return st.Detail
				}
			}
		}
	}
	return ""
}

// The newest check of a computer, whichever page ran it, says whether it can be reached: both
// pages then count and label it the same way and name their button the same way (重试 after a
// check that could not reach it, 重新检查 otherwise). The AI 对话 row still says what its own
// older check found.
func TestNewestCheckDecidesOnBothPages(t *testing.T) {
	a := dumpFleet(t) // studio-mac: drive without a repository 3 days ago, unreachable now
	sess := sessView(t, a, "")
	txt := visibleText(sess)
	if !strings.Contains(txt, "2 台连不上这台电脑") || strings.Contains(txt, "1 台"+noRepoText) {
		t.Errorf("AI 对话 counts:\n%s", txt)
	}
	if d := statDetail(diskView(t, a, "local", tabFleet), "硬盘用了多少"); !strings.Contains(d, "2 台连不上") {
		t.Errorf("硬盘空间 counts: %q", d)
	}
	row := itemsOf(sess, "studio-mac")
	if row[0].Badge == nil || row[0].Badge.Text != offlineText {
		t.Errorf("studio-mac badge: %+v", row[0].Badge)
	}
	lead := componentsText(row)
	if !strings.Contains(lead, "连不上「studio-mac」") || !strings.Contains(lead, "上次连得上时（3 天前检查）发现："+noRepoText) {
		t.Errorf("studio-mac lead:\n%s", lead)
	}
	if strings.Count(lead, "SSH 连接") > 0 {
		t.Errorf("the failed disk check is said twice:\n%s", lead)
	}
	for page, items := range map[string][]wire.Component{
		"AI 对话":   row,
		"AI 对话 1": itemsOf(sessView(t, a, "studio-mac"), "studio-mac"),
		"硬盘空间":    diskItemsOf(diskView(t, a, "studio-mac", tabFleet), "studio-mac", false),
	} {
		if got := checkButtons(items, "studio-mac"); !slices.Equal(got, []string{"重试 studio-mac"}) {
			t.Errorf("%s: buttons %v", page, got)
		}
	}
	if strings.Contains(componentsText(row), "把一周没动的 AI 对话搬到网盘") {
		t.Error("an unreachable computer offers the upload")
	}
	fb := sessionsFallbackSummary(t, a)
	if !strings.Contains(fb, "studio-mac："+offlineText) {
		t.Errorf("fallback: %q", fb)
	}

	// The other way round: AI 对话 could not reach offline-box, a later 硬盘空间 check did.
	if err := a.db().update(func(st *State, now time.Time) error {
		m := st.machine("offline-box")
		m.LastError, m.Reachable = "", true
		st.Scans["offline-box"].ScannedAt = now.Add(-time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sess = sessView(t, a, "")
	row = itemsOf(sess, "offline-box")
	if row[0].Badge == nil || row[0].Badge.Text != recheckText || strings.Contains(componentsText(row), offlineText+"。") {
		t.Errorf("offline-box after a later disk check: %+v\n%s", row[0].Badge, componentsText(row))
	}
	if !strings.Contains(componentsText(row), "后来「硬盘空间」页检查（1 小时前）时连上了它") {
		t.Errorf("offline-box lead:\n%s", componentsText(row))
	}
	if !strings.Contains(visibleText(sess), "1 台连不上这台电脑") {
		t.Errorf("AI 对话 counts after:\n%s", visibleText(sess))
	}
	for page, items := range map[string][]wire.Component{
		"AI 对话": row,
		"硬盘空间":  diskItemsOf(diskView(t, a, "offline-box", tabFleet), "offline-box", false),
	} {
		if got := checkButtons(items, "offline-box"); !slices.Equal(got, []string{"重新检查 offline-box"}) {
			t.Errorf("%s: buttons %v", page, got)
		}
	}
}

func sessionsFallbackSummary(t *testing.T, a *app) string {
	t.Helper()
	var out string
	if err := a.db().read(func(st *State) { out = a.drawer(st, nil).sessionsFallbackSummary(nil, "") }); err != nil {
		t.Fatal(err)
	}
	return out
}

// A computer that answers but whose tool could not be read is checked again with 重新检查, as on
// 硬盘空间; MCP keeps its state.
func TestUnreadableToolIsCheckedAgain(t *testing.T) {
	a := mixedFleet(t)
	if err := a.db().update(func(st *State, now time.Time) error {
		c := *st.cold("nas-box")
		c.State, c.Error = ColdUnreadable, "disklessly 回答看不懂"
		st.setCold("nas-box", &c)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	items := itemsOf(sessView(t, a, "nas-box"), "nas-box")
	if got := checkButtons(items, "nas-box"); !slices.Equal(got, []string{"重新检查 nas-box"}) {
		t.Errorf("buttons %v", got)
	}
	if txt := componentsText(items); strings.Contains(txt, "重试") || !strings.Contains(txt, "点「重新检查 nas-box」") {
		t.Errorf("text:\n%s", txt)
	}
}

// A network drive problem says how many of the computer's conversations the top of the page
// counts as 还没核对过 because of it, or that none is; never that the page shows what it does not.
func TestDriveProblemSaysHowManyAreUnchecked(t *testing.T) {
	a := mixedFleet(t) // studio-mac: drive without a repository, no conversation moved
	sv := sessView(t, a, "studio-mac")
	txt := componentsText(itemsOf(sv, "studio-mac"))
	if !strings.Contains(txt, "不过这台电脑还没有搬到网盘的对话，所以没有对话受影响。") || strings.Contains(txt, "暂时显示") {
		t.Errorf("no conversations:\n%s", txt)
	}
	// The advice block names the state as the row's badge does, not as MCP's state_label.
	if adv := sectionByKey(sv, "sessions-advice"); adv == nil || !strings.Contains(componentsText(adv.Items), "现在还搬不了："+noRepoText+"。") {
		t.Errorf("advice block: %+v", adv)
	}
	if err := a.db().update(func(st *State, now time.Time) error {
		c := *st.cold("studio-mac")
		c.Entries = slices.Clone(st.cold("local").Entries[:2])
		for i := range c.Entries {
			c.Entries[i].State, c.Entries[i].Segment = "archived", "seg-a"
		}
		st.setCold("studio-mac", &c)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	v := sessView(t, a, "studio-mac")
	txt = componentsText(itemsOf(v, "studio-mac"))
	if !strings.Contains(txt, "所以它搬到网盘的 2 个对话暂时算在「还没核对过」里，不会当成丢了。") {
		t.Errorf("two conversations:\n%s", txt)
	}
	if !strings.Contains(visibleText(v), uncheckedText+"\n2 个") {
		t.Errorf("the count does not show them:\n%s", visibleText(v))
	}
	// MCP keeps its sentence.
	var reason string
	if err := a.db().read(func(st *State) {
		reason = coldProblem(st, st.machine("studio-mac"), st.cold("studio-mac"), ColdBackendDown).Reason
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, noRepoUnknownClause) {
		t.Errorf("MCP reason changed: %q", reason)
	}
}

// One way to write a time on every page: 「3 小时前」「3 天前」 within a week, then 「11月14日」
// (with the year only when it is another year), written against the word after it.
func TestOneDateStyle(t *testing.T) {
	d := draw{now: t0, loc: time.UTC}
	for _, c := range []struct {
		at        time.Time
		when, stp string
	}{
		{t0.Add(-3 * time.Hour), "3 小时前", "3 小时前"},
		{t0.Add(-3 * 24 * time.Hour), "3 天前", "3 天前 10:00"},
		{t0.Add(-10 * 24 * time.Hour), "10月30日", "10月30日 10:00"},
		{time.Date(2025, 12, 31, 8, 5, 0, 0, time.UTC), "2025年12月31日", "2025年12月31日 08:05"},
	} {
		if got := d.when(c.at); got != c.when {
			t.Errorf("when %v: %q, want %q", c.at, got, c.when)
		}
		if got := d.stamp(c.at); got != c.stp {
			t.Errorf("stamp %v: %q, want %q", c.at, got, c.stp)
		}
	}
	a := dumpFleet(t)
	iso := regexp.MustCompile(`\d{4}-\d{2}-\d{2}|\d{2}-\d{2} \d{2}:\d{2}`)
	views := allViews(t, a)
	views["清理记录"] = diskView(t, a, "local", tabHistory)
	for name, v := range views {
		txt := visibleText(v)
		// File paths in confirms (rollout-2026-10-01T….jsonl) are not dates the page writes.
		paths := regexp.MustCompile(`\S*/\S*`)
		if m := iso.FindString(paths.ReplaceAllString(txt, "")); m != "" {
			t.Errorf("%s: a date written %q", name, m)
		}
		if strings.Contains(txt, "日 检查") || strings.Contains(txt, "前 检查") {
			t.Errorf("%s: a space before 检查:\n%s", name, txt)
		}
	}
	row := diskItemsOf(diskView(t, a, "offline-box", tabFleet), "offline-box", false)[0]
	if !strings.HasPrefix(row.Subtitle, "10月30日检查时连不上这台电脑 · 下面是 10月28日检查时的数") {
		t.Errorf("offline-box: %q", row.Subtitle)
	}
}

// 帮我腾空间 numbers its steps without a check mark (which reads as done) and opens a read-only
// page with every step; an app that does not push pages draws it in place with a way back.
func TestAdviceListsEveryStepOnAPage(t *testing.T) {
	noSandbox(t)
	a := adviceApp(t)
	ctx := context.Background()
	v, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8}})
	if err != nil {
		t.Fatal(err)
	}
	sec := sectionByKey(v, "advice")
	var all *wire.Component
	for i, c := range sec.Items {
		if c.Icon == "check" {
			t.Errorf("a check mark on %q", c.Title)
		}
		if c.Key == "steps-all" {
			all = &sec.Items[i]
		}
	}
	if all == nil || all.Action == nil || all.Action.Push == nil || all.Action.Push.UI != "" || all.Action.Confirm != "" {
		t.Fatalf("no row opens every step: %+v", sec.Items)
	}
	if strings.Contains(sec.Footer, "助手那里能看到全部") || !strings.Contains(sec.Footer, "点「"+adviceAllRow+"」能看到") {
		t.Errorf("footer: %q", sec.Footer)
	}
	var adv *advice
	if err := a.db().read(func(st *State) { adv, _ = buildAdvice(st, a.now(), adviceOptions{}) }); err != nil {
		t.Fatal(err)
	}
	pushed, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, State: all.Action.Push.State}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, pushed, false)
	assertMobileReadability360px(t, "every step", pushed)
	checkTapNames(t, "every step", pushed)
	if pushed.Title != adviceAllTitle {
		t.Errorf("title %q", pushed.Title)
	}
	steps, later, look := sectionByKey(pushed, "advice-all-steps"), sectionByKey(pushed, "advice-all-later"), sectionByKey(pushed, "advice-all-look")
	if steps == nil || later == nil || look == nil {
		t.Fatalf("sections: %+v %+v %+v", steps, later, look)
	}
	if len(steps.Items)+len(later.Items) != len(adv.Steps) || len(look.Items) != len(adv.LookFirst) {
		t.Fatalf("every step: %d+%d of %d, look first %+v", len(steps.Items), len(later.Items), len(adv.Steps), look)
	}
	for _, c := range later.Items {
		if !strings.Contains(c.Subtitle, "现在还做不了") {
			t.Errorf("a step under %q without its reason: %q", later.Title, c.Subtitle)
		}
	}
	for _, c := range steps.Items {
		if strings.Contains(c.Subtitle, "现在还做不了") {
			t.Errorf("a step that cannot be done now under %q: %q", steps.Title, c.Title)
		}
	}
	for i, c := range slices.Concat(steps.Items, later.Items, look.Items) {
		if c.Type == wire.ComponentButton || c.Icon == "check" || c.Action == nil || c.Action.Confirm != "" ||
			strings.HasPrefix(c.Action.ID, "clean") || strings.HasPrefix(c.Action.ID, "cold-") {
			t.Errorf("row %d acts by itself or reads as done: %+v", i, c)
		}
	}
	// Numbers go on across the two headings.
	for i, c := range slices.Concat(steps.Items, later.Items) {
		if !strings.HasPrefix(c.Title, strconv.Itoa(i+1)+". ") {
			t.Errorf("step %d not numbered: %q", i+1, c.Title)
		}
	}
	// An app without pushed pages: the row's id draws the page in place, with 返回.
	res, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 2, State: v.State, Action: all.Action.ID}})
	if err != nil || res.View == nil || res.View.Title != adviceAllTitle {
		t.Fatalf("in place: %+v %v", res, err)
	}
	if b := buttonIn(res.View.Sections[0].Items, "返回"); b == nil || b.Action.ID != "cancel" {
		t.Errorf("no way back: %+v", res.View.Sections[0].Items)
	}
}

// The AI 对话 page has its own short block: the computers with week-old conversations to move
// and what that frees (from the same plan), and a sentence to find one through the assistant.
func TestSessionsPageAdviceBlock(t *testing.T) {
	a := dumpFleet(t)
	v := sessView(t, a, "")
	idx := slices.IndexFunc(v.Sections, func(s wire.Section) bool { return s.Key == "sessions-advice" })
	if idx != 1 || v.Sections[idx+1].Key != "cold-machines" {
		t.Fatalf("block at %d", idx)
	}
	sec := v.Sections[idx]
	var adv *advice
	if err := a.db().read(func(st *State) { adv, _ = buildAdvice(st, a.now(), adviceOptions{}) }); err != nil {
		t.Fatal(err)
	}
	moves := map[string]adviceStep{}
	for _, s := range adv.Steps {
		if s.Action == adviceMove {
			moves[s.MachineID] = s
		}
	}
	rows := 0
	for i, c := range sec.Items {
		if c.Type == wire.ComponentButton {
			t.Errorf("a button in the block: %+v", c)
		}
		if c.Type != wire.ComponentRow {
			continue
		}
		rows++
		if i > 0 && !strings.Contains(c.Subtitle, "现在还搬不了") && strings.Contains(sec.Items[i-1].Subtitle, "现在还搬不了") {
			t.Error("a step that can be done now comes after one that cannot")
		}
		id := strings.TrimPrefix(c.Key, "move-")
		s, ok := moves[id]
		if !ok || c.Badge == nil || c.Badge.Text != "+"+FormatBytes(s.FreesBytes) || !strings.HasPrefix(c.Title, s.MachineName+"：") {
			t.Errorf("row %+v for %+v", c, s)
		}
		if c.Action == nil || c.Action.ID != "machine" || c.Action.Values["machine"] != id {
			t.Errorf("row action %+v", c.Action)
		}
		if strings.Contains(c.Subtitle, "现在还搬不了：现在") {
			t.Errorf("doubled word: %q", c.Subtitle)
		}
	}
	if rows != min(len(moves), sessionsAdviceShown) {
		t.Errorf("%d rows for %d computers", rows, len(moves))
	}
	if last := sec.Items[len(sec.Items)-1]; !strings.Contains(last.Text, "「"+findAskText+"」") {
		t.Errorf("no sentence for the assistant: %+v", last)
	}
	// Said once: the results' footer keeps only the search box's tips.
	if n := strings.Count(visibleText(v), findAskText); n != 1 {
		t.Errorf("the assistant sentence is said %d times", n)
	}
}

// The 硬盘空间 page names the kinds by one set of short names in the chart and the table, and
// spells them out once.
func TestFleetChartAndTableShareTheirShortNames(t *testing.T) {
	a := mixedFleet(t)
	v := diskView(t, a, "local", tabFleet)
	txt := visibleText(v)
	if n := strings.Count(txt, "简称："); n != 1 {
		t.Errorf("short names spelled out %d times:\n%s", n, txt)
	}
	for _, old := range []string{"容器与镜像", "系统与日志", "旧 AI 对话"} {
		if strings.Contains(txt, old) {
			t.Errorf("still says %q", old)
		}
	}
	tbl := findComponentByKey(v, "fleet-tbl-local")
	if tbl == nil {
		t.Fatal("no table")
	}
	for i, r := range tbl.Rows {
		if r[0] != CategoryAxisLabels[CategoryIDs[i]] {
			t.Errorf("table row %d: %q, chart says %q", i, r[0], CategoryAxisLabels[CategoryIDs[i]])
		}
	}
}

// The 电脑 stat's detail stays short enough for its card; which states the others are in is in
// the full-width line under the stats.
func TestSessionsMachineCountFits(t *testing.T) {
	a := dumpFleet(t)
	v := sessView(t, a, "")
	d := statDetail(v, "电脑")
	if d != "1 台正常 · 4 台要处理" {
		t.Errorf("detail %q", d)
	}
	if w := estTextWidthPx(d, 12); w > 140 {
		t.Errorf("detail %q is %.0f px wide", d, w)
	}
	if !strings.Contains(visibleText(v), "其余 4 台还不能搬（2 台连不上这台电脑、1 台还没装配套小工具、1 台还没设置网盘）") {
		t.Errorf("breakdown:\n%s", visibleText(v))
	}
}
