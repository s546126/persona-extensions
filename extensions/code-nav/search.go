package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Find in Files: a plain read-only scan of a repository's listed files for a piece of text. It reads no file the page
// could not open (the same folder and secret checks), follows no link out of the repository, skips binary files and
// the folders that hold dependencies and build output, and stops at a time and a hit budget.

const (
	maxQuery     = 200             // characters of a search
	maxGrepFile  = 1 << 20         // bytes of a file searched
	maxGrepHits  = 200             // places found
	maxGrepPerF  = 20              // places found in one file
	grepBudget   = 8 * time.Second // the scan stops after this long, with what it found
	previewChars = 160             // characters of a line shown around a hit
)

// grepSkip are folders whose files are never searched: dependencies, build output and caches.
var grepSkip = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true, "out": true, "target": true, "vendor": true,
	".next": true, ".nuxt": true, ".svelte-kit": true, "__pycache__": true, ".venv": true, "venv": true, ".tox": true,
	".mypy_cache": true, ".pytest_cache": true, "coverage": true, ".build": true, "DerivedData": true, "Pods": true,
	".gradle": true, ".idea": true, ".cache": true, "bower_components": true,
}

type grepHit struct {
	Path    string `json:"path"`
	Rel     string `json:"rel"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	Preview string `json:"preview"`
}

type grepResult struct {
	Hits      []grepHit `json:"hits"`
	Files     int       `json:"files"`               // files with a hit
	Scanned   int       `json:"scanned"`             // files read
	Truncated bool      `json:"truncated,omitempty"` // more places than are listed
	Partial   bool      `json:"partial,omitempty"`   // the time budget ran out before every file was read
}

// grepSkipped says whether rel (a listed file, / between names) is in a folder never searched.
func grepSkipped(rel string) bool {
	for d := path.Dir(rel); d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if grepSkip[path.Base(d)] {
			return true
		}
	}
	return false
}

func (n *node) grep(ctx context.Context, r repo, query string) (grepResult, error) {
	q := strings.TrimSpace(query)
	if utf8.RuneCountInString(q) > maxQuery {
		q = string([]rune(q)[:maxQuery])
	}
	res := grepResult{Hits: []grepHit{}}
	if q == "" {
		return res, nil
	}
	// Smart case, as IDEs do: a search with a capital letter matches case, one without does not.
	fold := q == strings.ToLower(q)
	needle := []byte(q)
	if fold {
		needle = bytes.ToLower(needle)
	}
	n.mu.Lock()
	home := n.home
	n.mu.Unlock()
	files, _ := n.list(ctx, r)
	deadline := time.Now().Add(grepBudget)
	for _, rel := range files {
		if ctx.Err() != nil || time.Now().After(deadline) {
			res.Partial = true
			break
		}
		if grepSkipped(rel) || strings.Contains("/"+rel+"/", "/../") {
			continue
		}
		real, err := filepath.EvalSymlinks(filepath.Join(r.Path, filepath.FromSlash(rel)))
		if err != nil || !within(real, r.Path) || secretPath(real, home) {
			continue // a link out of the repository, or a file that holds secrets
		}
		data, ok := grepRead(real)
		if !ok {
			continue
		}
		res.Scanned++
		hay := data
		if fold {
			hay = bytes.ToLower(data)
		}
		if !bytes.Contains(hay, needle) {
			continue
		}
		res.Files++
		lineNo, inFile := 0, 0
		for len(data) > 0 {
			lineNo++
			i := bytes.IndexByte(data, '\n')
			var line, low []byte
			if i < 0 {
				line, low, data, hay = data, hay, nil, nil
			} else {
				line, low, data, hay = data[:i], hay[:i], data[i+1:], hay[i+1:]
			}
			at := bytes.Index(low, needle)
			if at < 0 {
				continue
			}
			if len(res.Hits) >= maxGrepHits {
				res.Truncated = true
				return res, nil
			}
			text := strings.TrimRight(string(line), "\r")
			col := utf8.RuneCount(line[:at]) + 1
			res.Hits = append(res.Hits, grepHit{Path: real, Rel: rel, Line: lineNo, Col: col, Preview: previewAround(text, col)})
			if inFile++; inFile >= maxGrepPerF {
				break
			}
		}
	}
	return res, nil
}

// grepRead reads a file to search: a regular file of at most maxGrepFile bytes, not binary.
func grepRead(p string) ([]byte, bool) {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxGrepFile {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil || bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return nil, false
	}
	return data, true
}

// previewAround is line trimmed, and on a long line the part around col (1-based, characters).
func previewAround(line string, col int) string {
	rs := []rune(line)
	start := 0
	for start < len(rs) && start < col-1 && unicode.IsSpace(rs[start]) {
		start++
	}
	if len(rs)-start <= previewChars {
		return strings.TrimSpace(string(rs[start:]))
	}
	from := max(start, col-1-previewChars/3)
	to := min(len(rs), from+previewChars)
	out := strings.TrimSpace(string(rs[from:to]))
	if from > start {
		out = "…" + out
	}
	if to < len(rs) {
		out += "…"
	}
	return out
}

// hover is what the server says about the name at (line, col): its type or signature and its documentation.
func (n *node) hover(ctx context.Context, p navParams) (hoverResult, error) {
	srv, pos, real, lines, err := n.position(ctx, p)
	if errors.Is(err, errNoServer) {
		lang, _ := languageOf(p.Path)
		return hoverResult{NoServer: lang.name}, nil
	}
	if err != nil {
		return hoverResult{}, err
	}
	res := hoverResult{Word: wordAt(lines[pos.Line], p.Col)}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	doc := map[string]any{"textDocument": map[string]string{"uri": fileURI(real)}, "position": pos}
	if err := srv.call(cctx, "textDocument/hover", doc, &h); err != nil {
		return hoverResult{}, err
	}
	res.Text, res.Markdown = hoverText(h.Contents)
	res.Text = cutRunes(strings.TrimSpace(res.Text), 16000)
	return res, nil
}

// hoverText reads every shape of Hover.contents: MarkupContent, a MarkedString (a string or {language, value}), or a
// list of MarkedStrings. It says whether the text is Markdown.
func hoverText(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var markup struct {
		Kind     string `json:"kind"`
		Value    string `json:"value"`
		Language string `json:"language"`
	}
	switch raw[0] {
	case '"':
		var s string
		_ = json.Unmarshal(raw, &s)
		return s, true // a MarkedString string is Markdown
	case '{':
		if json.Unmarshal(raw, &markup) != nil {
			return "", false
		}
		if markup.Kind != "" {
			return markup.Value, markup.Kind == "markdown"
		}
		return fence(markup.Language, markup.Value), true
	case '[':
		var parts []json.RawMessage
		if json.Unmarshal(raw, &parts) != nil {
			return "", false
		}
		var out []string
		for _, part := range parts {
			if t, _ := hoverText(part); strings.TrimSpace(t) != "" {
				out = append(out, t)
			}
		}
		return strings.Join(out, "\n\n"), true
	}
	return "", false
}

func fence(lang, code string) string {
	if strings.TrimSpace(code) == "" {
		return ""
	}
	return "```" + lang + "\n" + strings.TrimRight(code, "\n") + "\n```"
}
