package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Mode        string `json:"m,omitempty"`
	BookID      int64  `json:"b,omitempty"`
	BookName    string `json:"n,omitempty"`
	HighlightID int64  `json:"i,omitempty"`
	Page        int    `json:"p,omitempty"`
	DraftID     string `json:"d,omitempty"`
	Text        string `json:"t,omitempty"`
	Title       string `json:"a,omitempty"`
	Author      string `json:"u,omitempty"`
	Note        string `json:"e,omitempty"`
	SourceURL   string `json:"s,omitempty"`
	TagName     string `json:"g,omitempty"`
}

const maxViewRecords = 250

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.BookID < 0 || st.HighlightID < 0 || st.Page < 0 || len([]rune(st.BookName)) > maxTitleChars || len(st.DraftID) > 128 || len([]rune(st.Text)) > maxHighlightChars || len([]rune(st.Title)) > maxTitleChars || len([]rune(st.Author)) > maxAuthorChars || len([]rune(st.Note)) > maxHighlightChars || len(st.SourceURL) > 2100 || len([]rune(st.TagName)) > maxTagNameChars {
		return pageState{}, errors.New("invalid page state")
	}
	switch st.Mode {
	case "", "highlights", "detail", "create", "review-create", "edit-note", "review-note", "add-tag", "review-tag":
	default:
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
	case "book":
		id, err := strconv.ParseInt(r.Values["id"], 10, 64)
		if err != nil || id <= 0 || r.Values["name"] == "" || len([]rune(r.Values["name"])) > maxTitleChars {
			return nil, errors.New("invalid book")
		}
		st = pageState{Mode: "highlights", BookID: id, BookName: r.Values["name"], Page: 1}
	case "books":
		st = pageState{}
	case "more":
		st.Page++
	case "detail":
		id, err := strconv.ParseInt(r.Values["id"], 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("invalid highlight ID")
		}
		st.Mode, st.HighlightID = "detail", id
	case "create":
		if st.Mode != "review-create" {
			st = pageState{Mode: "create", DraftID: randomID()}
		} else {
			st.Mode = "create"
		}
	case "edit-note":
		st.Mode = "edit-note"
		if st.DraftID == "" {
			st.DraftID = randomID()
		}
	case "add-tag":
		if st.Mode != "review-tag" {
			st = pageState{Mode: "add-tag", BookID: st.BookID, BookName: st.BookName, DraftID: randomID()}
		} else {
			st.Mode = "add-tag"
		}
	case "back":
		st.Mode, st.HighlightID, st.DraftID = "highlights", 0, ""
		st.Text, st.Title, st.Author, st.Note, st.SourceURL, st.TagName = "", "", "", "", "", ""
	case "review-create":
		in := createArgs{Text: r.Values["text"], Title: r.Values["title"], Author: r.Values["author"], Note: r.Values["note"], SourceURL: r.Values["source_url"]}
		if err := in.validate(); err != nil {
			return nil, err
		}
		st.Text, st.Title, st.Author, st.Note, st.SourceURL, st.Mode = in.Text, in.Title, in.Author, in.Note, in.SourceURL, "review-create"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("highlight draft exceeds the web state size limit")
		}
	case "review-note":
		if err := validText(r.Values["note"], maxHighlightChars, true); err != nil {
			return nil, err
		}
		st.Note, st.Mode = r.Values["note"], "review-note"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("note draft exceeds the web state size limit")
		}
	case "review-tag":
		if strings.TrimSpace(r.Values["tag_name"]) == "" || validText(r.Values["tag_name"], maxTagNameChars, false) != nil {
			return nil, errors.New("invalid book tag")
		}
		st.TagName, st.Mode = r.Values["tag_name"], "review-tag"
	case "confirm-create":
		if st.DraftID == "" {
			return nil, errors.New("draft is required")
		}
		raw, _ := json.Marshal(createArgs{Text: st.Text, Title: st.Title, Author: st.Author, Note: st.Note, SourceURL: st.SourceURL})
		out, err := a.doAction(ctx, "ui-create:"+st.DraftID, "readwise_library_create_highlight", raw)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	case "confirm-note":
		if st.DraftID == "" {
			return nil, errors.New("draft is required")
		}
		raw, _ := json.Marshal(noteArgs{HighlightID: st.HighlightID, Note: st.Note})
		out, err := a.doAction(ctx, "ui-note:"+st.DraftID, "readwise_library_update_note", raw)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	case "confirm-tag":
		if st.DraftID == "" {
			return nil, errors.New("draft is required")
		}
		raw, _ := json.Marshal(tagArgs{BookID: st.BookID, Name: st.TagName})
		out, err := a.doAction(ctx, "ui-tag:"+st.DraftID, "readwise_library_add_book_tag", raw)
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
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	switch st.Mode {
	case "create", "review-create", "edit-note", "review-note", "add-tag", "review-tag":
		return a.editorView(r, st), nil
	case "detail":
		item, err := a.client.highlight(ctx, st.HighlightID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		st.DraftID = randomID()
		return detailView(r, st, item), nil
	case "highlights":
		if err := validID(st.BookID); err != nil {
			return nil, err
		}
		if st.Page < 1 {
			st.Page = 1
		}
		page, err := a.client.highlights(ctx, st.BookID, st.Page)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{}
		seenHighlights := map[int64]bool{}
		for _, item := range page.Results {
			if item.ID <= 0 || item.Text == "" || seenHighlights[item.ID] {
				continue
			}
			seenHighlights[item.ID] = true
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "highlight-" + fmt.Sprint(item.ID), Title: clipText(item.Text, 1800), Subtitle: clipText(item.Note, 500), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": fmt.Sprint(item.ID)}}})
			if len(section.Items) >= maxViewRecords {
				break
			}
		}
		section.Title = fmt.Sprintf(t.highlights, len(section.Items))
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noHighlights})
		}
		if nextPage(page.Next, st.Page) > 0 {
			section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
		}
		v := &wire.View{Title: clipText(st.BookName, 1800), State: ext.EncodeState(st), Sections: []wire.Section{section, {Title: t.highlights, Items: []wire.Component{
			{Type: wire.ComponentButton, Title: t.addTag, Action: &wire.Action{ID: "add-tag", Push: &wire.Push{Title: t.addTag, State: ext.EncodeState(pageState{Mode: "add-tag", BookID: st.BookID, BookName: st.BookName, DraftID: randomID()})}}},
			{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "books"}},
		}}}}
		return v, nil
	case "":
		page, err := a.client.books(ctx, "articles", 1)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{Title: t.books}
		seenBooks := map[int64]bool{}
		for _, item := range page.Results {
			if item.ID <= 0 || item.Title == "" || seenBooks[item.ID] {
				continue
			}
			seenBooks[item.ID] = true
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "book-" + fmt.Sprint(item.ID), Title: clipText(item.Title, 1800), Subtitle: clipText(item.Author+" · "+fmt.Sprint(item.NumHighlights)+" highlights", 1800), Action: &wire.Action{ID: "book", Values: map[string]string{"id": fmt.Sprint(item.ID), "name": clipText(item.Title, maxTitleChars)}}})
			if len(section.Items) >= maxViewRecords {
				break
			}
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noBooks})
		}
		v := &wire.View{Title: t.library, State: ext.EncodeState(st), Sections: []wire.Section{section, {Title: t.library, Items: []wire.Component{
			{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "create", Push: &wire.Push{Title: t.create, State: ext.EncodeState(pageState{Mode: "create", DraftID: randomID()})}}},
		}}}}
		return v, nil
	}
	return nil, errors.New("invalid library view")
}

func (a *app) editorView(r *ext.UIRequest, st pageState) *wire.View {
	t := textsFor(r.Locale)
	if st.Mode == "review-create" || st.Mode == "review-note" || st.Mode == "review-tag" {
		action, back := "confirm-create", "create"
		if st.Mode == "review-note" {
			action, back = "confirm-note", "edit-note"
		} else if st.Mode == "review-tag" {
			action, back = "confirm-tag", "add-tag"
		}
		summary := strings.TrimSpace(st.Title + st.Note + st.TagName)
		if summary == "" {
			summary = t.note
		}
		items := []wire.Component{{Type: wire.ComponentText, Text: clipText(summary, 1800)}}
		if st.Mode == "review-create" {
			items = append(items, textComponents(st.Text)...)
		}
		items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.save, Action: &wire.Action{ID: action, Confirm: t.save + "\n" + clipText(summary, 500)}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: back}})
		return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.review, Items: items}}}
	}
	var items []wire.Component
	switch st.Mode {
	case "create":
		items = []wire.Component{
			{Type: wire.ComponentField, Kind: "text", Name: "text", Label: t.text, Value: clipText(st.Text, wire.StringMost)},
			{Type: wire.ComponentField, Kind: "text", Name: "title", Label: t.title, Value: clipText(st.Title, wire.StringMost)},
			{Type: wire.ComponentField, Kind: "text", Name: "author", Label: t.author, Value: clipText(st.Author, wire.StringMost)},
			{Type: wire.ComponentField, Kind: "text", Name: "note", Label: t.note, Value: clipText(st.Note, wire.StringMost)},
			{Type: wire.ComponentField, Kind: "text", Name: "source_url", Label: t.sourceURL, Value: clipText(st.SourceURL, wire.StringMost)},
		}
	case "edit-note":
		items = []wire.Component{{Type: wire.ComponentField, Kind: "text", Name: "note", Label: t.note, Value: clipText(st.Note, wire.StringMost)}}
	case "add-tag":
		items = []wire.Component{{Type: wire.ComponentField, Kind: "text", Name: "tag_name", Label: t.addTag, Value: clipText(st.TagName, wire.StringMost)}}
	}
	action := map[string]string{"create": "review-create", "edit-note": "review-note", "add-tag": "review-tag"}[st.Mode]
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: action, Submit: true}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Items: items}}}
}

func detailView(r *ext.UIRequest, st pageState, item highlight) *wire.View {
	t := textsFor(r.Locale)
	items := []wire.Component{{Type: wire.ComponentText, Text: clipText(fmt.Sprintf("%s %d", item.LocationType, item.Location), 1800)}}
	if link := safeLibraryLink(item.ReadwiseURL); link != "" {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: link})
	}
	editDraft := randomID()
	editState := ext.EncodeState(pageState{Mode: "edit-note", HighlightID: item.ID, Note: item.Note, DraftID: editDraft})
	if len(editState) > wire.StateMost {
		editState = ext.EncodeState(pageState{Mode: "edit-note", HighlightID: item.ID, DraftID: editDraft})
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.edit, Action: &wire.Action{ID: "edit-note", Push: &wire.Push{Title: t.edit, State: editState}}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	passage := capComponents(textComponents(item.Text), maxPassageItems)
	v := &wire.View{Title: fmt.Sprintf("Highlight #%d", item.ID), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.library, Items: items}, {Title: t.text, Items: passage}}}
	if item.Note != "" {
		note := capComponents(textComponents(item.Note), maxViewItems-len(items)-len(passage))
		if len(note) > 0 {
			v.Sections = append(v.Sections, wire.Section{Title: t.note, Items: note})
		}
	}
	return v
}

const (
	// maxViewItems keeps the highlight detail view within the SDK's 300 component limit across passage and note.
	maxViewItems    = 250
	maxPassageItems = 150
)

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

func textComponents(text string) []wire.Component {
	var out []wire.Component
	for _, line := range strings.Split(text, "\n") {
		r := []rune(line)
		for len(r) > 0 {
			n := min(len(r), 1800)
			out = append(out, wire.Component{Type: wire.ComponentText, Text: string(r[:n])})
			r = r[n:]
			if len(out) >= 240 {
				out[len(out)-1].Text = clipText(out[len(out)-1].Text, 1700) + " […]"
				return out
			}
		}
	}
	if len(out) == 0 {
		out = append(out, wire.Component{Type: wire.ComponentText, Text: " ", Style: wire.TextStyleSecondary})
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
