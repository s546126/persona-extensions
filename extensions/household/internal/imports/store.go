package imports

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Limits of what is kept: drafts live a day and at most DraftsMost are kept (a new one pushes the oldest out); the
// last AppliedMost applied imports are remembered, the newest of which can be undone.
const (
	DraftLife   = 24 * time.Hour
	DraftsMost  = 5
	AppliedMost = 10
)

// Draft is an import waiting for the user's yes: what was asked, the answers given since, and the plan as it was
// last shown (the user agrees to that plan; applying refuses a draft whose plan the records have changed).
type Draft struct {
	ID      string    `json:"id"`
	Rev     int       `json:"revision"`          // goes up each time the plan is stored again; a yes is for one revision
	ByUser  bool      `json:"by_user,omitempty"` // made on the page (粘贴清单): the assistant cannot re-plan or push it out
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Input   Input     `json:"input"`
	Edits   []Edit    `json:"edits,omitempty"`
	Plan    Plan      `json:"plan"`
}

// Touch is one record an applied import changed: what it was before (nil for a record the import created) and what it
// was right after, both as the kernel stores them.
type Touch struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Title  string          `json:"title"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after"`
}

// Applied is an import that was written.
type Applied struct {
	Draft   string    `json:"draft"`
	Source  string    `json:"source"`
	At      time.Time `json:"at"`
	Says    []string  `json:"says"`
	Totals  Totals    `json:"totals"`
	Touched []Touch   `json:"touched"`
	Undone  time.Time `json:"undone,omitzero"`
	// Pending marks an import whose records were being saved: it is settled against the records (Settle) as written
	// or as never written. UndoPending is the same for its undo.
	Pending     bool `json:"pending,omitempty"`
	UndoPending bool `json:"undo_pending,omitempty"`
	// Unknown is set when a pending change could not be decided from the records (see Settle).
	Unknown string `json:"unknown,omitempty"`
	// Seen marks an undecided undo the user has looked at (Acknowledge).
	Seen bool `json:"seen,omitempty"`
}

// State is everything the store keeps.
type State struct {
	Version int       `json:"version"`
	Seq     int       `json:"seq"`
	Drafts  []Draft   `json:"drafts"`
	Applied []Applied `json:"applied"`
}

// Draft returns the draft with the id.
func (s *State) Draft(id string) (Draft, bool) {
	i := slices.IndexFunc(s.Drafts, func(d Draft) bool { return d.ID == id })
	if i < 0 {
		return Draft{}, false
	}
	return s.Drafts[i], true
}

// Put stores d: a draft with an id replaces the stored one as its next revision; a new draft gets an id and revision
// 1. Beyond DraftsMost the oldest draft is pushed out, but a draft the user made on the page only by the user: when
// the assistant's draft would push out only those, it is refused. It returns the draft as stored.
func (s *State) Put(d Draft, now time.Time, byUser bool) (Draft, error) {
	if i := slices.IndexFunc(s.Drafts, func(x Draft) bool { return x.ID == d.ID && d.ID != "" }); i >= 0 {
		d.Rev = s.Drafts[i].Rev + 1
		s.Drafts[i] = d
		return d, nil
	}
	if len(s.Drafts) >= DraftsMost {
		i := slices.IndexFunc(s.Drafts, func(x Draft) bool { return byUser || !x.ByUser })
		if i < 0 {
			return Draft{}, fmt.Errorf("已经有 %d 份用户在页面上粘贴、还没确认的导入，请先让用户在家庭管家页面上确认或放弃", len(s.Drafts))
		}
		s.Drafts = slices.Delete(s.Drafts, i, i+1)
	}
	s.Seq++
	d.ID = "imp-" + strconv.Itoa(s.Seq)
	d.Rev = 1
	d.ByUser = byUser
	d.Created = now
	d.Expires = now.Add(DraftLife)
	s.Drafts = append(s.Drafts, d)
	return d, nil
}

// Drop removes the draft with the id and reports whether there was one.
func (s *State) Drop(id string) bool {
	n := len(s.Drafts)
	s.Drafts = slices.DeleteFunc(s.Drafts, func(d Draft) bool { return d.ID == id })
	return len(s.Drafts) < n
}

// Record remembers an applied import, keeping the last AppliedMost.
func (s *State) Record(a Applied) {
	s.Applied = append(s.Applied, a)
	if over := len(s.Applied) - AppliedMost; over > 0 {
		s.Applied = slices.Delete(s.Applied, 0, over)
	}
}

// Last is the import applied last, false when there is none. An import whose records were never confirmed saved
// (Pending), or whose write could not be decided (Unknown apply), is not it.
func (s *State) Last() (*Applied, bool) {
	for i := len(s.Applied) - 1; i >= 0; i-- {
		if a := s.Applied[i]; !a.Pending && a.Unknown != UnknownApply {
			return &s.Applied[i], true
		}
	}
	return nil, false
}

// prune drops the drafts that expired at now.
func (s *State) prune(now time.Time) {
	s.Drafts = slices.DeleteFunc(s.Drafts, func(d Draft) bool { return !now.Before(d.Expires) })
}

// Store keeps the State in imports.json in the data directory, beside the records, or in memory. An update takes an
// exclusive lock on the directory's imports.lock, reads the file, changes it and replaces it atomically, so the page
// and the assistant's server can both use it. A file that cannot be read is refused and never overwritten.
type Store struct {
	dir string // "" = in memory

	mu   sync.Mutex
	mem  []byte
	seen stamp
	now  func() time.Time
}

type stamp struct {
	ok    bool
	mtime time.Time
	size  int64
}

// Version is the format of imports.json this code writes; a file of a newer format is refused, never overwritten.
const Version = 1

const (
	fileName = "imports.json"
	lockName = "imports.lock"
)

// Open is the store in dir; "" keeps it in memory. It reads nothing.
func Open(dir string) *Store {
	s := &Store{dir: dir, now: time.Now}
	s.seen = s.stat()
	return s
}

// SetClock replaces the store's clock, for tests.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Now is the store's time.
func (s *Store) Now() time.Time { return s.now() }

// View gives f the state with the expired drafts left out. f must not keep it.
func (s *Store) View(f func(st *State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	st.prune(s.now())
	return f(st)
}

// Update gives f the state under the lock and saves what f leaves when it returns nil. Expired drafts are dropped.
func (s *Store) Update(f func(st *State, now time.Time) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" {
		unlock, err := s.lock()
		if err != nil {
			return err
		}
		defer unlock()
	}
	st, err := s.load()
	if err != nil {
		return err
	}
	now := s.now()
	st.prune(now)
	if err := f(st, now); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if s.dir == "" {
		s.mem = raw
		return nil
	}
	if err := writeAtomic(s.path(), raw); err != nil {
		return err
	}
	s.seen = s.stat()
	return nil
}

// Changed reports whether another process changed the file since this one last read or wrote it; each change once.
func (s *Store) Changed() bool {
	if s.dir == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stat()
	if st == s.seen {
		return false
	}
	s.seen = st
	return true
}

func (s *Store) load() (*State, error) {
	var raw []byte
	if s.dir == "" {
		raw = s.mem
	} else {
		var err error
		raw, err = os.ReadFile(s.path())
		if errors.Is(err, fs.ErrNotExist) {
			raw = nil
		} else if err != nil {
			return nil, fmt.Errorf("读不了导入草稿 %s：%w", s.path(), err)
		}
	}
	st := &State{Version: Version, Drafts: []Draft{}, Applied: []Applied{}}
	if len(bytes.TrimSpace(raw)) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("导入草稿的文件 %s 坏了，没有读它，也不会覆盖它：%w", s.path(), err)
	}
	if st.Version > Version {
		return nil, fmt.Errorf("导入草稿的文件 %s 是更新版本的家庭管家写的（格式 %d），这个版本读不了，也不会覆盖它", s.path(), st.Version)
	}
	st.Version = Version
	return st, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, fileName) }

func (s *Store) stat() stamp {
	if s.dir == "" {
		return stamp{}
	}
	fi, err := os.Stat(s.path())
	if err != nil {
		return stamp{}
	}
	return stamp{ok: true, mtime: fi.ModTime(), size: fi.Size()}
}

// lock takes the cross-process lock of the store and returns the function that releases it.
func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// writeAtomic writes data beside path and renames it over path.
func writeAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
