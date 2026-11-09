package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const stateFile = "spaces_state.json"

// Store keeps the State in <dir>/spaces_state.json. Two processes share it: the extension the hub runs (the views)
// and `spaces mcp` the assistant runs. Each writes the whole file atomically (a temporary file, then rename) and
// reads it again before any use when its modification time, size or generation is not what it last saw, so a
// change made by the other process shows on the next render or action. With no directory it keeps the state in
// memory.
type Store struct {
	mu   sync.Mutex
	dir  string
	path string
	st   *State
	// What this process last read or wrote.
	mod  time.Time
	size int64
	now  func() time.Time
	logf func(string, ...any)
}

func openStore(dir string, logf func(string, ...any)) *Store {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Store{dir: dir, now: time.Now, logf: logf}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			logf("spaces: %v", err)
		} else {
			s.path = filepath.Join(dir, stateFile)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
	return s
}

// refresh reads the file again when it changed since this process saw it; with no file it seeds the example spaces.
func (s *Store) refresh() {
	if s.path == "" {
		if s.st == nil {
			s.st = seedState(s.now())
		}
		return
	}
	fi, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		if s.st == nil {
			s.st = seedState(s.now())
			s.flush()
		}
		return
	}
	if err != nil {
		s.logf("spaces: %v", err)
		if s.st == nil {
			s.st = seedState(s.now())
		}
		return
	}
	// The same time and size as last seen: unchanged, unless that time is so recent that another write in the same
	// clock tick could hide behind it.
	if s.st != nil && fi.ModTime().Equal(s.mod) && fi.Size() == s.size && time.Since(fi.ModTime()) > 2*time.Second {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err == nil {
		var st State
		if err = json.Unmarshal(raw, &st); err == nil {
			s.st = normalizeState(&st, s.now())
			s.mod, s.size = fi.ModTime(), fi.Size()
			return
		}
	}
	// Unreadable: keep it aside and start again rather than lose it silently.
	aside := s.path + ".corrupt"
	if rerr := os.Rename(s.path, aside); rerr != nil {
		s.logf("spaces: %s unreadable (%v), and it could not be moved aside: %v", s.path, err, rerr)
	} else {
		s.logf("spaces: %s unreadable (%v), moved to %s", s.path, err, aside)
	}
	if s.st == nil {
		s.st = seedState(s.now())
	}
	s.flush()
}

// normalizeState makes a state read from disk whole: every space has the built-in types, maps are not nil.
func normalizeState(st *State, now time.Time) *State {
	if st.Version == 0 {
		st.Version = stateVersion
	}
	kept := st.Spaces[:0]
	for _, sp := range st.Spaces {
		if sp == nil || sp.ID == "" {
			continue
		}
		for _, bt := range builtinTypes() {
			t, ok := sp.objectType(bt.Key)
			if !ok {
				sp.Types = append([]ObjectType{bt}, sp.Types...)
				continue
			}
			// A built-in type keeps its own fields and statuses; fields added to it stay.
			extra := []FieldDef{}
			for _, f := range t.Fields {
				if _, own := bt.field(f.Key); !own {
					extra = append(extra, f)
				}
			}
			label, desc := t.Label, t.Description
			*t = bt
			t.Fields = append(t.Fields, extra...)
			if label != "" {
				t.Label = label
			}
			if desc != "" {
				t.Description = desc
			}
		}
		if sp.Entities == nil {
			sp.Entities = []*Entity{}
		}
		ents := sp.Entities[:0]
		for _, e := range sp.Entities {
			if e == nil || e.ID == "" {
				continue
			}
			if e.Properties == nil {
				e.Properties = map[string]any{}
			}
			e.SpaceID = sp.ID
			ents = append(ents, e)
		}
		sp.Entities = ents
		if sp.Relations == nil {
			sp.Relations = []Relation{}
		}
		if sp.Icon == "" || !validIcon(sp.Icon) {
			sp.Icon = "folder"
		}
		kept = append(kept, sp)
	}
	st.Spaces = kept
	if len(st.Spaces) == 0 {
		return seedState(now)
	}
	if st.activeSpace() != nil {
		st.Active = st.activeSpace().ID
	}
	return st
}

// flush writes the state: a temporary file, then rename.
func (s *Store) flush() {
	if s.path == "" {
		return
	}
	raw, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		s.logf("spaces: %v", err)
		return
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(s.dir, stateFile+".*.tmp")
	if err != nil {
		s.logf("spaces: %v", err)
		return
	}
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		s.logf("spaces: writing %s: %v %v", tmp.Name(), werr, cerr)
		return
	}
	_ = os.Chmod(tmp.Name(), 0o600)
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		_ = os.Remove(tmp.Name())
		s.logf("spaces: %v", err)
		return
	}
	if fi, err := os.Stat(s.path); err == nil {
		s.mod, s.size = fi.ModTime(), fi.Size()
	}
}

// read runs f on the current state; f must not keep it, nor call the store.
func (s *Store) read(f func(st *State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
	f(s.st)
}

// update runs f on the current state and saves what it changed; an error leaves the state as it was.
func (s *Store) update(f func(st *State, now time.Time) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
	backup, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	if err := f(s.st, s.now()); err != nil {
		var st State
		if jerr := json.Unmarshal(backup, &st); jerr == nil {
			s.st = normalizeState(&st, s.now())
		}
		return err
	}
	s.st.Generation++
	s.flush()
	return nil
}

// generation is the state's version counter (each saved change adds one).
func (s *Store) generation() int64 {
	var g int64
	s.read(func(st *State) { g = st.Generation })
	return g
}

// ---- Where the MCP server finds the hub's data ----

// locatorFile is where the extension the hub runs writes its data directory, for `spaces mcp` on the same computer.
// Tests point it elsewhere.
var locatorFile = func() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "persona-spaces", "data-dir")
}

// rememberDataDir writes dir to the locator (best effort: a sandbox may not allow it).
func rememberDataDir(dir string, logf func(string, ...any)) {
	path := locatorFile()
	if path == "" || dir == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		err = os.WriteFile(path, []byte(dir+"\n"), 0o600)
		if err == nil {
			return
		}
		logf("spaces: locator %s: %v", path, err)
	}
}

// resolveDataDir is the MCP server's data directory: the flag, PERSONA_SPACES_DATA, the directory the hub's
// extension left in the locator, else a directory of its own in the user's config directory.
func resolveDataDir(flagDir string) (string, error) {
	if flagDir != "" {
		return flagDir, nil
	}
	if d := strings.TrimSpace(os.Getenv("PERSONA_SPACES_DATA")); d != "" {
		return d, nil
	}
	if path := locatorFile(); path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			if d := strings.TrimSpace(string(raw)); d != "" {
				if fi, err := os.Stat(d); err == nil && fi.IsDir() {
					return d, nil
				}
			}
		}
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no data directory: pass --data or set PERSONA_SPACES_DATA (%w)", err)
	}
	return filepath.Join(dir, "persona-spaces", "data"), nil
}
