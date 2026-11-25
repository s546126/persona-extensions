package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
)

// The program on a computer (the manifest's node section): while it runs, the hook's entry is in each configured
// Claude config directory, its options are written and its heartbeat is fresh; when it stops (switched off for this
// computer, extension disabled, upgraded, uninstalled, the hub gone past its lease) it takes the entries out again.
// If it is killed before it can, the heartbeat goes stale and the hook does nothing (hook.go).

// config is the extension's settings, as the hub sends them.
type config struct {
	Machines       []string `json:"machines"`
	Mode           string   `json:"mode"`
	JudgeThreshold int      `json:"judge_threshold"`
	Stopwords      []string `json:"stopwords"`
	GiveupWords    []string `json:"giveup_words"`
	LLMCommand     []string `json:"llm_command"`
	ConfigDirs     []string `json:"config_dirs"`
}

func (c config) dirs() []string {
	if len(c.ConfigDirs) == 0 {
		return []string{"~/.claude"}
	}
	return c.ConfigDirs
}

func (c config) options() options {
	return options{Mode: c.Mode, JudgeThreshold: c.JudgeThreshold, Stopwords: c.Stopwords, GiveupWords: c.GiveupWords,
		LLMCommand: c.LLMCommand}.withDefaults()
}

type node struct {
	bin  string // this program, as the entries run it
	data string // its data directory: heartbeat, options and the store

	mu   sync.Mutex
	mode string
	dirs []dirState

	// beatWG is the heartbeat ticker goroutine; cleanup joins it before it removes the heartbeat, so a late beat
	// cannot bring the heartbeat back after the entries are gone.
	beatWG sync.WaitGroup
}

// dirState is one config directory: where its settings are, and whether the entry is in.
type dirState struct {
	Dir       string `json:"dir"`
	Settings  string `json:"settings,omitempty"`
	Installed bool   `json:"installed"`
	Error     string `json:"error,omitempty"`
}

// status is what ebd.status answers: the directories, the counts, and the newest pairs.
type status struct {
	Version string         `json:"version"`
	Mode    string         `json:"mode"`
	Dirs    []dirState     `json:"dirs"`
	Total   int            `json:"total"`
	ByMode  map[string]int `json:"by_mode"`
	Due     int            `json:"due"`
	Recent  []entry        `json:"recent"`
}

const recentShown = 30

func (n *node) program(id, version string) ext.Program {
	return ext.Program{
		ID: id, Version: version, Manifest: manifest,
		Init: n.init,
		Namespaces: map[string]ext.Handler{"ebd": func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
			if method != "status" {
				return nil, ext.UnknownMethod("ebd." + method)
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			return n.status(version, time.Now())
		}},
	}
}

func (n *node) status(version string, now time.Time) (status, error) {
	st := status{Version: version, Mode: n.mode, Dirs: n.check(), ByMode: map[string]int{}}
	es, err := store{dir: n.data}.entries()
	if err != nil {
		return st, err
	}
	st.Total = len(es)
	for _, e := range es {
		st.ByMode[e.Mode]++
		if t, err := time.Parse(time.RFC3339, e.NextReview); err == nil && !t.After(now) {
			st.Due++
		}
	}
	for i := len(es) - 1; i >= 0 && len(st.Recent) < recentShown; i-- {
		st.Recent = append(st.Recent, es[i])
	}
	return st, nil
}

func (n *node) init(ctx context.Context, in *ext.Init, _ *ext.Hub) (map[string]any, error) {
	var c config
	if err := in.Config(&c); err != nil {
		return nil, err
	}
	n.data = in.Info().DataDir
	if n.data == "" {
		n.data = ext.Env().Data
	}
	if n.data == "" {
		return nil, fmt.Errorf("no data directory")
	}
	if err := os.MkdirAll(n.data, 0o700); err != nil {
		return nil, err
	}
	if n.bin == "" {
		bin, err := os.Executable()
		if err != nil {
			return nil, err
		}
		if real, err := filepath.EvalSymlinks(bin); err == nil {
			bin = real
		}
		n.bin = bin
	}
	opts := c.options()
	b, err := json.Marshal(opts)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(n.data, optionsFile), b); err != nil {
		return nil, err // the hook must not work by an old setting
	}
	n.beat()
	n.mu.Lock()
	n.mode = opts.Mode
	n.install(c.dirs())
	n.mu.Unlock()
	n.beatWG.Add(1)
	go func() {
		defer n.beatWG.Done()
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n.beat()
			}
		}
	}()
	return nil, nil
}

// beat touches the heartbeat.
func (n *node) beat() {
	p := filepath.Join(n.data, heartbeatFile)
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		_ = os.WriteFile(p, nil, 0o600)
	}
}

// install puts the entry into each directory that has Claude Code's config.
func (n *node) install(dirs []string) {
	n.dirs = n.dirs[:0]
	for _, d := range dirs {
		s := dirState{Dir: d}
		abs, err := expand(d)
		switch {
		case err != nil:
			s.Error = err.Error()
		case !isDir(abs):
			s.Error = "no such directory"
		default:
			s.Settings = settingsPath(abs)
			if _, err := installHook(s.Settings, hookCommand(n.bin, n.data)); err != nil {
				s.Error = err.Error()
			} else {
				s.Installed = true
			}
		}
		if s.Error != "" {
			fmt.Fprintf(os.Stderr, "%s: %s: %s\n", extensionID, d, s.Error)
		}
		n.dirs = append(n.dirs, s)
	}
}

// check reads again whether each entry is still in (someone may have edited the file).
func (n *node) check() []dirState {
	out := make([]dirState, len(n.dirs))
	for i, s := range n.dirs {
		if s.Settings != "" && s.Error == "" {
			s.Installed = hasHook(s.Settings)
		}
		out[i] = s
	}
	return out
}

// cleanup takes the entries out and stops the heartbeat; the program is about to exit.
func (n *node) cleanup() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.beatWG.Wait()
	if n.data != "" {
		_ = os.Remove(filepath.Join(n.data, heartbeatFile))
	}
	for _, s := range n.dirs {
		if s.Settings == "" {
			continue
		}
		if _, err := uninstallHook(s.Settings); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %s: %v\n", extensionID, s.Dir, err)
		}
	}
}

// expand turns ~ into the home directory; the result must be absolute.
func expand(d string) (string, error) {
	d = strings.TrimSpace(d)
	if d == "~" || strings.HasPrefix(d, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		d = filepath.Join(home, strings.TrimPrefix(d, "~"))
	}
	if !filepath.IsAbs(d) {
		return "", fmt.Errorf("not an absolute path")
	}
	return filepath.Clean(d), nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
