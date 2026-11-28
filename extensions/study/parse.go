package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// cardText is one question and answer before it becomes a card or a draft.
type cardText struct {
	Front string `json:"front"`
	Back  string `json:"back"`
	Topic string `json:"topic,omitempty"`
}

var (
	srComment = regexp.MustCompile(`\s*<!--SR:[^>]*-->`)
	cloze     = regexp.MustCompile(`==([^=\n]+)==`)
	fence     = regexp.MustCompile("^\\s*(```|~~~)")
)

// hasTag reports whether an Obsidian note carries #tag (or a #tag/sub-deck) in its text or a tags entry in its front matter.
func hasTag(text, tag string) bool {
	tag = strings.TrimPrefix(strings.TrimSpace(tag), "#")
	if tag == "" {
		return true
	}
	re := regexp.MustCompile(`(^|[\s(\[])#` + regexp.QuoteMeta(tag) + `(/[^\s#]*)?([\s)\],.;]|$)`)
	if re.MatchString(text) {
		return true
	}
	fm, ok := frontMatter(text)
	if !ok {
		return false
	}
	inTags := false
	for _, l := range strings.Split(fm, "\n") {
		t := strings.TrimSpace(l)
		if k, v, found := strings.Cut(t, ":"); found && !strings.HasPrefix(t, "-") {
			inTags = strings.EqualFold(strings.TrimSpace(k), "tags") || strings.EqualFold(strings.TrimSpace(k), "tag")
			if inTags {
				for _, w := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '[' || r == ']' || r == ' ' || r == '"' || r == '\'' }) {
					if w = strings.TrimPrefix(w, "#"); w == tag || strings.HasPrefix(w, tag+"/") {
						return true
					}
				}
			}
			continue
		}
		if inTags && strings.HasPrefix(t, "-") {
			w := strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "-")), `"'#`)
			if w == tag || strings.HasPrefix(w, tag+"/") {
				return true
			}
		}
	}
	return false
}

func frontMatter(text string) (string, bool) {
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return "", false
	}
	rest := text[strings.Index(text, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// parseObsidian reads the flashcards a note writes in the syntax of Obsidian's Spaced Repetition plugin:
//
//	Question::Answer            one card
//	Question:::Answer           two cards, one each way
//	Question / ? / Answer       a multi-line card; ?? makes it both ways (ends at a blank line)
//	a ==hidden== word           a cloze card per highlight
//
// Code blocks and the plugin's own <!--SR:...--> schedule comments are skipped; this extension schedules on its own.
func parseObsidian(text string) []cardText {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if fm, ok := frontMatter(text); ok {
		text = strings.TrimPrefix(text[len("---\n")+len(fm):], "\n---")
	}
	lines := strings.Split(text, "\n")
	var out []cardText
	var para []string // lines of the current paragraph, before a ? line
	inFence := false
	for i := 0; i < len(lines); i++ {
		l := srComment.ReplaceAllString(lines[i], "")
		if fence.MatchString(l) {
			inFence = !inFence
			para = nil
			continue
		}
		if inFence {
			continue
		}
		t := strings.TrimSpace(l)
		if t == "" {
			para = nil
			continue
		}
		if t == "?" || t == "??" {
			var ans []string
			for i+1 < len(lines) && strings.TrimSpace(srComment.ReplaceAllString(lines[i+1], "")) != "" && !fence.MatchString(lines[i+1]) {
				i++
				ans = append(ans, strings.TrimRight(srComment.ReplaceAllString(lines[i], ""), " \t"))
			}
			q, a := strings.TrimSpace(strings.Join(para, "\n")), strings.TrimSpace(strings.Join(ans, "\n"))
			if q != "" && a != "" {
				out = append(out, cardText{Front: q, Back: a})
				if t == "??" {
					out = append(out, cardText{Front: a, Back: q})
				}
			}
			para = nil
			continue
		}
		if q, a, ok := strings.Cut(t, ":::"); ok {
			q, a = strings.TrimSpace(q), strings.TrimSpace(a)
			if q != "" && a != "" {
				out = append(out, cardText{Front: q, Back: a}, cardText{Front: a, Back: q})
				para = nil
				continue
			}
		}
		if q, a, ok := strings.Cut(t, "::"); ok && !strings.Contains(q, "://") {
			q, a = strings.TrimSpace(q), strings.TrimSpace(a)
			if q != "" && a != "" {
				out = append(out, cardText{Front: q, Back: a})
				para = nil
				continue
			}
		}
		if ms := cloze.FindAllStringSubmatchIndex(t, -1); len(ms) > 0 {
			for k, m := range ms {
				var b strings.Builder
				last := 0
				for j, n := range ms {
					b.WriteString(t[last:n[0]])
					if j == k {
						b.WriteString("［…］")
					} else {
						b.WriteString(t[n[2]:n[3]])
					}
					last = n[1]
				}
				b.WriteString(t[last:])
				out = append(out, cardText{Front: b.String(), Back: t[m[2]:m[3]]})
			}
		}
		para = append(para, t)
	}
	return out
}

// nlmFlashcards is notebooklm-py's flashcard export (download flashcards --format json).
type nlmFlashcards struct {
	Title string     `json:"title"`
	Cards []cardText `json:"cards"`
}

// nlmQuiz is notebooklm-py's quiz export (download quiz --format json).
type nlmQuiz struct {
	Title     string `json:"title"`
	Questions []struct {
		Question string `json:"question"`
		Options  []struct {
			Text    string `json:"text"`
			Correct bool   `json:"isCorrect"`
		} `json:"answerOptions"`
		Hint string `json:"hint"`
	} `json:"questions"`
}

// parseNLMArtifact turns an exported NotebookLM flashcard set or quiz into card texts. A quiz question becomes a
// card with its options on the front and the correct one on the back.
func parseNLMArtifact(kind string, raw []byte) (string, []cardText, error) {
	switch kind {
	case "flashcards":
		var f nlmFlashcards
		if err := json.Unmarshal(raw, &f); err != nil {
			return "", nil, errors.New("NotebookLM 闪卡的格式认不出来")
		}
		return f.Title, f.Cards, nil
	case "quiz":
		var q nlmQuiz
		if err := json.Unmarshal(raw, &q); err != nil {
			return "", nil, errors.New("NotebookLM 测验的格式认不出来")
		}
		var out []cardText
		for _, x := range q.Questions {
			var front strings.Builder
			front.WriteString(strings.TrimSpace(x.Question))
			var right []string
			for i, o := range x.Options {
				front.WriteString("\n" + string(rune('A'+i)) + ". " + strings.TrimSpace(o.Text))
				if o.Correct {
					right = append(right, string(rune('A'+i))+". "+strings.TrimSpace(o.Text))
				}
			}
			if len(right) == 0 || strings.TrimSpace(x.Question) == "" {
				continue
			}
			out = append(out, cardText{Front: front.String(), Back: strings.Join(right, "\n")})
		}
		return q.Title, out, nil
	}
	return "", nil, errors.New("只能读 NotebookLM 的闪卡或测验")
}

// ---- Logseq ----

var (
	lsBullet   = regexp.MustCompile(`^([\t ]*)-(?:[ \t](.*))?$`)
	lsProperty = regexp.MustCompile(`^\s*([A-Za-z0-9_-]+):: ?(.*)$`)
	lsCardTag  = regexp.MustCompile(`(?i)(^|\s)(#card|#\[\[card\]\]|\[\[card\]\])(\s|$)`)
	lsCloze    = regexp.MustCompile(`\{\{cloze:? ([^}]+)\}\}`)
	lsLink     = regexp.MustCompile(`\[\[([^\[\]]+)\]\]`)
	lsBlockRef = regexp.MustCompile(`\(\(([0-9a-f-]{36})\)\)`)
	lsJournal  = regexp.MustCompile(`^(\d{4})_(\d{2})_(\d{2})$`)
	lsCode     = regexp.MustCompile("(?s)```.*?(```|$)|`[^`\n]*`")
)

// lsBlock is one outline block of a Logseq page.
type lsBlock struct {
	indent   int
	lines    []string
	props    map[string]string
	children []*lsBlock
}

// logseqPage is what a Logseq page or journal says: its page properties and its outline.
type logseqPage struct {
	props  map[string]string
	blocks []*lsBlock
}

func indentWidth(s string) int {
	n := 0
	for _, r := range s {
		switch r {
		case '\t':
			n += 2
		case ' ':
			n++
		default:
			return n
		}
	}
	return n
}

// parseLogseqOutline reads a page's outline: "- " bullets nested by indentation (tabs or spaces), lines under a
// bullet that continue it, "key:: value" properties of a block or, before the first bullet, of the page. Code
// fences inside a block are kept as its text.
func parseLogseqOutline(text string) logseqPage {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	page := logseqPage{props: map[string]string{}}
	var stack []*lsBlock // the open blocks, outermost first
	var cur *lsBlock
	inFence := false
	for _, l := range strings.Split(text, "\n") {
		if !inFence {
			if m := lsBullet.FindStringSubmatch(l); m != nil {
				b := &lsBlock{indent: indentWidth(m[1]), props: map[string]string{}}
				for len(stack) > 0 && stack[len(stack)-1].indent >= b.indent {
					stack = stack[:len(stack)-1]
				}
				if len(stack) == 0 {
					page.blocks = append(page.blocks, b)
				} else {
					p := stack[len(stack)-1]
					p.children = append(p.children, b)
				}
				stack = append(stack, b)
				cur = b
				l = m[2]
				if fence.MatchString(l) {
					inFence = true
				}
				if pm := lsProperty.FindStringSubmatch(l); pm != nil {
					b.props[strings.ToLower(pm[1])] = strings.TrimSpace(pm[2])
					continue
				}
				b.lines = append(b.lines, l)
				continue
			}
		}
		if fence.MatchString(l) {
			inFence = !inFence
		}
		if cur == nil {
			if pm := lsProperty.FindStringSubmatch(l); pm != nil {
				page.props[strings.ToLower(pm[1])] = strings.TrimSpace(pm[2])
			}
			continue
		}
		if !inFence {
			if pm := lsProperty.FindStringSubmatch(l); pm != nil && !fence.MatchString(l) {
				cur.props[strings.ToLower(pm[1])] = strings.TrimSpace(pm[2])
				continue
			}
		}
		cur.lines = append(cur.lines, strings.TrimLeft(l, " \t"))
	}
	return page
}

func (b *lsBlock) text() string { return strings.TrimSpace(strings.Join(b.lines, "\n")) }

// isCard reports whether a block is a Logseq flashcard: tagged #card, #[[card]] or [[card]], or tags:: card.
func (b *lsBlock) isCard() bool {
	if lsCardTag.MatchString(lsCode.ReplaceAllString(b.text(), "")) {
		return true
	}
	for _, t := range strings.FieldsFunc(b.props["tags"], func(r rune) bool { return r == ',' || r == ' ' }) {
		if strings.EqualFold(strings.Trim(t, "#[]"), "card") {
			return true
		}
	}
	return false
}

// lsPlain makes block text readable on a card: the card tag goes, [[links]] keep their name.
func lsPlain(s string) string {
	s = lsCardTag.ReplaceAllString(s, "$1$3")
	s = lsBlockRef.ReplaceAllString(s, "")
	s = lsLink.ReplaceAllString(s, "$1")
	return strings.TrimSpace(s)
}

// outline renders a block's children as an indented list, the answer of a card.
func outline(bs []*lsBlock, depth int, b *strings.Builder) {
	for _, c := range bs {
		t := lsPlain(c.text())
		if t != "" {
			b.WriteString(strings.Repeat("  ", depth) + "- " + strings.ReplaceAll(t, "\n", "\n"+strings.Repeat("  ", depth+1)) + "\n")
		}
		outline(c.children, depth+1, b)
	}
}

// parseLogseq reads the flashcards of a Logseq page: a block tagged as a card is the question and its child blocks,
// as an outline, the answer; a {{cloze …}} in a card block makes a fill-in card per cloze. Logseq's own card-*
// scheduling properties are ignored; this extension schedules on its own.
func parseLogseq(text string) []cardText {
	page := parseLogseqOutline(text)
	var out []cardText
	var walk func(bs []*lsBlock)
	walk = func(bs []*lsBlock) {
		for _, b := range bs {
			if b.isCard() {
				front := lsPlain(b.text())
				if ms := lsCloze.FindAllStringSubmatchIndex(front, -1); len(ms) > 0 {
					for k, m := range ms {
						var q strings.Builder
						last := 0
						for j, n := range ms {
							q.WriteString(front[last:n[0]])
							if j == k {
								q.WriteString("［…］")
							} else {
								q.WriteString(front[n[2]:n[3]])
							}
							last = n[1]
						}
						q.WriteString(front[last:])
						out = append(out, cardText{Front: strings.TrimSpace(q.String()), Back: strings.TrimSpace(front[m[2]:m[3]])})
					}
				} else if front != "" {
					var back strings.Builder
					outline(b.children, 0, &back)
					if a := strings.TrimSpace(back.String()); a != "" {
						out = append(out, cardText{Front: front, Back: a})
					}
				}
				continue // a card's children are its answer, not more cards
			}
			walk(b.children)
		}
	}
	walk(page.blocks)
	return out
}

// logseqTitle is a page's name: its title:: property, else its file name, where Logseq writes a namespace's "/" as
// "___" (or, in older graphs, %2F) and a journal as 2026_11_27.
func logseqTitle(rel, text string) string {
	if t := strings.TrimSpace(parseLogseqOutline(text).props["title"]); t != "" {
		return t
	}
	name := noteTitle(rel)
	if m := lsJournal.FindStringSubmatch(name); m != nil {
		return m[1] + "-" + m[2] + "-" + m[3]
	}
	name = strings.ReplaceAll(name, "___", "/")
	if u, err := url.PathUnescape(name); err == nil {
		name = u
	}
	return name
}

// logseqText is a page as plain text for the model: the outline with properties left out.
func logseqText(text string) string {
	var b strings.Builder
	outline(parseLogseqOutline(text).blocks, 0, &b)
	return b.String()
}
