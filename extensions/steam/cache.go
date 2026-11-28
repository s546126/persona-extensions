package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	profileTTL   = 8 * time.Hour
	libraryTTL   = 8 * time.Hour
	presenceTTL  = 10 * time.Minute
	recentTTL    = 10 * time.Minute
	wishlistTTL  = 10 * time.Minute
	maxStaleAge  = 24 * time.Hour
	retryDelay   = 30 * time.Second
	cacheMaxSize = 16 << 20
)

type reading[T any] struct {
	Data  *T
	Issue problem
	Stale bool
	At    time.Time
}

type cacheRecord[T any] struct {
	Scope string    `json:"scope"`
	At    time.Time `json:"at"`
	Data  *T        `json:"data"`
}

// Each dataset has one slot. A read waiting for a concurrent fetch can honor its own deadline.
type cacheSlot[T any] struct {
	gate      chan struct{}
	path      string
	now       func() time.Time
	loaded    bool
	record    cacheRecord[T]
	freshOnly bool
	// QR slots belong to exactly one account and generation. The session lock
	// guards their records against logout while the gate serializes fetches.
	session *qrSession

	retryScope string
	retryAt    time.Time
	failedRead reading[T]
}

func newSlot[T any](dir, name string, now func() time.Time, session *qrSession) *cacheSlot[T] {
	s := &cacheSlot[T]{gate: make(chan struct{}, 1), now: now, session: session}
	if dir != "" {
		s.path = filepath.Join(dir, name+".json")
	}
	return s
}

func (s *cacheSlot[T]) read(ctx context.Context, scope string, ttl time.Duration, refresh bool, fetch func(context.Context) (*T, problem)) reading[T] {
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return reading[T]{Issue: sanitizeError(ctx.Err())}
	}
	out, ready := s.cached(scope, ttl, refresh)
	if ready {
		return out
	}
	if ctx.Err() != nil {
		return reading[T]{Issue: sanitizeError(ctx.Err())}
	}
	data, issue := fetch(ctx)
	return s.update(scope, data, issue)
}

func (s *cacheSlot[T]) cached(scope string, ttl time.Duration, refresh bool) (reading[T], bool) {
	if s.session != nil {
		s.session.auth.mu.Lock()
		defer s.session.auth.mu.Unlock()
		if !s.ownsScopeLocked(scope) {
			return reading[T]{Issue: problemCanceled}, true
		}
	}
	if !s.loaded {
		s.loaded = true
		s.record = readRecord[T](s.path)
	}
	now := s.now()
	age := now.Sub(s.record.At)
	hasData := s.record.Scope == scope && s.record.Data != nil && age >= 0
	if !refresh {
		if s.retryScope == scope && !s.retryAt.IsZero() {
			if now.Before(s.retryAt) {
				if s.failedRead.Stale && age > maxStaleAge {
					return reading[T]{Issue: s.failedRead.Issue}, true
				}
				return s.failedRead, true
			}
		} else if hasData && age < ttl {
			return reading[T]{Data: s.record.Data, At: s.record.At}, true
		}
	}
	return reading[T]{}, false
}

func (s *cacheSlot[T]) update(scope string, data *T, issue problem) reading[T] {
	if s.session != nil {
		s.session.auth.mu.Lock()
		defer s.session.auth.mu.Unlock()
		if !s.ownsScopeLocked(scope) {
			return reading[T]{Issue: problemCanceled}
		}
	}
	if issue == "" && data != nil {
		s.record = cacheRecord[T]{Scope: scope, At: s.now(), Data: data}
		s.retryAt, s.failedRead = time.Time{}, reading[T]{}
		writeRecord(s.path, s.record)
		return reading[T]{Data: data, At: s.record.At}
	}
	if issue == "" {
		issue = problemResponse
	}
	if issue == problemCanceled {
		return reading[T]{Issue: issue}
	}

	out := reading[T]{Data: data, Issue: issue}
	if issue.temporary() && !s.freshOnly {
		if age := s.now().Sub(s.record.At); s.record.Scope == scope && s.record.Data != nil && age >= 0 && age <= maxStaleAge {
			out = reading[T]{Data: s.record.Data, Issue: issue, Stale: true, At: s.record.At}
		}
	} else if s.record.Scope == scope || issue == problemWishlist {
		// Discard hidden or invalid data. Wishlist visibility applies across languages.
		s.record = cacheRecord[T]{}
		if s.path != "" {
			_ = os.Remove(s.path)
		}
	}
	s.retryScope, s.retryAt, s.failedRead = scope, s.now().Add(retryDelay), out
	return out
}

func (s *cacheSlot[T]) clear() {
	s.record, s.failedRead = cacheRecord[T]{}, reading[T]{}
	s.retryScope, s.retryAt = "", time.Time{}
}

// peek reads an already observed value without modifying cache state or waiting
// for a foreground fetch. It can also read a previous Web API key run's record.
func (s *cacheSlot[T]) peek(scope string, maxAge time.Duration) *T {
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	default:
		return nil
	}
	if s.session != nil {
		s.session.auth.mu.Lock()
		defer s.session.auth.mu.Unlock()
		if !s.ownsScopeLocked(scope) {
			return nil
		}
	}
	record := s.record
	if !s.loaded {
		record = readRecord[T](s.path)
	}
	age := s.now().Sub(record.At)
	if record.Scope == scope && age >= 0 && age <= maxAge {
		return record.Data
	}
	return nil
}

func (s *cacheSlot[T]) ownsScopeLocked(scope string) bool {
	id, _, _ := strings.Cut(scope, "/") // Wishlist scopes also include country and language.
	return s.session.currentLocked() && s.session.reads.config.AuthMode == modeQR && s.session.tokens.secret.SteamID == id
}

func readRecord[T any](path string) cacheRecord[T] {
	var out cacheRecord[T]
	if path == "" {
		return out
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > cacheMaxSize {
		return out
	}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, cacheMaxSize+1))
	if err != nil || len(raw) > cacheMaxSize || json.Unmarshal(raw, &out) != nil {
		return cacheRecord[T]{}
	}
	return out
}

func writeRecord[T any](path string, record cacheRecord[T]) {
	if path == "" {
		return
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > cacheMaxSize || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cache-*")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(raw)
	if f.Close() == nil && writeErr == nil {
		_ = os.Rename(f.Name(), path)
	}
}
