package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/budget"
	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/hub"
	"github.com/nwverse/persona-extensions/extensions/token-usage/internal/store"
)

var loc = time.FixedZone("UTC+8", 8*3600)

func fixedNow() time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04", "2026-11-13 12:00", loc)
	return t
}

// filledStore has two machines, two models and a launcher over two days.
func filledStore() *store.Store {
	s := store.Open("")
	at := func(date string, hour int) int64 {
		t, _ := time.ParseInLocation("2006-01-02", date, loc)
		return t.Add(time.Duration(hour) * time.Hour).Unix()
	}
	_, _ = s.Merge("m-a", []store.Bucket{
		{Minute: at("2026-11-13", 9), Model: "alpha", Launcher: "cli", Input: 1000, Output: 100, Requests: 2},
		{Minute: at("2026-11-13", 10), Model: "beta", Input: 400, Requests: 1},
		{Minute: at("2026-11-12", 9), Model: "alpha", Launcher: "cli", Input: 300, Requests: 1},
	}, loc)
	_, _ = s.Merge("m-b", []store.Bucket{
		{Minute: at("2026-11-13", 11), Model: "alpha", Input: 200, Requests: 1},
	}, loc)
	return s
}

func newApp(t *testing.T, withGateway bool) *App {
	t.Helper()
	call := func(ctx context.Context, method string, out any) error {
		return errNoGateway
	}
	if withGateway {
		call = func(ctx context.Context, method string, out any) error {
			ratio := 0.4
			*(out.(*hub.GatewayAccounts)) = hub.GatewayAccounts{Accounts: []hub.GatewayAccount{
				{Provider: "demo", Name: "账号一", Status: "active", Windows: []hub.GatewayWindow{{Kind: "weekly", UsedRatio: &ratio}}},
			}}
			return nil
		}
	}
	bf := budget.Open("")
	if err := bf.Set(budget.Budget{Span: budget.Daily, Limit: 5000}); err != nil {
		t.Fatal(err)
	}
	return New(filledStore(), bf, hub.New("http://hub.example", "tok", call), fixedNow, func() *time.Location { return loc })
}

var errNoGateway = &noGatewayError{}

type noGatewayError struct{}

func (noGatewayError) Error() string { return "unknown method" }

func render(t *testing.T, a *App, state string) *wire.View {
	t.Helper()
	v, err := a.Page(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{State: state}})
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("view rejected: %v", err)
	}
	return v
}

func TestOverviewShowsTotalsTrendAndRankings(t *testing.T) {
	a := newApp(t, true)
	v := render(t, a, "")
	if v.Tabs == nil || len(v.Tabs.Items) != 3 {
		t.Fatal("the page has no three tabs")
	}
	text := flatten(v)
	if !strings.Contains(text, "1.7K") { // today: 1000+100+400+200
		t.Fatalf("today's total missing from:\n%s", text)
	}
	if !strings.Contains(text, "alpha") || !strings.Contains(text, "m-a") {
		t.Fatalf("rankings missing from:\n%s", text)
	}
	if !strings.Contains(text, "终端直接打开") || strings.Contains(text, "其他") { // beta has no launcher; nothing past the top 8
		t.Fatalf("the empty launcher is not shown as 终端直接打开:\n%s", text)
	}
	if !strings.Contains(text, "账号一") {
		t.Fatalf("the gateway accounts are missing:\n%s", text)
	}
}

func TestGatewaySectionHiddenWhenUnavailable(t *testing.T) {
	v := render(t, newApp(t, false), "")
	if strings.Contains(flatten(v), "网关账号额度") {
		t.Fatal("the gateway section shows although the hub has no such method")
	}
}

func TestDetailFilters(t *testing.T) {
	a := newApp(t, false)
	v := render(t, a, `{"tab":"detail","model":"alpha"}`)
	text := flatten(v)
	if strings.Contains(text, "beta") {
		t.Fatalf("the model filter did not apply:\n%s", text)
	}
}

// csvOf is the CSV text the view shows, or "" when it shows none.
func csvOf(v *wire.View) (wire.Component, bool) {
	for _, s := range v.Sections {
		for _, it := range s.Items {
			if it.Type == wire.ComponentCode && it.Key == "csv" {
				return it, true
			}
		}
	}
	return wire.Component{}, false
}

// The export button hands the user the CSV itself: the page it answers with carries the rows as text in a code block,
// one per day, machine, model and launcher, under Chinese headers, filtered like the detail. No toast claims a file.
func TestExportShowsTheCSVToCopy(t *testing.T) {
	a := newApp(t, false)
	res, err := a.Act(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "export", State: `{"tab":"detail","days":7,"model":"alpha"}`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Toast != "" {
		t.Fatalf("the export answers a toast %q instead of the content", res.Toast)
	}
	if res.View == nil {
		t.Fatal("the export answered no page: the CSV never reaches the user")
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("view rejected: %v", err)
	}
	code, ok := csvOf(res.View)
	if !ok {
		t.Fatalf("no CSV on the page:\n%s", flatten(res.View))
	}
	want := "日期,电脑,模型,启动方式,输入,缓存写入,缓存读取,输出,请求次数\n" +
		"2026-11-12,m-a,alpha,cli,300,0,0,0,1\n" +
		"2026-11-13,m-a,alpha,cli,1000,0,0,100,2\n" +
		"2026-11-13,m-b,alpha,终端直接打开,200,0,0,0,1"
	if code.Text != want || code.Truncated || code.Language != "csv" || !strings.HasSuffix(code.Path, ".csv") {
		t.Fatalf("CSV %q (truncated %v, language %q, path %q), want\n%s", code.Text, code.Truncated, code.Language, code.Path, want)
	}
	if !strings.Contains(flatten(res.View), "共 3 行") {
		t.Fatalf("the page does not say how many rows:\n%s", flatten(res.View))
	}

	// It stays through a refresh and a new filter, and the close button takes it away.
	v := render(t, a, res.View.State)
	if _, ok := csvOf(v); !ok {
		t.Fatal("a refresh dropped the CSV")
	}
	res, err = a.Act(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "filter", State: v.State, Values: map[string]string{"model": "beta"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if code, ok := csvOf(res.View); !ok || !strings.Contains(code.Text, "2026-11-13,m-a,beta,终端直接打开,400,0,0,0,1") || strings.Contains(code.Text, "alpha") {
		t.Fatalf("the CSV does not follow the filter: %q", code.Text)
	}
	res, err = a.Act(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Action: "export-close", State: res.View.State}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := csvOf(res.View); ok {
		t.Fatal("the close button left the CSV")
	}
}

// A CSV too long for one page is cut at a whole row, inside the code budget, and the page says how many rows it shows.
func TestALongCSVIsCutAtARowAndSaysSo(t *testing.T) {
	s := store.Open("")
	t0, _ := time.ParseInLocation("2006-01-02", "2026-11-13", loc)
	var buckets []store.Bucket
	for i := 0; i < 6000; i++ {
		buckets = append(buckets, store.Bucket{Minute: t0.Unix() + int64(i%1440)*60, Model: fmt.Sprintf("model-%04d-with-a-long-name", i), Input: 123456789, Requests: 1})
	}
	if _, err := s.Merge("m-a", buckets, loc); err != nil {
		t.Fatal(err)
	}
	a := New(s, budget.Open(""), hub.New("http://hub.example", "tok", nil), fixedNow, func() *time.Location { return loc })
	v := render(t, a, `{"tab":"detail","csv":true}`)
	code, ok := csvOf(v)
	if !ok || !code.Truncated {
		t.Fatalf("a long CSV is not marked cut (found %v)", ok)
	}
	if len(code.Text) > csvMost || utf8.RuneCountInString(code.Text) > 200000 || strings.HasSuffix(code.Text, ",") {
		t.Fatalf("the CSV is %d bytes", len(code.Text))
	}
	shown := strings.Count(code.Text, "\n")
	if !strings.Contains(flatten(v), fmt.Sprintf("只显示了前 %d 行（共 6000 行）", shown)) {
		t.Fatalf("the page does not say how much it shows (%d rows):\n%s", shown, flatten(v))
	}
}

func TestBudgetSetAndDelete(t *testing.T) {
	a := newApp(t, false)
	res, err := a.Act(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "budget-set", Values: map[string]string{"model": "alpha", "span": "week", "limit": "100000"},
	}})
	if err != nil || !strings.Contains(res.Toast, "已保存") {
		t.Fatalf("set: %+v %v", res, err)
	}
	v := render(t, a, `{"tab":"budget"}`)
	if !strings.Contains(flatten(v), "alpha") {
		t.Fatal("the saved budget is not on the page")
	}
	res, err = a.Act(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "budget-delete", Values: map[string]string{"span": "week", "model": "alpha"},
	}})
	if err != nil || !strings.Contains(res.Toast, "已删除") {
		t.Fatalf("delete: %+v %v", res, err)
	}
}

func TestCardShowsTodayAgainstTheWeek(t *testing.T) {
	a := newApp(t, false)
	v, err := a.Card(&ext.UIRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("card rejected: %v", err)
	}
	if v.Title != CardTitle || v.Title == PageTitle {
		t.Errorf("card header %q, want %q", v.Title, CardTitle)
	}
	text := flatten(v)
	if !strings.Contains(text, "1.7K") || !strings.Contains(text, "alpha") {
		t.Fatalf("card:\n%s", text)
	}
}

func TestEmptyStateStillDraws(t *testing.T) {
	a := New(store.Open(""), budget.Open(""), hub.New("", "", nil), fixedNow, func() *time.Location { return loc })
	for _, state := range []string{"", `{"tab":"detail"}`, `{"tab":"budget"}`} {
		render(t, a, state)
	}
	if _, err := a.Card(&ext.UIRequest{}); err != nil {
		t.Fatal(err)
	}
}

// With no data yet, each tab says what will appear and when, not just that nothing is there.
func TestEmptyStatesSayWhatAndWhen(t *testing.T) {
	c := hub.New("http://hub.example", "tok", nil)
	a := New(store.Open(""), budget.Open(""), c, fixedNow, func() *time.Location { return loc })
	if text := flatten(render(t, a, "")); !strings.Contains(text, "每 10 分钟读一次") || !strings.Contains(text, "最近 7 天") {
		t.Fatalf("before the first read:\n%s", text)
	}
	c.ReportPull(hub.PullReport{At: fixedNow()})
	if text := flatten(render(t, a, "")); !strings.Contains(text, "已经读过一次") || !strings.Contains(text, "最多 10 分钟") {
		t.Fatalf("after a read with nothing in it:\n%s", text)
	}
	if text := flatten(render(t, a, `{"tab":"detail"}`)); !strings.Contains(text, "最近 30 天里还没有用量") {
		t.Fatalf("detail:\n%s", text)
	}
	if text := flatten(render(t, newApp(t, false), `{"tab":"detail","model":"nope"}`)); !strings.Contains(text, "把筛选改回“全部”") {
		t.Fatalf("detail with a filter that keeps nothing:\n%s", text)
	}
	if text := flatten(render(t, a, `{"tab":"budget"}`)); !strings.Contains(text, "保存预算") || !strings.Contains(text, "八成") {
		t.Fatalf("budgets:\n%s", text)
	}
	if text := flatten(render(t, a, "")); !strings.Contains(text, "启动方式是") || strings.Contains(text, "启动器") {
		t.Fatalf("the launcher ranking is not explained:\n%s", text)
	}
}

// When reading fails the page says why and what to do, in the user's words, above the data it still has.
func TestAFailedReadIsShownWithWhatToDo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := hub.New(srv.URL, "tok", nil)
	_, err := c.Machines(context.Background())
	c.ReportPull(hub.PullReport{At: fixedNow(), Err: err})
	a := New(filledStore(), budget.Open(""), c, fixedNow, func() *time.Location { return loc })
	text := flatten(render(t, a, ""))
	if !strings.Contains(text, "现在读不到用量") || !strings.Contains(text, "关掉再打开") || !strings.Contains(text, "之前已经读到") {
		t.Fatalf("the failed read is not explained:\n%s", text)
	}
	if strings.Contains(text, "hub") || strings.Contains(text, "HTTP") {
		t.Fatalf("the notice shows jargon:\n%s", text)
	}
	a = New(store.Open(""), budget.Open(""), hub.New("", "", nil), fixedNow, func() *time.Location { return loc })
	if text := flatten(render(t, a, `{"tab":"detail"}`)); !strings.Contains(text, "没有给这个扩展读取用量的权限") {
		t.Fatalf("no access:\n%s", text)
	}
}

// flatten is every string of a view, for assertions that do not care where a number sits.
func flatten(v *wire.View) string {
	var b strings.Builder
	for _, s := range v.Sections {
		b.WriteString(s.Title + "\n")
		for _, it := range s.Items {
			b.WriteString(strings.Join([]string{it.Text, it.Label, it.Value, it.Detail, it.Title}, " ") + "\n")
			for _, st := range it.Stats {
				b.WriteString(st.Label + " " + st.Value + " " + st.Detail + "\n")
			}
			for _, row := range it.Rows {
				b.WriteString(strings.Join(row, " ") + "\n")
			}
		}
		b.WriteString(s.Footer + "\n")
	}
	return b.String()
}

func TestActReturnsUpdatedViewsForNavigationFiltersAndBudgets(t *testing.T) {
	a := newApp(t, false)

	// Switch to detail tab.
	res, err := a.Act(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "tab", Values: map[string]string{"tab": tabDetail},
	}})
	if err != nil {
		t.Fatalf("tab action error: %v", err)
	}
	if res.View == nil {
		t.Fatalf("tab action returned nil View; state change would be discarded by SDK")
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("detail view invalid: %v", err)
	}
	if res.View.Tabs == nil || res.View.Tabs.Value != tabDetail {
		t.Fatalf("tab view Tabs.Value = %#v, want %q", res.View.Tabs, tabDetail)
	}
	hasFilterSubmit := false
	for _, it := range res.View.Sections[0].Items {
		if it.Type == wire.ComponentField && it.Action != nil {
			t.Fatalf("choice field %q should not attach non-schema Action", it.Name)
		}
		if it.Type == wire.ComponentButton && it.Action != nil && it.Action.ID == "filter" && it.Action.Submit {
			hasFilterSubmit = true
		}
	}
	if !hasFilterSubmit {
		t.Fatalf("detail view missing submit button for filter action")
	}

	// Change range on detail tab -> preserves tab and updates days.
	res, err = a.Act(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "range", State: res.View.State, Values: map[string]string{"days": "7"},
	}})
	if err != nil || res.View == nil {
		t.Fatalf("range action = %#v, %v", res, err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("range view invalid: %v", err)
	}
	var ps pageState
	if err := (&ext.UIRequest{UIParams: wire.UIParams{State: res.View.State}}).DecodeState(&ps); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if ps.Tab != tabDetail || ps.Days != 7 {
		t.Fatalf("after range: state = %+v, want tab=detail days=7", ps)
	}

	// Filter updates keep previously set filter dimensions when only one key is sent.
	res, err = a.Act(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "filter", State: res.View.State, Values: map[string]string{"model": "alpha"},
	}})
	if err != nil || res.View == nil {
		t.Fatalf("filter model = %#v, %v", res, err)
	}
	res, err = a.Act(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "filter", State: res.View.State, Values: map[string]string{"machine": "m-a"},
	}})
	if err != nil || res.View == nil {
		t.Fatalf("filter machine = %#v, %v", res, err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("filtered view invalid: %v", err)
	}
	ps = pageState{}
	_ = (&ext.UIRequest{UIParams: wire.UIParams{State: res.View.State}}).DecodeState(&ps)
	if ps.Model != "alpha" || ps.Machine != "m-a" {
		t.Fatalf("after filters: state = %+v, want model=alpha machine=m-a", ps)
	}

	// Switch budget form span to weekly, then save without repeating span in submit values.
	res, err = a.Act(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "budget-span", State: `{"tab":"budget"}`, Values: map[string]string{"span": budget.Weekly},
	}})
	if err != nil || res.View == nil {
		t.Fatalf("budget-span = %#v, %v", res, err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("budget-span view invalid: %v", err)
	}
	foundWeekly := false
	for _, it := range res.View.Sections[1].Items {
		if it.Type == wire.ComponentSegmented && it.Name == "span" && it.Value == budget.Weekly {
			foundWeekly = true
		}
	}
	if !foundWeekly {
		t.Fatalf("budget-span did not update segmented control value to %q", budget.Weekly)
	}

	res, err = a.Act(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "budget-set", State: res.View.State, Values: map[string]string{"model": "alpha", "limit": "50000"},
	}})
	if err != nil || res.View == nil {
		t.Fatalf("budget-set must return refreshed View, got res=%#v err=%v", res, err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("budget-set view invalid: %v", err)
	}
	if !strings.Contains(flatten(res.View), "每周 · alpha") {
		t.Fatalf("budget-set refreshed view missing saved weekly alpha budget:\n%s", flatten(res.View))
	}

	// Delete budget returns refreshed View without deleted budget.
	res, err = a.Act(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "budget-delete", State: res.View.State, Values: map[string]string{"span": budget.Weekly, "model": "alpha"},
	}})
	if err != nil || res.View == nil {
		t.Fatalf("budget-delete must return refreshed View, got res=%#v err=%v", res, err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("budget-delete view invalid: %v", err)
	}
	if strings.Contains(flatten(res.View), "每周 · alpha") {
		t.Fatalf("budget-delete refreshed view still shows deleted budget:\n%s", flatten(res.View))
	}
}

func TestOverviewHandlesEmptyModelName(t *testing.T) {
	s := store.Open("")
	t0, _ := time.ParseInLocation("2006-01-02 15", "2026-11-13 09", loc)
	_, _ = s.Merge("m-a", []store.Bucket{
		{Minute: t0.Unix(), Model: "", Input: 500, Requests: 1},
	}, loc)
	a := New(s, budget.Open(""), hub.New("", "", nil), fixedNow, func() *time.Location { return loc })
	v := render(t, a, "")
	if !strings.Contains(flatten(v), "（未命名）") {
		t.Fatalf("empty model should render as （未命名）:\n%s", flatten(v))
	}
}

// The notice for computers a round could not read names them and tells a removed one (404) from a passing fault, and
// never says the hub is too old for one computer's 404; a round where every computer failed says so.
func TestFailedComputersAreNamedWithTheRightCause(t *testing.T) {
	gone := httpErr(t, http.StatusNotFound)
	busy := httpErr(t, http.StatusBadGateway)
	for _, tt := range []struct {
		name    string
		report  hub.PullReport
		want    []string
		mustNot []string
	}{
		{"one removed", hub.PullReport{Total: 3, Failed: []hub.FailedMachine{{Name: "旧笔记本", Err: gone}}},
			[]string{"有 1 台电脑的用量没读到", "旧笔记本", "共 3 台电脑，其余的已照常更新", "找不到这台电脑，可能它被移除了"},
			[]string{"版本太旧", "更新到最新版"}},
		{"one passing fault", hub.PullReport{Total: 3, Failed: []hub.FailedMachine{{Name: "书房电脑", Err: busy}}},
			[]string{"有 1 台电脑的用量没读到", "书房电脑", "其余的已照常更新", "10 分钟后会自动再试"},
			[]string{"运行 Persona 的那台电脑暂时出了问题", "版本太旧"}},
		{"all failed", hub.PullReport{Total: 2, Failed: []hub.FailedMachine{{Name: "甲", Err: busy}, {Name: "乙", Err: busy}}},
			[]string{"这次一台电脑的用量都没读到", "甲、乙", "暂时出了问题"},
			[]string{"其余的"}},
		{"many", hub.PullReport{Total: 9, Failed: []hub.FailedMachine{{Name: "1", Err: busy}, {Name: "2", Err: busy}, {Name: "3", Err: busy}, {Name: "4", Err: busy}, {Name: "5", Err: busy}, {Name: "6", Err: busy}, {Name: "7", Err: gone}}},
			[]string{"有 7 台电脑的用量没读到", "1、2、3、4、5 等 7 台", "可能被移除了"},
			[]string{"、6"}},
		{"not a hub error", hub.PullReport{Err: errors.New("invalid character '<' looking for beginning of value")},
			[]string{"现在读不到用量", "出了意外的问题", "重启 Persona"},
			[]string{"invalid", "character"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := hub.New("http://hub.example", "tok", nil)
			tt.report.At = fixedNow()
			c.ReportPull(tt.report)
			a := New(filledStore(), budget.Open(""), c, fixedNow, func() *time.Location { return loc })
			text := flatten(render(t, a, ""))
			for _, w := range tt.want {
				if !strings.Contains(text, w) {
					t.Fatalf("missing %q:\n%s", w, text)
				}
			}
			for _, w := range tt.mustNot {
				if strings.Contains(text, w) {
					t.Fatalf("shows %q:\n%s", w, text)
				}
			}
		})
	}
}

// The no-access notice fits a case where nothing was ever tried: it does not speak of a last attempt.
func TestNoAccessIsNotALastAttempt(t *testing.T) {
	a := New(store.Open(""), budget.Open(""), hub.New("", "", nil), fixedNow, func() *time.Location { return loc })
	text := flatten(render(t, a, ""))
	if !strings.Contains(text, "现在读不到用量") || strings.Contains(text, "最近一次") {
		t.Fatalf("no access:\n%s", text)
	}
}

// httpErr is the error the client gets for a hub answering status.
func httpErr(t *testing.T, status int) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "x", status)
	}))
	defer srv.Close()
	_, err := hub.New(srv.URL, "tok", nil).Machines(context.Background())
	if err == nil {
		t.Fatal("no error")
	}
	return err
}
