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
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	View     string `json:"v,omitempty"`
	Query    string `json:"q,omitempty"`
	Offset   int    `json:"o,omitempty"`
	ID       string `json:"i,omitempty"`
	Mode     string `json:"m,omitempty"`
	DraftID  string `json:"d,omitempty"`
	Title    string `json:"t,omitempty"`
	URL      string `json:"u,omitempty"`
	Labels   string `json:"l,omitempty"`
	Progress int    `json:"p,omitempty"`
	Marked   bool   `json:"f,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Offset < 0 || st.Offset > 1000000 || len(st.ID) > 128 || len(st.DraftID) > 128 || len([]rune(st.Query)) > maxQueryChars || len([]rune(st.Title)) > maxTitleChars || len(st.URL) > 1100 || len([]rune(st.Labels)) > maxLabelsStateChars || st.Progress < 0 || st.Progress > 100 {
		return pageState{}, errors.New("invalid page state")
	}
	if st.View != "" && st.View != "unread" && st.View != "favorites" && st.View != "search" {
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
		if r.Values["view"] != "unread" && r.Values["view"] != "favorites" {
			return nil, errors.New("invalid reading view")
		}
		st.View, st.Query, st.Offset = r.Values["view"], "", 0
	case "more":
		st.Offset = intFromString(r.Values["offset"])
		if st.Offset <= 0 {
			return nil, errors.New("invalid next page")
		}
	case "detail":
		if !validBookmarkID(r.Values["id"]) {
			return nil, errors.New("invalid bookmark ID")
		}
		st.Mode, st.ID = "detail", r.Values["id"]
	case "back":
		st.Mode, st.ID, st.DraftID = "", "", ""
	case "save":
		if st.Mode == "review-save" {
			st.Mode = "save"
		} else {
			st = pageState{Mode: "save", DraftID: randomID()}
		}
	case "review-save":
		in := saveArgs{URL: strings.TrimSpace(r.Values["url"]), Title: strings.TrimSpace(r.Values["title"]), Labels: splitLabels(r.Values["labels"])}
		if err := in.validate(); err != nil {
			return nil, err
		}
		st.URL, st.Title, st.Labels, st.Mode = in.URL, in.Title, strings.Join(in.Labels, ", "), "review-save"
	case "confirm-save":
		in := saveArgs{URL: st.URL, Title: st.Title, Labels: splitLabels(st.Labels)}
		if err := in.validate(); err != nil || st.DraftID == "" {
			return nil, errors.New("a valid save draft is required")
		}
		raw, _ := json.Marshal(in)
		out, err := a.doAction(ctx, "ui-save:"+st.DraftID, "readeck_save_bookmark", raw)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	case "update":
		if st.Mode != "detail" || !validBookmarkID(st.ID) || st.DraftID == "" {
			return nil, errors.New("bookmark and update draft are required")
		}
		progress, err := strconv.Atoi(r.Values["progress"])
		if err != nil || progress < 0 || progress > 100 {
			return nil, errors.New("reading progress must be between 0 and 100")
		}
		marked, err := strconv.ParseBool(r.Values["marked"])
		if err != nil {
			return nil, errors.New("invalid favorite state")
		}
		st.Mode, st.Progress, st.Marked = "review-update", progress, marked
	case "confirm-update":
		if st.Mode != "review-update" || !validBookmarkID(st.ID) || st.DraftID == "" {
			return nil, errors.New("bookmark and update draft are required")
		}
		progress, marked := st.Progress, st.Marked
		raw, _ := json.Marshal(updateArgs{BookmarkID: st.ID, ReadProgress: &progress, IsMarked: &marked})
		out, err := a.doAction(ctx, "ui-update:"+st.DraftID, "readeck_update_reading", raw)
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

const (
	maxViewRecords = 250
	maxViewPages   = 10
)

func bookmarkKey(id string) string {
	key := "bookmark-" + id
	if len(key) <= 128 {
		return key
	}
	sum := sha256.Sum256([]byte(id))
	return "bookmark-" + hex.EncodeToString(sum[:])
}

func bookmarkTitle(item bookmark) string {
	for _, candidate := range []string{item.Title, item.URL, item.ID} {
		if s := strings.TrimSpace(candidate); s != "" {
			return s
		}
	}
	return "Bookmark"
}

func clipLabelsField(raw string, limit int) string {
	if len([]rune(raw)) <= limit {
		return raw
	}
	labels := splitLabels(raw)
	var kept []string
	total := 0
	for _, label := range labels {
		n := len([]rune(label))
		sep := 0
		if len(kept) > 0 {
			sep = 2
		}
		if total+sep+n > limit {
			break
		}
		kept = append(kept, label)
		total += sep + n
	}
	if len(kept) > 0 {
		return strings.Join(kept, ", ")
	}
	return clipText(raw, limit)
}

// collectBookmarks reads the batches from the start through the one at offset, following Readeck's next links, so that
// Load more answers the whole list, longer. It stops at maxViewRecords to keep the view within the SDK's component
// limit, and HasMore (with NextPage) reports whether another batch can be loaded.
func (a *app) collectBookmarks(ctx context.Context, view, query string, offset int) (bookmarkPage, error) {
	var out bookmarkPage
	seenIDs := map[string]bool{}
	seenOffsets := map[int]bool{}
	pages := 0
	for pos := 0; len(out.Items) < maxViewRecords && pages < maxViewPages; {
		if seenOffsets[pos] {
			out.HasMore = false
			break
		}
		seenOffsets[pos] = true
		pages++
		batch, err := a.client.bookmarks(ctx, view, query, pos)
		if err != nil {
			return bookmarkPage{}, err
		}
		for _, item := range batch.Items {
			if !validBookmarkID(item.ID) || item.Title == "" || item.IsDeleted || seenIDs[item.ID] {
				continue
			}
			seenIDs[item.ID] = true
			out.Items = append(out.Items, item)
			if len(out.Items) >= maxViewRecords {
				break
			}
		}
		out.Total, out.HasMore, out.NextPage = batch.Total, batch.HasMore, batch.NextPage
		if !batch.HasMore || pos >= offset {
			break
		}
		pos = batch.NextPage
	}
	if len(out.Items) >= maxViewRecords || pages >= maxViewPages || seenOffsets[out.NextPage] {
		out.HasMore = false
	}
	return out, nil
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if a.client.base == "" || a.client.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "save" || st.Mode == "review-save" {
		return saveView(r, st), nil
	}
	if st.Mode == "review-update" {
		return updateReviewView(r, st), nil
	}
	if st.Mode == "detail" {
		item, err := a.client.bookmark(ctx, st.ID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		var article string
		if item.HasArticle {
			article, err = a.client.article(ctx, item.ID)
			if err != nil && !errors.Is(err, errNotFound) {
				return nil, err
			}
		}
		highlights, err := a.client.highlights(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		st.DraftID = randomID()
		return detailView(r, st, item, article, highlights), nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid reading view")
	}
	if st.View == "" {
		st.View = "unread"
	}
	if st.View != "unread" && st.View != "favorites" && st.View != "search" {
		return nil, errors.New("invalid reading view")
	}
	var page bookmarkPage
	if st.View != "search" || strings.TrimSpace(st.Query) != "" {
		var err error
		page, err = a.collectBookmarks(ctx, st.View, st.Query, st.Offset)
		a.record(err)
		if err != nil {
			return nil, err
		}
	}
	filterValue := st.View
	if filterValue == "search" {
		filterValue = "unread"
	}
	v := &wire.View{Title: t.reading, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}, Filter: &wire.Filter{
		Name: "view", Title: t.reading, Value: filterValue, Action: wire.Action{ID: "view"},
		Choices: []wire.Choice{{Value: "unread", Title: t.unread}, {Value: "favorites", Title: t.favorites}},
	}}
	section := wire.Section{Title: t.results}
	for _, item := range page.Items {
		if !validBookmarkID(item.ID) || item.Title == "" || item.IsDeleted {
			continue
		}
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: bookmarkKey(item.ID), Title: clipText(item.Title, 1800), Subtitle: clipText(fmt.Sprintf("%s · %d%% · %s", item.SiteName, item.ReadProgress, strings.Join(item.Labels, ", ")), 1800), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID}}})
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noBookmarks})
	}
	if page.HasMore {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more", Values: map[string]string{"offset": fmt.Sprint(page.NextPage)}}}
	}
	v.Sections = []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.save, Action: &wire.Action{ID: "save", Push: &wire.Push{Title: t.save, State: ext.EncodeState(pageState{Mode: "save", DraftID: randomID()})}}}}}}
	return v, nil
}

func updateReviewView(r *ext.UIRequest, st pageState) *wire.View {
	t := textsFor(r.Locale)
	summary := fmt.Sprintf("%s: %d%%\n%s: %v", t.progress, st.Progress, t.marked, st.Marked)
	return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.update, Items: []wire.Component{
		{Type: wire.ComponentText, Text: clipText(summary, 1800)},
		{Type: wire.ComponentButton, Title: t.update, Action: &wire.Action{ID: "confirm-update", Confirm: t.update + "\n" + summary}},
		{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "detail", Values: map[string]string{"id": st.ID}}},
	}}}}
}

func saveView(r *ext.UIRequest, st pageState) *wire.View {
	t := textsFor(r.Locale)
	if st.Mode == "review-save" {
		return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.save, Items: []wire.Component{
			{Type: wire.ComponentText, Text: clipText(st.URL, 1800)},
			{Type: wire.ComponentText, Text: clipText(st.Title+"\n"+st.Labels, 1800)},
			{Type: wire.ComponentButton, Title: t.save, Action: &wire.Action{ID: "confirm-save", Confirm: t.save + "\n" + st.URL}},
			{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "save"}},
		}}}}
	}
	return &wire.View{Title: t.save, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.save, Items: []wire.Component{
		{Type: wire.ComponentField, Kind: "text", Name: "url", Label: t.url, Value: st.URL},
		{Type: wire.ComponentField, Kind: "text", Name: "title", Label: t.title, Value: st.Title},
		{Type: wire.ComponentField, Kind: "text", Name: "labels", Label: t.labels, Value: clipLabelsField(st.Labels, wire.StringMost)},
		{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "review-save", Submit: true}},
	}}}}
}

func detailView(r *ext.UIRequest, st pageState, item bookmark, article string, highlights []highlight) *wire.View {
	t := textsFor(r.Locale)
	items := []wire.Component{{Type: wire.ComponentText, Text: clipText(fmt.Sprintf("%s · %d %s · %s", item.SiteName, item.ReadingTime, t.minutes, strings.Join(item.Labels, ", ")), 1800)}}
	if link := safeArticleLink(item.URL); httpsLink(link) {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: link})
	}
	items = append(items,
		wire.Component{Type: wire.ComponentField, Kind: "text", Name: "progress", Label: t.progress, Value: fmt.Sprint(item.ReadProgress)},
		wire.Component{Type: wire.ComponentField, Kind: "text", Name: "marked", Label: t.marked, Value: strconv.FormatBool(item.IsMarked)},
		wire.Component{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "update", Submit: true}},
		wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	)
	v := &wire.View{Title: clipText(bookmarkTitle(item), 1800), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.reading, Items: items}}}
	used := len(items)
	if article != "" {
		body := capComponents(textComponents(clipText(article, 30000)), maxArticleItems)
		used += len(body)
		v.Sections = append(v.Sections, wire.Section{Title: t.article, Items: body})
	} else {
		used++
		v.Sections = append(v.Sections, wire.Section{Title: t.article, Items: []wire.Component{{Type: wire.ComponentText, Text: t.noContent, Style: wire.TextStyleSecondary}}})
	}
	if item.Note != "" {
		note := capComponents(textComponents(clipText(item.Note, 6000)), maxNoteItems)
		used += len(note)
		v.Sections = append(v.Sections, wire.Section{Title: t.note, Items: note})
	}
	section := wire.Section{Title: t.highlights}
	for _, h := range highlights {
		if h.Text != "" {
			section.Items = append(section.Items, textComponents(clipText(h.Text, 6000))...)
		}
	}
	section.Items = capComponents(section.Items, maxViewItems-used)
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: t.noHighlights, Style: wire.TextStyleSecondary})
	}
	v.Sections = append(v.Sections, section)
	return v
}

const (
	// maxViewItems keeps the bookmark detail view within the SDK's 300 component limit across article, note, and highlights.
	maxViewItems    = 250
	maxArticleItems = 100
	maxNoteItems    = 60
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

func safeArticleLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	return u.String()
}

// httpsLink reports whether a link component can carry raw: the view check refuses the whole view for any other.
func httpsLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

func splitLabels(raw string) []string {
	var out []string
	for _, label := range strings.Split(raw, ",") {
		if label = strings.TrimSpace(label); label != "" {
			out = append(out, label)
		}
	}
	return out
}

func intFromString(raw string) int {
	value, _ := strconv.Atoi(raw)
	return value
}

func textComponents(text string) []wire.Component {
	var out []wire.Component
	for _, line := range strings.Split(text, "\n") {
		r := []rune(line)
		for len(r) > 0 {
			n := min(len(r), 1800)
			out = append(out, wire.Component{Type: wire.ComponentText, Text: string(r[:n])})
			r = r[n:]
			if len(out) >= 100 {
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
