package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	storeDirectory = "steam-store-v1"
	storeVersion   = 1

	maxPriceSamples     = 32
	maxPriceHistories   = 2_000
	maxPlaytimeDays     = 400
	maxPlaytimeAccounts = 16
	maxBacklogEntries   = 2_000
	maxBacklogNoteRunes = 240
	maxBacklogTags      = 8
	maxBacklogTagRunes  = 32
	maxEventCursors     = 5_000
	eventCursorTTL      = 90 * 24 * time.Hour
)

type steamStores struct {
	prices   *PriceHistoryStore
	playtime *PlaytimeSnapshotStore
	cursors  *EventCursorStore
}

func newSteamStores(dataDir string) steamStores {
	return steamStores{
		prices:   NewPriceHistoryStore(dataDir),
		playtime: NewPlaytimeSnapshotStore(dataDir),
		cursors:  NewEventCursorStore(dataDir),
	}
}

func (s steamStores) clearMemory() {
	s.prices.file.clearMemory()
	s.playtime.file.clearMemory()
	s.playtime.recaps.clearMemory()
	s.cursors.file.clearMemory()
}

// Each dataset has its own file and writer lock. Disk readers see either complete version
// of an atomic replacement. An empty data directory uses a mutex-protected in-memory map.
type snapshotFile[T any] struct {
	path string
	mu   sync.Mutex
	mem  map[string]T
}

type snapshotRecord[T any] struct {
	Version int          `json:"version"`
	Entries map[string]T `json:"entries"`
}

func newSnapshotFile[T any](dataDir, name string) *snapshotFile[T] {
	s := &snapshotFile[T]{}
	if dataDir != "" {
		s.path = filepath.Join(dataDir, storeDirectory, name+".json")
	}
	return s
}

func (s *snapshotFile[T]) clearMemory() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mem = nil
}

func (s *snapshotFile[T]) view(read func(map[string]T)) error {
	if s.path == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		read(s.mem)
		return nil
	}
	entries, err := s.load()
	if err != nil {
		return err
	}
	read(entries)
	return nil
}

func (s *snapshotFile[T]) update(change func(map[string]T) (bool, error)) error {
	if s.path == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.mem == nil {
			s.mem = make(map[string]T)
		}
		_, err := change(s.mem)
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := checkStoreDirectory(dir); err != nil {
		return err
	}
	lock, err := lockSnapshot(s.path + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()

	entries, err := s.load()
	if err != nil {
		return err
	}
	if entries == nil {
		entries = make(map[string]T)
	}
	changed, err := change(entries)
	if err != nil || !changed {
		return err
	}
	raw, err := json.Marshal(snapshotRecord[T]{Version: storeVersion, Entries: entries})
	if err != nil {
		return err
	}
	if len(raw) > cacheMaxSize {
		return fmt.Errorf("Steam snapshot %s exceeds the size limit", filepath.Base(s.path))
	}
	return replaceSnapshot(s.path, raw)
}

func checkStoreDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("Steam snapshot directory must be a directory")
	}
	return nil
}

func lockSnapshot(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("Steam snapshot lock must be a regular file")
	}
	if err == nil {
		for {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
			if !errors.Is(err, syscall.EINTR) {
				break
			}
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (s *snapshotFile[T]) load() (map[string]T, error) {
	if err := checkStoreDirectory(filepath.Dir(s.path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	badFile := fmt.Errorf("Steam snapshot %s must be a regular file within the size limit", filepath.Base(s.path))
	if !info.Mode().IsRegular() || info.Size() > cacheMaxSize {
		return nil, badFile
	}
	f, err := os.OpenFile(s.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > cacheMaxSize {
		return nil, badFile
	}
	raw, err := io.ReadAll(io.LimitReader(f, cacheMaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > cacheMaxSize {
		return nil, badFile
	}
	var record snapshotRecord[T]
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("decode Steam snapshot %s: %w", filepath.Base(s.path), err)
	}
	if record.Version != storeVersion || record.Entries == nil {
		return nil, fmt.Errorf("unsupported Steam snapshot format in %s", filepath.Base(s.path))
	}
	return record.Entries, nil
}

func replaceSnapshot(path string, raw []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".snapshot-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	if dirFile, err := os.Open(dir); err == nil {
		// Some file systems cannot sync directories. The replacement has already succeeded.
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func validSnapshotTime(at time.Time) bool {
	return !at.IsZero() && at.Year() >= 1 && at.Year() <= 9999
}

func utcDay(at time.Time) time.Time { return at.UTC().Truncate(24 * time.Hour) }

// putDaily maintains chronological order and keeps the latest observation on each UTC day.
func putDaily[T comparable](entries []T, entry T, at func(T) time.Time) ([]T, bool) {
	when := at(entry)
	i, exists := slices.BinarySearchFunc(entries, utcDay(when), func(existing T, day time.Time) int {
		return utcDay(at(existing)).Compare(day)
	})
	if !exists {
		return slices.Insert(entries, i, entry), true
	}
	if when.Before(at(entries[i])) || entries[i] == entry {
		return entries, false
	}
	entries[i] = entry
	return entries, true
}

func trimOldest[K cmp.Ordered, T any](entries map[K]T, limit int, at func(T) time.Time) {
	extra := len(entries) - limit
	if extra <= 0 {
		return
	}
	keys := slices.SortedFunc(maps.Keys(entries), func(a, b K) int {
		if order := at(entries[a]).Compare(at(entries[b])); order != 0 {
			return order
		}
		return cmp.Compare(a, b)
	})
	for _, key := range keys[:extra] {
		delete(entries, key)
	}
}

// PriceQuote uses the Store's minor currency units and discount percentage.
type PriceQuote struct {
	Currency        string `json:"currency"`
	Final           uint32 `json:"final"`
	DiscountPercent int    `json:"discount_percent"`
}

type PriceSample struct {
	PriceQuote
	At time.Time `json:"at"`
}

// PriceHistory keeps the latest sample per observed UTC day and the first occurrence of the local low.
// A newer quote in a different currency starts a new series for the same account, country and app.
type PriceHistory struct {
	Samples                []PriceSample `json:"samples"`
	Latest                 PriceSample   `json:"latest"`
	ObservedLowestFinal    uint32        `json:"observed_lowest_final"`
	ObservedLowestDiscount int           `json:"observed_lowest_discount"`
	ObservedLowestAt       time.Time     `json:"observed_lowest_at"`
	ObservedLowestImproved bool          `json:"observed_lowest_improved,omitempty"`
}

type PriceHistoryStore struct{ file *snapshotFile[PriceHistory] }

func NewPriceHistoryStore(dataDir string) *PriceHistoryStore {
	return &PriceHistoryStore{newSnapshotFile[PriceHistory](dataDir, "prices")}
}

func priceScope(steamID, country string) (string, error) {
	country = strings.ToUpper(strings.TrimSpace(country))
	if !validSteamID(steamID) || len(country) != 2 || !strings.Contains(isoCountries, " "+country+" ") {
		return "", errors.New("price history requires a SteamID64 and a Store country code")
	}
	return steamID + "/" + country + "/", nil
}

// Observe records all valid quotes from one fetch in a single update.
func (s *PriceHistoryStore) Observe(steamID, country string, at time.Time, prices map[uint32]PriceQuote) error {
	scope, err := priceScope(steamID, country)
	if err != nil {
		return err
	}
	at = at.UTC()
	if !validSnapshotTime(at) {
		return errors.New("price observation requires a timestamp")
	}
	for appID, price := range prices {
		if appID == 0 || len(price.Currency) != 3 || strings.IndexFunc(price.Currency, func(r rune) bool {
			return r < 'A' || r > 'Z'
		}) >= 0 || price.DiscountPercent < 0 || price.DiscountPercent > 100 {
			return errors.New("invalid Steam price observation")
		}
	}
	if len(prices) == 0 {
		return nil
	}
	return s.file.update(func(entries map[string]PriceHistory) (bool, error) {
		changed := false
		for appID, price := range prices {
			key := scope + appIDText(appID)
			history := entries[key]
			if history.observe(PriceSample{PriceQuote: price, At: at}) {
				entries[key] = history
				changed = true
			}
		}
		trimOldest(entries, maxPriceHistories, func(h PriceHistory) time.Time { return h.Latest.At })
		return changed, nil
	})
}

func (h *PriceHistory) observe(sample PriceSample) bool {
	if h.Latest.Currency != sample.Currency {
		if sample.At.Before(h.Latest.At) {
			return false
		}
		*h = PriceHistory{}
	}
	var changed bool
	h.Samples, changed = putDaily(h.Samples, sample, func(s PriceSample) time.Time { return s.At })
	if len(h.Samples) > maxPriceSamples {
		h.Samples = slices.Delete(h.Samples, 0, len(h.Samples)-maxPriceSamples)
	}
	if !sample.At.Before(h.Latest.At) && sample != h.Latest {
		h.Latest = sample
		changed = true
	}
	if h.ObservedLowestAt.IsZero() || sample.Final < h.ObservedLowestFinal ||
		sample.Final == h.ObservedLowestFinal && sample.At.Before(h.ObservedLowestAt) {
		h.ObservedLowestImproved = !h.ObservedLowestAt.IsZero() && sample.Final < h.ObservedLowestFinal && !sample.At.Before(h.Latest.At)
		h.ObservedLowestFinal = sample.Final
		h.ObservedLowestDiscount = sample.DiscountPercent
		h.ObservedLowestAt = sample.At
		changed = true
	}
	return changed
}

// Histories returns the available histories for the requested AppIDs with one read.
func (s *PriceHistoryStore) Histories(steamID, country string, appIDs ...uint32) (map[uint32]PriceHistory, error) {
	scope, err := priceScope(steamID, country)
	if err != nil {
		return nil, err
	}
	out := make(map[uint32]PriceHistory)
	err = s.file.view(func(entries map[string]PriceHistory) {
		for _, appID := range appIDs {
			if history, ok := entries[scope+appIDText(appID)]; ok {
				history.Samples = slices.Clone(history.Samples)
				out[appID] = history
			}
		}
	})
	return out, err
}

func wishlistQuotes(list *wishList) map[uint32]PriceQuote {
	quotes := make(map[uint32]PriceQuote, len(list.Items))
	for _, item := range list.Items {
		if item.PriceChecked && item.Price != nil {
			quotes[item.AppID] = PriceQuote{
				Currency: item.Price.Currency, Final: item.Price.Final, DiscountPercent: item.Price.DiscountPercent,
			}
		}
	}
	return quotes
}

// PlaytimeSnapshot holds library-wide minutes and counts from a complete library observation.
type PlaytimeSnapshot struct {
	At             time.Time `json:"at"`
	TotalMinutes   uint64    `json:"total_minutes"`
	TwoWeekMinutes uint64    `json:"two_week_minutes"`
	OwnedGames     uint32    `json:"owned_games"`
	UnplayedGames  uint32    `json:"unplayed_games"`
}

func snapshotPlaytime(list *gameList, at time.Time) PlaytimeSnapshot {
	snapshot := PlaytimeSnapshot{At: at, OwnedGames: uint32(list.Total)}
	for _, game := range list.Games {
		snapshot.TotalMinutes += uint64(game.PlaytimeForever)
		snapshot.TwoWeekMinutes += uint64(game.Playtime2Weeks)
		if game.PlaytimeForever == 0 {
			snapshot.UnplayedGames++
		}
	}
	return snapshot
}

type PlaytimeSnapshotStore struct {
	file   *snapshotFile[[]PlaytimeSnapshot]
	recaps *snapshotFile[[]recapSnapshot]
}

func NewPlaytimeSnapshotStore(dataDir string) *PlaytimeSnapshotStore {
	return &PlaytimeSnapshotStore{
		file:   newSnapshotFile[[]PlaytimeSnapshot](dataDir, "playtime"),
		recaps: newSnapshotFile[[]recapSnapshot](dataDir, "playtime-recap"),
	}
}

// Record keeps one snapshot per UTC day within 400 days of the latest observation.
func (s *PlaytimeSnapshotStore) Record(steamID string, snapshot PlaytimeSnapshot) error {
	snapshot.At = snapshot.At.UTC()
	if !validSteamID(steamID) || !validSnapshotTime(snapshot.At) || snapshot.UnplayedGames > snapshot.OwnedGames {
		return errors.New("invalid Steam playtime snapshot")
	}
	return s.file.update(func(entries map[string][]PlaytimeSnapshot) (bool, error) {
		history, changed := putDaily(entries[steamID], snapshot, func(p PlaytimeSnapshot) time.Time { return p.At })
		if !changed {
			return false, nil
		}
		cutoff := utcDay(history[len(history)-1].At).AddDate(0, 0, 1-maxPlaytimeDays)
		first, _ := slices.BinarySearchFunc(history, cutoff, func(p PlaytimeSnapshot, day time.Time) int {
			return utcDay(p.At).Compare(day)
		})
		entries[steamID] = slices.Delete(history, 0, first)
		trimOldest(entries, maxPlaytimeAccounts, func(history []PlaytimeSnapshot) time.Time {
			if len(history) == 0 {
				return time.Time{}
			}
			return history[len(history)-1].At
		})
		return true, nil
	})
}

// History returns daily snapshots ordered from oldest to newest.
func (s *PlaytimeSnapshotStore) History(steamID string) ([]PlaytimeSnapshot, error) {
	if !validSteamID(steamID) {
		return nil, errors.New("playtime history requires a SteamID64")
	}
	var out []PlaytimeSnapshot
	err := s.file.view(func(entries map[string][]PlaytimeSnapshot) { out = slices.Clone(entries[steamID]) })
	return out, err
}

type recapSnapshot struct {
	At       time.Time                `json:"at"`
	Minutes  map[uint32]uint32        `json:"minutes"`
	Statuses map[uint32]BacklogStatus `json:"statuses"`
}

// All calendar recap periods start at a month boundary. Keep each month's first
// observed day and the latest observed day, in a separate file so overview reads
// keep using the small aggregate history. The first recap on a UTC day writes once.
func (s *PlaytimeSnapshotStore) observeRecap(id string, current recapSnapshot) ([]recapSnapshot, error) {
	var history []recapSnapshot
	err := s.recaps.update(func(entries map[string][]recapSnapshot) (bool, error) {
		prior := entries[id]
		cutoff := utcDay(current.At).AddDate(0, 0, 1-maxPlaytimeDays)
		first := slices.IndexFunc(prior, func(p recapSnapshot) bool { return !p.At.Before(cutoff) })
		if first >= 0 {
			prior = prior[first:]
		} else {
			prior = nil
		}
		history = prior
		n := len(prior)
		if n > 0 && !utcDay(current.At).After(utcDay(prior[n-1].At)) {
			return false, nil
		}
		if n > 1 && monthStart(prior[n-2].At).Equal(monthStart(prior[n-1].At)) {
			prior = prior[:n-1]
		}
		entries[id] = append(slices.Clone(prior), current)
		trimRecapHistory(entries)
		return true, nil
	})
	return history, err
}

func trimRecapHistory(entries map[string][]recapSnapshot) {
	latest := func(history []recapSnapshot) time.Time {
		if len(history) == 0 {
			return time.Time{}
		}
		return history[len(history)-1].At
	}
	trimOldest(entries, maxPlaytimeAccounts, latest)
	// Budget the JSON cost at up to 26 bytes for each integer item or status.
	// This leaves room for the enclosing records within the 16 MiB store limit.
	const maxItems = 600_000
	costs := make(map[string]int, len(entries))
	total := 0
	for id, history := range entries {
		for _, p := range history {
			costs[id] += len(p.Minutes) + len(p.Statuses)
		}
		total += costs[id]
	}
	for total > maxItems {
		oldest := slices.MinFunc(slices.Collect(maps.Keys(entries)), func(a, b string) int {
			if order := latest(entries[a]).Compare(latest(entries[b])); order != 0 {
				return order
			}
			return cmp.Compare(a, b)
		})
		total -= costs[oldest]
		delete(entries, oldest)
	}
}

type BacklogStatus string

const (
	BacklogUnplayed  BacklogStatus = "unplayed"
	BacklogPlaying   BacklogStatus = "playing"
	BacklogQueued    BacklogStatus = "queued"
	BacklogPaused    BacklogStatus = "paused"
	BacklogCompleted BacklogStatus = "completed"
	BacklogDropped   BacklogStatus = "dropped"
)

var backlogStatuses = [...]BacklogStatus{
	BacklogUnplayed, BacklogPlaying, BacklogQueued, BacklogPaused, BacklogCompleted, BacklogDropped,
}

type BacklogEntry struct {
	Status BacklogStatus `json:"status"`
	Note   string        `json:"note,omitempty"`
	Tags   []string      `json:"tags,omitempty"`
}

// BacklogStore writes personal annotations to the extension's local data directory.
type BacklogStore struct{ file *snapshotFile[BacklogEntry] }

func NewBacklogStore(dataDir string) *BacklogStore {
	return &BacklogStore{newSnapshotFile[BacklogEntry](dataDir, "backlog")}
}

func backlogKey(steamID string, appID uint32) (string, error) {
	if !validSteamID(steamID) || appID == 0 {
		return "", errors.New("backlog requires a SteamID64 and AppID")
	}
	return steamID + "/" + appIDText(appID), nil
}

func validateBacklogEntry(entry BacklogEntry) error {
	if !slices.Contains(backlogStatuses[:], entry.Status) {
		return errors.New("invalid Steam backlog status")
	}
	if !utf8.ValidString(entry.Note) || utf8.RuneCountInString(entry.Note) > maxBacklogNoteRunes {
		return fmt.Errorf("backlog note must use valid UTF-8 and at most %d characters", maxBacklogNoteRunes)
	}
	if len(entry.Tags) > maxBacklogTags {
		return fmt.Errorf("backlog allows at most %d tags", maxBacklogTags)
	}
	for _, tag := range entry.Tags {
		if strings.TrimSpace(tag) == "" || !utf8.ValidString(tag) || utf8.RuneCountInString(tag) > maxBacklogTagRunes {
			return fmt.Errorf("backlog tags must be nonblank UTF-8 strings up to %d characters", maxBacklogTagRunes)
		}
	}
	return nil
}

func (s *BacklogStore) Set(steamID string, appID uint32, entry BacklogEntry) error {
	if err := validateBacklogEntry(entry); err != nil {
		return err
	}
	return s.edit(steamID, appID, func(BacklogEntry) (*BacklogEntry, error) { return &entry, nil })
}

// edit applies a partial change against the latest entry under the store's writer lock.
// Returning a nil entry clears the annotation.
func (s *BacklogStore) edit(steamID string, appID uint32, change func(BacklogEntry) (*BacklogEntry, error)) error {
	key, err := backlogKey(steamID, appID)
	if err != nil {
		return err
	}
	return s.file.update(func(entries map[string]BacklogEntry) (bool, error) {
		current, exists := entries[key]
		next, err := change(current)
		if err != nil {
			return false, err
		}
		if next == nil {
			delete(entries, key)
			return exists, nil
		}
		if err := validateBacklogEntry(*next); err != nil {
			return false, err
		}
		if !exists && len(entries) >= maxBacklogEntries {
			return false, errors.New("Steam backlog store is full")
		}
		entry := *next
		entry.Tags = slices.Clone(entry.Tags)
		entries[key] = entry
		return true, nil
	})
}

func (s *BacklogStore) Get(steamID string, appID uint32) (BacklogEntry, bool, error) {
	key, err := backlogKey(steamID, appID)
	if err != nil {
		return BacklogEntry{}, false, err
	}
	var entry BacklogEntry
	var exists bool
	err = s.file.view(func(entries map[string]BacklogEntry) {
		entry, exists = entries[key]
		entry.Tags = slices.Clone(entry.Tags)
	})
	return entry, exists, err
}

func (s *BacklogStore) List(steamID string) (map[uint32]BacklogEntry, error) {
	if !validSteamID(steamID) {
		return nil, errors.New("backlog requires a SteamID64")
	}
	out := make(map[uint32]BacklogEntry)
	err := s.file.view(func(entries map[string]BacklogEntry) {
		for key, entry := range entries {
			if id, ok := strings.CutPrefix(key, steamID+"/"); ok {
				if appID, err := strconv.ParseUint(id, 10, 32); err == nil && appID != 0 {
					entry.Tags = slices.Clone(entry.Tags)
					out[uint32(appID)] = entry
				}
			}
		}
	})
	return out, err
}

func (s *BacklogStore) Delete(steamID string, appID uint32) error {
	return s.edit(steamID, appID, func(BacklogEntry) (*BacklogEntry, error) { return nil, nil })
}

// EventCursor identifies a successful delivery. Window can be a sale window or an update version.
type EventCursor struct {
	Window string
	SentAt time.Time
}

type eventCursorRecord struct {
	SentAt   time.Time           `json:"sent_at,omitzero"`
	SeenAt   time.Time           `json:"seen_at,omitzero"`
	Delivery *wishlistDelivery   `json:"delivery,omitempty"`
	Wishlist *wishlistWatchState `json:"wishlist,omitempty"`
}

func (r *eventCursorRecord) UnmarshalJSON(raw []byte) error {
	// Slice 4 stored only the delivery timestamp under each cursor key.
	if len(raw) > 0 && raw[0] == '"' {
		return json.Unmarshal(raw, &r.SentAt)
	}
	type record eventCursorRecord
	return json.Unmarshal(raw, (*record)(r))
}

func (r eventCursorRecord) lastSeen() time.Time {
	if r.SeenAt.After(r.SentAt) {
		return r.SeenAt
	}
	return r.SentAt
}

type EventCursorStore struct {
	file *snapshotFile[eventCursorRecord]
}

func NewEventCursorStore(dataDir string) *EventCursorStore {
	return &EventCursorStore{newSnapshotFile[eventCursorRecord](dataDir, "events")}
}

func eventCursorKey(steamID, kind, dedupKey, window string) (string, error) {
	if !validSteamID(steamID) || kind == "" || len(kind) > 64 || dedupKey == "" || len(dedupKey) > 256 || len(window) > 128 {
		return "", errors.New("invalid Steam event cursor key")
	}
	for _, part := range []string{kind, dedupKey, window} {
		if !utf8.ValidString(part) || strings.ContainsRune(part, 0) {
			return "", errors.New("invalid Steam event cursor key")
		}
	}
	return strings.Join([]string{steamID, kind, dedupKey, window}, "\x00"), nil
}

// Lookup returns a still-active delivery for the requested window or version. An empty window
// represents an event deduplicated by key alone.
func (s *EventCursorStore) Lookup(steamID, kind, dedupKey, window string, now time.Time) (EventCursor, bool, error) {
	key, err := eventCursorKey(steamID, kind, dedupKey, window)
	if err != nil {
		return EventCursor{}, false, err
	}
	var cursor EventCursor
	var seen bool
	err = s.file.view(func(entries map[string]eventCursorRecord) {
		if record, ok := entries[key]; ok && now.Before(record.lastSeen().Add(eventCursorTTL)) {
			cursor, seen = EventCursor{Window: window, SentAt: record.SentAt}, true
		}
	})
	return cursor, seen, err
}

// MarkSent records a delivery and removes expired cursors using its timestamp as the clock.
func (s *EventCursorStore) MarkSent(steamID, kind, dedupKey string, cursor EventCursor) error {
	key, err := eventCursorKey(steamID, kind, dedupKey, cursor.Window)
	if err != nil {
		return err
	}
	at := cursor.SentAt.UTC()
	if !validSnapshotTime(at) {
		return errors.New("Steam event cursor requires a sent timestamp")
	}
	return s.file.update(func(entries map[string]eventCursorRecord) (bool, error) {
		changed := pruneEventCursors(entries, at)
		record := entries[key]
		if !at.After(record.SentAt) {
			return changed, nil
		}
		record.SentAt = at
		entries[key] = record
		trimOldest(entries, maxEventCursors, eventCursorRecord.lastSeen)
		return true, nil
	})
}

// Prune removes deliveries whose deduplication TTL has elapsed.
func (s *EventCursorStore) Prune(now time.Time) error {
	return s.file.update(func(entries map[string]eventCursorRecord) (bool, error) {
		return pruneEventCursors(entries, now), nil
	})
}

func pruneEventCursors(entries map[string]eventCursorRecord, now time.Time) bool {
	changed := false
	for key, record := range entries {
		if !now.Before(record.lastSeen().Add(eventCursorTTL)) {
			delete(entries, key)
			changed = true
		} else if record.Delivery != nil && !now.Before(record.Delivery.Until) {
			record.Delivery = nil
			entries[key] = record
			changed = true
		}
	}
	return changed
}
