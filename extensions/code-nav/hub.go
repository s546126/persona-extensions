package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The program on the hub: the 代码 page. It keeps where you are (computer, repository, file, line, the folders open in
// the tree, the way back and forward) in the page's state, and asks the computer's program (nodes.call → nav.*) for
// everything else, so a page drawn again shows the files as they are now.
//
// Every jump works on a UI version 12 app: the outline, the names of the marked line and the search results are rows
// whose tap and menu go to a definition or an implementation, list usages or show quick info. An app of UI version 13
// also follows a name chosen in the code itself (the code's navigate, action "nav").

const codeUI = "code"

// nodeCaller is how the page reaches a computer's program: the hub's nodes.call, or a fake in tests.
type nodeCaller func(ctx context.Context, machine, method string, params, out any) error

type hubApp struct {
	mu       sync.Mutex
	machines []string
	call     nodeCaller
}

func (a *hubApp) program(id, version string) ext.Program {
	return ext.Program{
		ID: id, Version: version, Manifest: manifest,
		Init: a.init,
		UI:   map[string]ext.Page{codeUI: {Render: a.render, Action: a.action}},
	}
}

func (a *hubApp) init(_ context.Context, in *ext.Init, hub *ext.Hub) (map[string]any, error) {
	var c config
	if err := in.Config(&c); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.machines = c.Machines
	if a.call == nil {
		a.call = func(ctx context.Context, machine, method string, params, out any) error {
			return hub.Call(ctx, "nodes.call", map[string]any{"machine": machine, "method": "nav." + method, "params": params}, out)
		}
	}
	return nil, nil
}

// state is where the page is. Short names: it travels with every draw and must stay under 16 KB.
type state struct {
	M string   `json:"m,omitempty"` // computer
	R string   `json:"r,omitempty"` // repository (its path on the computer)
	P string   `json:"p,omitempty"` // file open (absolute)
	L int      `json:"l,omitempty"` // line marked, 0 none
	C int      `json:"c,omitempty"` // its column
	F int      `json:"f,omitempty"` // first line shown, 0 from the marked line
	O []string `json:"o,omitempty"` // folders open in the tree, relative to the repository
	H []spot   `json:"h,omitempty"` // the way back, newest last
	N []spot   `json:"n,omitempty"` // the way forward, newest last
	V string   `json:"v,omitempty"` // what the main pane lists instead of the file: refs, defs, impls, search, dir
	Q string   `json:"q,omitempty"` // the search
	K string   `json:"k,omitempty"` // what the search looks for: "" symbols, "text" the files' text
	W *spot    `json:"w,omitempty"` // where refs / defs / impls were asked from
	X *spot    `json:"x,omitempty"` // where quick info was asked for
	D string   `json:"d,omitempty"` // the folder the main pane lists (V dir), relative to the repository
}

type spot struct {
	P string `json:"p"`
	L int    `json:"l,omitempty"`
	C int    `json:"c,omitempty"`
}

const (
	maxBack     = 30
	maxOpenDirs = 64
	maxTree     = 3000
	maxOutline  = 150
	maxHits     = 150 // rows of a search or a list of places
	maxDir      = 200 // rows of a folder
	maxWords    = 12  // names of the marked line
	maxMenus    = 80  // outline rows with a menu: a page has at most 100 rows with one (the line's names take the rest)
	headlineMax = 200 // characters of a headline
	maxCrumbs   = 20  // the breadcrumb's limit
	stateBudget = 12 << 10
)

// views the main pane can show instead of the file
var listViews = map[string]bool{"": true, "refs": true, "defs": true, "impls": true, "search": true, "dir": true}

func (a *hubApp) snapshot() ([]string, nodeCaller) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.machines...), a.call
}

func (a *hubApp) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	s := decodeState(r)
	return a.view(ctx, r, &s, ""), nil
}

// decodeState reads the page's state and drops what it cannot be: a state is the app's to send back, so it is held to
// the shapes this program writes.
func decodeState(r *ext.UIRequest) state {
	var s state
	if r.DecodeState(&s) != nil {
		return state{}
	}
	s.sanitize()
	return s
}

func (s *state) sanitize() {
	okPath := func(p string) bool { return p == "" || (filepath.IsAbs(p) && len(p) <= 4096) }
	if !okPath(s.R) || s.R == "" {
		s.R, s.P = "", ""
	}
	if !okPath(s.P) {
		s.P = ""
	}
	s.L, s.C, s.F = max(0, s.L), max(0, s.C), max(0, s.F)
	if !listViews[s.V] {
		s.V = ""
	}
	if s.K != "text" {
		s.K = ""
	}
	s.Q = clip(strings.TrimSpace(s.Q), maxQuery)
	if !relOK(s.D) {
		s.D = ""
	}
	s.O = slices.DeleteFunc(s.O, func(d string) bool { return d == "" || len(d) > 1024 || !relOK(d) })
	if len(s.O) > maxOpenDirs {
		s.O = s.O[len(s.O)-maxOpenDirs:]
	}
	spots := func(list []spot) []spot {
		list = slices.DeleteFunc(list, func(x spot) bool { return !okPath(x.P) || x.P == "" })
		if len(list) > maxBack {
			list = list[len(list)-maxBack:]
		}
		return list
	}
	s.H, s.N = spots(s.H), spots(s.N)
	for _, w := range []**spot{&s.W, &s.X} {
		if *w != nil && (!okPath((*w).P) || (*w).P == "") {
			*w = nil
		}
	}
	s.trim()
}

// relOK says whether rel is a path inside the repository as the tree writes it.
func relOK(rel string) bool {
	return rel == "" || (!strings.HasPrefix(rel, "/") && !strings.HasSuffix(rel, "/") && !strings.Contains(rel, "//") &&
		!strings.Contains("/"+rel+"/", "/../") && !strings.Contains("/"+rel+"/", "/./") && !strings.Contains(rel, "\\"))
}

// view draws the page for s; note, when set, is a line above the code (a definition that was not found).
func (a *hubApp) view(ctx context.Context, r *ext.UIRequest, s *state, note string) *wire.View {
	t := texts(r.Locale)
	all, call := a.snapshot()
	var machines []string
	for _, c := range machineChoices(all) {
		machines = append(machines, c.Value)
	}
	v := &wire.View{Title: t.title}
	if len(machines) == 0 {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Key: "setup", Title: t.noMachines, Detail: t.noMachinesHint, Icon: wire.NavIconCode}}}}
		return v
	}
	switch {
	case s.M == "": // the first draw: the first computer, keeping what the state already says
		s.M = machines[0]
	case !slices.Contains(machines, s.M): // a computer no longer chosen: start over on the first
		*s = state{M: machines[0]}
	}
	if choices := machineChoices(machines); len(choices) > 1 {
		v.Filter = &wire.Filter{Name: "machine", Title: t.machine, Value: s.M, Action: wire.Action{ID: "machine"}, Choices: choices}
	}
	nc := func(method string, params, out any) error {
		cctx, cancel := context.WithTimeout(ctx, 35*time.Second)
		defer cancel()
		return call(cctx, s.M, method, params, out)
	}
	var repos reposResult
	if err := nc("repos", map[string]any{}, &repos); err != nil {
		v.State = ext.EncodeState(s)
		v.Sections = []wire.Section{{Items: []wire.Component{errorEmpty("error", t.noComputer, err)}}}
		return v
	}
	if s.R != "" && !slices.ContainsFunc(repos.Repos, func(x repo) bool { return x.Path == s.R }) {
		*s = state{M: s.M}
	}
	if s.R == "" {
		v.State = ext.EncodeState(s)
		v.Sections = repoList(t, repos)
		return v
	}
	rp := repos.Repos[slices.IndexFunc(repos.Repos, func(x repo) bool { return x.Path == s.R })]
	v.Title = clip(rp.Name, 200)
	v.Layout = wire.LayoutSplit
	v.Search = &wire.Search{Placeholder: t.searchHint, Value: s.Q, Action: wire.Action{ID: "search"}}

	var list listResult
	listErr := nc("list", map[string]any{"repo": s.R}, &list)

	side := []wire.Section{}
	if listErr != nil {
		side = append(side, wire.Section{Key: "tree", Pane: wire.PaneSide, Items: []wire.Component{errorEmpty("tree-error", t.noFiles, listErr)}})
	} else {
		title := fmt.Sprintf(t.files, len(list.Files))
		if list.Truncated {
			title += t.filesCut
		}
		side = append(side, wire.Section{Key: "tree", Title: title, Pane: wire.PaneSide, Items: []wire.Component{tree(s, rp, list.Files)}})
	}

	top := wire.Section{Key: "nav"}
	top.Items = append(top.Items, crumbs(t, s, rp))
	if len(s.H) > 0 {
		top.Items = append(top.Items, wire.Component{Type: wire.ComponentButton, Key: "back", Title: t.back, Action: &wire.Action{ID: "back"}})
	}
	if len(s.N) > 0 {
		top.Items = append(top.Items, wire.Component{Type: wire.ComponentButton, Key: "forward", Title: t.forward, Action: &wire.Action{ID: "forward"}})
	}
	if note != "" {
		top.Items = append(top.Items, wire.Component{Type: wire.ComponentText, Key: "note", Style: wire.TextStyleSecondary, Text: clip(note, 2000)})
	}
	main := []wire.Section{top}
	body := wire.Section{Key: "main"}

	switch s.V {
	case "search":
		body.Items = a.searchResults(t, s, nc)
	case "refs", "defs", "impls":
		body.Items = a.locationList(t, s, nc)
	case "dir":
		body.Items = dirList(t, s, s.D, list.Files)
	default:
		if s.P == "" {
			body.Items = append(dirList(t, s, "", list.Files), langHints(t, repos, list.Files)...)
			body.Items = append(body.Items, readme(t, rp, list.Files, nc)...)
			break
		}
		f := a.file(t, r, s, nc)
		main = append(main, f.before...)
		body.Items = f.items
		if len(f.outline) > 0 {
			side = append(side, wire.Section{Key: "outline", Title: t.outline, Pane: wire.PaneSide, Items: f.outline})
		}
	}
	if len(body.Items) > 0 {
		main = append(main, body)
	}
	v.State = ext.EncodeState(s)
	v.Sections = append(side, main...)
	return v
}

// machineChoices are the computers as the filter lists them: each once.
func machineChoices(machines []string) []wire.Choice {
	var out []wire.Choice
	seen := map[string]bool{}
	for _, m := range machines {
		if m == "" || seen[m] || len(out) >= 100 {
			continue
		}
		seen[m] = true
		out = append(out, wire.Choice{Value: m})
	}
	return out
}

// repoList is the first page: the repositories found on the computer, and which language servers it has.
func repoList(t text, repos reposResult) []wire.Section {
	if len(repos.Repos) == 0 {
		hint := t.noReposHint
		if len(repos.Roots) > 0 {
			hint = clip(fmt.Sprintf(t.noReposIn, strings.Join(repos.Roots, t.listSep)), 2000)
		}
		return []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Key: "none", Title: t.noRepos, Detail: hint, Icon: wire.NavIconFolder}}}}
	}
	sec := wire.Section{Key: "repos", Title: fmt.Sprintf(t.repos, len(repos.Repos))}
	for i, r := range repos.Repos {
		if i >= maxDir {
			break
		}
		sec.Items = append(sec.Items, wire.Component{
			Type: wire.ComponentRow, Key: fmt.Sprintf("repo:%d", i), Title: clip(r.Name, 200), Subtitle: clip(r.Path, 300), Icon: wire.NavIconFolder,
			Action: &wire.Action{ID: "repo", Values: map[string]string{"repo": r.Path}},
		})
	}
	foot := t.noLsp
	if len(repos.Lsp) > 0 {
		var names []string
		for _, l := range repos.Lsp {
			if l != "javascript" || !slices.Contains(repos.Lsp, "typescript") {
				names = append(names, languageTitle(l))
			}
		}
		foot = fmt.Sprintf(t.lsp, strings.Join(names, t.listSep))
	}
	sec.Footer = clip(foot, 2000)
	return []wire.Section{sec}
}

// langHints say, on a repository's first page, which of its languages cannot be followed yet and what to install.
func langHints(t text, repos reposResult, files []string) []wire.Component {
	count := map[string]int{}
	for _, f := range files {
		if l, ok := languageOf(f); ok && !grepSkipped(f) {
			count[l.name]++
		}
	}
	var missing []string
	for name := range count {
		if !slices.Contains(repos.Lsp, name) {
			missing = append(missing, name)
		}
	}
	sort.Slice(missing, func(i, j int) bool {
		return count[missing[i]] > count[missing[j]] || count[missing[i]] == count[missing[j]] && missing[i] < missing[j]
	})
	var out []wire.Component
	seen := map[string]bool{}
	for _, name := range missing {
		hint := t.install[name]
		if len(out) >= 3 || seen[hint] {
			continue
		}
		seen[hint] = true
		out = append(out, wire.Component{Type: wire.ComponentText, Key: "lang:" + name, Style: wire.TextStyleCaption,
			Text: clip(fmt.Sprintf(t.langMissing, languageTitle(name), hint), 2000)})
	}
	return out
}

// tree is the repository's files as far as the open folders go: the files and folders in the repository's top and in
// every open folder, the folders still closed listed without what is in them.
func tree(s *state, rp repo, files []string) wire.Component {
	open := map[string]bool{"": true}
	for _, d := range s.O {
		open[d] = true
	}
	sel := ""
	if s.P != "" {
		if rel, ok := inside(rp.Path, s.P); ok {
			sel = rel
			for d := path.Dir(sel); d != "." && d != "/"; d = path.Dir(d) {
				open[d] = true
			}
		}
	}
	if s.V == "dir" && s.D != "" {
		for d := s.D; d != "." && d != "/" && d != ""; d = path.Dir(d) {
			open[d] = true
		}
	}
	c := wire.Component{Type: wire.ComponentFileTree, Key: "tree", Selected: clip(sel, 4096), Action: &wire.Action{ID: "open"}, Expand: &wire.Action{ID: "expand"}}
	dirs := map[string]bool{}
	for _, f := range files {
		if len(c.Entries) >= maxTree {
			break
		}
		parent := path.Dir(f)
		if parent == "." {
			parent = ""
		}
		if open[parent] {
			c.Entries = append(c.Entries, wire.TreeEntry{Path: clip(f, 4096)})
			continue
		}
		// the nearest folder of f that shows: the first one below an open folder
		d := ""
		for _, part := range strings.Split(f, "/") {
			next := part
			if d != "" {
				next = d + "/" + part
			}
			if !open[d] {
				break
			}
			d = next
		}
		if d != "" && !open[d] && !dirs[d] {
			dirs[d] = true
			c.Entries = append(c.Entries, wire.TreeEntry{Path: clip(d, 4096), Kind: wire.TreeEntryKindDir})
		}
	}
	return c
}

// inside is p relative to dir with / between names, when p is inside dir.
func inside(dir, p string) (string, bool) {
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// crumbs is the path of what the main pane shows: 仓库 / repository / folders / file, every folder tappable to list
// it. A path too deep for the breadcrumb keeps its first folder and its last ones, with … between.
func crumbs(t text, s *state, rp repo) wire.Component {
	c := wire.Component{Type: wire.ComponentBreadcrumb, Key: "crumbs", Crumbs: []wire.Crumb{
		{Title: t.repoList, Action: &wire.Action{ID: "repos"}},
		{Title: clip(rp.Name, 200), Action: &wire.Action{ID: "home"}},
	}}
	rel, isFile := "", false
	switch {
	case s.V == "dir":
		rel = s.D
	case s.P != "" && s.V == "":
		r, ok := inside(rp.Path, s.P)
		if !ok { // outside the repository (a standard library, a dependency): its own path, not tappable
			c.Crumbs = append(c.Crumbs, wire.Crumb{Title: clip(filepath.ToSlash(s.P), 300)})
			return c
		}
		rel, isFile = r, true
	}
	if rel == "" {
		c.Crumbs[1].Action = nil
		return c
	}
	parts := strings.Split(rel, "/")
	type part struct {
		title, dir string
	}
	var ps []part
	for i, p := range parts {
		x := part{title: clip(p, 200)}
		if i < len(parts)-1 || !isFile {
			x.dir = strings.Join(parts[:i+1], "/")
		}
		ps = append(ps, x)
	}
	if room := maxCrumbs - len(c.Crumbs); len(ps) > room {
		keep := room - 2 // the first folder, …, then the last ones
		ps = append([]part{ps[0], {title: "…"}}, ps[len(ps)-keep:]...)
	}
	for i, p := range ps {
		cr := wire.Crumb{Title: p.title}
		if p.dir != "" && i < len(ps)-1 {
			cr.Action = &wire.Action{ID: "dir", Values: map[string]string{"path": p.dir}}
		}
		c.Crumbs = append(c.Crumbs, cr)
	}
	return c
}

// dirList is a folder's folders and files as rows, folders first: the way through a repository on a phone, where the
// tree is a tap away.
func dirList(t text, s *state, dir string, files []string) []wire.Component {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	var folders, plain []string
	seen := map[string]bool{}
	for _, f := range files {
		rest, ok := strings.CutPrefix(f, prefix)
		if !ok || rest == "" {
			continue
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			if name := rest[:i]; !seen[name] {
				seen[name] = true
				folders = append(folders, name)
			}
			continue
		}
		plain = append(plain, rest)
	}
	var out []wire.Component
	if dir != "" {
		out = append(out, wire.Component{Type: wire.ComponentText, Key: "dir-title", Style: wire.TextStyleHeadline,
			Text: clip(fmt.Sprintf(t.dirTitle, dir, len(folders)+len(plain)), headlineMax)})
		if s.P != "" {
			out = append(out, wire.Component{Type: wire.ComponentButton, Key: "dir-close", Title: t.backToCode, Action: &wire.Action{ID: "close_list"}})
		}
		if len(folders)+len(plain) == 0 {
			return append(out, wire.Component{Type: wire.ComponentEmpty, Key: "dir-empty", Title: t.dirEmpty, Icon: wire.NavIconFolder})
		}
	}
	for i, name := range folders {
		if len(out) >= maxDir {
			break
		}
		out = append(out, wire.Component{Type: wire.ComponentRow, Key: fmt.Sprintf("d:%d", i), Title: clip(name, 300), Subtitle: t.folderWord,
			Icon: wire.NavIconFolder, Action: &wire.Action{ID: "dir", Values: map[string]string{"path": prefix + name}}})
	}
	for i, name := range plain {
		if len(out) >= maxDir {
			break
		}
		out = append(out, wire.Component{Type: wire.ComponentRow, Key: fmt.Sprintf("f:%d", i), Title: clip(name, 300),
			Icon: wire.NavIconFile, Action: &wire.Action{ID: "open", Values: map[string]string{"path": prefix + name}}})
	}
	return out
}

// readme shows the repository's README below its top folder when no file is open.
func readme(t text, rp repo, files []string, nc func(string, any, any) error) []wire.Component {
	for _, f := range files {
		if path.Dir(f) == "." && strings.EqualFold(strings.TrimSuffix(f, path.Ext(f)), "readme") && strings.EqualFold(path.Ext(f), ".md") {
			var rd readResult
			if err := nc("read", map[string]any{"path": filepath.Join(rp.Path, f)}, &rd); err == nil && !rd.Binary {
				return []wire.Component{safeMarkdown("readme", rd.Text, f)}
			}
		}
	}
	return []wire.Component{{Type: wire.ComponentEmpty, Key: "pick", Title: t.pickFile, Detail: t.pickFileHint, Icon: wire.NavIconFile}}
}

var (
	mdImageRE = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	mdLinkRE  = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]*)[^)]*\)`)
	mdRefRE   = regexp.MustCompile(`(?m)^\s{0,3}\[[^\]]+\]:\s*\S+.*$`)
	mdAutoRE  = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9+.-]*:[^>\s]*)>`)
)

// safeMarkdown is text as a markdown component the app accepts: images become their words, links that are not https
// (a README's links to its own files) and reference links become their words. When that still does not pass, the text
// is shown as code.
func safeMarkdown(key, text, name string) wire.Component {
	md := cutRunes(text, 19000)
	md = mdImageRE.ReplaceAllString(md, "$1")
	md = mdLinkRE.ReplaceAllStringFunc(md, func(m string) string {
		sub := mdLinkRE.FindStringSubmatch(m)
		if strings.HasPrefix(sub[2], "https://") && !strings.Contains(sub[2], "@") {
			return m
		}
		return sub[1]
	})
	md = mdRefRE.ReplaceAllString(md, "")
	md = mdAutoRE.ReplaceAllStringFunc(md, func(m string) string {
		if strings.HasPrefix(m, "<https://") {
			return m
		}
		return strings.Trim(m, "<>")
	})
	c := wire.Component{Type: wire.ComponentMarkdown, Key: key, Text: md}
	if wire.CheckView(&wire.View{Sections: []wire.Section{{Items: []wire.Component{c}}}}) == nil {
		return c
	}
	return wire.Component{Type: wire.ComponentCode, Key: key, Path: clip(name, 4096), Language: "markdown", Text: cutRunes(text, 20000)}
}

// filePane is what the open file draws: sections above the code (quick info, the names of the marked line), the code
// and what goes with it, and the outline for the side pane.
type filePane struct {
	before  []wire.Section
	items   []wire.Component
	outline []wire.Component
}

// file is the open file: its code around the marked line, the marked line's names, quick info, and the outline.
func (a *hubApp) file(t text, r *ext.UIRequest, s *state, nc func(string, any, any) error) filePane {
	var out filePane
	from := s.F
	if from <= 0 {
		from = max(1, s.L-windowBefore)
	}
	var rd readResult
	if err := nc("read", map[string]any{"path": s.P, "from": from}, &rd); err != nil {
		out.items = []wire.Component{errorEmpty("read-error", t.noRead, err)}
		return out
	}
	if rd.Binary {
		out.items = []wire.Component{{Type: wire.ComponentEmpty, Key: "binary", Title: t.binary, Icon: wire.NavIconFile}}
		return out
	}
	lang, known := languageOf(s.P)
	c := wire.Component{Type: wire.ComponentCode, Key: "code", Path: clip(rd.Rel, 4096), Text: rd.Text, StartLine: max(1, rd.From)}
	if known {
		c.Language = lang.id
	}
	shown := strings.Count(rd.Text, "\n") + 1
	if s.L >= rd.From && s.L < rd.From+shown {
		c.Highlight = &wire.LineRange{Start: s.L, End: s.L}
	}
	if rd.More {
		c.Truncated = true
		c.Load = &wire.Action{ID: "more", Values: map[string]string{"from": strconv.Itoa(rd.From + shown)}}
	}
	navigates := r.UIVersion >= 13 && setNavigate(&c, &wire.Action{ID: "nav"})

	var syms symbolsResult
	var symsErr error
	if known {
		symsErr = nc("symbols", map[string]any{"repo": s.R, "path": s.P}, &syms)
	}
	hasServer := known && symsErr == nil && syms.NoServer == ""

	// quick info, above everything else in the file's pane
	if s.X != nil && s.X.P == s.P {
		out.before = append(out.before, a.hoverSection(t, s, nc))
	}
	// the marked line's names: the way to follow a name on an app that cannot report a click in the code
	if s.L > 0 && s.L >= rd.From && s.L < rd.From+shown {
		lines := strings.Split(rd.Text, "\n")
		if sec, ok := lineSection(t, r, s, lines[s.L-rd.From], hasServer); ok {
			out.before = append(out.before, sec)
		}
	}

	if rd.From > 1 {
		out.items = append(out.items, wire.Component{Type: wire.ComponentButton, Key: "prev", Title: fmt.Sprintf(t.earlier, rd.From-1), Action: &wire.Action{ID: "prev"}})
	}
	if rd.Repo == "" {
		out.items = append(out.items, wire.Component{Type: wire.ComponentText, Key: "outside", Style: wire.TextStyleCaption, Text: t.outside})
	}
	if navigates && hasServer {
		out.items = append(out.items, wire.Component{Type: wire.ComponentText, Key: "nav-hint", Style: wire.TextStyleCaption, Text: t.navHint})
	}
	out.items = append(out.items,
		wire.Component{Type: wire.ComponentField, Key: "goto", Name: "goto", Label: t.gotoLine, Kind: "text", Placeholder: t.gotoLineHint},
		wire.Component{Type: wire.ComponentButton, Key: "goto-go", Title: t.gotoButton, Action: &wire.Action{ID: "goto_line", Submit: true}},
		c)
	if rd.Truncated {
		out.items = append(out.items, wire.Component{Type: wire.ComponentText, Key: "cut", Style: wire.TextStyleCaption, Text: t.fileCut})
	}

	switch {
	case !known:
	case symsErr != nil:
		out.outline = []wire.Component{{Type: wire.ComponentText, Key: "outline-err", Style: wire.TextStyleCaption, Text: clip(t.noOutline+errText(symsErr), 300)}}
	case syms.NoServer != "":
		msg := fmt.Sprintf(t.noServer, languageTitle(syms.NoServer))
		if hint := t.install[syms.NoServer]; hint != "" {
			msg = fmt.Sprintf(t.noServerHint, languageTitle(syms.NoServer), hint)
		}
		out.outline = []wire.Component{{Type: wire.ComponentText, Key: "outline-none", Style: wire.TextStyleCaption, Text: clip(msg, 2000)}}
	default:
		for i, sym := range syms.Symbols {
			if i >= maxOutline {
				break
			}
			at := map[string]string{"line": strconv.Itoa(sym.Line), "col": strconv.Itoa(sym.Col)}
			row := wire.Component{
				Type: wire.ComponentRow, Key: fmt.Sprintf("sym:%d", i), Title: clip(strings.Repeat("· ", min(sym.Depth, 4))+cmp.Or(strings.TrimSpace(sym.Name), kindName(t, sym.Kind)), 200),
				Subtitle: clip(kindName(t, sym.Kind)+detail(sym.Detail), 200), Value: strconv.Itoa(sym.Line),
				Action: &wire.Action{ID: "goto", Values: at},
			}
			if r.UIVersion >= 6 && i < maxMenus {
				row.Actions = []wire.RowAction{
					{Title: t.findRefs, Action: wire.Action{ID: "refs_at", Values: at}},
					{Title: t.gotoImpl, Action: wire.Action{ID: "impl_at", Values: at}},
					{Title: t.showHover, Action: wire.Action{ID: "hover_at", Values: at}},
				}
			}
			out.outline = append(out.outline, row)
		}
	}
	return out
}

// lineSection lists the names on the marked line: a tap goes to a name's definition, its menu to implementations,
// usages and quick info.
func lineSection(t text, r *ext.UIRequest, s *state, line string, hasServer bool) (wire.Section, bool) {
	words := lineWords(line, s.C)
	if len(words) == 0 {
		return wire.Section{}, false
	}
	sec := wire.Section{Key: "line", Title: fmt.Sprintf(t.lineNames, s.L)}
	if !hasServer {
		sec.Items = []wire.Component{{Type: wire.ComponentText, Key: "line-none", Style: wire.TextStyleCaption, Text: t.lineNoServer}}
		return sec, true
	}
	for i, w := range words {
		at := map[string]string{"line": strconv.Itoa(s.L), "col": strconv.Itoa(w.col)}
		row := wire.Component{Type: wire.ComponentRow, Key: fmt.Sprintf("w:%d", i), Title: clip(w.word, 200),
			Subtitle: fmt.Sprintf(t.lineAt, s.L, w.col), Action: &wire.Action{ID: "def_at", Values: at}}
		if r.UIVersion >= 6 {
			row.Actions = []wire.RowAction{
				{Title: t.gotoImpl, Action: wire.Action{ID: "impl_at", Values: at}},
				{Title: t.findRefs, Action: wire.Action{ID: "refs_at", Values: at}},
				{Title: t.showHover, Action: wire.Action{ID: "hover_at", Values: at}},
			}
		}
		sec.Items = append(sec.Items, row)
	}
	return sec, true
}

type lineWord struct {
	word string
	col  int // 1-based, characters
}

// keywords are the words of the usual languages that name nothing to follow.
var keywords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`break case chan const continue default defer else fallthrough for func go goto if import
		interface map package range return select struct switch type var nil true false iota any
		let function class extends implements new this super export from as async await yield typeof instanceof void
		delete in of try catch finally throw throws null undefined static public private protected readonly enum
		def lambda pass raise except with global nonlocal is not and or None True False self elif del assert while
		fn pub mut impl trait use mod crate where loop match ref move unsafe dyn Self
		guard init deinit extension protocol override final inout open some do repeat fileprivate internal operator
		int string bool byte rune error float32 float64 int64 int32 uint uint64 char double float long short unsigned
		signed auto sizeof typedef union extern register volatile include define`) {
		keywords[w] = true
	}
}

// lineWords are the names on line worth following, each once, the one at col first; numbers and keywords are left out.
func lineWords(line string, col int) []lineWord {
	rs := []rune(line)
	isWord := func(r rune) bool { return r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r) }
	var out []lineWord
	seen := map[string]bool{}
	first := -1
	for i := 0; i < len(rs); {
		if !isWord(rs[i]) {
			i++
			continue
		}
		j := i
		for j < len(rs) && isWord(rs[j]) {
			j++
		}
		w := string(rs[i:j])
		if !unicode.IsDigit(rs[i]) && !keywords[w] && !seen[w] {
			seen[w] = true
			if col >= i+1 && col <= j {
				first = len(out)
			}
			out = append(out, lineWord{word: w, col: i + 1})
		}
		i = j
	}
	if first > 0 {
		w := out[first]
		out = append([]lineWord{w}, slices.Delete(out, first, first+1)...)
	}
	if len(out) > maxWords {
		out = out[:maxWords]
	}
	return out
}

// hoverSection is quick info about the name at s.X: its type or signature and its documentation.
func (a *hubApp) hoverSection(t text, s *state, nc func(string, any, any) error) wire.Section {
	at := s.X
	vals := map[string]string{"line": strconv.Itoa(at.L), "col": strconv.Itoa(at.C)}
	sec := wire.Section{Key: "hover"}
	var h hoverResult
	err := nc("hover", map[string]any{"repo": s.R, "path": at.P, "line": at.L, "col": at.C}, &h)
	word := h.Word
	if word == "" {
		word = t.thisSymbol
	}
	sec.Title = clip(fmt.Sprintf(t.hoverOf, word), 200)
	switch {
	case err != nil:
		sec.Items = append(sec.Items, errorEmpty("hover-error", t.noHover, err))
	case h.NoServer != "":
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentText, Key: "hover-none", Style: wire.TextStyleCaption,
			Text: clip(fmt.Sprintf(t.noServer, languageTitle(h.NoServer)), 2000)})
	case strings.TrimSpace(h.Text) == "":
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentText, Key: "hover-empty", Style: wire.TextStyleCaption, Text: t.hoverNone})
	case h.Markdown:
		sec.Items = append(sec.Items, safeMarkdown("hover-text", h.Text, "hover.md"))
	default:
		sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentCode, Key: "hover-text", Path: "hover.txt", Language: "text", Text: cutRunes(h.Text, 20000)})
	}
	sec.Items = append(sec.Items,
		wire.Component{Type: wire.ComponentButton, Key: "hover-def", Title: t.gotoDef, Action: &wire.Action{ID: "def_at", Values: vals}},
		wire.Component{Type: wire.ComponentButton, Key: "hover-close", Title: t.close, Action: &wire.Action{ID: "close_hover"}})
	return sec
}

func detail(d string) string {
	if d == "" {
		return ""
	}
	return " · " + d
}

// searchKind is what the search segmented control offers: symbols through the language server, or the files' text.
func searchKind(t text, s *state) wire.Component {
	v := "sym"
	if s.K == "text" {
		v = "text"
	}
	return wire.Component{Type: wire.ComponentSegmented, Key: "scope", Name: "scope", Value: v, Action: &wire.Action{ID: "scope"},
		Choices: []wire.Choice{{Value: "sym", Title: t.scopeSymbols}, {Value: "text", Title: t.scopeText}}}
}

func (a *hubApp) searchResults(t text, s *state, nc func(string, any, any) error) []wire.Component {
	out := []wire.Component{searchKind(t, s)}
	if s.P != "" {
		out = append(out, wire.Component{Type: wire.ComponentButton, Key: "search-close", Title: t.backToCode, Action: &wire.Action{ID: "close_list"}})
	}
	if s.Q == "" {
		return append(out, wire.Component{Type: wire.ComponentEmpty, Key: "search-empty", Title: t.grepEmpty, Icon: wire.NavIconSearch})
	}
	if s.K == "text" {
		return append(out, a.grepResults(t, s, nc)...)
	}
	var res symbolsResult
	if err := nc("search", map[string]any{"repo": s.R, "query": s.Q}, &res); err != nil {
		return append(out, errorEmpty("search-error", t.noSearch, err))
	}
	if res.NoServer != "" {
		// no server for this repository's language: the text search answers instead
		fallback := t.symbolFallback
		if _, ok := languageNamed(res.NoServer); ok {
			fallback = fmt.Sprintf(t.symbolFallbackLang, languageTitle(res.NoServer))
		}
		out = append(out, wire.Component{Type: wire.ComponentText, Key: "search-fallback", Style: wire.TextStyleCaption, Text: clip(fallback, 2000)})
		return append(out, a.grepResults(t, s, nc)...)
	}
	if len(res.Symbols) == 0 {
		return append(out, wire.Component{Type: wire.ComponentEmpty, Key: "search-none", Title: clip(fmt.Sprintf(t.noMatch, s.Q), 2000), Icon: wire.NavIconSearch})
	}
	out = append(out, wire.Component{Type: wire.ComponentText, Key: "search-title", Style: wire.TextStyleHeadline, Text: clip(fmt.Sprintf(t.matches, len(res.Symbols), s.Q), headlineMax)})
	for i, sym := range res.Symbols {
		if i >= maxHits {
			break
		}
		where := fmt.Sprintf("%s:%d", sym.Rel, sym.Line)
		if sym.Container != "" {
			where = sym.Container + " · " + where
		}
		out = append(out, wire.Component{
			Type: wire.ComponentRow, Key: fmt.Sprintf("hit:%d", i), Title: clip(cmp.Or(strings.TrimSpace(sym.Name), kindName(t, sym.Kind)), 200), Subtitle: clip(kindName(t, sym.Kind)+" · "+where, 300),
			Action: &wire.Action{ID: "jump", Values: map[string]string{"path": sym.Path, "line": strconv.Itoa(sym.Line), "col": strconv.Itoa(sym.Col)}},
		})
	}
	return out
}

// grepResults is Find in Files: the places the search's text is in, a row each.
func (a *hubApp) grepResults(t text, s *state, nc func(string, any, any) error) []wire.Component {
	var res grepResult
	if err := nc("grep", map[string]any{"repo": s.R, "query": s.Q}, &res); err != nil {
		return []wire.Component{errorEmpty("grep-error", t.noGrep, err)}
	}
	if len(res.Hits) == 0 {
		out := []wire.Component{{Type: wire.ComponentEmpty, Key: "grep-none", Title: clip(fmt.Sprintf(t.grepNone, s.Q), 2000), Icon: wire.NavIconSearch}}
		if res.Partial {
			out = append(out, wire.Component{Type: wire.ComponentText, Key: "grep-partial", Style: wire.TextStyleCaption, Text: fmt.Sprintf(t.grepPartial, res.Scanned)})
		}
		return out
	}
	out := []wire.Component{{Type: wire.ComponentText, Key: "grep-title", Style: wire.TextStyleHeadline, Text: clip(fmt.Sprintf(t.grepTitle, s.Q, res.Files, len(res.Hits)), headlineMax)}}
	if res.Truncated || len(res.Hits) > maxHits {
		out = append(out, wire.Component{Type: wire.ComponentText, Key: "grep-cut", Style: wire.TextStyleCaption, Text: fmt.Sprintf(t.grepCut, min(len(res.Hits), maxHits))})
	}
	if res.Partial {
		out = append(out, wire.Component{Type: wire.ComponentText, Key: "grep-partial", Style: wire.TextStyleCaption, Text: fmt.Sprintf(t.grepPartial, res.Scanned)})
	}
	for i, h := range res.Hits {
		if i >= maxHits {
			break
		}
		out = append(out, wire.Component{
			Type: wire.ComponentRow, Key: fmt.Sprintf("g:%d", i), Title: clip(fmt.Sprintf("%s:%d", h.Rel, h.Line), 300), Subtitle: clip(h.Preview, 300),
			Action: &wire.Action{ID: "jump", Values: map[string]string{"path": h.Path, "line": strconv.Itoa(h.Line), "col": strconv.Itoa(h.Col)}},
		})
	}
	return out
}

func (a *hubApp) locationList(t text, s *state, nc func(string, any, any) error) []wire.Component {
	if s.W == nil {
		s.V = ""
		return nil
	}
	method := map[string]string{"refs": "references", "defs": "definition", "impls": "implementation"}[s.V]
	var res locationsResult
	if err := nc(method, map[string]any{"repo": s.R, "path": s.W.P, "line": s.W.L, "col": s.W.C}, &res); err != nil {
		return []wire.Component{errorEmpty("refs-error", t.noRefs, err)}
	}
	if res.NoServer != "" {
		return []wire.Component{{Type: wire.ComponentEmpty, Key: "refs-none", Title: fmt.Sprintf(t.noServer, languageTitle(res.NoServer)), Detail: t.install[res.NoServer], Icon: wire.NavIconSearch}}
	}
	word := res.Word
	if word == "" {
		word = t.thisSymbol
	}
	title := map[string]string{"refs": t.refsOf, "defs": t.defsOf, "impls": t.implsOf}[s.V]
	out := []wire.Component{
		{Type: wire.ComponentText, Key: "refs-title", Style: wire.TextStyleHeadline, Text: clip(fmt.Sprintf(title, word, len(res.Locations)), headlineMax)},
		{Type: wire.ComponentButton, Key: "refs-close", Title: t.backToCode, Action: &wire.Action{ID: "close_list"}},
	}
	if len(res.Locations) == 0 {
		out = append(out, wire.Component{Type: wire.ComponentEmpty, Key: "refs-empty", Title: t.noneFound, Icon: wire.NavIconSearch})
	}
	for i, l := range res.Locations {
		if i >= maxHits {
			break
		}
		out = append(out, wire.Component{
			Type: wire.ComponentRow, Key: fmt.Sprintf("loc:%d", i), Title: clip(fmt.Sprintf("%s:%d", l.Rel, l.Line), 300), Subtitle: clip(l.Preview, 300),
			Action: &wire.Action{ID: "jump", Values: map[string]string{"path": l.Path, "line": strconv.Itoa(l.Line), "col": strconv.Itoa(l.Col)}},
		})
	}
	return out
}

// action answers a tap: it moves the state and draws the page again.
func (a *hubApp) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	t := texts(r.Locale)
	s := decodeState(r)
	vals := r.Values
	num := func(k string) int {
		n, _ := strconv.Atoi(strings.TrimSpace(vals[k]))
		return max(0, n)
	}
	here := func() spot { return spot{P: s.P, L: num("line"), C: max(num("col"), num("column"))} }
	note := ""
	switch r.Action {
	case "machine":
		s = state{M: vals["machine"]}
	case "repo":
		s = state{M: s.M, R: vals["repo"]}
	case "repos":
		s = state{M: s.M}
	case "home":
		s.remember()
		s.P, s.L, s.C, s.F, s.V, s.W, s.X, s.D = "", 0, 0, 0, "", nil, nil, ""
	case "open":
		if p := vals["path"]; p != "" && s.R != "" && relOK(p) {
			s.moveTo(filepath.Join(s.R, filepath.FromSlash(p)), 0, 0)
		}
	case "expand":
		s.toggle(vals["path"])
	case "expand_to":
		if p := vals["path"]; p != "" && !slices.Contains(s.O, p) {
			s.toggle(p)
		}
	case "dir":
		if p := strings.Trim(vals["path"], "/"); relOK(p) {
			s.V, s.D, s.W = "dir", p, nil
			if p == "" {
				s.V = ""
				if s.P != "" {
					s.remember()
					s.P, s.L, s.C, s.F, s.X = "", 0, 0, 0, nil
				}
			}
		}
	case "goto":
		if s.P != "" {
			s.moveTo(s.P, num("line"), num("col"))
		}
	case "goto_line":
		if line, col, ok := parseLine(vals["goto"]); ok && s.P != "" {
			s.moveTo(s.P, line, col)
		}
	case "jump":
		if s.W != nil && s.W.P == s.P && (s.V == "defs" || s.V == "impls" || s.V == "refs") {
			s.L, s.C = s.W.L, s.W.C // the way back returns to the name the list was asked for
		}
		s.moveTo(clip(vals["path"], 4096), num("line"), num("col"))
	case "back":
		s.back()
	case "forward":
		s.forward()
	case "more":
		s.F = num("from")
	case "prev":
		s.F = max(1, s.firstShown()-2000)
	case "search":
		s.Q = clip(strings.TrimSpace(vals["q"]), maxQuery)
		s.V = ""
		if s.Q != "" {
			s.V = "search"
		}
	case "scope":
		s.K = ""
		if vals["scope"] == "text" {
			s.K = "text"
		}
		s.V = "search"
	case "close_list":
		s.V, s.W, s.D, s.Q = "", nil, "", ""
	case "close_hover":
		s.X = nil
	case "refs_at":
		if s.P != "" {
			at := here()
			s.V, s.W = "refs", &at
		}
	case "hover_at":
		if s.P != "" {
			at := here()
			s.X = &at
			s.L, s.C = at.L, at.C
		}
	case "def_at", "impl_at":
		if s.P == "" {
			break
		}
		method := "definition"
		if r.Action == "impl_at" {
			method = "implementation"
		}
		n, err := a.jumpTo(ctx, &s, method, here())
		switch {
		case err != nil:
			return &wire.ActionResult{Toast: clip(t.noDef+errText(err), 300)}, nil
		case n == 0 && method == "definition":
			note = t.noDefHere
		case n == 0:
			note = t.noImplHere
		}
	case "nav":
		// A name in the code followed (⌘-click, ⌘B, ⌥F7, ⌘[), where the app draws a code's navigate (UI version 13:
		// values line, column, word and intent definition, references or back; implementation and hover too). The file
		// is the page's own, never one the values name.
		switch vals["intent"] {
		case "back":
			s.back()
		case "forward":
			s.forward()
		case "references":
			if s.P != "" {
				at := here()
				s.V, s.W = "refs", &at
			}
		case "hover":
			if s.P != "" {
				at := here()
				s.X = &at
			}
		case "implementation", "definition", "":
			if s.P == "" {
				break
			}
			method := "definition"
			if vals["intent"] == "implementation" {
				method = "implementation"
			}
			n, err := a.jumpTo(ctx, &s, method, here())
			switch {
			case err != nil:
				return &wire.ActionResult{Toast: clip(t.noDef+errText(err), 300)}, nil
			case n == 0 && method == "implementation":
				note = t.noImplHere
			case n == 0:
				note = t.noDefHere
			}
		}
	default:
		return nil, &ext.Error{Code: "unknown_action", Message: clip(r.Action, 128)}
	}
	s.sanitize()
	return &wire.ActionResult{View: a.view(ctx, r, &s, note)}, nil
}

// parseLine reads "120" or "120:7" (also "120,7" and spaces) as a line and a column.
func parseLine(v string) (int, int, bool) {
	v = strings.TrimSpace(v)
	ls, cs, _ := strings.Cut(strings.NewReplacer(",", ":", "：", ":", " ", ":").Replace(v), ":")
	line, err := strconv.Atoi(ls)
	if err != nil || line <= 0 {
		return 0, 0, false
	}
	col, _ := strconv.Atoi(strings.TrimSpace(cs))
	return line, max(0, col), true
}

// jumpTo asks for the definition or the implementations at at: one opens it (the way back remembers where you were),
// several are listed, none leaves the page as it was. It says how many there were.
func (a *hubApp) jumpTo(ctx context.Context, s *state, method string, at spot) (int, error) {
	_, call := a.snapshot()
	cctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	var res locationsResult
	if err := call(cctx, s.M, method, map[string]any{"repo": s.R, "path": at.P, "line": at.L, "col": at.C}, &res); err != nil {
		return 0, err
	}
	if res.NoServer != "" {
		return 0, fmt.Errorf("no %s language server on this computer", res.NoServer)
	}
	switch len(res.Locations) {
	case 0:
		return 0, nil
	case 1:
		l := res.Locations[0]
		s.L, s.C = at.L, at.C // the way back returns to the name, not to the top of the old window
		s.moveTo(l.Path, l.Line, l.Col)
		return 1, nil
	default:
		s.V, s.W = "defs", &at
		if method == "implementation" {
			s.V = "impls"
		}
		return len(res.Locations), nil
	}
}

// moveTo opens p at (line, col), remembering where the page was; a new place drops the way forward.
func (s *state) moveTo(p string, line, col int) {
	if p == "" || !filepath.IsAbs(p) {
		return
	}
	s.remember()
	s.N = nil
	s.P, s.L, s.C, s.F, s.V, s.W, s.X, s.D = p, line, col, 0, "", nil, nil, ""
}

// back returns to where the page was before its last jump; forward undoes a back.
func (s *state) back() {
	if n := len(s.H); n > 0 {
		b := s.H[n-1]
		s.H = s.H[:n-1]
		if s.P != "" {
			s.N = append(s.N, spot{P: s.P, L: s.L, C: s.C})
		}
		s.P, s.L, s.C, s.F, s.V, s.W, s.X, s.D = b.P, b.L, b.C, 0, "", nil, nil, ""
		s.trim()
	}
}

func (s *state) forward() {
	if n := len(s.N); n > 0 {
		f := s.N[n-1]
		s.N = s.N[:n-1]
		if s.P != "" {
			s.H = append(s.H, spot{P: s.P, L: s.L, C: s.C})
		}
		s.P, s.L, s.C, s.F, s.V, s.W, s.X, s.D = f.P, f.L, f.C, 0, "", nil, nil, ""
		s.trim()
	}
}

func (s *state) remember() {
	if s.P == "" {
		return
	}
	if n := len(s.H); n > 0 && s.H[n-1] == (spot{P: s.P, L: s.L, C: s.C}) {
		return
	}
	s.H = append(s.H, spot{P: s.P, L: s.L, C: s.C})
	s.trim()
}

// trim keeps the state small: the ways back and forward hold the last maxBack places, and a long way with long paths
// drops its oldest steps, then the folders open longest.
func (s *state) trim() {
	if len(s.H) > maxBack {
		s.H = s.H[len(s.H)-maxBack:]
	}
	if len(s.N) > maxBack {
		s.N = s.N[len(s.N)-maxBack:]
	}
	for len(ext.EncodeState(s)) > stateBudget {
		switch {
		case len(s.N) > 0:
			s.N = s.N[1:]
		case len(s.H) > 0:
			s.H = s.H[1:]
		case len(s.O) > 0:
			s.O = s.O[1:]
		default:
			return
		}
	}
}

func (s *state) toggle(dir string) {
	if dir == "" || !relOK(dir) || len(dir) > 1024 {
		return
	}
	if i := slices.Index(s.O, dir); i >= 0 {
		s.O = slices.Delete(s.O, i, i+1)
		// closing a folder closes the ones inside it
		s.O = slices.DeleteFunc(s.O, func(d string) bool { return strings.HasPrefix(d, dir+"/") })
		return
	}
	s.O = append(s.O, dir)
	if len(s.O) > maxOpenDirs {
		s.O = s.O[len(s.O)-maxOpenDirs:]
	}
	s.trim()
}

func (s *state) firstShown() int {
	if s.F > 0 {
		return s.F
	}
	return max(1, s.L-windowBefore)
}

// navigateField is the index of wire.Component's Navigate (UI version 13: a name chosen in the code is followed with
// it), or -1 while the SDK copy predates it. Set through it, the page draws a code's navigate as soon as the SDK is
// synced, with no change here; until then it draws the rows that do the same on UI version 12.
var navigateField = func() int {
	f, ok := reflect.TypeOf(wire.Component{}).FieldByName("Navigate")
	if !ok || f.Type != reflect.TypeOf(&wire.Action{}) || len(f.Index) != 1 {
		return -1
	}
	return f.Index[0]
}()

// setNavigate sets c's navigate when the SDK has it, and says whether it did.
func setNavigate(c *wire.Component, a *wire.Action) bool {
	if navigateField < 0 {
		return false
	}
	reflect.ValueOf(c).Elem().Field(navigateField).Set(reflect.ValueOf(a))
	return true
}

func errorEmpty(key, title string, err error) wire.Component {
	return wire.Component{Type: wire.ComponentEmpty, Key: key, Title: title, Detail: clip(errText(err), 500), Icon: wire.NavIconWarning}
}

// errText is an error in words: an extension error's message, else the error.
func errText(err error) string {
	var e *ext.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

func clip(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// kindName is an LSP SymbolKind in words.
func kindName(t text, k int) string {
	if k > 0 && k <= len(t.kinds) {
		return t.kinds[k-1]
	}
	return t.symbolWord
}
