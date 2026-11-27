package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const maxQueryChars = 200

type pageState struct {
	Filter  string `json:"f,omitempty"`
	Query   string `json:"q,omitempty"`
	StoryID string `json:"s,omitempty"`
	Mode    string `json:"m,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || len(st.StoryID) > 32 || utf8.RuneCountInString(st.Query) > maxQueryChars {
		return pageState{}, errors.New("invalid page state")
	}
	if st.Filter != "" && st.Filter != "hot" && st.Filter != "new" {
		return pageState{}, errors.New("invalid page state")
	}
	switch st.Mode {
	case "", "list":
	case "detail":
		if err := validID(st.StoryID); err != nil {
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
		st.Query, st.Mode = clipText(r.Values["q"], maxQueryChars), "list"
	case "filter":
		if r.Values["filter"] != "hot" && r.Values["filter"] != "new" {
			return nil, errors.New("invalid feed")
		}
		st.Filter, st.Mode = r.Values["filter"], "list"
	case "detail":
		if err := validID(r.Values["id"]); err != nil {
			return nil, err
		}
		st.StoryID, st.Mode = r.Values["id"], "detail"
	case "back":
		st.Mode, st.StoryID = "list", ""
	default:
		return nil, fmt.Errorf("unknown page action %q", r.Action)
	}
	v, err := a.page(ctx, r, st)
	return &wire.ActionResult{View: v}, err
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if st.Mode == "detail" {
		item, err := a.client.story(ctx, st.StoryID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item), nil
	}
	if st.Filter == "" {
		st.Filter = "hot"
	}
	items, err := a.client.stories(ctx, st.Filter == "new")
	a.record(err)
	if err != nil {
		return nil, err
	}
	items = filterStories(items, a.currentConfig(), nil, st.Query, 0)
	v := &wire.View{Title: t.hot, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	v.Filter = &wire.Filter{Name: "filter", Title: t.read, Value: st.Filter, Action: wire.Action{ID: "filter"}, Choices: []wire.Choice{{Value: "hot", Title: t.hot}, {Value: "new", Title: t.newest}}}
	seen := make(map[string]struct{}, min(len(items), maxViewItems))
	var rows []wire.Component
	for _, item := range items {
		if validID(item.ShortID) != nil {
			continue
		}
		if _, dup := seen[item.ShortID]; dup {
			continue
		}
		seen[item.ShortID] = struct{}{}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = item.ShortID
		}
		rows = append(rows, wire.Component{
			Type:     wire.ComponentRow,
			Key:      "story-" + item.ShortID,
			Title:    clipText(title, 2000),
			Subtitle: clipText(fmt.Sprintf("%d · %d comments · %s", item.Score, item.CommentCount, strings.Join(item.Tags, ", ")), 2000),
			Action:   &wire.Action{ID: "detail", Values: map[string]string{"id": item.ShortID}},
		})
		if len(rows) >= maxViewItems {
			break
		}
	}
	section := wire.Section{Title: fmt.Sprintf(t.results, len(rows)), Items: rows}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noStories})
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

// httpsLink reports whether a link component can carry raw: the view check refuses the whole view for any other.
func httpsLink(raw string) bool {
	if raw == "" || utf8.RuneCountInString(raw) > wire.StringMost || strings.ContainsAny(raw, "\x00\r\n") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

func detailView(r *ext.UIRequest, st pageState, item story) *wire.View {
	t := textsFor(r.Locale)
	items := []wire.Component{
		{Type: wire.ComponentText, Text: clipText(fmt.Sprintf("%d points · %d comments · by %s", item.Score, item.CommentCount, item.Submitter), 2000)},
	}
	if tags := strings.TrimSpace(strings.Join(item.Tags, ", ")); tags != "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(tags, 2000), Style: wire.TextStyleSecondary})
	}
	if httpsLink(item.URL) {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: item.URL})
	}
	if httpsLink(item.CommentsURL) {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.discussion, URL: item.CommentsURL})
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	v := &wire.View{Title: clipText(strings.TrimSpace(item.Title), 2000), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.read, Items: items}}}
	used := len(items)
	if item.DescriptionPlain != "" {
		if description := capComponents(textComponents(item.DescriptionPlain), maxDescriptionItems); len(description) > 0 {
			used += len(description)
			v.Sections = append(v.Sections, wire.Section{Title: t.description, Items: description})
		}
	}
	if len(item.Comments) > 0 && maxViewItems-used > 0 {
		section := wire.Section{Title: t.comments}
		for _, c := range item.Comments {
			if c.IsDeleted || c.IsModerated {
				continue
			}
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(strings.Repeat("  ", min(c.Depth, 8))+c.User+" · "+fmt.Sprint(c.Score), 2000), Style: wire.TextStyleSecondary})
			section.Items = append(section.Items, textComponents(c.Text)...)
		}
		if len(section.Items) > 0 {
			section.Items = capComponents(section.Items, maxViewItems-used)
			if len(section.Items) > 0 {
				v.Sections = append(v.Sections, section)
			}
		}
	}
	return v
}

const (
	// maxViewItems keeps the story view within the SDK's 300 component limit, whatever the discussion holds.
	maxViewItems = 250
	// maxDescriptionItems leaves the comments room when the description is long.
	maxDescriptionItems = 100
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
