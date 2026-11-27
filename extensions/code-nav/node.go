package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
)

// The program on each computer you pick: it lists the repositories under the configured folders, lists, reads and
// searches their files, and asks a language server about them. It reads only: no file is written, no command but the language
// servers (and git ls-files, go env) is run. Every path is checked against the folders it may read.

type config struct {
	Machines    []string `json:"machines"`
	Roots       []string `json:"roots"`
	Gopls       string   `json:"gopls"`
	TypeScript  string   `json:"typescript_server"`
	Python      string   `json:"python_server"`
	Rust        string   `json:"rust_server"`
	Swift       string   `json:"swift_server"`
	IdleMinutes int      `json:"idle_minutes"`
}

// customServer is the server path the settings give for a language, or "".
func (c config) customServer(lang string) string {
	switch lang {
	case "go":
		return c.Gopls
	case "typescript", "javascript":
		return c.TypeScript
	case "python":
		return c.Python
	case "rust":
		return c.Rust
	case "swift":
		return c.Swift
	}
	return ""
}

// idle is how long a language server nobody asks anything is kept running.
func (c config) idle() time.Duration {
	if c.IdleMinutes > 0 {
		return time.Duration(min(c.IdleMinutes, 24*60)) * time.Minute
	}
	return idleStop
}

type node struct {
	mu      sync.Mutex
	cfg     config
	home    string
	env     []string
	repos   []repo
	found   []string // languages whose server was found, with the repositories
	reposAt time.Time
	extra   []string // read-only folders definitions may lead to: GOROOT, the module cache
	lists   map[string]fileList
	servers map[string]*serverSlot
	stopped chan struct{}
	once    sync.Once
}

type repo struct {
	Name string `json:"name"`
	Path string `json:"path"` // absolute, links followed
}

type fileList struct {
	at        time.Time
	files     []string
	truncated bool
}

// serverSlot holds one root's server for one language, started once at a time.
type serverSlot struct {
	mu  sync.Mutex
	srv *lspServer
	err error
	at  time.Time // when the last start failed: not tried again for a while
}

const (
	maxFiles     = 20000            // files listed per repository
	maxRead      = 4 << 20          // bytes of a file read
	maxRepos     = 200              // repositories listed
	listFresh    = 15 * time.Second // how long a repository's file list is reused
	reposFresh   = 60 * time.Second
	idleStop     = 10 * time.Minute // a language server nobody asked for this long is stopped
	retryStart   = 30 * time.Second // after a failed start
	windowChars  = 190000           // characters of a file sent at once
	windowBefore = 200              // lines shown above a line jumped to
)

func (n *node) program(id, version string) ext.Program {
	return ext.Program{
		ID: id, Version: version, Manifest: manifest,
		Init:       n.init,
		Namespaces: map[string]ext.Handler{"nav": n.handle},
	}
}

func (n *node) init(_ context.Context, in *ext.Init, _ *ext.Hub) (map[string]any, error) {
	var c config
	if err := in.Config(&c); err != nil {
		return nil, err
	}
	n.setup(c)
	go n.reap()
	return nil, nil
}

// setup reads the computer: the home, an environment a language server can work in, and where Go keeps its sources.
func (n *node) setup(c config) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cfg = c
	n.home = homeDir()
	n.env = programEnv(n.home)
	n.lists = map[string]fileList{}
	n.servers = map[string]*serverSlot{}
	n.stopped = make(chan struct{})
	n.extra = goDirs(n.env)
}

func (n *node) cleanup() {
	n.once.Do(func() {
		n.mu.Lock()
		if n.stopped != nil {
			close(n.stopped)
		}
		slots := n.servers
		n.servers = map[string]*serverSlot{}
		n.mu.Unlock()
		for _, s := range slots {
			s.mu.Lock()
			if s.srv != nil {
				s.srv.stop()
			}
			s.mu.Unlock()
		}
	})
}

// reap stops language servers nobody has asked anything for a while.
func (n *node) reap() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		n.mu.Lock()
		stopped := n.stopped
		n.mu.Unlock()
		select {
		case <-stopped:
			return
		case <-t.C:
		}
		n.reapIdle(time.Now())
	}
}

// reapIdle stops the servers idle longer than the settings allow, or dead. The slot stays in the map: a request may
// hold it already, and starts the next server in it.
func (n *node) reapIdle(now time.Time) int {
	n.mu.Lock()
	limit := n.cfg.idle()
	slots := make([]*serverSlot, 0, len(n.servers))
	for _, s := range n.servers {
		slots = append(slots, s)
	}
	n.mu.Unlock()
	var idle []*lspServer
	for _, s := range slots {
		if !s.mu.TryLock() { // starting or in use right now: not idle
			continue
		}
		if s.srv != nil && (!s.srv.alive() || now.Sub(s.srv.idleSince()) > limit) {
			idle = append(idle, s.srv)
			s.srv = nil
		}
		s.mu.Unlock()
	}
	for _, s := range idle {
		s.stop()
	}
	return len(idle)
}

// handle answers the hub's nodes.call for nav.<method>.
func (n *node) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var p navParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &ext.Error{Code: "invalid", Message: err.Error()}
		}
	}
	switch method {
	case "repos":
		return n.reposResult(), nil
	case "list":
		r, err := n.repoAt(p.Repo)
		if err != nil {
			return nil, err
		}
		files, truncated := n.list(ctx, r)
		return listResult{Files: files, Truncated: truncated}, nil
	case "read":
		return n.read(p)
	case "definition", "references", "implementation":
		return n.locations(ctx, method, p)
	case "hover":
		return n.hover(ctx, p)
	case "grep":
		r, err := n.repoAt(p.Repo)
		if err != nil {
			return nil, err
		}
		return n.grep(ctx, r, p.Query)
	case "symbols":
		return n.symbols(ctx, p)
	case "search":
		return n.search(ctx, p)
	}
	return nil, ext.UnknownMethod("nav." + method)
}

// navParams is every method's parameters; each reads the ones it needs.
type navParams struct {
	Repo  string `json:"repo,omitempty"`  // a repository's path, as nav.repos gave it
	Path  string `json:"path,omitempty"`  // a file's absolute path
	From  int    `json:"from,omitempty"`  // first line wanted (1-based)
	Line  int    `json:"line,omitempty"`  // 1-based
	Col   int    `json:"col,omitempty"`   // 1-based, in characters
	Query string `json:"query,omitempty"` // a symbol search
}

type reposResult struct {
	Repos []repo   `json:"repos"`
	Roots []string `json:"roots"` // the configured folders as written
	Lsp   []string `json:"lsp"`   // languages whose server was found
}

type hoverResult struct {
	Text     string `json:"text,omitempty"`
	Markdown bool   `json:"markdown,omitempty"`
	Word     string `json:"word,omitempty"`
	NoServer string `json:"no_server,omitempty"`
}

type listResult struct {
	Files     []string `json:"files"` // paths relative to the repository, / between names
	Truncated bool     `json:"truncated,omitempty"`
}

type readResult struct {
	Path      string `json:"path"`
	Repo      string `json:"repo,omitempty"` // the repository it is in; empty for Go's own sources and modules
	Rel       string `json:"rel"`            // the path to show: relative to the repository, or to GOROOT / the module cache
	Text      string `json:"text"`
	From      int    `json:"from"`
	Lines     int    `json:"lines"` // the file's line count
	More      bool   `json:"more,omitempty"`
	Binary    bool   `json:"binary,omitempty"`
	Truncated bool   `json:"truncated,omitempty"` // the file is longer than the program reads
}

type location struct {
	Path    string `json:"path"`
	Rel     string `json:"rel"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Preview string `json:"preview,omitempty"`
}

type locationsResult struct {
	Locations []location `json:"locations"`
	Word      string     `json:"word,omitempty"`
	NoServer  string     `json:"no_server,omitempty"` // the language whose server is missing
}

type symbol struct {
	Name      string `json:"name"`
	Kind      int    `json:"kind"`
	Detail    string `json:"detail,omitempty"`
	Container string `json:"container,omitempty"`
	Path      string `json:"path,omitempty"`
	Rel       string `json:"rel,omitempty"`
	Line      int    `json:"line"`
	Col       int    `json:"col"`
	Depth     int    `json:"depth,omitempty"`
}

type symbolsResult struct {
	Symbols  []symbol `json:"symbols"`
	NoServer string   `json:"no_server,omitempty"`
}

func (n *node) reposResult() reposResult {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.repos == nil || time.Since(n.reposAt) > reposFresh {
		n.repos = findRepos(n.cfg.Roots, n.home)
		n.reposAt = time.Now()
		n.found = nil
		for _, l := range languages {
			if _, ok := n.serverLocked(l); ok {
				n.found = append(n.found, l.name)
			}
		}
	}
	return reposResult{Repos: append([]repo(nil), n.repos...), Roots: n.cfg.Roots, Lsp: append([]string(nil), n.found...)}
}

// repoAt is the configured repository at path (as nav.repos gave it), never anything else.
func (n *node) repoAt(path string) (repo, error) {
	for _, r := range n.reposResult().Repos {
		if r.Path == path {
			return r, nil
		}
	}
	return repo{}, &ext.Error{Code: "not_found", Message: "not one of the configured repositories: " + path}
}

// findRepos lists the repositories in roots: a root that is one, or the ones up to two levels below it.
func findRepos(roots []string, home string) []repo {
	var out []repo
	seen := map[string]bool{}
	realHome, _ := filepath.EvalSymlinks(home)
	add := func(p string) {
		real, err := filepath.EvalSymlinks(p)
		if err != nil || seen[real] || len(out) >= maxRepos {
			return
		}
		// the home folder, or a folder holding it, is never one repository (a stray package.json in ~ would open
		// everything in it)
		if real == "/" || realHome != "" && within(realHome, real) {
			return
		}
		seen[real] = true
		out = append(out, repo{Name: filepath.Base(real), Path: real})
	}
	for _, r := range roots {
		dir := expandHome(strings.TrimSpace(r), home)
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		if isRepo(dir) {
			add(dir)
			continue
		}
		for _, d1 := range subdirs(dir) {
			if isRepo(d1) {
				add(d1)
				continue
			}
			for _, d2 := range subdirs(d1) {
				if isRepo(d2) {
					add(d2)
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// isRepo says whether dir is a repository: a Git work tree, or a project of a language the page knows (its marker
// file at the top).
func isRepo(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	return len(workspaceLanguages(dir)) > 0
}

func subdirs(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || skipDir[name] {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out
}

// skipDir are folders never listed: dependencies and build output that bury the code.
var skipDir = map[string]bool{"node_modules": true, ".git": true, "__pycache__": true, ".venv": true}

// list is a repository's files, from git when it is a repository (what .gitignore leaves out stays out), otherwise a
// walk that skips hidden folders and node_modules.
func (n *node) list(ctx context.Context, r repo) ([]string, bool) {
	n.mu.Lock()
	if l, ok := n.lists[r.Path]; ok && time.Since(l.at) < listFresh {
		n.mu.Unlock()
		return l.files, l.truncated
	}
	env, home := n.env, n.home
	n.mu.Unlock()
	files, truncated := gitFiles(ctx, r.Path, env)
	if files == nil {
		files, truncated = walkFiles(r.Path)
	}
	// files that hold secrets are not listed at all, as they are never read
	files = slices.DeleteFunc(files, func(f string) bool { return secretPath(filepath.Join(r.Path, filepath.FromSlash(f)), home) })
	n.mu.Lock()
	n.lists[r.Path] = fileList{at: time.Now(), files: files, truncated: truncated}
	n.mu.Unlock()
	return files, truncated
}

func gitFiles(ctx context.Context, dir string, env []string) ([]string, bool) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return nil, false
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-c", "core.quotePath=off", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = dir
	cmd.Env = append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var files []string
	seen := map[string]bool{}
	for _, f := range strings.Split(string(out), "\x00") {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		if len(files) >= maxFiles {
			return sortFiles(files), true
		}
		files = append(files, f)
	}
	return sortFiles(files), false
}

func walkFiles(dir string) ([]string, bool) {
	var files []string
	truncated := false
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && (strings.HasPrefix(d.Name(), ".") || skipDir[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(files) >= maxFiles {
			truncated = true
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(dir, p)
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return sortFiles(files), truncated
}

func sortFiles(files []string) []string {
	sort.Strings(files)
	return files
}

// allowed checks that path may be read and says where it is: the repository it is in, or "" for GOROOT and the module
// cache, and the path to show.
func (n *node) allowed(path string) (real, repoPath, rel string, err error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", "", "", &ext.Error{Code: "invalid", Message: "path is not absolute"}
	}
	real, err = filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", "", "", &ext.Error{Code: "not_found", Message: "no such file"}
	}
	n.mu.Lock()
	home := n.home
	extra := append([]string(nil), n.extra...)
	n.mu.Unlock()
	if secretPath(real, home) {
		return "", "", "", &ext.Error{Code: "forbidden", Message: "this path holds secrets and is not shown"}
	}
	for _, r := range n.reposResult().Repos {
		if within(real, r.Path) {
			rel, _ := filepath.Rel(r.Path, real)
			return real, r.Path, filepath.ToSlash(rel), nil
		}
	}
	for _, d := range extra {
		if within(real, d) {
			rel, _ := filepath.Rel(d, real)
			return real, "", filepath.ToSlash(rel), nil
		}
	}
	return "", "", "", &ext.Error{Code: "forbidden", Message: "outside the configured folders"}
}

func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// secretPath mirrors what Persona's own file viewer refuses: private keys, credential folders and files in the home,
// iCloud Drive, and .env files with real values.
func secretPath(path, home string) bool {
	p := strings.ToLower(filepath.ToSlash(path))
	if strings.Contains(p, "/library/mobile documents") || strings.Contains(p+"/", "/.git/") {
		return true
	}
	base := filepath.Base(p)
	switch base {
	case "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "id_ecdsa_sk", "id_ed25519_sk":
		return true
	}
	// .env, .env.local, .env-prod, .envrc...: all but the templates
	if strings.HasPrefix(base, ".env") && !strings.HasSuffix(base, ".example") && !strings.HasSuffix(base, ".sample") && !strings.HasSuffix(base, ".template") {
		return true
	}
	switch base { // credential files, wherever they are
	case ".npmrc", ".pypirc", ".netrc", "_netrc", ".git-credentials", ".pgpass", ".htpasswd", "credentials.toml", ".dockercfg":
		return true
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx", ".p8", ".keystore", ".jks"} {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	if home == "" {
		return false
	}
	h := strings.ToLower(filepath.ToSlash(home))
	if !strings.HasPrefix(p, h+"/") {
		return false
	}
	rel := strings.TrimPrefix(p, h+"/")
	for _, d := range []string{".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".password-store", "library/keychains", ".local/share/keyrings", ".config/gcloud", ".config/gh", ".persona"} {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	for _, f := range []string{".netrc", ".git-credentials", ".npmrc", ".pypirc", ".pgpass", ".claude/.credentials.json", ".codex/auth.json", ".claude.json"} {
		if rel == f {
			return true
		}
	}
	return false
}

func (n *node) read(p navParams) (readResult, error) {
	real, repoPath, rel, err := n.allowed(p.Path)
	if err != nil {
		return readResult{}, err
	}
	text, truncated, binary, err := readText(real)
	if err != nil {
		return readResult{}, err
	}
	res := readResult{Path: real, Repo: repoPath, Rel: rel, Binary: binary, Truncated: truncated}
	if binary {
		return res, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	res.Lines = len(lines)
	from := max(1, min(p.From, len(lines)))
	size, i := 0, from-1
	for ; i < len(lines); i++ {
		if size > 0 && size+len(lines[i])+1 > windowChars {
			break
		}
		size += len(lines[i]) + 1
	}
	res.From = from
	res.Text = strings.Join(lines[from-1:i], "\n")
	if len(res.Text) > windowChars { // one line over the window (a minified bundle): its start
		res.Text = cutRunes(res.Text, windowChars)
	}
	res.More = i < len(lines)
	return res, nil
}

// readText reads a file as text: at most maxRead bytes, binary when a NUL is in its first 8000 bytes (as Git decides).
func readText(path string) (text string, truncated, binary bool, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", false, false, &ext.Error{Code: "not_found", Message: "no such file"}
	}
	if !st.Mode().IsRegular() {
		return "", false, false, &ext.Error{Code: "invalid", Message: "not a file"}
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, false, err
	}
	defer f.Close()
	buf := make([]byte, min(st.Size(), maxRead))
	k, err := f.Read(buf)
	if err != nil && k == 0 && len(buf) > 0 {
		return "", false, false, err
	}
	buf = buf[:k]
	head := buf[:min(len(buf), 8000)]
	for _, b := range head {
		if b == 0 {
			return "", false, true, nil
		}
	}
	return strings.ReplaceAll(string(buf), "\r\n", "\n"), st.Size() > maxRead, false, nil
}

func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

// fileLines is a file's lines (as read for the page), for turning columns into the server's encoding.
func fileLines(path string) ([]string, string, error) {
	text, _, binary, err := readText(path)
	if err != nil {
		return nil, "", err
	}
	if binary {
		return nil, "", &ext.Error{Code: "invalid", Message: "binary file"}
	}
	return strings.Split(text, "\n"), text, nil
}

// server is the language server for path's language in repoPath, started when first needed.
func (n *node) server(ctx context.Context, repoPath, path string) (*lspServer, language, error) {
	lang, ok := languageOf(path)
	if !ok {
		return nil, language{}, errNoLanguage
	}
	n.mu.Lock()
	cmd, found := n.serverLocked(lang)
	env := n.env
	key := lang.name + "\x00" + repoPath
	slot := n.servers[key]
	if slot == nil {
		slot = &serverSlot{}
		n.servers[key] = slot
	}
	n.mu.Unlock()
	if !found {
		return nil, lang, errNoServer
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.srv != nil && slot.srv.alive() {
		return slot.srv, lang, nil
	}
	if slot.srv != nil { // dead: its process group goes before a new one starts
		slot.srv.kill()
		slot.srv = nil
	}
	if slot.err != nil && time.Since(slot.at) < retryStart {
		return nil, lang, slot.err
	}
	srv, err := startLSP(ctx, cmd.bin, cmd.args, repoPath, env)
	if err != nil {
		slot.err, slot.at = fmt.Errorf("could not start %s: %w", filepath.Base(cmd.bin), err), time.Now()
		return nil, lang, slot.err
	}
	slot.srv, slot.err = srv, nil
	return srv, lang, nil
}

var (
	errNoLanguage = &ext.Error{Code: "unsupported", Message: "no language server for this kind of file"}
	errNoServer   = errors.New("language server not found")
)

// serverLocked finds a language's server: the path the settings give, else the first of its programs on the
// program's PATH. A path from the settings speaks the way its program is known to (pyright-langserver --stdio, pylsp
// as it is), else as the language's first server does.
func (n *node) serverLocked(l language) (serverCmd, bool) {
	if custom := strings.TrimSpace(n.cfg.customServer(l.name)); custom != "" {
		p := expandHome(custom, n.home)
		if !filepath.IsAbs(p) {
			p = lookPath(p, n.env)
		}
		st, err := os.Stat(p)
		if p == "" || err != nil || !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
			return serverCmd{}, false
		}
		cmd := serverCmd{bin: p, args: l.servers[0].args}
		for _, s := range l.servers {
			if filepath.Base(p) == s.bin {
				cmd.args = s.args
			}
		}
		return cmd, true
	}
	for _, s := range l.servers {
		if p := lookPath(s.bin, n.env); p != "" {
			return serverCmd{bin: p, args: s.args}, true
		}
	}
	return serverCmd{}, false
}

// position checks path, finds its repository (or uses the one the page is in, for Go's own sources) and its server,
// and turns (line, col) into the server's position.
func (n *node) position(ctx context.Context, p navParams) (*lspServer, lspPos, string, []string, error) {
	real, repoPath, _, err := n.allowed(p.Path)
	if err != nil {
		return nil, lspPos{}, "", nil, err
	}
	if repoPath == "" {
		r, err := n.repoAt(p.Repo)
		if err != nil {
			return nil, lspPos{}, "", nil, err
		}
		repoPath = r.Path
	}
	lines, text, err := fileLines(real)
	if err != nil {
		return nil, lspPos{}, "", nil, err
	}
	srv, lang, err := n.server(ctx, repoPath, real)
	if err != nil {
		if errors.Is(err, errNoServer) {
			return nil, lspPos{}, lang.name, nil, errNoServer
		}
		return nil, lspPos{}, "", nil, err
	}
	if err := srv.open(real, lang.id, text); err != nil {
		return nil, lspPos{}, "", nil, err
	}
	line := max(1, min(p.Line, len(lines)))
	pos := lspPos{Line: line - 1}
	if p.Col > 0 {
		pos.Character = toLSP(lines[line-1], p.Col, srv.encoding)
	}
	return srv, pos, real, lines, nil
}

func (n *node) locations(ctx context.Context, method string, p navParams) (locationsResult, error) {
	srv, pos, real, lines, err := n.position(ctx, p)
	if errors.Is(err, errNoServer) {
		lang, _ := languageOf(p.Path)
		return locationsResult{NoServer: lang.name}, nil
	}
	if err != nil {
		return locationsResult{}, err
	}
	res := locationsResult{Word: wordAt(lines[pos.Line], p.Col)}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	doc := map[string]any{"textDocument": map[string]string{"uri": fileURI(real)}, "position": pos}
	var raw json.RawMessage
	switch method {
	case "definition", "implementation":
		err = srv.call(cctx, "textDocument/"+method, doc, &raw)
	default:
		doc["context"] = map[string]bool{"includeDeclaration": false}
		err = srv.call(cctx, "textDocument/references", doc, &raw)
	}
	if err != nil {
		return locationsResult{}, err
	}
	var locs []lspLocationOrLink
	if len(raw) > 0 && raw[0] == '{' {
		var one lspLocationOrLink
		if json.Unmarshal(raw, &one) == nil {
			locs = append(locs, one)
		}
	} else if len(raw) > 0 {
		_ = json.Unmarshal(raw, &locs)
	}
	cache := map[string][]string{}
	for _, l := range locs {
		if len(res.Locations) >= 200 {
			break
		}
		if loc, ok := n.toLocation(l.location(), srv.encoding, cache); ok {
			res.Locations = append(res.Locations, loc)
		}
	}
	return res, nil
}

// toLocation turns a server's location into one the page can open: a path it may read, a 1-based line and column, and
// the line's text.
func (n *node) toLocation(l lspLocation, encoding string, cache map[string][]string) (location, bool) {
	path := uriPath(l.URI)
	if path == "" {
		return location{}, false
	}
	real, _, rel, err := n.allowed(path)
	if err != nil {
		return location{}, false
	}
	lines, ok := cache[real]
	if !ok {
		lines, _, _ = fileLines(real)
		cache[real] = lines
	}
	if l.Range.Start.Line < 0 || l.Range.Start.Character < 0 {
		return location{}, false
	}
	loc := location{Path: real, Rel: rel, Line: l.Range.Start.Line + 1, Col: l.Range.Start.Character + 1}
	if l.Range.Start.Line < len(lines) {
		text := lines[l.Range.Start.Line]
		loc.Col = fromLSP(text, l.Range.Start.Character, encoding)
		loc.Preview = cutRunes(strings.TrimSpace(text), 300)
	}
	return loc, true
}

func (n *node) symbols(ctx context.Context, p navParams) (symbolsResult, error) {
	srv, _, real, lines, err := n.position(ctx, navParams{Repo: p.Repo, Path: p.Path, Line: 1})
	if errors.Is(err, errNoServer) {
		lang, _ := languageOf(p.Path)
		return symbolsResult{NoServer: lang.name}, nil
	}
	if err != nil {
		return symbolsResult{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var syms []lspSymbol
	if err := srv.call(cctx, "textDocument/documentSymbol", map[string]any{"textDocument": map[string]string{"uri": fileURI(real)}}, &syms); err != nil {
		return symbolsResult{}, err
	}
	var out []symbol
	var walk func(list []lspSymbol, depth int)
	walk = func(list []lspSymbol, depth int) {
		for _, s := range list {
			if len(out) >= 500 {
				return
			}
			r := s.SelectionRange
			if s.Location != nil {
				r = s.Location.Range
			}
			if r.Start.Line < 0 || r.Start.Character < 0 {
				continue
			}
			sym := symbol{Name: s.Name, Kind: s.Kind, Detail: cutRunes(s.Detail, 200), Container: s.ContainerName, Line: r.Start.Line + 1, Depth: depth}
			if r.Start.Line < len(lines) {
				sym.Col = fromLSP(lines[r.Start.Line], r.Start.Character, srv.encoding)
			}
			out = append(out, sym)
			walk(s.Children, depth+1)
		}
	}
	walk(syms, 0)
	return symbolsResult{Symbols: out}, nil
}

func (n *node) search(ctx context.Context, p navParams) (symbolsResult, error) {
	r, err := n.repoAt(p.Repo)
	if err != nil {
		return symbolsResult{}, err
	}
	q := strings.TrimSpace(p.Query)
	if q == "" {
		return symbolsResult{}, nil
	}
	// The workspace's language: the first one its marker files name whose server is installed.
	langs := workspaceLanguages(r.Path)
	if len(langs) == 0 {
		return symbolsResult{NoServer: "?"}, nil
	}
	var srv *lspServer
	for _, l := range langs {
		probe := filepath.Join(r.Path, "workspace"+l.exts[0]) // only its extension matters: it picks the server
		s, _, err := n.server(ctx, r.Path, probe)
		if errors.Is(err, errNoServer) {
			continue
		}
		if err != nil {
			return symbolsResult{}, err
		}
		srv = s
		n.openOne(ctx, r, s, l)
		break
	}
	if srv == nil {
		return symbolsResult{NoServer: langs[0].name}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var syms []lspSymbol
	if err := srv.call(cctx, "workspace/symbol", map[string]string{"query": q}, &syms); err != nil {
		return symbolsResult{}, err
	}
	cache := map[string][]string{}
	var out []symbol
	for _, s := range syms {
		if len(out) >= 100 || s.Location == nil {
			continue
		}
		loc, ok := n.toLocation(*s.Location, srv.encoding, cache)
		if !ok {
			continue
		}
		out = append(out, symbol{Name: s.Name, Kind: s.Kind, Container: s.ContainerName, Path: loc.Path, Rel: loc.Rel, Line: loc.Line, Col: loc.Col})
	}
	return symbolsResult{Symbols: out}, nil
}

// openOne tells a server about one of the repository's files of its language before a workspace search: some servers
// (TypeScript's) know no project until a file of it is open.
func (n *node) openOne(ctx context.Context, r repo, srv *lspServer, l language) {
	files, _ := n.list(ctx, r)
	for _, f := range files {
		if fl, ok := languageOf(f); !ok || fl.servers[0].bin != l.servers[0].bin || grepSkipped(f) {
			continue
		}
		real, _, _, err := n.allowed(filepath.Join(r.Path, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		if _, text, err := fileLines(real); err == nil {
			fl, _ := languageOf(real)
			_ = srv.open(real, fl.id, text)
		}
		return
	}
}

// wordAt is the identifier at col (1-based, characters) on line, for titles ("Definition of Foo").
func wordAt(line string, col int) string {
	rs := []rune(line)
	i := col - 1
	if i < 0 || i >= len(rs) {
		return ""
	}
	isWord := func(r rune) bool {
		return r == '_' || r == '$' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127
	}
	if !isWord(rs[i]) {
		return ""
	}
	a, b := i, i
	for a > 0 && isWord(rs[a-1]) {
		a--
	}
	for b < len(rs)-1 && isWord(rs[b+1]) {
		b++
	}
	return string(rs[a : b+1])
}

// language is a language server the program knows how to start.
type language struct {
	name    string      // what the page shows
	id      string      // LSP languageId
	exts    []string    // file name endings
	servers []serverCmd // server programs, first found wins
	markers []string    // files at a repository's top that make it a workspace of this language
}

// serverCmd is a server program and the arguments that make it speak over stdin and stdout.
type serverCmd struct {
	bin  string
	args []string
}

var (
	goplsCmd   = serverCmd{bin: "gopls"}
	tsCmd      = serverCmd{bin: "typescript-language-server", args: []string{"--stdio"}}
	pyrightCmd = serverCmd{bin: "pyright-langserver", args: []string{"--stdio"}}
	pylspCmd   = serverCmd{bin: "pylsp"}
	rustCmd    = serverCmd{bin: "rust-analyzer"}
	swiftCmd   = serverCmd{bin: "sourcekit-lsp"}
	clangdCmd  = serverCmd{bin: "clangd", args: []string{"--background-index=false"}}
)

// languages are found by a file's ending. Go, TypeScript and JavaScript, Python, Rust and Swift are the ones the page
// names; C and C++ start clangd when it is installed.
var languages = []language{
	{name: "go", id: "go", exts: []string{".go"}, servers: []serverCmd{goplsCmd}, markers: []string{"go.mod", "go.work"}},
	{name: "typescript", id: "typescript", exts: []string{".ts", ".tsx", ".mts", ".cts"}, servers: []serverCmd{tsCmd}, markers: []string{"tsconfig.json"}},
	{name: "javascript", id: "javascript", exts: []string{".js", ".jsx", ".mjs", ".cjs"}, servers: []serverCmd{tsCmd}, markers: []string{"package.json", "jsconfig.json"}},
	{name: "python", id: "python", exts: []string{".py", ".pyi"}, servers: []serverCmd{pyrightCmd, pylspCmd}, markers: []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "pyrightconfig.json"}},
	{name: "rust", id: "rust", exts: []string{".rs"}, servers: []serverCmd{rustCmd}, markers: []string{"Cargo.toml"}},
	{name: "swift", id: "swift", exts: []string{".swift"}, servers: []serverCmd{swiftCmd}, markers: []string{"Package.swift"}},
	{name: "c", id: "c", exts: []string{".c", ".h"}, servers: []serverCmd{clangdCmd}, markers: []string{"compile_commands.json"}},
	{name: "cpp", id: "cpp", exts: []string{".cc", ".cpp", ".cxx", ".hpp", ".hh"}, servers: []serverCmd{clangdCmd}, markers: []string{"CMakeLists.txt"}},
}

func languageOf(path string) (language, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	for _, l := range languages {
		// JSX is a language of its own to the TypeScript server
		if ext == ".tsx" && l.name == "typescript" || ext == ".jsx" && l.name == "javascript" {
			l.id += "react"
			return l, true
		}
		for _, e := range l.exts {
			if e == ext {
				return l, true
			}
		}
	}
	return language{}, false
}

func languageNamed(name string) (language, bool) {
	for _, l := range languages {
		if l.name == name {
			return l, true
		}
	}
	return language{}, false
}

// workspaceLanguages are the languages a repository's top-level marker files name, in the table's order.
func workspaceLanguages(dir string) []language {
	var out []language
	for _, l := range languages {
		for _, m := range l.markers {
			if st, err := os.Stat(filepath.Join(dir, m)); err == nil && st.Mode().IsRegular() {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

// homeDir is the user's home, from HOME or the account (the node may start the program with a clean environment).
func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return ""
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") && home != "" {
		return filepath.Join(home, p[2:])
	}
	return p
}

// programEnv is the environment language servers run in: the user's login PATH (gopls needs go on it) plus the usual
// places tools are installed, HOME, and nothing secret from the node's own environment.
func programEnv(home string) []string {
	var path []string
	seen := map[string]bool{}
	add := func(dirs ...string) {
		for _, d := range dirs {
			if d != "" && !seen[d] {
				seen[d] = true
				path = append(path, d)
			}
		}
	}
	add(filepath.SplitList(loginPath())...)
	add(filepath.SplitList(os.Getenv("PATH"))...)
	if home != "" {
		add(filepath.Join(home, "go", "bin"), filepath.Join(home, ".local", "bin"), filepath.Join(home, ".cargo", "bin"))
	}
	add("/opt/homebrew/bin", "/usr/local/go/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	env := []string{"PATH=" + strings.Join(path, string(os.PathListSeparator)), "HOME=" + home}
	for _, k := range []string{"USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR", "GOPATH", "GOROOT", "GOMODCACHE", "GOFLAGS", "GOPROXY", "GOPRIVATE", "GOTOOLCHAIN", "XDG_CACHE_HOME", "XDG_CONFIG_HOME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// loginPath asks the user's login shell for its PATH (where Homebrew and Go put themselves), or "".
func loginPath() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
		if runtime.GOOS == "darwin" {
			shell = "/bin/zsh"
		}
	}
	if _, err := os.Stat(shell); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-l", "-c", `printf '%s' "$PATH"`)
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// lookPath finds name on env's PATH.
func lookPath(name string, env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			for _, d := range filepath.SplitList(v) {
				p := filepath.Join(d, name)
				if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
					return p
				}
			}
		}
	}
	return ""
}

// goDirs are GOROOT and the module cache (links followed), which Go definitions lead to and the page may then read.
func goDirs(env []string) []string {
	goBin := lookPath("go", env)
	if goBin == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, "env", "GOROOT", "GOMODCACHE")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var dirs []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || !filepath.IsAbs(l) {
			continue
		}
		if real, err := filepath.EvalSymlinks(l); err == nil {
			dirs = append(dirs, real)
		}
	}
	return dirs
}
