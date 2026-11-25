package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// What the hook keeps, in the node program's data directory on that computer:
//
//	queue.jsonl    the pairs: one JSON line each, appended; past queueCompactAt lines the oldest are dropped
//	pending.json   per session, the prompt that was blocked and waits for its English rewrite
//	store.lock     an flock that serializes every read-modify-write: several Claude Code sessions run their
//	               hooks at once, each in its own process
//
// Nothing here leaves the computer except through ebd.status, which the extension's page asks for.

const (
	queueFile   = "queue.jsonl"
	pendingFile = "pending.json"
	lockFile    = "store.lock"

	queueCap       = 1000 // pairs kept
	queueCompactAt = 1200 // lines at which the file is cut back to queueCap
	pendingTTL     = 30 * time.Minute
	maxStoredRunes = 4000 // a prompt longer than this is kept cut
	reviewFirstDay = 24 * time.Hour
)

// entry is one pair: what the user wrote and its English, with the review schedule a later review reads.
type entry struct {
	ID          string `json:"id"`
	At          string `json:"at"`
	Original    string `json:"original"`
	English     string `json:"english,omitempty"`
	Mode        string `json:"mode"` // rewrite, unverified, giveup, warn, log, stopword
	Language    string `json:"language,omitempty"`
	Score       *int   `json:"score,omitempty"`
	Natural     *bool  `json:"natural,omitempty"`
	NaturalHint string `json:"natural_hint,omitempty"`
	Reviews     int    `json:"reviews"`
	NextReview  string `json:"next_review"`
	Session     string `json:"session,omitempty"`
}

// pending is a blocked prompt waiting for its rewrite.
type pending struct {
	Original string    `json:"original"`
	Language string    `json:"language,omitempty"`
	At       time.Time `json:"at"`
	Attempts int       `json:"attempts"`
}

type store struct{ dir string }

// locked runs fn holding the store's lock.
func (s store) locked(fn func() error) error {
	f, err := os.OpenFile(filepath.Join(s.dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func cut(s string) string {
	if r := []rune(s); len(r) > maxStoredRunes {
		return string(r[:maxStoredRunes]) + "…"
	}
	return s
}

// add appends a pair, cutting the file back when it has grown past queueCompactAt lines.
func (s store) add(e entry, now time.Time) error {
	var id [6]byte
	_, _ = rand.Read(id[:])
	e.ID = hex.EncodeToString(id[:])
	e.At = now.UTC().Format(time.RFC3339)
	e.NextReview = now.Add(reviewFirstDay).UTC().Format(time.RFC3339)
	e.Original, e.English = cut(e.Original), cut(e.English)
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.locked(func() error {
		p := filepath.Join(s.dir, queueFile)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, werr := f.Write(append(line, '\n'))
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return werr
		}
		lines, err := readLines(p)
		if err != nil || len(lines) <= queueCompactAt {
			return err
		}
		return writeFileAtomic(p, bytes.Join(append(lines[len(lines)-queueCap:], nil), []byte("\n")))
	})
}

// readLines is the file's non-empty lines.
func readLines(p string) ([][]byte, error) {
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if b := bytes.TrimSpace(sc.Bytes()); len(b) > 0 {
			out = append(out, append([]byte(nil), b...))
		}
	}
	return out, sc.Err()
}

// entries is every pair, oldest first; a damaged line is skipped, not the whole queue.
func (s store) entries() ([]entry, error) {
	lines, err := readLines(filepath.Join(s.dir, queueFile))
	if err != nil {
		return nil, err
	}
	out := make([]entry, 0, len(lines))
	for _, l := range lines {
		var e entry
		if json.Unmarshal(l, &e) == nil && e.Original != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s store) loadPending() map[string]pending {
	all := map[string]pending{}
	b, err := os.ReadFile(filepath.Join(s.dir, pendingFile))
	if err == nil {
		_ = json.Unmarshal(b, &all)
	}
	return all
}

func (s store) savePending(all map[string]pending) error {
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, pendingFile), b)
}

// getPending is the session's blocked prompt, if it has one that has not expired. Most prompts come from sessions
// with none, so the file is read first without the lock: it is only ever replaced whole.
func (s store) getPending(session string, now time.Time) (*pending, error) {
	if _, ok := s.loadPending()[session]; !ok {
		return nil, nil
	}
	var out *pending
	err := s.locked(func() error {
		all := s.loadPending()
		p, ok := all[session]
		if !ok {
			return nil
		}
		if now.Sub(p.At) > pendingTTL {
			delete(all, session)
			return s.savePending(all)
		}
		out = &p
		return nil
	})
	return out, err
}

// setPending records (or renews) the session's blocked prompt.
func (s store) setPending(session, original, lang string, now time.Time) error {
	return s.locked(func() error {
		all := s.loadPending()
		p := pending{Original: cut(original), Language: lang, At: now}
		if prev, ok := all[session]; ok && prev.Original == p.Original {
			p.Attempts = prev.Attempts + 1
		}
		// drop the expired ones while here
		for k, v := range all {
			if now.Sub(v.At) > pendingTTL {
				delete(all, k)
			}
		}
		all[session] = p
		return s.savePending(all)
	})
}

func (s store) clearPending(session string) error {
	return s.locked(func() error {
		all := s.loadPending()
		if _, ok := all[session]; !ok {
			return nil
		}
		delete(all, session)
		return s.savePending(all)
	})
}

// writeFileAtomic replaces path with b (mode 0600) so that a reader sees the old file or the new one.
func writeFileAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
