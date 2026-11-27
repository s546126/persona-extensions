package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Query   string `json:"q,omitempty"`
	Start   int    `json:"s,omitempty"`
	PaperID string `json:"i,omitempty"`
	Mode    string `json:"m,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Start < 0 || st.Start > maxViewPapers || len(st.PaperID) > 100 || len([]rune(st.Query)) > maxQueryChars || strings.ContainsRune(st.Query, 0) {
		return pageState{}, errors.New("invalid page state")
	}
	switch st.Mode {
	case "", "recent":
		if st.PaperID != "" && !validID(st.PaperID) {
			return pageState{}, errors.New("invalid page state")
		}
	case "detail":
		if !validID(st.PaperID) {
			return pageState{}, errors.New("invalid page state")
		}
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
	case "search":
		q := strings.ReplaceAll(r.Values["q"], "\x00", "")
		st.Query, st.Start, st.Mode = clipText(q, maxQueryChars), 0, ""
	case "recent":
		st.Query, st.Start, st.Mode = "", 0, "recent"
	case "more":
		if st.Start+20 >= maxViewPapers {
			return nil, errors.New("no more papers available")
		}
		st.Start += 20
	case "detail":
		if !validID(r.Values["id"]) {
			return nil, errors.New("invalid arXiv ID")
		}
		st.Mode, st.PaperID = "detail", r.Values["id"]
	case "back":
		st.Mode, st.PaperID = "", ""
	default:
		return nil, fmt.Errorf("unknown page action %q", r.Action)
	}
	v, err := a.page(ctx, r, st)
	return &wire.ActionResult{View: v}, err
}

// maxViewPapers is the most papers the client accepts in one response.
const maxViewPapers = 100

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if st.Mode == "detail" {
		item, err := a.client.paper(ctx, st.PaperID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item), nil
	}
	if st.Mode != "" && st.Mode != "recent" {
		return nil, errors.New("invalid paper view")
	}
	// Load more answers the whole view, longer: one request for everything from the first paper through the
	// requested batch, capped at the most entries the client accepts in a response.
	count := min(st.Start+20, maxViewPapers)
	feed, err := a.client.queryRange(ctx, st.Query, st.Mode == "recent" || st.Query == "", 0, count)
	a.record(err)
	if err != nil {
		return nil, err
	}
	v := &wire.View{Title: t.papers, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	seen := make(map[string]struct{}, len(feed.Entries))
	var rows []wire.Component
	for _, item := range feed.Entries {
		id := paperID(item)
		title := strings.Join(strings.Fields(item.Title), " ")
		if id == "" || title == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		var authors []string
		for _, author := range item.Authors {
			authors = append(authors, author.Name)
		}
		rows = append(rows, wire.Component{
			Type:     wire.ComponentRow,
			Key:      "paper-" + id,
			Title:    clipText(title, 1800),
			Subtitle: clipText(strings.Join(authors, ", ")+" · "+item.Category.Term, 1800),
			Action:   &wire.Action{ID: "detail", Values: map[string]string{"id": id}},
		})
	}
	section := wire.Section{Title: fmt.Sprintf(t.results, len(rows)), Items: rows}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noPapers})
	}
	if len(feed.Entries) == count && count < maxViewPapers {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
	}
	v.Sections = []wire.Section{section, {Title: t.recent, Items: []wire.Component{{Type: wire.ComponentButton, Title: t.recent, Action: &wire.Action{ID: "recent"}}}}}
	return v, nil
}

func detailView(r *ext.UIRequest, st pageState, item paper) *wire.View {
	t := textsFor(r.Locale)
	var authors []string
	for _, author := range item.Authors {
		authors = append(authors, author.Name)
	}
	items := []wire.Component{
		{Type: wire.ComponentText, Text: clipText(t.authors+": "+strings.Join(authors, ", "), 1800)},
		{Type: wire.ComponentText, Text: clipText(t.category+": "+item.Category.Term+" · "+t.published+": "+item.Published, 1800)},
	}
	if link := paperURL(item); link != "" {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: link})
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	v := &wire.View{Title: clipText(strings.Join(strings.Fields(item.Title), " "), 1800), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.papers, Items: items}}}
	summary := strings.Join(strings.Fields(item.Summary), " ")
	if summary != "" {
		v.Sections = append(v.Sections, wire.Section{Title: t.abstract, Items: textComponents(summary)})
	} else {
		v.Sections = append(v.Sections, wire.Section{Title: t.abstract, Items: []wire.Component{{Type: wire.ComponentText, Text: t.noAbstract, Style: wire.TextStyleSecondary}}})
	}
	return v
}

func textComponents(text string) []wire.Component {
	var out []wire.Component
	for len([]rune(text)) > 0 {
		r := []rune(text)
		n := min(len(r), 1800)
		out = append(out, wire.Component{Type: wire.ComponentText, Text: string(r[:n])})
		text = string(r[n:])
		if len(out) >= 30 {
			out[len(out)-1].Text = clipText(out[len(out)-1].Text, 1700) + " […]"
			break
		}
	}
	if len(out) == 0 {
		out = append(out, wire.Component{Type: wire.ComponentText, Text: " ", Style: wire.TextStyleSecondary})
	}
	return out
}
