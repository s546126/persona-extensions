package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// uploadFleet is a local computer whose CLI is ready, build-box without a CLI and nas-box without
// a backend; apply answers the upload run and gate, when set, holds it until closed.
type uploadFleet struct {
	a     *app
	apply string
	err   error
	gate  chan struct{}
}

func newUploadFleet(t *testing.T) *uploadFleet {
	t.Helper()
	f := &uploadFleet{a: newTestApp(t), apply: sampleSessionApply}
	f.a.runner = func(ctx context.Context, m *Machine, s string, d time.Duration) (string, error) {
		switch {
		case m.ID == "build-box" && strings.Contains(s, "sessions list --state all"):
			return "@@SECTION:COLD_UNAME\nLinux x86_64\n@@COLD_NOCLI\n", nil
		case m.ID == "nas-box" && strings.Contains(s, "sessions list --state all"):
			return coldRefreshOut(coldListJSON(), `{"ok":true,"backend":{"id":"","type":"","reachable":false},"counts":{},"segments":{}}`), nil
		case strings.Contains(s, "sessions --apply"):
			if f.gate != nil {
				<-f.gate
			}
			return f.apply, f.err
		}
		return coldRunner(nil)(ctx, m, s, d)
	}
	for _, h := range []string{"build-box", "nas-box"} {
		if err := f.a.db().update(func(st *State, now time.Time) error {
			_, err := st.upsertMachine(MachineInput{Host: h, Name: h}, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.act(t, "", "cold-refresh", map[string]string{"m": ""})
	f.a.waitJobs()
	return f
}

func (f *uploadFleet) act(t *testing.T, state, action string, values map[string]string) *wire.ActionResult {
	t.Helper()
	res, err := f.a.actionSessions(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{State: state, Action: action, Values: values}})
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("%s: CheckView: %v", action, err)
	}
	return res
}

// view draws AI 对话 with machine selected ("" for all computers).
func (f *uploadFleet) view(t *testing.T, machine string) *wire.View {
	t.Helper()
	return f.act(t, ext.EncodeState(sessPage{Machine: machine}), "search", map[string]string{"q": ""}).View
}

func offloadButtons(items []wire.Component) []wire.Component {
	var out []wire.Component
	for _, c := range items {
		if c.Type == wire.ComponentButton && c.Action != nil && c.Action.ID == "cold-offload" {
			out = append(out, c)
		}
	}
	return out
}

func allOffloadButtons(v *wire.View) []wire.Component {
	var out []wire.Component
	for _, s := range v.Sections {
		out = append(out, offloadButtons(s.Items)...)
	}
	return out
}

// F1: the upload is reachable from 全部电脑, offered only on a ready computer, and a computer
// that is not ready says why instead.
func TestUploadReachableFromAllComputersAndOnlyWhenReady(t *testing.T) {
	f := newUploadFleet(t)

	all := f.view(t, "")
	if b := offloadButtons(itemsOf(all, "local")); len(b) != 1 || b[0].Title != coldOffloadText || b[0].Action.Values["m"] != "local" {
		t.Fatalf("全部电脑: the ready computer has no upload button: %+v", itemsOf(all, "local"))
	}
	for _, id := range []string{"build-box", "nas-box"} {
		if b := offloadButtons(itemsOf(all, id)); len(b) != 0 {
			t.Fatalf("%s is not ready but offers the upload: %+v", id, b)
		}
	}
	if n := len(allOffloadButtons(all)); n != 1 {
		t.Fatalf("全部电脑: %d upload buttons, want the ready computer's only", n)
	}
	if txt := texts(all); !strings.Contains(txt, "1 台电脑可以搬") || !strings.Contains(txt, "其余 2 台还不能搬") {
		t.Fatalf("全部电脑 does not say where the upload is:\n%s", txt)
	}

	for id, why := range map[string]string{"build-box": coldStateLabel(ColdNoCLI), "nas-box": coldStateLabel(ColdNoBackend)} {
		v := f.view(t, id)
		if b := allOffloadButtons(v); len(b) != 0 {
			t.Fatalf("%s selected: upload offered although it is not ready: %+v", id, b)
		}
		if txt := texts(v); !strings.Contains(txt, "现在还不能把对话搬到网盘（"+why+"）") {
			t.Fatalf("%s selected: no reason given:\n%s", id, txt)
		}
	}
	nas := f.view(t, "nas-box")
	if !strings.Contains(fmt.Sprint(machineItems(nas)), "disklessly backend add rclone") {
		t.Fatal("nas-box: the command that sets up a network drive is missing")
	}

	// A stale tap on a computer known not to be ready is refused with the same reason, and starts nothing.
	_, err := f.a.actionSessions(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		State: ext.EncodeState(sessPage{Machine: "build-box"}), Action: "cold-offload", Values: map[string]string{"m": "build-box"}}})
	if err == nil || !strings.Contains(err.Error(), "现在还不能把对话搬到网盘") || !strings.Contains(err.Error(), "配套小工具") {
		t.Fatalf("upload on build-box: %v", err)
	}
	if j, ok := f.a.jobSnapshot()["build-box"]; ok && j.Kind == jobOffload {
		t.Fatalf("a refused upload started a job: %+v", j)
	}

	one := f.view(t, "local")
	if b := allOffloadButtons(one); len(b) != 1 || !strings.Contains(b[0].Action.Confirm, "读回来核对") ||
		!strings.Contains(b[0].Action.Confirm, "核对无误才删掉电脑上的那份") || !strings.Contains(b[0].Action.Confirm, "一个都不删") {
		t.Fatalf("one computer: upload button or its confirm: %+v", b)
	}
}

// F2: a failed upload offers a real retry, the text names that button, and a run on the hub's own
// computer is not called an SSH connection.
func TestFailedUploadOffersRetryAndNamesNoSSHForThisComputer(t *testing.T) {
	f := newUploadFleet(t)
	f.apply = "@@SESSIONS_RC 1\n@@SESSIONS_JSON {\"ok\":false,\"error\":{\"code\":1,\"message\":\"index locked\",\"name\":\"EX_SOFTWARE\"}}\n"
	f.act(t, ext.EncodeState(sessPage{Machine: "local"}), "cold-offload", map[string]string{"m": "local"})
	f.a.waitJobs()
	items := itemsOf(f.view(t, "local"), "local")
	b := offloadButtons(items)
	if len(b) != 1 || b[0].Title != coldRetryText || b[0].Action.Confirm != sessionOffloadConfirm(&Machine{Name: "local (x)", IsLocal: true}) {
		t.Fatalf("no retry button after a failed upload: %+v", items)
	}
	txt := fmt.Sprint(items)
	if !strings.Contains(txt, "没有成功") || !strings.Contains(txt, "「"+coldRetryText+"」") {
		t.Fatalf("the failure does not point at the retry button: %s", txt)
	}
	if !buttonsHave(items, "详情") {
		t.Fatal("the raw CLI answer is not behind 详情")
	}

	// The runner fails with an SSH-shaped error on the hub's own computer.
	f.apply, f.err = "", fmt.Errorf("SSH 连接 localhost 失败：exit status 255")
	f.act(t, ext.EncodeState(sessPage{Machine: "local"}), "cold-offload", map[string]string{"m": "local"})
	f.a.waitJobs()
	j := f.a.jobSnapshot()["local"]
	if j.Err == "" || strings.Contains(j.Err, "SSH") || strings.Contains(j.Err, "hub") || strings.Contains(j.Err, "远程登录localhost") {
		t.Fatalf("local failure is worded as SSH: %q", j.Err)
	}
	items = itemsOf(f.view(t, "local"), "local")
	if txt := fmt.Sprint(items); !strings.Contains(txt, "「"+coldRetryText+"」") || strings.Contains(txt, "点「重试」") {
		t.Fatalf("local failure: %s", txt)
	}
	if len(offloadButtons(items)) != 1 {
		t.Fatal("no retry after a run error")
	}
}

func buttonsHave(items []wire.Component, title string) bool {
	return buttonIn(items, title) != nil
}

// F3: an upload the CLI refuses because no backend is configured (exit 78) says so and gives the
// same fix as a computer whose check found no backend.
func TestUploadWithoutBackendExplainsAndShowsFix(t *testing.T) {
	f := newUploadFleet(t)
	f.apply = "@@SESSIONS_RC 78\n@@SESSIONS_JSON {\"ok\":false,\"error\":{\"code\":78,\"message\":\"no backend configured\",\"name\":\"EX_CONFIG\"}}\n@@SESSIONS_ERR no backend configured\n"
	f.act(t, ext.EncodeState(sessPage{Machine: "local"}), "cold-offload", map[string]string{"m": "local"})
	f.a.waitJobs()
	j := f.a.jobSnapshot()["local"]
	if !strings.Contains(j.Err, "还没有设置网盘") || !strings.Contains(j.Err, "一个都没有搬走或删除") || strings.Contains(j.Err, "退出码") {
		t.Fatalf("no-backend failure: %q", j.Err)
	}
	if j.Command != noBackendCommand || !strings.Contains(j.Raw, "no backend configured") {
		t.Fatalf("no-backend fix: command %q raw %q", j.Command, j.Raw)
	}
	items := itemsOf(f.view(t, "local"), "local")
	var code string
	for _, c := range items {
		if c.Type == wire.ComponentCode {
			code = c.Text
		}
	}
	if code != noBackendCommand {
		t.Fatalf("the row does not show the command that sets up a network drive: %+v", items)
	}
}

// F4: a running upload says what is being done; a finished one says how many moved, the space
// freed and how many in-use conversations stayed.
func TestUploadProgressAndOutcomeInPlainWords(t *testing.T) {
	f := newUploadFleet(t)
	f.gate = make(chan struct{})
	res := f.act(t, ext.EncodeState(sessPage{Machine: "local"}), "cold-offload", map[string]string{"m": "local"})
	if !strings.Contains(res.Toast, "搬到网盘") || !strings.Contains(res.Toast, "核对无误才删") {
		t.Fatalf("toast: %s", res.Toast)
	}
	for _, m := range []string{"local", ""} {
		txt := texts(f.view(t, m))
		if strings.Contains(txt, "读取") || !strings.Contains(txt, "正在进行：「"+localMachineName+"」把一周没动的 AI 对话搬到网盘") {
			t.Fatalf("running upload (%q):\n%s", m, txt)
		}
	}
	close(f.gate)
	f.a.waitJobs()
	j := f.a.jobSnapshot()["local"]
	want := "搬完了：1 个一周没动的 AI 对话已经放进网盘，电脑上腾出 1.2 MiB；1 个还在用的对话留在电脑上没动。"
	if j.Err != "" || j.Note != want {
		t.Fatalf("outcome %q (err %q), want %q", j.Note, j.Err, want)
	}
	if strings.Contains(j.Note, "→") {
		t.Fatal("outcome shows the before → after free-space arithmetic")
	}
}

// F5: session state lives on AI 对话 only; the main page's fourth tab is 清理记录 and links there.
func TestMainPageDoesNotRepeatSessionState(t *testing.T) {
	a := newTestApp(t)
	a.runner = coldRunner(nil)
	ctx := context.Background()
	for _, tab := range []string{tabSessions, tabHistory} {
		res, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(page{Tab: tab}), Action: "tab", Values: map[string]string{"tab": tab}}})
		if err != nil {
			t.Fatal(err)
		}
		v := res.View
		if v.Tabs == nil || v.Tabs.Value != tabHistory || v.Tabs.Items[3].Title != "清理记录" || v.Tabs.Items[3].Badge != "0" {
			t.Fatalf("tab %s: %+v", tab, v.Tabs)
		}
		for _, s := range v.Sections {
			if s.Key == "cold-machines" || strings.Contains(s.Title, "会话") {
				t.Fatalf("tab %s draws session state: %q", tab, s.Title)
			}
		}
		link := rowsWithPrefix(v, "open-sessions")
		if len(link) != 1 || link[0].Action.Push == nil || link[0].Action.Push.UI != sessionsUI {
			t.Fatalf("tab %s: no link to %s: %+v", tab, sessionsTitle, link)
		}
	}
	// An app that does not push pages calls the action instead.
	res, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(page{Tab: tabHistory}), Action: "open-sessions"}})
	if err != nil || res.Push == nil || res.Push.UI != sessionsUI {
		t.Fatalf("open-sessions: %+v %v", res, err)
	}
}

// jargonRE is wording the pages no longer use: English levels and terms, and internal Chinese.
var jargonRE = regexp.MustCompile(`(?i)\bsafe\b|\breview\b|dry-run|水位|纳管|台账|根分区|挂载点|扫描模式|\bhub\b|冷会话|节点|备份后端|\bCLI\b|\bbackend\b|invalid page state`)

// scanJargonRE is the developer wording the probe script's item titles and reasons used to carry
// (item paths such as docker://dangling-images stay exact, so "dangling" is checked on titles and
// reasons only).
var scanJargonRE = regexp.MustCompile(`悬空|内容寻址|轮子|极速|无头|轮转|陈旧|产物|镜像层|引用|DerivedData|Crates|元数据|dlx|>7天|HTTP 缓存|检出|预编译|同步的|全局存储`)

// probeScriptSample is probe output carrying every item line ProbeScript can print, with the
// script's own ids, titles and reasons (sizes and paths made up), plus the journal and the
// Docker build cache lines that ParseProbeOutput words itself.
func probeScriptSample(t *testing.T) string {
	t.Helper()
	var caches, tmp, docker []string
	dirRE := regexp.MustCompile(`^probe_dir "([^"]*)" "([^"]*)" "([^"]*)" "[^"]*" "([^"]*)" "([^"]*)"$`)
	fmtRE := regexp.MustCompile(`^\s*printf '((?:logs-rotated|tmp-stale|docker-dangling)\\t[^']*)\\n'`)
	for _, line := range strings.Split(ProbeScript, "\n") {
		if m := dirRE.FindStringSubmatch(line); m != nil {
			caches = append(caches, strings.Join([]string{m[1], m[2], m[3], "/home/dev/" + m[1], "2048", m[4], m[5]}, "\t"))
		} else if m := fmtRE.FindStringSubmatch(line); m != nil {
			l := strings.ReplaceAll(strings.Replace(m[1], "%s", "2048", 1), `\t`, "\t")
			if strings.HasPrefix(l, "docker-") {
				docker = append(docker, l)
			} else {
				tmp = append(tmp, l)
			}
		}
	}
	if len(caches) < 15 || len(tmp) != 2 || len(docker) != 1 {
		t.Fatalf("probe script lines not found: %d caches, %d tmp, %d docker", len(caches), len(tmp), len(docker))
	}
	return "@@SECTION:DF\nFilesystem 1B-blocks Used Available Use% Mounted on\n/dev/sda1 100000000000 60000000000 40000000000 60% /\n" +
		"@@SECTION:CACHES\n" + strings.Join(caches, "\n") + "\n" +
		"@@SECTION:TMP_LOGS\nArchived and active journals take up 1.5G in the file system.\n" + strings.Join(tmp, "\n") + "\n" +
		"@@SECTION:DOCKER\n" + strings.Join(docker, "\n") + "\ndocker-buildx\t1.2GB\n@@SECTION:END\n"
}

// visibleText is what a view shows people: titles, texts, stats, tables, fields, confirms;
// commands to copy and links are left out (they stay exact).
func visibleText(v *wire.View) string {
	var b strings.Builder
	add := func(s ...string) { b.WriteString(strings.Join(s, "\n") + "\n") }
	add(v.Title)
	if v.Tabs != nil {
		for _, t := range v.Tabs.Items {
			add(t.Title)
		}
	}
	if v.Filter != nil {
		add(v.Filter.Title)
		for _, c := range v.Filter.Choices {
			add(c.Title)
		}
	}
	if v.Search != nil {
		add(v.Search.Placeholder)
	}
	for _, s := range v.Sections {
		add(s.Title, s.Footer)
		for _, c := range s.Items {
			if c.Type == wire.ComponentCode {
				continue
			}
			add(c.Title, c.Subtitle, c.Detail, c.Text, c.Label, c.Placeholder, c.Caption)
			if c.Badge != nil {
				add(c.Badge.Text)
			}
			for _, bd := range c.Badges {
				add(bd.Text)
			}
			for _, f := range c.Facts {
				add(f.Label, f.Value)
			}
			for _, st := range c.Stats {
				add(st.Label, st.Value, st.Detail)
			}
			for _, col := range c.Columns {
				add(col.Title)
			}
			for _, r := range c.Rows {
				add(r...)
			}
			for _, se := range c.Series {
				add(se.Name)
				for _, pt := range se.Points {
					add(pt.Label, pt.Display)
				}
			}
			if c.Action != nil {
				add(c.Action.Confirm)
			}
			if c.Button != nil {
				add(c.Button.Title)
			}
			for _, ra := range c.Actions {
				add(ra.Title, ra.Action.Confirm)
			}
		}
	}
	return b.String()
}

// The pages a first-time user sees speak plain Chinese: no levels in English, no internal terms.
func TestPagesUsePlainWords(t *testing.T) {
	ctx := context.Background()
	f := newUploadFleet(t)
	a := f.a
	var views []*wire.View
	push := func(v *wire.View, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, v)
	}
	push(a.render(ctx, &ext.UIRequest{}))
	push(a.card(ctx, &ext.UIRequest{}))
	main := func(state, action string, values map[string]string) {
		t.Helper()
		res, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: state, Action: action, Values: values}})
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, res.View)
	}
	main("", "scan-all", nil)
	a.waitJobs()
	for _, tab := range []string{tabFleet, tabItems, tabPlan, tabHistory} {
		main(ext.EncodeState(page{Machine: "local"}), "tab", map[string]string{"tab": tab})
	}
	main("", "new-machine", nil)
	main("", "edit-machine", map[string]string{"m": "build-box"})
	push(a.card(ctx, &ext.UIRequest{}))
	for _, m := range []string{"", "local", "build-box", "nas-box"} {
		views = append(views, f.view(t, m))
	}
	f.apply = "@@SESSIONS_RC 78\n@@SESSIONS_JSON {\"ok\":false,\"error\":{\"code\":78,\"message\":\"no backend configured\",\"name\":\"EX_CONFIG\"}}\n"
	f.act(t, ext.EncodeState(sessPage{Machine: "local"}), "cold-offload", map[string]string{"m": "local"})
	a.waitJobs()
	views = append(views, f.view(t, "local"))
	// Every item the probe script can report, by its own titles and reasons, on 能删什么 and
	// 先看看会删什么.
	scan := ParseProbeOutput(&Machine{ID: "local", IsLocal: true}, probeScriptSample(t), t0)
	if len(scan.Items) < 20 {
		t.Fatalf("probe sample gave %d items", len(scan.Items))
	}
	for _, it := range scan.Items {
		words := it.Title + "\n" + it.Reason
		if m := scanJargonRE.FindAllString(words, -1); len(m) > 0 || jargonRE.MatchString(words) || strings.Contains(strings.ToLower(words), "dangling") {
			t.Errorf("item %s uses %q: %s / %s", it.ID, m, it.Title, it.Reason)
		}
	}
	if err := a.db().update(func(st *State, now time.Time) error {
		st.Scans["local"] = scan
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tab := range []string{tabItems, tabPlan} {
		main(ext.EncodeState(page{Machine: "local"}), "tab", map[string]string{"tab": tab})
		if txt := visibleText(views[len(views)-1]); !strings.Contains(txt, "Docker 没名字的旧镜像") && tab == tabItems {
			t.Errorf("%s does not list the probe's items:\n%s", tab, txt)
		}
	}
	for i, v := range views {
		if m := jargonRE.FindAllString(visibleText(v), -1); len(m) > 0 {
			t.Errorf("view %d (%s) uses %q:\n%s", i, v.Title, m, visibleText(v))
		}
		if m := scanJargonRE.FindAllString(visibleText(v), -1); len(m) > 0 {
			t.Errorf("view %d (%s) uses %q:\n%s", i, v.Title, m, visibleText(v))
		}
	}
	// An unreadable page state is reported in Chinese.
	_, err := a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: "%%%"}})
	if err == nil || !strings.Contains(err.Error(), "重新打开") {
		t.Fatalf("bad state: %v", err)
	}
}

// setJob puts a finished job of kind with error text errText on machine id.
func setJob(a *app, id, kind, errText string) {
	a.jobs.mu.Lock()
	defer a.jobs.mu.Unlock()
	if a.jobs.jobs == nil {
		a.jobs.jobs = map[string]*coldJob{}
		a.jobs.sem = make(chan struct{}, coldParallel)
	}
	a.jobs.jobs[id] = &coldJob{Kind: kind, Label: "x", Started: t0, Finished: t0, Err: errText}
}

// quoted returns the 「…」 names in s.
func quoted(s string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`「([^「」]+)」`).FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// A failed job's text names only a button its row really draws, and never the upload unless the
// upload failed: a restore, install, Homebrew or file job that failed does not point at
// 「重试：搬到网盘」, which deletes local copies.
func TestFailedJobRetryNamesItsOwnButton(t *testing.T) {
	f := newUploadFleet(t)
	const generic = "网盘连不上，修好后点「重试」。"
	cases := []struct {
		machine, kind string
		wantRetry     string // the button the text must name; "" none
		wantHow       string // a sentence the text must carry instead
	}{
		{"local", jobOffload, coldRetryText, ""},
		{"local", jobRestore, "", "点「拿回来」"},
		{"local", jobInstall, "重新检查 " + localMachineName, ""},
		{"local", jobBrew, "重新检查 " + localMachineName, ""},
		{"local", jobFileOffload, "", "磁盘占用图"},
		{"local", jobFileRestore, "", "磁盘占用图"},
		{"build-box", jobOffload, "重新检查 build-box", ""},
		{"build-box", jobRestore, "", "点「拿回来」"},
		{"build-box", jobInstall, "重新检查 build-box", ""},
		{"build-box", jobBrew, "重新检查 build-box", ""},
		{"nas-box", jobFileOffload, "", "磁盘占用图"},
	}
	for _, c := range cases {
		t.Run(c.machine+"/"+c.kind, func(t *testing.T) {
			setJob(f.a, c.machine, c.kind, generic)
			items := itemsOf(f.view(t, c.machine), c.machine)
			var outcome string
			buttons := map[string]bool{}
			for _, it := range items {
				if it.Type == wire.ComponentText && strings.Contains(it.Text, "没有成功") {
					outcome = it.Text
				}
				if it.Type == wire.ComponentButton {
					buttons[it.Title] = true
				}
			}
			if outcome == "" {
				t.Fatalf("no outcome line: %+v", items)
			}
			if c.kind != jobOffload && (strings.Contains(outcome, coldRetryText) || buttons[coldRetryText]) {
				t.Fatalf("a failed %s points at the upload: %q", c.kind, outcome)
			}
			if strings.Contains(outcome, "「重试」") {
				t.Fatalf("names a 重试 button the row does not have: %q", outcome)
			}
			if c.wantRetry != "" && (!strings.Contains(outcome, "「"+c.wantRetry+"」") || !buttons[c.wantRetry]) {
				t.Fatalf("want retry %q (drawn %v): %q", c.wantRetry, buttons, outcome)
			}
			if c.wantHow != "" && !strings.Contains(outcome, c.wantHow) {
				t.Fatalf("want %q: %q", c.wantHow, outcome)
			}
			for _, q := range quoted(outcome) {
				if strings.HasPrefix(q, "重试") || strings.HasPrefix(q, "重新检查") || strings.HasPrefix(q, "用 Homebrew") || strings.HasPrefix(q, "从 ") {
					if !buttons[q] {
						t.Fatalf("names button %q that the row does not draw (%v): %q", q, buttons, outcome)
					}
				}
			}
		})
	}
}

// 删掉放心删的缓存 says that old logs and temporary files are deleted too and do not come back.
func TestSafeCleanConfirmNamesLogs(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	if _, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{Action: "scan-all"}}); err != nil {
		t.Fatal(err)
	}
	a.waitJobs()
	var confirms []string
	for _, tab := range []string{tabFleet, tabPlan} {
		res, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(page{Machine: "local"}), Action: "tab", Values: map[string]string{"tab": tab}}})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range res.View.Sections {
			for _, c := range s.Items {
				if c.Action != nil && c.Action.ID == "clean-safe" {
					confirms = append(confirms, c.Action.Confirm)
				}
			}
		}
	}
	if len(confirms) != 2 {
		t.Fatalf("clean-safe buttons: %d", len(confirms))
	}
	// What BuildItemCleanScript really runs: the journal size is the whole journal of which only the
	// part older than 7 days goes; every rotated file (.gz, .1, .old) goes; build and install
	// leftovers in /tmp go however recent they are.
	for _, c := range confirms {
		for _, w := range []string{"系统日志里 7 天以前的部分", "全部系统日志的大小", "实际腾出的会少一些", "系统换下来的旧日志", ".gz", ".1、.old",
			"临时文件夹 /tmp", "go-build", "最近的也会删", "放了 7 天以上", "找不回来", "AI 对话都不"} {
			if !strings.Contains(c, w) {
				t.Fatalf("confirm lacks %q: %s", w, c)
			}
		}
		for _, old := range []string{"压缩归档的旧日志", "放了 7 天以上的临时文件"} {
			if strings.Contains(c, old) {
				t.Fatalf("confirm still says %q: %s", old, c)
			}
		}
	}
	for id, want := range map[string]string{"tmp-stale": "-name 'go-build*'", "logs-rotated": "-name '*.old'", "journalctl-vacuum": "--vacuum-time=7d"} {
		if !strings.Contains(BuildItemCleanScript(ScanItem{ID: id}, nil), want) {
			t.Fatalf("clean script for %s no longer has %q: update the confirm wording", id, want)
		}
	}
	if got := safeLogsNote(&Scan{Items: []ScanItem{{Category: CatGoCache, Safety: SafetySafe, Bytes: 1}}}); got != "" {
		t.Fatalf("no logs, yet: %q", got)
	}
}

// The MCP contract: machine_name 运行 Persona 的电脑 is accepted back as machine, the stored host
// name still finds that computer's sessions, and the tool description lists the location values
// the results carry.
func TestMCPAcceptsLocalLabelAndHostName(t *testing.T) {
	st, m := coldState(t)
	m.Name = "local (devbox-7)"
	if got := st.machine(localMachineName); got == nil || got.ID != m.ID {
		t.Fatalf("machine(%q) = %+v", localMachineName, got)
	}
	if hits := SearchCold(st, "checkout", localMachineName, 0, t0); len(hits) != 1 || hits[0].MachineName != localMachineName {
		t.Fatalf("search by label: %+v", hits)
	}
	if hits := SearchCold(st, "devbox-7 checkout", "", 0, t0); len(hits) != 1 {
		t.Fatalf("search by host name: %d hits", len(hits))
	}

	s := newStore(t.TempDir())
	s.homeDir = ""
	s.now = func() time.Time { return t0 }
	srv := &mcpServer{store: s, runner: coldRunner(nil)}
	ctx := context.Background()
	if _, err := srv.call(ctx, "disklessly_sessions_status", []byte(`{"refresh":true}`)); err != nil {
		t.Fatal(err)
	}
	out, err := srv.call(ctx, "disklessly_sessions_search", []byte(`{"query":"checkout","machine":"`+localMachineName+`"}`))
	if err != nil {
		t.Fatalf("machine_name sent back: %v", err)
	}
	if txt := fmt.Sprint(out); !strings.Contains(txt, "claude --resume "+uuidClaude) || !strings.Contains(txt, localMachineName) {
		t.Fatalf("search: %s", txt)
	}

	var desc string
	for _, tl := range tools {
		if tl.name == "disklessly_sessions_search" {
			desc = tl.description
		}
	}
	for _, w := range []string{LocCloud, LocUnknown, LocMissing, LocLocal, "不代表丢失"} {
		if !strings.Contains(desc, w) {
			t.Fatalf("sessions_search description lacks %q: %s", w, desc)
		}
	}
	for _, old := range []string{"在云端", "云端缺失", "位置「未知」"} {
		if strings.Contains(desc, old) {
			t.Fatalf("sessions_search description still lists %q", old)
		}
	}
}

// A remote computer that carries the local label (older state) must not capture the label: the
// tools resolve 运行 Persona 的电脑 to the hub's own computer before matching names.
func TestLocalLabelResolvesBeforeRemoteNames(t *testing.T) {
	st, m := coldState(t)
	m.Name = "local (devbox-7)"
	remote := &Machine{ID: "rbox", Host: "rbox", Name: localMachineName, Enabled: true, Reachable: true, Source: "manual"}
	st.Machines = append(st.Machines, remote)
	cat := *st.cold(m.ID)
	st.setCold(remote.ID, &cat)
	for _, label := range []string{localMachineName, "运行Persona的电脑", " 运行 persona 的电脑 "} {
		if got := st.machine(label); got == nil || got.ID != m.ID {
			t.Fatalf("machine(%q) = %+v, want the local computer", label, got)
		}
	}
	if got := st.machine("rbox"); got != remote {
		t.Fatalf("machine(rbox) = %+v", got)
	}
	hits := SearchCold(st, "checkout", localMachineName, 0, t0)
	if len(hits) == 0 {
		t.Fatal("search by label: no hits")
	}
	for _, h := range hits {
		if h.MachineID != m.ID {
			t.Fatalf("search by label hit %q, want the local computer", h.MachineID)
		}
	}
}

// Adding or renaming a remote computer to the local label is refused with a plain reason.
func TestRemoteComputerCannotTakeLocalLabel(t *testing.T) {
	st := &State{}
	st.ensureDefaults(t0)
	if _, err := st.upsertMachine(MachineInput{Host: "rbox", Name: "build box", Source: "manual"}, t0); err != nil {
		t.Fatal(err)
	}
	for _, in := range []MachineInput{
		{Host: "newbox", Name: localMachineName, Source: "manual"},
		{Host: "newbox", Name: "运行persona的电脑", Source: "manual"},
		{Host: "newbox", Name: localMachineName + " (192.0.2.5)", Source: "manual"},
		{ID: "rbox", Host: "rbox", Name: localMachineName, Source: "manual"},
	} {
		_, err := st.upsertMachine(in, t0)
		if err == nil || !strings.Contains(err.Error(), "换一个名字") || strings.Contains(err.Error(), "local") {
			t.Fatalf("upsert %+v: err = %v", in, err)
		}
	}
	if got := st.machine("rbox"); got == nil || got.Name != "build box" {
		t.Fatalf("rename went through: %+v", got)
	}
	if st.machine("newbox") != nil {
		t.Fatal("refused computer was added")
	}
	// The hub's own computer may still be called by its label.
	if m, err := st.upsertMachine(MachineInput{ID: "local", Name: localMachineName}, t0); err != nil || !m.IsLocal {
		t.Fatalf("local rename: %+v %v", m, err)
	}
}

// One spelling for the hub's own computer across the code's texts.
func TestOneNameForThisComputer(t *testing.T) {
	re := regexp.MustCompile(`运行 Persona 的(那台|这台)电脑|运行 Persona 的这台|本机节点`)
	for _, f := range []string{"ui.go", "ui_sessions.go", "ui_charts.go", "coldjobs.go", "coldstate.go", "problems.go", "mainjobs.go", "model.go", "offload_i18n.go", "scanner.go", "cleaner.go", "coldindex.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := re.FindAllString(string(b), -1); len(m) > 0 {
			t.Errorf("%s: %q", f, m)
		}
	}
	for _, loc := range []string{locEN, locJA} {
		for k, v := range offloadText[loc] {
			if strings.Contains(strings.ToLower(v), "cloud") || strings.Contains(v, "クラウド") {
				t.Errorf("%s %s mixes cloud with network drive: %q", loc, k, v)
			}
		}
	}
}

// clickables are the names a person can tap on v: buttons, rows with an action, row actions,
// empty-state buttons, tabs, the computer filter and links.
func clickables(v *wire.View) map[string]bool {
	out := map[string]bool{}
	if v.Tabs != nil {
		for _, t := range v.Tabs.Items {
			out[t.Title] = true
		}
	}
	if v.Filter != nil {
		out[v.Filter.Title] = true
		for _, c := range v.Filter.Choices {
			out[c.Title] = true
		}
	}
	for _, s := range v.Sections {
		for _, c := range s.Items {
			switch {
			case c.Type == wire.ComponentButton, c.Type == wire.ComponentLink, c.Type == wire.ComponentRow && c.Action != nil:
				out[c.Title] = true
			}
			if c.Button != nil {
				out[c.Button.Title] = true
			}
			for _, ra := range c.Actions {
				out[ra.Title] = true
			}
		}
	}
	return out
}

// tapRE finds the 「…」 name an instruction asks to tap: 点「X」, 点下面这台电脑的「X」, 再点「X」.
var tapRE = regexp.MustCompile(`点([^「」。；，]{0,10})「([^「」]+)」`)

// tapNames are the names v's texts ask to tap (confirms and commands are not instructions).
func tapNames(v *wire.View) []string {
	var texts []string
	for _, s := range v.Sections {
		texts = append(texts, s.Title, s.Footer)
		for _, c := range s.Items {
			if c.Type == wire.ComponentCode {
				continue
			}
			texts = append(texts, c.Subtitle, c.Detail, c.Text, c.Caption)
		}
	}
	var out []string
	for _, t := range texts {
		for _, m := range tapRE.FindAllStringSubmatch(t, -1) {
			// 「拿回来」 is on a conversation's row: the text says to find that row first.
			if m[2] == "拿回来" && regexp.MustCompile(`(搜到它|找到这个对话)，$`).MatchString(t[:strings.Index(t, m[0])]) {
				continue
			}
			out = append(out, m[2])
		}
	}
	return out
}

// checkTapNames fails when a text on v asks to tap a name that v does not draw.
func checkTapNames(t *testing.T, what string, v *wire.View) {
	t.Helper()
	can := clickables(v)
	for _, n := range tapNames(v) {
		if !can[n] {
			names := slices.Sorted(maps.Keys(can))
			t.Errorf("%s: asks to tap 「%s」, which the page does not draw (it draws %q):\n%s", what, n, names, visibleText(v))
		}
	}
}

// Every button an outcome or instruction names is drawn on that same page state: the 硬盘空间
// page's tabs (with failed, finished and running checks and cleanups) and the AI 对话 page in
// each state a computer can be in.
func TestInstructionsNameButtonsOnTheirPage(t *testing.T) {
	ctx := context.Background()
	const sshDown = "SSH 连接 build-box 失败：ssh: connect to host build-box port 22: Connection timed out"
	f := newUploadFleet(t)
	a := f.a
	mainTabs := func(label string) {
		t.Helper()
		for _, m := range []string{"local", "build-box"} {
			for _, tab := range []string{tabFleet, tabItems, tabPlan, tabHistory} {
				res, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(page{Machine: m}), Action: "tab", Values: map[string]string{"tab": tab}}})
				if err != nil {
					t.Fatal(err)
				}
				checkTapNames(t, label+" "+m+"/"+tab, res.View)
				if tab == tabFleet {
					// Each 帮我腾空间 step names a button the page it opens draws.
					checkAdviceButtons(t, a, label+" "+m+" 帮我腾空间", res.View)
				}
			}
		}
	}
	mainTabs("not checked")
	if _, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{Action: "scan-all"}}); err != nil {
		t.Fatal(err)
	}
	a.waitJobs()
	mainTabs("checked")
	for _, kind := range []string{jobScan, jobClean} {
		setJob(a, "local", kind, "网盘连不上，修好后点「重试」。")
		mainTabs("failed " + kind)
	}
	if err := a.db().update(func(st *State, now time.Time) error {
		st.machine("build-box").LastError = sshDown
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	setJob(a, "build-box", jobScan, sshDown)
	mainTabs("unreachable")
	// A later AI 对话 check reached it: the line says so and still names 重试.
	if err := a.db().update(func(st *State, now time.Time) error {
		st.machine("build-box").Reachable = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mainTabs("unreachable, reached later")
	// Never checked and unreachable: its own tab draws 重试 in the empty state only.
	if err := a.db().update(func(st *State, now time.Time) error {
		delete(st.Scans, "build-box")
		st.machine("build-box").Reachable = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mainTabs("unreachable, never checked")

	// A check running on a computer never checked: its 检查 button is hidden meanwhile.
	g := newUploadFleet(t)
	g.a.jobs.mu.Lock()
	g.a.jobs.jobs["build-box"] = &coldJob{Kind: jobScan, Label: "正在检查硬盘（只看不删）", Started: t0}
	g.a.jobs.mu.Unlock()
	res, err := g.a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(page{Machine: "build-box"}), Action: "tab", Values: map[string]string{"tab": tabFleet}}})
	if err != nil {
		t.Fatal(err)
	}
	checkTapNames(t, "running check", res.View)
	if txt := visibleText(res.View); !strings.Contains(txt, "正在检查这台电脑") {
		t.Errorf("a running check is not said:\n%s", txt)
	}
	g.a.jobs.mu.Lock()
	g.a.jobs.jobs["build-box"].Finished = t0
	g.a.jobs.mu.Unlock()

	// The AI 对话 page, build-box in every state.
	reach, down := true, false
	states := []struct {
		name string
		set  func(c *ColdCatalog)
	}{
		{"ready", func(c *ColdCatalog) { c.State, c.Backend.Reachable = ColdReady, &reach }},
		{"no tool, Homebrew", func(c *ColdCatalog) { c.State, c.Brew, c.BrewKnown = ColdNoCLI, "/opt/homebrew/bin/brew", true }},
		{"no tool, Homebrew unknown", func(c *ColdCatalog) { c.State, c.Brew, c.BrewKnown = ColdNoCLI, "", false }},
		{"no tool, no Homebrew", func(c *ColdCatalog) { c.State, c.Brew, c.BrewKnown = ColdNoCLI, "", true }},
		{"old tool", func(c *ColdCatalog) { c.State, c.Brew, c.BrewKnown = ColdCLIOld, "/opt/homebrew/bin/brew", true }},
		{"old tool, no Homebrew", func(c *ColdCatalog) { c.State, c.Brew, c.BrewKnown = ColdCLIOld, "", true }},
		{"old tool, Homebrew unknown", func(c *ColdCatalog) { c.State, c.Brew, c.BrewKnown = ColdCLIOld, "", false }},
		{"no tool, system unknown", func(c *ColdCatalog) { c.State, c.Brew, c.BrewKnown, c.Platform = ColdNoCLI, "", true, "" }},
		{"no drive", func(c *ColdCatalog) { c.State = ColdNoBackend }},
		{"drive down", func(c *ColdCatalog) { c.State, c.Backend.Reachable = ColdBackendDown, &down }},
		{"drive without repo", func(c *ColdCatalog) {
			c.State, c.Backend.Reachable, c.Backend.Error = ColdBackendDown, &down, "repo.json is not in the folder"
		}},
		{"offline", func(c *ColdCatalog) { c.State, c.Error = ColdOffline, sshDown }},
		{"unreadable", func(c *ColdCatalog) { c.State, c.Error = ColdUnreadable, "disklessly 没有正常结束" }},
		{"unchecked", func(c *ColdCatalog) { c.State = ColdUnchecked }},
	}
	for _, s := range states {
		for _, kind := range []string{"", jobRefresh, jobOffload, jobRestore, jobInstall, jobBrew} {
			if err := g.a.db().update(func(st *State, now time.Time) error {
				c := *st.cold("build-box")
				c.Error, c.Backend.Error = "", ""
				s.set(&c)
				st.setCold("build-box", &c)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if kind != "" {
				setJob(g.a, "build-box", kind, "网盘连不上，修好后点「重试」。")
			}
			for _, m := range []string{"", "build-box"} {
				checkTapNames(t, fmt.Sprintf("AI 对话 %s/%s/%q", s.name, kind, m), g.view(t, m))
			}
		}
	}
	// An upload running on a ready computer hides its button: the hint does not name it.
	if err := g.a.db().update(func(st *State, now time.Time) error {
		c := *st.cold("build-box")
		c.State, c.Error, c.Backend.Reachable = ColdReady, "", &reach
		st.setCold("build-box", &c)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	g.a.jobs.mu.Lock()
	g.a.jobs.jobs["build-box"] = &coldJob{Kind: jobOffload, Label: "正在搬到网盘", Started: t0}
	g.a.jobs.mu.Unlock()
	checkTapNames(t, "AI 对话 upload running", g.view(t, "build-box"))
	g.a.jobs.mu.Lock()
	g.a.jobs.jobs["build-box"].Finished = t0
	g.a.jobs.mu.Unlock()

	// Every way a computer cannot be reached, on both pages: the plain line under its row names
	// only buttons drawn there.
	for _, cause := range unreachableErrors {
		h := newUploadFleet(t)
		if err := h.a.db().update(func(st *State, now time.Time) error {
			m := st.machine("build-box")
			m.LastError, m.Reachable = cause.err, false
			c := *st.cold("build-box")
			c.State, c.Error = ColdOffline, cause.err
			st.setCold("build-box", &c)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		setJob(h.a, "build-box", jobScan, cause.err)
		for _, tab := range []string{tabFleet, tabItems, tabPlan} {
			res, err := h.a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(page{Machine: "build-box"}), Action: "tab", Values: map[string]string{"tab": tab}}})
			if err != nil {
				t.Fatal(err)
			}
			checkTapNames(t, "硬盘空间 "+cause.name+"/"+tab, res.View)
		}
		for _, m := range []string{"", "build-box"} {
			checkTapNames(t, fmt.Sprintf("AI 对话 %s/%q", cause.name, m), h.view(t, m))
		}
	}

	// 还不知道有没有 Homebrew: the button it says will appear is the one drawn once it is known.
	if adv := coldProblem(&State{}, &Machine{ID: "x", Name: "x"}, nil, ColdNoCLI); !strings.Contains(adv.Next, "「"+brewButtonTitle(false)+"」") {
		t.Errorf("Homebrew button named as %q", adv.Next)
	}
}
