package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
)

// The program on the user's chosen computer (the manifest's node section, `study node`). It reads, never writes:
//
//	study.vault_cards  the flashcards written in the vault's tagged notes (Spaced Repetition plugin syntax)
//	study.vault_notes  notes whose path or title matches a query, for making cards with the model
//	study.vault_read   one note's text
//	study.nlm_status   whether the notebooklm command is installed and signed in
//	study.nlm_list     NotebookLM notebooks, or the flashcard and quiz sets of one notebook
//	study.nlm_download one flashcard or quiz set as JSON
//
// The vault folder and the notebooklm command come from this extension's settings as the computer reads them, never
// from the hub's call, so a call cannot point the program at another folder or program.

const (
	maxNotes       = 3000 // notes looked at per scan
	maxNoteBytes   = 512 << 10
	maxScanCards   = 5000
	maxCLIOutput   = 8 << 20
	nlmTimeout     = 90 * time.Second
	nlmAuthTimeout = 30 * time.Second
)

type nodeConfig struct {
	Vault      string `json:"obsidian_vault"`
	FlashTag   string `json:"flash_tag"`
	NLMCommand string `json:"notebooklm_command"`
}

type nodeWorker struct {
	cfg  nodeConfig
	data string
	run  func(ctx context.Context, path string, args ...string) ([]byte, error)
	look func() string
}

func nodeProgram() ext.Program {
	w := &nodeWorker{run: runCLI}
	return ext.Program{ID: about.ID, Version: about.Version, Manifest: manifest,
		Init: func(_ context.Context, in *ext.Init, _ *ext.Hub) (map[string]any, error) {
			_ = in.Config(&w.cfg)
			w.data = in.Info().DataDir
			w.look = func() string { return findNLM(w.cfg.NLMCommand) }
			return nil, nil
		},
		Namespaces: map[string]ext.Handler{"study": w.rpc},
	}
}

func (w *nodeWorker) rpc(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var p struct {
		Query    string `json:"query"`
		Path     string `json:"path"`
		Notebook string `json:"notebook"`
		Artifact string `json:"artifact"`
		Kind     string `json:"kind"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &p) != nil {
		return nil, &ext.Error{Code: "invalid", Message: "invalid parameters"}
	}
	switch method {
	case "vault_cards":
		return w.vaultCards()
	case "vault_notes":
		return w.vaultNotes(p.Query)
	case "vault_read":
		return w.vaultRead(p.Path)
	case "nlm_status":
		return w.nlmStatus(ctx), nil
	case "nlm_list":
		return w.nlmList(ctx, p.Notebook)
	case "nlm_download":
		return w.nlmDownload(ctx, p.Notebook, p.Artifact, p.Kind)
	}
	return nil, ext.UnknownMethod("study." + method)
}

// ---- Obsidian vault ----

type vaultNote struct {
	Path     string     `json:"path"` // relative to the vault, with forward slashes
	Title    string     `json:"title"`
	Modified time.Time  `json:"modified"`
	Cards    []cardText `json:"cards,omitempty"`
}

type vaultScan struct {
	Notes     []vaultNote `json:"notes"`
	Scanned   int         `json:"scanned"`
	Truncated bool        `json:"truncated,omitempty"`
}

func (w *nodeWorker) root() (string, error) {
	v := strings.TrimSpace(w.cfg.Vault)
	if v == "" {
		return "", errors.New("还没在设置里填 Obsidian 库的文件夹")
	}
	if strings.HasPrefix(v, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			v = filepath.Join(home, v[2:])
		}
	}
	if !filepath.IsAbs(v) {
		return "", errors.New("Obsidian 库的文件夹要写完整路径")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(v))
	if err != nil {
		return "", errors.New("这台电脑上找不到设置里的 Obsidian 库文件夹")
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", errors.New("设置里的 Obsidian 库不是一个文件夹")
	}
	return real, nil
}

// walkVault visits the vault's Markdown notes, skipping hidden folders (.obsidian, .trash, .git) and symlinks.
func walkVault(root string, fn func(rel string, d fs.DirEntry) bool) (int, bool) {
	n, cut := 0, false
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(name), ".md") {
			return nil
		}
		if n >= maxNotes {
			cut = true
			return filepath.SkipAll
		}
		n++
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if !fn(filepath.ToSlash(rel), d) {
			return filepath.SkipAll
		}
		return nil
	})
	return n, cut
}

func readNote(path string) (string, time.Time, error) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return "", time.Time{}, errors.New("读不了这条笔记")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, errors.New("读不了这条笔记")
	}
	defer f.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(&limitReader{f: f, n: maxNoteBytes}); err != nil {
		return "", time.Time{}, errors.New("读不了这条笔记")
	}
	return b.String(), fi.ModTime(), nil
}

type limitReader struct {
	f *os.File
	n int
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, io.EOF
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	k, err := l.f.Read(p)
	l.n -= k
	return k, err
}

func noteTitle(rel string) string { return strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel)) }

func (w *nodeWorker) vaultCards() (vaultScan, error) {
	root, err := w.root()
	if err != nil {
		return vaultScan{}, err
	}
	tag := w.cfg.FlashTag
	if strings.TrimSpace(tag) == "" {
		tag = "flashcards"
	}
	out := vaultScan{Notes: []vaultNote{}}
	total := 0
	out.Scanned, out.Truncated = walkVault(root, func(rel string, _ fs.DirEntry) bool {
		text, mod, err := readNote(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || !hasTag(text, tag) {
			return true
		}
		cards := parseObsidian(text)
		if len(cards) == 0 {
			return true
		}
		if total+len(cards) > maxScanCards {
			out.Truncated = true
			return false
		}
		total += len(cards)
		out.Notes = append(out.Notes, vaultNote{Path: rel, Title: noteTitle(rel), Modified: mod.UTC(), Cards: cards})
		return true
	})
	return out, nil
}

func (w *nodeWorker) vaultNotes(query string) (vaultScan, error) {
	root, err := w.root()
	if err != nil {
		return vaultScan{}, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	out := vaultScan{Notes: []vaultNote{}}
	out.Scanned, out.Truncated = walkVault(root, func(rel string, d fs.DirEntry) bool {
		if q != "" && !strings.Contains(strings.ToLower(rel), q) {
			return true
		}
		var mod time.Time
		if fi, err := d.Info(); err == nil {
			mod = fi.ModTime().UTC()
		}
		out.Notes = append(out.Notes, vaultNote{Path: rel, Title: noteTitle(rel), Modified: mod})
		return true
	})
	sort.Slice(out.Notes, func(i, j int) bool { return out.Notes[i].Modified.After(out.Notes[j].Modified) })
	if len(out.Notes) > 50 {
		out.Notes, out.Truncated = out.Notes[:50], true
	}
	return out, nil
}

type noteText struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

func (w *nodeWorker) vaultRead(rel string) (noteText, error) {
	root, err := w.root()
	if err != nil {
		return noteText{}, err
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if rel == "" || filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." ||
		!strings.EqualFold(filepath.Ext(clean), ".md") {
		return noteText{}, errors.New("笔记路径不对")
	}
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if strings.HasPrefix(part, ".") {
			return noteText{}, errors.New("笔记路径不对")
		}
	}
	full := filepath.Join(root, clean)
	real, err := filepath.EvalSymlinks(full)
	if err != nil || real != full {
		return noteText{}, errors.New("读不了这条笔记")
	}
	text, _, err := readNote(full)
	if err != nil {
		return noteText{}, err
	}
	return noteText{Path: filepath.ToSlash(clean), Title: noteTitle(clean), Text: text}, nil
}

// ---- NotebookLM through notebooklm-py (github.com/teng-lin/notebooklm-py) ----

// nlmPaths are where uv, pipx and Homebrew put the command when it is not on the node's PATH.
var nlmPaths = []string{"~/.local/bin/notebooklm", "/opt/homebrew/bin/notebooklm", "/usr/local/bin/notebooklm", "/usr/bin/notebooklm"}

var nlmID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func findNLM(configured string) string {
	home, _ := os.UserHomeDir()
	expand := func(p string) string {
		if strings.HasPrefix(p, "~/") && home != "" {
			return filepath.Join(home, p[2:])
		}
		return p
	}
	exe := func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
	}
	if c := strings.TrimSpace(configured); c != "" {
		if c = expand(c); filepath.IsAbs(c) && filepath.Base(c) == "notebooklm" && exe(c) {
			return c
		}
		return ""
	}
	if p, err := exec.LookPath("notebooklm"); err == nil {
		return p
	}
	for _, p := range nlmPaths {
		if p = expand(p); exe(p) {
			return p
		}
	}
	return ""
}

type nlmState struct {
	Installed bool   `json:"installed"`
	SignedIn  bool   `json:"signed_in"`
	Message   string `json:"message,omitempty"`
}

func (w *nodeWorker) nlmStatus(ctx context.Context) nlmState {
	path := w.look()
	if path == "" {
		return nlmState{Message: "这台电脑上没找到 notebooklm 命令"}
	}
	ctx, cancel := context.WithTimeout(ctx, nlmAuthTimeout)
	defer cancel()
	if _, err := w.run(ctx, path, "--quiet", "auth", "check", "--test", "--json"); err != nil {
		return nlmState{Installed: true, Message: "notebooklm 还没登录或登录过期了，在这台电脑上运行 notebooklm login"}
	}
	return nlmState{Installed: true, SignedIn: true}
}

type nlmItem struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Kind    string `json:"kind,omitempty"` // flashcards or quiz, for a notebook's sets
	Created string `json:"created,omitempty"`
}

func (w *nodeWorker) nlm(ctx context.Context, args ...string) ([]byte, error) {
	path := w.look()
	if path == "" {
		return nil, &ext.Error{Code: "missing", Message: "这台电脑上没找到 notebooklm 命令"}
	}
	ctx, cancel := context.WithTimeout(ctx, nlmTimeout)
	defer cancel()
	return w.run(ctx, path, append([]string{"--quiet"}, args...)...)
}

func (w *nodeWorker) nlmList(ctx context.Context, notebook string) ([]nlmItem, error) {
	if notebook == "" {
		out, err := w.nlm(ctx, "list", "--json")
		if err != nil {
			return nil, err
		}
		var v struct {
			Notebooks []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"notebooks"`
		}
		if json.Unmarshal(lastJSON(out), &v) != nil {
			return nil, errors.New("notebooklm list 的输出认不出来")
		}
		items := []nlmItem{}
		for _, n := range v.Notebooks {
			if nlmID.MatchString(n.ID) {
				items = append(items, nlmItem{ID: n.ID, Title: clip(n.Title, 200)})
			}
		}
		return items, nil
	}
	if !nlmID.MatchString(notebook) {
		return nil, errors.New("NotebookLM 笔记本 ID 不对")
	}
	items := []nlmItem{}
	for _, kind := range []string{"flashcards", "quiz"} {
		typ := map[string]string{"flashcards": "flashcard", "quiz": "quiz"}[kind]
		out, err := w.nlm(ctx, "artifact", "list", "-n", notebook, "--type", typ, "--json")
		if err != nil {
			return nil, err
		}
		var v struct {
			Artifacts []struct {
				ID      string  `json:"id"`
				Title   string  `json:"title"`
				Status  string  `json:"status"`
				Created *string `json:"created_at"`
			} `json:"artifacts"`
		}
		if json.Unmarshal(lastJSON(out), &v) != nil {
			return nil, errors.New("notebooklm artifact list 的输出认不出来")
		}
		for _, a := range v.Artifacts {
			if !nlmID.MatchString(a.ID) || (a.Status != "" && !strings.EqualFold(a.Status, "completed")) {
				continue
			}
			it := nlmItem{ID: a.ID, Title: clip(a.Title, 200), Kind: kind}
			if a.Created != nil {
				it.Created = *a.Created
			}
			items = append(items, it)
		}
	}
	return items, nil
}

type nlmSet struct {
	Kind  string     `json:"kind"`
	Title string     `json:"title"`
	Cards []cardText `json:"cards"`
}

func (w *nodeWorker) nlmDownload(ctx context.Context, notebook, artifact, kind string) (nlmSet, error) {
	if !nlmID.MatchString(notebook) || !nlmID.MatchString(artifact) {
		return nlmSet{}, errors.New("NotebookLM ID 不对")
	}
	if kind != "flashcards" && kind != "quiz" {
		return nlmSet{}, errors.New("只能读闪卡或测验")
	}
	base := w.data
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "nlm-")
	if err != nil {
		return nlmSet{}, err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, kind+".json")
	if _, err := w.nlm(ctx, "download", kind, "-n", notebook, "-a", artifact, "--format", "json", file); err != nil {
		return nlmSet{}, err
	}
	raw, err := os.ReadFile(file)
	if err != nil || len(raw) > maxCLIOutput {
		return nlmSet{}, errors.New("notebooklm 没有写出这份" + map[string]string{"flashcards": "闪卡", "quiz": "测验"}[kind])
	}
	title, cards, err := parseNLMArtifact(kind, raw)
	if err != nil {
		return nlmSet{}, err
	}
	if len(cards) > 500 {
		cards = cards[:500]
	}
	return nlmSet{Kind: kind, Title: clip(title, 200), Cards: cards}, nil
}

// lastJSON is the JSON object the command printed, skipping any line of other output before it.
func lastJSON(out []byte) []byte {
	if i := bytes.IndexByte(out, '{'); i >= 0 {
		return out[i:]
	}
	return out
}

// runCLI runs a command with the node's environment plus HOME (the command keeps its sign-in under it), and gives
// what it wrote to stdout. What it wrote to stderr is not passed on: it can hold account details.
func runCLI(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	env := os.Environ()
	if os.Getenv("HOME") == "" {
		if u, err := user.Current(); err == nil && u.HomeDir != "" {
			env = append(env, "HOME="+u.HomeDir)
		}
	}
	cmd.Env = append(env, "NO_COLOR=1", "PYTHONIOENCODING=utf-8")
	var stdout capped
	cmd.Stdout, cmd.Stderr = &stdout, nil
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("notebooklm 太久没回应")
		}
		return nil, errors.New("notebooklm 没能完成，可能需要在这台电脑上重新运行 notebooklm login")
	}
	if stdout.over {
		return nil, errors.New("notebooklm 的输出太大")
	}
	return stdout.Bytes(), nil
}

type capped struct {
	bytes.Buffer
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.Len()+len(p) > maxCLIOutput {
		c.over = true
		return len(p), nil
	}
	return c.Buffer.Write(p)
}
