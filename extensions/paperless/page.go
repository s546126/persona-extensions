package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Mode       string `json:"m,omitempty"`
	Query      string `json:"q,omitempty"`
	Page       int    `json:"p,omitempty"`
	DocumentID int64  `json:"i,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Page < 0 || st.DocumentID < 0 || len([]rune(st.Query)) > maxQueryChars {
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
		st.Mode, st.Query, st.Page = "", strings.TrimSpace(clipText(r.Values["q"], maxQueryChars)), 1
	case "more":
		st.Page, err = strconv.Atoi(r.Values["page"])
		if err != nil || st.Page < 2 {
			return nil, errors.New("invalid next page")
		}
	case "detail":
		st.DocumentID, err = strconv.ParseInt(r.Values["id"], 10, 64)
		if err != nil || st.DocumentID <= 0 {
			return nil, errors.New("invalid document ID")
		}
		st.Mode = "detail"
	case "back":
		st.Mode, st.DocumentID = "", 0
	case "lookup":
		if r.Values["kind"] != "tags" && r.Values["kind"] != "correspondents" {
			return nil, errors.New("invalid lookup")
		}
		st = pageState{Mode: r.Values["kind"]}
	default:
		return nil, fmt.Errorf("unknown page action %q", r.Action)
	}
	v, err := a.page(ctx, r, st)
	return &wire.ActionResult{View: v}, err
}

const (
	// maxViewRecords is half the SDK's component limit: a document can render a row and a search highlight line.
	maxViewRecords = 125
	maxViewLookup  = 250
	maxViewItems   = 250
	maxOCRItems    = 180
)

// collectDocuments reads page 1 through the current page so that Load more answers the whole list, longer. It stops at
// maxViewRecords to keep the view within the SDK's component limit, and reports whether another page can be loaded
// (HasMore, NextPage) and the total Paperless-ngx counts (Count) as of the last page read.
func (a *app) collectDocuments(ctx context.Context, query string, current int) (documentPage, error) {
	var out documentPage
	seen := map[int64]bool{}
	for pageNumber := 1; pageNumber <= current && len(out.Items) < maxViewRecords; {
		batch, err := a.client.documents(ctx, query, pageNumber)
		if err != nil {
			return documentPage{}, err
		}
		for _, item := range batch.Items {
			if item.ID <= 0 || seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			out.Items = append(out.Items, item)
		}
		out.Count, out.HasMore, out.NextPage = batch.Count, batch.HasMore, batch.NextPage
		if !batch.HasMore {
			break
		}
		pageNumber = batch.NextPage
	}
	if len(out.Items) > maxViewRecords {
		out.Items = out.Items[:maxViewRecords]
	}
	if len(out.Items) >= maxViewRecords {
		out.HasMore = false
	}
	return out, nil
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if a.client.base == "" || a.client.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "detail" {
		item, err := a.client.document(ctx, st.DocumentID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item, a.client.originalURL(item.ID)), nil
	}
	if st.Mode == "tags" || st.Mode == "correspondents" {
		page, err := a.client.named(ctx, st.Mode, 1)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{Title: t.lookup(st.Mode)}
		for _, item := range page.Results {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(fmt.Sprintf("%s · %d", item.Name, item.DocumentCount), 1800)})
		}
		section.Items = capComponents(section.Items, maxViewLookup)
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noLookup})
		}
		return &wire.View{Title: t.lookup(st.Mode), State: ext.EncodeState(st), Sections: []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}}}}}}, nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid document view")
	}
	if st.Page == 0 {
		st.Page = 1
	}
	page, err := a.collectDocuments(ctx, st.Query, st.Page)
	a.record(err)
	if err != nil {
		return nil, err
	}
	v := &wire.View{Title: t.documents, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	section := wire.Section{Title: fmt.Sprintf(t.results, page.Count)}
	seen := make(map[int64]bool, len(page.Items))
	for _, item := range page.Items {
		if item.ID <= 0 || item.Title == "" || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: fmt.Sprintf("document-%d", item.ID), Title: clipText(item.Title, 1800), Subtitle: clipText(fmt.Sprintf("%s · %d %s", item.Created, item.PageCount, t.pages), 1800), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": fmt.Sprint(item.ID)}}})
		if hl := plainText(item.SearchHit.Highlights); hl != "" {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(hl, 1800), Style: wire.TextStyleSecondary})
		}
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noDocuments})
	}
	if page.HasMore {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more", Values: map[string]string{"page": fmt.Sprint(page.NextPage)}}}
	}
	v.Sections = []wire.Section{section, {Items: []wire.Component{
		{Type: wire.ComponentButton, Title: t.tags, Action: &wire.Action{ID: "lookup", Values: map[string]string{"kind": "tags"}, Push: &wire.Push{Title: t.tags, State: ext.EncodeState(pageState{Mode: "tags"})}}},
		{Type: wire.ComponentButton, Title: t.correspondents, Action: &wire.Action{ID: "lookup", Values: map[string]string{"kind": "correspondents"}, Push: &wire.Push{Title: t.correspondents, State: ext.EncodeState(pageState{Mode: "correspondents"})}}},
	}}}
	return v, nil
}

func detailView(r *ext.UIRequest, st pageState, item document, originalURL string) *wire.View {
	t := textsFor(r.Locale)
	items := []wire.Component{{Type: wire.ComponentText, Text: clipText(fmt.Sprintf("%s · %d %s · %s", item.Created, item.PageCount, t.pages, item.OriginalFileName), 1800)}}
	if originalURL != "" {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.download, URL: originalURL})
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	v := &wire.View{Title: clipText(item.Title, 1800), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.documents, Items: items}}}
	used := len(items)
	ocr := capComponents(textComponents(clipText(item.Content, 50000)), maxOCRItems)
	used += len(ocr)
	v.Sections = append(v.Sections, wire.Section{Title: t.ocr, Items: ocr})
	if len(item.Notes) > 0 {
		section := wire.Section{Title: t.notes}
		for _, n := range item.Notes {
			if strings.TrimSpace(n.Note) != "" {
				section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(n.User.Username+": "+n.Note, 1800)})
			}
		}
		section.Items = capComponents(section.Items, maxViewItems-used)
		if len(section.Items) > 0 {
			v.Sections = append(v.Sections, section)
		}
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

func textComponents(text string) []wire.Component {
	var out []wire.Component
	for _, line := range strings.Split(text, "\n") {
		r := []rune(line)
		for len(r) > 0 {
			n := min(len(r), 1800)
			out = append(out, wire.Component{Type: wire.ComponentText, Text: string(r[:n])})
			r = r[n:]
			if len(out) >= 220 {
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
