package main

import (
	"context"
	"sort"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
)

const (
	lookupContribution = "relingo"
	maxSavedWords      = 5000
)

// settings is the configuration form. underline_saved defaults to false.
type settings struct {
	UnderlineSaved bool `json:"underline_saved"`
}

type lookupParams struct {
	Contribution string `json:"contribution"`
	Word         string `json:"word"`
	Context      string `json:"context,omitempty"`
	Source       string `json:"source,omitempty"`
	Lang         string `json:"lang"`
	UILang       string `json:"ui_lang"`
}

type sense struct {
	POS  string `json:"pos,omitempty"`
	Text string `json:"text"`
}

type lookupResult struct {
	Found    bool    `json:"found"`
	Word     string  `json:"word"`
	Lemma    string  `json:"lemma,omitempty"`
	Phonetic string  `json:"phonetic,omitempty"`
	Senses   []sense `json:"senses"`
	Saved    bool    `json:"saved"`
}

type stateResult struct {
	UnderlineSaved bool     `json:"underline_saved"`
	Words          []string `json:"words"`
}

type savedResult struct {
	Saved bool `json:"saved"`
}

func (a *app) lookupMethods() ext.Handler {
	return ext.Methods{
		"state":  ext.Method(a.lookupState),
		"lookup": ext.Method(a.lookupWord),
		"save":   ext.Method(a.lookupSave),
		"remove": ext.Method(a.lookupRemove),
	}.For(lookupContribution)
}

func (a *app) lookupState(_ context.Context, _ lookupParams) (stateResult, error) {
	return stateResult{UnderlineSaved: a.currentUnderlineSaved(), Words: a.savedLemmas()}, nil
}

func (a *app) savedLemmas() []string {
	snap := a.currentStore().snapshot()
	words := make([]string, 0, len(snap.Words))
	for lemma := range snap.Words {
		words = append(words, lemma)
	}
	sort.Strings(words)
	if len(words) > maxSavedWords {
		words = words[:maxSavedWords]
	}
	return words
}

func (a *app) lookupWord(_ context.Context, p lookupParams) (lookupResult, error) {
	out := lookupResult{Word: strings.TrimSpace(p.Word), Senses: []sense{}}
	snap := a.currentStore().snapshot()
	lemma, g, ok := lexicon.lookup(out.Word, false)
	if !ok {
		// The candidate rules (developer words, function words, short acronyms) keep a word from being offered, not
		// from being explained once it is saved: the state lists it and the apps underline it, so it answers, as the
		// reading page's underline does.
		if lem, rg, rok := lexicon.lookup(out.Word, true); rok {
			if _, saved := snap.Words[lem]; saved {
				lemma, g, ok = lem, rg, true
			}
		}
	}
	if !ok {
		return out, nil
	}
	out.Found = true
	out.Lemma = lemma
	out.Senses = sensesOf(g, p.UILang)
	_, out.Saved = snap.Words[lemma]
	return out, nil
}

func sensesOf(g gloss, uiLang string) []sense {
	text := g.text(uiLang)
	if text == "" {
		return []sense{}
	}
	pos, rest, found := strings.Cut(text, ". ")
	if found && pos != "" && !strings.Contains(pos, " ") && len(pos) <= 12 {
		return []sense{{POS: pos, Text: rest}}
	}
	return []sense{{Text: text}}
}

func (a *app) lookupSave(_ context.Context, p lookupParams) (savedResult, error) {
	lemma, g, ok := lexicon.lookup(strings.TrimSpace(p.Word), false)
	if !ok {
		return savedResult{}, nil
	}
	st := a.currentStore()
	st.save(lemma, g.text(p.UILang))
	// store.save turns away an empty gloss without saying so, so report what the store now holds.
	_, saved := st.snapshot().Words[lemma]
	return savedResult{Saved: saved}, nil
}

func (a *app) lookupRemove(_ context.Context, p lookupParams) (savedResult, error) {
	word := strings.TrimSpace(p.Word)
	// Find the saved lemma whatever the candidate rules say: a saved developer word's inflected form removes it too.
	if lemma, _, ok := lexicon.lookup(word, true); ok {
		word = lemma
	} else {
		word = foldWord(word)
	}
	st := a.currentStore()
	st.remove(word)
	// Membership, not the entry's lemma: a blank or unknown word reads the zero entry, whose empty lemma equals "".
	_, saved := st.snapshot().Words[word]
	return savedResult{Saved: saved}, nil
}
