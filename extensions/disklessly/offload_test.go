package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
)

// fakeBox is one synthetic computer for the disk_offload tests: what the CLI and the file system answer, and what
// commands ran.
type fakeBox struct {
	mu      sync.Mutex
	cli     bool
	old     bool
	brew    bool
	avail   uint64
	entries map[string]string // path -> file, dir, app, stubapp, symlink
	ran     []string          // commands, in order: brew, offload <path>, pack <path>, fetch <path>, unpack <id>, data <id>, restore <id>
	gate    chan struct{}     // when set, item commands wait on it
	started chan string       // when set, each item command reports its start
}

var (
	chkLine   = regexp.MustCompile(`(?m)^chk (\d+) '([^']*)'$`)
	itemCmd   = regexp.MustCompile(`--json ((?:'[^']*' ?)+) 2>/dev/null\)`)
	quotedArg = regexp.MustCompile(`'([^']*)'`)
)

const fakeHome = "/Users/dev"

func newFakeBox() *fakeBox {
	return &fakeBox{cli: true, brew: true, avail: 10 << 30, entries: map[string]string{
		fakeHome + "/Movies/Big.mov":       "file",
		fakeHome + "/Projects/old-dataset": "dir",
		fakeHome + "/Documents":            "dir",
		fakeHome + "/Library/Caches/thing": "dir",
		fakeHome + "/link":                 "symlink",
		fakeHome + "/.ssh/id_ed25519":      "file",
		"/etc/hosts":                       "file",
		"/Applications/Editor.app":         "app",
		"/Applications/Packed.app":         "stubapp",
	}}
}

func (b *fakeBox) log(s string) {
	b.mu.Lock()
	b.ran = append(b.ran, s)
	b.mu.Unlock()
}

func (b *fakeBox) commands() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.ran)
}

func (b *fakeBox) probe() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := "@@SECTION:INV_UNAME\nDarwin arm64\n@@SECTION:INV_HOME\n" + fakeHome + "\n@@SECTION:INV_BREW\n"
	if b.brew {
		s += "/opt/homebrew/bin/brew\n"
	}
	if !b.cli {
		return s + "@@SECTION:INV_NOCLI\n@@SECTION:END\n"
	}
	ver, probe := "disklessly 0.3.2", `{"error":{"code":2,"message":"usage: disklessly offload PATH...","name":"EX_USAGE"},"ok":false}`
	if b.old {
		ver, probe = "disklessly 0.1.0", `{"error":{"code":2,"message":"Unknown command: offload","name":"EX_USAGE"},"ok":false}`
	}
	return s + "@@SECTION:COLD_CLI\nversion\t" + ver + "\npath\t/opt/homebrew/opt/disklessly-cli/bin/disklessly\n@@SECTION:INV_PROBE\n" + probe + "\n"
}

const invBody = `@@SECTION:INV_STATUS
{"repoPath":"/Users/dev/Library/Mobile Documents/com~apple~CloudDocs/Disklessly","apps":{"com.example.Editor":{"imageId":"img1","state":"idle","lastUsed":"2026-10-01T00:00:00Z"},"com.example.Back":{"imageId":"img2","state":"restored"},"com.example.Other":{"imageId":"img9"}},"cache":{"img1":{"bytes":1000,"lastUsed":"2026-10-02T00:00:00Z"}},"restored":{}}
@@SECTION:INV_LIST
[{"imageId":"img1","bundleId":"com.example.Editor","name":"Editor"},{"imageId":"img2","bundleId":"com.example.Back","name":"Back"}]
@@SECTION:INV_DATA
{"ok":true,"apps":[{"app":"Editor","bundleId":"com.example.Editor","state":"offloaded","roots":["com.example.Editor"],"bytes":3000000,"localBytes":0},{"app":"Notes","bundleId":"com.example.Notes","state":"local","bytes":5}]}
@@SECTION:INV_FILES
[{"id":"f1","placeholderPath":"/Users/dev/Movies/Raw.disklessly","name":"Raw","kind":"folder","size":9000000,"createdAt":"2026-08-01T00:00:00Z"},{"id":"f2","placeholderPath":"/Users/dev/gone.mov.disklessly","name":"gone.mov","kind":"file","size":4},{"id":"f3","placeholderPath":"/Users/dev/.vphone.disklessly","name":".vphone","kind":"folder","size":2000000}]
@@SECTION:INV_LINKED
[{"id":"b1","name":"VPhone","appPath":"/Applications/vphone.app","paths":["/Users/dev/.vphone"]}]
@@SECTION:INV_PH
/Users/dev/Movies/Raw.disklessly	{ "kind" : "folder", "name" : "Raw", "originalPath" : "/Users/dev/Movies/Raw", "size" : 9000000, "storedSize" : 8000000, "modifiedAt" : "2026-07-01T00:00:00Z" }
/Users/dev/.vphone.disklessly	{ "kind" : "folder", "name" : ".vphone", "originalPath" : "/Users/dev/.vphone", "size" : 2000000, "storedSize" : 1500000 }
@@SECTION:INV_MANIFESTS
{"imageId":"img1","bundleId":"com.example.Editor","name":"Editor","version":"1.2","createdAt":"2026-09-01T00:00:00Z","image":{"size":4000000,"encryption":{"plainSize":5000000}},"app":{"originalPath":"\/Applications\/Editor.app"}}
@@SECTION:END
`

func (b *fakeBox) run(ctx context.Context, m *Machine, script string, _ time.Duration) (string, error) {
	switch {
	case strings.Contains(script, "@@BREW_OK"):
		b.log("brew")
		b.mu.Lock()
		b.cli, b.old = true, false
		b.mu.Unlock()
		return "@@BREW_OP install\n@@BREW_OK disklessly 0.3.2\n@@BREW_KEG /opt/homebrew/opt/disklessly-cli/bin/disklessly\n", nil
	case strings.Contains(script, "@@SECTION:COLD_LIST"):
		return "@@SECTION:COLD_UNAME\nDarwin arm64\n@@COLD_NOCLI\n", nil
	case strings.Contains(script, "@@RESTORE_RC"):
		id := quotedArg.FindStringSubmatch(script[strings.Index(script, "sessions restore"):])[1]
		b.log("restore " + id)
		return `@@RESTORE_RC 0
@@RESTORE_JSON {"ok":true,"path":"/Users/dev/.claude/projects/x/s1.jsonl","id":"s1"}
@@RESTORE_PRESENT
`, nil
	case strings.Contains(script, "@@SECTION:INV_STATUS"):
		p := b.probe()
		if !strings.Contains(p, "INV_PROBE") {
			return p, nil
		}
		return p + invBody, nil
	case strings.Contains(script, "@@SECTION:INV_PROBE"):
		return b.probe() + "@@SECTION:END\n", nil
	case strings.Contains(script, "@@AVAIL"):
		b.mu.Lock()
		defer b.mu.Unlock()
		return fmt.Sprintf("@@AVAIL %d\n", b.avail), nil
	case strings.Contains(script, "@@SIZE"):
		var out strings.Builder
		for i := range strings.Count(script, "du -sk") {
			fmt.Fprintf(&out, "@@SIZE\t%d\t%d\n", i, i+1)
		}
		return out.String(), nil
	case strings.Contains(script, "chk() {"):
		out := "@@HOME\t" + fakeHome + "\n@@OWN\t" + fakeHome + "/Library/Application Support/Disklessly\n"
		for _, mm := range chkLine.FindAllStringSubmatch(script, -1) {
			i, p := mm[1], mm[2]
			switch b.entries[p] {
			case "":
				out += "@@CHK\t" + i + "\tnot_found\n"
			case "symlink":
				out += "@@CHK\t" + i + "\tsymlink\n"
			case "file":
				out += "@@CHK\t" + i + "\tok\tfile\t-\t-\n"
			case "dir", "app":
				out += "@@CHK\t" + i + "\tok\tdir\t-\t-\n"
			case "stubapp":
				out += "@@CHK\t" + i + "\tok\tdir\tstub\t-\n"
			}
		}
		return out, nil
	case strings.Contains(script, "@@ITEM_RC"):
		mm := itemCmd.FindStringSubmatch(script)
		if mm == nil {
			return "", errors.New("no command in item script")
		}
		var args []string
		for _, q := range quotedArg.FindAllStringSubmatch(mm[1], -1) {
			args = append(args, q[1])
		}
		if b.started != nil {
			b.started <- strings.Join(args, " ")
		}
		if b.gate != nil {
			<-b.gate
		}
		if args[0] == "data" {
			args = args[1:]
		}
		b.log(args[0] + " " + args[1])
		b.mu.Lock()
		b.avail += 1 << 20
		b.mu.Unlock()
		if strings.Contains(args[1], "fail") {
			return "@@ITEM_RC 1\n@@ITEM_JSON {\"ok\":false,\"error\":{\"code\":1,\"message\":\"is open in another app\"}}\n", nil
		}
		return "@@ITEM_RC 0\n@@ITEM_JSON {\"ok\":true,\"sizeBytes\":4096}\n@@ITEM_VERIFIED\n", nil
	}
	return "", fmt.Errorf("unexpected script: %.80q", script)
}

// offloadApp is a test app managing "studio" (a Mac over SSH) with box behind it.
func offloadApp(t *testing.T, box *fakeBox) *app {
	t.Helper()
	a := newTestApp(t)
	a.runner = box.run
	if err := a.db().update(func(st *State, _ time.Time) error {
		st.Machines = append(st.Machines, &Machine{ID: "studio", Host: "studio", Name: "studio", Enabled: true, Reachable: true, Platform: "darwin-arm64"})
		st.setCold("studio", &ColdCatalog{MachineID: "studio", State: ColdReady, RefreshedAt: t0, Entries: []ColdEntry{
			{ID: "s1", Local: true, State: "archived", Bytes: 7000000, Tool: "claude", Title: "Fix bug", Path: "/Users/dev/.claude/projects/x/s1.jsonl", ArchivedAt: "2026-10-01T00:00:00Z"},
			{ID: "s2", Local: true, State: "restored", Bytes: 1},
			{ID: "s3", Local: false, State: "remote", Bytes: 1, DeviceID: "other"},
		}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

var online = true

func studio() personaMachine {
	return personaMachine{ID: "studio", Name: "studio", OS: "darwin", Online: &online}
}

func call(t *testing.T, a *app, method string, params map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(params)
	res, err := a.offload(context.Background(), method, raw)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	out, _ := json.Marshal(res)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if method == "run" {
		runItemKeysOnly(t, m)
	}
	return m
}

// runItemKeysOnly fails when a run's items carry a field Persona's offload schema (OffloadRunItem, additionalProperties
// false) has no room for: the manager would refuse the whole answer as plugin_failed.
func runItemKeysOnly(t *testing.T, res map[string]any) {
	t.Helper()
	items, _ := res["items"].([]any)
	for _, x := range items {
		for k := range x.(map[string]any) {
			if !slices.Contains([]string{"path", "id", "ok", "reason", "refusal", "bytes"}, k) {
				t.Errorf("run item has %q, which offload.schema.json refuses: %v", k, x)
			}
		}
	}
}

func params(extra map[string]any) map[string]any {
	p := map[string]any{"contribution": "disklessly", "locale": "en", "machine": studio()}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func TestOffloadInventoryKinds(t *testing.T) {
	box := newFakeBox()
	a := offloadApp(t, box)
	inv := call(t, a, "inventory", params(nil))
	if inv["state"] != invReady {
		t.Fatalf("state %v: %v", inv["state"], inv["detail"])
	}
	type row struct {
		ID, Kind, Group, Origin, Stub string
		Bytes, Stored, Local          float64
	}
	var got []row
	for _, x := range inv["items"].([]any) {
		it := x.(map[string]any)
		r := row{ID: it["id"].(string), Kind: it["kind"].(string), Bytes: it["bytes"].(float64)}
		r.Group, _ = it["group"].(string)
		r.Origin, _ = it["origin_path"].(string)
		r.Stub, _ = it["stub_path"].(string)
		r.Stored, _ = it["stored_bytes"].(float64)
		r.Local, _ = it["local_bytes"].(float64)
		if it["restorable"] != true {
			t.Errorf("%s not restorable", r.ID)
		}
		got = append(got, r)
	}
	want := []row{
		{ID: "file:f1", Kind: "dir", Origin: "/Users/dev/Movies/Raw", Stub: "/Users/dev/Movies/Raw.disklessly", Bytes: 9000000, Stored: 8000000},
		{ID: "session:s1", Kind: "session", Group: "Claude Code", Origin: "/Users/dev/.claude/projects/x/s1.jsonl", Bytes: 7000000},
		{ID: "app:com.example.Editor", Kind: "app", Origin: "/Applications/Editor.app", Stub: "/Applications/Editor.app", Bytes: 5000000, Stored: 4000000, Local: 1000},
		{ID: "data:com.example.Editor", Kind: "app_data", Group: "Editor", Origin: "/Users/dev/Library/Application Support/com.example.Editor", Bytes: 3000000},
		{ID: "file:f3", Kind: "bundle", Group: "VPhone", Origin: "/Users/dev/.vphone", Stub: "/Users/dev/.vphone.disklessly", Bytes: 2000000, Stored: 1500000},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("items\n got %+v\nwant %+v", got, want)
	}
	if inv["total_bytes"].(float64) != 26000000 || inv["stored_bytes"].(float64) != 13500000 {
		t.Fatalf("totals %v / %v", inv["total_bytes"], inv["stored_bytes"])
	}
	first := inv["items"].([]any)[2].(map[string]any)
	if first["offloaded_at"].(float64) != float64(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix()) ||
		first["used_at"].(float64) != float64(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix()) {
		t.Fatalf("app times %v %v", first["offloaded_at"], first["used_at"])
	}
	if inv["at"].(float64) != float64(t0.Unix()) {
		t.Fatalf("at %v", inv["at"])
	}

	// A second ask within the TTL is answered from memory; refresh asks the machine again.
	calls := 0
	a.runner = func(ctx context.Context, m *Machine, s string, d time.Duration) (string, error) {
		calls++
		return box.run(ctx, m, s, d)
	}
	call(t, a, "inventory", params(nil))
	if calls != 0 {
		t.Fatalf("cached inventory ran %d scripts", calls)
	}
	call(t, a, "inventory", params(map[string]any{"refresh": true}))
	if calls != 1 {
		t.Fatalf("refresh ran %d scripts", calls)
	}
}

func TestOffloadInventoryCap(t *testing.T) {
	var entries []ColdEntry
	for i := range offloadMaxItems + 20 {
		entries = append(entries, ColdEntry{ID: fmt.Sprintf("s%03d", i), Local: true, State: "archived", Bytes: uint64(1000 + i)})
	}
	items, _ := parseInventory(map[string][]string{}, toolProbe{}, &ColdCatalog{Entries: entries}, t0)
	if len(items) != offloadMaxItems+20 {
		t.Fatalf("%d rows", len(items))
	}
	box := newFakeBox()
	a := offloadApp(t, box)
	if err := a.db().update(func(st *State, _ time.Time) error {
		st.setCold("studio", &ColdCatalog{MachineID: "studio", State: ColdReady, Entries: entries})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	inv := call(t, a, "inventory", params(nil))
	if n := len(inv["items"].([]any)); n != offloadMaxItems {
		t.Fatalf("%d items", n)
	}
	more := inv["more"].(map[string]any)
	if more["count"].(float64) != 24 || more["bytes"].(float64) != float64(24*1000+(0+23)*24/2) { // the 24 smallest sessions
		t.Fatalf("more %v", more)
	}
}

func TestOffloadUnknownMachineAndLocale(t *testing.T) {
	a := offloadApp(t, newFakeBox())
	ghost := personaMachine{ID: "ghost-7", Name: "Ghost"}
	for loc, want := range map[string]string{"en": "is not one of the computers", "zh-Hans": "不在 Disklessly 管的电脑里", "zh-CN": "不在", "ja-JP": "管理するコンピュータではありません", "fr": "is not one of the computers", "": "is not one of the computers"} {
		inv := call(t, a, "inventory", map[string]any{"locale": loc, "machine": ghost})
		if inv["state"] != invUnavailable || !strings.Contains(inv["detail"].(string), want) || !strings.Contains(inv["detail"].(string), "Ghost") {
			t.Errorf("%q: %v %v", loc, inv["state"], inv["detail"])
		}
	}
	off := false
	inv := call(t, a, "inventory", map[string]any{"locale": "en", "machine": personaMachine{ID: "studio", Name: "studio", Online: &off}})
	if inv["state"] != invUnavailable || !strings.Contains(inv["detail"].(string), "offline") {
		t.Fatalf("offline: %v", inv)
	}
	if _, err := a.offload(context.Background(), "run", mustJSON(map[string]any{"action": "offload", "paths": []string{"/x"}, "machine": ghost})); err == nil {
		t.Fatal("run on an unknown machine started")
	}
	if _, err := a.offload(context.Background(), "nope", nil); !isCode(err, "unknown_method") {
		t.Fatalf("unknown method: %v", err)
	}
	// A wrong action tells a protocol caller the two values it accepts, in every language.
	for _, loc := range []string{"en", "zh-Hans", "ja-JP"} {
		_, err := a.offload(context.Background(), "run", mustJSON(map[string]any{"action": "move", "locale": loc, "paths": []string{"/x"}, "machine": ghost}))
		var e *ext.Error
		if !errors.As(err, &e) || e.Code != "invalid_params" || !strings.Contains(e.Message, "offload") || !strings.Contains(e.Message, "restore") {
			t.Errorf("%s: bad action: %v", loc, err)
		}
	}
	// Every key has all three languages.
	for k := range offloadText[locEN] {
		for _, l := range []string{locZH, locJA} {
			if offloadText[l][k] == "" {
				t.Errorf("%s lacks %s", l, k)
			}
		}
	}
	if got := otr("de", "offline", "x"); got != "x is offline." {
		t.Fatalf("fallback %q", got)
	}
}

func TestOffloadHubComputerByHostName(t *testing.T) {
	old := offloadHostname
	offloadHostname = func() (string, error) { return "Studio-Mac.local", nil }
	defer func() { offloadHostname = old }()
	st := &State{Machines: []*Machine{{ID: "local", Host: "localhost", Name: "local", IsLocal: true}, {ID: "build", Host: "build", Name: "build", Hostname: "build.example.test"}}}
	if m := offloadMachine(st, personaMachine{ID: "m-1", Name: "studio-mac"}); m == nil || m.ID != "local" {
		t.Fatalf("hub computer: %+v", m)
	}
	if m := offloadMachine(st, personaMachine{ID: "m-2", Name: "build"}); m == nil || m.ID != "build" {
		t.Fatalf("by name: %+v", m)
	}
	if m := offloadMachine(st, personaMachine{ID: "m-3", Name: "Build.example.test"}); m == nil || m.ID != "build" {
		t.Fatalf("by host name: %+v", m)
	}
	if m := offloadMachine(st, personaMachine{ID: "m-4", Name: "elsewhere"}); m != nil {
		t.Fatalf("matched %+v", m)
	}
}

func TestOffloadInventoryNeedsInstall(t *testing.T) {
	box := newFakeBox()
	box.cli = false
	a := offloadApp(t, box)
	inv := call(t, a, "inventory", params(nil))
	tool := inv["tool"].(map[string]any)
	if inv["state"] != invNeedsInstall || tool["installed"] != false || tool["name"] != "disklessly" ||
		!strings.Contains(tool["source"].(string), "Homebrew tap nwverse/tap") || !strings.Contains(tool["source"].(string), "studio") {
		t.Fatalf("no cli: %v", inv)
	}
	inv = call(t, a, "inventory", params(map[string]any{"locale": "zh-Hans", "refresh": true}))
	if !strings.Contains(inv["tool"].(map[string]any)["source"].(string), "用 Homebrew 从 nwverse/tap 安装") {
		t.Fatalf("zh source: %v", inv["tool"])
	}

	box.cli, box.old = true, true
	inv = call(t, a, "inventory", params(map[string]any{"refresh": true}))
	tool = inv["tool"].(map[string]any)
	if inv["state"] != invNeedsInstall || tool["installed"] != false || tool["version"] != "0.1.0" || !strings.Contains(inv["detail"].(string), "too old") {
		t.Fatalf("old cli: %v", inv)
	}

	box.old, box.brew = false, false
	box.cli = false
	inv = call(t, a, "inventory", params(map[string]any{"refresh": true}))
	if !strings.Contains(inv["tool"].(map[string]any)["source"].(string), "cannot be installed automatically") {
		t.Fatalf("no route: %v", inv["tool"])
	}
}

func TestOffloadNeedsInstallThenInstallAndOffload(t *testing.T) {
	box := newFakeBox()
	box.cli = false
	a := offloadApp(t, box)
	paths := []string{fakeHome + "/Movies/Big.mov", "/Applications/Editor.app"}

	res := call(t, a, "run", params(map[string]any{"action": "offload", "paths": paths, "request_id": "r1"}))
	if res["status"] != "needs_install" || res["tool"].(map[string]any)["installed"] != false || res["tool"].(map[string]any)["source"] == "" {
		t.Fatalf("without install: %v", res)
	}
	a.waitJobs()
	if got := box.commands(); len(got) != 0 {
		t.Fatalf("declined install still ran %v", got)
	}

	res = call(t, a, "run", params(map[string]any{"action": "offload", "paths": paths, "install": true, "request_id": "r1"}))
	if res["status"] != "started" || res["job"] == "" {
		t.Fatalf("with install: %v", res)
	}
	a.waitJobs()
	if got, want := box.commands(), []string{"brew", "offload " + fakeHome + "/Movies/Big.mov", "pack /Applications/Editor.app"}; !slices.Equal(got, want) {
		t.Fatalf("commands %v, want %v", got, want)
	}
	j := a.offloadData.jobs[res["job"].(string)]
	if want := []string{phaseInstall, phaseCheck, phaseUpload, phaseVerify, phaseCleanup}; !slices.Equal(compactPhases(j.phases), want) {
		t.Fatalf("phases %v, want %v", j.phases, want)
	}
	st := call(t, a, "job", params(map[string]any{"job": res["job"]}))
	if st["state"] != ojDone || st["phase"] != phaseCleanup || st["freed_bytes"].(float64) != 2<<20 {
		t.Fatalf("job %v", st)
	}
	if st["done_bytes"].(float64) != st["total_bytes"].(float64) || st["total_bytes"].(float64) != 2*4096 {
		t.Fatalf("bytes %v / %v", st["done_bytes"], st["total_bytes"])
	}
	for _, x := range st["items"].([]any) {
		if x.(map[string]any)["status"] != "done" {
			t.Fatalf("item %v", x)
		}
	}
	// The job is still there long after it finished, then forgotten.
	a.now = func() time.Time { return t0.Add(15 * time.Minute) }
	call(t, a, "job", params(map[string]any{"job": res["job"]}))
	a.now = func() time.Time { return t0.Add(time.Hour) }
	if _, err := a.offload(context.Background(), "job", mustJSON(params(map[string]any{"job": res["job"]}))); !isCode(err, "not_found") {
		t.Fatalf("old job: %v", err)
	}
}

// compactPhases drops repeats of the per-item upload/verify cycle.
func compactPhases(p []string) []string {
	var out []string
	for _, s := range p {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func TestOffloadDryRunRefusals(t *testing.T) {
	box := newFakeBox()
	box.cli = false
	a := offloadApp(t, box)
	cases := map[string]string{
		"relative/path":                    "bad_path",
		fakeHome + "/Movies/../Movies/x":   "bad_path",
		fakeHome + "/Documents":            "standard_folder",
		fakeHome + "/Library/Caches/thing": "app_managed",
		fakeHome + "/link":                 "symlink",
		fakeHome + "/.ssh/id_ed25519":      "credentials",
		fakeHome + "/missing":              "not_found",
		"/etc/hosts":                       "outside_home",
		"/Applications/Packed.app":         "already_offloaded",
		fakeHome + "/Movies/Big.mov":       "",
		fakeHome + "/Projects/old-dataset": "",
		"/Applications/Editor.app":         "",
	}
	var paths []string
	for p := range cases {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	res := call(t, a, "run", params(map[string]any{"action": "offload", "paths": paths, "dry_run": true}))
	if res["status"] != "ok" || res["tool"].(map[string]any)["installed"] != false {
		t.Fatalf("dry run %v", res)
	}
	for _, x := range res["items"].([]any) {
		it := x.(map[string]any)
		p := it["path"].(string)
		if it["ok"] != (cases[p] == "") {
			t.Errorf("%s: ok %v, want code %q", p, it["ok"], cases[p])
		}
		if cases[p] != "" && it["reason"] != refusalText("en", cases[p], "") {
			t.Errorf("%s: reason %q, want the %s sentence", p, it["reason"], cases[p])
		}
	}
	if got := box.commands(); len(got) != 0 {
		t.Fatalf("dry run ran %v", got)
	}

	// Everything refused: nothing starts.
	box.cli = true
	res = call(t, a, "run", params(map[string]any{"action": "offload", "paths": []string{fakeHome + "/Documents", "/etc/hosts"}, "locale": "ja"}))
	if res["status"] != "refused" || len(res["items"].([]any)) != 2 || !strings.Contains(res["items"].([]any)[0].(map[string]any)["reason"].(string), "標準フォルダ") {
		t.Fatalf("refused %v", res)
	}
	a.waitJobs()
	if got := box.commands(); len(got) != 0 {
		t.Fatalf("refused run ran %v", got)
	}
	if _, err := a.offload(context.Background(), "run", mustJSON(params(map[string]any{"action": "offload", "paths": make([]string, 51)}))); !isCode(err, "invalid_params") {
		t.Fatalf("51 paths: %v", err)
	}
	// Apps only on a Mac.
	checks, err := checkOffloadPaths(context.Background(), box.run, &Machine{ID: "studio"}, "linux-amd64", offloadKinds, []string{"/Applications/Editor.app"}, "en")
	if err != nil || checks[0].Code != "app_platform" {
		t.Fatalf("linux app: %+v %v", checks, err)
	}
}

func TestOffloadPartialRefusalSkips(t *testing.T) {
	box := newFakeBox()
	a := offloadApp(t, box)
	res := call(t, a, "run", params(map[string]any{"action": "offload", "paths": []string{fakeHome + "/Movies/Big.mov", "/etc/hosts"}}))
	if res["status"] != "started" {
		t.Fatalf("%v", res)
	}
	a.waitJobs()
	st := call(t, a, "job", params(map[string]any{"job": res["job"]}))
	items := st["items"].([]any)
	if st["state"] != ojDone || items[0].(map[string]any)["status"] != "done" || items[1].(map[string]any)["status"] != "skipped" || items[1].(map[string]any)["error"] == "" {
		t.Fatalf("job %v", st)
	}
	if _, phases := st["phase"], a.offloadData.jobs[res["job"].(string)].phases; phases[0] != phaseCheck {
		t.Fatalf("installed CLI still had phases %v", phases)
	}
}

func TestOffloadRestore(t *testing.T) {
	box := newFakeBox()
	a := offloadApp(t, box)
	call(t, a, "inventory", params(nil))
	ids := []string{"file:f1", "app:com.example.Editor", "data:com.example.Editor", "session:s1", "file:nope"}
	dry := call(t, a, "run", params(map[string]any{"action": "restore", "items": ids, "dry_run": true}))
	if dry["status"] != "ok" || dry["items"].([]any)[4].(map[string]any)["ok"] != false || dry["items"].([]any)[0].(map[string]any)["ok"] != true {
		t.Fatalf("dry restore %v", dry)
	}
	res := call(t, a, "run", params(map[string]any{"action": "restore", "items": ids}))
	if res["status"] != "started" {
		t.Fatalf("restore %v", res)
	}
	a.waitJobs()
	want := []string{"fetch /Users/dev/Movies/Raw.disklessly", "unpack com.example.Editor", "disable com.example.Editor", "restore s1"}
	if got := box.commands(); !slices.Equal(got, want) {
		t.Fatalf("commands %v, want %v", got, want)
	}
	st := call(t, a, "job", params(map[string]any{"job": res["job"]}))
	if st["state"] != ojDone || compactPhases(a.offloadData.jobs[res["job"].(string)].phases)[1] != phaseDownload {
		t.Fatalf("job %v", st)
	}
	items := st["items"].([]any)
	if items[4].(map[string]any)["status"] != "skipped" || items[4].(map[string]any)["id"] != "file:nope" {
		t.Fatalf("unknown item %v", items[4])
	}
	// Restoring only unknown ids refuses.
	res = call(t, a, "run", params(map[string]any{"action": "restore", "items": []string{"file:nope"}}))
	if res["status"] != "refused" {
		t.Fatalf("unknown only %v", res)
	}
}

func TestOffloadFailureAndCancel(t *testing.T) {
	box := newFakeBox()
	box.entries[fakeHome+"/Movies/fail.mov"] = "file"
	a := offloadApp(t, box)
	res := call(t, a, "run", params(map[string]any{"action": "offload", "paths": []string{fakeHome + "/Movies/fail.mov"}}))
	a.waitJobs()
	st := call(t, a, "job", params(map[string]any{"job": res["job"]}))
	if st["state"] != ojFailed || !strings.Contains(st["error"].(string), "open in another app") {
		t.Fatalf("failed job %v", st)
	}

	box.gate, box.started = make(chan struct{}), make(chan string, 4)
	res = call(t, a, "run", params(map[string]any{"action": "offload", "paths": []string{fakeHome + "/Movies/Big.mov", fakeHome + "/Projects/old-dataset"}}))
	if res["status"] != "started" {
		t.Fatalf("%v", res)
	}
	<-box.started
	// A second run on the same machine waits for the first: refused as busy.
	busy := call(t, a, "run", params(map[string]any{"action": "offload", "paths": []string{fakeHome + "/Movies/Big.mov"}}))
	if busy["status"] != "refused" || busy["items"].([]any)[0].(map[string]any)["reason"] != otr("en", "busy", "studio") {
		t.Fatalf("busy %v", busy)
	}
	running := call(t, a, "job", params(map[string]any{"job": res["job"]}))
	if running["state"] != ojRunning || running["phase"] != phaseUpload {
		t.Fatalf("running %v", running)
	}
	call(t, a, "cancel", params(map[string]any{"job": res["job"]}))
	close(box.gate)
	a.waitJobs()
	st = call(t, a, "job", params(map[string]any{"job": res["job"]}))
	items := st["items"].([]any)
	if st["state"] != ojCanceled || items[0].(map[string]any)["status"] != "done" || items[1].(map[string]any)["status"] != "skipped" {
		t.Fatalf("canceled %v", st)
	}
	if _, err := a.offload(context.Background(), "cancel", mustJSON(params(map[string]any{"job": "off-missing"}))); !isCode(err, "not_found") {
		t.Fatalf("cancel unknown: %v", err)
	}
	other := personaMachine{ID: "elsewhere"}
	if _, err := a.offload(context.Background(), "job", mustJSON(map[string]any{"job": res["job"], "machine": other})); !isCode(err, "not_found") {
		t.Fatalf("job of another machine: %v", err)
	}
}

func TestMCPFilesOffload(t *testing.T) {
	box := newFakeBox()
	a := offloadApp(t, box)
	srv := &mcpServer{store: a.store, runner: box.run}
	out, err := srv.call(context.Background(), "disklessly_files_offload", mustJSON(map[string]any{"machine": "studio", "paths": []string{fakeHome + "/Movies/Big.mov", "/etc/hosts"}, "dry_run": true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(box.commands()) != 0 || out.(map[string]any)["cli_installed"] != true {
		t.Fatalf("dry run %v %v", out, box.commands())
	}
	out, err = srv.call(context.Background(), "disklessly_files_offload", mustJSON(map[string]any{"machine": "studio", "paths": []string{fakeHome + "/Movies/Big.mov", "/etc/hosts"}}))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"status":"done"`) || !strings.Contains(string(raw), `"status":"refused"`) || !slices.Equal(box.commands(), []string{"offload " + fakeHome + "/Movies/Big.mov"}) {
		t.Fatalf("offload %s %v", raw, box.commands())
	}
	box.cli = false
	if _, err := srv.call(context.Background(), "disklessly_files_offload", mustJSON(map[string]any{"machine": "studio", "paths": []string{fakeHome + "/Movies/Big.mov"}})); err == nil || !strings.Contains(err.Error(), "disklessly_cli_install") {
		t.Fatalf("no cli: %v", err)
	}
}

func TestOffloadThroughProgram(t *testing.T) {
	a := offloadApp(t, newFakeBox())
	h := program(a).Namespaces[offloadNamespace]
	if h == nil {
		t.Fatal("no offload namespace")
	}
	res, err := h(context.Background(), "inventory", mustJSON(params(nil)))
	if err != nil || res.(map[string]any)["state"] != invReady {
		t.Fatalf("%v %v", res, err)
	}
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func isCode(err error, code string) bool {
	var e *ext.Error
	return errors.As(err, &e) && e.Code == code
}

// TestOffloadScriptsUnderSh runs the real inventory, check and item scripts under /bin/sh against a stand-in CLI
// and a synthetic home folder.
func TestOffloadScriptsUnderSh(t *testing.T) {
	home := t.TempDir()
	sd := filepath.Join(home, "state")
	repo := filepath.Join(home, "repo")
	for _, d := range []string{filepath.Join(sd, "files"), filepath.Join(repo, "manifests"), filepath.Join(home, "Movies"), filepath.Join(home, "Documents"), filepath.Join(home, "Applications", "Tool.app", "Contents")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ph := filepath.Join(home, "Movies", "Raw.disklessly")
	write(filepath.Join(sd, "files", "offloaded.json"), "[\n  {\n    \"createdAt\" : \"2026-08-01T00:00:00Z\",\n    \"id\" : \"f1\",\n    \"kind\" : \"folder\",\n    \"name\" : \"Raw\",\n    \"placeholderPath\" : \""+ph+"\",\n    \"size\" : 9000000\n  },\n  {\n    \"id\" : \"f2\",\n    \"placeholderPath\" : \""+home+"/gone.disklessly\",\n    \"size\" : 1\n  }\n]\n")
	write(ph, "{\n  \"kind\" : \"folder\",\n  \"originalPath\" : \""+home+"/Movies/Raw\",\n  \"storedSize\" : 8000000\n}\n")
	write(filepath.Join(repo, "manifests", "img1.json"), "{\n  \"imageId\" : \"img1\", \"bundleId\" : \"com.example.Tool\", \"name\" : \"Tool\",\n  \"image\" : {\"size\" : 4000000}, \"app\" : {\"originalPath\" : \"\\/Applications\\/Tool.app\"}, \"createdAt\" : \"2026-09-01T00:00:00Z\"\n}\n")
	write(filepath.Join(home, "Movies", "big.mov"), "movie")
	if err := os.Symlink(filepath.Join(home, "Movies"), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(home, "disklessly")
	write(cli, `#!/bin/sh
[ "$1" = --json ] && shift
case "$1" in
  version) echo "disklessly 0.3.2" ;;
  offload)
    if [ -z "$2" ]; then echo '{"ok":false,"error":{"code":2,"message":"usage: disklessly offload PATH...","name":"EX_USAGE"}}'; exit 2; fi
    mv "$2" "$2.disklessly" && printf '{\n  "ok" : true,\n  "sizeBytes" : 5\n}\n' ;;
  status) printf '{\n  "repoPath" : "%s",\n  "apps" : {"com.example.Tool" : {"imageId" : "img1", "state" : "idle"}}\n}\n' "$REPO" ;;
  list) echo '[{"imageId":"img1","bundleId":"com.example.Tool","name":"Tool"}]' ;;
  data) echo '{"ok":true,"apps":[]}' ;;
  *) echo '{"ok":false}'; exit 1 ;;
esac
`)
	if err := os.Chmod(cli, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, _ *Machine, script string, _ time.Duration) (string, error) {
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "DISKLESSLY_BIN=" + cli, "DISKLESSLY_STATE_DIR=" + sd, "REPO=" + repo}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	m := &Machine{ID: "box"}

	out, err := run(context.Background(), m, inventoryScript(), 0)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	sections := splitSections(out)
	pr := parseProbe(sections)
	if pr.noCLI || !pr.offloadOK || pr.home != home || shortVersion(pr.cli.Version) != "0.3.2" {
		t.Fatalf("probe %+v\n%s", pr, out)
	}
	items, partial := parseInventory(sections, pr, nil, t0)
	if partial || len(items) != 2 {
		t.Fatalf("items %+v (partial %v)\n%s", items, partial, out)
	}
	byID := map[string]offloadItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	if f := byID["file:f1"]; f.Kind != "dir" || f.Bytes != 9000000 || f.StoredBytes != 8000000 || f.StubPath != ph || f.OriginPath != home+"/Movies/Raw" {
		t.Fatalf("file %+v", f)
	}
	if a := byID["app:com.example.Tool"]; a.Bytes != 4000000 || a.OriginPath != "/Applications/Tool.app" {
		t.Fatalf("app %+v", a)
	}

	paths := []string{home + "/Movies/big.mov", home + "/Documents", home + "/link", home + "/nothing", home + "/Applications/Tool.app"}
	checks, err := checkOffloadPaths(context.Background(), run, m, "darwin-arm64", offloadKinds, paths, "en")
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, c := range checks {
		codes = append(codes, c.Code)
	}
	if want := []string{"", "standard_folder", "symlink", "not_found", ""}; !slices.Equal(codes, want) {
		t.Fatalf("codes %q, want %q", codes, want)
	}
	if checks[0].kind != "file" || checks[4].kind != "app" {
		t.Fatalf("kinds %+v", checks)
	}

	out, err = run(context.Background(), m, offloadItemScript(offloadJobItem{Path: home + "/Movies/big.mov", kind: "file"}), 0)
	o := parseItem(out)
	if err != nil || o.rc != 0 || !o.ok || !o.verified || o.bytes != 5 {
		t.Fatalf("item %+v %v\n%s", o, err, out)
	}
	out, err = run(context.Background(), m, sizeScript([]string{home + "/Movies/big.mov.disklessly"}), 0)
	if err != nil || !strings.HasPrefix(out, "@@SIZE\t0\t") {
		t.Fatalf("size %q %v", out, err)
	}
	if n, ok := availBytes(context.Background(), run, m); !ok || n == 0 {
		t.Fatalf("avail %d %v", n, ok)
	}
}

// A long session title or CLI error is clipped to what Persona's offload schema allows, ids are kept whole, and sizes
// stay exact integers.
func TestOffloadAnswersFitTheSchemaBounds(t *testing.T) {
	long := strings.Repeat("长", 450)
	got := fitOffloadSchema(map[string]any{"state": "ready", "detail": long, "total_bytes": uint64(1) << 40,
		"tool":  offloadTool{Name: "disklessly", Version: strings.Repeat("9", 60), Installed: true, Source: long},
		"items": []offloadItem{{ID: "session:" + strings.Repeat("x", 150), Name: long, Kind: "session", Bytes: 7, Group: long, Restorable: true}},
	}).(map[string]any)
	raw, _ := json.Marshal(got)
	var back struct {
		Detail     string `json:"detail"`
		TotalBytes uint64 `json:"total_bytes"`
		Tool       offloadTool
		Items      []offloadItem
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	n := func(s string) int { return len([]rune(s)) }
	if n(back.Detail) != 300 || !strings.HasSuffix(back.Detail, "…") || back.TotalBytes != 1<<40 {
		t.Fatalf("detail %d, total %d", n(back.Detail), back.TotalBytes)
	}
	if n(back.Tool.Version) != 40 || n(back.Tool.Source) != 300 || back.Tool.Name != "disklessly" {
		t.Fatalf("tool %+v", back.Tool)
	}
	if it := back.Items[0]; n(it.Name) != 200 || n(it.Group) != 200 || it.ID != "session:"+strings.Repeat("x", 150) || it.Bytes != 7 {
		t.Fatalf("item %+v", it)
	}
}

func TestOffloadInventoryUnescapesFoundationPlaceholderPaths(t *testing.T) {
	home := t.TempDir()
	sd := filepath.Join(home, "state")
	repo := filepath.Join(home, "repo")
	for _, d := range []string{filepath.Join(sd, "files"), filepath.Join(repo, "manifests"), filepath.Join(home, "Movies")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	escapeSlashes := func(s string) string { return strings.ReplaceAll(s, "/", `\/`) }

	ph := filepath.Join(home, "Movies", "Raw.disklessly")
	// Foundation JSONEncoder escapes '/' as '\/' by default in offloaded.json, placeholders, and status.
	write(filepath.Join(sd, "files", "offloaded.json"), "[\n  {\n    \"createdAt\" : \"2026-08-01T00:00:00Z\",\n    \"id\" : \"f1\",\n    \"kind\" : \"folder\",\n    \"name\" : \"Raw\",\n    \"placeholderPath\" : \""+escapeSlashes(ph)+"\",\n    \"size\" : 9000000\n  }\n]\n")
	write(ph, "{\n  \"kind\" : \"folder\",\n  \"originalPath\" : \""+escapeSlashes(filepath.Join(home, "Movies", "Raw"))+"\",\n  \"storedSize\" : 8000000\n}\n")

	cli := filepath.Join(home, "disklessly")
	write(cli, `#!/bin/sh
[ "$1" = --json ] && shift
case "$1" in
  version) echo "disklessly 0.3.2" ;;
  status) printf '{"repoPath":"%s","apps":{}}\n' "$REPO_ESC" ;;
  list) echo '[]' ;;
  data) echo '{"ok":true,"apps":[]}' ;;
  *) echo '{"ok":false}'; exit 1 ;;
esac
`)
	if err := os.Chmod(cli, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", inventoryScript())
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"DISKLESSLY_BIN=" + cli,
		"DISKLESSLY_STATE_DIR=" + sd,
		"REPO_ESC=" + escapeSlashes(repo),
	}
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	sections := splitSections(string(raw))
	pr := parseProbe(sections)
	items, partial := parseInventory(sections, pr, nil, t0)
	if partial || len(items) != 1 {
		t.Fatalf("expected 1 offloaded file item from escaped JSON paths, got %d (partial=%v): %+v\nraw:\n%s", len(items), partial, items, raw)
	}
	if it := items[0]; it.ID != "file:f1" || it.StubPath != ph || it.OriginPath != filepath.Join(home, "Movies", "Raw") || it.StoredBytes != 8000000 || !it.Restorable {
		t.Fatalf("unexpected item: %+v", it)
	}
}

func TestCheckOffloadPathsEnforcesRedlinesOnMacOSApps(t *testing.T) {
	home := t.TempDir()
	sd := filepath.Join(home, "state")
	dirs := []string{
		filepath.Join(home, "Applications", "Tool.app"),
		filepath.Join(home, "Applications", "Xcode.app", "Contents", "Applications", "Simulator.app"),
		filepath.Join(home, "Library", "Application Support", "Vendor", "Helper.app"),
		filepath.Join(home, ".Trash", "Old.app"),
		filepath.Join(home, ".ssh", "Secret.app"),
		filepath.Join(sd, "Internal.app"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(_ context.Context, _ *Machine, script string, _ time.Duration) (string, error) {
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "DISKLESSLY_STATE_DIR=" + sd}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	checks, err := checkOffloadPaths(context.Background(), run, &Machine{ID: "mac"}, "darwin-arm64", offloadKinds, dirs, "en")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"", "inside_app", "app_managed", "app_managed", "credentials", "own_data"}
	var got []string
	for _, c := range checks {
		got = append(got, c.Code)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("codes = %q, want %q", got, want)
	}
	if code := appOffloadPolicy("/Applications/Tool.app", home, []string{sd}); code != "" {
		t.Fatalf("/Applications/Tool.app got %q, want allowed", code)
	}
	if code := appOffloadPolicy("/Applications/Xcode.app/Contents/Applications/Simulator.app", home, []string{sd}); code != "inside_app" {
		t.Fatalf("nested /Applications app got %q, want inside_app", code)
	}
	if code := appOffloadPolicy("/opt/Tool.app", home, []string{sd}); code != "outside_home" {
		t.Fatalf("/opt/Tool.app got %q, want outside_home", code)
	}
}

func TestUnixTimestampsClampedNonNegative(t *testing.T) {
	if got := unixText("1969-12-31T23:59:59Z"); got != 0 {
		t.Fatalf("unixText pre-epoch = %d, want 0", got)
	}
	if got := unixOf(json.RawMessage(`-2000000000`)); got != 0 {
		t.Fatalf("unixOf negative = %d, want 0", got)
	}
	if got := unixOf(json.RawMessage(`"1969-01-01T00:00:00Z"`)); got != 0 {
		t.Fatalf("unixOf pre-epoch string = %d, want 0", got)
	}
}
