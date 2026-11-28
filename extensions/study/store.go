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
	srcLogseq     = "logseq"     // a page or journal in the Logseq graph on the chosen computer
	srcTutor      = "tutor"      // a reinforcement card drafted after a practice mistake or for a weak topic
	srcGuide      = "guide"      // a card drafted from a guided lesson
)

const (
	stateVersion = 1
	maxCards     = 20000
	maxDrafts    = 400
	maxReviews   = 20000
	maxFront     = 600
	maxBack      = 2000
	maxTopic     = 80
	maxAttempts  = 5000
	maxExercises = 100
	maxGuides    = 100
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
	Topic     string    `json:"topic,omitempty"` // the knowledge point the card belongs to; empty falls back to the source title
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
	Topic   string    `json:"topic,omitempty"`
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

// Attempt is one answer to a practice exercise, kept so mastery can weigh recent practice beside FSRS reviews.
type Attempt struct {
	Exercise string    `json:"exercise"`
	Card     string    `json:"card,omitempty"`
	Topic    string    `json:"topic"`
	Kind     string    `json:"kind"`
	Verdict  string    `json:"verdict"` // correct, partial or wrong
	Cause    string    `json:"cause,omitempty"`
	Hints    int       `json:"hints,omitempty"`
	Rated    bool      `json:"rated,omitempty"` // also recorded as the card's FSRS review, so counted once
	At       time.Time `json:"at"`
}

type state struct {
	Version    int                  `json:"version"`
	Cards      map[string]*Card     `json:"cards"`
	Drafts     []Draft              `json:"drafts"`
	Reviews    []Review             `json:"reviews"`
	Attempts   []Attempt            `json:"attempts,omitempty"`
	Exercises  map[string]*Exercise `json:"exercises,omitempty"`
	Guides     map[string]*Guide    `json:"guides,omitempty"`      // guided lessons, a cache like Exercises (0.3.0)
	LastRemind string               `json:"last_remind,omitempty"` // YYYY-MM-DD of the last daily reminder check
}

func emptyState() state {
	return state{Version: stateVersion, Cards: map[string]*Card{}, Drafts: []Draft{}, Reviews: []Review{},
		Attempts: []Attempt{}, Exercises: map[string]*Exercise{}, Guides: map[string]*Guide{}}
}

// clone deep-copies everything a later update may change in place, so a failed write leaves the old state whole.
func (s state) clone() state {
	out := state{Version: s.Version, LastRemind: s.LastRemind, Cards: make(map[string]*Card, len(s.Cards)),
		Drafts: append([]Draft(nil), s.Drafts...), Reviews: append([]Review(nil), s.Reviews...),
		Attempts: append([]Attempt(nil), s.Attempts...), Exercises: make(map[string]*Exercise, len(s.Exercises)),
		Guides: make(map[string]*Guide, len(s.Guides))}
	for id, c := range s.Cards {
		cp := *c
		out.Cards[id] = &cp
	}
	for id, e := range s.Exercises {
		cp := e.clone()
		out.Exercises[id] = &cp
	}
	for id, g := range s.Guides {
		cp := g.clone()
		out.Guides[id] = &cp
	}
	if out.Drafts == nil {
		out.Drafts = []Draft{}
	}
	if out.Reviews == nil {
		out.Reviews = []Review{}
	}
	if out.Attempts == nil {
		out.Attempts = []Attempt{}
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
	if s.Attempts == nil {
		s.Attempts = []Attempt{}
	}
	if s.Exercises == nil {
		s.Exercises = map[string]*Exercise{}
	}
	if s.Guides == nil {
		s.Guides = map[string]*Guide{} // a state written before 0.3.0 has none
	}
	for id, c := range s.Cards {
		if c == nil || c.ID != id {
			return state{}, errors.New("复习记录里有无效的卡片")
		}
	}
	for id, e := range s.Exercises {
		if e == nil || e.ID != id || !e.valid() {
			delete(s.Exercises, id) // an exercise is a cache: one that does not hold together is dropped, not trusted
		}
	}
	for id, g := range s.Guides {
		if g == nil || g.ID != id || !g.valid() || !g.tidy() {
			delete(s.Guides, id) // a lesson is a cache too
		}
	}
	for len(s.Guides) > maxGuides {
		delete(s.Guides, s.oldestGuide())
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
func (s *state) addCard(front, back, topic string, src Source, now time.Time) (*Card, bool) {
	front, back = clip(strings.TrimSpace(front), maxFront), clip(strings.TrimSpace(back), maxBack)
	if front == "" || back == "" || len(s.Cards) >= maxCards {
		return nil, false
	}
	id := cardID(src, front)
	if old, ok := s.Cards[id]; ok {
		return old, false
	}
	c := &Card{ID: id, Front: front, Back: back, Topic: cleanTopic(topic), Source: src, Created: now.UTC(), FSRS: fsrs.NewCard(now.UTC())}
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
		s.Drafts = append(s.Drafts, Draft{ID: id, Front: front, Back: back, Topic: cleanTopic(it.Topic), Source: src, Created: now.UTC()})
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
			if _, ok := s.addCard(d.Front, d.Back, d.Topic, d.Source, now); ok {
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

func (s *state) logAttempt(at Attempt) {
	s.Attempts = append(s.Attempts, at)
	if over := len(s.Attempts) - maxAttempts; over > 0 {
		s.Attempts = append([]Attempt(nil), s.Attempts[over:]...)
	}
}

// putExercise stores an exercise, dropping the oldest beyond maxExercises.
func (s *state) putExercise(e Exercise) {
	if s.Exercises == nil {
		s.Exercises = map[string]*Exercise{}
	}
	s.Exercises[e.ID] = &e
	for len(s.Exercises) > maxExercises {
		oldest := ""
		for id, x := range s.Exercises {
			if oldest == "" || x.Created.Before(s.Exercises[oldest].Created) || x.Created.Equal(s.Exercises[oldest].Created) && id < oldest {
				oldest = id
			}
		}
		delete(s.Exercises, oldest)
	}
}

// putGuide stores a lesson, dropping the oldest beyond maxGuides.
func (s *state) putGuide(g Guide) {
	if s.Guides == nil {
		s.Guides = map[string]*Guide{}
	}
	s.Guides[g.ID] = &g
	// The lesson just stored stays, whatever the clock said when the others were made.
	for len(s.Guides) > maxGuides {
		delete(s.Guides, s.oldestGuideBut(g.ID))
	}
}

func (s *state) oldestGuide() string { return s.oldestGuideBut("") }

func (s *state) oldestGuideBut(keep string) string {
	oldest := ""
	for id, x := range s.Guides {
		if id == keep {
			continue
		}
		if oldest == "" || x.Created.Before(s.Guides[oldest].Created) || x.Created.Equal(s.Guides[oldest].Created) && id < oldest {
			oldest = id
		}
	}
	return oldest
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

// cleanTopic keeps a knowledge-point name to one short line.
func cleanTopic(t string) string { return clip(strings.Join(strings.Fields(t), " "), maxTopic) }

// topicOf is the knowledge point a card counts toward: its own topic, else its source's title.
func topicOf(c *Card) string {
	if c.Topic != "" {
		return c.Topic
	}
	if t := cleanTopic(c.Source.Title); t != "" {
		return t
	}
	return untitledTopic
}

const untitledTopic = "未归类"

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:max(0, n-1)]) + "…"
}
