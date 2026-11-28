package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Node-side probing and runtime configuration: checks configured code root directories (existence, readability,
// sandbox boundaries, repository counts), discovers candidate folders on the machine, probes each managed language
// server's binary and version, and applies per-machine runtime overrides safely.

type rootProbe struct {
	Input    string `json:"input"`
	Resolved string `json:"resolved,omitempty"`
	Status   string `json:"status"` // "ok", "missing", "permission_denied", "forbidden", "not_dir"
	Repos    int    `json:"repos"`
	Error    string `json:"error,omitempty"`
}

type rootSuggestion struct {
	Path  string `json:"path"`
	Repos int    `json:"repos"`
}

type lspProbe struct {
	Lang       string   `json:"lang"`
	Enabled    bool     `json:"enabled"`
	Status     string   `json:"status"` // "ready", "missing", "disabled", "invalid_path"
	Binary     string   `json:"binary,omitempty"`
	CustomPath string   `json:"custom_path,omitempty"`
	Version    string   `json:"version,omitempty"`
	Running    int      `json:"running,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type probeResult struct {
	Home        string           `json:"home,omitempty"`
	OS          string           `json:"os,omitempty"`
	IdleMinutes int              `json:"idle_minutes"`
	TotalRepos  int              `json:"total_repos"`
	Roots       []rootProbe      `json:"roots"`
	Suggestions []rootSuggestion `json:"suggestions,omitempty"`
	Languages   []lspProbe       `json:"languages"`
	ProbedAt    string           `json:"probed_at,omitempty"`
}

type versionCacheEntry struct {
	modTime time.Time
	size    int64
	at      time.Time
	version string
}

const (
	versionCacheTTL = 5 * time.Minute
	versionTimeout  = 2 * time.Second
)

// applyOverride updates the node's runtime override if it differs from the current one.
// Any language that was disabled or whose effective server path changed has its running servers stopped.
func (n *node) applyOverride(ov *nodeOverride) {
	if ov == nil {
		return
	}
	var toStop []*lspServer

	n.mu.Lock()
	if overridesEqual(n.override, ov) {
		n.mu.Unlock()
		return
	}
	oldRoots := slices.Clone(n.effectiveRootsLocked())
	oldBins := make(map[string]string, len(managedLanguages))
	for _, langName := range managedLanguages {
		if l, ok := languageNamed(langName); ok {
			if cmd, ok := n.serverLocked(l); ok {
				oldBins[langName] = cmd.bin
			}
		}
	}

	n.override = cloneNodeOverride(ov)
	newRoots := n.effectiveRootsLocked()
	if !slices.Equal(oldRoots, newRoots) {
		n.repos = nil
		n.reposAt = time.Time{}
		n.lists = map[string]fileList{}
	} else {
		// Refresh found LSP list on next reposResult.
		n.reposAt = time.Time{}
	}

	// Stop running servers for any language whose enabled state or binary path changed.
	for _, langName := range managedLanguages {
		l, ok := languageNamed(langName)
		if !ok {
			continue
		}
		newBin := ""
		if cmd, ok := n.serverLocked(l); ok {
			newBin = cmd.bin
		}
		if oldBins[langName] != newBin {
			for key, slot := range n.servers {
				prefix, _, _ := strings.Cut(key, "\x00")
				if canonicalLang(prefix) != langName {
					continue
				}
				if slot.mu.TryLock() {
					if slot.srv != nil {
						toStop = append(toStop, slot.srv)
						slot.srv = nil
					}
					slot.err = nil
					slot.mu.Unlock()
				}
			}
		}
	}
	n.mu.Unlock()

	for _, srv := range toStop {
		srv.stop()
	}
}

func overridesEqual(a, b *nodeOverride) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.RootsSet != b.RootsSet || a.IdleMinutes != b.IdleMinutes || !slices.Equal(a.Roots, b.Roots) {
		return false
	}
	if len(a.Languages) != len(b.Languages) {
		return false
	}
	for k, va := range a.Languages {
		if vb, ok := b.Languages[k]; !ok || va != vb {
			return false
		}
	}
	return true
}

func (n *node) effectiveRootsLocked() []string {
	if n.override != nil && n.override.RootsSet {
		return n.override.Roots
	}
	return n.cfg.Roots
}

func (n *node) effectiveIdleLocked() time.Duration {
	if n.override != nil && n.override.IdleMinutes > 0 {
		return time.Duration(min(n.override.IdleMinutes, 24*60)) * time.Minute
	}
	return n.cfg.idle()
}

func (n *node) effectiveIdleMinutesLocked() int {
	if n.override != nil && n.override.IdleMinutes > 0 {
		return min(n.override.IdleMinutes, 24*60)
	}
	if n.cfg.IdleMinutes > 0 {
		return min(n.cfg.IdleMinutes, 24*60)
	}
	return int(idleStop / time.Minute)
}

func (n *node) langOverrideLocked(langName string) (disabled bool, customPath string) {
	canon := canonicalLang(langName)
	if n.override != nil {
		if ov, ok := n.override.Languages[canon]; ok {
			if ov.Disabled {
				return true, strings.TrimSpace(ov.Path)
			}
			if p := strings.TrimSpace(ov.Path); p != "" {
				return false, p
			}
		}
	}
	return false, strings.TrimSpace(n.cfg.customServer(canon))
}

// probe inspects configured roots, candidate suggestions, and all managed language servers.
func (n *node) probe(ctx context.Context, force bool) probeResult {
	n.mu.Lock()
	home := n.home
	env := slices.Clone(n.env)
	roots := slices.Clone(n.effectiveRootsLocked())
	idleMins := n.effectiveIdleMinutesLocked()

	runningCounts := map[string]int{}
	for key, slot := range n.servers {
		langName, _, _ := strings.Cut(key, "\x00")
		if slot.mu.TryLock() {
			if slot.srv != nil && slot.srv.alive() {
				runningCounts[canonicalLang(langName)]++
			}
			slot.mu.Unlock()
		}
	}

	type langInput struct {
		lang       language
		disabled   bool
		customPath string
		autoBin    string
		effective  serverCmd
		hasEff     bool
	}
	var langInputs []langInput
	for _, name := range managedLanguages {
		l, ok := languageNamed(name)
		if !ok {
			continue
		}
		disabled, custom := n.langOverrideLocked(name)
		autoBin := ""
		for _, s := range l.servers {
			if p := lookPath(s.bin, env); p != "" {
				autoBin = p
				break
			}
		}
		eff, hasEff := n.serverLocked(l)
		langInputs = append(langInputs, langInput{
			lang:       l,
			disabled:   disabled,
			customPath: custom,
			autoBin:    autoBin,
			effective:  eff,
			hasEff:     hasEff,
		})
	}
	n.mu.Unlock()

	realHome, _ := filepath.EvalSymlinks(home)
	res := probeResult{
		Home:        home,
		OS:          runtime.GOOS,
		IdleMinutes: idleMins,
		Roots:       make([]rootProbe, 0, len(roots)),
		Languages:   make([]lspProbe, 0, len(langInputs)),
		ProbedAt:    time.Now().UTC().Format(time.RFC3339),
	}

	configuredReal := map[string]bool{}
	seenRepos := map[string]bool{}
	for _, raw := range roots {
		rp := probeOneRoot(raw, home, realHome)
		res.Roots = append(res.Roots, rp)
		if rp.Resolved != "" {
			configuredReal[rp.Resolved] = true
		}
		if rp.Status == "ok" {
			for _, repo := range findRepos([]string{raw}, home) {
				seenRepos[repo.Path] = true
			}
		}
	}
	res.TotalRepos = len(seenRepos)

	// Discover common candidate folders that exist and hold repositories, not already configured.
	if home != "" {
		for _, rel := range []string{"code", "projects", "workspace", "src", "dev", "GitHub"} {
			display := "~/" + rel
			abs := filepath.Join(home, rel)
			real, err := filepath.EvalSymlinks(abs)
			if err != nil || configuredReal[real] || secretPath(real, home) {
				continue
			}
			st, err := os.Stat(real)
			if err != nil || !st.IsDir() {
				continue
			}
			found := findRepos([]string{display}, home)
			if len(found) > 0 {
				res.Suggestions = append(res.Suggestions, rootSuggestion{Path: display, Repos: len(found)})
			}
		}
	}

	// Probe each managed language server.
	for _, li := range langInputs {
		var cands []string
		for _, s := range li.lang.servers {
			cands = append(cands, s.bin)
		}
		lp := lspProbe{
			Lang:       li.lang.name,
			Enabled:    !li.disabled,
			CustomPath: li.customPath,
			Running:    runningCounts[li.lang.name],
			Candidates: cands,
		}
		switch {
		case li.disabled:
			lp.Status = "disabled"
			if li.autoBin != "" {
				lp.Binary = li.autoBin
			}
		case li.hasEff:
			lp.Status = "ready"
			lp.Binary = li.effective.bin
			lp.Version = n.probeBinaryVersion(ctx, li.effective.bin, env, force)
		case li.customPath != "":
			lp.Status = "invalid_path"
			lp.Error = probePathError(li.customPath, home, env)
		default:
			lp.Status = "missing"
		}
		res.Languages = append(res.Languages, lp)
	}

	// Refresh cached repos/found when force probing so the repo list reflects changes immediately.
	if force {
		n.mu.Lock()
		n.repos = nil
		n.reposAt = time.Time{}
		n.mu.Unlock()
	}
	return res
}

func probeOneRoot(raw, home, realHome string) rootProbe {
	rp := rootProbe{Input: raw}
	dir := expandHome(strings.TrimSpace(raw), home)
	if dir == "" || !filepath.IsAbs(dir) {
		rp.Status = "forbidden"
		rp.Error = "path must be absolute or start with ~/"
		return rp
	}
	st, err := os.Stat(dir)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			rp.Status = "missing"
		case errors.Is(err, os.ErrPermission):
			rp.Status = "permission_denied"
		default:
			rp.Status = "missing"
		}
		rp.Error = err.Error()
		return rp
	}
	if !st.IsDir() {
		rp.Status = "not_dir"
		rp.Error = "not a directory"
		return rp
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		rp.Status = "permission_denied"
		rp.Error = err.Error()
		return rp
	}
	rp.Resolved = real
	if real == "/" || secretPath(real, home) {
		rp.Status = "forbidden"
		rp.Error = "protected or secret directory"
		return rp
	}
	if realHome != "" && within(realHome, real) && real != realHome {
		rp.Status = "forbidden"
		rp.Error = "ancestor of home directory"
		return rp
	}
	if _, err := os.ReadDir(real); err != nil {
		rp.Status = "permission_denied"
		rp.Error = err.Error()
		return rp
	}
	repos := findRepos([]string{raw}, home)
	rp.Status = "ok"
	rp.Repos = len(repos)
	return rp
}

func probePathError(custom, home string, env []string) string {
	p := expandHome(strings.TrimSpace(custom), home)
	if !filepath.IsAbs(p) {
		if found := lookPath(p, env); found != "" {
			p = found
		} else {
			return "not found on PATH"
		}
	}
	st, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "file does not exist"
		}
		if errors.Is(err, os.ErrPermission) {
			return "permission denied"
		}
		return err.Error()
	}
	if !st.Mode().IsRegular() {
		return "not a regular file"
	}
	if st.Mode()&0o111 == 0 {
		return "file is not executable"
	}
	return "invalid executable"
}

// knownVersionArgs returns safe read-only version flags for recognized LSP binaries.
// Unknown custom wrapper binaries return nil so we never invoke arbitrary flags on them.
func knownVersionArgs(bin string) []string {
	switch filepath.Base(bin) {
	case "gopls":
		return []string{"version"}
	case "typescript-language-server", "pyright-langserver", "pylsp", "rust-analyzer", "sourcekit-lsp", "clangd":
		return []string{"--version"}
	default:
		return nil
	}
}

func (n *node) probeBinaryVersion(ctx context.Context, bin string, env []string, force bool) string {
	args := knownVersionArgs(bin)
	if len(args) == 0 {
		return ""
	}
	st, err := os.Stat(bin)
	if err != nil {
		return ""
	}
	n.mu.Lock()
	if !force && n.versions != nil {
		if ce, ok := n.versions[bin]; ok && ce.modTime.Equal(st.ModTime()) && ce.size == st.Size() && time.Since(ce.at) < versionCacheTTL {
			n.mu.Unlock()
			return ce.version
		}
	}
	n.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Env = env
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	ver := ""
	if err == nil || len(out) > 0 {
		if len(out) > 4096 {
			out = out[:4096]
		}
		for _, line := range bytes.Split(out, []byte("\n")) {
			s := strings.TrimSpace(string(line))
			if s != "" {
				ver = clip(s, 120)
				break
			}
		}
	}

	n.mu.Lock()
	if n.versions == nil {
		n.versions = map[string]versionCacheEntry{}
	}
	n.versions[bin] = versionCacheEntry{modTime: st.ModTime(), size: st.Size(), at: time.Now(), version: ver}
	n.mu.Unlock()
	return ver
}
