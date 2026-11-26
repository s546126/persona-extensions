package main

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// cardText is one question and answer before it becomes a card or a draft.
type cardText struct {
	Front string `json:"front"`
	Back  string `json:"back"`
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
