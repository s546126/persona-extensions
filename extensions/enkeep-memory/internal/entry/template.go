package entry

import (
	"strings"
	"unicode/utf8"
)

// Templates (0.2.0-beta.52, after cogpipe's collect.mjs origin split): a line the user is said to have typed in other
// agents' sessions (S3) that comes back word for word in TemplateSessions sessions or more is a template a
// supervisor, the assistant or the platform types into every task (a dispatch prompt's rules, the hub's handoff
// notes), not the user's own habit. People do not type the same sentence into many sessions; templates do. Entries
// resting only on such lines are not the user's: they never stand, and a pattern resting mostly on them neither.
const (
	TemplateSessions = 3
	templateRunes    = 12 // a shorter quote ("好的", "继续") says nothing about where it came from
)

// TemplateKey is a quote or line without its spacing; "" when too short to tell.
func TemplateKey(q string) string {
	k := strings.Join(strings.Fields(q), "")
	if utf8.RuneCountInString(k) < templateRunes {
		return ""
	}
	return k
}

func templateKey(q string) string { return TemplateKey(q) }

// BatchTemplateLines finds lines or clauses (at least templateRunes runes) that recur verbatim in TemplateSessions or
// more distinct S3 sessions in a batch, so pre-model cleaning strips repeated dispatch templates before extraction.
func BatchTemplateLines(sessionLines map[string][]string) map[string]bool {
	refs := map[string]map[string]bool{}
	add := func(ref, s string) {
		k := TemplateKey(s)
		if k == "" {
			return
		}
		if refs[k] == nil {
			refs[k] = map[string]bool{}
		}
		refs[k][ref] = true
	}
	for ref, lines := range sessionLines {
		for _, line := range lines {
			add(ref, line)
			for _, cl := range splitTemplateClauses(line) {
				add(ref, cl)
			}
		}
	}
	out := map[string]bool{}
	for k, r := range refs {
		if len(r) >= TemplateSessions {
			out[k] = true
		}
	}
	return out
}

// IsTemplateLine says whether text (or every non-trivial clause of it) matches a known multi-session template.
func IsTemplateLine(text string, tpl map[string]bool) bool {
	if len(tpl) == 0 {
		return false
	}
	if k := TemplateKey(text); k != "" && tpl[k] {
		return true
	}
	clauses := splitTemplateClauses(text)
	checked := 0
	for _, cl := range clauses {
		k := TemplateKey(cl)
		if k == "" {
			continue
		}
		checked++
		if !tpl[k] {
			return false
		}
	}
	return checked > 0
}

func splitTemplateClauses(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '。' || r == '；' || r == ';'
	})
}

// selfLine: evidence that says the user typed it.
func selfLine(ev Evidence) bool { return ev.Who == WhoSelf && !ev.Reported }

// TemplateQuotes are the template lines among entries' evidence: S3 lines of the user's that recur in
// TemplateSessions sessions or more.
func TemplateQuotes(entries map[string]*Entry) map[string]bool {
	refs := map[string]map[string]bool{}
	for _, e := range entries {
		for _, ev := range e.Evidence {
			if ev.Source != SrcAgent || !selfLine(ev) {
				continue
			}
			k := templateKey(ev.Quote)
			if k == "" {
				continue
			}
			if refs[k] == nil {
				refs[k] = map[string]bool{}
			}
			refs[k][ev.Ref] = true
		}
	}
	out := map[string]bool{}
	for k, r := range refs {
		if len(r) >= TemplateSessions {
			out[k] = true
		}
	}
	return out
}

// TemplateOnly says whether every line of the user's behind e is a template (and there is one).
func TemplateOnly(e *Entry, tpl map[string]bool) bool {
	if len(tpl) == 0 {
		return false
	}
	n := 0
	for _, ev := range e.Evidence {
		if !selfLine(ev) {
			continue
		}
		if ev.Source != SrcAgent || !tpl[templateKey(ev.Quote)] {
			return false
		}
		n++
	}
	return n > 0
}

// TemplateBacked says whether at least half of the entries ids (a pattern's evidence) are TemplateOnly.
func TemplateBacked(entries map[string]*Entry, ids []string, tpl map[string]bool) bool {
	n, t := 0, 0
	for _, id := range ids {
		if e := entries[id]; e != nil {
			n++
			if TemplateOnly(e, tpl) {
				t++
			}
		}
	}
	return n > 0 && 2*t >= n
}
