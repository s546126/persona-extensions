// Package i18n is the extension's own text in the hub's three interface languages: the connection test's results,
// the status page and the settings check's refusals. It is the subset of the Persona hub's i18n package the program
// used as a built-in, with its volc-check catalog under locales/<lang>/volc-check.json, so the texts read the same.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Lang is an interface language: a directory under locales.
type Lang string

// The languages the hub speaks.
const (
	ZhHans Lang = "zh-Hans"
	En     Lang = "en"
	Ja     Lang = "ja"
)

// Default is the language when no client has said one, as on the hub.
const Default = ZhHans

// Fallback is the language for a tag no catalog matches, and the first one a missing key is looked up in.
const Fallback = En

// matches are the BCP 47 language subtags (lowercase) that resolve to each language (any Chinese is zh-Hans).
var matches = map[string]Lang{"zh": ZhHans, "en": En, "ja": Ja}

//go:embed locales
var files embed.FS

var (
	once     sync.Once
	catalogs map[Lang]map[string]string
)

// catalog is l's texts by key, "<file>.<key>" (volc-check.ok).
func catalog(l Lang) map[string]string {
	once.Do(func() {
		catalogs = map[Lang]map[string]string{}
		for _, lang := range []Lang{ZhHans, En, Ja} {
			out := map[string]string{}
			entries, _ := files.ReadDir(path.Join("locales", string(lang)))
			for _, e := range entries {
				raw, err := files.ReadFile(path.Join("locales", string(lang), e.Name()))
				if err != nil {
					panic("i18n: " + err.Error()) // embedded and tested: never at run time
				}
				var kv map[string]string
				if err := json.Unmarshal(raw, &kv); err != nil {
					panic("i18n: " + e.Name() + ": " + err.Error())
				}
				ns := strings.TrimSuffix(e.Name(), ".json")
				for k, v := range kv {
					out[ns+"."+k] = v
				}
			}
			catalogs[lang] = out
		}
	})
	return catalogs[l]
}

// Resolve maps any BCP 47 tag to a supported language: its language subtag picks it, the rest is en. An empty tag is
// "" (unknown), so callers can tell "not said" from "said English".
func Resolve(tag string) Lang {
	primary, _, _ := strings.Cut(strings.ReplaceAll(strings.TrimSpace(tag), "_", "-"), "-")
	if primary == "" {
		return ""
	}
	if l, ok := matches[strings.ToLower(primary)]; ok {
		return l
	}
	return Fallback
}

// Valid: one of the supported codes exactly.
func Valid(l Lang) bool { return l == ZhHans || l == En || l == Ja }

// Or is l, or fallback when l is unknown.
func (l Lang) Or(fallback Lang) Lang {
	if Valid(l) {
		return l
	}
	return fallback
}

// Args are a text's placeholder values.
type Args map[string]any

// T is key's text in l (an unknown l is the default), placeholders filled from args; a key no language has is
// itself.
func T(l Lang, key string, args ...Args) string {
	l = l.Or(Default)
	s, ok := catalog(l)[key]
	if !ok {
		if s, ok = catalog(Fallback)[key]; !ok {
			if s, ok = catalog(ZhHans)[key]; !ok {
				return key
			}
		}
	}
	if len(args) == 0 || args[0] == nil {
		return s
	}
	return fill(s, args[0])
}

// fill replaces each {name} in s (name an ASCII word) with a[name]; an unknown name stays as written. A value that
// ends a sentence, filled in right before the text's own punctuation, loses its full stop, as on the hub.
func fill(s string, a Args) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := i + 1
		for j < len(s) && isWordByte(s[j]) {
			j++
		}
		if j > i+1 && j < len(s) && s[j] == '}' {
			if v, ok := a[s[i+1:j]]; ok {
				val := argText(v)
				if markAfter(s[j+1:]) {
					val = withoutFullStop(val)
				}
				b.WriteString(s[:i])
				b.WriteString(val)
				s = s[j+1:]
				continue
			}
		}
		b.WriteString(s[:j])
		s = s[j:]
	}
}

func argText(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	}
	return fmt.Sprint(v)
}

// markAfter: rest (the text after a placeholder) starts with punctuation that makes a value's own full stop a second one.
func markAfter(rest string) bool {
	r, _ := utf8.DecodeRuneInString(rest)
	return strings.ContainsRune("）)，,。．.：:；;、", r)
}

// withoutFullStop is v without the full stop it ends with; an ellipsis keeps its dots.
func withoutFullStop(v string) string {
	t := strings.TrimRightFunc(v, unicode.IsSpace)
	last, n := utf8.DecodeLastRuneInString(t)
	if n == 0 || !isFullStop(last) {
		return v
	}
	if prev, _ := utf8.DecodeLastRuneInString(t[:len(t)-n]); isFullStop(prev) {
		return v
	}
	return t[:len(t)-n]
}

func isFullStop(r rune) bool { return r == '。' || r == '．' || r == '.' }

func isWordByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
}
