package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "disklessly-locator-")
	if err != nil {
		panic(err)
	}
	locatorFile = func() string { return filepath.Join(dir, "data-dir") }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

var t0 = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

const sampleProbeLocal = `@@SECTION:UNAME
Linux x86_64
@@SECTION:HOSTNAME
devvm-dev
@@SECTION:HOME
/home/dev
@@SECTION:BIN_VERSION
disklessly 0.3.0
@@SECTION:DF
Filesystem     1B-blocks         Used    Available Use% Mounted on
/dev/sda1   214748364800 182536110080  32212254720  85% /
@@SECTION:CACHES
cache-gobuild	go-cache	Go 编译缓存	/home/dev/.cache/go-build	4404019	safe	Go 编译时留下的缓存，删了下次编译会自动再生成
cache-gomod	go-cache	Go 下载的依赖包	/home/dev/go/pkg/mod	3145728	safe	Go 项目下载过的依赖包，删了下次用到时会重新下载
cache-pnpm	node-python-cache	pnpm 存的包	/home/dev/.local/share/pnpm/store	2621440	safe	pnpm 给所有项目共用的一份包，删了下次安装时会重新下载
cache-uv	node-python-cache	uv 下载的 Python 包	/home/dev/.cache/uv	1572864	safe	uv 下载过的 Python 包，删了下次用到时会重新下载
cache-cargo-registry	rust-xcode-build	Rust 下载的依赖包（Cargo）	/home/dev/.cargo/registry	1048576	safe	Rust 项目下载过的依赖包，删了下次编译时会重新下载
cache-playwright	rust-xcode-build	Playwright 自动测试用的浏览器	/home/dev/.cache/ms-playwright	1258291	review	自动测试用的几个浏览器，删了下次跑测试前要重新下载，比较大、比较慢
@@SECTION:TMP_LOGS
Archived and active journals take up 1.5G in the file system.
logs-rotated	tmp-logs	系统换下来的旧日志	/var/log	524288	safe	/var/log 里换下来的旧日志文件（.gz 压缩的和 .1、.old 结尾的），正在写的日志不碰
tmp-stale	tmp-logs	临时文件夹里的旧文件和编译剩下的文件	/tmp	314572	safe	临时文件夹 /tmp 里编译和安装剩下的文件（go-build、playwright、npm、pnpm、cargo-install 等开头的，最近的也删）和放了 7 天以上的文件
@@SECTION:DOCKER
docker-dangling	docker-unused	Docker 没名字的旧镜像	docker://dangling-images	2097152	safe	重新构建后被换下来、已经没有名字的旧镜像，没有任何容器（不管在不在运行）用到它们
docker-buildx	3.2GB (100%)
@@SECTION:SESSIONS
tool	claude	Claude Code	/home/dev/.claude/projects	18	245760	42	1887436
tool	codex	Codex	/home/dev/.codex/sessions	7	98304	19	629145
archive	/home/dev/.disklessly/session-archives	6	314572
@@SECTION:SESSIONS_CLI_PLAN
{   "applied" : false,   "cold" : [     "/home/dev/.claude/projects/-home-dev-app/aaa.jsonl",     "/home/dev/.claude/projects/-home-dev-app/bbb.jsonl",     "/home/dev/.codex/sessions/2026/10/01/rollout-2026-10-01T10-00-00-0199aaaa-bbbb-cccc-dddd-eeeeffff0000.jsonl"   ],   "ok" : true,   "skipped" : [ ] } 
@@SECTION:SESSIONS_CLI_STATUS
{"ok":true,"deviceId":"00000000-0000-4000-8000-000000000001","backend":{"id":"main","type":"rclone","reachable":true,"latencyMs":42,"error":""},"counts":{"archived":12,"remote":3,"restored":1,"keptLocal":0,"pendingDelete":0},"archivedBytes":7340032,"segments":{"total":4,"checked":0,"missing":[]}}
@@SECTION:SESSIONS_CLI_LIST
{"ok":true,"deviceId":"00000000-0000-4000-8000-000000000001","entries":[{"id":"0123456789ab","deviceId":"00000000-0000-4000-8000-000000000001","local":true,"tool":"claude","path":"/home/dev/.claude/projects/-home-dev-old/ccc.jsonl","sessionId":"ccc","cwd":"/home/dev/old","project":"home-dev-old","month":"2026-09","mtime":"2026-09-01T10:00:00Z","archivedAt":"2026-10-01T10:00:00Z","bytes":2048,"state":"archived","title":"refactor parser","firstPrompt":"refactor parser","segment":"","encrypted":false}]}
@@SECTION:SESSIONS_FILES
1200	/home/dev/.claude/projects/-home-dev-app/aaa.jsonl
300	/home/dev/.claude/projects/-home-dev-app/bbb.jsonl
50	/home/dev/.claude/projects/-home-dev-app/hot-but-old-open.jsonl
600	/home/dev/.codex/sessions/2026/10/01/rollout-2026-10-01T10-00-00-0199aaaa-bbbb-cccc-dddd-eeeeffff0000.jsonl
@@SECTION:END
`

const sampleProbeARMBOX = `@@SECTION:UNAME
Linux aarch64
@@SECTION:HOSTNAME
armbox-arm64
@@SECTION:HOME
/home/armbox
@@SECTION:BIN_VERSION
@@SECTION:DF
Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/sda1        104857600  96468992   8388608      92% /
@@SECTION:CACHES
cache-gobuild	go-cache	Go 编译缓存	/home/armbox/.cache/go-build	5242880	safe	Go 编译时留下的缓存，删了下次编译会自动再生成
cache-pnpm	node-python-cache	pnpm 存的包	/home/armbox/.local/share/pnpm/store	4194304	safe	pnpm 给所有项目共用的一份包，删了下次安装时会重新下载
@@SECTION:TMP_LOGS
Archived and active journals take up 800M in the file system.
@@SECTION:DOCKER
docker-buildx	2.5GB
@@SECTION:SESSIONS
tool	claude	Claude Code	/home/armbox/.claude/projects	9	131072	31	1468006
@@SECTION:END
`

const sampleSessionApply = `@@SESSIONS_RC 0
@@SESSIONS_JSON {"ok":true,"applied":true,"cold":[],"skipped":[{"path":"/home/dev/.claude/projects/-home-dev-app/bbb.jsonl","reason":"referenced"}],"keptLocal":[],"segments":[],"uploadedBytes":1228800,"archived":[{"id":"aaaaaaaaaaaa","path":"/home/dev/.claude/projects/-home-dev-app/aaa.jsonl","sessionId":"aaa","bytes":1228800}]}
@@RC 0
@@BEFORE_B 8589934592
@@AFTER_B 8591163392
`

func fakeRunner(_ context.Context, m *Machine, script string, _ time.Duration) (string, error) {
	if strings.Contains(script, "@@SECTION:UNAME") {
		if m != nil && m.Host == "offline-box" {
			return "", fmt.Errorf("SSH 连接 offline-box 失败：Connection timed out")
		}
		if m != nil && (m.ID == "armbox" || m.Host == "armbox") {
			return sampleProbeARMBOX, nil
		}
		return sampleProbeLocal, nil
	}
	if strings.Contains(script, "sessions --apply") {
		return sampleSessionApply, nil
	}
	// Clean script simulation: report before/after df bytes
	return "@@RC 0\n@@BEFORE_B 8589934592\n@@AFTER_B 21797958656\n", nil
}

func newTestApp(t *testing.T) *app {
	t.Helper()
	a := newApp(t.TempDir())
	t.Cleanup(a.waitJobs)
	a.store.homeDir = "" // isolate from real host ~/.ssh/config in unit tests
	a.store.now = func() time.Time { return t0 }
	a.now = func() time.Time { return t0 }
	a.runner = fakeRunner
	return a
}

func TestConformance(t *testing.T) {
	a := newTestApp(t)
	exttest.Check(t, manifest, exttest.Serve(t, program(a)))
}

func TestManifest(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m struct {
		Contributes struct {
			MCPServers []struct {
				Allow []string `json:"allow"`
			} `json:"mcp_servers"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil || len(m.Contributes.MCPServers) != 1 {
		t.Fatalf("one mcp server: %v", err)
	}
	var readOnly []string
	for _, tl := range tools {
		if tl.readOnly {
			readOnly = append(readOnly, tl.name)
		}
	}
	if allow := m.Contributes.MCPServers[0].Allow; !slices.Equal(allow, readOnly) {
		t.Fatalf("allow %v, want exactly read-only tools %v", allow, readOnly)
	}
	if !strings.Contains(usage, "mcp") {
		t.Fatal("--help names mcp, which the manifest requires")
	}
	if _, err := os.Stat(filepath.Join("skills", "disklessly", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestSSHAndPersonaDiscovery(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".persona"), 0o700); err != nil {
		t.Fatal(err)
	}

	sshCfg := `
# Global defaults
Host *
    User defaultuser
    Port 22

Host armbox
    HostName 198.51.100.10
    User armbox
    IdentityFile ~/.ssh/id_ed25519

Host mini-2
    HostName 198.51.100.20
    Port 2222

Host *.internal !bastion.internal
    User internal
`
	if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(sshCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	personaCfg := `{
  "machines": [
    {"id": "devvm", "name": "devvm (Cloud Workstation)", "host": "devvm", "user": "dev"},
    {"id": "local", "name": "local", "host": "localhost"}
  ]
}`
	if err := os.WriteFile(filepath.Join(home, ".persona", "config.json"), []byte(personaCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	st := &State{}
	added := st.DiscoverMachines(home, t0)
	if added != 3 {
		t.Fatalf("DiscoverMachines added = %d, want 3 (devvm, armbox, mini-2); machines=%+v", added, st.Machines)
	}

	armbox := st.machine("armbox")
	if armbox == nil || armbox.User != "armbox" || armbox.Port != 22 || !strings.Contains(armbox.Name, "198.51.100.10") {
		t.Fatalf("armbox machine = %+v", armbox)
	}
	mini2 := st.machine("mini-2")
	if mini2 == nil || mini2.User != "defaultuser" || mini2.Port != 2222 {
		t.Fatalf("mini-2 machine (with wildcard User default) = %+v", mini2)
	}
	devvm := st.machine("devvm")
	if devvm == nil || devvm.Source != "persona" || devvm.Name != "devvm (Cloud Workstation)" {
		t.Fatalf("devvm machine = %+v", devvm)
	}

	// Deleting a discovered machine persists a tombstone so re-discovery cannot add it.
	if err := st.deleteMachine("armbox"); err != nil {
		t.Fatal(err)
	}
	if st.DiscoverMachines(home, t0) != 0 {
		t.Fatal("disabled discovered machine must stay disabled on re-discovery")
	}
	if st.machine("armbox") != nil {
		t.Fatal("armbox should remain deleted")
	}
	if err := st.deleteMachine("local"); err == nil {
		t.Fatal("local machine must not be deletable")
	}
}

func TestHealthEvaluationAndOrdering(t *testing.T) {
	if got := EvaluateHealth(false, 100*gib, 80*gib); got != HealthOffline {
		t.Fatalf("offline = %s", got)
	}
	if got := EvaluateHealth(true, 100*gib, 8*gib); got != HealthCritical {
		t.Fatalf("92%% used = %s, want critical", got)
	}
	if got := EvaluateHealth(true, 100*gib, 15*gib); got != HealthWarning {
		t.Fatalf("85%% used = %s, want warning", got)
	}
	if got := EvaluateHealth(true, 100*gib, 50*gib); got != HealthOK {
		t.Fatalf("50%% used = %s, want ok", got)
	}
}

func TestParseProbeAndCleanSafetyRedlines(t *testing.T) {
	m := &Machine{ID: "local", Host: "localhost", Name: "local (devvm-dev)", IsLocal: true, Enabled: true, Reachable: true}
	scan := ParseProbeOutput(m, sampleProbeLocal, t0)
	if scan.Platform != "linux-amd64" || scan.ScanMode != "disklessly-cli" || scan.BinaryVersion != "disklessly 0.3.0" {
		t.Fatalf("unexpected scan metadata: %+v", scan)
	}
	if scan.Mount.TotalBytes != 214748364800 || scan.Mount.AvailableBytes != 32212254720 {
		t.Fatalf("unexpected mount: %+v", scan.Mount)
	}
	if scan.Sessions.HotCount != 25 || scan.Sessions.ColdCount != 3 || scan.Sessions.ColdBytes != 2100*1024 || scan.Sessions.ArchivedCount != 6 || !scan.Sessions.CLIReady {
		t.Fatalf("unexpected sessions summary: %+v", scan.Sessions)
	}
	if len(scan.Items) < 10 {
		t.Fatalf("expected >= 10 scan items, got %d", len(scan.Items))
	}
	// Verify items are sorted by size descending
	for i := 1; i < len(scan.Items); i++ {
		if scan.Items[i].Bytes > scan.Items[i-1].Bytes {
			t.Fatalf("items not sorted descending at %d: %d > %d", i, scan.Items[i].Bytes, scan.Items[i-1].Bytes)
		}
	}

	// Verify dry-run plan excludes review items by default
	safePlan, err := BuildCleanPlan(m, scan, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range safePlan.Items {
		if it.Safety != SafetySafe {
			t.Fatalf("safe-only plan included non-safe item: %+v", it)
		}
	}
	fullPlan, err := BuildCleanPlan(m, scan, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(fullPlan.Items) <= len(safePlan.Items) {
		t.Fatalf("fullPlan (%d) should include review items beyond safePlan (%d)", len(fullPlan.Items), len(safePlan.Items))
	}

	// Verify safety redlines reject forbidden paths
	for _, badPath := range []string{
		"/",
		"/etc/passwd",
		"/usr/bin",
		"/home/dev",
		"/home/dev/.ssh",
		"/home/dev/.ssh/id_ed25519",
		"/home/dev/.persona/launch.key",
		"/home/dev/../dev",
		"relative/path",
		"docker://running-postgres-container",
	} {
		if err := CheckSafeCleanTarget(ScanItem{ID: "bad", Path: badPath}, "/home/dev"); err == nil {
			t.Errorf("CheckSafeCleanTarget accepted unsafe path %q", badPath)
		}
	}
}

func TestUIViewsAndActions(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()

	// Seed a second remote machine
	if err := a.db().update(func(st *State, now time.Time) error {
		_, err := st.upsertMachine(MachineInput{ID: "armbox", Host: "armbox", Name: "armbox (ARM VM)", Source: "ssh-config"}, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	check := func(v *wire.View) {
		t.Helper()
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("CheckView failed: %v\nview=%+v", err, v)
		}
	}

	// 1. Initial render (unscanned)
	v, err := a.render(ctx, &ext.UIRequest{})
	if err != nil {
		t.Fatal(err)
	}
	check(v)

	// 2. Scan all machines
	res, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{State: v.State, Action: "scan-all"}})
	if err != nil {
		t.Fatal(err)
	}
	check(res.View)
	a.waitJobs()

	// 3. Switch across all tabs and verify every view passes CheckView
	for _, tab := range []string{tabFleet, tabItems, tabPlan, tabSessions} {
		r, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{
			State:  res.View.State,
			Action: "tab",
			Values: map[string]string{"tab": tab},
		}})
		if err != nil {
			t.Fatalf("tab %s: %v", tab, err)
		}
		check(r.View)
		res = r
	}

	// 4. Search on items tab
	rSearch, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		State:  ext.EncodeState(page{Machine: "local", Tab: tabItems}),
		Action: "search",
		Values: map[string]string{"q": "go-build"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	check(rSearch.View)

	// 5. Archive cold sessions on local
	rArch, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		State:  ext.EncodeState(page{Machine: "local", Tab: tabSessions}),
		Action: "archive-sessions",
		Values: map[string]string{"m": "local"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	check(rArch.View)
	if !strings.Contains(rArch.Toast, "搬到网盘") {
		t.Fatalf("unexpected toast: %s", rArch.Toast)
	}
	a.waitJobs()
	if j := a.jobSnapshot()["local"]; !strings.Contains(j.Note, "搬完了：1 个") || !strings.Contains(j.Note, "1 个还在用的对话留在电脑上没动") {
		t.Fatalf("offload outcome: %+v", j)
	}

	// 6. Execute safe clean on armbox
	rClean, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		State:  ext.EncodeState(page{Machine: "armbox", Tab: tabPlan}),
		Action: "clean-safe",
		Values: map[string]string{"m": "armbox"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	check(rClean.View)
	a.waitJobs()

	// 7. New machine form & save
	rNew, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		State:  rClean.View.State,
		Action: "new-machine",
	}})
	if err != nil {
		t.Fatal(err)
	}
	check(rNew.View)

	rSave, err := a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		State:  rNew.View.State,
		Action: "save-machine",
		Values: map[string]string{
			"host": "mini-2",
			"name": "mini-2 (Mac mini M2)",
			"user": "admin",
			"port": "22",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	check(rSave.View)

	// 8. Overview card
	cardView, err := a.card(ctx, &ext.UIRequest{})
	if err != nil {
		t.Fatal(err)
	}
	check(cardView)
}

func TestMCPOverStdio(t *testing.T) {
	dir := t.TempDir()
	st := newStore(dir)
	st.homeDir = ""
	st.now = func() time.Time { return t0 }
	srv := &mcpServer{store: st, runner: fakeRunner}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = srv.serve(ctx, inR, outW)
		_ = outW.Close()
	}()

	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	id := 0
	rpc := func(method string, params any) map[string]any {
		t.Helper()
		id++
		req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
		b, _ := json.Marshal(req)
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
		if !sc.Scan() {
			t.Fatalf("no response for %s", method)
		}
		var resp map[string]any
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["error"] != nil {
			t.Fatalf("%s returned error: %v", method, resp["error"])
		}
		res, _ := resp["result"].(map[string]any)
		return res
	}
	callTool := func(name string, args map[string]any) map[string]any {
		t.Helper()
		res := rpc("tools/call", map[string]any{"name": name, "arguments": args})
		if res["isError"] == true {
			t.Fatalf("tool %s failed: %v", name, res["content"])
		}
		scMap, _ := res["structuredContent"].(map[string]any)
		return scMap
	}

	initRes := rpc("initialize", map[string]any{"protocolVersion": mcpProtocol})
	if initRes["protocolVersion"] != mcpProtocol {
		t.Fatalf("initialize: %v", initRes)
	}
	listRes := rpc("tools/list", map[string]any{})
	tList, _ := listRes["tools"].([]any)
	if len(tList) != len(tools) || len(tools) != 14 {
		t.Fatalf("tools/list count = %d, want 14", len(tList))
	}

	// 1. Upsert remote machine armbox
	upRes := callTool("disklessly_machine_upsert", map[string]any{
		"host": "armbox",
		"name": "armbox (ARM VM)",
		"user": "armbox",
	})
	if upRes["machine"] == nil {
		t.Fatalf("upsert result: %v", upRes)
	}

	// 2. Scan local and armbox
	scanLocal := callTool("disklessly_scan", map[string]any{"machine": "local"})
	if scanLocal["scan"] == nil || scanLocal["dry_run_plan"] == nil {
		t.Fatalf("scan local: %v", scanLocal)
	}
	scanARMBOX := callTool("disklessly_scan", map[string]any{"machine": "armbox"})
	if scanARMBOX["scan"] == nil {
		t.Fatalf("scan armbox: %v", scanARMBOX)
	}

	// 3. Check fleet status
	statusRes := callTool("disklessly_machines_status", map[string]any{})
	if statusRes["machine_count"] != float64(2) {
		t.Fatalf("machine_count = %v, want 2", statusRes["machine_count"])
	}

	// 4. Execute clean on armbox (dry-run then real)
	dryRes := callTool("disklessly_clean", map[string]any{"machine": "armbox", "dry_run": true})
	if dryRes["dry_run"] != true || dryRes["record"] != nil {
		t.Fatalf("dry_run clean: %v", dryRes)
	}
	cleanRes := callTool("disklessly_clean", map[string]any{
		"machine":    "armbox",
		"categories": []string{CatGoCache, CatColdAgentSession},
	})
	if cleanRes["record"] == nil {
		t.Fatalf("real clean should produce record: %v", cleanRes)
	}

	// 5. History ledger
	histRes := callTool("disklessly_history", map[string]any{"machine": "armbox"})
	if histRes["count"] != float64(1) {
		t.Fatalf("history count = %v, want 1", histRes["count"])
	}

	// 6. Delete remote machine
	delRes := callTool("disklessly_machine_delete", map[string]any{"machine": "armbox"})
	if delRes["deleted"] != "armbox" {
		t.Fatalf("delete result: %v", delRes)
	}
	_ = inW.Close()
}
