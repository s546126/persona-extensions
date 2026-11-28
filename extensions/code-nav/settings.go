package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// hubMachineID identifies the hub's own local read-only code-nav node.
const hubMachineID = "hub"

// managedLanguages lists the canonical language groups configurable in the UI.
// TypeScript covers JavaScript; C covers C++.
var managedLanguages = []string{"go", "typescript", "python", "rust", "swift", "c"}

// canonicalLang maps a language name to its configurable group name.
func canonicalLang(name string) string {
	switch name {
	case "javascript":
		return "typescript"
	case "cpp":
		return "c"
	default:
		return name
	}
}

// isManagedLang reports whether name is one of the configurable language groups.
func isManagedLang(name string) bool {
	return slices.Contains(managedLanguages, name)
}

// langOverride is a user's per-language override on one computer.
type langOverride struct {
	Disabled bool   `json:"disabled,omitempty"`
	Path     string `json:"path,omitempty"` // custom LSP binary path; "" uses auto-detect or manifest default
}

// machineSettings holds per-computer runtime settings persisted by the hub.
type machineSettings struct {
	Roots       []string                `json:"roots,omitempty"`
	RootsSet    bool                    `json:"roots_set,omitempty"` // distinguishes explicit empty list from inherit
	IdleMinutes int                     `json:"idle_minutes,omitempty"`
	Languages   map[string]langOverride `json:"languages,omitempty"`
}

// nodeOverride is the effective runtime override sent to a node (hub local or remote).
type nodeOverride struct {
	Roots       []string                `json:"roots,omitempty"`
	RootsSet    bool                    `json:"roots_set,omitempty"`
	IdleMinutes int                     `json:"idle_minutes,omitempty"`
	Languages   map[string]langOverride `json:"languages,omitempty"`
}

// persistedSettings is saved as JSON in the extension's DataDir on the hub.
type persistedSettings struct {
	Version    int                        `json:"version"`
	Configured bool                       `json:"configured,omitempty"`
	UseHub     bool                       `json:"use_hub,omitempty"`
	Machines   map[string]machineSettings `json:"machines,omitempty"`
}

const (
	settingsFileName = "code-nav-settings.json"
	maxRootsLimit    = 32
	maxRootPathLen   = 512
	maxLSPPathLen    = 512
)

// settingsStore keeps persistedSettings in memory and atomically writes it to DataDir when set.
type settingsStore struct {
	mu   sync.Mutex
	dir  string
	data persistedSettings
}

func newSettingsStore(dir string) *settingsStore {
	s := &settingsStore{
		dir:  strings.TrimSpace(dir),
		data: persistedSettings{Version: 1, Machines: map[string]machineSettings{}},
	}
	if s.dir != "" {
		_ = s.loadLocked()
	}
	return s
}

func (s *settingsStore) path() string {
	return filepath.Join(s.dir, settingsFileName)
}

func (s *settingsStore) loadLocked() error {
	raw, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var p persistedSettings
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if p.Version == 0 {
		p.Version = 1
	}
	if p.Machines == nil {
		p.Machines = map[string]machineSettings{}
	}
	s.data = p
	return nil
}

func (s *settingsStore) saveLocked() error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	s.data.Version = 1
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(s.dir, ".code-nav-settings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path()); err != nil {
		return err
	}
	ok = true
	return nil
}

func (s *settingsStore) get() persistedSettings {
	if s == nil {
		return persistedSettings{Version: 1, Machines: map[string]machineSettings{}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return clonePersistedSettings(s.data)
}

func (s *settingsStore) update(fn func(*persistedSettings) error) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Machines == nil {
		s.data.Machines = map[string]machineSettings{}
	}
	cp := clonePersistedSettings(s.data)
	if err := fn(&cp); err != nil {
		return err
	}
	s.data = cp
	return s.saveLocked()
}

func clonePersistedSettings(in persistedSettings) persistedSettings {
	out := persistedSettings{
		Version:    in.Version,
		Configured: in.Configured,
		UseHub:     in.UseHub,
		Machines:   make(map[string]machineSettings, len(in.Machines)),
	}
	for k, ms := range in.Machines {
		out.Machines[k] = cloneMachineSettings(ms)
	}
	return out
}

func cloneMachineSettings(ms machineSettings) machineSettings {
	out := machineSettings{
		Roots:       slices.Clone(ms.Roots),
		RootsSet:    ms.RootsSet,
		IdleMinutes: ms.IdleMinutes,
	}
	if len(ms.Languages) > 0 {
		out.Languages = make(map[string]langOverride, len(ms.Languages))
		for k, v := range ms.Languages {
			out.Languages[k] = v
		}
	}
	return out
}

func cloneNodeOverride(in *nodeOverride) *nodeOverride {
	if in == nil {
		return nil
	}
	out := &nodeOverride{
		Roots:       slices.Clone(in.Roots),
		RootsSet:    in.RootsSet,
		IdleMinutes: in.IdleMinutes,
	}
	if len(in.Languages) > 0 {
		out.Languages = make(map[string]langOverride, len(in.Languages))
		for k, v := range in.Languages {
			out.Languages[k] = v
		}
	}
	return out
}

func (ms machineSettings) toOverride() *nodeOverride {
	if !ms.RootsSet && ms.IdleMinutes <= 0 && len(ms.Languages) == 0 {
		return nil
	}
	ov := &nodeOverride{
		Roots:       slices.Clone(ms.Roots),
		RootsSet:    ms.RootsSet,
		IdleMinutes: ms.IdleMinutes,
	}
	if len(ms.Languages) > 0 {
		ov.Languages = make(map[string]langOverride, len(ms.Languages))
		for k, v := range ms.Languages {
			ov.Languages[k] = v
		}
	}
	return ov
}

// cleanRootPath trims and validates a user-entered code root path.
func cleanRootPath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", errors.New("path is empty")
	}
	if len(p) > maxRootPathLen {
		return "", fmt.Errorf("path exceeds %d characters", maxRootPathLen)
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("path contains NUL byte")
	}
	if p != "~" && !strings.HasPrefix(p, "~/") && !filepath.IsAbs(p) {
		return "", errors.New("path must be absolute or start with ~/")
	}
	return p, nil
}

// cleanLSPPath trims and validates a user-entered custom LSP binary path.
func cleanLSPPath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", nil
	}
	if len(p) > maxLSPPathLen {
		return "", fmt.Errorf("path exceeds %d characters", maxLSPPathLen)
	}
	if strings.ContainsAny(p, "\x00\r\n") {
		return "", errors.New("path contains control characters")
	}
	return p, nil
}

// pairedMachine is a computer reported by the hub's GET /api/v1/machines endpoint.
type pairedMachine struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
	OS     string `json:"os,omitempty"`
	Trust  string `json:"trust,omitempty"`
}

const maxHubMachinesBody = 1 << 20

// fetchPairedMachines reads the hub's paired computers list when machines.read is granted.
func fetchPairedMachines(ctx context.Context, baseURL, token string) ([]pairedMachine, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	token = strings.TrimSpace(token)
	if baseURL == "" || token == "" {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, baseURL+"/api/v1/machines", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/v1/machines: http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHubMachinesBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxHubMachinesBody {
		return nil, errors.New("GET /api/v1/machines: response too large")
	}
	// Accept either a JSON array or {"machines":[...]}.
	var list []pairedMachine
	if json.Unmarshal(body, &list) == nil {
		return list, nil
	}
	var wrap struct {
		Machines []pairedMachine `json:"machines"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, err
	}
	return wrap.Machines, nil
}
