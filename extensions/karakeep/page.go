package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	// pageLimit is how many bookmarks one page of the bookmarks view requests.
	pageLimit     = 50
	maxQueryChars = 200
	maxViewItems  = 250
	maxNoteItems  = 40
	maxTextItems  = 120
)

type pageState struct {
	Query      string `json:"q,omitempty"`
	Cursor     string `json:"c,omitempty"`
	BookmarkID string `json:"b,omitempty"`
	Mode       string `json:"m,omitempty"`
	DraftID    string `json:"d,omitempty"`
	SaveURL    string `json:"u,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || len([]rune(st.Query)) > maxQueryChars || len(st.Cursor) > 256 || len(st.BookmarkID) > 128 || len(st.DraftID) > 128 || len(st.SaveURL) > 8192 {
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
		st.Query, st.Cursor, st.Mode = strings.TrimSpace(clipText(r.Values["q"], maxQueryChars)), "", "list"
	case "more":
		if st.Cursor == "" {
			return nil, errors.New("no further bookmarks")
		}
	case "detail":
		if err := validID(r.Values["id"]); err != nil {
			return nil, err
		}
		st.BookmarkID, st.Mode, st.DraftID = r.Values["id"], "detail", randomID()
	case "back":
		st.Mode, st.BookmarkID, st.DraftID = "list", "", ""
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
			return nil, errors.New("the save form expired")
		}
		out, err := a.doAction(ctx, "ui-save:"+st.DraftID, "karakeep_save", []byte(`{"url":`+quote(st.SaveURL)+`}`))
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	case "favourite", "archive":
		if err := validID(st.BookmarkID); err != nil {
			return nil, err
		}
		if st.DraftID == "" {
			return nil, errors.New("the bookmark action form expired")
		}
		field := "favourited"
		if r.Action == "archive" {
			field = "archived"
		}
		raw := []byte(`{"id":` + quote(st.BookmarkID) + `,` + quote(field) + `:true}`)
		out, err := a.doAction(ctx, "ui-"+r.Action+":"+st.DraftID, "karakeep_update", raw)
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
	if a.client.base == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "save" {
		return saveView(r, st), nil
	}
	if st.Mode == "review-save" {
		return reviewSaveView(r, st), nil
	}
	if st.Mode == "detail" {
		item, err := a.client.bookmark(ctx, st.BookmarkID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		highlights, err := a.client.bookmarkHighlights(ctx, st.BookmarkID)
		if err != nil {
			return nil, err
		}
		if st.DraftID == "" {
			st.DraftID = randomID()
		}
		return detailView(r, st, item, highlights), nil
	}
	page, err := a.client.bookmarks(ctx, st.Query, st.Cursor, pageLimit)
	a.record(err)
	if err != nil {
		return nil, err
	}
	v := &wire.View{Title: t.bookmarks, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	section := wire.Section{Title: fmt.Sprintf(t.results, len(page.Bookmarks)), Items: []wire.Component{{
		Type: wire.ComponentButton, Title: t.save, Action: &wire.Action{ID: "save-form", Push: &wire.Push{State: ext.EncodeState(pageState{Mode: "save", DraftID: randomID()}), Title: t.save}},
	}}}
	seen := make(map[string]bool, len(page.Bookmarks))
	for _, item := range page.Bookmarks {
		if len(section.Items) >= maxViewItems {
			break
		}
		if item.ID == "" || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: boundedKey("bookmark-", item.ID), Title: clipText(item.displayTitle(), 2000), Subtitle: clipText(item.kind(), 2000), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID}}})
	}
	if len(section.Items) == 1 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noBookmarks})
	}
	if page.NextCursor != nil && *page.NextCursor != "" {
		st.Cursor = *page.NextCursor
		v.State = ext.EncodeState(st)
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

// boundedKey returns prefix+id when it fits the 128-character key limit, and a stable hashed key otherwise.
func boundedKey(prefix, id string) string {
	full := prefix + id
	if len(full) <= 128 {
		return full
	}
	sum := sha256.Sum256([]byte(id))
	suffix := "-" + hex.EncodeToString(sum[:8])
	keep := max(0, 128-len(prefix)-len(suffix))
	return prefix + id[:keep] + suffix
}

// httpsLink reports whether a link component can carry raw: the view check refuses the whole view for any other.
func httpsLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

func detailView(r *ext.UIRequest, st pageState, item bookmark, highlights []highlight) *wire.View {
	t := textsFor(r.Locale)
	items := []wire.Component{}
	title := clipText(item.displayTitle(), 2000)
	confirmTitle := clipText(title, 1800)
	if kind := item.kind(); kind != "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(kind, 2000), Style: wire.TextStyleSecondary})
	}
	if link := item.link(); httpsLink(link) {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: link})
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.favourite, Action: &wire.Action{ID: "favourite", Confirm: t.favourite + "\n" + confirmTitle}})
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.archive, Action: &wire.Action{ID: "archive", Confirm: t.archive + "\n" + confirmTitle}})
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	v := &wire.View{Title: title, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.read, Items: items}}}
	used := len(items)
	if note := capComponents(textComponents(item.Note), maxNoteItems); len(note) > 0 {
		used += len(note)
		v.Sections = append(v.Sections, wire.Section{Title: t.note, Items: note})
	}
	if body := capComponents(textComponents(item.Content.Text), maxTextItems); len(body) > 0 {
		used += len(body)
		v.Sections = append(v.Sections, wire.Section{Title: t.text, Items: body})
	}
	section := wire.Section{Title: t.highlights}
	for _, h := range highlights {
		if h.BookmarkID != item.ID {
			continue
		}
		section.Items = append(section.Items, textComponents(h.Text)...)
		section.Items = append(section.Items, textComponents(h.Note)...)
	}
	section.Items = capComponents(section.Items, maxViewItems-used)
	if len(section.Items) > 0 {
		v.Sections = append(v.Sections, section)
	}
	return v
}

// capComponents keeps the first n components and shows the cut on the last one.
func capComponents(items []wire.Component, n int) []wire.Component {
	if n <= 0 || len(items) == 0 {
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
	displayURL := clipText(st.SaveURL, 1800)
	return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Items: []wire.Component{
		{Type: wire.ComponentText, Text: displayURL},
		{Type: wire.ComponentButton, Title: t.confirm, Action: &wire.Action{ID: "save", Confirm: t.save + "\n" + displayURL}},
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
