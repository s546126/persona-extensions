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

// The page against a real computer's program: repositories, the tree, a file and its outline, a jump to a
// definition in another package and back, the usages of a function, a symbol search.
func TestThePageOnARepository(t *testing.T) {
	root := t.TempDir()
	proj := writeRepo(t, root)
	n := newNode(t, root)
	t.Cleanup(n.cleanup)
	app := &hubApp{machines: []string{"m1"}, call: func(ctx context.Context, machine, method string, params, out any) error {
		raw, _ := json.Marshal(params)
		res, err := n.handle(ctx, method, raw)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(res)
		return json.Unmarshal(b, out)
	}}
	ctx := context.Background()
	uiVersion := 13
	req := func(st string, action string, values map[string]string) *ext.UIRequest {
		r := &ext.UIRequest{}
		r.Contribution, r.Size, r.UIVersion, r.Locale, r.State, r.Action, r.Values = codeUI, wire.SizeFull, uiVersion, "zh-Hans", st, action, values
		return r
	}
	check := func(v *wire.View) string {
		t.Helper()
		raw, _ := json.Marshal(v)
		if err := exttest.CheckView(raw, false); err != nil {
			t.Fatalf("%v: %s", err, raw)
		}
		return string(raw)
	}
	act := func(v *wire.View, action string, values map[string]string) *wire.View {
		t.Helper()
		res, err := app.action(ctx, req(v.State, action, values))
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if res.View == nil {
			t.Fatalf("%s: no view, toast %q", action, res.Toast)
		}
		return res.View
	}

	v, err := app.render(ctx, req("", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if raw := check(v); !strings.Contains(raw, `"repo":"`+proj+`"`) {
		t.Fatalf("the repository is listed: %s", raw)
	}
	v = act(v, "repo", map[string]string{"repo": proj})
	if raw := check(v); !strings.Contains(raw, `"file_tree"`) || !strings.Contains(raw, "# demo") {
		t.Fatalf("the tree and the README: %s", raw)
	}
	v = act(v, "open", map[string]string{"path": "main.go"})
	raw := check(v)
	if !strings.Contains(raw, `"path":"main.go"`) || !strings.Contains(raw, "util.Greet") {
		t.Fatalf("main.go: %s", raw)
	}
	if !strings.Contains(raw, `"navigate":{"id":"nav"}`) {
		t.Fatalf("a Go file's code follows names at UI version 13: %s", raw)
	}
	uiVersion = 12
	if raw := check(act(v, "open", map[string]string{"path": "main.go"})); strings.Contains(raw, `"navigate"`) {
		t.Fatalf("no navigate below UI version 13: %s", raw)
	}
	uiVersion = 13
	if raw := check(act(v, "open", map[string]string{"path": "README.md"})); strings.Contains(raw, `"navigate"`) {
		t.Fatalf("no navigate where no language server reads the file: %s", raw)
	}

	if _, err := exec.LookPath(goplsPath()); err != nil {
		t.Skip("gopls is not installed; the rest needs it")
	}
	if !strings.Contains(raw, `"sym:`) {
		t.Fatalf("the outline: %s", raw)
	}
	// main.go line 7 is `	fmt.Println(util.Greet("you"))`: Greet starts at column 19.
	v = act(v, "nav", map[string]string{"line": "7", "column": "20", "intent": "definition"})
	var s state
	_ = json.Unmarshal([]byte(v.State), &s)
	if raw := check(v); s.P != filepath.Join(proj, "util", "util.go") || s.L != 4 || len(s.H) != 1 || !strings.Contains(raw, `"highlight":{"start":4,"end":4}`) {
		t.Fatalf("the definition of Greet: %+v %s", s, raw)
	}
	v = act(v, "refs_at", map[string]string{"line": "4", "column": "6", "col": "6"})
	if raw := check(v); !strings.Contains(raw, "main.go:7") {
		t.Fatalf("the usages of Greet: %s", raw)
	}
	v = act(v, "back", nil)
	s = state{}
	_ = json.Unmarshal([]byte(v.State), &s)
	if s.P != filepath.Join(proj, "main.go") || s.L != 7 || len(s.H) != 0 {
		t.Fatalf("back to main.go: %+v", s)
	}
	v = act(v, "search", map[string]string{"q": "Greet"})
	if raw := check(v); !strings.Contains(raw, `"hit:0"`) || !strings.Contains(raw, "util.go:4") {
		t.Fatalf("the symbol search: %s", raw)
	}
	v = act(v, "nav", map[string]string{"line": "1", "column": "1", "intent": "definition"})
	if raw := check(v); !strings.Contains(raw, `"note"`) {
		t.Fatalf("no definition on `package`: %s", raw)
	}
}

func writeRepo(t *testing.T, root string) string {
	t.Helper()
	proj := filepath.Join(root, "demo")
	files := map[string]string{
		"go.mod":       "module example.com/demo\n\ngo 1.22\n",
		"README.md":    "# demo\n",
		"main.go":      "package main\n\nimport \"example.com/demo/util\"\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(util.Greet(\"you\"))\n}\n",
		"util/util.go": "package util\n\n// Greet says hello.\nfunc Greet(name string) string {\n\treturn \"hello \" + name\n}\n",
		".env":         "TOKEN=x\n",
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
