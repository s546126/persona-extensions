package main

import (
	"regexp"
	"strings"
	"unicode"
)

// Telling an English prompt from one written in another language.
//
// The question is which language the user wrote in, not which characters the prompt contains. Developers who write
// in Chinese mix in English terms, paths and links ("把 handleRequest 的 timeout 改成 30s"), and developers who write
// in English quote strings, file names and pasted text in other languages ("Change the label "登录" to …",
// "Translate this into English:" and a paragraph). So the detector
//
//  1. sets aside what the user quoted rather than wrote: code, quotes, XML-style tags (Persona's selected text),
//     block quotes, the text after an English line that ends in a colon, and links, paths and e-mail addresses;
//  2. weighs what is left: words of a script that is not Latin against Latin words, with code-like identifiers and
//     acronyms counting half, since a Chinese sentence carries English terms;
//  3. looks at the grammar: English function words ("the", "to", "why") mean the user wrote the sentence in
//     English; with none of them, any text in another script means the user did not;
//  4. tells Latin-script languages (Spanish, French, German and others) by their function words and letters,
//     requiring clearly more of them than of English ones.
//
// testdata/detect.jsonl is the labeled corpus that measures it (corpus_test.go).

// script is one writing system that is not Latin, and how many words one of its characters is worth. Han
// characters, kana and Hangul syllables are counted one by one, as a fraction of a word; letters of an alphabet are
// counted by the word.
type script struct {
	lang   string
	lo, hi rune
	perRun bool    // an alphabet: count a run of its letters as one word
	weight float64 // otherwise: how much of a word one character is
}

var scripts = []script{
	{lang: "zh", lo: 0x3400, hi: 0x4dbf, weight: 0.6}, // CJK extension A
	{lang: "zh", lo: 0x4e00, hi: 0x9fff, weight: 0.6}, // CJK unified ideographs
	{lang: "zh", lo: 0xf900, hi: 0xfaff, weight: 0.6}, // CJK compatibility ideographs
	{lang: "ja", lo: 0x3040, hi: 0x309f, weight: 0.4}, // hiragana
	{lang: "ja", lo: 0x30a0, hi: 0x30ff, weight: 0.4}, // katakana
	{lang: "ko", lo: 0xac00, hi: 0xd7af, weight: 0.4}, // Hangul syllables
	{lang: "el", lo: 0x0370, hi: 0x03ff, perRun: true},
	{lang: "ru", lo: 0x0400, hi: 0x04ff, perRun: true}, // Cyrillic
	{lang: "hy", lo: 0x0530, hi: 0x058f, perRun: true}, // Armenian
	{lang: "he", lo: 0x0590, hi: 0x05ff, perRun: true},
	{lang: "ar", lo: 0x0600, hi: 0x06ff, perRun: true},
	{lang: "hi", lo: 0x0900, hi: 0x097f, perRun: true}, // Devanagari
	{lang: "bn", lo: 0x0980, hi: 0x09ff, perRun: true},
	{lang: "ta", lo: 0x0b80, hi: 0x0bff, perRun: true},
	{lang: "th", lo: 0x0e00, hi: 0x0e7f, perRun: true},
	{lang: "ka", lo: 0x10a0, hi: 0x10ff, perRun: true}, // Georgian
}

func scriptOf(r rune) (int, bool) {
	for i, s := range scripts {
		if r >= s.lo && r <= s.hi {
			return i, true
		}
	}
	return -1, false
}

// The thresholds, tuned on the corpus.
const (
	// foreignShare: with English grammar in the prompt, the part of its words the other script must make up.
	foreignShare = 0.4
	// latinMargin: a Latin-script language needs more than this many times the English function words.
	latinMargin = 2
	// latinMin: and at least this many of its own, unless the prompt is a short reply (shortReply words or fewer).
	latinMin   = 2
	shortReply = 3
)

var (
	fencedCode = regexp.MustCompile("(?s)```.*?(```|$)")
	inlineCode = regexp.MustCompile("`[^`\n]*`")
	// Persona carries selected text as XML tags ("AI edit", "Use as context"); other tools paste the same way.
	tagBlock = regexp.MustCompile(`(?s)<([A-Za-z][\w-]*)(\s[^<>]*)?>.*?</([A-Za-z][\w-]*)>`)
	quoted   = regexp.MustCompile(`"[^"\n]*"|“[^”\n]*”|‘[^’\n]*’|「[^」\n]*」|『[^』\n]*』|«[^»\n]*»|‹[^›\n]*›|《[^》\n]*》`)
	link     = regexp.MustCompile(`(?i)\b(?:[a-z][a-z0-9+.-]*://|www\.)\S+|\S+@\S+\.\S+`)
	// A path: a token with a slash or backslash in it, or a file name with an extension.
	path = regexp.MustCompile(`(?:[\w.~-]*[/\\][^\s,，。;；:：)）'"]+)|(?:\b[\w-]+\.(?:[a-z][a-z0-9]{0,5})\b)`)
	// A pasted log message: "error: …", "fatal: …", "panic: …" up to the end of the line or the next text in
	// another script ("error: failed to push some refs 这个怎么办").
	// A code comment: "// …" or "/* … */" to the end of the line.
	codeComment = regexp.MustCompile(`(?m)(?:^|\s)(?://|/\*)\s.*$`)
	logMessage  = regexp.MustCompile(`(?i)\b(?:error|fatal|warning|panic|exception|traceback|caused by)\s*[:(][^\p{Han}\p{Hiragana}\p{Katakana}\p{Hangul}\p{Cyrillic}\n]*`)
)

// authored is the part of the prompt the user wrote, with what they quoted set aside.
func authored(text string) string {
	s := fencedCode.ReplaceAllString(text, "\n")
	s = inlineCode.ReplaceAllString(s, " ")
	s = removeTagBlocks(s)
	s = quoted.ReplaceAllString(s, " ")

	var keep []string
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, ">") || isCodeLine(t) {
			continue // a block quote, or code pasted without a fence
		}
		keep = append(keep, line)
		// "Translate this into English:" and what follows: the English line is the user's, the rest is material.
		if strings.HasSuffix(t, ":") || strings.HasSuffix(t, "：") {
			if a := weigh(t); a.Foreign == 0 && a.Grammar >= 1 && a.Words >= 2 {
				break
			}
		}
	}
	s = strings.Join(keep, "\n")
	s = codeComment.ReplaceAllString(s, " ")
	s = logMessage.ReplaceAllString(s, " ")
	s = link.ReplaceAllString(s, " ")
	return path.ReplaceAllString(s, " ")
}

// isCodeLine says whether a line reads as code rather than prose: it ends like a statement or block, or is dense
// with brackets and operators. A line with text in another script is never code here: it is the user writing.
func isCodeLine(t string) bool {
	if t == "" {
		return false
	}
	symbols, letters := 0, 0
	for _, r := range t {
		if _, ok := scriptOf(r); ok {
			return false
		}
		switch {
		case strings.ContainsRune("{}[]();=<>|&+*", r):
			symbols++
		case unicode.IsLetter(r):
			letters++
		}
	}
	switch t[len(t)-1] {
	case '{', '}', ';':
		return true
	}
	return symbols >= 2 && float64(symbols) >= 0.15*float64(letters)
}

// removeTagBlocks removes <tag …>…</tag> blocks whose closing tag matches the opening one, innermost first.
func removeTagBlocks(s string) string {
	for range 8 {
		changed := false
		s = tagBlock.ReplaceAllStringFunc(s, func(m string) string {
			sub := tagBlock.FindStringSubmatch(m)
			if sub[1] != sub[3] {
				return m
			}
			changed = true
			return " "
		})
		if !changed {
			break
		}
	}
	return s
}

// analysis is what the detector found in the user's own words.
type analysis struct {
	Foreign  float64 // words' worth of text in a script that is not Latin
	Words    float64 // Latin words, identifiers and acronyms counting half
	Grammar  int     // English function words
	Latin    int     // function words of the likeliest Latin-script language that is not English
	Accented int     // Latin words with letters English does not use
	Language string  // the most likely language of the text that is not English, "" when there is none
}

func analyze(text string) analysis { return weigh(authored(text)) }

// weigh counts the words of s.
func weigh(s string) analysis {
	var a analysis
	seen := make([]float64, len(scripts))
	latin := map[string]int{}

	runes := []rune(s)
	for i := 0; i < len(runes); {
		r := runes[i]
		if k, ok := scriptOf(r); ok {
			j := i
			for j < len(runes) {
				if k2, ok := scriptOf(runes[j]); !ok || scripts[k2].lang != scripts[k].lang {
					break
				}
				j++
			}
			if scripts[k].perRun {
				seen[k]++
				a.Foreign++
			} else {
				for _, rr := range runes[i:j] {
					k2, _ := scriptOf(rr)
					seen[k2] += scripts[k2].weight
					a.Foreign += scripts[k2].weight
				}
			}
			i = j
			continue
		}
		if isLatinLetter(r) {
			j := i
			for j < len(runes) && (isLatinLetter(runes[j]) || isWordInner(runes, j)) {
				j++
			}
			a.addLatinWord(string(runes[i:j]), latin)
			i = j
			continue
		}
		if unicode.IsDigit(r) || r == '_' {
			// a token that starts with a digit or underscore: a number or an identifier, not a word
			j := i
			for j < len(runes) && (isLatinLetter(runes[j]) || unicode.IsDigit(runes[j]) || runes[j] == '_') {
				j++
			}
			i = j
			continue
		}
		i++
	}

	best := ""
	for lang, n := range latin {
		if n > a.Latin || (n == a.Latin && lang < best) {
			a.Latin, best = n, lang
		}
	}
	var most float64
	for k, w := range seen {
		lang := scripts[k].lang
		if lang == "zh" && kanaIn(seen) {
			continue // Han characters are shared by Chinese and Japanese: kana decide it
		}
		if w > most {
			most, a.Language = w, lang
		}
	}
	if a.Language == "" && best != "" {
		a.Language = best
	}
	return a
}

func kanaIn(seen []float64) bool {
	for k, w := range seen {
		if w > 0 && scripts[k].lang == "ja" {
			return true
		}
	}
	return false
}

func isLatinLetter(r rune) bool {
	return r < 0x250 && unicode.IsLetter(r) || r >= 0x1e00 && r <= 0x1eff
}

// isWordInner says whether the rune at i joins two parts of one word: an apostrophe, a hyphen, a digit or an
// underscore between letters ("can't", "c'est", "vas-y", "zh-Hans", "de_id", "utf8").
func isWordInner(runes []rune, i int) bool {
	r := runes[i]
	if r != '\'' && r != '’' && r != '-' && r != '_' && !unicode.IsDigit(r) {
		return false
	}
	return i+1 < len(runes) && (isLatinLetter(runes[i+1]) || unicode.IsDigit(runes[i+1]) || runes[i+1] == '_')
}

// addLatinWord counts one Latin word: as English grammar, as a function word of another Latin-script language, as
// an identifier or acronym, or as a plain word.
func (a *analysis) addLatinWord(w string, latin map[string]int) {
	if isIdentifier(w) {
		a.Words += 0.5
		return
	}
	a.Words++
	lw := strings.ToLower(strings.ReplaceAll(w, "’", "'"))
	accented := false
	for _, r := range lw {
		if r > unicode.MaxASCII {
			accented = true
			break
		}
	}
	if accented {
		a.Accented++
	}
	if englishWords[lw] {
		a.Grammar++
		return
	}
	for lang, words := range latinWords {
		if words[lw] || words[elided(lw)] {
			latin[lang]++
		}
	}
	if accented {
		if _, ok := latin["vi"]; !ok && vietnamese(lw) {
			latin["vi"]++
		}
	}
}

// elided turns French "l'annulation" into "l'", so that the article counts.
func elided(w string) string {
	if i := strings.IndexByte(w, '\''); i > 0 && i < len(w)-1 {
		return w[:i+1]
	}
	return w
}

// vietnamese says whether a word has a letter only Vietnamese uses (Latin Extended Additional, đ, ơ, ư).
func vietnamese(w string) bool {
	for _, r := range w {
		if r >= 0x1ea0 && r <= 0x1ef9 || r == 'đ' || r == 'ơ' || r == 'ư' {
			return true
		}
	}
	return false
}

// isIdentifier says whether a word reads as code or an acronym rather than prose: inner capitals (handleRequest),
// digits or underscores (utf8, de_id), or two to five capitals (PR, CI, LGTM).
func isIdentifier(w string) bool {
	upper, lower := 0, 0
	inner := false
	for i, r := range w {
		switch {
		case unicode.IsDigit(r) || r == '_':
			return true
		case unicode.IsUpper(r):
			upper++
			if i > 0 && lower > 0 {
				inner = true
			}
		case unicode.IsLower(r):
			lower++
		}
	}
	return inner || lower == 0 && upper >= 2 && upper <= 5
}

// isNonEnglish says whether the user wrote the prompt in another language.
func isNonEnglish(text string) bool {
	a := analyze(text)
	if a.Foreign > 0 {
		// Text in another script with no English grammar around it ("开 PR", "跑 scripts/ci.sh"); or enough of it
		// to outweigh the English ("please 帮我看看 this function").
		return a.Grammar == 0 || a.Foreign/(a.Foreign+a.Words) >= foreignShare
	}
	if a.Latin == 0 {
		return false
	}
	if a.Words <= shortReply && a.Grammar == 0 {
		return true // "Sí, continúa", "Oui, vas-y"
	}
	return a.Latin >= latinMin && a.Latin > latinMargin*a.Grammar
}

// detectLanguage is the language of the prompt's text that is not English, for the messages; "" when unknown.
func detectLanguage(text string) string { return analyze(text).Language }

var wordBoundary = regexp.MustCompile(`^[a-z0-9]+$`)

// hasStopword says whether the prompt contains one of the words, ignoring case. A word of plain letters and digits
// must stand alone ("asap", not inside "wasapi"); others, such as Chinese ones, match anywhere.
func hasStopword(text string, words []string) bool {
	lower := strings.ToLower(text)
	for _, w := range words {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" {
			continue
		}
		if !wordBoundary.MatchString(w) {
			if strings.Contains(lower, w) {
				return true
			}
			continue
		}
		for i := 0; ; {
			j := strings.Index(lower[i:], w)
			if j < 0 {
				break
			}
			start, end := i+j, i+j+len(w)
			if (start == 0 || !isASCIIWordByte(lower[start-1])) && (end == len(lower) || !isASCIIWordByte(lower[end])) {
				return true
			}
			i = start + 1
		}
	}
	return false
}

func isASCIIWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
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

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// englishWords are English function words and the commonest verbs of a request: what makes a sentence English
// rather than another language with English terms in it.
var englishWords = set(
	"the", "a", "an", "to", "of", "and", "or", "in", "on", "for", "with", "from", "by", "at", "into", "about",
	"is", "are", "was", "were", "be", "been", "it", "its", "this", "that", "these", "those", "there", "here",
	"what", "why", "how", "when", "where", "which", "who", "whether",
	"can", "could", "should", "would", "will", "do", "does", "did", "don't", "doesn't", "can't", "isn't", "won't",
	"not", "no", "yes", "please", "you", "your", "i", "i'm", "me", "my", "we", "our", "us", "they", "them", "their",
	"if", "then", "so", "but", "as", "than", "too", "very", "just", "also", "only", "all", "any", "some", "every",
	"have", "has", "had", "need", "needs", "want", "let", "let's", "after", "before", "instead", "again", "still",
	"make", "add", "fix", "use", "run", "check", "change", "show", "tell", "find", "write", "explain", "remove",
	"thanks", "thank", "looks", "good", "ok", "okay", "sure", "it's", "that's", "what's", "here's", "there's",
)

// latinWords are function words of Latin-script languages, leaving out those English uses too ("a", "in", "no").
var latinWords = map[string]map[string]bool{
	"es": set("el", "los", "las", "del", "que", "por", "para", "con", "una", "uno", "es", "está", "este",
		"esta", "esto", "pero", "como", "más", "muy", "sí", "qué", "cuando", "también", "luego", "después", "hay",
		"puedes", "porque", "sin", "sobre", "se", "lo", "su", "sus", "mi", "tu", "al", "ya", "cuál", "dónde", "cómo",
		"favor", "falla", "añade", "cambia", "arregla", "ejecuta", "continúa", "sigue", "gracias"),
	"fr": set("le", "la", "les", "des", "du", "et", "est", "une", "que", "qui", "pour", "dans", "avec", "sur", "pas",
		"ce", "cette", "ces", "mais", "ou", "où", "je", "tu", "il", "nous", "vous", "ils", "mon", "ton", "sa", "ses",
		"leur", "aussi", "après", "avant", "très", "oui", "peux", "puis", "ensuite", "pourquoi", "comment", "quand",
		"c'est", "n'est", "l'", "d'", "qu'", "j'", "vas-y", "est-ce", "peux-tu", "corrige", "lance", "ajouter",
		"merci"),
	"de": set("der", "die", "das", "und", "ist", "nicht", "ein", "eine", "einen", "mit", "für", "auf", "dem", "den",
		"zu", "von", "bitte", "warum", "wie", "wenn", "dann", "danach", "auch", "aber", "oder", "ich", "du", "wir",
		"ihr", "sie", "kannst", "kann", "wird", "werden", "noch", "schon", "mach", "weiter", "ja", "nein", "diesen",
		"dieser", "dieses", "läuft", "führe", "aus", "danke"),
	"pt": set("os", "um", "uma", "que", "não", "por", "para", "com", "do", "da", "dos", "das", "na", "é", "está",
		"este", "esta", "isso", "mas", "depois", "também", "porque", "como", "quando", "sim", "você", "rode",
		"corrija", "continua", "obrigado"),
	"it": set("il", "lo", "gli", "la", "le", "di", "che", "per", "con", "non", "una", "uno", "è", "sono", "questo",
		"questa", "poi", "perché", "come", "quando", "anche", "dopo", "stato", "del", "della", "dei", "esegui",
		"sistema", "grazie"),
	"nl": set("de", "het", "een", "en", "van", "je", "jij", "kun", "kunt", "wij", "graag", "alsjeblieft", "dat", "niet", "met", "voor", "ook", "maar", "daarna", "deze", "dit",
		"zijn", "wordt", "waarom", "hoe", "draai", "repareer", "bedankt"),
	"tr": set("ve", "bu", "bir", "için", "ile", "sonra", "ne", "neden", "nasıl", "değil", "da", "mi", "düzelt",
		"çalıştır", "testi"),
	"pl": set("na", "nie", "jest", "że", "się", "ta", "potem", "dlaczego", "jak", "czy",
		"oraz", "napraw", "uruchom"),
	"id": set("dan", "yang", "ini", "itu", "untuk", "dengan", "tidak", "lalu", "kemudian", "tolong", "bisa", "apa",
		"kenapa", "mengapa", "ke", "perbaiki", "jalankan"),
	"vi": set("và", "của", "là", "không", "có", "cho", "này", "được", "với", "các", "những", "tôi", "bạn", "giúp",
		"rồi", "sau", "khi", "tại", "sao", "vẫn", "bị", "thì", "đã", "sẽ", "đang", "chạy", "sửa"),
}
