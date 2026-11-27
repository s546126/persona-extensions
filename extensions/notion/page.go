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

const maxViewItems = 250

type pageState struct {
	Query    string `json:"q,omitempty"`
	Cursor   string `json:"c,omitempty"`
	PageID   string `json:"p,omitempty"`
	Mode     string `json:"m,omitempty"`
	DraftID  string `json:"d,omitempty"`
	Title    string `json:"t,omitempty"`
	Content  string `json:"b,omitempty"`
	ParentID string `json:"r,omitempty"`
	Kind     string `json:"k,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || len([]rune(st.Query)) > 200 || len([]rune(st.Title)) > 500 || len([]rune(st.Content)) > 2000 || len(st.Cursor) > 256 || len(st.PageID) > 128 || len(st.DraftID) > 128 || len(st.ParentID) > 128 {
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
		if len([]rune(r.Values["q"])) > 200 {
			return nil, errors.New("search query is too long")
		}
		st.Query, st.Cursor, st.Mode = r.Values["q"], "", "list"
	case "more":
		if st.Cursor == "" {
			return nil, errors.New("no further page")
		}
	case "detail":
		if err := validNotionID(r.Values["id"]); err != nil {
			return nil, err
		}
		st.PageID, st.Mode = r.Values["id"], "detail"
	case "back":
		st.Mode, st.PageID = "list", ""
	case "create":
		st.Mode, st.DraftID = "create", randomID()
	case "review-create":
		if err := validNotionID(r.Values["parent_id"]); err != nil {
			return nil, err
		}
		if err := validNote(r.Values["title"], 500); err != nil {
			return nil, err
		}
		if r.Values["content"] != "" {
			if err := validNote(r.Values["content"], 2000); err != nil {
				return nil, err
			}
		}
		st.ParentID, st.Title, st.Content, st.Mode = r.Values["parent_id"], r.Values["title"], r.Values["content"], "review"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("page draft exceeds page state limit")
		}
	case "confirm-create":
		if st.DraftID == "" {
			return nil, errors.New("the create form expired")
		}
		raw, _ := json.Marshal(map[string]string{"parent_id": st.ParentID, "title": st.Title, "content": st.Content})
		out, err := a.doAction(ctx, "ui-create:"+st.DraftID, "notion_create", raw)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	case "append":
		st.Mode, st.DraftID = "append", randomID()
	case "review-append":
		if err := validNotionID(st.PageID); err != nil {
			return nil, err
		}
		if r.Values["kind"] != "paragraph" && r.Values["kind"] != "todo" {
			return nil, errors.New("choose paragraph or todo")
		}
		if err := validNote(r.Values["text"], 2000); err != nil {
			return nil, err
		}
		st.Kind, st.Content, st.Mode = r.Values["kind"], r.Values["text"], "review-append"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("note draft exceeds page state limit")
		}
	case "confirm-append":
		if st.DraftID == "" {
			return nil, errors.New("the note form expired")
		}
		raw, _ := json.Marshal(map[string]string{"page_id": st.PageID, "kind": st.Kind, "text": st.Content})
		out, err := a.doAction(ctx, "ui-append:"+st.DraftID, "notion_append", raw)
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
	if st.Mode == "create" {
		return a.createView(ctx, r, st)
	}
	if st.Mode == "append" {
		return a.appendView(r, st), nil
	}
	if st.Mode == "review-append" {
		return a.reviewAppendView(r, st), nil
	}
	if st.Mode == "review" {
		return a.reviewView(r, st), nil
	}
	if st.Mode == "detail" {
		page, err := a.client.page(ctx, st.PageID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		lines, err := a.readBlocks(ctx, st.PageID, 0)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, page, lines), nil
	}
	page, err := a.client.search(ctx, st.Query, st.Cursor)
	a.record(err)
	if err != nil {
		return nil, err
	}
	v := &wire.View{Title: t.pages, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	rows := make([]wire.Component, 0, len(page.Results))
	seen := map[string]bool{}
	for _, item := range page.Results {
		if seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		rows = append(rows, wire.Component{
			Type:     wire.ComponentRow,
			Key:      boundedKey("page-", item.ID),
			Title:    clipText(pageTitle(item), 2000),
			Subtitle: clipText(item.LastEditedTime, 2000),
			Action:   &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID}},
		})
	}
	section := wire.Section{Title: fmt.Sprintf(t.results, len(rows)), Items: []wire.Component{{
		Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "create", Push: &wire.Push{State: ext.EncodeState(pageState{Mode: "create", DraftID: randomID()}), Title: t.create}},
	}}}
	section.Items = append(section.Items, rows...)
	if len(rows) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noPages})
	}
	if page.HasMore && page.NextCursor != nil && *page.NextCursor != "" {
		st.Cursor = *page.NextCursor
		v.State = ext.EncodeState(st)
		section.More = &wire.More{Title: "Load more", Action: wire.Action{ID: "more"}}
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

func httpsLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

func detailView(r *ext.UIRequest, st pageState, page notionPage, lines []string) *wire.View {
	t := textsFor(r.Locale)
	items := []wire.Component{}
	if httpsLink(page.URL) {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: page.URL})
	}
	items = append(items,
		wire.Component{Type: wire.ComponentButton, Title: t.append, Action: &wire.Action{ID: "append", Push: &wire.Push{State: ext.EncodeState(pageState{Mode: "append", PageID: st.PageID, DraftID: randomID()}), Title: t.append}}},
		wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	)
	limit := maxViewItems - len(items)
	var body []wire.Component
	for _, line := range lines {
		if len(body) > limit {
			break
		}
		body = append(body, textComponents(line)...)
	}
	body = capComponents(body, limit)
	if len(body) == 0 {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: t.noContent, Style: wire.TextStyleSecondary})
	} else {
		items = append(items, body...)
	}
	return &wire.View{Title: clipText(pageTitle(page), 2000), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.read, Items: items}}}
}

func (a *app) createView(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	pages, err := a.client.search(ctx, "", "")
	if err != nil {
		return nil, err
	}
	choices := []wire.Choice{}
	seen := map[string]bool{}
	for _, item := range pages.Results {
		if seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		choices = append(choices, wire.Choice{Value: item.ID, Title: clipText(pageTitle(item), 2000)})
	}
	if len(choices) == 0 {
		return nil, errors.New("no shared Notion page can be a parent")
	}
	return &wire.View{Title: t.create, State: ext.EncodeState(st), Sections: []wire.Section{{Items: []wire.Component{
		{Type: wire.ComponentField, Kind: "choice", Name: "parent_id", Label: t.parent, Value: choices[0].Value, Choices: choices},
		{Type: wire.ComponentField, Kind: "text", Name: "title", Label: t.title},
		{Type: wire.ComponentField, Kind: "text", Name: "content", Label: t.content},
		{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "review-create", Submit: true}},
	}}}}, nil
}

func (a *app) appendView(r *ext.UIRequest, st pageState) *wire.View {
	t := textsFor(r.Locale)
	return &wire.View{Title: t.append, State: ext.EncodeState(st), Sections: []wire.Section{{Items: []wire.Component{
		{Type: wire.ComponentField, Kind: "choice", Name: "kind", Label: t.content, Value: "paragraph", Choices: []wire.Choice{{Value: "paragraph", Title: t.paragraph}, {Value: "todo", Title: t.todo}}},
		{Type: wire.ComponentField, Kind: "text", Name: "text", Label: t.append},
		{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "review-append", Submit: true}},
	}}}}
}

func (a *app) reviewAppendView(r *ext.UIRequest, st pageState) *wire.View {
	t := textsFor(r.Locale)
	items := capComponents(textComponents(st.Content), maxViewItems-2)
	items = append(items,
		wire.Component{Type: wire.ComponentText, Text: clipText(t.parent+": "+st.PageID, 2000)},
		wire.Component{Type: wire.ComponentButton, Title: t.save, Action: &wire.Action{ID: "confirm-append", Confirm: clipText(t.append+"\n"+st.Content, 1800)}},
	)
	return &wire.View{Title: t.append, State: ext.EncodeState(st), Sections: []wire.Section{{Items: items}}}
}

func (a *app) reviewView(r *ext.UIRequest, st pageState) *wire.View {
	t := textsFor(r.Locale)
	title := clipText(strings.TrimSpace(st.Title), 2000)
	if title == "" {
		title = t.create
	}
	items := []wire.Component{
		{Type: wire.ComponentText, Text: title},
		{Type: wire.ComponentText, Text: clipText(t.parent+": "+st.ParentID, 2000)},
	}
	items = append(items, capComponents(textComponents(st.Content), maxViewItems-len(items)-1)...)
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.confirm, Action: &wire.Action{ID: "confirm-create", Confirm: clipText(t.create+"\n"+title, 1800)}})
	return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Items: items}}}
}

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

func boundedKey(prefix, id string) string {
	key := prefix + id
	if len(key) <= 128 {
		return key
	}
	sum := sha256.Sum256([]byte(id))
	return prefix + hex.EncodeToString(sum[:16])
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
