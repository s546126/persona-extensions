package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// checkedLocal is a test app whose computers were all checked through the fake runner; edit,
// when not nil, then changes the local scan.
func checkedLocal(t *testing.T, edit func(sc *Scan)) *app {
	t.Helper()
	a := newTestApp(t)
	if _, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Action: "scan-all"}}); err != nil {
		t.Fatal(err)
	}
	a.waitJobs()
	if edit != nil {
		if err := a.db().update(func(st *State, _ time.Time) error {
			edit(st.Scans["local"])
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// kindRows are the 删掉：<种类> rows of the local computer's 能删什么 tab, by kind.
func kindRows(t *testing.T, a *app, query string) map[string]wire.Component {
	t.Helper()
	ctx := context.Background()
	st := ext.EncodeState(page{Machine: "local", Tab: tabItems})
	res, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: st, Action: "tab", Values: map[string]string{"tab": tabItems}}})
	if err == nil && query != "" {
		res, err = a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: res.View.State, Action: "search", Values: map[string]string{"q": query}}})
	}
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]wire.Component{}
	for _, s := range res.View.Sections {
		for _, c := range s.Items {
			if c.Action != nil && c.Action.ID == "clean-category" {
				rows[c.Action.Values["cat"]] = c
			}
		}
	}
	return rows
}

// reallyDeleted runs the row's clean-category action and returns the ids the cleanup recorded.
func reallyDeleted(t *testing.T, a *app, row wire.Component) []string {
	t.Helper()
	if _, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		State:  ext.EncodeState(page{Machine: "local", Tab: tabItems}),
		Action: row.Action.ID,
		Values: row.Action.Values,
	}}); err != nil {
		t.Fatal(err)
	}
	a.waitJobs()
	var ids []string
	if err := a.db().read(func(st *State) {
		if len(st.History) == 0 {
			t.Fatal("no cleanup recorded")
		}
		for _, it := range st.History[0].Items {
			ids = append(ids, it.ID)
		}
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(ids)
	return ids
}

// A kind of only 放心删 caches: the row and its confirmation count the two items, say they come
// back, and say nothing about 建议先看; the tap deletes exactly those two.
func TestKindRowWithOnlySafeItems(t *testing.T) {
	a := checkedLocal(t, nil)
	row, ok := kindRows(t, a, "")[CatGoCache]
	if !ok {
		t.Fatal("no Go row")
	}
	if want := "2 项 · 大约腾出 7.2 GiB"; row.Subtitle != want {
		t.Fatalf("subtitle %q, want %q", row.Subtitle, want)
	}
	c := row.Action.Confirm
	for _, want := range []string{"一共 2 项，大约腾出 7.2 GiB", "这 2 项都是「放心删」的缓存，删了以后用到时会重新下载或重新生成"} {
		if !strings.Contains(c, want) {
			t.Errorf("confirm lacks %q: %s", want, c)
		}
	}
	if strings.Contains(c, "建议先看") || strings.Contains(c, "找不回来") {
		t.Errorf("confirm of a cache-only kind: %s", c)
	}
	if got, want := reallyDeleted(t, a, row), []string{"cache-gobuild", "cache-gomod"}; !slices.Equal(got, want) {
		t.Fatalf("deleted %q, want %q", got, want)
	}
}

// A kind holding a 建议先看 item: the row says so before the tap, the confirmation names it
// with its size and its own reason and says it goes too, and the tap does delete it.
func TestKindRowNamesItsReviewItems(t *testing.T) {
	a := checkedLocal(t, nil)
	row, ok := kindRows(t, a, "")[CatRustXcodeBuild]
	if !ok {
		t.Fatal("no Rust/Xcode row")
	}
	if want := "2 项 · 其中 1 项建议先看 · 大约腾出 2.2 GiB"; row.Subtitle != want {
		t.Fatalf("subtitle %q, want %q", row.Subtitle, want)
	}
	c := row.Action.Confirm
	for _, want := range []string{
		"一共 2 项，大约腾出 2.2 GiB",
		"其中 1 项是「放心删」的缓存",
		"其中 1 项是「建议先看」的",
		"Playwright 自动测试用的浏览器（1.2 GiB），自动测试用的几个浏览器，删了下次跑测试前要重新下载，比较大、比较慢",
		"点这一行，它也会一起删掉",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("confirm lacks %q: %s", want, c)
		}
	}
	if got, want := reallyDeleted(t, a, row), []string{"cache-cargo-registry", "cache-playwright"}; !slices.Equal(got, want) {
		t.Fatalf("deleted %q, want %q", got, want)
	}
}

// Past a few 建议先看 items the confirmation names the first ones and then 等 N 项.
func TestKindConfirmNamesAFewReviewItemsThenCounts(t *testing.T) {
	m := &Machine{ID: "build-box", Name: "build-box", Host: "build-box"}
	sc := &Scan{RemoteHome: "/home/wren"}
	for i := range 5 {
		sc.Items = append(sc.Items, ScanItem{
			ID: fmt.Sprintf("big-%d", i), Category: CatRustXcodeBuild, Title: fmt.Sprintf("大东西 %d", i),
			Path: fmt.Sprintf("/home/wren/.cache/big-%d", i), Bytes: uint64(i+1) << 30, Safety: SafetyReview, Reason: "重新下载很慢。",
		})
	}
	plan := categoryCleanPlan(m, sc, CatRustXcodeBuild)
	c := categoryConfirm(m, sc, CatRustXcodeBuild, plan)
	for _, want := range []string{"这 5 项都是「建议先看」的", "大东西 0（1.0 GiB），重新下载很慢；", "大东西 2（3.0 GiB），重新下载很慢等 5 项。", "点这一行，它们也会一起删掉；不想删它们，就先别点。"} {
		if !strings.Contains(c, want) {
			t.Errorf("confirm lacks %q: %s", want, c)
		}
	}
	if strings.Contains(c, "大东西 3") || strings.Contains(c, "放心删") {
		t.Errorf("confirm: %s", c)
	}
}

// In-use items are skipped by the cleanup, so neither the row nor its confirmation counts them;
// the confirmation says they stay.
func TestKindRowLeavesInUseItemsOutOfItsNumbers(t *testing.T) {
	a := checkedLocal(t, func(sc *Scan) {
		for i := range sc.Items {
			if sc.Items[i].ID == "cache-gomod" || sc.Items[i].ID == "cache-cargo-registry" {
				sc.Items[i].InUse = true
			}
		}
	})
	rows := kindRows(t, a, "")
	goRow, rustRow := rows[CatGoCache], rows[CatRustXcodeBuild]
	if want := "1 项 · 大约腾出 4.2 GiB"; goRow.Subtitle != want || goRow.Badge.Text != "4.2 GiB" {
		t.Fatalf("Go row %q / %q, want %q", goRow.Subtitle, goRow.Badge.Text, want)
	}
	if want := "1 项 · 其中 1 项建议先看 · 大约腾出 1.2 GiB"; rustRow.Subtitle != want {
		t.Fatalf("Rust row %q, want %q", rustRow.Subtitle, want)
	}
	for _, c := range []string{goRow.Action.Confirm, rustRow.Action.Confirm} {
		if !strings.Contains(c, "一共 1 项") || !strings.Contains(c, "另有 1 项正在用，不删，也没算在里面。") {
			t.Errorf("confirm: %s", c)
		}
	}
	if !strings.Contains(goRow.Action.Confirm, "这 1 项是「放心删」的缓存") {
		t.Errorf("Go confirm: %s", goRow.Action.Confirm)
	}
	if got, want := reallyDeleted(t, a, rustRow), []string{"cache-playwright"}; !slices.Equal(got, want) {
		t.Fatalf("deleted %q, want %q", got, want)
	}
}

// A search narrows the list, not the tap: the row still counts the whole kind it deletes.
func TestKindRowCountsTheWholeKindWhileSearching(t *testing.T) {
	a := checkedLocal(t, nil)
	rows := kindRows(t, a, "go-build")
	if _, ok := rows[CatDockerUnused]; ok {
		t.Fatal("a kind the search hides still has its row")
	}
	if row := rows[CatGoCache]; row.Subtitle != "2 项 · 大约腾出 7.2 GiB" || !strings.Contains(row.Action.Confirm, "一共 2 项") {
		t.Fatalf("row %q: %s", row.Subtitle, row.Action.Confirm)
	}
}

// Old logs and temporary files do not come back: their kind keeps those facts item by item.
func TestLogsKindRowSaysTheyAreGoneForGood(t *testing.T) {
	a := checkedLocal(t, nil)
	row := kindRows(t, a, "")[CatTmpLogs]
	c := row.Action.Confirm
	for _, want := range []string{
		"一共 3 项",
		"这 3 项都是日志和临时文件，删了就找不回来",
		"只删其中 7 天以前的，最近 7 天的留着",
		"正在写的日志不碰",
		"最近的也会删",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("confirm lacks %q: %s", want, c)
		}
	}
	if strings.Contains(c, "重新下载") || strings.Contains(c, "建议先看") {
		t.Errorf("logs confirm: %s", c)
	}
}

// Old conversations are moved, not deleted: their row keeps its own confirmation.
func TestConversationsKindRowKeepsItsConfirm(t *testing.T) {
	a := checkedLocal(t, nil)
	row, ok := kindRows(t, a, "")[CatColdAgentSession]
	if !ok {
		t.Fatal("no conversations row")
	}
	var m *Machine
	_ = a.db().read(func(st *State) { m = st.machine("local") })
	if want := sessionOffloadConfirm(m); row.Action.Confirm != want {
		t.Fatalf("confirm %q, want %q", row.Action.Confirm, want)
	}
}

// The assistant is told what the page now says: categories take that kind's 建议先看 items too,
// whatever include_review says, and in-use items are always left alone.
func TestCleanToolSaysCategoriesTakeReviewItems(t *testing.T) {
	for _, tl := range tools {
		if tl.name != "disklessly_clean" {
			continue
		}
		cats := tl.props["categories"].(map[string]any)["description"].(string)
		rev := tl.props["include_review"].(map[string]any)["description"].(string)
		if !strings.Contains(cats, "也会一起清理，不看 include_review") || !strings.Contains(rev, "填了 categories 时那几类的 review 项总会清理") || !strings.Contains(tl.description, "正在用的项和空项总是跳过") {
			t.Fatalf("categories %q\ninclude_review %q\n%s", cats, rev, tl.description)
		}
		return
	}
	t.Fatal("no disklessly_clean tool")
}
