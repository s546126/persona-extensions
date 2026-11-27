package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Query        string `json:"q,omitempty"`
	CollectionID int64  `json:"c,omitempty"`
	LinkID       int64  `json:"l,omitempty"`
	Mode         string `json:"m,omitempty"`
	DraftID      string `json:"d,omitempty"`
	SaveURL      string `json:"u,omitempty"`
}

const maxQueryChars = 200

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.CollectionID < 0 || st.LinkID < 0 || len([]rune(st.Query)) > maxQueryChars || len(st.DraftID) > 128 || len(st.SaveURL) > 8192 {
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
		st.Query, st.Mode = clipText(strings.TrimSpace(r.Values["q"]), maxQueryChars), "list"
	case "collection":
		id, err := strconv.ParseInt(r.Values["collection"], 10, 64)
		if err != nil || id < 0 {
			return nil, errors.New("invalid collection ID")
		}
		st.CollectionID, st.Mode = id, "list"
	case "detail":
		id, err := strconv.ParseInt(r.Values["id"], 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("invalid link ID")
		}
		st.LinkID, st.Mode, st.DraftID = id, "detail", randomID()
	case "back":
		st.Mode, st.LinkID, st.DraftID = "list", 0, ""
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
		out, err := a.doAction(ctx, "ui-save:"+st.DraftID, "linkwarden_save", []byte(`{"url":`+quote(st.SaveURL)+`}`))
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	case "archive":
		if st.LinkID <= 0 {
			return nil, errors.New("invalid link ID")
		}
		if st.DraftID == "" {
			return nil, errors.New("the link view expired; reopen it")
		}
		raw := []byte(`{"id":` + strconv.FormatInt(st.LinkID, 10) + `}`)
		out, err := a.doAction(ctx, "ui-archive:"+st.DraftID, "linkwarden_archive", raw)
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
		if st.LinkID <= 0 {
			return nil, errors.New("invalid link ID")
		}
		item, err := a.client.link(ctx, st.LinkID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		highlights, err := a.client.highlights(ctx, st.LinkID)
		if err != nil {
			return nil, err
		}
		if st.DraftID == "" {
			st.DraftID = randomID()
		}
		return detailView(r, st, item, highlights), nil
	}
	if st.Mode != "" && st.Mode != "list" {
		return nil, errors.New("invalid links view")
	}
	items, err := a.client.search(ctx, st.Query, st.CollectionID)
	a.record(err)
	if err != nil {
		return nil, err
	}
	v := &wire.View{Title: t.links, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: clipText(st.Query, maxQueryChars), Action: wire.Action{ID: "search"}}}
	if collections, err := a.client.collections(ctx); err == nil {
		// A drop-down has 2 to 100 choices: the first collections, and the one being looked at in the last place
		// when it is beyond them.
		choices := []wire.Choice{{Value: "0", Title: t.all}}
		seenCol := map[int64]bool{0: true}
		selected := st.CollectionID == 0
		for _, c := range collections {
			if c.ID <= 0 || seenCol[c.ID] {
				continue
			}
			seenCol[c.ID] = true
			title := strings.TrimSpace(c.Name)
			if title == "" {
				title = "#" + strconv.FormatInt(c.ID, 10)
			}
			choice := wire.Choice{Value: strconv.FormatInt(c.ID, 10), Title: clipText(title, wire.StringMost)}
			switch {
			case len(choices) < maxFilterChoices:
				choices = append(choices, choice)
			case c.ID == st.CollectionID:
				choices[len(choices)-1] = choice
			}
			selected = selected || c.ID == st.CollectionID
		}
		if selected && len(choices) >= 2 {
			v.Filter = &wire.Filter{Name: "collection", Title: t.collections, Value: strconv.FormatInt(st.CollectionID, 10), Choices: choices, Action: wire.Action{ID: "collection"}}
		}
	}
	section := wire.Section{Items: []wire.Component{{
		Type: wire.ComponentButton, Title: t.save, Action: &wire.Action{ID: "save-form", Push: &wire.Push{State: ext.EncodeState(pageState{Mode: "save", DraftID: randomID()}), Title: t.save}},
	}}}
	seenLinks := map[int64]bool{}
	rendered := 0
	for _, item := range items {
		if item.ID <= 0 || seenLinks[item.ID] {
			continue
		}
		seenLinks[item.ID] = true
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "link-" + strconv.FormatInt(item.ID, 10), Title: clipText(linkTitle(item), 2000), Subtitle: clipText(item.URL, 2000), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": strconv.FormatInt(item.ID, 10)}}})
		rendered++
		if rendered >= maxViewLinks {
			break
		}
	}
	section.Title = fmt.Sprintf(t.results, rendered)
	if rendered == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noLinks})
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

const (
	// maxFilterChoices is the most choices a filter has.
	maxFilterChoices = 100
	// maxViewLinks and maxViewItems keep the list and detail views within the SDK's 300 component limit.
	maxViewLinks        = 250
	maxViewItems        = 250
	maxNoteItems        = 40
	maxDescriptionItems = 80
)

func detailView(r *ext.UIRequest, st pageState, item link, highlights []highlight) *wire.View {
	t := textsFor(r.Locale)
	items := []wire.Component{}
	if httpsLink(item.URL) {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: item.URL})
	}
	if item.Collection != nil && strings.TrimSpace(item.Collection.Name) != "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(strings.TrimSpace(item.Collection.Name), wire.StringMost), Style: wire.TextStyleSecondary})
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.archive, Action: &wire.Action{ID: "archive", Confirm: t.archive + "\n" + clipText(linkTitle(item), 500)}})
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	v := &wire.View{Title: clipText(linkTitle(item), 2000), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.read, Items: items}}}
	used := len(items)
	if item.Note != "" {
		note := capComponents(textComponents(item.Note), maxNoteItems)
		if len(note) > 0 {
			used += len(note)
			v.Sections = append(v.Sections, wire.Section{Title: t.note, Items: note})
		}
	}
	if item.Description != "" {
		desc := capComponents(textComponents(item.Description), maxDescriptionItems)
		if len(desc) > 0 {
			used += len(desc)
			v.Sections = append(v.Sections, wire.Section{Title: t.read, Items: desc})
		}
	}
	if len(highlights) > 0 {
		section := wire.Section{Title: t.highlights}
		for _, h := range highlights {
			section.Items = append(section.Items, textComponents(h.Text)...)
			section.Items = append(section.Items, textComponents(h.Note)...)
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
	return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Items: []wire.Component{
		{Type: wire.ComponentText, Text: clipText(st.SaveURL, 2000)},
		{Type: wire.ComponentButton, Title: t.confirm, Action: &wire.Action{ID: "save", Confirm: t.save + "\n" + st.SaveURL}},
	}}}}
}

// linkTitle is what a link is called: its name, else its description, else its address, as Linkwarden shows it. A link
// not named yet, or from a page without a title, has an empty name, and a row needs a title.
func linkTitle(item link) string {
	for _, s := range []string{item.Name, item.Description, item.URL} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return "#" + strconv.FormatInt(item.ID, 10)
}

// httpsLink reports whether a link component can carry raw: the view check refuses the whole view for any other.
func httpsLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
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
