package main

import (
	"regexp"
	"strings"
	"unicode"
)

// Telling an English prompt from one in another language, by the scripts its letters are in. Only scripts that are
// not Latin can be told apart this way; a prompt in Spanish or German reads as English here, on purpose: telling
// those apart needs word lists, and a wrong block costs more than a missed one.

// script is one writing system the detector counts, and how much one of its characters weighs against one Latin
// letter. A Han character or a kana or Hangul syllable carries about as much as a short English word, so it
// counts as several letters; an alphabet's letter counts as one.
type script struct {
	lang   string
	weight int
	lo, hi rune
}

var scripts = []script{
	{"zh", 3, 0x3400, 0x4dbf}, // CJK extension A
	{"zh", 3, 0x4e00, 0x9fff}, // CJK unified ideographs
	{"zh", 3, 0xf900, 0xfaff}, // CJK compatibility ideographs
	{"ja", 3, 0x3040, 0x309f}, // hiragana
	{"ja", 3, 0x30a0, 0x30ff}, // katakana
	{"ko", 3, 0xac00, 0xd7af}, // Hangul syllables
	{"ru", 1, 0x0400, 0x04ff}, // Cyrillic
	{"he", 1, 0x0590, 0x05ff}, // Hebrew
	{"ar", 1, 0x0600, 0x06ff}, // Arabic
	{"hi", 1, 0x0900, 0x097f}, // Devanagari
	{"th", 1, 0x0e00, 0x0e7f}, // Thai
}

// minForeign is how many characters of another script a prompt needs before it can count as not English, and
// foreignShare how large a part of its weighted letters they must be. A share, not only a count: an English
// prompt that names a file or a UI string in Chinese stays English.
const (
	minForeign   = 2
	foreignShare = 0.3
)

var (
	fencedCode = regexp.MustCompile("(?s)```.*?```")
	inlineCode = regexp.MustCompile("`[^`\n]*`")
)

// stripQuoted removes fenced and inline code: pasted logs, code and localized strings are not the user writing in
// another language.
func stripQuoted(s string) string {
	return inlineCode.ReplaceAllString(fencedCode.ReplaceAllString(s, " "), " ")
}

func scriptOf(r rune) (script, bool) {
	for _, s := range scripts {
		if r >= s.lo && r <= s.hi {
			return s, true
		}
	}
	return script{}, false
}

// analysis is what the detector found in a prompt.
type analysis struct {
	Foreign  int     // characters in a script of scripts
	Latin    int     // Latin letters a to z
	Share    float64 // the foreign characters' weighted share of all letters
	Language string  // the most likely language of the foreign text, "" when there is none
}

func analyze(text string) analysis {
	var a analysis
	var foreignWeight int
	seen := map[string]bool{}
	for _, r := range stripQuoted(text) {
		if s, ok := scriptOf(r); ok {
			a.Foreign++
			foreignWeight += s.weight
			seen[s.lang] = true
			continue
		}
		if r < unicode.MaxASCII && unicode.IsLetter(r) {
			a.Latin++
		}
	}
	if total := foreignWeight + a.Latin; total > 0 {
		a.Share = float64(foreignWeight) / float64(total)
	}
	// Han characters are shared by Chinese and Japanese: kana decide it.
	switch {
	case seen["ja"]:
		a.Language = "ja"
	case seen["zh"]:
		a.Language = "zh"
	default:
		for _, s := range scripts {
			if seen[s.lang] {
				a.Language = s.lang
				break
			}
		}
	}
	return a
}

// isNonEnglish says whether the prompt is written in another language.
func isNonEnglish(text string) bool {
	a := analyze(text)
	return a.Foreign >= minForeign && a.Share >= foreignShare
}

// detectLanguage is the language of the prompt's foreign text, for the messages; "" when unknown.
func detectLanguage(text string) string { return analyze(text).Language }

// hasStopword says whether the prompt contains one of the words, ignoring case.
func hasStopword(text string, words []string) bool {
	lower := strings.ToLower(text)
	for _, w := range words {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" && strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// isGiveup says whether the whole prompt is one of the give-up words, ignoring case.
func isGiveup(text string, words []string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	for _, w := range words {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" && t == w {
			return true
		}
	}
	return false
}
