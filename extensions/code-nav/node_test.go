package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

// The test binary doubles as a small language server: started with --stdio (as the page starts a Python server), it
// answers over stdin and stdout with fixed results. It lets the tests drive every language the page knows through a
// server path from the settings, without the real servers installed.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--stdio" {
		fakeServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeServer() {
	r := bufio.NewReader(os.Stdin)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(b), b)
	}
	root := ""
	for {
		msg, err := readMessage(r)
		if err != nil {
			return
		}
		var p struct {
			RootURI      string `json:"rootUri"`
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		loc := func(rel string, line int) map[string]any {
			return map[string]any{"uri": fileURI(filepath.Join(root, rel)), "range": map[string]any{
				"start": map[string]int{"line": line, "character": 4}, "end": map[string]int{"line": line, "character": 9}}}
		}
		var result any
		switch msg.Method {
		case "initialize":
			root = uriPath(p.RootURI)
			result = map[string]any{"capabilities": map[string]any{"positionEncoding": "utf-16"}}
		case "textDocument/hover":
			result = map[string]any{"contents": map[string]string{"kind": "markdown",
				"value": "```python\ndef greet(name: str) -> str\n```\nSays hello. ![badge](https://example.com/b.svg) [docs](https://example.com/docs) [local](./README.md)"}}
		case "textDocument/definition":
			result = []any{loc("app.py", 0)}
		case "textDocument/implementation":
			result = []any{loc("app.py", 0), loc("lib/impl.py", 0)}
		case "textDocument/references":
			result = []any{loc("app.py", 3)}
		case "textDocument/documentSymbol":
			result = []any{map[string]any{"name": "greet", "kind": 12, "detail": "(name)",
				"range":          map[string]any{"start": map[string]int{"line": 0, "character": 0}, "end": map[string]int{"line": 1, "character": 0}},
				"selectionRange": map[string]any{"start": map[string]int{"line": 0, "character": 4}, "end": map[string]int{"line": 0, "character": 9}}}}
		case "workspace/symbol":
			result = []any{map[string]any{"name": "greet", "kind": 12, "location": loc("app.py", 0)}}
		case "exit":
			return
		}
		if len(msg.ID) > 0 {
			send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		}
	}
}

func testBinary(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func realDir(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A Python repository with the server from the settings: found, started on the first question, asked for an outline,
// quick info, implementations and a symbol search, stopped when idle and started again when asked.
func TestServerFromTheSettings(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "py")
	writeFiles(t, proj, map[string]string{
		"pyproject.toml": "[project]\nname = \"py\"\n",
		"app.py":         "def greet(name):\n    return 'hi ' + name\n\nprint(greet('you'))\n",
		"lib/impl.py":    "def greet(name):\n    return name\n",
	})
	proj = realDir(t, proj)
	n := &node{}
	n.setup(config{Roots: []string{root}, Python: testBinary(t), Gopls: "/nonexistent/gopls", IdleMinutes: 1})
	t.Cleanup(n.cleanup)
	if res := n.reposResult(); !strings.Contains(strings.Join(res.Lsp, " "), "python") || strings.Contains(strings.Join(res.Lsp, " "), "go") {
		t.Fatalf("the languages: %v", res.Lsp)
	}
	if len(n.servers) != 0 {
		t.Fatalf("no server before the first question: %d", len(n.servers))
	}
	app := &hubApp{machines: []string{"m1"}, call: direct(n)}
	ctx := context.Background()
	act := func(st, action string, values map[string]string) *wire.View {
		t.Helper()
		res, err := app.action(ctx, uiReq(st, action, values, 12))
		if err != nil || res.View == nil {
			t.Fatalf("%s: %v %+v", action, err, res)
		}
		if err := wire.CheckView(res.View); err != nil {
			raw, _ := json.Marshal(res.View)
			t.Fatalf("%s: CheckView: %v: %s", action, err, raw)
		}
		return res.View
	}
	v := act("", "repo", map[string]string{"repo": proj})
	v = act(v.State, "open", map[string]string{"path": "app.py"})
	raw, _ := json.Marshal(v)
	if !strings.Contains(string(raw), `"title":"greet"`) || !strings.Contains(string(raw), `"language":"python"`) {
		t.Fatalf("the outline from the server: %s", raw)
	}
	v = act(v.State, "hover_at", map[string]string{"line": "1", "col": "5"})
	raw, _ = json.Marshal(v)
	if !strings.Contains(string(raw), "def greet(name: str)") || strings.Contains(string(raw), "README.md)") || strings.Contains(string(raw), "![badge]") {
		t.Fatalf("quick info, with its image and its local link made words: %s", raw)
	}
	v = act(v.State, "impl_at", map[string]string{"line": "1", "col": "5"})
	raw, _ = json.Marshal(v)
	if !strings.Contains(string(raw), "lib/impl.py:1") || !strings.Contains(string(raw), "「greet」的实现（2）") {
		t.Fatalf("two implementations are listed: %s", raw)
	}
	v = act(v.State, "search", map[string]string{"q": "greet"})
	raw, _ = json.Marshal(v)
	if !strings.Contains(string(raw), `"hit:0"`) {
		t.Fatalf("the workspace symbol search through the Python server: %s", raw)
	}

	if stopped := n.reapIdle(time.Now()); stopped != 0 {
		t.Fatalf("a server just used is kept: %d stopped", stopped)
	}
	if stopped := n.reapIdle(time.Now().Add(2 * time.Minute)); stopped != 1 {
		t.Fatalf("an idle server is stopped: %d", stopped)
	}
	if _, err := n.hover(ctx, navParams{Repo: proj, Path: filepath.Join(proj, "app.py"), Line: 1, Col: 5}); err != nil {
		t.Fatalf("started again on the next question: %v", err)
	}
}

// Go code on a computer without gopls: the code, the folders and Find in Files work; the outline, the jumps and the
// symbol search say what to install, and nothing fails.
func TestWithoutALanguageServer(t *testing.T) {
	root := t.TempDir()
	proj := realDir(t, writeRepo(t, root))
	n := &node{}
	n.setup(config{Roots: []string{root}, Gopls: "/nonexistent/gopls"})
	t.Cleanup(n.cleanup)
	app := &hubApp{machines: []string{"m1"}, call: direct(n)}
	ctx := context.Background()
	var st string
	for _, step := range []struct {
		action string
		values map[string]string
		want   []string
	}{
		{"repo", map[string]string{"repo": proj}, []string{"go install golang.org/x/tools/gopls@latest"}},
		{"open", map[string]string{"path": "main.go"}, []string{"util.Greet", "装上就能跳转", "go install golang.org/x/tools/gopls@latest"}},
		{"goto_line", map[string]string{"goto": "7:19"}, []string{`"key":"line"`, "装上这种语言的服务后"}},
		{"def_at", map[string]string{"line": "7", "col": "19"}, nil},
		{"refs_at", map[string]string{"line": "7", "col": "19"}, []string{"这台电脑上没有 Go 的语言服务"}},
		{"close_list", nil, nil},
		{"hover_at", map[string]string{"line": "7", "col": "19"}, []string{"这台电脑上没有 Go 的语言服务"}},
		{"search", map[string]string{"q": "Greet"}, []string{"没有 Go 的语言服务，下面是全文搜索", "main.go:7", "util/util.go:4"}},
	} {
		res, err := app.action(ctx, uiReq(st, step.action, step.values, 12))
		if err != nil {
			t.Fatalf("%s: %v", step.action, err)
		}
		if res.View == nil {
			if step.action == "def_at" && res.Toast != "" {
				continue // a toast says the server is missing; the page stays
			}
			t.Fatalf("%s: no view", step.action)
		}
		if err := wire.CheckView(res.View); err != nil {
			t.Fatalf("%s: CheckView: %v", step.action, err)
		}
		raw, _ := json.Marshal(res.View)
		for _, w := range step.want {
			if !strings.Contains(string(raw), w) {
				t.Fatalf("%s: want %q in %s", step.action, w, raw)
			}
		}
		st = res.View.State
	}
}

// Find in Files reads only what the page could open: no link out of the repository (to a file or a folder), no file
// that holds secrets, no binary or oversized file, nothing under dependency or build folders; smart case.
func TestFindInFilesStaysInTheSandbox(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	proj := filepath.Join(root, "app")
	outside := t.TempDir()
	writeFiles(t, outside, map[string]string{"secret.txt": "needle outside\n", "dir/also.txt": "needle outside dir\n"})
	writeFiles(t, proj, map[string]string{
		"main.go":                 "package main\n// Needle in code\nvar needle = 1\n",
		".env":                    "TOKEN=needle\n",
		".env.example":            "TOKEN=needle-placeholder\n",
		"certs/server.pem":        "needle\n",
		"node_modules/x/index.js": "needle\n",
		"dist/bundle.js":          "needle\n",
		"big.txt":                 strings.Repeat("x", maxGrepFile) + "needle\n",
		"long.txt":                strings.Repeat("y", 5000) + "needle" + strings.Repeat("z", 5000) + "\n",
	})
	if err := os.WriteFile(filepath.Join(proj, "blob.bin"), []byte("needle\x00\x01"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(proj, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "dir"), filepath.Join(proj, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", proj, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	n := &node{}
	n.setup(config{Roots: []string{root}, Gopls: "/nonexistent/gopls"})
	r, err := n.repoAt(realDir(t, proj))
	if err != nil {
		t.Fatal(err)
	}
	res, err := n.grep(context.Background(), r, "needle")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range res.Hits {
		got = append(got, fmt.Sprintf("%s:%d", h.Rel, h.Line))
		if strings.Contains(h.Preview, "outside") {
			t.Errorf("read through a link out of the repository: %+v", h)
		}
		if len([]rune(h.Preview)) > previewChars+2 {
			t.Errorf("a long line's preview is cut: %d characters", len([]rune(h.Preview)))
		}
	}
	if want := ".env.example:1 long.txt:1 main.go:2 main.go:3"; strings.Join(got, " ") != want {
		t.Errorf("hits %q, want %q", strings.Join(got, " "), want)
	}
	res, _ = n.grep(context.Background(), r, "Needle")
	if len(res.Hits) != 1 || res.Hits[0].Line != 2 {
		t.Errorf("a capital letter matches case: %+v", res.Hits)
	}
	// the listing leaves the secrets out too, and reading through a linked folder is refused
	files, _ := n.list(context.Background(), r)
	for _, f := range files {
		if f == ".env" || strings.HasSuffix(f, ".pem") {
			t.Errorf("a secret file is listed: %s", f)
		}
	}
	if _, err := n.read(navParams{Path: filepath.Join(r.Path, "linkdir", "also.txt")}); err == nil {
		t.Error("read through a linked folder out of the repository")
	}
}

func TestSecretPathsMore(t *testing.T) {
	home := "/home/me"
	for p, secret := range map[string]bool{
		"/home/me/code/app/.env.local":           true,
		"/home/me/code/app/.env.sample":          false,
		"/home/me/code/app/.env.template":        false,
		"/home/me/code/app/keys/app.p12":         true,
		"/home/me/code/app/.git":                 true,
		"/home/me/code/app/.git/HEAD":            true,
		"/home/me/.aws/credentials":              true,
		"/home/me/.config/gh/hosts.yml":          true,
		"/home/me/.persona":                      true,
		"/home/me/.netrc":                        true,
		"/home/me/Library/Mobile Documents/a.go": true,
		"/home/me/code/app/gitignore.go":         false,
	} {
		if got := secretPath(p, home); got != secret {
			t.Errorf("secretPath(%s) = %v, want %v", p, got, secret)
		}
	}
}

func TestLanguagesAndServers(t *testing.T) {
	for path, want := range map[string]string{
		"a.go": "go", "a.ts": "typescript", "a.tsx": "typescript", "a.js": "javascript", "a.py": "python",
		"a.rs": "rust", "a.swift": "swift", "a.cpp": "cpp", "README.md": "",
	} {
		l, _ := languageOf(path)
		if l.name != want {
			t.Errorf("languageOf(%s) = %q, want %q", path, l.name, want)
		}
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"Cargo.toml": "", "package.json": "{}", "Package.swift": ""})
	var names []string
	for _, l := range workspaceLanguages(dir) {
		names = append(names, l.name)
	}
	if got := strings.Join(names, " "); got != "javascript rust swift" {
		t.Errorf("workspace languages %q", got)
	}
	// a server path from the settings: found when it is a program, its arguments by its name
	bin := filepath.Join(t.TempDir(), "pylsp")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	n := &node{}
	n.setup(config{Python: bin, Rust: "/nonexistent/rust-analyzer"})
	py, _ := languageNamed("python")
	rust, _ := languageNamed("rust")
	n.mu.Lock()
	cmd, ok := n.serverLocked(py)
	_, rustOK := n.serverLocked(rust)
	n.mu.Unlock()
	if !ok || cmd.bin != bin || len(cmd.args) != 0 {
		t.Errorf("pylsp from the settings: %+v %v", cmd, ok)
	}
	if rustOK {
		t.Error("a missing server path from the settings is not found")
	}
	if got := (config{IdleMinutes: 0}).idle(); got != idleStop {
		t.Errorf("default idle %v", got)
	}
	if got := (config{IdleMinutes: 99999}).idle(); got != 24*time.Hour {
		t.Errorf("idle is at most a day: %v", got)
	}
}

func TestHoverText(t *testing.T) {
	for raw, want := range map[string]string{
		`{"kind":"markdown","value":"**x**"}`:           "**x**|true",
		`{"kind":"plaintext","value":"x int"}`:          "x int|false",
		`"just text"`:                                   "just text|true",
		`{"language":"go","value":"func F()"}`:          "```go\nfunc F()\n```|true",
		`["a",{"language":"rust","value":"fn f()"},""]`: "a\n\n```rust\nfn f()\n```|true",
		`null`: "|false",
		`42`:   "|false",
	} {
		text, md := hoverText(json.RawMessage(raw))
		if got := fmt.Sprintf("%s|%v", text, md); got != want {
			t.Errorf("hoverText(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestReadEdges(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "r")
	long := strings.Repeat("あ", windowChars) // one line longer than a window, in multi-byte characters
	writeFiles(t, proj, map[string]string{"go.mod": "module r\n", "long.txt": long + "\nnext\n", "crlf.txt": "a\r\nb\r\n"})
	n := &node{}
	n.setup(config{Roots: []string{root}, Gopls: "/nonexistent/gopls"})
	rd, err := n.read(navParams{Path: filepath.Join(realDir(t, proj), "long.txt")})
	if err != nil {
		t.Fatal(err)
	}
	if len(rd.Text) > windowChars || !strings.HasPrefix(long, rd.Text) || !rd.More {
		t.Errorf("a long line is cut on a character: %d bytes, more %v", len(rd.Text), rd.More)
	}
	rd, _ = n.read(navParams{Path: filepath.Join(realDir(t, proj), "crlf.txt"), From: 99})
	if rd.Text != "b" || rd.From != 2 || rd.Lines != 2 {
		t.Errorf("from past the end clamps, CRLF read as LF: %+v", rd)
	}
	if _, err := n.read(navParams{Path: "relative/path.go"}); err == nil {
		t.Error("a relative path is refused")
	}
	if _, err := n.read(navParams{Path: proj}); err == nil {
		t.Error("a folder is not read")
	}
}

// Review regressions: lowering that changes a line's length in bytes (İ, the Kelvin sign, Latin-1 bytes) must not
// break Find in Files; JSX goes to the TypeScript server as its own language; the home folder is never a repository;
// credential files inside a repository are secrets too.
func TestReviewRegressions(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "r")
	writeFiles(t, proj, map[string]string{
		"go.mod":     "module r\n",
		"tr.txt":     "İİ x\nfoo here\nKK\nbar foo\n",
		"latin1.txt": "caf\xe9 \xe9\xe9 foo\n",
	})
	n := &node{}
	n.setup(config{Roots: []string{root}, Gopls: "/nonexistent/gopls"})
	r, err := n.repoAt(realDir(t, proj))
	if err != nil {
		t.Fatal(err)
	}
	res, err := n.grep(context.Background(), r, "foo")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range res.Hits {
		got = append(got, fmt.Sprintf("%s:%d:%d", h.Rel, h.Line, h.Col))
	}
	if want := "latin1.txt:1:9 tr.txt:2:1 tr.txt:4:5"; strings.Join(got, " ") != want {
		t.Errorf("hits %q, want %q", strings.Join(got, " "), want)
	}

	for path, id := range map[string]string{"a.tsx": "typescriptreact", "a.jsx": "javascriptreact", "a.ts": "typescript", "a.js": "javascript"} {
		if l, _ := languageOf(path); l.id != id {
			t.Errorf("languageOf(%s).id = %q, want %q", path, l.id, id)
		}
	}

	home := t.TempDir()
	writeFiles(t, home, map[string]string{"package.json": "{}", "code/app/go.mod": "module app\n"})
	repos := findRepos([]string{home, "~", filepath.Join(home, "code")}, home)
	if len(repos) != 1 || repos[0].Name != "app" {
		t.Errorf("the home folder is not a repository: %+v", repos)
	}

	for p, secret := range map[string]bool{
		"/w/app/.npmrc": true, "/w/app/sub/.pypirc": true, "/w/app/.netrc": true, "/w/app/.git-credentials": true,
		"/w/app/.envrc": true, "/w/app/.env-prod": true, "/w/app/.env_local": true, "/w/app/.env.example": false,
		"/w/app/npmrc.md": false,
	} {
		if got := secretPath(p, "/home/me"); got != secret {
			t.Errorf("secretPath(%s) = %v, want %v", p, got, secret)
		}
	}
	if initOptions("/x/rust-analyzer") == nil || initOptions("/x/sourcekit-lsp") == nil || initOptions("/x/gopls") != nil {
		t.Error("read-only settings for rust-analyzer and sourcekit-lsp only")
	}
	if clangdCmd.args[0] != "--background-index=false" {
		t.Error("clangd without its background index")
	}
}
