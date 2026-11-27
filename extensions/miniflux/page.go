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
	View    string `json:"v,omitempty"`
	Query   string `json:"q,omitempty"`
	Offset  int    `json:"o,omitempty"`
	EntryID int64  `json:"i,omitempty"`
	Mode    string `json:"m,omitempty"`
	DraftID string `json:"d,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Offset < 0 || st.EntryID < 0 || len(st.DraftID) > 128 || len([]rune(st.Query)) > maxQueryChars {
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
		st.Query, st.Offset = clipText(r.Values["q"], maxQueryChars), 0
		if strings.TrimSpace(st.Query) != "" {
			st.View = "search"
		} else {
			st.View = "unread"
		}
	case "view":
		if r.Values["view"] != "unread" && r.Values["view"] != "starred" {
			return nil, errors.New("invalid reading view")
		}
		st.View, st.Query, st.Offset = r.Values["view"], "", 0
	case "more":
		st.Offset += 50
	case "detail":
		id, err := strconv.ParseInt(r.Values["id"], 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("invalid entry ID")
		}
		st.EntryID, st.Mode = id, "detail"
	case "back":
		st.Mode, st.EntryID = "", 0
	case "read", "bookmark":
		if st.EntryID <= 0 || st.DraftID == "" {
			return nil, errors.New("entry and draft are required")
		}
		raw, _ := json.Marshal(entryArgs{EntryID: st.EntryID})
		name := "miniflux_mark_read"
		if r.Action == "bookmark" {
			name = "miniflux_toggle_bookmark"
		}
		out, err := a.doAction(ctx, "ui-"+r.Action+":"+st.DraftID, name, raw)
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

// maxViewEntries keeps the accumulated list within the SDK's component limit.
const maxViewEntries = 250

// collectEntries reads batches from offset 0 through offset so that Load more extends the list instead of
// replacing it, and reports whether the last batch was full and the cap still leaves room to show more.
func (a *app) collectEntries(ctx context.Context, view, query string, offset int) ([]entry, bool, error) {
	var out []entry
	seen := map[int64]bool{}
	more := false
	for pos := 0; pos <= offset && len(out) < maxViewEntries; pos += 50 {
		batch, err := a.client.entries(ctx, view, query, pos)
		if err != nil {
			return nil, false, err
		}
		for _, item := range batch.Entries {
			if item.ID > 0 {
				if seen[item.ID] {
					continue
				}
				seen[item.ID] = true
			}
			out = append(out, item)
		}
		more = len(batch.Entries) == 50
		if !more {
			break
		}
	}
	if len(out) > maxViewEntries {
		out = out[:maxViewEntries]
	}
	return out, more && len(out) < maxViewEntries, nil
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if a.client.base == "" || a.client.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "detail" {
		if st.EntryID <= 0 {
			return nil, errors.New("entry ID is required")
		}
		item, err := a.client.entry(ctx, st.EntryID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		text, err := articleText(item.Content)
		if err != nil {
			return nil, errors.New("article HTML could not be converted to text")
		}
		st.DraftID = randomID()
		return detailView(r, st, item, text), nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid reading view")
	}
	if st.View == "" {
		st.View = "unread"
	}
	if st.View != "unread" && st.View != "starred" && st.View != "search" {
		return nil, errors.New("invalid reading view")
	}
	entries, more, err := a.collectEntries(ctx, st.View, st.Query, st.Offset)
	a.record(err)
	if err != nil {
		return nil, err
	}
	v := &wire.View{Title: t.reading, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}, Filter: &wire.Filter{
		Name: "view", Title: t.reading, Value: st.View, Action: wire.Action{ID: "view"},
		Choices: []wire.Choice{{Value: "unread", Title: t.unread}, {Value: "starred", Title: t.starred}},
	}}
	if st.View == "search" {
		v.Filter.Value = "unread"
	}
	rows := make([]wire.Component, 0, len(entries))
	for _, item := range entries {
		if item.ID <= 0 || item.Title == "" {
			continue
		}
		rows = append(rows, wire.Component{Type: wire.ComponentRow, Key: "entry-" + strconv.FormatInt(item.ID, 10), Title: clipText(item.Title, 1800), Subtitle: clipText(item.Feed.Title+" · "+item.Author+" · "+item.PublishedAt, 1800), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": strconv.FormatInt(item.ID, 10)}}})
	}
	section := wire.Section{Title: fmt.Sprintf(t.results, len(rows)), Items: rows}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noEntries})
	}
	if more {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

func detailView(r *ext.UIRequest, st pageState, item entry, text string) *wire.View {
	t := textsFor(r.Locale)
	title := clipText(strings.TrimSpace(item.Title), 1800)
	if title == "" {
		title = fmt.Sprintf("#%d", item.ID)
	}
	items := []wire.Component{{Type: wire.ComponentText, Text: clipText(t.feed+": "+item.Feed.Title+" · "+t.author+": "+item.Author, 1800)}}
	if link := safeArticleLink(item.URL); link != "" {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: link})
	}
	if item.Status != "read" {
		items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.markRead, Action: &wire.Action{ID: "read", Confirm: clipText(t.markRead+"\n"+title, 1800)}})
	}
	bookmarkTitle := t.bookmark
	if item.Starred {
		bookmarkTitle = t.unbookmark
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: bookmarkTitle, Action: &wire.Action{ID: "bookmark", Confirm: clipText(bookmarkTitle+"\n"+title, 1800)}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	v := &wire.View{Title: title, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.reading, Items: items}}}
	if text != "" {
		v.Sections = append(v.Sections, wire.Section{Title: t.article, Items: textComponents(text)})
	} else {
		v.Sections = append(v.Sections, wire.Section{Title: t.article, Items: []wire.Component{{Type: wire.ComponentText, Text: t.noContent, Style: wire.TextStyleSecondary}}})
	}
	return v
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
