package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
)

// The program on a computer (the manifest's node section): while it runs, the hook's entry is in each configured
// Claude config directory and its heartbeat is fresh; when it stops (switched off for this computer, extension
// disabled, upgraded, uninstalled, the hub gone past its lease) it takes the entries out again. If it is killed
// before it can, the heartbeat goes stale and the hook allows nothing (hook.go).

// config is the extension's settings, as the hub sends them.
type config struct {
	Machines         []string `json:"machines"`
	ConfigDirs       []string `json:"config_dirs"`
	AgyDirs          []string `json:"agy_dirs"`
	AllowDestructive bool     `json:"allow_destructive"`
	AssistantWrites  bool     `json:"assistant_writes"`
	AssistantDirs    []string `json:"assistant_write_dirs"`
}

func (c config) dirs() []string {
	if len(c.ConfigDirs) == 0 {
		return []string{"~/.claude"}
	}
	return c.ConfigDirs
}

type node struct {
	bin        string // this program, as the entries run it
	data       string // its data directory: heartbeat and audit log
	matcher    string // the Claude entries' matcher (matcherFor)
	agyMatcher string // the Antigravity entries' matcher (agyMatcherFor)

	mu   sync.Mutex
	dirs []dirState

	// beatWG is the heartbeat ticker goroutine. cleanup joins it before it removes the heartbeat, so a beat
	// already due cannot land during cleanup and recreate the file (a fresh heartbeat with the entry still in
	// would keep answering allow after this program exited).
	beatWG sync.WaitGroup
}

// dirState is one config directory: its folded display settings path, whether the entry is in, and any controlled error summary.
type dirState struct {
	Dir          string `json:"dir"`
	Settings     string `json:"settings,omitempty"`
	Installed    bool   `json:"installed"`
	Error        string `json:"error,omitempty"`
	settingsPath string
	agy          bool
}

// status is what bypass.status answers: the directories and the newest auto-allows.
type status struct {
	Version string       `json:"version"`
	Dirs    []dirState   `json:"dirs"`
	Recent  []auditEntry `json:"recent"`
}

const recentShown = 30

var (
	errHomeUnavailable = errors.New("cannot determine home directory")
	errNotAbsPath      = errors.New("not an absolute path")
)

func (n *node) program(id, version string) ext.Program {
	return ext.Program{
		ID: id, Version: version, Manifest: manifest,
		Init: n.init,
		Namespaces: map[string]ext.Handler{"bypass": func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
			if method != "status" {
				return nil, ext.UnknownMethod("bypass." + method)
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			return status{Version: version, Dirs: n.check(), Recent: minimizeAudit(readAudit(n.data, recentShown))}, nil
		}},
	}
}

func (n *node) init(ctx context.Context, in *ext.Init, _ *ext.Hub) (map[string]any, error) {
	var c config
	if err := in.Config(&c); err != nil {
		return nil, errors.New("invalid configuration")
	}
	n.data = in.Info().DataDir
	if n.data == "" {
		n.data = ext.Env().Data
	}
	if n.data == "" {
		return nil, errors.New("no data directory")
	}
	if err := os.MkdirAll(n.data, 0o700); err != nil {
		return nil, sanitizeInitError("data directory unavailable", err)
	}
	if err := os.Chmod(n.data, 0o700); err != nil {
		return nil, sanitizeInitError("data directory unavailable", err)
	}
	if n.bin == "" {
		bin, err := os.Executable()
		if err != nil {
			return nil, sanitizeInitError("extension binary unavailable", err)
		}
		if real, err := filepath.EvalSymlinks(bin); err == nil {
			bin = real
		}
		n.bin = bin
	}
	n.matcher = matcherFor(c.AssistantWrites)
	n.agyMatcher = agyMatcherFor(c.AssistantWrites)
	opts, err := json.Marshal(options{AllowDestructive: c.AllowDestructive, AssistantWrites: c.AssistantWrites, AssistantWriteDirs: c.AssistantDirs})
	if err != nil {
		return nil, errors.New("cannot encode options")
	}
	if err := writeFileAtomic(filepath.Join(n.data, optionsFile), opts); err != nil {
		return nil, sanitizeInitError("cannot save options", err) // the hook must not read an old setting
	}
	n.beat()
	n.mu.Lock()
	n.install(c.dirs())
	n.installAgy(c.AgyDirs)
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
			s.Error = sanitizeDirError(err)
		case !isDir(abs):
			s.Error = "no such directory"
		default:
			s.settingsPath = settingsPath(abs)
			s.Settings = foldDisplayPath(abs, s.settingsPath)
			if _, err := installHookWith(s.settingsPath, cmp.Or(n.matcher, hookMatcher), hookCommand(n.bin, abs, n.data)); err != nil {
				s.Error = sanitizeDirError(err)
			} else {
				s.Installed = true
			}
		}
		if s.Error != "" {
			fmt.Fprintf(os.Stderr, "%s: %s: %s\n", currentExtensionID, d, s.Error)
		}
		n.dirs = append(n.dirs, s)
	}
}

// installAgy puts the PreToolUse entry into each configured Gemini/Antigravity config directory. When agy_dirs is
// left at the default ~/.gemini and that directory does not exist on the machine, it is skipped silently so computers
// that only have Claude Code keep a clean status list.
func (n *node) installAgy(agyDirs []string) {
	defaultOnly := len(agyDirs) == 0 || (len(agyDirs) == 1 && strings.TrimSpace(agyDirs[0]) == "~/.gemini")
	if len(agyDirs) == 0 {
		agyDirs = []string{"~/.gemini"}
	}
	for _, d := range agyDirs {
		s := dirState{Dir: d, agy: true}
		abs, err := expand(d)
		switch {
		case err != nil:
			s.Error = sanitizeDirError(err)
		case !isDir(abs):
			if defaultOnly {
				continue
			}
			s.Error = "no such directory"
		default:
			s.settingsPath = agyHooksPath(abs)
			s.Settings = foldDisplayPath(abs, s.settingsPath)
			if _, err := installAgyHookWith(s.settingsPath, cmp.Or(n.agyMatcher, agyHookMatcher), agyHookCommand(n.bin, abs, n.data)); err != nil {
				s.Error = sanitizeDirError(err)
			} else {
				s.Installed = true
			}
		}
		if s.Error != "" {
			fmt.Fprintf(os.Stderr, "%s: %s: %s\n", currentExtensionID, d, s.Error)
		}
		n.dirs = append(n.dirs, s)
	}
}

// check reads again whether each entry is still in (someone may have edited the file).
func (n *node) check() []dirState {
	out := make([]dirState, len(n.dirs))
	for i, s := range n.dirs {
		if s.settingsPath != "" && s.Error == "" {
			if s.agy {
				s.Installed = hasAgyHook(s.settingsPath)
			} else {
				s.Installed = hasHook(s.settingsPath)
			}
		}
		out[i] = s
	}
	return out
}

// cleanup takes the entries out and stops the heartbeat; the program is about to exit. ext.Main has returned by
// then, so the run context is cancelled: join the ticker goroutine (under the node lock, which a beat never
// takes) before removing the heartbeat, and no beat can afterwards resurrect it.
func (n *node) cleanup() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.beatWG.Wait()
	if n.data != "" {
		_ = os.Remove(filepath.Join(n.data, heartbeatFile))
	}
	for _, s := range n.dirs {
		if s.settingsPath == "" {
			continue
		}
		var err error
		if s.agy {
			_, err = uninstallAgyHook(s.settingsPath)
		} else {
			_, err = uninstallHook(s.settingsPath)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %s: %s\n", currentExtensionID, s.Dir, sanitizeDirError(err))
		}
	}
}

// expand turns ~ into the home directory; the result must be absolute.
func expand(d string) (string, error) {
	d = strings.TrimSpace(d)
	if d == "~" || strings.HasPrefix(d, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errHomeUnavailable
		}
		d = filepath.Join(home, strings.TrimPrefix(d, "~"))
	}
	if !filepath.IsAbs(d) {
		return "", errNotAbsPath
	}
	return filepath.Clean(d), nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// writeFileAtomic replaces path with b (mode 0600) so that a reader sees the old file or the new one.
func writeFileAtomic(path string, b []byte) error {
	return writeFileAtomicMode(path, b, 0o600, ".options-*")
}

func sanitizeInitError(summary string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return errors.New(summary + ": permission denied")
	}
	return errors.New(summary)
}

func sanitizeDirError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errNotAbsPath):
		return errNotAbsPath.Error()
	case errors.Is(err, errHomeUnavailable):
		return errHomeUnavailable.Error()
	case errors.Is(err, errSettingsJSON):
		return errSettingsJSON.Error()
	case errors.Is(err, errSettingsFormat):
		return errSettingsFormat.Error()
	case errors.Is(err, errSettingsRead):
		if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
			return errSettingsRead.Error() + ": permission denied"
		}
		return errSettingsRead.Error()
	case errors.Is(err, errSettingsBackup):
		if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
			return errSettingsBackup.Error() + ": permission denied"
		}
		return errSettingsBackup.Error()
	case errors.Is(err, errSettingsWrite):
		if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
			return errSettingsWrite.Error() + ": permission denied"
		}
		return errSettingsWrite.Error()
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	case errors.Is(err, os.ErrNotExist):
		return "no such directory"
	default:
		return "cannot update settings file"
	}
}

// foldDisplayPath turns a resolved host settings path into a safe display summary without leaking full host paths:
// ~/... when under the user's home directory, a relative path inside dir, or the file's basename.
func foldDisplayPath(dir, resolved string) string {
	if resolved == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		homeClean := filepath.Clean(home)
		if rel, ok := relWithin(homeClean, resolved); ok {
			return "~/" + filepath.ToSlash(rel)
		}
		if realHome, err := filepath.EvalSymlinks(homeClean); err == nil && realHome != homeClean {
			if rel, ok := relWithin(realHome, resolved); ok {
				return "~/" + filepath.ToSlash(rel)
			}
		}
	}
	if dir != "" {
		if rel, ok := relWithin(dir, resolved); ok {
			return filepath.ToSlash(rel)
		}
		if realDir, err := filepath.EvalSymlinks(dir); err == nil && realDir != dir {
			if rel, ok := relWithin(realDir, resolved); ok {
				return filepath.ToSlash(rel)
			}
		}
	}
	return filepath.Base(resolved)
}

func relWithin(base, target string) (string, bool) {
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// minimizeAudit strips unused full host paths from audit entries sent over bypass.status RPC.
func minimizeAudit(entries []auditEntry) []auditEntry {
	out := make([]auditEntry, len(entries))
	for i, e := range entries {
		e.ConfigDir = ""
		if e.Cwd != "" {
			e.Cwd = filepath.Base(e.Cwd)
		}
		out[i] = e
	}
	return out
}
