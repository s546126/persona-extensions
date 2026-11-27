package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Location   string `json:"l,omitempty"`
	Cursor     string `json:"c,omitempty"`
	Page       int    `json:"p,omitempty"`
	Query      string `json:"q,omitempty"`
	DocumentID string `json:"d,omitempty"`
	Mode       string `json:"m,omitempty"`
	DraftID    string `json:"r,omitempty"`
	SaveURL    string `json:"u,omitempty"`
}

const maxQueryChars = 200

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Page < 0 || len(st.Cursor) > 256 || len([]rune(st.Query)) > maxQueryChars || len(st.DocumentID) > 128 || len(st.DraftID) > 128 || len(st.SaveURL) > 8192 {
		return pageState{}, errors.New("invalid page state")
	}
	if st.Location != "" && validLocation(st.Location) != nil {
		return pageState{}, errors.New("invalid page state")
	}
	if st.Mode != "" && st.Mode != "list" && st.Mode != "detail" && st.Mode != "save" && st.Mode != "review-save" {
		return pageState{}, errors.New("invalid page state")
	}
	return st, nil
}

func (a *app) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	st, err := readState(r)
	if err != nil {
		return nil, err
	}
	return a.page(ctx, r, st)
}

func (a *app) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	st, err := readState(r)
	if err != nil {
		return nil, err
	}
	switch r.Action {
	case "search":
		st.Query, st.Cursor, st.Page, st.Mode = clipText(strings.TrimSpace(r.Values["q"]), maxQueryChars), "", 0, "list"
	case "location":
		if err := validLocation(r.Values["location"]); err != nil {
			return nil, err
		}
		st.Location, st.Cursor, st.Mode = r.Values["location"], "", "list"
		st.Page, st.Query = 0, ""
	case "more":
		if st.Cursor == "" {
			return nil, errors.New("no further reading page")
		}
		st.Page = min(st.Page+1, 3)
	case "detail":
		if err := validDocumentID(r.Values["id"]); err != nil {
			return nil, err
		}
		st.DocumentID, st.Mode = r.Values["id"], "detail"
		st.DraftID = randomID()
	case "back":
		st.Mode, st.DocumentID = "list", ""
	case "save-form":
		st.Mode, st.DraftID = "save", randomID()
	case "review-save":
		u, err := validHTTPURL(r.Values["url"])
		if err != nil {
			return nil, err
		}
		st.SaveURL, st.Mode = u, "review-save"
	case "save":
		if st.DraftID == "" {
			return nil, errors.New("the save form expired; reopen it")
		}
		out, err := a.doAction(ctx, "ui-save:"+st.DraftID, "readwise_save", []byte(`{"url":`+quote(st.SaveURL)+`}`))
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	case "shortlist":
		if err := validDocumentID(st.DocumentID); err != nil {
			return nil, err
		}
		if st.DraftID == "" {
			return nil, errors.New("the document view expired; reopen it")
		}
		out, err := a.doAction(ctx, "ui-move:"+st.DraftID, "readwise_update", []byte(`{"id":`+quote(st.DocumentID)+`,"location":"shortlist"}`))
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	default:
		return nil, fmt.Errorf("unknown page action %q", r.Action)
	}
	v, err := a.page(ctx, r, st)
	return &wire.ActionResult{View: v}, err
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if a.client.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingToken}}}}}, nil
	}
	if st.Mode == "save" {
		return saveView(r, st), nil
	}
	if st.Mode == "review-save" {
		return reviewSaveView(r, st), nil
	}
	if st.Mode == "detail" {
		if err := validDocumentID(st.DocumentID); err != nil {
			return nil, err
		}
		page, err := a.client.list(ctx, listParams{ID: st.DocumentID, HTML: true})
		a.record(err)
		if err != nil {
			return nil, err
		}
		if len(page.Results) == 0 {
			return nil, errNotFound
		}
		item := page.Results[0]
		highlights, err := a.client.highlights(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item, highlights)
	}
	if st.Mode != "" && st.Mode != "list" {
		return nil, errors.New("invalid reading view")
	}
	if st.Location == "" {
		st.Location = "new"
	}
	if err := validLocation(st.Location); err != nil {
		return nil, err
	}
	page, err := a.readQueue(ctx, st)
	a.record(err)
	if err != nil {
		return nil, err
	}
	// A page holds up to 100 documents: the queue is cut to fit the hub's 300 component limit, and then offers no more.
	if len(page.Results) >= maxViewDocuments {
		page.Results, page.Cursor = page.Results[:maxViewDocuments], ""
		page.Count = len(page.Results)
	}
	st.Cursor = page.Cursor
	v := &wire.View{Title: t.new, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	v.Filter = &wire.Filter{Name: "location", Title: t.read, Value: st.Location, Action: wire.Action{ID: "location"}, Choices: []wire.Choice{
		{Value: "new", Title: t.new}, {Value: "later", Title: t.later}, {Value: "shortlist", Title: t.shortlist}, {Value: "archive", Title: t.archive}, {Value: "feed", Title: t.feed},
	}}
	section := wire.Section{Title: fmt.Sprintf(t.results, page.Count), Items: []wire.Component{{
		Type: wire.ComponentButton, Title: t.save, Action: &wire.Action{ID: "save-form", Push: &wire.Push{State: ext.EncodeState(pageState{Mode: "save", DraftID: randomID()}), Title: t.save}},
	}}}
	for _, item := range page.Results {
		section.Items = append(section.Items, documentRow(item))
	}
	if page.Cursor != "" && st.Page < 3 {
		section.More = &wire.More{Title: textsFor(r.Locale).more, Action: wire.Action{ID: "more"}}
	}
	if len(page.Results) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noDocuments})
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

const maxViewDocuments = 250

func (a *app) readQueue(ctx context.Context, st pageState) (listPage, error) {
	var out listPage
	cursor, seenCursors, seenIDs := "", map[string]bool{}, map[string]bool{}
	for n := 0; n <= st.Page; n++ {
		page, err := a.client.list(ctx, listParams{Location: st.Location, Cursor: cursor})
		if err != nil {
			return listPage{}, err
		}
		for _, item := range page.Results {
			if strings.TrimSpace(item.ID) == "" || seenIDs[item.ID] {
				continue
			}
			if st.Query != "" && !strings.Contains(strings.ToLower(item.Title+" "+item.Author+" "+item.SiteName+" "+item.Notes), strings.ToLower(st.Query)) {
				continue
			}
			seenIDs[item.ID] = true
			out.Results = append(out.Results, item)
		}
		out.Count = len(out.Results)
		out.Cursor = page.Cursor
		if page.Cursor == "" {
			return out, nil
		}
		if seenCursors[page.Cursor] {
			return listPage{}, errors.New("Readwise repeated its pagination cursor")
		}
		seenCursors[page.Cursor], cursor = true, page.Cursor
	}
	out.Count = len(out.Results)
	return out, nil
}

func documentKey(id string) string {
	key := "document-" + id
	if len(key) <= 128 {
		return key
	}
	sum := sha256.Sum256([]byte(id))
	return "document-" + hex.EncodeToString(sum[:])
}

func clampRatio(value float64) float64 {
	if math.IsNaN(value) || value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func documentTitle(item document) string {
	for _, candidate := range []string{item.Title, item.SourceURL, item.ID} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return "Untitled"
}

func documentRow(item document) wire.Component {
	var parts []string
	if s := strings.TrimSpace(item.SiteName); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(item.Author); s != "" {
		parts = append(parts, s)
	}
	parts = append(parts, fmt.Sprintf("%.0f%%", clampRatio(item.ReadingProgress)*100))
	return wire.Component{Type: wire.ComponentRow, Key: documentKey(item.ID), Title: clipText(documentTitle(item), 2000), Subtitle: clipText(strings.Join(parts, " · "), 2000), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID}}}
}

// httpsLink reports whether a link component can carry raw: the view check refuses the whole view for any other.
func httpsLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

func detailView(r *ext.UIRequest, st pageState, item document, highlights []document) (*wire.View, error) {
	t := textsFor(r.Locale)
	text, err := articleText(item.HTMLContent)
	if err != nil {
		return nil, err
	}
	title := documentTitle(item)
	ratio := clampRatio(item.ReadingProgress)
	var items []wire.Component
	var meta []string
	if s := strings.TrimSpace(item.Author); s != "" {
		meta = append(meta, s)
	}
	if s := strings.TrimSpace(item.SiteName); s != "" {
		meta = append(meta, s)
	}
	if len(meta) > 0 {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(strings.Join(meta, " · "), wire.StringMost), Style: wire.TextStyleSecondary})
	}
	items = append(items,
		wire.Component{Type: wire.ComponentProgress, Label: fmt.Sprintf(t.progress, ratio*100), Ratio: &ratio},
		wire.Component{Type: wire.ComponentButton, Title: t.move, Action: &wire.Action{ID: "shortlist", Confirm: t.move + "\n" + clipText(title, 500)}},
	)
	if httpsLink(item.SourceURL) {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: item.SourceURL})
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	v := &wire.View{Title: clipText(title, 2000), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.read, Items: items}}}
	used := len(items)
	if item.Notes != "" {
		if note := capComponents(textComponents(item.Notes), maxNoteItems); len(note) > 0 {
			used += len(note)
			v.Sections = append(v.Sections, wire.Section{Title: t.note, Items: note})
		}
	}
	if len(highlights) > 0 {
		section := wire.Section{Title: t.highlights}
		for _, h := range highlights {
			section.Items = append(section.Items, textComponents(h.Content)...)
			section.Items = append(section.Items, textComponents(h.Notes)...)
		}
		section.Items = capComponents(section.Items, maxHighlightItems)
		if len(section.Items) > 0 {
			used += len(section.Items)
			v.Sections = append(v.Sections, section)
		}
	}
	if body := capComponents(textComponents(text), maxViewItems-used); len(body) > 0 {
		v.Sections = append(v.Sections, wire.Section{Title: t.read, Items: body})
	} else {
		v.Sections = append(v.Sections, wire.Section{Items: []wire.Component{{Type: wire.ComponentText, Text: t.noText, Style: wire.TextStyleSecondary}}})
	}
	return v, nil
}

const (
	// maxViewItems keeps the document view within the hub's 300 component limit, whatever the document holds.
	maxViewItems = 250
	// maxNoteItems and maxHighlightItems leave the article text room when the notes or the highlights are many.
	maxNoteItems      = 20
	maxHighlightItems = 120
)

// capComponents keeps the first n components and shows the cut on the last one.
func capComponents(items []wire.Component, n int) []wire.Component {
	if n <= 0 {
		return nil
	}
	if len(items) <= n {
		return items
	}
	items = items[:n]
	items[n-1].Text = clipText(items[n-1].Text, 1700) + " […]"
	return items
}

func saveView(r *ext.UIRequest, st pageState) *wire.View {
	t := textsFor(r.Locale)
	return &wire.View{Title: t.save, State: ext.EncodeState(st), Sections: []wire.Section{{Items: []wire.Component{
		{Type: wire.ComponentField, Kind: "text", Name: "url", Label: "URL", Placeholder: "https://example.com/article"},
		{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "review-save", Submit: true}},
	}}}}
}

func reviewSaveView(r *ext.UIRequest, st pageState) *wire.View {
	t := textsFor(r.Locale)
	shown := clipText(strings.TrimSpace(st.SaveURL), wire.StringMost)
	if shown == "" {
		shown = t.save
	}
	return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Items: []wire.Component{
		{Type: wire.ComponentText, Text: shown},
		{Type: wire.ComponentButton, Title: t.confirm, Action: &wire.Action{ID: "save", Confirm: t.save + "\n" + st.SaveURL}},
	}}}}
}

func textComponents(text string) []wire.Component {
	var out []wire.Component
	for _, line := range strings.Split(text, "\n") {
		r := []rune(line)
		for len(r) > 0 {
			n := min(len(r), 2000)
			out = append(out, wire.Component{Type: wire.ComponentText, Text: string(r[:n])})
			r = r[n:]
		}
	}
	return out
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func quote(text string) string {
	out, _ := json.Marshal(text)
	return string(out)
}
