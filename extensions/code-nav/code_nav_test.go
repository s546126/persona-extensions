package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestManifest(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
}

func TestConformance(t *testing.T) {
	app := &hubApp{call: func(context.Context, string, string, any, any) error { return nil }}
	exttest.Check(t, manifest, exttest.Serve(t, app.program("code-nav", version(t))), json.RawMessage(`{"machines":["m1"]}`))
}

func version(t *testing.T) string {
	var m struct{ Version string }
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	return m.Version
}

func TestSecretPaths(t *testing.T) {
	home := "/home/me"
	for p, secret := range map[string]bool{
		"/home/me/code/app/main.go":          false,
		"/home/me/code/app/.env.example":     false,
		"/home/me/code/app/.env":             true,
		"/home/me/code/app/.env.production":  true,
		"/home/me/code/app/.git/config":      true,
		"/home/me/code/app/certs/server.key": true,
		"/home/me/code/app/certs/server.pem": true,
		"/home/me/.ssh/id_ed25519":           true,
		"/home/me/.persona/hub.json":         true,
		"/home/me/code/app/id_rsa":           true,
	} {
		if got := secretPath(p, home); got != secret {
			t.Errorf("secretPath(%s) = %v, want %v", p, got, secret)
		}
	}
}

func TestReadStaysInsideTheRepositories(t *testing.T) {
	root := t.TempDir()
	proj := writeRepo(t, root)
	outside := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(proj, "notes.go")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	n := newNode(t, root)
	if _, err := n.read(navParams{Path: filepath.Join(proj, "main.go")}); err != nil {
		t.Fatalf("a file of the repository: %v", err)
	}
	for _, p := range []string{outside, link, filepath.Join(proj, "..", "..", "etc", "passwd"), filepath.Join(proj, ".env")} {
		if _, err := n.read(navParams{Path: p}); err == nil {
			t.Errorf("read %s: want refused", p)
		}
	}
}

func TestTreeShowsOnlyOpenFolders(t *testing.T) {
	files := []string{"README.md", "cmd/app/main.go", "go.mod", "pkg/a/a.go", "pkg/b/b.go", "pkg/doc.go"}
	rp := repo{Name: "app", Path: "/w/app"}
	paths := func(c wire.Component) string {
		var out []string
		for _, e := range c.Entries {
			p := e.Path
			if e.Kind == wire.TreeEntryKindDir {
				p += "/"
			}
			out = append(out, p)
		}
		return strings.Join(out, " ")
	}
	s := &state{}
	if got, want := paths(tree(s, rp, files)), "README.md cmd/ go.mod pkg/"; got != want {
		t.Errorf("closed: %q, want %q", got, want)
	}
	s.O = []string{"pkg"}
	if got, want := paths(tree(s, rp, files)), "README.md cmd/ go.mod pkg/a/ pkg/b/ pkg/doc.go"; got != want {
		t.Errorf("pkg open: %q, want %q", got, want)
	}
	s = &state{P: "/w/app/cmd/app/main.go"}
	c := tree(s, rp, files)
	if got, want := paths(c), "README.md cmd/app/main.go go.mod pkg/"; got != want || c.Selected != "cmd/app/main.go" {
		t.Errorf("a file open opens its folders: %q selected %q, want %q", got, c.Selected, want)
	}
	s.toggle("pkg")
	s.toggle("pkg/a")
	s.toggle("pkg")
	if len(s.O) != 0 {
		t.Errorf("closing a folder closes the ones inside: %v", s.O)
	}
}

func TestBackKeepsTheStateSmall(t *testing.T) {
	s := &state{}
	for i := range 100 {
		s.moveTo("/w/app/"+strings.Repeat("x", 400)+strconv.Itoa(i)+".go", i+1, 1)
	}
	if len(s.H) > maxBack || len(ext.EncodeState(s)) > 16<<10 {
		t.Fatalf("%d steps back, %d bytes", len(s.H), len(ext.EncodeState(s)))
	}
}

// The page against a real computer's program and a real gopls: repositories, the folder rows, a file and its
// outline, the names of a marked line, a jump to a definition in another package, quick info, an implementation,
// usages, back and forward, a symbol search and Find in Files, and the UI version 13 navigate. With
// CODE_NAV_REQUIRE_GOPLS=1 a missing gopls fails the test instead of skipping its language-server part.
func TestThePageOnARepository(t *testing.T) {
	root := t.TempDir()
	proj := writeRepo(t, root)
	n := newNode(t, root)
	t.Cleanup(n.cleanup)
	app := &hubApp{machines: []string{"m1"}, call: direct(n)}
	ctx := context.Background()
	check := func(v *wire.View) string {
		t.Helper()
		if err := wire.CheckView(v); err != nil {
			raw, _ := json.Marshal(v)
			t.Fatalf("CheckView: %v: %s", err, raw)
		}
		raw, _ := json.Marshal(v)
		if err := exttest.CheckView(raw, false); err != nil {
			t.Fatalf("exttest.CheckView: %v: %s", err, raw)
		}
		return string(raw)
	}
	act := func(v *wire.View, action string, values map[string]string) *wire.View {
		t.Helper()
		res, err := app.action(ctx, uiReq(v.State, action, values, 12))
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if res.View == nil {
			t.Fatalf("%s: no view, toast %q", action, res.Toast)
		}
		return res.View
	}
	at := func(v *wire.View) state {
		var s state
		_ = json.Unmarshal([]byte(v.State), &s)
		return s
	}

	v, err := app.render(ctx, uiReq("", "", nil, 12))
	if err != nil {
		t.Fatal(err)
	}
	if raw := check(v); !strings.Contains(raw, `"repo":"`+proj+`"`) {
		t.Fatalf("the repository is listed: %s", raw)
	}
	v = act(v, "repo", map[string]string{"repo": proj})
	if raw := check(v); !strings.Contains(raw, `"file_tree"`) || !strings.Contains(raw, "# demo") || !strings.Contains(raw, `"title":"util","subtitle":"文件夹"`) || strings.Contains(raw, ".env") {
		t.Fatalf("the tree, the folder rows and the README: %s", raw)
	}
	v = act(v, "dir", map[string]string{"path": "util"})
	if raw := check(v); !strings.Contains(raw, `"title":"util.go"`) || !strings.Contains(raw, `"breadcrumb"`) {
		t.Fatalf("the util folder: %s", raw)
	}
	v = act(v, "open", map[string]string{"path": "main.go"})
	raw := check(v)
	if !strings.Contains(raw, `"path":"main.go"`) || !strings.Contains(raw, "util.Greet") || !strings.Contains(raw, `"language":"go"`) {
		t.Fatalf("main.go: %s", raw)
	}

	gopls := goplsPath()
	if _, err := exec.LookPath(gopls); err != nil {
		if os.Getenv("CODE_NAV_REQUIRE_GOPLS") == "1" {
			t.Fatalf("gopls is required (CODE_NAV_REQUIRE_GOPLS=1) and not installed at %s", gopls)
		}
		t.Skip("gopls is not installed; the rest needs it")
	}
	if out, err := exec.Command(gopls, "version").Output(); err == nil {
		t.Logf("real language server: %s", strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]))
	}
	if !strings.Contains(raw, `"sym:`) {
		t.Fatalf("the outline: %s", raw)
	}
	// main.go line 7 is `	fmt.Println(util.Greet("you"))`: go to it (a jump the way back remembers), then follow Greet
	// from the line's names (UI 12).
	v = act(v, "goto_line", map[string]string{"goto": "7"})
	raw = check(v)
	if s := at(v); s.L != 7 || !strings.Contains(raw, `"key":"line"`) || !strings.Contains(raw, `"title":"Greet"`) {
		t.Fatalf("the names of line 7: %+v %s", s, raw)
	}
	greet := strconv.Itoa(strings.Index("\tfmt.Println(util.Greet(\"you\"))", "Greet") + 1)
	v = act(v, "def_at", map[string]string{"line": "7", "col": greet})
	s := at(v)
	if raw := check(v); s.P != filepath.Join(proj, "util", "util.go") || s.L != 4 || len(s.H) != 2 || !strings.Contains(raw, `"highlight":{"start":4,"end":4}`) {
		t.Fatalf("the definition of Greet: %+v %s", s, raw)
	}
	v = act(v, "hover_at", map[string]string{"line": "4", "col": "6"})
	if raw := check(v); !strings.Contains(raw, `"key":"hover"`) || !strings.Contains(raw, "func Greet(name string) string") || !strings.Contains(raw, "Greet says hello") {
		t.Fatalf("quick info on Greet: %s", raw)
	}
	v = act(v, "close_hover", nil)
	// util.go line 9 is `type Speaker interface {`: its implementation is Dog, line 14.
	v = act(v, "impl_at", map[string]string{"line": "9", "col": "6"})
	s = at(v)
	if raw := check(v); s.P != filepath.Join(proj, "util", "util.go") || s.L != 14 || len(s.H) != 3 {
		t.Fatalf("the implementation of Speaker: %+v %s", s, raw)
	}
	v = act(v, "refs_at", map[string]string{"line": "4", "col": "6"})
	if raw := check(v); !strings.Contains(raw, "main.go:7") {
		t.Fatalf("the usages of Greet: %s", raw)
	}
	v = act(v, "back", nil)
	if s = at(v); s.P != filepath.Join(proj, "util", "util.go") || s.L != 9 || len(s.N) != 1 {
		t.Fatalf("back to Speaker: %+v", s)
	}
	v = act(v, "back", nil)
	if s = at(v); s.P != filepath.Join(proj, "main.go") || s.L != 7 || len(s.H) != 1 || len(s.N) != 2 {
		t.Fatalf("back to main.go: %+v", s)
	}
	if raw := check(v); !strings.Contains(raw, `"key":"forward"`) || !strings.Contains(raw, `"key":"back"`) {
		t.Fatalf("back and forward: %s", raw)
	}
	v = act(v, "forward", nil)
	if s = at(v); s.P != filepath.Join(proj, "util", "util.go") || s.L != 9 {
		t.Fatalf("forward to Speaker: %+v", s)
	}
	v = act(v, "forward", nil)
	if s = at(v); s.L != 14 || len(s.N) != 0 || len(s.H) != 3 {
		t.Fatalf("forward to Dog: %+v", s)
	}
	v = act(v, "search", map[string]string{"q": "Greet"})
	if raw := check(v); !strings.Contains(raw, `"hit:0"`) || !strings.Contains(raw, "util.go:4") || !strings.Contains(raw, `"segmented"`) {
		t.Fatalf("the symbol search: %s", raw)
	}
	v = act(v, "scope", map[string]string{"scope": "text"})
	if raw := check(v); !strings.Contains(raw, `"g:0"`) || !strings.Contains(raw, "main.go:7") || !strings.Contains(raw, "util/util.go:4") {
		t.Fatalf("Find in Files: %s", raw)
	}
	v = act(v, "jump", map[string]string{"path": filepath.Join(proj, "main.go"), "line": "1", "col": "1"})
	v = act(v, "nav", map[string]string{"line": "1", "column": "1", "intent": "definition"})
	if raw := check(v); !strings.Contains(raw, `"note"`) {
		t.Fatalf("no definition on `package`: %s", raw)
	}
	// the UI version 13 way: a name chosen in the code
	v = act(v, "nav", map[string]string{"line": "7", "column": greet, "word": "Greet", "intent": "definition"})
	if s = at(v); s.P != filepath.Join(proj, "util", "util.go") || s.L != 4 {
		t.Fatalf("navigate to Greet: %+v", s)
	}
	v = act(v, "nav", map[string]string{"intent": "back"})
	if s = at(v); s.P != filepath.Join(proj, "main.go") || s.L != 7 {
		t.Fatalf("navigate back: %+v", s)
	}
}

// direct is a node caller that answers from n, through JSON as the hub's nodes.call does.
func direct(n *node) nodeCaller {
	return func(ctx context.Context, machine, method string, params, out any) error {
		raw, _ := json.Marshal(params)
		res, err := n.handle(ctx, method, raw)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(res)
		return json.Unmarshal(b, out)
	}
}

func uiReq(st, action string, values map[string]string, version int) *ext.UIRequest {
	r := &ext.UIRequest{}
	r.Contribution, r.Size, r.UIVersion, r.Locale, r.State, r.Action, r.Values = codeUI, wire.SizeFull, version, "zh-Hans", st, action, values
	return r
}

func writeRepo(t *testing.T, root string) string {
	t.Helper()
	proj := filepath.Join(root, "demo")
	files := map[string]string{
		"go.mod":    "module example.com/demo\n\ngo 1.22\n",
		"README.md": "# demo\n",
		"main.go":   "package main\n\nimport \"example.com/demo/util\"\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(util.Greet(\"you\"))\n}\n",
		"util/util.go": "package util\n\n// Greet says hello.\nfunc Greet(name string) string {\n\treturn \"hello \" + name\n}\n\n" +
			"// Speaker says something.\ntype Speaker interface {\n\tSay() string\n}\n\n// Dog is a Speaker.\ntype Dog struct{}\n\n// Say barks.\nfunc (Dog) Say() string { return \"woof\" }\n",
		".env": "TOKEN=x\n",
	}
	for name, body := range files {
		p := filepath.Join(proj, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// t.TempDir may be behind a link (macOS /var): the program answers with links followed
	real, err := filepath.EvalSymlinks(proj)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func newNode(t *testing.T, root string) *node {
	t.Helper()
	n := &node{}
	n.setup(config{Machines: []string{"m1"}, Roots: []string{root}, Gopls: goplsPath()})
	deadline := time.Now().Add(time.Second)
	for len(n.reposResult().Repos) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return n
}

func goplsPath() string {
	if p, err := exec.LookPath("gopls"); err == nil {
		return p
	}
	return filepath.Join(homeDir(), "go", "bin", "gopls")
}
