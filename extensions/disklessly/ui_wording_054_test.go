package main

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// 0.5.4: fullness words match the legend, what cannot be done now is apart, one size format,
// one name per kind of item, and a mixed 电脑 stat is not all alarm.

// The words and the wording of 0.3.x scans, which a hub keeps until the next check.
const (
	oldPlaywrightTitle  = "Playwright 浏览器缓存 (ms-playwright)"
	oldPlaywrightReason = "无头浏览器镜像，清理后首次运行需重新下载"
)

func renameItem(sc *Scan, id, title, reason string) {
	for i := range sc.Items {
		if sc.Items[i].ID == id {
			sc.Items[i].Title, sc.Items[i].Reason = title, reason
		}
	}
}

func statOf(v *wire.View, label string) *wire.Stat {
	for _, s := range v.Sections {
		for _, c := range s.Items {
			for i := range c.Stats {
				if c.Stats[i].Label == label {
					return &c.Stats[i]
				}
			}
		}
	}
	return nil
}

// viewText is every string a person reads on v.
func viewText(v *wire.View) string { return dumpView(v) }

// Item 1: the legend names the same bands, with the same words, as the badges and bars.
func TestFullnessLegendMatchesBands(t *testing.T) {
	for _, legend := range []string{fullnessLegendShort, fullnessLegend} {
		for _, w := range []string{"80%", "90%", healthLevelText(HealthWarning), healthLevelText(HealthCritical)} {
			if !strings.Contains(legend, w) {
				t.Errorf("legend %q lacks %q", legend, w)
			}
		}
	}
	for _, w := range []string{"25 GiB", "10 GiB"} {
		if !strings.Contains(fullnessLegend, w) {
			t.Errorf("legend %q lacks the free-space floor %q", fullnessLegend, w)
		}
	}
	// The bands themselves (EvaluateHealth), as the legend states them.
	cases := []struct {
		total, avail uint64
		want         string
	}{
		{100 * gib, 30 * gib, HealthOK},        // 70%
		{100 * gib, 20 * gib, HealthWarning},   // 80%
		{100 * gib, 11 * gib, HealthWarning},   // 89%
		{100 * gib, 10 * gib, HealthCritical},  // 90%
		{1000 * gib, 200 * gib, HealthWarning}, // 80% of a big disk
		{49 * gib, 5 * gib, HealthCritical},    // 89.8%, under 10 GiB
		{126 * gib, 20 * gib, HealthWarning},   // 84%, under 25 GiB
	}
	for _, c := range cases {
		if got := EvaluateHealth(true, c.total, c.avail); got != c.want {
			t.Errorf("EvaluateHealth(%d GiB, %d GiB free) = %s, want %s", c.total/gib, c.avail/gib, got, c.want)
		}
	}

	// 88% put in 快满了 by its free space says why on its bar; so does 67% put in 偏满; 84% is 偏满
	// by its percentage alone.
	for _, c := range []struct {
		total, avail uint64
		want         string
	}{
		{49 * gib, 5 * gib, "快满了，只剩 5.0 GiB，不到 10 GiB"},
		{60 * gib, 20 * gib, "偏满，只剩 20.0 GiB，不到 25 GiB"},
		{126 * gib, 20 * gib, "84.1% · 偏满"},
		{100 * gib, 5 * gib, "· 快满了"},
	} {
		used := c.total - c.avail
		ms := MachineStatus{Machine: &Machine{ID: "build-box"}, TotalBytes: c.total, AvailableBytes: c.avail, UsedBytes: used,
			UsedPercent: float64(used) * 100 / float64(c.total), Level: EvaluateHealth(true, c.total, c.avail)}
		if d := machineProgressComponent(ms).Detail; !strings.HasSuffix(d, c.want) {
			t.Errorf("bar of %d GiB with %d GiB free: %q, want …%q", c.total/gib, c.avail/gib, d, c.want)
		}
	}

	a := dumpFleet(t)
	fleet := diskView(t, a, "local", tabFleet)
	if s := statOf(fleet, "硬盘用了多少"); s == nil || !strings.HasPrefix(s.Detail, fullnessLegendShort) {
		t.Errorf("the fullness stat's legend: %+v", s)
	}
	views := allViews(t, a)
	views["card"] = cardView(t, a)
	for name, v := range views {
		text := viewText(v) + v.Fallback
		for _, w := range []string{"告急", "80% 提醒"} {
			if strings.Contains(text, w) {
				t.Errorf("%s still says %q", name, w)
			}
		}
	}
	if strings.Contains(views["card"].Fallback, "提醒") {
		t.Errorf("card fallback: %q", views["card"].Fallback)
	}
}

func cardView(t *testing.T, a *app) *wire.View {
	t.Helper()
	v, err := a.card(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Item 2a: the 可以腾出 stat counts what can be done now; the rest is said apart.
func TestReclaimStatLeavesOutUnreachable(t *testing.T) {
	a := dumpFleet(t)
	var now, safe, off uint64
	offN := 0
	if err := a.db().read(func(st *State) {
		for _, ms := range st.statuses() {
			if ms.Machine.Reachable {
				now += ms.ReclaimableBytes
				safe += ms.SafeBytes
			} else if ms.ReclaimableBytes > 0 {
				off += ms.ReclaimableBytes
				offN++
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if off == 0 || offN != 2 {
		t.Fatalf("fixture: %d unreachable computers with %d bytes", offN, off)
	}
	fleet := diskView(t, a, "local", tabFleet)
	s := statOf(fleet, "可以腾出")
	want := "其中放心删 " + FormatBytes(safe) + "；另有 " + FormatBytes(off) + " 在连不上的 2 台电脑上，按上次检查的数"
	if s == nil || s.Value != FormatBytes(now) || s.Detail != want {
		t.Errorf("stat %+v, want %s (%s)", s, FormatBytes(now), want)
	}
	card := cardView(t, a)
	if s := statOf(card, "可以腾出"); s == nil || s.Value != FormatBytes(now) || !strings.Contains(s.Detail, "连不上的 2 台") {
		t.Errorf("card stat %+v", s)
	}
	if !strings.Contains(card.Fallback, "现在一共可以腾出 "+FormatBytes(now)) {
		t.Errorf("card fallback %q", card.Fallback)
	}
}

// Item 2b: on the page as in the tool, the steps that can be done now come first.
func TestAdviceDoableStepsFirstOnPage(t *testing.T) {
	a := dumpFleet(t) // studio-mac, the fullest computer, cannot be reached
	var adv *advice
	if err := a.db().read(func(st *State) { adv, _ = buildAdvice(st, a.now(), adviceOptions{}) }); err != nil {
		t.Fatal(err)
	}
	seenBlocked := false
	for _, s := range adv.Steps {
		if !s.Possible {
			seenBlocked = true
		} else if seenBlocked {
			t.Fatalf("tool: %s/%s can be done now but comes after a step that cannot", s.MachineID, s.Kind)
		}
	}
	if !seenBlocked || adv.Steps[0].MachineID == "studio-mac" {
		t.Fatalf("fixture: first step %+v", adv.Steps[0])
	}
	fleet := diskView(t, a, "local", tabFleet)
	sec := sectionByKey(fleet, "advice")
	rows := 0
	for _, c := range sec.Items {
		if strings.HasPrefix(c.Key, "advice-") && c.Key != "advice-assistant" {
			rows++
			if strings.Contains(c.Subtitle, "现在还做不了") {
				t.Errorf("top step that cannot be done now: %q", c.Title)
			}
		}
	}
	if rows != adviceShown {
		t.Errorf("%d top steps", rows)
	}
	all := findComponentByKey(fleet, "steps-all")
	pushed, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8, State: all.Action.Push.State}})
	if err != nil {
		t.Fatal(err)
	}
	now, later := sectionByKey(pushed, "advice-all-steps"), sectionByKey(pushed, "advice-all-later")
	if now == nil || later == nil || !strings.Contains(now.Title, "现在就能做的") || !strings.HasPrefix(later.Title, "现在还做不了的") {
		t.Fatalf("headings: %+v / %+v", now, later)
	}
	// The page's order is the tool's.
	i := 0
	for _, c := range append(now.Items, later.Items...) {
		s := adv.Steps[i]
		if !strings.Contains(c.Title, s.MachineName+"：") || c.Key != boundedKey("advice-"+strconv.Itoa(s.Rank)+"-", s.MachineID) {
			t.Errorf("row %d %q is not step %d of %s", i, c.Title, s.Rank, s.MachineID)
		}
		i++
	}
	// The order is told to the assistant.
	var desc string
	for _, tl := range tools {
		if tl.name == "disklessly_advise" {
			desc = tl.description
		}
	}
	if !strings.Contains(desc, "现在就能做的在前") {
		t.Errorf("advise description does not tell the order: %q", desc)
	}
}

// Item 3: one size format, never four digits before the point.
func TestFormatBytesSwitchesUnitBefore1000(t *testing.T) {
	for b, want := range map[uint64]string{
		0:                   "0 B",
		999:                 "999 B",
		1000:                "1.0 KiB",
		1023:                "1.0 KiB",
		1024:                "1.0 KiB",
		999 * 1024:          "999.0 KiB",
		1000 * 1024:         "1.0 MiB",
		1023 * mib:          "1.0 GiB",
		1022*mib + 900*1024: "1.0 GiB",
		999 * mib:           "999.0 MiB",
		999*mib + 900*1024:  "999.9 MiB",
		999*mib + 1000*1024: "1.0 GiB",
		5 * gib / 2:         "2.5 GiB",
		999 * gib:           "999.0 GiB",
		1000 * gib:          "0.98 TiB",
		1024 * gib:          "1.00 TiB",
		3 * 1024 * gib:      "3.00 TiB",
	} {
		if got := FormatBytes(b); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", b, got, want)
		}
	}
	four := regexp.MustCompile(`\d{4}\.\d [KMG]iB`)
	for name, v := range allViews(t, dumpFleet(t)) {
		if m := four.FindString(viewText(v)); m != "" {
			t.Errorf("%s: %q", name, m)
		}
	}
}

// Item 3: a computer's row says how much of what it can free is 放心删 (its button's number).
func TestMachineRowSaysSafePart(t *testing.T) {
	for _, c := range []struct {
		all, safe uint64
		want      string
	}{
		{20 * gib, 19 * gib, "可以腾出 20.0 GiB，其中放心删 19.0 GiB"},
		{1023 * mib, 1023*mib - 100*1024, "可以腾出 1.0 GiB，几乎都是放心删的"},
		{4 * gib, 4 * gib, "可以腾出 4.0 GiB"},
		{0, 0, "可以腾出 0 B"},
	} {
		if got := reclaimWords(c.all, c.safe); got != c.want {
			t.Errorf("reclaimWords(%d, %d) = %q, want %q", c.all, c.safe, got, c.want)
		}
	}
	fleet := diskView(t, dumpFleet(t), "local", tabFleet)
	row := findComponentByKey(fleet, "mach-build-box")
	if row == nil || !strings.Contains(row.Subtitle, "可以腾出 20.9 GiB，其中放心删 19.7 GiB") {
		t.Errorf("build-box row: %+v", row)
	}
	if b := buttonIn(sectionByTitle(fleet, "build-box").Items, "删掉放心删的缓存（19.7 GiB）"); b == nil {
		t.Errorf("build-box button")
	}
}

func sectionByTitle(v *wire.View, title string) *wire.Section {
	for i := range v.Sections {
		if v.Sections[i].Title == title {
			return &v.Sections[i]
		}
	}
	return &wire.Section{}
}

// Item 4: an item stored by an older version shows this version's name and description.
func TestItemWordsOneNamePerKind(t *testing.T) {
	ids := regexp.MustCompile(`(?m)^(?:probe_dir "|\s*printf ')([a-z-]+)["\\]`).FindAllStringSubmatch(ProbeScript, -1)
	n := 0
	for _, m := range ids {
		if m[1] == "tool" || m[1] == "archive" || m[1] == "docker-buildx" {
			continue
		}
		n++
		if w, ok := itemWords[m[1]]; !ok || w.title == "" || w.reason == "" {
			t.Errorf("no current words for %q", m[1])
		}
	}
	if n < 15 || itemWords["docker-buildx"].title != buildCacheTitle || itemWords["journalctl-vacuum"].title != journalTitle {
		t.Errorf("%d ids; %+v", n, itemWords)
	}
	// Every item a fresh scan makes already has these words.
	for _, it := range ParseProbeOutput(&Machine{ID: "local", IsLocal: true}, sampleProbeLocal, t0).Items {
		if itemTitle(it) != strings.TrimSpace(it.Title) || itemReason(it) != strings.TrimSpace(it.Reason) {
			t.Errorf("%s: %q / %q against %q / %q", it.ID, itemTitle(it), itemReason(it), it.Title, it.Reason)
		}
	}
	old := ScanItem{ID: "sessions-cold-claude", Category: CatColdAgentSession, Title: "Claude Code 冷会话日志 (3 个 >7天会话)", SessionCnt: 3, Reason: "由 disklessly 上传到备份后端"}
	if itemTitle(old) != "Claude Code 一周没动的对话（3 个）" || itemReason(old) != coldItemReason {
		t.Errorf("old conversation item: %q / %q", itemTitle(old), itemReason(old))
	}
	if got := itemTitle(ScanItem{ID: "something-new", Title: " 新东西 "}); got != "新东西" {
		t.Errorf("unknown item: %q", got)
	}

	a := dumpFleet(t) // nas-box's check has the old Playwright words
	views := allViews(t, a)
	pushed, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{UIVersion: 8,
		State: findComponentByKey(views["硬盘空间 "+tabFleet+" local"], "steps-all").Action.Push.State}})
	if err != nil {
		t.Fatal(err)
	}
	views["every step"] = pushed
	for name, v := range views {
		text := viewText(v)
		if strings.Contains(text, oldPlaywrightTitle) || strings.Contains(text, "无头") || strings.Contains(text, "(ms-playwright)") {
			t.Errorf("%s shows the old words", name)
		}
	}
	look := sectionByKey(pushed, "advice-all-look")
	nas := 0
	for _, c := range look.Items {
		if strings.Contains(c.Title, "nas-box") {
			nas++
			if !strings.Contains(c.Title, "「Playwright 自动测试用的浏览器」") || !strings.HasPrefix(c.Subtitle, "自动测试用的几个浏览器") {
				t.Errorf("nas-box item: %q · %q", c.Title, c.Subtitle)
			}
		}
	}
	if nas != 1 {
		t.Errorf("nas-box look-first rows: %d", nas)
	}
	if v := views["硬盘空间 "+tabItems+" nas-box"]; !strings.Contains(viewText(v), "Playwright 自动测试用的浏览器") {
		t.Errorf("nas-box items tab lacks the current name")
	}
}

// Item 5: a 电脑 stat that counts some fine computers is a caution, not an alarm.
func TestSessionsMachineStatTone(t *testing.T) {
	v := sessView(t, dumpFleet(t), "")
	s := statOf(v, "电脑")
	if s == nil || !strings.Contains(s.Detail, "台正常") || s.Tone != wire.ToneCaution {
		t.Errorf("mixed 电脑 stat: %+v", s)
	}
	for _, c := range []struct {
		ready, total int
		want         string
	}{{0, 3, "3 台要处理"}, {3, 3, "都正常"}, {2, 5, "2 台正常 · 3 台要处理"}} {
		if got := machineCounts(c.ready, c.total); got != c.want {
			t.Errorf("machineCounts(%d, %d) = %q", c.ready, c.total, got)
		}
	}
}
