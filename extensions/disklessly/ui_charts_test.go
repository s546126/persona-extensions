package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

const sampleProbeMacOK = `@@SECTION:UNAME
Darwin arm64
@@SECTION:HOSTNAME
mac-mini
@@SECTION:HOME
/Users/dev
@@SECTION:BIN_VERSION
disklessly 0.3.0
@@SECTION:DF
Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/disk3s5     524288000 251658240 272629760      48% /System/Volumes/Data
@@SECTION:CACHES
cache-gobuild	go-cache	Go 编译缓存	/Users/dev/Library/Caches/go-build	1048576	safe	Go 编译时留下的缓存，删了下次编译会自动再生成
cache-xcode-derived	rust-xcode-build	Xcode 编译缓存	/Users/dev/Library/Developer/Xcode/DerivedData	6291456	safe	Xcode 编译时留下的文件，删了下次编译会重新生成，第一次会慢一些
cache-playwright	rust-xcode-build	Playwright 自动测试用的浏览器	/Users/dev/.cache/ms-playwright	1572864	review	自动测试用的几个浏览器，删了下次跑测试前要重新下载，比较大、比较慢
@@SECTION:TMP_LOGS
tmp-stale	tmp-logs	临时文件夹里的旧文件和编译剩下的文件	/tmp	262144	safe	临时文件夹 /tmp 里编译和安装剩下的文件（go-build、playwright、npm、pnpm、cargo-install 等开头的，最近的也删）和放了 7 天以上的文件
@@SECTION:DOCKER
@@SECTION:SESSIONS
tool	claude	Claude Code	/Users/dev/.claude/projects	12	163840	8	524288	/Users/dev/.claude/projects
@@SECTION:SESSIONS_CLI_PLAN
{"ok":true,"applied":false,"cold":["/Users/dev/.claude/projects/-Users-dev-demo/s1.jsonl"],"skipped":[]}
@@SECTION:SESSIONS_CLI_STATUS
{"ok":true,"deviceId":"00000000-0000-4000-8000-000000000003","backend":{"id":"icloud","type":"folder","reachable":true,"latencyMs":8,"error":""},"counts":{"archived":5,"remote":0,"restored":0,"keptLocal":0,"pendingDelete":0},"archivedBytes":4194304,"segments":{"total":2,"checked":0,"missing":[]}}
@@SECTION:SESSIONS_FILES
524288	/Users/dev/.claude/projects/-Users-dev-demo/s1.jsonl
@@SECTION:END
`

func seedScannedThreeMachineFleet(t *testing.T) *app {
	t.Helper()
	noSandbox(t)
	a := newTestApp(t)
	a.runner = func(ctx context.Context, m *Machine, script string, d time.Duration) (string, error) {
		if strings.Contains(script, "@@SECTION:UNAME") && m != nil && m.ID == "mac-mini" {
			return sampleProbeMacOK, nil
		}
		return fakeRunner(ctx, m, script, d)
	}
	if err := a.db().update(func(st *State, now time.Time) error {
		st.ensureDefaults(now)
		if local := st.machine("local"); local != nil {
			local.Name = "local (dev-workstation)"
		}
		if _, err := st.upsertMachine(MachineInput{ID: "armbox", Host: "armbox", Name: "armbox (ARM Builder)", Source: "ssh-config"}, now); err != nil {
			return err
		}
		if _, err := st.upsertMachine(MachineInput{ID: "mac-mini", Host: "mac-mini", Name: "mac-mini (Mac mini)", Source: "persona"}, now); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	v, err := a.render(ctx, &ext.UIRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: v.State, Action: "scan-all"}}); err != nil {
		t.Fatal(err)
	}
	a.waitJobs()
	return a
}

func checkStrictView(t *testing.T, v *wire.View, compact bool) {
	t.Helper()
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("wire.CheckView failed: %v", err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.CheckViewJSON(raw); err != nil {
		t.Fatalf("wire.CheckViewJSON failed: %v", err)
	}
	if err := exttest.CheckView(raw, compact); err != nil {
		t.Fatalf("exttest.CheckView(compact=%v) failed: %v", compact, err)
	}
}

func findComponentByKey(v *wire.View, key string) *wire.Component {
	for si := range v.Sections {
		for ii := range v.Sections[si].Items {
			if v.Sections[si].Items[ii].Key == key {
				return &v.Sections[si].Items[ii]
			}
		}
	}
	return nil
}

func findFieldByName(v *wire.View, name string) *wire.Component {
	for si := range v.Sections {
		for ii := range v.Sections[si].Items {
			if v.Sections[si].Items[ii].Type == wire.ComponentField && v.Sections[si].Items[ii].Name == name {
				return &v.Sections[si].Items[ii]
			}
		}
	}
	return nil
}

var privateOrInternalPattern = regexp.MustCompile(
	`(?i)(\b10\.\d{1,3}\.\d{1,3}\.\d{1,3}\b|` +
		`\b192\.168\.\d{1,3}\.\d{1,3}\b|` +
		`\b172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}\b|` +
		`\b100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.\d{1,3}\.\d{1,3}\b|` +
		`\.(internal|corp|lan|intranet|localdomain)\b)`,
)

func assertNoPrivateDataInViewJSON(t *testing.T, label string, v *wire.View) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if m := privateOrInternalPattern.FindString(s); m != "" {
		t.Fatalf("%s leaked private IP or internal hostname %q in JSON", label, m)
	}
	if hn, err := os.Hostname(); err == nil {
		hn = strings.TrimSpace(hn)
		if hn != "" && !slices.Contains([]string{"localhost", "local", "armbox", "mac-mini", "devvm-dev"}, strings.ToLower(hn)) {
			if strings.Contains(strings.ToLower(s), strings.ToLower(hn)) {
				t.Fatalf("%s leaked host os.Hostname() %q in JSON", label, hn)
			}
		}
	}
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" && !slices.Contains([]string{"dev", "admin", "ubuntu", "root", "armbox"}, strings.ToLower(u)) {
		if strings.Contains(strings.ToLower(s), strings.ToLower(u)) {
			t.Fatalf("%s leaked environment USER %q in JSON", label, u)
		}
	}
}

// assertMobileReadability360px checks that every Progress component and every 4-column Table in v
// fits a 360px phone viewport (296px content width inside px-4 page + px-4 section) without truncating
// byte usage on Progress.Label or forcing horizontal table scroll.
func assertMobileReadability360px(t *testing.T, label string, v *wire.View) {
	t.Helper()
	const (
		phoneContentPx = 296.0 // 360px viewport - 32px page px-4 - 32px section px-4
		progressGapPx  = 12.0  // gap-3 in ViewBody.tsx
		tablePaddingPx = 48.0  // 4 columns: pr-2 (8) + px-2 (16) + px-2 (16) + pl-2 (8)
	)
	for _, s := range v.Sections {
		for _, it := range s.Items {
			switch it.Type {
			case wire.ComponentProgress:
				if strings.Contains(it.Label, "根分区") || strings.Contains(it.Label, "/System") {
					t.Fatalf("%s progress %q should omit mount path from Label so byte usage never truncates: %q", label, it.Key, it.Label)
				}
				lw := personaLabelWidth(it.Label, 13.0)
				dw := personaLabelWidth(it.Detail, 12.0)
				if lw+progressGapPx+dw > phoneContentPx {
					t.Fatalf("%s progress %q overflows 360px phone row: label=%q (%.1fpx) + gap (12px) + detail=%q (%.1fpx) = %.1fpx > %.1fpx",
						label, it.Key, it.Label, lw, it.Detail, dw, lw+progressGapPx+dw, phoneContentPx)
				}
			case wire.ComponentTable:
				if len(it.Columns) > 4 {
					t.Fatalf("%s table %q has %d columns, want <= 4", label, it.Key, len(it.Columns))
				}
				colMax := make([]float64, len(it.Columns))
				for c, col := range it.Columns {
					colMax[c] = personaLabelWidth(col.Title, 12.5)
				}
				for rIdx, row := range it.Rows {
					for c, cell := range row {
						if utf8.RuneCountInString(cell) > 16 {
							t.Fatalf("%s table %q row %d col %d has %d runes (>16): %q",
								label, it.Key, rIdx, c, utf8.RuneCountInString(cell), cell)
						}
						if cw := personaLabelWidth(cell, 12.5); cw > colMax[c] {
							colMax[c] = cw
						}
					}
				}
				var sum float64
				for _, w := range colMax {
					sum += w
				}
				totalW := tablePaddingPx + sum
				if totalW > phoneContentPx {
					t.Fatalf("%s table %q overflows 360px phone width: cols=%v + 48px padding = %.1fpx > %.1fpx",
						label, it.Key, colMax, totalW, phoneContentPx)
				}
			}
		}
	}
}

func TestPerMachineChartsThresholdsAndDetailTables(t *testing.T) {
	a := seedScannedThreeMachineFleet(t)
	ctx := context.Background()

	// 1. Fleet tab: verify all 3 machines (85% warning, 92% critical, 48% ok) have consistent tones,
	// root used-vs-free stacked bar charts, 6-category reclaimable stacked bar charts, and 4-column tables.
	fleetView, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, TZ: "Asia/Shanghai"}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, fleetView, false)
	assertMobileReadability360px(t, "fleet", fleetView)

	if fleetView.Fallback == "" || utf8.RuneCountInString(fleetView.Fallback) > 2000 {
		t.Fatalf("unexpected fallback length %d: %q", utf8.RuneCountInString(fleetView.Fallback), fleetView.Fallback)
	}
	for _, word := range []string{"偏满", "快满了", "宽裕", localMachineName, "armbox", "mac-mini"} {
		if !strings.Contains(fleetView.Fallback, word) {
			t.Fatalf("fallback missing %q:\n%s", word, fleetView.Fallback)
		}
	}

	wantMachines := []struct {
		id         string
		tone       string
		levelWord  string
		wantDetail string
		wantLabel  string
		wantSer    int
		coldName   string
	}{
		// local: 85% used -> warning (caution orange), has safe + review + cold-session CLI offload (3 series)
		{id: "local", tone: wire.ToneCaution, levelWord: "偏满", wantDetail: "85.0% · 偏满", wantLabel: "已用 170.0 / 200.0 GiB", wantSer: 3, coldName: "搬到网盘"},
		// armbox: 92% used -> critical (danger red), has safe + pending cold sessions without CLI (2 series)
		{id: "armbox", tone: wire.ToneDanger, levelWord: "快满了", wantDetail: "92.0% · 快满了", wantLabel: "已用 92.0 / 100.0 GiB", wantSer: 2, coldName: "搬到网盘（缺小工具）"},
		// mac-mini: 48% used -> ok (positive green), has safe + review + cold-session CLI offload (3 series)
		{id: "mac-mini", tone: wire.TonePositive, levelWord: "宽裕", wantDetail: "48.0% · 宽裕", wantLabel: "已用 240.0 / 500.0 GiB", wantSer: 3, coldName: "搬到网盘"},
	}

	wantAxis := []string{"Go", "Node/Py", "容器", "Rust/XC", "日志", "旧对话"}

	for _, wm := range wantMachines {
		// The charts are drawn for the selected computer; the others keep their bar only.
		fleetView, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, TZ: "Asia/Shanghai", State: ext.EncodeState(page{Machine: wm.id})}})
		if err != nil {
			t.Fatal(err)
		}
		checkStrictView(t, fleetView, false)
		assertMobileReadability360px(t, "fleet "+wm.id, fleetView)
		for _, other := range wantMachines {
			if other.id == wm.id {
				continue
			}
			if findComponentByKey(fleetView, "fleet-root-"+other.id) != nil || findComponentByKey(fleetView, "fleet-stats-"+other.id) != nil {
				t.Fatalf("%s selected: %s also drawn in full", wm.id, other.id)
			}
			if findComponentByKey(fleetView, "prog-"+other.id) == nil || findComponentByKey(fleetView, "btn-plan-"+other.id) == nil {
				t.Fatalf("%s selected: %s lost its bar or buttons", wm.id, other.id)
			}
		}
		if findComponentByKey(fleetView, "fleet-stats-"+wm.id) == nil {
			t.Fatalf("%s: missing disjoint fleet-stats-%s stat_grid", wm.id, wm.id)
		}
		prog := findComponentByKey(fleetView, "prog-"+wm.id)
		if prog == nil || prog.Type != wire.ComponentProgress {
			t.Fatalf("%s: missing progress component", wm.id)
		}
		if prog.Tone != wm.tone || prog.Detail != wm.wantDetail || prog.Label != wm.wantLabel {
			t.Fatalf("%s: progress tone=%q label=%q detail=%q, want tone=%q label=%q detail=%q",
				wm.id, prog.Tone, prog.Label, prog.Detail, wm.tone, wm.wantLabel, wm.wantDetail)
		}

		// The system disk is the bar above (prog-) and the table's caption: no tall one-bar chart.
		if rootChart := findComponentByKey(fleetView, "fleet-root-"+wm.id); rootChart != nil {
			t.Fatalf("%s: a separate system-disk chart is drawn next to its bar: %+v", wm.id, rootChart)
		}

		catChart := findComponentByKey(fleetView, "fleet-cats-"+wm.id)
		if catChart == nil || catChart.Type != wire.ComponentChart || catChart.Kind != wire.ChartKindBar || !catChart.Stacked {
			t.Fatalf("%s: missing or non-stacked category chart: %+v", wm.id, catChart)
		}
		if len(catChart.Series) != wm.wantSer || len(catChart.Series) > wire.SeriesMost {
			t.Fatalf("%s: category chart series count = %d, want %d (<= %d)", wm.id, len(catChart.Series), wm.wantSer, wire.SeriesMost)
		}
		lastSer := catChart.Series[len(catChart.Series)-1]
		if lastSer.Name != wm.coldName || lastSer.Color != "blue" || lastSer.Tone != "" {
			t.Fatalf("%s: cold series = %+v, want Name=%q Color=blue Tone=\"\"", wm.id, lastSer, wm.coldName)
		}
		var gotLabels []string
		for _, pt := range catChart.Series[0].Points {
			gotLabels = append(gotLabels, pt.Label)
		}
		if !slices.Equal(gotLabels, wantAxis) {
			t.Fatalf("%s: category labels = %v, want %v", wm.id, gotLabels, wantAxis)
		}

		tbl := findComponentByKey(fleetView, "fleet-tbl-"+wm.id)
		if tbl == nil || tbl.Type != wire.ComponentTable {
			t.Fatalf("%s: missing detail table", wm.id)
		}
		if len(tbl.Columns) != 4 || len(tbl.Rows) != 6 || !strings.Contains(tbl.Caption, wm.levelWord) {
			t.Fatalf("%s: unexpected table cols=%d rows=%d caption=%q", wm.id, len(tbl.Columns), len(tbl.Rows), tbl.Caption)
		}
	}

	// 2. Items tab & search filtering (including empty search result path)
	itemsView, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 8,
		State:     ext.EncodeState(page{Machine: "local", Tab: tabItems}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, itemsView, false)
	assertMobileReadability360px(t, "items", itemsView)
	if findComponentByKey(itemsView, "prog-local") == nil || findComponentByKey(itemsView, "items-cats-local") == nil {
		t.Fatal("items tab missing per-machine charts")
	}
	if tbl := findComponentByKey(itemsView, "items-detail-tbl-local"); tbl == nil || len(tbl.Columns) != 4 {
		t.Fatalf("items tab missing 4-column detail table: %+v", tbl)
	}

	filteredRes, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 8,
		State:     ext.EncodeState(page{Machine: "local", Tab: tabItems}),
		Action:    "search",
		Values:    map[string]string{"q": "ms-playwright"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, filteredRes.View, false)
	assertMobileReadability360px(t, "items-filtered", filteredRes.View)
	filteredCat := findComponentByKey(filteredRes.View, "items-cats-local")
	if filteredCat == nil || len(filteredCat.Series) != 1 || filteredCat.Series[0].Tone != wire.ToneCaution {
		t.Fatalf("search-filtered category chart should isolate review series: %+v", filteredCat)
	}

	emptyFilterRes, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 8,
		State:     ext.EncodeState(page{Machine: "local", Tab: tabItems}),
		Action:    "search",
		Values:    map[string]string{"q": "no-such-artifact-xyz"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, emptyFilterRes.View, false)
	assertMobileReadability360px(t, "items-empty-filter", emptyFilterRes.View)
	if findComponentByKey(emptyFilterRes.View, "prog-local") == nil || findComponentByKey(emptyFilterRes.View, "items-tbl-local") == nil {
		t.Fatalf("empty search filter on items tab must still include companion detail table items-tbl-local: %+v", emptyFilterRes.View)
	}

	// 3. Plan tab: verify before-vs-after root capacity stacked bar chart (distinct pre/post tones + <25 GiB note) & cold-session toggle default
	planView, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 8,
		State:     ext.EncodeState(page{Machine: "armbox", Tab: tabPlan}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, planView, false)
	assertMobileReadability360px(t, "plan", planView)
	impact := findComponentByKey(planView, "plan-impact-armbox")
	if impact == nil || impact.Kind != wire.ChartKindBar || !impact.Stacked || len(impact.Series) != 3 || len(impact.Series[0].Points) != 2 {
		t.Fatalf("plan impact chart for armbox (critical -> warning) should have 3 stacked series: %+v", impact)
	}
	if impact.Series[0].Tone != wire.ToneDanger || impact.Series[1].Tone != wire.ToneCaution || impact.Series[2].Tone != wire.ToneNeutral {
		t.Fatalf("armbox plan impact series tones = [%s, %s, %s], want [danger, caution, neutral]",
			impact.Series[0].Tone, impact.Series[1].Tone, impact.Series[2].Tone)
	}
	if !strings.Contains(impact.Series[0].Points[0].Display, "快满了") || !strings.Contains(impact.Series[1].Points[1].Display, "偏满") {
		t.Fatalf("armbox plan impact Point.Display must carry status words: before=%q after=%q",
			impact.Series[0].Points[0].Display, impact.Series[1].Points[1].Display)
	}
	if !strings.Contains(impact.Detail, "快满了") || !strings.Contains(impact.Detail, "偏满") || !strings.Contains(impact.Detail, "不到 25 GiB") {
		t.Fatalf("armbox plan impact Detail should explain why 79.7%% (<80%%) stays 偏满 (< 25 GiB free): %q", impact.Detail)
	}
	if tbl := findComponentByKey(planView, "plan-tbl-armbox"); tbl == nil || len(tbl.Columns) != 4 {
		t.Fatalf("plan tab missing 4-column table: %+v", tbl)
	}

	planLocal, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 8,
		State:     ext.EncodeState(page{Machine: "local", Tab: tabPlan}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, planLocal, false)
	assertMobileReadability360px(t, "plan-local", planLocal)
	if f := findFieldByName(planLocal, "cat_"+CatColdAgentSession); f == nil || f.Value != "false" {
		t.Fatalf("cold-agent-sessions toggle must default to false to preserve local transcripts: %+v", f)
	}
	if f := findFieldByName(planLocal, "cat_"+CatGoCache); f == nil || f.Value != "true" {
		t.Fatalf("safe Go cache toggle should default to true: %+v", f)
	}

	// 4. Overview card BEFORE clean-safe: armbox is at 92% critical (rank 0) -> features card-root-armbox and disjoint card-mach-* keys
	cardBefore, err := a.card(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, Size: wire.SizeCompact, Placement: wire.PlacementOverviewCard}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, cardBefore, true)
	cardBeforeRoot := findComponentByKey(cardBefore, "card-root-armbox")
	if findComponentByKey(cardBefore, "card-stats") == nil || cardBeforeRoot == nil || findComponentByKey(cardBefore, "card-mach-armbox") == nil {
		t.Fatalf("overview card before clean-safe should feature highest-severity machine armbox chart and card-mach-* rows: %+v", cardBefore)
	}
	if cardBeforeRoot.Type != wire.ComponentProgress || cardBeforeRoot.Tone != wire.ToneDanger || !strings.Contains(cardBeforeRoot.Detail, "快满了") || !strings.HasPrefix(cardBeforeRoot.Label, "armbox · ") {
		t.Fatalf("card-root-armbox should reflect critical danger state: %+v", cardBeforeRoot)
	}

	// 5. Execute clean-safe on armbox, check 4-column Sessions & History tables, and verify Overview card AFTER clean-safe:
	// armbox drops from 92% critical to ~42.8% ok, so local (85% warning) becomes the featured machine (card-root-local).
	cleanRes, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 8,
		State:     ext.EncodeState(page{Machine: "armbox", Tab: tabPlan}),
		Action:    "clean-safe",
		Values:    map[string]string{"m": "armbox"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, cleanRes.View, false)
	a.waitJobs()

	sessView, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 8,
		State:     ext.EncodeState(page{Machine: "local", Tab: tabSessions}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, sessView, false)
	assertMobileReadability360px(t, "sessions-and-history", sessView)
	if findComponentByKey(sessView, "hist-tbl") == nil {
		t.Fatal("sessions tab after clean-safe should include hist-tbl")
	}

	cardAfter, err := a.card(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, Size: wire.SizeCompact, Placement: wire.PlacementOverviewCard}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, cardAfter, true)
	cardAfterRoot := findComponentByKey(cardAfter, "card-root-local")
	if findComponentByKey(cardAfter, "card-stats") == nil || cardAfterRoot == nil || findComponentByKey(cardAfter, "card-root-armbox") != nil {
		t.Fatalf("overview card after armbox clean-safe should switch featured chart to warning machine local: %+v", cardAfter)
	}
	if cardAfterRoot.Type != wire.ComponentProgress || cardAfterRoot.Tone != wire.ToneCaution || !strings.Contains(cardAfterRoot.Detail, "偏满") {
		t.Fatalf("card-root-local should reflect warning caution state: %+v", cardAfterRoot)
	}
	if !strings.Contains(texts(cardAfter), "最近一次清理 armbox") {
		t.Fatalf("overview card after clean-safe should include latest df cleanup ledger summary:\n%s", texts(cardAfter))
	}

	for name, v := range map[string]*wire.View{
		"fleet":      fleetView,
		"items":      itemsView,
		"plan":       planView,
		"sessions":   sessView,
		"cardBefore": cardBefore,
		"cardAfter":  cardAfter,
	} {
		assertNoPrivateDataInViewJSON(t, name, v)
	}

	if dumpDir := os.Getenv("DISKLESSLY_DUMP_SAMPLE_DIR"); dumpDir != "" {
		if err := os.MkdirAll(dumpDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, v := range map[string]*wire.View{
			"fleet.json": fleetView,
			"items.json": itemsView,
			"plan.json":  planView,
			"card.json":  cardBefore,
		} {
			b, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dumpDir, name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The hub's own computer in the 清理记录 table's narrow 电脑 cell.
func TestHistoryTableNamesThisComputerReadably(t *testing.T) {
	a := seedScannedThreeMachineFleet(t)
	ctx := context.Background()
	for _, id := range []string{"armbox", "local"} {
		if _, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, State: ext.EncodeState(page{Machine: id, Tab: tabPlan}),
			Action: "clean-safe", Values: map[string]string{"m": id}}}); err != nil {
			t.Fatal(err)
		}
		a.waitJobs()
	}
	// The hub's own computer in the narrow 电脑 cell reads 本机, not a clipped 「运行 P…」, and the
	// record rows below still spell out its name.
	histView, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, State: ext.EncodeState(page{Machine: "local", Tab: tabHistory})}})
	if err != nil {
		t.Fatal(err)
	}
	tbl := findComponentByKey(histView, "hist-tbl")
	if tbl == nil {
		t.Fatal("history tab should include hist-tbl")
	}
	cells := map[string]bool{}
	for _, r := range tbl.Rows {
		cells[r[1]] = true
		if strings.HasPrefix(r[1], "运行") {
			t.Fatalf("local computer clipped in the history table: %q", r[1])
		}
	}
	if !cells[localMachineCell] || !cells["armbox"] {
		t.Fatalf("history 电脑 cells: %v", cells)
	}
	if w := personaLabelWidth(localMachineCell, 12.5); w > 48 {
		t.Fatalf("%q is %.1fpx, wider than the 48px cell", localMachineCell, w)
	}
	if !strings.Contains(visibleText(histView), localMachineName) {
		t.Fatalf("history page no longer names %s:\n%s", localMachineName, visibleText(histView))
	}
}

// personaLabelWidth mirrors web/src/lib/extension-ui.ts labelWidth(text, px).
func personaLabelWidth(text string, px float64) float64 {
	var w float64
	for _, r := range text {
		if (r >= 0x2e80 && r <= 0x9fff) || (r >= 0xac00 && r <= 0xd7af) || (r >= 0xff00 && r <= 0xffef) {
			w += px
		} else {
			w += px * 0.6
		}
	}
	return w
}

// personaThinLabels mirrors web/src/lib/extension-ui.ts thinLabels(labels, xs, width, px, gap).
func personaThinLabels(labels []string, xs []float64, width, px, gap float64) []int {
	n := len(labels)
	if n == 0 {
		return nil
	}
	span := func(i int) [2]float64 {
		w := personaLabelWidth(labels[i], px)
		a := math.Min(math.Max(0, xs[i]-w/2), math.Max(0, width-w))
		return [2]float64{a, a + w}
	}
	fits := func(list []int) bool {
		for j := 1; j < len(list); j++ {
			if span(list[j-1])[1]+gap > span(list[j])[0] {
				return false
			}
		}
		return true
	}
	for k := 1; k <= n; k++ {
		var list []int
		for i := 0; i < n; i += k {
			list = append(list, i)
		}
		if !fits(list) {
			continue
		}
		if list[len(list)-1] != n-1 {
			withLast := append(slices.Clone(list), n-1)
			if fits(withLast) {
				return withLast
			}
			swapped := append(slices.Clone(list[:len(list)-1]), n-1)
			if fits(swapped) {
				return swapped
			}
		}
		return list
	}
	return []int{n - 1}
}

func TestMobileAxisLabelsFit360pxPhoneAndPrivacy(t *testing.T) {
	// On a 360px phone screen, Persona web plotW = 360 - 32 (page px-4) - 32 (ChartFrame px-4) - axisW (28 to 33.2px)
	// = 262.8px to 268px (or ~256.8px with 5-char decimal ticks). Verify all 6 category X-axis labels fit without
	// stride thinning across both 255px and 262px mobile plot widths.
	const (
		axisFontPx    = 10.5
		minLabelGapPx = 8.0
	)
	labels := make([]string, 0, len(CategoryIDs))
	for _, cid := range CategoryIDs {
		labels = append(labels, CategoryAxisLabels[cid])
	}
	want := []int{0, 1, 2, 3, 4, 5}
	for _, mobilePlotWidth := range []float64{255.0, 262.0} {
		band := mobilePlotWidth / float64(len(labels))
		xs := make([]float64, len(labels))
		for i := range labels {
			xs[i] = band * (float64(i) + 0.5)
		}
		shown := personaThinLabels(labels, xs, mobilePlotWidth, axisFontPx, minLabelGapPx)
		if !slices.Equal(shown, want) {
			t.Fatalf("6 category X-axis labels %v thinned to %v at %.0fpx mobile plot width, want all %v",
				labels, shown, mobilePlotWidth, want)
		}

		// Also verify the 2-bar cleanImpactChart labels fit comfortably without thinning.
		planLabels := []string{"清理前", "Safe 清理后"}
		planBand := mobilePlotWidth / float64(len(planLabels))
		planXS := []float64{planBand * 0.5, planBand * 1.5}
		if got := personaThinLabels(planLabels, planXS, mobilePlotWidth, axisFontPx, minLabelGapPx); !slices.Equal(got, []int{0, 1}) {
			t.Fatalf("cleanImpactChart labels %v thinned to %v at %.0fpx, want [0 1]", planLabels, got, mobilePlotWidth)
		}
	}

	// Verify exact 80% / 90% / <80% thresholds across EvaluateHealth, machineProgressComponent, and rootUsageMeter.
	cases := []struct {
		name      string
		totalGiB  uint64
		usedPct   float64
		wantLevel string
		wantTone  string
		wantText  string
	}{
		{name: "below_80_ok", totalGiB: 200, usedPct: 79.5, wantLevel: HealthOK, wantTone: wire.TonePositive, wantText: "宽裕"},
		{name: "exact_80_warning", totalGiB: 200, usedPct: 80.0, wantLevel: HealthWarning, wantTone: wire.ToneCaution, wantText: "偏满"},
		{name: "89_5_warning", totalGiB: 200, usedPct: 89.5, wantLevel: HealthWarning, wantTone: wire.ToneCaution, wantText: "偏满"},
		{name: "exact_90_critical", totalGiB: 200, usedPct: 90.0, wantLevel: HealthCritical, wantTone: wire.ToneDanger, wantText: "快满了"},
	}
	for _, tc := range cases {
		tot := tc.totalGiB * gib
		used := uint64(math.Round(float64(tot) * tc.usedPct / 100.0))
		avail := tot - used
		lvl := EvaluateHealth(true, tot, avail)
		if lvl != tc.wantLevel {
			t.Fatalf("%s: EvaluateHealth = %q, want %q", tc.name, lvl, tc.wantLevel)
		}
		ms := MachineStatus{
			Machine:        &Machine{ID: "node", Name: "node"},
			Level:          lvl,
			UsedPercent:    tc.usedPct,
			TotalBytes:     tot,
			UsedBytes:      used,
			AvailableBytes: avail,
		}
		prog := machineProgressComponent(ms)
		wantDetail := fmt.Sprintf("%.1f%% · %s", tc.usedPct, tc.wantText)
		if prog.Tone != tc.wantTone || prog.Detail != wantDetail {
			t.Fatalf("%s: progress tone=%q detail=%q, want tone=%q detail=%q", tc.name, prog.Tone, prog.Detail, tc.wantTone, wantDetail)
		}
		rc := rootUsageMeter(ms, "test")
		if rc.Type != wire.ComponentProgress || rc.Tone != tc.wantTone || !strings.Contains(rc.Detail, tc.wantText) || rc.Key != "test-root-node" {
			t.Fatalf("%s: rootUsageMeter=%+v, want a bar with tone=%q and %q", tc.name, rc, tc.wantTone, tc.wantText)
		}
	}

	// Verify standalone disklessly-sessions page also passes strict View validation and leaks zero internal data.
	a := seedScannedThreeMachineFleet(t)
	sessPageView, err := a.renderSessions(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, TZ: "Asia/Shanghai"}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, sessPageView, false)
	assertNoPrivateDataInViewJSON(t, "disklessly-sessions", sessPageView)

	// Verify committed extension source files do not hardcode RFC 1918 or CGNAT private IPv4 literals.
	privateIPv4Literal := regexp.MustCompile(`\b(10\.\d{1,3}\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.\d{1,3}\.\d{1,3})\b`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if ent.IsDir() || (!strings.HasSuffix(ent.Name(), ".go") && !strings.HasSuffix(ent.Name(), ".json") && !strings.HasSuffix(ent.Name(), ".md")) {
			continue
		}
		b, err := os.ReadFile(ent.Name())
		if err != nil {
			t.Fatal(err)
		}
		if hit := privateIPv4Literal.FindString(string(b)); hit != "" {
			t.Fatalf("source file %s contains private/CGNAT IPv4 literal %q; use RFC 5737 TEST-NET addresses", ent.Name(), hit)
		}
	}
}

func TestDisjointComponentKeyNamespacesAndEmptyItemsDetailTable(t *testing.T) {
	noSandbox(t)
	a := newTestApp(t)
	ctx := context.Background()

	// Seed adversarial SSH host IDs that would collide if component prefixes were not disjoint
	// (e.g. "stats", "root-stats", "stats-local", "summary", "tbl"), plus a clean machine and a cold-only machine.
	if err := a.db().update(func(st *State, now time.Time) error {
		st.ensureDefaults(now)
		st.Scans["local"] = ParseProbeOutput(st.machine("local"), sampleProbeLocal, now)

		adversarial := []string{"stats", "root-stats", "root-local", "stats-local", "summary", "tbl"}
		for _, id := range adversarial {
			m, err := st.upsertMachine(MachineInput{ID: id, Host: id, Name: id, Source: "ssh-config"}, now)
			if err != nil {
				return err
			}
			st.Scans[m.ID] = ParseProbeOutput(m, sampleProbeARMBOX, now)
		}

		cleanM, err := st.upsertMachine(MachineInput{ID: "clean-node", Host: "clean-node", Name: "clean-node", Source: "manual"}, now)
		if err != nil {
			return err
		}
		st.Scans[cleanM.ID] = &Scan{
			MachineID: cleanM.ID,
			Host:      cleanM.Host,
			ScanMode:  "builtin-sh",
			Mount:     MountInfo{MountPoint: "/", FSType: "ext4", TotalBytes: 200 * gib, UsedBytes: 100 * gib, AvailableBytes: 100 * gib},
			ScannedAt: now,
		}

		coldOnlyM, err := st.upsertMachine(MachineInput{ID: "cold-only", Host: "cold-only", Name: "cold-only", Source: "manual"}, now)
		if err != nil {
			return err
		}
		st.Scans[coldOnlyM.ID] = &Scan{
			MachineID: coldOnlyM.ID,
			Host:      coldOnlyM.Host,
			ScanMode:  "builtin-sh",
			Mount:     MountInfo{MountPoint: "/", FSType: "ext4", TotalBytes: 200 * gib, UsedBytes: 170 * gib, AvailableBytes: 30 * gib},
			Sessions:  SessionSummary{ColdCount: 15, ColdBytes: 2 * gib},
			ScannedAt: now,
		}

		st.History = append(st.History, &CleanRecord{
			ID:          "tbl",
			MachineID:   "stats",
			MachineName: "stats",
			Host:        "stats",
			BeforeAvail: 8 * gib,
			AfterAvail:  20 * gib,
			FreedBytes:  12 * gib,
			Summary:     "8.0 GiB → 20.0 GiB (+12.0 GiB)",
			CreatedAt:   now,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Overview card with "stats" as highest-severity critical machine (featured = "stats", plus row "stats", "root-stats", etc.)
	cardView, err := a.card(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, Size: wire.SizeCompact, Placement: wire.PlacementOverviewCard}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, cardView, true)

	// All 4 tabs on adversarial machine IDs
	for _, machID := range []string{"stats", "root-stats", "stats-local", "clean-node", "cold-only"} {
		for _, tab := range []string{tabFleet, tabItems, tabPlan, tabSessions} {
			v, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{
				UIVersion: 8,
				State:     ext.EncodeState(page{Machine: machID, Tab: tab}),
			}})
			if err != nil {
				t.Fatalf("render machine=%s tab=%s: %v", machID, tab, err)
			}
			checkStrictView(t, v, false)
			assertMobileReadability360px(t, machID+"-"+tab, v)
		}
	}

	// Verify clean-node (0 items, 0 cold) and cold-only (0 cache items, coldPending=true) both render items-tbl-*
	for _, machID := range []string{"clean-node", "cold-only"} {
		v, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{
			UIVersion: 8,
			State:     ext.EncodeState(page{Machine: machID, Tab: tabItems}),
		}})
		if err != nil {
			t.Fatal(err)
		}
		if findComponentByKey(v, "prog-"+machID) == nil || findComponentByKey(v, "items-tbl-"+machID) == nil {
			t.Fatalf("%s: itemsTab with 0 cache items must render both prog-%s and items-tbl-%s", machID, machID, machID)
		}
	}
	coldOnlyItems, _ := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 8,
		State:     ext.EncodeState(page{Machine: "cold-only", Tab: tabItems}),
	}})
	if findComponentByKey(coldOnlyItems, "items-cats-cold-only") == nil {
		t.Fatal("cold-only machine should render items-cats-cold-only alongside items-tbl-cold-only")
	}

	// Verify cleanImpactChart warning -> ok transition colors pre-bar caution and post-bar positive
	msWarn := MachineStatus{
		Machine:        &Machine{ID: "warn-node", Name: "warn-node"},
		Level:          HealthWarning,
		UsedPercent:    85.0,
		TotalBytes:     200 * gib,
		UsedBytes:      170 * gib,
		AvailableBytes: 30 * gib,
		SafeBytes:      30 * gib,
	}
	scWarn := &Scan{
		MachineID: "warn-node",
		Mount:     MountInfo{MountPoint: "/", TotalBytes: 200 * gib, UsedBytes: 170 * gib, AvailableBytes: 30 * gib},
		SafeBytes: 30 * gib,
	}
	imp, ok := cleanImpactChart(msWarn, scWarn)
	if !ok || len(imp.Series) != 3 || imp.Series[0].Tone != wire.ToneCaution || imp.Series[1].Tone != wire.TonePositive {
		t.Fatalf("warning -> ok cleanImpactChart should use [caution, positive, neutral] series: %+v", imp)
	}
}

func TestUIVersion1PlainTextDegradation(t *testing.T) {
	a := seedScannedThreeMachineFleet(t)
	ctx := context.Background()

	v1Disallowed := []string{
		wire.ComponentChart,
		wire.ComponentStatGrid,
		wire.ComponentTable,
		wire.ComponentEmpty,
		wire.ComponentHeader,
		wire.ComponentCode,
	}

	for _, tab := range []string{tabFleet, tabItems, tabPlan, tabSessions} {
		v, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{
			UIVersion: 1,
			State:     ext.EncodeState(page{Machine: "local", Tab: tab}),
		}})
		if err != nil {
			t.Fatalf("UIVersion 1 tab %s: %v", tab, err)
		}
		checkStrictView(t, v, false)
		if v.Tabs != nil || v.Filter != nil || v.Search != nil || v.RefreshAfter != 0 {
			t.Fatalf("UIVersion 1 tab %s included v2+ top-level chrome: %+v", tab, v)
		}
		for _, s := range v.Sections {
			if s.Key != "" {
				t.Fatalf("UIVersion 1 tab %s included Section.Key %q", tab, s.Key)
			}
			for _, it := range s.Items {
				if slices.Contains(v1Disallowed, it.Type) {
					t.Fatalf("UIVersion 1 tab %s included %s component", tab, it.Type)
				}
				if it.Key != "" || it.Icon != "" {
					t.Fatalf("UIVersion 1 tab %s included v3+ key=%q or icon=%q", tab, it.Key, it.Icon)
				}
			}
		}
		if tab == tabFleet {
			all := texts(v)
			for _, want := range []string{"偏满", "快满了", "Go", "Docker"} {
				if !strings.Contains(all, want) {
					t.Fatalf("UIVersion 1 fleet text missing %q:\n%s", want, all)
				}
			}
		}
	}

	cardV1, err := a.card(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 1, Size: wire.SizeCompact}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, cardV1, true)
	for _, it := range cardV1.Sections[0].Items {
		if slices.Contains(v1Disallowed, it.Type) {
			t.Fatalf("UIVersion 1 card included %s", it.Type)
		}
	}

	sessPageV1, err := a.renderSessions(ctx, &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 1}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, sessPageV1, false)
	if sessPageV1.Filter != nil || sessPageV1.Search != nil || sessPageV1.RefreshAfter != 0 {
		t.Fatalf("UIVersion 1 sessions page included v2+ top-level chrome: %+v", sessPageV1)
	}
	for _, s := range sessPageV1.Sections {
		for _, it := range s.Items {
			if slices.Contains(v1Disallowed, it.Type) {
				t.Fatalf("UIVersion 1 sessions page included %s", it.Type)
			}
		}
	}
}

func TestLargeFleetStaysWithinViewItemBudget(t *testing.T) {
	noSandbox(t)
	a := newTestApp(t)
	if err := a.db().update(func(st *State, now time.Time) error {
		st.ensureDefaults(now)
		if local := st.machine("local"); local != nil {
			local.Name = "local (dev-workstation)"
		}
		st.Scans["local"] = ParseProbeOutput(st.machine("local"), sampleProbeLocal, now)
		for i := 1; i < maxMachines; i++ {
			id := fmt.Sprintf("node-%02d", i)
			m, err := st.upsertMachine(MachineInput{ID: id, Host: id, Name: id, Source: "manual"}, now)
			if err != nil {
				return err
			}
			st.Scans[m.ID] = ParseProbeOutput(m, sampleProbeLocal, now)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, uiVer := range []int{1, 2, 3, 6, 8} {
		v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: uiVer}})
		if err != nil {
			t.Fatalf("UIVersion %d: %v", uiVer, err)
		}
		checkStrictView(t, v, false)
		assertNoPrivateDataInViewJSON(t, fmt.Sprintf("large-fleet-v%d", uiVer), v)
	}
}
