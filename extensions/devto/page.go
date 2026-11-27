package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Query     string `json:"q,omitempty"`
	Page      int    `json:"p,omitempty"`
	ArticleID int64  `json:"a,omitempty"`
	Mode      string `json:"m,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Page < 0 || st.Page > 5 || len([]rune(st.Query)) > 200 || st.ArticleID < 0 {
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
		st.Query, st.Page, st.Mode = r.Values["q"], 0, "list"
	case "more":
		st.Page = min(st.Page+1, 5)
	case "detail":
		id, err := strconv.ParseInt(r.Values["id"], 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("invalid article ID")
		}
		st.ArticleID, st.Mode = id, "detail"
	case "back":
		st.Mode, st.ArticleID = "list", 0
	default:
		return nil, fmt.Errorf("unknown page action %q", r.Action)
	}
	v, err := a.page(ctx, r, st)
	return &wire.ActionResult{View: v}, err
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if st.Mode == "detail" {
		item, err := a.client.article(ctx, st.ArticleID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		comments, err := a.client.comments(ctx, st.ArticleID)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item, comments), nil
	}
	var items []article
	seen := map[int64]bool{}
	full := false // whether the last batch read was full, so another page may exist (before any filter hides articles)
	for n := 0; n <= st.Page; n++ {
		batch, err := a.client.articles(ctx, "", n+1)
		if err != nil {
			a.record(err)
			return nil, err
		}
		for _, item := range batch {
			if seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			items = append(items, item)
		}
		if full = len(batch) == 30; !full {
			break
		}
	}
	a.record(nil)
	items = filterArticles(items, a.config(), st.Query, 0)
	v := &wire.View{Title: t.articles, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	section := wire.Section{Title: fmt.Sprintf(t.results, len(items))}
	for _, item := range items {
		section.Items = append(section.Items, wire.Component{
			Type:     wire.ComponentRow,
			Key:      "article-" + strconv.FormatInt(item.ID, 10),
			Title:    articleTitle(item),
			Subtitle: clipText(fmt.Sprintf("%d min · %d comments · %s", item.ReadingMinutes, item.CommentsCount, strings.Join(item.tagNames(), ", ")), 2000),
			Action:   &wire.Action{ID: "detail", Values: map[string]string{"id": strconv.FormatInt(item.ID, 10)}},
		})
	}
	if len(items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noArticles})
	}
	if full && st.Page < 5 {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

func articleTitle(item article) string {
	if title := clipText(strings.TrimSpace(item.Title), 2000); title != "" {
		return title
	}
	return "#" + strconv.FormatInt(item.ID, 10)
}

// httpsLink reports whether a link component can carry raw: the view check refuses the whole view for any other.
func httpsLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

func detailView(r *ext.UIRequest, st pageState, item article, comments []comment) *wire.View {
	t := textsFor(r.Locale)
	items := []wire.Component{
		{Type: wire.ComponentText, Text: clipText(item.User.Name+" · "+fmt.Sprintf("%d min read", item.ReadingMinutes), 2000), Style: wire.TextStyleSecondary},
	}
	if httpsLink(item.URL) {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: item.URL})
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	v := &wire.View{Title: articleTitle(item), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.read, Items: items}}}
	used := len(items)
	if item.Description != "" {
		summary := capComponents(textComponents(item.Description), maxSummaryItems)
		if len(summary) > 0 {
			used += len(summary)
			v.Sections = append(v.Sections, wire.Section{Title: t.summary, Items: summary})
		}
	}
	if item.BodyMarkdown != "" {
		body := capComponents(textComponents(item.BodyMarkdown), maxArticleItems)
		if len(body) > 0 {
			used += len(body)
			v.Sections = append(v.Sections, wire.Section{Title: t.read, Items: body})
		}
	}
	if len(comments) > 0 && used < maxViewItems {
		section := wire.Section{Title: t.comments}
		limit := maxViewItems - used
		addCommentViews(&section, comments, 0, limit)
		section.Items = capComponents(section.Items, limit)
		if len(section.Items) > 0 {
			v.Sections = append(v.Sections, section)
		}
	}
	return v
}

const (
	// maxViewItems keeps the article view within the SDK's 300 component limit, whatever the discussion holds.
	maxViewItems    = 250
	maxSummaryItems = 40
	maxArticleItems = 150
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

func addCommentViews(section *wire.Section, comments []comment, depth, limit int) {
	if depth > 6 || len(section.Items) > limit {
		return
	}
	for _, c := range comments {
		if len(section.Items) > limit {
			return
		}
		text, err := plainHTML(c.BodyHTML)
		if err != nil || text == "" {
			continue
		}
		if author := strings.TrimSpace(c.User.Name); author != "" {
			section.Items = append(section.Items, wire.Component{
				Type:  wire.ComponentText,
				Text:  clipText(strings.Repeat("  ", depth)+author, 2000),
				Style: wire.TextStyleSecondary,
			})
		}
		section.Items = append(section.Items, textComponents(text)...)
		addCommentViews(section, c.Children, depth+1, limit)
	}
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
