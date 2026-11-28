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

	mu            sync.Mutex
	dirs          []dirState
	configured    []string
	agyConfigured []string
	profiles      string // Persona's assistant channel profiles, beside node-plugins

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
	auto         bool // discovered, not an explicit config_dir: revalidate its final component before cleanup
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
	n.configured = c.dirs()
	n.agyConfigured = append([]string(nil), c.AgyDirs...)
	// <persona home>/node-plugins/<storage>/data identifies the same Persona
	// home whose profiles/<launcher> directories the assistant selects. Do not
	// infer it from HOME: PERSONA_HOME may point somewhere else.
	if plugins := filepath.Dir(filepath.Dir(n.data)); filepath.Base(n.data) == "data" && filepath.Base(plugins) == "node-plugins" {
		n.profiles = filepath.Join(filepath.Dir(plugins), "profiles")
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
	n.installAgy(n.agyConfigured)
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
				n.refresh()
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

// refresh is the heartbeat tick: renew freshness, then install newly created
// profiles and replace stale entries with this executable's current command.
func (n *node) refresh() {
	n.beat()
	n.mu.Lock()
	defer n.mu.Unlock()
	n.install(nil)
	n.installAgy(n.agyConfigured)
}

// install puts the entry into each explicitly configured directory and each
// existing assistant channel profile. A nil dirs reconciles the initial
// configuration again. Discovery never creates a profile and does not follow a
// final-component symlink out of Persona's profiles directory.
func (n *node) install(dirs []string) {
	if dirs == nil {
		dirs = n.configured
	}
	configured := append([]string(nil), dirs...)
	discovered, discoveryErr := n.assistantProfiles()
	var retained []dirState
	if discoveryErr != nil {
		for _, s := range n.dirs {
			if s.auto {
				retained = append(retained, s)
			}
		}
	}
	explicit := map[string]bool{}
	for _, d := range configured {
		if abs, err := expand(d); err == nil {
			explicit[abs] = true
		}
	}
	automatic := map[string]bool{}
	for _, d := range discovered {
		if abs, err := expand(d); err == nil {
			automatic[abs] = true
		}
	}
	dirs = append(configured, discovered...)
	n.dirs = n.dirs[:0]
	seen := map[string]bool{}
	for _, d := range dirs {
		s := dirState{Dir: d}
		abs, err := expand(d)
		if err == nil {
			if seen[abs] {
				continue
			}
			seen[abs] = true
			s.auto = automatic[abs] && !explicit[abs]
		}
		switch {
		case err != nil:
			s.Error = sanitizeDirError(err)
		case !isDir(abs):
			s.Error = "no such directory"
		default:
			if s.auto {
				s.settingsPath, err = n.automaticSettingsPath(abs)
				if err != nil {
					s.Error = sanitizeDirError(err)
					break
				}
			} else {
				s.settingsPath = settingsPath(abs)
			}
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
	// A transient discovery failure must not erase the cleanup paths for
	// automatic profiles that were already installed.
	n.dirs = append(n.dirs, retained...)
}

// assistantProfiles lists the current channel directories. ReadDir sorts them,
// so status and upgrade order stay stable. A profiles directory that does not
// exist yet is normal for a node whose assistant has not started.
func (n *node) assistantProfiles() ([]string, error) {
	if n.profiles == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(n.profiles)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(n.profiles, entry.Name()))
		}
	}
	return dirs, nil
}

// realAssistantProfile revalidates an automatically discovered path at cleanup:
// it is still the same immediate child kind, not a symlink substituted after the
// last refresh. Lstat never follows that final component.
func (n *node) realAssistantProfile(dir string) bool {
	if n.profiles == "" || filepath.Dir(dir) != n.profiles {
		return false
	}
	st, err := os.Lstat(dir)
	return err == nil && st.IsDir() && st.Mode()&os.ModeSymlink == 0
}

// automaticSettingsPath keeps automatically discovered profiles from editing a
// settings.json symlink outside their real profile directory. Explicit
// config_dirs retain settingsPath's symlink-following behavior.
func (n *node) automaticSettingsPath(dir string) (string, error) {
	if !n.realAssistantProfile(dir) {
		return "", fmt.Errorf("not a real assistant profile")
	}
	path := filepath.Join(dir, "settings.json")
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("automatic assistant profile settings.json is a symlink")
	}
	return path, nil
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
// then, so the run context is cancelled: join the ticker goroutine before taking the node lock (a due refresh may be
// waiting for that lock), then remove the heartbeat and entries. No beat or refresh can afterwards resurrect them.
func (n *node) cleanup() {
	n.beatWG.Wait()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.data != "" {
		_ = os.Remove(filepath.Join(n.data, heartbeatFile))
	}
	// A refresh while a configured directory is temporarily absent leaves an
	// error state without its internal cleanup path (and can drop the default
	// AGY state entirely). Rebuild only configured paths that are accessible
	// again; automatic profiles keep their stricter revalidation below.
	type cleanupKey struct {
		dir string
		agy bool
	}
	configuredStates := map[cleanupKey]int{}
	for i, s := range n.dirs {
		if abs, err := expand(s.Dir); err == nil {
			configuredStates[cleanupKey{dir: abs, agy: s.agy}] = i
		}
	}
	backfill := func(d string, agy bool) {
		abs, err := expand(d)
		if err != nil || !isDir(abs) {
			return
		}
		settings := settingsPath(abs)
		if agy {
			settings = agyHooksPath(abs)
		}
		key := cleanupKey{dir: abs, agy: agy}
		if i, ok := configuredStates[key]; ok {
			if n.dirs[i].settingsPath == "" {
				n.dirs[i].settingsPath = settings
				n.dirs[i].Settings = foldDisplayPath(abs, settings)
			}
			return
		}
		s := dirState{Dir: d, settingsPath: settings, agy: agy}
		s.Settings = foldDisplayPath(abs, settings)
		n.dirs = append(n.dirs, s)
		configuredStates[key] = len(n.dirs) - 1
	}
	for _, d := range n.configured {
		backfill(d, false)
	}
	agyDirs := n.agyConfigured
	if len(agyDirs) == 0 {
		agyDirs = []string{"~/.gemini"}
	}
	for _, d := range agyDirs {
		backfill(d, true)
	}
	// A new profile may have been seeded by Persona after the last heartbeat.
	// Merge current profiles with old states: a previously missing explicit
	// directory has no settings path yet and must not suppress the cleanup.
	seen := map[string]int{}
	for i, s := range n.dirs {
		// AGY and Claude can share a directory but have independent hooks.
		if s.agy {
			continue
		}
		if abs, err := expand(s.Dir); err == nil {
			seen[abs] = i
		}
	}
	// Discovery is best-effort during cleanup; retained automatic states are
	// revalidated by automaticSettingsPath before anything is uninstalled.
	discovered, _ := n.assistantProfiles()
	for _, d := range discovered {
		abs, err := expand(d)
		if err != nil {
			continue
		}
		if i, ok := seen[abs]; ok {
			if n.dirs[i].settingsPath == "" {
				if n.dirs[i].auto {
					n.dirs[i].settingsPath = filepath.Join(abs, "settings.json")
				} else {
					n.dirs[i].settingsPath = settingsPath(abs)
				}
				n.dirs[i].Settings = foldDisplayPath(abs, n.dirs[i].settingsPath)
			}
			continue
		}
		s := dirState{Dir: d, settingsPath: filepath.Join(abs, "settings.json"), auto: true}
		s.Settings = foldDisplayPath(abs, s.settingsPath)
		n.dirs = append(n.dirs, s)
	}
	for _, s := range n.dirs {
		// Only automatic discovery has the no-symlink-escape rule. An explicit
		// config_dir keeps the extension's existing settings-symlink semantics.
		if s.auto {
			settings, err := n.automaticSettingsPath(s.Dir)
			if err != nil {
				continue
			}
			s.settingsPath = settings
		}
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
