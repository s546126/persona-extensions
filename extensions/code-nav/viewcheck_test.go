package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// Every page the extension draws, in every state and at the edges, against wire.CheckView (what the hub holds a view
// to): the empty states, the repository list, the tree and the folders, the code, Find in Files, the symbol search,
// quick info, definitions, implementations and usages, the hints without a language server, long lines and names,
// keys given once, the segmented control's limits and the breadcrumb's.

// fakeNode answers nav.* with what a test puts in it.
type fakeNode struct {
	repos   reposResult
	list    listResult
	read    readResult
	syms    symbolsResult
	search  symbolsResult
	grep    grepResult
	locs    locationsResult
	hover   hoverResult
	fail    map[string]error // a method that fails
	methods []string
}

func (f *fakeNode) call(_ context.Context, _, method string, params, out any) error {
	f.methods = append(f.methods, method)
	if err := f.fail[method]; err != nil {
		return err
	}
	var res any
	switch method {
	case "repos":
		res = f.repos
	case "list":
		res = f.list
	case "read":
		r := f.read
		var p navParams
		raw, _ := json.Marshal(params)
		_ = json.Unmarshal(raw, &p)
		if r.Path == "" {
			r.Path = p.Path
		}
		res = r
	case "symbols":
		res = f.syms
	case "search":
		res = f.search
	case "grep":
		res = f.grep
	case "definition", "references", "implementation":
		res = f.locs
	case "hover":
		res = f.hover
	default:
		return fmt.Errorf("unknown method %s", method)
	}
	b, _ := json.Marshal(res)
	return json.Unmarshal(b, out)
}

const repoPath = "/w/app"

func goodNode() *fakeNode {
	return &fakeNode{
		repos: reposResult{Repos: []repo{{Name: "app", Path: repoPath}}, Roots: []string{"~/code"}, Lsp: []string{"go"}},
		list:  listResult{Files: []string{"README.md", "go.mod", "main.go", "pkg/a/a.go", "pkg/b.go", "web/app.ts"}},
		read:  readResult{Repo: repoPath, Rel: "main.go", Text: "package main\n\nfunc main() {\n\tfmt.Println(util.Greet(\"you\"))\n}", From: 1, Lines: 5},
		syms:  symbolsResult{Symbols: []symbol{{Name: "main", Kind: 12, Line: 3, Col: 6}}},
	}
}

func viewOf(t *testing.T, app *hubApp, s state, version int, locale string) *wire.View {
	t.Helper()
	r := uiReq(ext.EncodeState(s), "", nil, version)
	r.Locale = locale
	v, err := app.render(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	mustCheck(t, v)
	return v
}

func mustCheck(t *testing.T, v *wire.View) {
	t.Helper()
	if err := wire.CheckView(v); err != nil {
		raw, _ := json.Marshal(v)
		t.Fatalf("CheckView: %v\n%s", err, raw)
	}
}

func rawOf(v *wire.View) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func components(v *wire.View, typ string) []wire.Component {
	var out []wire.Component
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Type == typ {
				out = append(out, c)
			}
		}
	}
	return out
}

func TestViewEmptyStates(t *testing.T) {
	// no computer chosen
	v := viewOf(t, &hubApp{}, state{}, 12, "zh-Hans")
	if !strings.Contains(rawOf(v), "还没选电脑") {
		t.Fatalf("no computer: %s", rawOf(v))
	}
	// the computer cannot be reached
	f := goodNode()
	f.fail = map[string]error{"repos": &ext.Error{Code: "unavailable", Message: strings.Repeat("down ", 400)}}
	v = viewOf(t, &hubApp{machines: []string{"m1"}, call: f.call}, state{}, 12, "en")
	if !strings.Contains(rawOf(v), "Couldn't reach") {
		t.Fatalf("unreachable: %s", rawOf(v))
	}
	// no repository in the folders
	f = goodNode()
	f.repos = reposResult{Roots: []string{strings.Repeat("~/very/long/root/", 200)}}
	v = viewOf(t, &hubApp{machines: []string{"m1"}, call: f.call}, state{}, 12, "zh-Hans")
	if !strings.Contains(rawOf(v), "没找到仓库") {
		t.Fatalf("no repository: %s", rawOf(v))
	}
	// a nil view's parts: a node that answers nothing at all
	f = &fakeNode{}
	viewOf(t, &hubApp{machines: []string{"m1"}, call: f.call}, state{R: repoPath}, 12, "zh-Hans")
}

func TestViewRepositoryList(t *testing.T) {
	f := goodNode()
	long := "/w/" + strings.Repeat("x", 300)
	f.repos.Repos = nil
	for i := range maxDir + 20 {
		f.repos.Repos = append(f.repos.Repos, repo{Name: strings.Repeat("名", 300), Path: fmt.Sprintf("%s/%d", long, i)})
	}
	f.repos.Lsp = []string{"typescript", "javascript", "python"}
	// one computer given twice: no filter (a filter needs two choices)
	app := &hubApp{machines: []string{"m1", "m1"}, call: f.call}
	v := viewOf(t, app, state{}, 12, "zh-Hans")
	if v.Filter != nil {
		t.Fatalf("a filter of one computer: %+v", v.Filter)
	}
	if n := len(components(v, wire.ComponentRow)); n != maxDir {
		t.Fatalf("%d repository rows, want %d", n, maxDir)
	}
	if foot := v.Sections[0].Footer; !strings.Contains(foot, "TypeScript、Python") || strings.Contains(foot, "JavaScript") {
		t.Fatalf("the languages, TypeScript standing for JavaScript too: %q", foot)
	}
	// two computers: a filter with both
	app = &hubApp{machines: []string{"m1", "m2", "m1"}, call: f.call}
	v = viewOf(t, app, state{}, 12, "zh-Hans")
	if v.Filter == nil || len(v.Filter.Choices) != 2 || v.Filter.Value != "m1" {
		t.Fatalf("the computer filter: %+v", v.Filter)
	}
	// no language server at all
	f.repos.Lsp = nil
	v = viewOf(t, app, state{}, 12, "zh-Hans")
	if !strings.Contains(v.Sections[0].Footer, "还没装语言服务") {
		t.Fatalf("no server: %q", v.Sections[0].Footer)
	}
}

func TestViewRepositoryHome(t *testing.T) {
	f := goodNode()
	f.read = readResult{Repo: repoPath, Rel: "README.md", From: 1, Text: "# App\n\n![logo](docs/logo.png) [guide](docs/guide.md) [site](https://example.com) " +
		"[ref][1] <mailto:me@example.com> [mail](mailto:me@example.com) [cred](https://user@example.com)\n\n[1]: http://example.com\n" + strings.Repeat("长", 30000)}
	app := &hubApp{machines: []string{"m1"}, call: f.call}
	v := viewOf(t, app, state{M: "m1", R: repoPath}, 12, "zh-Hans")
	raw := rawOf(v)
	md := components(v, wire.ComponentMarkdown)
	if len(md) != 1 || strings.Contains(md[0].Text, "logo.png") || !strings.Contains(md[0].Text, "[site](https://example.com)") || utf8.RuneCountInString(md[0].Text) > 20000 {
		t.Fatalf("the README made safe: %s", raw)
	}
	if !strings.Contains(raw, `"title":"pkg","subtitle":"文件夹"`) || !strings.Contains(raw, `"title":"main.go"`) {
		t.Fatalf("the top folder's rows: %s", raw)
	}
	if !strings.Contains(raw, "TypeScript / JavaScript：npm install -g typescript-language-server") {
		t.Fatalf("the install hint for the TypeScript files: %s", raw)
	}
	// a README the markdown component cannot take at all is shown as code
	c := safeMarkdown("readme", "[x](javascript:alert(1)) ![a](b)", "README.md")
	if err := wire.CheckView(&wire.View{Sections: []wire.Section{{Items: []wire.Component{c}}}}); err != nil {
		t.Fatalf("safeMarkdown: %v", err)
	}
}

func TestViewFolders(t *testing.T) {
	f := goodNode()
	for i := range 400 {
		f.list.Files = append(f.list.Files, fmt.Sprintf("pkg/many/%03d.go", i))
	}
	app := &hubApp{machines: []string{"m1"}, call: f.call}
	v := viewOf(t, app, state{M: "m1", R: repoPath, V: "dir", D: "pkg/many"}, 12, "zh-Hans")
	if n := len(components(v, wire.ComponentRow)); n > maxDir {
		t.Fatalf("%d rows in a folder", n)
	}
	cr := components(v, wire.ComponentBreadcrumb)[0].Crumbs
	if len(cr) != 4 || cr[2].Action == nil || cr[3].Action != nil {
		t.Fatalf("仓库 / app / pkg / many, the last not tappable: %+v", cr)
	}
	v = viewOf(t, app, state{M: "m1", R: repoPath, V: "dir", D: "nothing/here"}, 12, "zh-Hans")
	if !strings.Contains(rawOf(v), "这个文件夹是空的") {
		t.Fatalf("an empty folder: %s", rawOf(v))
	}
}

func TestViewDeepBreadcrumb(t *testing.T) {
	f := goodNode()
	deep := strings.Repeat("d/", 40) + "file.go"
	f.list.Files = []string{deep}
	f.read.Rel = deep
	app := &hubApp{machines: []string{"m1"}, call: f.call}
	v := viewOf(t, app, state{M: "m1", R: repoPath, P: repoPath + "/" + deep}, 12, "zh-Hans")
	cr := components(v, wire.ComponentBreadcrumb)[0].Crumbs
	if len(cr) != maxCrumbs || cr[len(cr)-1].Title != "file.go" || cr[len(cr)-1].Action != nil || cr[3].Title != "…" || cr[3].Action != nil {
		t.Fatalf("a deep path keeps the breadcrumb's limit: %d %+v", len(cr), cr)
	}
	// a file outside the repository: its path, untappable
	v = viewOf(t, app, state{M: "m1", R: repoPath, P: "/usr/local/go/src/fmt/print.go"}, 12, "zh-Hans")
	cr = components(v, wire.ComponentBreadcrumb)[0].Crumbs
	if len(cr) != 3 || cr[2].Action != nil || cr[2].Title == "" {
		t.Fatalf("outside the repository: %+v", cr)
	}
}

func TestViewCodeAtTheEdges(t *testing.T) {
	f := goodNode()
	longLine := strings.Repeat("x", 150000)
	f.read = readResult{Repo: repoPath, Rel: strings.Repeat("p/", 3000) + "main.go", From: 3, Lines: 9000, More: true, Truncated: true,
		Text: "func main() {\n\t" + longLine + "\n}"}
	for i := range 600 {
		f.syms.Symbols = append(f.syms.Symbols, symbol{Name: strings.Repeat("名", 500) + fmt.Sprint(i), Kind: 99, Detail: strings.Repeat("d", 500), Line: i + 1, Depth: i % 9})
	}
	app := &hubApp{machines: []string{"m1"}, call: f.call}
	for _, version := range []int{4, 5, 6, 12, 13} {
		v := viewOf(t, app, state{M: "m1", R: repoPath, P: repoPath + "/main.go", L: 3, C: 6}, version, "en")
		code := components(v, wire.ComponentCode)
		if len(code) != 1 || code[0].Highlight == nil || code[0].Load == nil || code[0].StartLine != 3 {
			t.Fatalf("UI %d: the code: %+v", version, code)
		}
		var outline *wire.Section
		for i := range v.Sections {
			if v.Sections[i].Key == "outline" {
				outline = &v.Sections[i]
			}
		}
		if outline == nil || len(outline.Items) != maxOutline {
			t.Fatalf("UI %d: the outline is cut at %d", version, maxOutline)
		}
		if has := len(outline.Items[0].Actions) > 0; has != (version >= 6) {
			t.Fatalf("UI %d: row actions %v", version, has)
		}
		for _, a := range outline.Items[0].Actions {
			if utf8.RuneCountInString(a.Title) > 20 {
				t.Fatalf("a row action's title over 20: %q", a.Title)
			}
		}
		raw := rawOf(v)
		if !strings.Contains(raw, `"key":"line"`) || !strings.Contains(raw, `"title":"main"`) || !strings.Contains(raw, `"goto_line"`) {
			t.Fatalf("UI %d: the names of the marked line and go to line: %s", version, raw[:min(len(raw), 3000)])
		}
	}
	// binary, unreadable
	f.read = readResult{Repo: repoPath, Rel: "a.png", Binary: true}
	v := viewOf(t, app, state{M: "m1", R: repoPath, P: repoPath + "/a.png"}, 12, "zh-Hans")
	if !strings.Contains(rawOf(v), "二进制") {
		t.Fatal("a binary file")
	}
	f.fail = map[string]error{"read": errors.New("permission denied"), "list": errors.New("git failed")}
	v = viewOf(t, app, state{M: "m1", R: repoPath, P: repoPath + "/main.go"}, 12, "zh-Hans")
	if raw := rawOf(v); !strings.Contains(raw, `"key":"read-error"`) || !strings.Contains(raw, `"key":"tree-error"`) {
		t.Fatalf("two errors on one page, two keys: %s", raw)
	}
}

func TestViewWithoutAServer(t *testing.T) {
	f := goodNode()
	f.syms = symbolsResult{NoServer: "rust"}
	f.search = symbolsResult{NoServer: "rust"}
	f.locs = locationsResult{NoServer: "rust"}
	f.hover = hoverResult{NoServer: "rust"}
	f.grep = grepResult{Hits: []grepHit{{Path: repoPath + "/main.rs", Rel: "main.rs", Line: 2, Col: 1, Preview: "fn main"}}, Files: 1, Scanned: 3}
	f.read.Rel = "main.rs"
	app := &hubApp{machines: []string{"m1"}, call: f.call}
	at := &spot{P: repoPath + "/main.rs", L: 2, C: 4}
	for _, s := range []state{
		{M: "m1", R: repoPath, P: at.P, L: 2, C: 4},
		{M: "m1", R: repoPath, P: at.P, L: 2, X: at},
		{M: "m1", R: repoPath, P: at.P, V: "refs", W: at},
		{M: "m1", R: repoPath, P: at.P, V: "search", Q: "main"},
	} {
		v := viewOf(t, app, s, 12, "zh-Hans")
		raw := rawOf(v)
		if !strings.Contains(raw, "Rust") {
			t.Fatalf("names the missing Rust server: %s", raw)
		}
		if s.V == "search" && !strings.Contains(raw, `"g:0"`) {
			t.Fatalf("the symbol search falls back to the text: %s", raw)
		}
	}
	// a file of no known language: no outline asked
	f.methods = nil
	f.read.Rel = "notes.txt"
	viewOf(t, app, state{M: "m1", R: repoPath, P: repoPath + "/notes.txt"}, 12, "zh-Hans")
	for _, m := range f.methods {
		if m == "symbols" {
			t.Fatal("asked for the outline of a text file")
		}
	}
}

func TestViewSearches(t *testing.T) {
	f := goodNode()
	for i := range 250 {
		f.search.Symbols = append(f.search.Symbols, symbol{Name: strings.Repeat("S", 3000), Kind: 5, Container: strings.Repeat("c", 3000), Rel: "a.go", Path: repoPath + "/a.go", Line: i + 1})
		f.grep.Hits = append(f.grep.Hits, grepHit{Path: repoPath + "/a.go", Rel: strings.Repeat("r/", 2000), Line: i + 1, Preview: strings.Repeat("p", 3000)})
	}
	f.grep.Files, f.grep.Truncated, f.grep.Partial, f.grep.Scanned = 9, true, true, 5000
	app := &hubApp{machines: []string{"m1"}, call: f.call}
	q := strings.Repeat("查", maxQuery)
	for _, k := range []string{"", "text"} {
		v := viewOf(t, app, state{M: "m1", R: repoPath, P: repoPath + "/main.go", V: "search", Q: q, K: k}, 12, "zh-Hans")
		seg := components(v, wire.ComponentSegmented)
		if len(seg) != 1 || len(seg[0].Choices) > 5 {
			t.Fatalf("one segmented of at most 5: %+v", seg)
		}
		for _, c := range seg[0].Choices {
			if utf8.RuneCountInString(c.Title) > 12 {
				t.Fatalf("a segmented title over 12: %q", c.Title)
			}
		}
		if n := len(components(v, wire.ComponentRow)); n != maxHits {
			t.Fatalf("%q: %d rows, want %d", k, n, maxHits)
		}
	}
	for _, locale := range []string{"en", "zh-Hans"} {
		seg := searchKind(texts(locale), &state{})
		if err := wire.CheckView(&wire.View{Sections: []wire.Section{{Items: []wire.Component{seg}}}}); err != nil {
			t.Fatalf("%s: %v", locale, err)
		}
	}
	// nothing found, nothing typed, a failure
	f.search, f.grep = symbolsResult{}, grepResult{Partial: true, Scanned: 10}
	for _, s := range []state{
		{M: "m1", R: repoPath, V: "search", Q: "nope"},
		{M: "m1", R: repoPath, V: "search", Q: "nope", K: "text"},
		{M: "m1", R: repoPath, V: "search"},
	} {
		viewOf(t, app, s, 12, "en")
	}
	f.fail = map[string]error{"search": errors.New("boom"), "grep": errors.New("boom")}
	viewOf(t, app, state{M: "m1", R: repoPath, V: "search", Q: "x"}, 12, "en")
	viewOf(t, app, state{M: "m1", R: repoPath, V: "search", Q: "x", K: "text"}, 12, "en")
}

func TestViewPlacesAndQuickInfo(t *testing.T) {
	f := goodNode()
	for i := range 300 {
		f.locs.Locations = append(f.locs.Locations, location{Path: repoPath + "/a.go", Rel: strings.Repeat("a/", 1000), Line: i, Preview: strings.Repeat("x", 3000)})
	}
	f.locs.Word = strings.Repeat("W", 3000)
	app := &hubApp{machines: []string{"m1"}, call: f.call}
	at := &spot{P: repoPath + "/main.go", L: 4, C: 7}
	for _, view := range []string{"refs", "defs", "impls"} {
		v := viewOf(t, app, state{M: "m1", R: repoPath, P: at.P, V: view, W: at}, 12, "zh-Hans")
		if n := len(components(v, wire.ComponentRow)); n != maxHits {
			t.Fatalf("%s: %d rows", view, n)
		}
	}
	f.locs = locationsResult{}
	viewOf(t, app, state{M: "m1", R: repoPath, P: at.P, V: "impls", W: at}, 12, "zh-Hans")
	viewOf(t, app, state{M: "m1", R: repoPath, P: at.P, V: "refs"}, 12, "zh-Hans") // no spot: back to the code
	f.fail = map[string]error{"references": errors.New("server crashed")}
	viewOf(t, app, state{M: "m1", R: repoPath, P: at.P, V: "refs", W: at}, 12, "zh-Hans")

	for _, h := range []hoverResult{
		{Word: "Greet", Markdown: true, Text: "```go\nfunc Greet(name string) string\n```\n\nGreet says hello. [pkg.go.dev](https://pkg.go.dev/x) ![i](x.png)"},
		{Word: "x", Text: strings.Repeat("plain ", 5000)},
		{Word: "x", Markdown: true, Text: strings.Repeat("长", 40000)},
		{},
	} {
		f.hover, f.fail = h, nil
		v := viewOf(t, app, state{M: "m1", R: repoPath, P: at.P, L: 4, X: at}, 12, "zh-Hans")
		if !strings.Contains(rawOf(v), `"key":"hover"`) {
			t.Fatalf("quick info: %s", rawOf(v))
		}
	}
	f.fail = map[string]error{"hover": errors.New("timeout")}
	viewOf(t, app, state{M: "m1", R: repoPath, P: at.P, X: at}, 12, "en")
}

// Every action on every kind of state, with odd values, draws a page the hub takes; nothing panics.
func TestEveryActionDrawsAValidPage(t *testing.T) {
	f := goodNode()
	f.locs = locationsResult{Locations: []location{{Path: repoPath + "/pkg/b.go", Rel: "pkg/b.go", Line: 2, Col: 3}}}
	f.hover = hoverResult{Word: "Greet", Text: "func Greet()", Markdown: true}
	f.grep = grepResult{Hits: []grepHit{{Path: repoPath + "/main.go", Rel: "main.go", Line: 4}}, Files: 1}
	app := &hubApp{machines: []string{"m1", "m2"}, call: f.call}
	long := strings.Repeat("/x", 3000)
	states := []string{
		"", "not json", `{"m":"gone"}`, `{"m":"m1","r":"relative"}`, `{"m":"m1","r":"/elsewhere"}`,
		ext.EncodeState(state{M: "m1", R: repoPath}),
		ext.EncodeState(state{M: "m1", R: repoPath, P: repoPath + "/main.go", L: 4, C: 7, H: []spot{{P: repoPath + "/go.mod"}}, N: []spot{{P: repoPath + "/pkg/b.go", L: 2}}}),
		ext.EncodeState(state{M: "m1", R: repoPath, P: repoPath + "/main.go", V: "bogus", K: "bogus", D: "../../etc", O: []string{"../x", "", "pkg"}, L: -5}),
		ext.EncodeState(state{M: "m1", R: repoPath, P: "relative.go", W: &spot{P: "rel"}, X: &spot{P: ""}, V: "refs"}),
		ext.EncodeState(state{M: "m1", R: repoPath, P: repoPath + long, Q: strings.Repeat("q", 5000), V: "search"}),
	}
	actions := map[string][]map[string]string{
		"machine": {{"machine": "m2"}, {"machine": "nope"}},
		"repo":    {{"repo": repoPath}, {"repo": "/nope"}},
		"repos":   {nil}, "home": {nil}, "back": {nil}, "forward": {nil}, "more": {{"from": "x"}, {"from": "40"}}, "prev": {nil},
		"open":       {{"path": "main.go"}, {"path": "../../etc/passwd"}, {"path": ""}},
		"expand":     {{"path": "pkg"}, {"path": "../x"}},
		"expand_to":  {{"path": "pkg/a"}},
		"dir":        {{"path": "pkg"}, {"path": ""}, {"path": "../.."}, {"path": "/abs"}},
		"goto":       {{"line": "3", "col": "1"}, {"line": "-9"}},
		"goto_line":  {{"goto": "12:3"}, {"goto": "12，3"}, {"goto": "abc"}, {"goto": ""}, {"goto": "99999999999999999999"}},
		"jump":       {{"path": repoPath + "/pkg/b.go", "line": "2"}, {"path": "relative.go"}},
		"search":     {{"q": "Greet"}, {"q": ""}, {"q": strings.Repeat("长", 3000)}},
		"scope":      {{"scope": "text"}, {"scope": "sym"}, {"scope": "bogus"}},
		"close_list": {nil}, "close_hover": {nil},
		"refs_at":  {{"line": "4", "col": "7"}},
		"hover_at": {{"line": "4", "col": "7"}},
		"def_at":   {{"line": "4", "col": "7"}, {"line": "x"}},
		"impl_at":  {{"line": "4", "col": "7"}},
		"nav": {{"intent": "definition", "line": "4", "column": "7"}, {"intent": "references", "line": "4", "column": "7"},
			{"intent": "implementation", "line": "4", "column": "7"}, {"intent": "hover", "line": "4", "column": "7"},
			{"intent": "back"}, {"intent": "forward"}, {"intent": "bogus"}, nil},
	}
	for _, st := range states {
		for action, valuesList := range actions {
			for _, values := range valuesList {
				res, err := app.action(context.Background(), uiReq(st, action, values, 12))
				if err != nil {
					t.Fatalf("%s %v on %q: %v", action, values, st, err)
				}
				if res.View == nil {
					if res.Toast == "" {
						t.Fatalf("%s %v on %q: neither a view nor a toast", action, values, st)
					}
					continue
				}
				if err := wire.CheckView(res.View); err != nil {
					t.Fatalf("%s %v on %q: CheckView: %v", action, values, st, err)
				}
				if len(res.View.State) > stateBudget+1024 {
					t.Fatalf("%s: state of %d bytes", action, len(res.View.State))
				}
			}
		}
	}
	if _, err := app.action(context.Background(), uiReq("", "no_such_action", nil, 12)); err == nil {
		t.Fatal("an unknown action is refused")
	}
}

func TestStateStaysSmall(t *testing.T) {
	s := &state{}
	for i := range 200 {
		s.moveTo("/w/app/"+strings.Repeat("x", 900)+fmt.Sprint(i)+".go", i+1, 1)
		s.toggle(strings.Repeat("d", 1000) + fmt.Sprint(i))
	}
	for range 40 {
		s.back()
	}
	if n := len(ext.EncodeState(s)); n > stateBudget {
		t.Fatalf("state of %d bytes", n)
	}
	if len(s.H) > maxBack || len(s.N) > maxBack || len(s.O) > maxOpenDirs {
		t.Fatalf("%d back, %d forward, %d folders", len(s.H), len(s.N), len(s.O))
	}
}

func TestLineWordsAndGotoLine(t *testing.T) {
	ws := lineWords("\tfmt.Println(util.Greet(\"you\"), 42, x1)", 19)
	var got []string
	for _, w := range ws {
		got = append(got, fmt.Sprintf("%s@%d", w.word, w.col))
	}
	if want := "Greet@19 fmt@2 Println@6 util@14 you@26 x1@37"; strings.Join(got, " ") != want {
		t.Errorf("lineWords: %q, want %q", strings.Join(got, " "), want)
	}
	if ws := lineWords("func return if else for", 1); len(ws) != 0 {
		t.Errorf("keywords only: %v", ws)
	}
	many := strings.Repeat("a b c d e f g h i j k l m n o p ", 3)
	if ws := lineWords(many, 0); len(ws) != maxWords {
		t.Errorf("at most %d names: %d", maxWords, len(ws))
	}
	if ws := lineWords("名前 = 値", 1); len(ws) != 2 || ws[1].col != 6 {
		t.Errorf("columns in characters: %+v", ws)
	}
	for in, want := range map[string]string{"12": "12:0", " 12:3 ": "12:3", "12,3": "12:3", "12：3": "12:3", "12 3": "12:3", "0": "x", "a": "x", "": "x"} {
		l, c, ok := parseLine(in)
		got := fmt.Sprintf("%d:%d", l, c)
		if !ok {
			got = "x"
		}
		if got != want {
			t.Errorf("parseLine(%q) = %s, want %s", in, got, want)
		}
	}
}

// The UI version 13 navigate is drawn exactly when the SDK has it and the app is of version 13.
func TestNavigateFollowsTheSDK(t *testing.T) {
	var c wire.Component
	set := setNavigate(&c, &wire.Action{ID: "nav"})
	raw, _ := json.Marshal(c)
	if set != strings.Contains(string(raw), `"navigate"`) {
		t.Fatalf("setNavigate %v, json %s", set, raw)
	}
	f := goodNode()
	app := &hubApp{machines: []string{"m1"}, call: f.call}
	for _, version := range []int{12, 13} {
		v := viewOf(t, app, state{M: "m1", R: repoPath, P: repoPath + "/main.go"}, version, "zh-Hans")
		code := components(v, wire.ComponentCode)[0]
		b, _ := json.Marshal(code)
		want := version >= 13 && navigateField >= 0
		if strings.Contains(string(b), `"navigate"`) != want || strings.Contains(rawOf(v), "⌘B") != want {
			t.Fatalf("UI %d: navigate drawn %v, want %v", version, !want, want)
		}
	}
}
