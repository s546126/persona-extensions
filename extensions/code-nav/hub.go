package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The program on the hub: the 代码 page. It keeps where you are (computer, repository, file, line, the folders open in
// the tree, the way back) in the page's state, and asks the computer's program (nodes.call → nav.*) for everything
// else, so a page drawn again shows the files as they are now.

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
	V string   `json:"v,omitempty"` // what the main pane lists instead of the file: refs, defs, search
	Q string   `json:"q,omitempty"` // the symbol search
	W *spot    `json:"w,omitempty"` // where refs / defs were asked from
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
)

func (a *hubApp) snapshot() ([]string, nodeCaller) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.machines...), a.call
}

func (a *hubApp) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	var s state
	_ = r.DecodeState(&s)
	return a.view(ctx, r, &s, ""), nil
}

// view draws the page for s; note, when set, is a line above the code (a definition that was not found).
func (a *hubApp) view(ctx context.Context, r *ext.UIRequest, s *state, note string) *wire.View {
	t := texts(r.Locale)
	machines, call := a.snapshot()
	v := &wire.View{Title: t.title}
	if len(machines) == 0 {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Key: "setup", Title: t.noMachines, Text: t.noMachinesHint, Icon: wire.NavIconCode}}}}
		return v
	}
	if !slices.Contains(machines, s.M) {
		*s = state{M: machines[0]}
	}
	if len(machines) > 1 {
		f := &wire.Filter{Name: "machine", Title: t.machine, Value: s.M, Action: wire.Action{ID: "machine"}}
		for _, m := range machines {
			f.Choices = append(f.Choices, wire.Choice{Value: m})
		}
		v.Filter = f
	}
	nc := func(method string, params, out any) error {
		cctx, cancel := context.WithTimeout(ctx, 35*time.Second)
		defer cancel()
		return call(cctx, s.M, method, params, out)
	}
	var repos reposResult
	if err := nc("repos", map[string]any{}, &repos); err != nil {
		v.State = ext.EncodeState(s)
		v.Sections = []wire.Section{{Items: []wire.Component{errorEmpty(t.noComputer, err)}}}
		return v
	}
	if s.R != "" && !slices.ContainsFunc(repos.Repos, func(x repo) bool { return x.Path == s.R }) {
		s.R, s.P, s.O, s.H, s.V, s.W = "", "", nil, nil, "", nil
	}
	if s.R == "" {
		v.State = ext.EncodeState(s)
		v.Sections = repoList(t, repos)
		return v
	}
	rp := repos.Repos[slices.IndexFunc(repos.Repos, func(x repo) bool { return x.Path == s.R })]
	v.Title = rp.Name
	v.Layout = wire.LayoutSplit
	v.Search = &wire.Search{Placeholder: t.searchHint, Value: s.Q, Action: wire.Action{ID: "search"}}

	var list listResult
	listErr := nc("list", map[string]any{"repo": s.R}, &list)

	side := []wire.Section{}
	if listErr != nil {
		side = append(side, wire.Section{Pane: wire.PaneSide, Items: []wire.Component{errorEmpty(t.noFiles, listErr)}})
	} else {
		title := fmt.Sprintf(t.files, len(list.Files))
		if list.Truncated {
			title += t.filesCut
		}
		side = append(side, wire.Section{Key: "tree", Title: title, Pane: wire.PaneSide, Items: []wire.Component{tree(s, rp, list.Files)}})
	}

	main := wire.Section{Key: "main"}
	main.Items = append(main.Items, crumbs(t, s, rp))
	if len(s.H) > 0 {
		main.Items = append(main.Items, wire.Component{Type: wire.ComponentButton, Key: "back", Title: t.back, Action: &wire.Action{ID: "back"}})
	}
	if note != "" {
		main.Items = append(main.Items, wire.Component{Type: wire.ComponentText, Key: "note", Style: wire.TextStyleSecondary, Text: note})
	}

	switch s.V {
	case "search":
		main.Items = append(main.Items, a.searchResults(t, s, nc)...)
	case "refs", "defs":
		main.Items = append(main.Items, a.locationList(t, s, nc)...)
	default:
		if s.P == "" {
			main.Items = append(main.Items, readme(t, s, rp, list.Files, nc)...)
			break
		}
		items, outline := a.file(t, r, s, nc)
		main.Items = append(main.Items, items...)
		if len(outline) > 0 {
			side = append(side, wire.Section{Key: "outline", Title: t.outline, Pane: wire.PaneSide, Items: outline})
		}
	}
	v.State = ext.EncodeState(s)
	v.Sections = append(side, main)
	return v
}

// repoList is the first page: the repositories found on the computer, and which language servers it has.
func repoList(t text, repos reposResult) []wire.Section {
	if len(repos.Repos) == 0 {
		hint := t.noReposHint
		if len(repos.Roots) > 0 {
			hint = fmt.Sprintf(t.noReposIn, strings.Join(repos.Roots, t.listSep))
		}
		return []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Key: "none", Title: t.noRepos, Text: hint, Icon: wire.NavIconFolder}}}}
	}
	sec := wire.Section{Key: "repos", Title: fmt.Sprintf(t.repos, len(repos.Repos))}
	for _, r := range repos.Repos {
		sec.Items = append(sec.Items, wire.Component{
			Type: wire.ComponentRow, Key: "repo:" + clip(r.Path, 120), Title: r.Name, Subtitle: clip(r.Path, 300), Icon: wire.NavIconFolder,
			Action: &wire.Action{ID: "repo", Values: map[string]string{"repo": r.Path}},
		})
	}
	foot := t.noLsp
	if len(repos.Lsp) > 0 {
		foot = fmt.Sprintf(t.lsp, strings.Join(repos.Lsp, t.listSep))
		if !slices.Contains(repos.Lsp, "go") {
			foot += t.noGopls
		}
	}
	sec.Footer = foot
	return []wire.Section{sec}
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
		if rel, err := filepath.Rel(rp.Path, s.P); err == nil && !strings.HasPrefix(rel, "..") {
			sel = filepath.ToSlash(rel)
			for d := path.Dir(sel); d != "." && d != "/"; d = path.Dir(d) {
				open[d] = true
			}
		}
	}
	c := wire.Component{Type: wire.ComponentFileTree, Key: "tree", Selected: sel, Action: &wire.Action{ID: "open"}, Expand: &wire.Action{ID: "expand"}}
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

func crumbs(t text, s *state, rp repo) wire.Component {
	c := wire.Component{Type: wire.ComponentBreadcrumb, Key: "crumbs", Crumbs: []wire.Crumb{{Title: t.repoList, Action: &wire.Action{ID: "repos"}}, {Title: rp.Name, Action: &wire.Action{ID: "home"}}}}
	if s.P == "" {
		c.Crumbs[1].Action = nil
		return c
	}
	rel := relTo(rp.Path, s.P)
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		cr := wire.Crumb{Title: clip(p, 200)}
		if i < len(parts)-1 && !strings.HasPrefix(rel, "..") {
			cr.Action = &wire.Action{ID: "expand_to", Values: map[string]string{"path": strings.Join(parts[:i+1], "/")}}
		}
		c.Crumbs = append(c.Crumbs, cr)
	}
	return c
}

// relTo is p relative to dir with / between names, or p itself when it is outside (Go's own sources, a module).
func relTo(dir, p string) string {
	if rel, err := filepath.Rel(dir, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(p)
}

// readme shows the repository's README when no file is open.
func readme(t text, s *state, rp repo, files []string, nc func(string, any, any) error) []wire.Component {
	for _, f := range files {
		if path.Dir(f) == "." && strings.EqualFold(strings.TrimSuffix(f, path.Ext(f)), "readme") && strings.EqualFold(path.Ext(f), ".md") {
			var rd readResult
			if err := nc("read", map[string]any{"path": filepath.Join(rp.Path, f)}, &rd); err == nil && !rd.Binary {
				return []wire.Component{{Type: wire.ComponentMarkdown, Key: "readme", Text: cutRunes(rd.Text, 20000)}}
			}
		}
	}
	return []wire.Component{{Type: wire.ComponentEmpty, Key: "pick", Title: t.pickFile, Text: t.pickFileHint, Icon: wire.NavIconFile}}
}

// file is the open file: its code around the marked line, and its outline for the side pane.
func (a *hubApp) file(t text, r *ext.UIRequest, s *state, nc func(string, any, any) error) ([]wire.Component, []wire.Component) {
	from := s.F
	if from <= 0 {
		from = max(1, s.L-windowBefore)
	}
	var rd readResult
	if err := nc("read", map[string]any{"path": s.P, "from": from}, &rd); err != nil {
		return []wire.Component{errorEmpty(t.noRead, err)}, nil
	}
	if rd.Binary {
		return []wire.Component{{Type: wire.ComponentEmpty, Key: "binary", Title: t.binary, Icon: wire.NavIconFile}}, nil
	}
	c := wire.Component{Type: wire.ComponentCode, Key: "code", Path: clip(rd.Rel, 4096), Text: rd.Text, StartLine: rd.From}
	if s.L >= rd.From && s.L < rd.From+strings.Count(rd.Text, "\n")+1 {
		c.Highlight = &wire.LineRange{Start: s.L, End: s.L}
	}
	if rd.More {
		c.Truncated = true
		c.Load = &wire.Action{ID: "more", Values: map[string]string{"from": strconv.Itoa(rd.From + strings.Count(rd.Text, "\n") + 1)}}
	}
	var items []wire.Component
	if rd.From > 1 {
		items = append(items, wire.Component{Type: wire.ComponentButton, Key: "prev", Title: fmt.Sprintf(t.earlier, rd.From-1), Action: &wire.Action{ID: "prev"}})
	}
	if rd.Repo == "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Key: "outside", Style: wire.TextStyleCaption, Text: t.outside})
	}
	items = append(items, c)
	if rd.Truncated {
		items = append(items, wire.Component{Type: wire.ComponentText, Key: "cut", Style: wire.TextStyleCaption, Text: t.fileCut})
	}

	var syms symbolsResult
	if _, ok := languageOf(s.P); !ok {
		return items, nil
	}
	if err := nc("symbols", map[string]any{"repo": s.R, "path": s.P}, &syms); err != nil {
		return items, []wire.Component{{Type: wire.ComponentText, Key: "outline-err", Style: wire.TextStyleCaption, Text: clip(t.noOutline+err.Error(), 300)}}
	}
	if syms.NoServer != "" {
		return items, []wire.Component{{Type: wire.ComponentText, Key: "outline-none", Style: wire.TextStyleCaption, Text: fmt.Sprintf(t.noServer, syms.NoServer)}}
	}
	var outline []wire.Component
	for i, sym := range syms.Symbols {
		if i >= maxOutline {
			break
		}
		at := map[string]string{"line": strconv.Itoa(sym.Line), "col": strconv.Itoa(sym.Col)}
		row := wire.Component{
			Type: wire.ComponentRow, Key: fmt.Sprintf("sym:%d", i), Title: clip(strings.Repeat("· ", min(sym.Depth, 4))+sym.Name, 200),
			Subtitle: clip(kindName(t, sym.Kind)+detail(sym.Detail), 200), Value: strconv.Itoa(sym.Line),
			Action: &wire.Action{ID: "goto", Values: at},
		}
		if r.UIVersion >= 6 {
			row.Actions = []wire.RowAction{{Title: t.findRefs, Action: wire.Action{ID: "refs_at", Values: at}}}
		}
		outline = append(outline, row)
	}
	return items, outline
}

func detail(d string) string {
	if d == "" {
		return ""
	}
	return " · " + d
}

func (a *hubApp) searchResults(t text, s *state, nc func(string, any, any) error) []wire.Component {
	var res symbolsResult
	if err := nc("search", map[string]any{"repo": s.R, "query": s.Q}, &res); err != nil {
		return []wire.Component{errorEmpty(t.noSearch, err)}
	}
	if res.NoServer != "" {
		return []wire.Component{{Type: wire.ComponentEmpty, Key: "search-none", Title: fmt.Sprintf(t.noServer, res.NoServer), Icon: wire.NavIconSearch}}
	}
	if len(res.Symbols) == 0 {
		return []wire.Component{{Type: wire.ComponentEmpty, Key: "search-empty", Title: fmt.Sprintf(t.noMatch, s.Q), Icon: wire.NavIconSearch}}
	}
	out := []wire.Component{{Type: wire.ComponentText, Key: "search-title", Style: wire.TextStyleHeadline, Text: clip(fmt.Sprintf(t.matches, len(res.Symbols), s.Q), 200)}}
	for i, sym := range res.Symbols {
		where := fmt.Sprintf("%s:%d", sym.Rel, sym.Line)
		if sym.Container != "" {
			where = sym.Container + " · " + where
		}
		out = append(out, wire.Component{
			Type: wire.ComponentRow, Key: fmt.Sprintf("hit:%d", i), Title: clip(sym.Name, 200), Subtitle: clip(kindName(t, sym.Kind)+" · "+where, 300),
			Action: &wire.Action{ID: "jump", Values: map[string]string{"path": sym.Path, "line": strconv.Itoa(sym.Line), "col": strconv.Itoa(sym.Col)}},
		})
	}
	return out
}

func (a *hubApp) locationList(t text, s *state, nc func(string, any, any) error) []wire.Component {
	if s.W == nil {
		s.V = ""
		return nil
	}
	method := "references"
	if s.V == "defs" {
		method = "definition"
	}
	var res locationsResult
	if err := nc(method, map[string]any{"repo": s.R, "path": s.W.P, "line": s.W.L, "col": s.W.C}, &res); err != nil {
		return []wire.Component{errorEmpty(t.noRefs, err)}
	}
	if res.NoServer != "" {
		return []wire.Component{{Type: wire.ComponentEmpty, Key: "refs-none", Title: fmt.Sprintf(t.noServer, res.NoServer), Icon: wire.NavIconSearch}}
	}
	word := res.Word
	if word == "" {
		word = t.thisSymbol
	}
	title := fmt.Sprintf(t.refsOf, word, len(res.Locations))
	if s.V == "defs" {
		title = fmt.Sprintf(t.defsOf, word, len(res.Locations))
	}
	out := []wire.Component{
		{Type: wire.ComponentText, Key: "refs-title", Style: wire.TextStyleHeadline, Text: clip(title, 200)},
		{Type: wire.ComponentButton, Key: "refs-close", Title: t.backToCode, Action: &wire.Action{ID: "close_list"}},
	}
	if len(res.Locations) == 0 {
		out = append(out, wire.Component{Type: wire.ComponentEmpty, Key: "refs-empty", Title: t.noneFound, Icon: wire.NavIconSearch})
	}
	for i, l := range res.Locations {
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
	var s state
	_ = r.DecodeState(&s)
	vals := r.Values
	num := func(k string) int { n, _ := strconv.Atoi(vals[k]); return n }
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
		s.P, s.L, s.C, s.F, s.V, s.W = "", 0, 0, 0, "", nil
	case "open":
		if p := vals["path"]; p != "" && s.R != "" && !strings.Contains("/"+p+"/", "/../") {
			s.moveTo(filepath.Join(s.R, filepath.FromSlash(p)), 0, 0)
		}
	case "expand":
		s.toggle(vals["path"])
	case "expand_to":
		if p := vals["path"]; p != "" && !slices.Contains(s.O, p) {
			s.toggle(p)
		}
	case "goto":
		if s.P != "" {
			s.moveTo(s.P, num("line"), num("col"))
		}
	case "jump":
		s.moveTo(vals["path"], num("line"), num("col"))
	case "back":
		s.back()
	case "more":
		s.F = num("from")
	case "prev":
		s.F = max(1, s.firstShown()-2000)
	case "search":
		s.Q = strings.TrimSpace(vals["q"])
		s.V = ""
		if s.Q != "" {
			s.V = "search"
		}
	case "close_list":
		s.V, s.W, s.Q = "", nil, ""
	case "refs_at":
		if s.P != "" {
			s.V, s.W = "refs", &spot{P: s.P, L: num("line"), C: num("col")}
		}
	case "nav":
		// A name in the code followed (⌘-click, ⌘B, ⌥F7, ⌘[), where the app draws a code's navigate (UI version 13:
		// values line, column and intent definition, references or back). The file is the page's own, never one the
		// values name.
		if vals["intent"] == "back" {
			s.back()
			break
		}
		if s.P == "" {
			break
		}
		at := spot{P: s.P, L: num("line"), C: num("column")}
		if vals["intent"] == "references" {
			s.V, s.W = "refs", &at
			break
		}
		n, err := a.jumpToDefinition(ctx, &s, at)
		switch {
		case err != nil:
			return &wire.ActionResult{Toast: clip(t.noDef+err.Error(), 300)}, nil
		case n == 0:
			note = t.noDefHere
		}
	default:
		return nil, &ext.Error{Code: "unknown_action", Message: r.Action}
	}
	return &wire.ActionResult{View: a.view(ctx, r, &s, note)}, nil
}

// jumpToDefinition asks for the definition at at: one opens it (the way back remembers where you were), several are
// listed, none leaves the page as it was. It says how many there were.
func (a *hubApp) jumpToDefinition(ctx context.Context, s *state, at spot) (int, error) {
	_, call := a.snapshot()
	cctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	var res locationsResult
	if err := call(cctx, s.M, "definition", map[string]any{"repo": s.R, "path": at.P, "line": at.L, "col": at.C}, &res); err != nil {
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
		return len(res.Locations), nil
	}
}

// moveTo opens p at (line, col), remembering where the page was.
func (s *state) moveTo(p string, line, col int) {
	if p == "" {
		return
	}
	s.remember()
	s.P, s.L, s.C, s.F, s.V, s.W = p, line, col, 0, "", nil
}

// back returns to where the page was before its last jump.
func (s *state) back() {
	if n := len(s.H); n > 0 {
		b := s.H[n-1]
		s.H = s.H[:n-1]
		s.P, s.L, s.C, s.F, s.V, s.W = b.P, b.L, b.C, 0, "", nil
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
	if len(s.H) > maxBack {
		s.H = s.H[len(s.H)-maxBack:]
	}
	// keep the state small: a long way back with long paths drops its oldest steps
	for len(s.H) > 1 && len(ext.EncodeState(s)) > 12<<10 {
		s.H = s.H[1:]
	}
}

func (s *state) toggle(dir string) {
	if dir == "" || strings.Contains("/"+dir+"/", "/../") {
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
}

func (s *state) firstShown() int {
	if s.F > 0 {
		return s.F
	}
	return max(1, s.L-windowBefore)
}

func errorEmpty(title string, err error) wire.Component {
	msg := err.Error()
	var e *ext.Error
	if errors.As(err, &e) {
		msg = e.Message
	}
	return wire.Component{Type: wire.ComponentEmpty, Key: "error", Title: title, Text: clip(msg, 500), Icon: wire.NavIconWarning}
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
