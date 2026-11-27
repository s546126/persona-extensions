package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	stateFile             = "reading.json"
	maxCachedTranslations = 200
)

// readingState is the extension's on-disk state: the selected mode, whether the
// extra paragraph is shown, remembered words, and translations that already came
// back from the translator.
type readingState struct {
	Mode         string               `json:"mode"`
	More         bool                 `json:"more"`
	Words        map[string]savedWord `json:"words"`
	Translations map[string]string    `json:"translations,omitempty"`
}

type savedWord struct {
	Lemma string `json:"lemma"`
	Gloss string `json:"gloss"`
	At    string `json:"saved_at"`
}

type store struct {
	mu   sync.Mutex
	path string
	logf func(string, ...any)
	readingState
}

func openStore(dir string, logf func(string, ...any)) *store {
	s := &store{logf: logf, readingState: readingState{Mode: modeTranslation, Words: map[string]savedWord{}}}
	if dir == "" {
		return s
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logf("state: %v", err)
		return s
	}
	s.path = filepath.Join(dir, stateFile)
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s
	}
	if err == nil {
		var decoded readingState
		if err = json.Unmarshal(raw, &decoded); err == nil {
			s.readingState = normalize(decoded)
			return s
		}
	}
	if rerr := os.Rename(s.path, s.path+".corrupt"); rerr != nil {
		logf("state: %s unreadable (%v), and it could not be moved aside: %v", s.path, err, rerr)
	} else {
		logf("state: %s unreadable (%v), moved aside", s.path, err)
	}
	return s
}

func normalize(in readingState) readingState {
	if in.Mode != modeVocabulary {
		in.Mode = modeTranslation
	}
	if in.Words == nil {
		in.Words = map[string]savedWord{}
	}
	lemmas := make([]string, 0, len(in.Words))
	for lemma, word := range in.Words {
		if !surfaceOK(lemma) || word.Lemma != lemma || word.Gloss == "" {
			continue
		}
		lemmas = append(lemmas, lemma)
	}
	sort.Strings(lemmas)
	if len(lemmas) > maxSavedWords {
		lemmas = lemmas[:maxSavedWords]
	}
	clean := make(map[string]savedWord, len(lemmas))
	for _, lemma := range lemmas {
		word := in.Words[lemma]
		word.Gloss = clipBytes(word.Gloss, 1800)
		if word.Gloss == "" {
			continue
		}
		clean[lemma] = word
	}
	in.Words = clean
	if in.Translations == nil {
		in.Translations = map[string]string{}
	}
	tkeys := make([]string, 0, len(in.Translations))
	for k, v := range in.Translations {
		if k == "" || v == "" {
			continue
		}
		tkeys = append(tkeys, k)
	}
	sort.Strings(tkeys)
	if len(tkeys) > maxCachedTranslations {
		tkeys = tkeys[:maxCachedTranslations]
	}
	cleanTr := make(map[string]string, len(tkeys))
	for _, k := range tkeys {
		if clipped := clipBytes(in.Translations[k], 1800); clipped != "" {
			cleanTr[k] = clipped
		}
	}
	in.Translations = cleanTr
	return in
}

func (s *store) snapshot() readingState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copy()
}

func (s *store) copy() readingState {
	out := s.readingState
	out.Words = make(map[string]savedWord, len(s.Words))
	for k, v := range s.Words {
		out.Words[k] = v
	}
	out.Translations = make(map[string]string, len(s.Translations))
	for k, v := range s.Translations {
		out.Translations[k] = v
	}
	return out
}

func (s *store) setMode(mode string) {
	if mode != modeTranslation && mode != modeVocabulary {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Mode = mode
	s.flush()
}

func (s *store) toggleMore() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.More = !s.More
	s.flush()
	return s.More
}

// save remembers lemma. A second save keeps the first record and reports already.
func (s *store) save(lemma, gloss string) (already bool) {
	gloss = clipBytes(gloss, 1800)
	if !surfaceOK(lemma) || gloss == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Words[lemma]; ok {
		return true
	}
	if len(s.Words) >= maxSavedWords {
		return false
	}
	s.Words[lemma] = savedWord{Lemma: lemma, Gloss: gloss, At: time.Now().UTC().Format(time.RFC3339)}
	s.flush()
	return false
}

// remove forgets lemma. missing is true when it was not saved.
func (s *store) remove(lemma string) (missing bool) {
	if !surfaceOK(lemma) {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Words[lemma]; !ok {
		return true
	}
	delete(s.Words, lemma)
	s.flush()
	return false
}

func (s *store) translation(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text, ok := s.Translations[key]
	return text, ok && text != ""
}

func (s *store) rememberTranslation(key, text string) {
	text = clipBytes(text, 1800)
	if key == "" || text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Translations == nil {
		s.Translations = map[string]string{}
	}
	if s.Translations[key] == text {
		return
	}
	if _, exists := s.Translations[key]; !exists && len(s.Translations) >= maxCachedTranslations {
		keys := make([]string, 0, len(s.Translations))
		for k := range s.Translations {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			delete(s.Translations, k)
			if len(s.Translations) < maxCachedTranslations {
				break
			}
		}
	}
	s.Translations[key] = text
	s.flush()
}

func (s *store) flush() {
	if s.path == "" {
		return
	}
	raw, err := json.MarshalIndent(s.readingState, "", "  ")
	if err != nil {
		s.logf("state: %v", err)
		return
	}
	raw = append(raw, '\n')
	f, err := os.CreateTemp(filepath.Dir(s.path), stateFile+".tmp-*")
	if err != nil {
		s.logf("state: %v", err)
		return
	}
	tmp := f.Name()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		s.logf("state: %v", err)
		return
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		s.logf("state: %v", err)
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		s.logf("state: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		s.logf("state: %v", err)
	}
}

func (s *store) pathDir() string {
	if s.path == "" {
		return ""
	}
	return filepath.Dir(s.path)
}
