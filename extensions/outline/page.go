package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Mode         string `json:"m,omitempty"`
	Query        string `json:"q,omitempty"`
	Offset       int    `json:"o,omitempty"`
	DocumentID   string `json:"i,omitempty"`
	CollectionID string `json:"c,omitempty"`
	Title        string `json:"t,omitempty"`
	Text         string `json:"b,omitempty"`
	DraftID      string `json:"d,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Offset < 0 || st.Offset > 1000000 || len([]rune(st.Query)) > maxQueryChars || len(st.DocumentID) > 128 || len(st.CollectionID) > 128 || len([]rune(st.Title)) > 150 || len(st.Text) > 11000 || len(st.DraftID) > 128 {
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
		st.Query, st.Offset = strings.TrimSpace(clipText(r.Values["q"], maxQueryChars)), 0
	case "more":
		st.Offset = intFromString(r.Values["offset"])
		if st.Offset <= 0 {
			return nil, errors.New("invalid next page")
		}
	case "detail":
		if !validDocumentID(r.Values["id"]) {
			return nil, errors.New("invalid document ID")
		}
		st.Mode, st.DocumentID = "detail", r.Values["id"]
	case "back":
		st.Mode, st.DocumentID = "", ""
	case "collections":
		st = pageState{Mode: "collections"}
	case "create":
		if st.Mode != "review-create" {
			st = pageState{Mode: "create", CollectionID: r.Values["collection_id"], DraftID: randomID()}
		} else {
			st.Mode = "create"
		}
	case "personal":
		st = pageState{Mode: "create", CollectionID: "personal", DraftID: randomID()}
	case "review-create":
		in := createArgs{CollectionID: st.CollectionID, Title: strings.TrimSpace(r.Values["title"]), Text: strings.TrimSpace(r.Values["text"])}
		if err := in.validate(); err != nil || len(in.Text) > 5000 {
			return nil, errors.New("the Web draft must be valid and at most 5000 UTF-8 bytes")
		}
		st.Title, st.Text, st.Mode = in.Title, in.Text, "review-create"
	case "confirm-create":
		in := createArgs{CollectionID: st.CollectionID, Title: st.Title, Text: st.Text}
		if err := in.validate(); err != nil || st.DraftID == "" || st.Mode != "review-create" || len(st.Text) > 5000 {
			return nil, errors.New("a valid reviewed document draft is required")
		}
		raw, _ := json.Marshal(in)
		out, err := a.doAction(ctx, "ui-create:"+st.DraftID, "outline_create_document", raw)
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

// maxViewRecords is half the SDK's component limit: a result renders a row and a context line.
const (
	maxViewRecords = 125
	maxViewPages   = 10
)

func documentKey(id string) string {
	key := "document-" + id
	if len(key) <= 128 {
		return key
	}
	sum := sha256.Sum256([]byte(id))
	return "document-" + hex.EncodeToString(sum[:])
}

func documentTitle(item document) string {
	for _, candidate := range []string{item.Title, item.ID} {
		if s := strings.TrimSpace(candidate); s != "" {
			return s
		}
	}
	return "Document"
}

// collectResults reads the batches from the start through the one at offset so that Load more answers the whole list,
// longer. It stops at maxViewRecords to keep the view within the SDK's component limit, and reports whether another
// batch can be loaded and where it starts.
func (a *app) collectResults(ctx context.Context, query string, offset int) ([]searchResult, int, bool, error) {
	var out []searchResult
	seenIDs := map[string]bool{}
	seenOffsets := map[int]bool{}
	next, more, pages := 0, false, 0
	for pos := 0; len(out) < maxViewRecords && pages < maxViewPages; pos = next {
		if seenOffsets[pos] {
			more = false
			break
		}
		seenOffsets[pos] = true
		pages++
		page, err := a.client.search(ctx, query, pos)
		if err != nil {
			return nil, 0, false, err
		}
		for _, result := range page.Data {
			item := result.Document
			if !validDocumentID(item.ID) || item.Title == "" || item.ArchivedAt != "" || item.DeletedAt != "" || seenIDs[item.ID] {
				continue
			}
			seenIDs[item.ID] = true
			out = append(out, result)
			if len(out) >= maxViewRecords {
				break
			}
		}
		if next, more, err = nextOffset(page.Pagination); err != nil {
			return nil, 0, false, err
		}
		if !more || pos >= offset {
			break
		}
	}
	return out, next, more && len(out) < maxViewRecords && pages < maxViewPages && !seenOffsets[next], nil
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if a.client.base == "" || a.client.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "create" || st.Mode == "review-create" {
		return createView(r, st), nil
	}
	if st.Mode == "collections" {
		page, err := a.client.collections(ctx, 0)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{Title: t.collections}
		seenCol := map[string]bool{}
		for _, item := range page.Data {
			if item.ID == "" || item.ArchivedAt != "" || item.DeletedAt != "" || !uuidPattern.MatchString(item.ID) || seenCol[item.ID] {
				continue
			}
			seenCol[item.ID] = true
			name := strings.TrimSpace(item.Name)
			if name == "" {
				name = item.ID
			}
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "collection-" + item.ID, Title: clipText(name, 1800), Subtitle: clipText(item.Permission, 200), Action: &wire.Action{ID: "create", Values: map[string]string{"collection_id": item.ID}}})
			if len(section.Items) >= maxViewRecords {
				break
			}
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noCollections})
		}
		return &wire.View{Title: t.collections, State: ext.EncodeState(st), Sections: []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.personal, Action: &wire.Action{ID: "personal"}}, {Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}}}}}}, nil
	}
	if st.Mode == "detail" {
		item, err := a.client.document(ctx, st.DocumentID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item), nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid knowledge view")
	}
	var v *wire.View
	section := wire.Section{Title: t.results}
	if st.Query == "" {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.searchHint})
		v = &wire.View{Title: t.knowledge, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Action: wire.Action{ID: "search"}}}
	} else {
		results, next, more, err := a.collectResults(ctx, st.Query, st.Offset)
		a.record(err)
		if err != nil {
			return nil, err
		}
		for _, result := range results {
			item := result.Document
			if !validDocumentID(item.ID) || item.Title == "" || item.ArchivedAt != "" || item.DeletedAt != "" {
				continue
			}
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: documentKey(item.ID), Title: clipText(item.Title, 1800), Subtitle: clipText(item.UpdatedAt, 200), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID}}})
			if result.Context != "" {
				section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(result.Context, 1800), Style: wire.TextStyleSecondary})
			}
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noDocuments})
		}
		if more {
			section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more", Values: map[string]string{"offset": fmt.Sprint(next)}}}
		}
		v = &wire.View{Title: t.knowledge, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	}
	v.Sections = []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "collections", Push: &wire.Push{Title: t.collections, State: ext.EncodeState(pageState{Mode: "collections"})}}}}}}
	return v, nil
}

func createView(r *ext.UIRequest, st pageState) *wire.View {
	t := textsFor(r.Locale)
	if st.Mode == "review-create" {
		title := clipText(strings.TrimSpace(st.Title), 1800)
		if title == "" {
			title = t.create
		}
		items := []wire.Component{{Type: wire.ComponentText, Text: title}, {Type: wire.ComponentText, Text: t.draftNotice}}
		items = append(items, textComponents(clipText(st.Text, 10000))...)
		items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "confirm-create", Confirm: t.create + "\n" + title + "\n" + t.draftNotice}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "create"}})
		return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.create, Items: items}}}
	}
	return &wire.View{Title: t.create, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.draftNotice, Items: []wire.Component{
		{Type: wire.ComponentField, Kind: "text", Name: "title", Label: t.title, Value: clipText(st.Title, wire.StringMost)},
		{Type: wire.ComponentField, Kind: "text", Name: "text", Label: t.text, Value: clipText(st.Text, wire.StringMost)},
		{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "review-create", Submit: true}},
	}}}}
}

func detailView(r *ext.UIRequest, st pageState, item document) *wire.View {
	t := textsFor(r.Locale)
	v := &wire.View{Title: clipText(documentTitle(item), 1800), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.knowledge, Items: []wire.Component{
		{Type: wire.ComponentText, Text: clipText(t.updated+": "+item.UpdatedAt, 1800)},
		{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	}}}}
	v.Sections = append(v.Sections, wire.Section{Title: t.document, Items: textComponents(clipText(item.Text, 50000))})
	return v
}

func intFromString(raw string) int {
	var n int
	_, _ = fmt.Sscanf(raw, "%d", &n)
	return n
}

func textComponents(text string) []wire.Component {
	var out []wire.Component
	for _, line := range strings.Split(text, "\n") {
		r := []rune(line)
		for len(r) > 0 {
			n := min(len(r), 1800)
			out = append(out, wire.Component{Type: wire.ComponentText, Text: string(r[:n])})
			r = r[n:]
			if len(out) >= 200 {
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
