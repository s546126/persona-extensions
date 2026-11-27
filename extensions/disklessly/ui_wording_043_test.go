package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// mixedFleet is both pages' state with five computers: the hub's own one ready (with several
// untitled conversations of one day), build-box without the tool or Homebrew (linux-arm64),
// nas-box without a network drive, studio-mac nearly full whose network drive holds no
// repository, and offline-box checked three days ago (1% used) and unreachable now.
func mixedFleet(t *testing.T) *app {
	t.Helper()
	noSandbox(t)
	a := newTestApp(t)
	var entries []map[string]any
	for i, hm := range []string{"01:12", "03:40", "06:05"} {
		entries = append(entries, map[string]any{"id": fmt.Sprintf("bbbbbbbbbbb%d", i), "local": true, "tool": "claude",
			"path": fmt.Sprintf("/home/dev/.claude/projects/x/%d.jsonl", i), "cwd": []string{"/home/dev/shop", "/home/dev/blog", "/home/dev/shop"}[i],
			"mtime": "2026-11-01T" + hm + ":00Z", "bytes": 4096 * (i + 1) * 37, "state": "archived", "segment": "seg-a"})
	}
	entries = append(entries, coldEntries...)
	a.runner = func(ctx context.Context, m *Machine, s string, d time.Duration) (string, error) {
		if strings.Contains(s, "sessions list --state all") {
			return coldRefreshOut(coldListJSON(entries...), coldStatusJSON), nil
		}
		return coldRunner(nil)(ctx, m, s, d)
	}
	down := false
	if err := a.db().update(func(st *State, now time.Time) error {
		st.ensureDefaults(now)
		st.Scans["local"] = ParseProbeOutput(st.machine("local"), sampleProbeLocal, now)
		for _, id := range []string{"build-box", "nas-box", "studio-mac", "offline-box"} {
			m, err := st.upsertMachine(MachineInput{ID: id, Host: id, Name: id, Source: "manual"}, now)
			if err != nil {
				return err
			}
			probe := sampleProbeLocal
			switch id {
			case "studio-mac":
				probe = sampleProbeARMBOX
			case "offline-box":
				probe = strings.Replace(sampleProbeLocal, "/dev/sda1   214748364800 182536110080  32212254720  85% /",
					"/dev/sda1   214748364800 2147483648  212600881152  1% /", 1)
			}
			st.Scans[id] = ParseProbeOutput(m, probe, now.Add(-72*time.Hour))
			if id == "offline-box" {
				m.LastError, m.Reachable = unreachableErrors[0].err, false
			}
		}
		if _, err := RefreshColdIndex(context.Background(), a.runner, st, st.machine("local"), now); err != nil {
			return err
		}
		base := *st.cold("local")
		set := func(id string, f func(c *ColdCatalog)) {
			c := base
			c.MachineID, c.Entries, c.Error, c.Backend.Error = id, nil, "", ""
			f(&c)
			st.setCold(id, &c)
		}
		set("build-box", func(c *ColdCatalog) { c.State, c.Brew, c.BrewKnown, c.Platform = ColdNoCLI, "", true, "linux-arm64" })
		set("nas-box", func(c *ColdCatalog) { c.State = ColdNoBackend })
		set("studio-mac", func(c *ColdCatalog) {
			c.State, c.Backend.Reachable, c.Backend.Error, c.Backend.ID = ColdBackendDown, &down, "repo.json is not in the folder", "nas-drive"
		})
		set("offline-box", func(c *ColdCatalog) { c.State, c.Error = ColdOffline, unreachableErrors[0].err })
		st.machine("build-box").Platform = "linux-arm64"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

func diskView(t *testing.T, a *app, machine, tab string) *wire.View {
	t.Helper()
	res, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8,
		State: ext.EncodeState(page{Machine: machine}), Action: "tab", Values: map[string]string{"tab": tab}}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, res.View, false)
	assertMobileReadability360px(t, machine+" "+tab, res.View)
	checkTapNames(t, machine+" "+tab, res.View)
	return res.View
}

func sessView(t *testing.T, a *app, machine string) *wire.View {
	t.Helper()
	v := sessionsSearch(t, a, machine, "")
	checkStrictView(t, v, false)
	checkTapNames(t, "AI 对话 "+machine, v)
	return v
}

// allViews draws every page and tab this change touches, with each computer selected.
func allViews(t *testing.T, a *app) map[string]*wire.View {
	out := map[string]*wire.View{}
	for _, m := range []string{"local", "build-box", "nas-box", "studio-mac", "offline-box"} {
		for _, tab := range []string{tabFleet, tabItems, tabPlan} {
			out["硬盘空间 "+tab+" "+m] = diskView(t, a, m, tab)
		}
		out["AI 对话 "+m] = sessView(t, a, m)
	}
	out["AI 对话 全部电脑"] = sessView(t, a, "")
	return out
}

func codeBlocks(v *wire.View) []wire.Component {
	var out []wire.Component
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Type == wire.ComponentCode {
				out = append(out, c)
			}
		}
	}
	return out
}

// 1. Every command block on both pages is headed 终端命令 (not 文件), keeps its exact text, and the
// raw output under 详情 is headed 原始信息.
func TestCommandBlocksAreLabelledAsCommands(t *testing.T) {
	a := mixedFleet(t)
	want := map[string]bool{}
	for _, cmd := range []string{
		noBackendCommand,
		"ssh -o BatchMode=yes -o ConnectTimeout=10 offline-box true",
		"disklessly backend list\ndisklessly backend use <后端 ID>   # 换到已经在用的那个网盘\ndisklessly init                    # 或者：在当前网盘里新建存放目录",
		brewSetupComment + "\n" + brewSetupCommand + "\n" + brewLinuxShellEnv + "\n" + brewInstallCmd,
	} {
		want[cmd] = false
	}
	for name, v := range allViews(t, a) {
		for _, c := range codeBlocks(v) {
			if !strings.HasPrefix(c.Key, "cmd-") {
				continue
			}
			if c.Path != commandBlockLabel || c.Language != "sh" {
				t.Errorf("%s: command block %q headed %q (%s)", name, c.Text, c.Path, c.Language)
			}
			if _, ok := want[c.Text]; ok {
				want[c.Text] = true
			}
		}
	}
	for cmd, seen := range want {
		if !seen {
			t.Errorf("command not drawn byte for byte: %q", cmd)
		}
	}
	// 详情 opens the raw output under its own label.
	res, err := a.actionSessions(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8,
		State: ext.EncodeState(sessPage{Machine: "studio-mac"}), Action: "details", Values: map[string]string{"k": detailsKey("cold", "studio-mac")}}})
	if err != nil {
		t.Fatal(err)
	}
	raw := 0
	for _, c := range codeBlocks(res.View) {
		if strings.HasPrefix(c.Key, "raw-") {
			raw++
			if c.Path != rawBlockLabel {
				t.Errorf("raw output headed %q", c.Path)
			}
		}
	}
	if raw != 1 {
		t.Fatalf("详情 drew %d raw blocks", raw)
	}
	// An app before code components still gets the command as text, without the label.
	old, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 3, State: ext.EncodeState(page{Machine: "offline-box"})}})
	if err != nil {
		t.Fatal(err)
	}
	if len(codeBlocks(old)) != 0 || !strings.Contains(texts(old), "ssh -o BatchMode=yes -o ConnectTimeout=10 offline-box true") || strings.Contains(texts(old), commandBlockLabel) {
		t.Fatalf("UI version 3: command not drawn as plain text:\n%s", texts(old))
	}
}

// 2. Untitled conversations of one day read apart: the title has the time, the row the working
// folder and the size; a title from the first message stays as it was.
func TestUntitledConversationsReadApart(t *testing.T) {
	a := mixedFleet(t)
	v := sessView(t, a, "local")
	titles := map[string]bool{}
	untitled := 0
	for _, r := range conversationRows(v) {
		if titles[r.Title+"|"+r.Subtitle] {
			t.Errorf("two rows read the same: %q / %q", r.Title, r.Subtitle)
		}
		titles[r.Title+"|"+r.Subtitle] = true
		if !strings.HasPrefix(r.Title, untitledText) {
			if !clockRE.MatchString(r.Subtitle) {
				t.Errorf("titled row lost its date and time: %q", r.Subtitle)
			}
			continue
		}
		untitled++
		if !strings.Contains(r.Title, "11月1日 ") || !strings.Contains(r.Title, ":") {
			t.Errorf("untitled row without its time: %q", r.Title)
		}
		if !strings.Contains(r.Subtitle, "KiB") || (!strings.Contains(r.Subtitle, "shop") && !strings.Contains(r.Subtitle, "blog")) {
			t.Errorf("untitled row without folder or size: %q", r.Subtitle)
		}
		if strings.Contains(r.Subtitle, "2026-11-01") {
			t.Errorf("the date is said twice: %q / %q", r.Title, r.Subtitle)
		}
	}
	if untitled != 3 {
		t.Fatalf("%d untitled rows, want 3", untitled)
	}
	// TZ: the title's time is the viewer's.
	d := draw{now: t0, loc: time.FixedZone("CST", 8*3600)}
	if got, _ := d.coldTitle(ColdEntry{Tool: "claude", Mtime: "2026-11-01T01:12:00Z"}); got != "没有标题的对话 · 11月1日 09:12 · Claude Code" {
		t.Fatalf("title %q", got)
	}
	// A folder other than the project is named; the same one is not repeated.
	row := d.coldRow(ColdHit{ColdEntry: ColdEntry{Title: "Fix it", Project: "shop", Cwd: "/home/dev/shop-v2", Bytes: 2048, Local: true}, MachineName: "build-box"})
	if !strings.Contains(row.Subtitle, "shop · 文件夹 shop-v2") || !strings.Contains(row.Subtitle, "2.0 KiB") {
		t.Fatalf("subtitle %q", row.Subtitle)
	}
	if row := d.coldRow(ColdHit{ColdEntry: ColdEntry{Title: "Fix it", Cwd: "/home/dev/shop"}, MachineName: "build-box"}); strings.Contains(row.Subtitle, "文件夹") {
		t.Fatalf("folder said twice: %q", row.Subtitle)
	}
}

// 3. The page says 还没核对过 for LocUnknown everywhere (stat, badges, sentences); MCP answers and
// tool descriptions keep 暂时不确定; search behaves as before.
func TestOneWordForUncheckedConversations(t *testing.T) {
	a := mixedFleet(t)
	unchecked := 0
	for name, v := range allViews(t, a) {
		if txt := visibleText(v); strings.Contains(txt, LocUnknown) {
			t.Errorf("%s still says %q:\n%s", name, LocUnknown, txt)
		}
		for _, r := range conversationRows(v) {
			if r.Badge != nil && r.Badge.Text == uncheckedText {
				unchecked++
				if r.Badge.Tone != wire.ToneCaution {
					t.Errorf("%s: unchecked badge tone %q", name, r.Badge.Tone)
				}
			}
		}
	}
	if unchecked == 0 {
		t.Fatal("no row badged 还没核对过")
	}
	// studio-mac has no conversation in its network drive: its line does not claim any is
	// shown as 还没核对过 (see TestDriveProblemSaysHowManyAreUnchecked).
	if !strings.Contains(visibleText(sessView(t, a, "studio-mac")), "没有对话受影响") {
		t.Error("studio-mac's line does not say none is affected")
	}
	// MCP: values and descriptions unchanged.
	srv := &mcpServer{store: a.db(), runner: a.runner}
	out, err := srv.call(context.Background(), "disklessly_sessions_search", json.RawMessage(`{"query":"flaky"}`))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out); !strings.Contains(string(b), `"`+LocUnknown+`"`) || strings.Contains(string(b), uncheckedText) {
		t.Fatalf("MCP search location changed: %s", b)
	}
	for _, tl := range tools {
		if (tl.name == "disklessly_sessions_search" || tl.name == "disklessly_sessions_status") && !strings.Contains(tl.description, LocUnknown) {
			t.Errorf("%s description lost %q", tl.name, LocUnknown)
		}
	}
	// Search matches what it matched before: the place is not searched, by either word.
	for _, q := range []string{LocUnknown, uncheckedText} {
		if n := len(conversationRows(sessionsSearch(t, a, "", q))); n != 0 {
			t.Errorf("search %q: %d rows", q, n)
		}
	}
	if n := len(conversationRows(sessionsSearch(t, a, "", "flaky"))); n != 1 {
		t.Errorf("search flaky: %d rows", n)
	}
}

// 4. A computer that cannot be reached: its bar and badge show only how full its disk was at
// the last check (marked 上次, with the date on its row); reachability is a neutral badge, an
// icon and words, and the fullness stat counts it by its disk only.
func TestUnreachableComputerIsNotAFullnessWarning(t *testing.T) {
	a := mixedFleet(t)
	v := diskView(t, a, "local", tabFleet)
	prog := findComponentByKey(v, "prog-offline-box")
	row := findComponentByKey(v, "mach-offline-box")
	if prog == nil || row == nil {
		t.Fatal("offline-box has no row or bar")
	}
	if prog.Tone != wire.TonePositive || !strings.HasPrefix(prog.Detail, "上次 ") || !strings.Contains(prog.Detail, "宽裕") {
		t.Errorf("last-known bar: tone %q detail %q", prog.Tone, prog.Detail)
	}
	if row.Badge == nil || row.Badge.Tone != wire.TonePositive || !strings.HasPrefix(row.Badge.Text, "上次 宽裕") || strings.Contains(row.Badge.Text, "连不上") {
		t.Errorf("badge next to the bar: %+v", row.Badge)
	}
	if row.Icon != "wifi" || !strings.HasPrefix(row.Subtitle, "刚刚检查时连不上这台电脑 · 下面是 3 天前检查时的数") {
		t.Errorf("row: icon %q subtitle %q", row.Icon, row.Subtitle)
	}
	for _, st := range v.Sections[1].Items[0].Stats {
		if st.Label == "硬盘用了多少" && (st.Value != "1 台快满了" || !strings.Contains(st.Detail, "1 台连不上")) {
			t.Errorf("fullness stat: %+v", st)
		}
	}
	// Its own tab: two badges, fullness and reachability, and the caption says 上次检查时.
	items := diskView(t, a, "offline-box", tabItems)
	var header *wire.Component
	for _, s := range items.Sections {
		for i, c := range s.Items {
			if c.Type == wire.ComponentHeader {
				header = &s.Items[i]
			}
		}
	}
	if header == nil || len(header.Badges) != 2 || header.Badges[1] != *offlineBadge() || header.Badges[0].Tone != wire.TonePositive {
		t.Fatalf("header: %+v", header)
	}
	if tbl := findComponentByKey(items, "items-tbl-offline-box"); tbl == nil || !strings.HasPrefix(tbl.Caption, "上次检查时，") {
		t.Fatalf("caption: %+v", tbl)
	}
	// Never checked and unreachable: a neutral badge, and the stat does not call it full.
	ms := MachineStatus{Machine: &Machine{ID: "x"}, Level: HealthOffline}
	if b := healthBadge(ms); b.Tone != wire.ToneNeutral || b.Text != "连不上这台电脑" {
		t.Errorf("never-checked offline badge: %+v", b)
	}
	if text, tone, n := fleetAlertSummary([]MachineStatus{ms}); tone != wire.ToneNeutral || n != 1 || strings.Contains(text, "满") {
		t.Errorf("fleet stat for one unreachable computer: %q %q %d", text, tone, n)
	}
	// A last-known critical disk still reads critical, marked 上次.
	crit := MachineStatus{Machine: &Machine{ID: "y"}, Level: HealthOffline, TotalBytes: 100 * gib, UsedBytes: 95 * gib, AvailableBytes: 5 * gib, UsedPercent: 95}
	if b := healthBadge(crit); b.Tone != wire.ToneDanger || b.Text != "上次 快满了 95%" {
		t.Errorf("last-known critical badge: %+v", b)
	}
}

// 5. Kinds are named plainly where a line has room; the short labels of the chart and the
// narrow tables are spelled out in their legend; platforms read Mac / Linux 电脑.
func TestKindsAndPlatformsInPlainWords(t *testing.T) {
	a := mixedFleet(t)
	items := diskView(t, a, "local", tabItems)
	txt := visibleText(items)
	for _, w := range []string{"删掉：Node 和 Python 下载的包", "删掉：Rust 和 Xcode 的编译缓存"} {
		if !strings.Contains(txt, w) {
			t.Errorf("items tab lacks %q", w)
		}
	}
	// One set of short names, spelled out once: in the chart, the first to use them.
	for _, key := range []string{"items-cats-local", "items-tbl-local", "items-detail-tbl-local"} {
		c := findComponentByKey(items, key)
		if c == nil {
			t.Fatalf("%s missing", key)
		}
		legend := c.Detail + c.Caption
		has := strings.Contains(legend, "「Node/Py」是 Node 和 Python 下载的包") && strings.Contains(legend, "「Rust/XC」是 Rust 和 Xcode 的编译缓存")
		if has != (key == "items-cats-local") {
			t.Errorf("%s legend %v: %q", key, has, legend)
		}
	}
	if n := strings.Count(txt, "简称："); n != 1 {
		t.Errorf("items tab spells the short names out %d times", n)
	}
	for _, s := range items.Sections {
		for _, c := range s.Items {
			if c.Type == wire.ComponentRow && (strings.Contains(c.Title, "Node/Py") || strings.Contains(c.Title, "Rust/XC") || strings.Contains(c.Title, "Node / Py")) {
				t.Errorf("row title with an abbreviation: %q", c.Title)
			}
		}
	}
	if got := categoryLegend(CategoryAxisLabels, []string{CatGoCache}); got != "" {
		t.Errorf("legend for Go only: %q", got)
	}
	// MCP keeps its category names.
	if CategoryLabels[CatNodePythonCache] != "Node / Python 下载的包" {
		t.Error("CategoryLabels changed")
	}
	for p, want := range map[string]string{"linux-arm64": "Linux 电脑（ARM 芯片）", "linux-amd64": "Linux 电脑（Intel 或 AMD 芯片）",
		"darwin-arm64": "Mac（Apple 芯片）", "darwin-amd64": "Mac（Intel 芯片）", "": "", "freebsd-amd64": "freebsd-amd64"} {
		if got := platformLabel(p); got != want {
			t.Errorf("platformLabel(%q) = %q, want %q", p, got, want)
		}
	}
	for name, v := range allViews(t, a) {
		if strings.Contains(visibleText(v), "linux-arm64") {
			t.Errorf("%s shows the raw platform", name)
		}
	}
	if !strings.Contains(visibleText(sessView(t, a, "build-box")), "Linux 电脑（ARM 芯片）") {
		t.Error("build-box's platform not worded")
	}
}

// 6. The selected computer's system disk is its one bar (no tall one-bar chart next to it); the
// plan tab without a before/after chart and the overview card draw it as a bar.
func TestSystemDiskIsACompactBar(t *testing.T) {
	a := mixedFleet(t)
	for name, v := range allViews(t, a) {
		for _, s := range v.Sections {
			for _, c := range s.Items {
				if c.Type == wire.ComponentChart && len(c.Series) > 0 && len(c.Series[0].Points) == 1 {
					t.Errorf("%s: one-bar chart %q", name, c.Key)
				}
			}
		}
	}
	fleet := diskView(t, a, "local", tabFleet)
	tbl := findComponentByKey(fleet, "fleet-tbl-local")
	if findComponentByKey(fleet, "prog-local") == nil || tbl == nil ||
		!strings.Contains(tbl.Caption, "用了 170.0 GiB（85.0%，偏满）· 还剩 30.0 GiB / 共 200.0 GiB · "+fullnessLegend) {
		t.Fatalf("the system disk's numbers are gone: %+v", tbl)
	}
	// Nothing safe to delete: the plan tab shows the bar.
	if err := a.db().update(func(st *State, now time.Time) error {
		st.Scans["local"].SafeBytes = 0
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if c := findComponentByKey(diskView(t, a, "local", tabPlan), "plan-root-local"); c == nil || c.Type != wire.ComponentProgress || c.Ratio == nil {
		t.Fatalf("plan tab bar: %+v", c)
	}
	card, err := a.card(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, Size: wire.SizeCompact, Placement: wire.PlacementOverviewCard}})
	if err != nil {
		t.Fatal(err)
	}
	checkStrictView(t, card, true)
	if c := findComponentByKey(card, "card-root-studio-mac"); c == nil || c.Type != wire.ComponentProgress || !strings.HasPrefix(c.Label, "studio-mac · ") || c.Tone != wire.ToneDanger {
		t.Fatalf("card bar: %+v", c)
	}
}

// 7. A computer that is not ready leads with the button that fixes it when the page has one;
// a step only a terminal can do says so in one sentence, and nothing promises the assistant.
func TestNotReadyLeadsSayWhenOnlyATerminalCan(t *testing.T) {
	a := mixedFleet(t)
	const terminalOnly = "这一步页面上没有按钮能代劳，只能在"
	for _, id := range []string{"build-box", "nas-box", "studio-mac"} {
		v := sessView(t, a, id)
		lead := rowSays(itemsOf(v, id))
		if strings.Count(lead, terminalOnly) != 1 || !strings.Contains(lead, "点「重新检查 "+id+"」") {
			t.Errorf("%s: %q", id, lead)
		}
		if buttonIn(itemsOf(v, id), "重新检查 "+id) == nil {
			t.Errorf("%s: the button the line names is not there", id)
		}
		if strings.Contains(visibleText(v), "助手帮") || strings.Contains(visibleText(v), "让 Persona") {
			t.Errorf("%s: promises the assistant", id)
		}
	}
	// With Homebrew, or another computer to copy from, the line points to that button first.
	if err := a.db().update(func(st *State, now time.Time) error {
		c := *st.cold("build-box")
		c.Brew = "/home/linuxbrew/.linuxbrew/bin/brew"
		st.setCold("build-box", &c)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	v := sessView(t, a, "build-box")
	lead := rowSays(itemsOf(v, "build-box"))
	if strings.Contains(lead, terminalOnly) || !strings.Contains(lead, "不用开终端：点「"+brewButtonTitle(false)+"」") || buttonIn(itemsOf(v, "build-box"), brewButtonTitle(false)) == nil {
		t.Errorf("Homebrew: %q", lead)
	}
}
