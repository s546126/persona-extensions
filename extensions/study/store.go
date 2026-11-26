package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	fsrs "github.com/open-spaced-repetition/go-fsrs/v4"
)

// Source kinds: where a card's material came from.
const (
	srcTopic      = "topic"      // a Persona knowledge-base topic
	srcNotion     = "notion"     // a Notion page shared with the integration
	srcObsidian   = "obsidian"   // a note in the Obsidian vault on the chosen computer
	srcNotebookLM = "notebooklm" // a NotebookLM notebook, read through a community command line
	srcManual     = "manual"     // text handed over in a conversation
)

const (
	stateVersion = 1
	maxCards     = 20000
	maxDrafts    = 400
	maxReviews   = 20000
	maxFront     = 600
	maxBack      = 2000
)

// Source says where a card or draft came from.
type Source struct {
	Kind  string `json:"kind"`
	Ref   string `json:"ref,omitempty"`   // the topic id, Notion page id, vault-relative path or notebook id
	Title string `json:"title,omitempty"` // a name to show
	URL   string `json:"url,omitempty"`
}

// Card is one question and answer with its FSRS schedule.
type Card struct {
	ID        string    `json:"id"`
	Front     string    `json:"front"`
	Back      string    `json:"back"`
	Source    Source    `json:"source"`
	Created   time.Time `json:"created"`
	Suspended bool      `json:"suspended,omitempty"`
	FSRS      fsrs.Card `json:"fsrs"`
}

// Draft is a generated card the user has not kept yet.
type Draft struct {
	ID      string    `json:"id"`
	Front   string    `json:"front"`
	Back    string    `json:"back"`
	Source  Source    `json:"source"`
	Created time.Time `json:"created"`
}

// Review is one entry of the append-only review log, kept so the schedule can be replayed or refitted later.
type Review struct {
	Card   string      `json:"card"`
	Rating fsrs.Rating `json:"rating"`
	At     time.Time   `json:"at"`
	WasNew bool        `json:"was_new,omitempty"`
	Due    time.Time   `json:"due"` // the next due time the review set
}

type state struct {
	Version    int              `json:"version"`
	Cards      map[string]*Card `json:"cards"`
	Drafts     []Draft          `json:"drafts"`
	Reviews    []Review         `json:"reviews"`
	LastRemind string           `json:"last_remind,omitempty"` // YYYY-MM-DD of the last daily reminder check
}

func emptyState() state {
	return state{Version: stateVersion, Cards: map[string]*Card{}, Drafts: []Draft{}, Reviews: []Review{}}
}

func (s state) clone() state {
	out := state{Version: s.Version, LastRemind: s.LastRemind, Cards: make(map[string]*Card, len(s.Cards)),
		Drafts: append([]Draft(nil), s.Drafts...), Reviews: append([]Review(nil), s.Reviews...)}
	for id, c := range s.Cards {
		cp := *c
		out.Cards[id] = &cp
	}
	if out.Drafts == nil {
		out.Drafts = []Draft{}
	}
	if out.Reviews == nil {
		out.Reviews = []Review{}
	}
	return out
}

func loadState(path string) (state, error) {
	if path == "" {
		return emptyState(), nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyState(), nil
	}
	if err != nil {
		return state{}, err
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		return state{}, errors.New("复习记录文件损坏，没法读")
	}
	if s.Version > stateVersion {
		return state{}, errors.New("复习记录来自更新的版本，先升级扩展")
	}
	if s.Cards == nil {
		s.Cards = map[string]*Card{}
	}
	if s.Drafts == nil {
		s.Drafts = []Draft{}
	}
	if s.Reviews == nil {
		s.Reviews = []Review{}
	}
	for id, c := range s.Cards {
		if c == nil || c.ID != id {
			return state{}, errors.New("复习记录里有无效的卡片")
		}
	}
	s.Version = stateVersion
	return s, nil
}

// saveState writes atomically: a temporary file in the same directory, then a rename.
func saveState(path string, s state) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".study-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// cardID is stable for the same source and question, so importing a note twice does not duplicate its cards.
func cardID(src Source, front string) string {
	h := sha256.Sum256([]byte(src.Kind + "\x00" + src.Ref + "\x00" + normalize(front)))
	return hex.EncodeToString(h[:8])
}

func normalize(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

// addCard adds a new card; it reports false when the card already exists or the collection is full.
func (s *state) addCard(front, back string, src Source, now time.Time) (*Card, bool) {
	front, back = clip(strings.TrimSpace(front), maxFront), clip(strings.TrimSpace(back), maxBack)
	if front == "" || back == "" || len(s.Cards) >= maxCards {
		return nil, false
	}
	id := cardID(src, front)
	if old, ok := s.Cards[id]; ok {
		return old, false
	}
	c := &Card{ID: id, Front: front, Back: back, Source: src, Created: now.UTC(), FSRS: fsrs.NewCard(now.UTC())}
	s.Cards[id] = c
	return c, true
}

// addDrafts appends generated drafts, skipping questions that already are cards or drafts.
func (s *state) addDrafts(items []cardText, src Source, now time.Time) int {
	seen := map[string]bool{}
	for _, d := range s.Drafts {
		seen[cardID(d.Source, d.Front)] = true
	}
	n := 0
	for _, it := range items {
		front, back := clip(strings.TrimSpace(it.Front), maxFront), clip(strings.TrimSpace(it.Back), maxBack)
		if front == "" || back == "" || len(s.Drafts) >= maxDrafts {
			continue
		}
		id := cardID(src, front)
		if seen[id] || s.Cards[id] != nil {
			continue
		}
		seen[id] = true
		s.Drafts = append(s.Drafts, Draft{ID: id, Front: front, Back: back, Source: src, Created: now.UTC()})
		n++
	}
	return n
}

// keepDraft turns a draft into a card; ids empty keeps them all.
func (s *state) keepDrafts(ids []string, now time.Time) int {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	kept, rest := 0, s.Drafts[:0:0]
	for _, d := range s.Drafts {
		if len(ids) == 0 || want[d.ID] {
			if _, ok := s.addCard(d.Front, d.Back, d.Source, now); ok {
				kept++
			}
			continue
		}
		rest = append(rest, d)
	}
	s.Drafts = rest
	return kept
}

func (s *state) dropDrafts(ids []string) int {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	n, rest := 0, s.Drafts[:0:0]
	for _, d := range s.Drafts {
		if len(ids) == 0 || want[d.ID] {
			n++
			continue
		}
		rest = append(rest, d)
	}
	s.Drafts = rest
	return n
}

func (s *state) logReview(r Review) {
	s.Reviews = append(s.Reviews, r)
	if over := len(s.Reviews) - maxReviews; over > 0 {
		s.Reviews = append([]Review(nil), s.Reviews[over:]...)
	}
}

// sortedCards lists cards, newest first.
func (s state) sortedCards() []*Card {
	out := make([]*Card, 0, len(s.Cards))
	for _, c := range s.Cards {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:max(0, n-1)]) + "…"
}
