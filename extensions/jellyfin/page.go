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
	Mode       string `json:"m,omitempty"`
	View       string `json:"v,omitempty"`
	Query      string `json:"q,omitempty"`
	StartIndex int    `json:"o,omitempty"`
	ItemID     string `json:"i,omitempty"`
	DraftID    string `json:"d,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.StartIndex < 0 || len([]rune(st.Query)) > maxQueryChars || len(st.ItemID) > 64 || len(st.DraftID) > 128 || len(st.Mode) > 30 || len(st.View) > 30 {
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
	case "view":
		if r.Values["view"] != "resume" && r.Values["view"] != "favorites" && r.Values["view"] != "search" {
			return nil, errors.New("invalid media view")
		}
		st.View, st.StartIndex = r.Values["view"], 0
	case "search":
		st.Query, st.StartIndex = strings.TrimSpace(clipText(r.Values["q"], maxQueryChars)), 0
		if st.Query == "" {
			st.View = "resume"
		} else {
			st.View = "search"
		}
	case "more":
		st.StartIndex, err = strconv.Atoi(r.Values["start_index"])
		if err != nil || st.StartIndex <= 0 {
			return nil, errors.New("invalid start index")
		}
	case "detail":
		if !validMediaID(r.Values["id"]) {
			return nil, errors.New("invalid media item ID")
		}
		st.Mode, st.ItemID = "detail", r.Values["id"]
	case "back":
		st.Mode, st.ItemID, st.DraftID = "", "", ""
	case "favorite":
		if st.Mode != "detail" || !validMediaID(st.ItemID) || st.DraftID == "" {
			return nil, errors.New("a favorite draft is required")
		}
		favorite, err := strconv.ParseBool(r.Values["favorite"])
		if err != nil {
			return nil, errors.New("invalid favorite value")
		}
		raw, _ := json.Marshal(favoriteArgs{ItemID: st.ItemID, Favorite: &favorite})
		out, err := a.doAction(ctx, "ui-favorite:"+st.DraftID, "jellyfin_set_favorite", raw)
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

const maxViewRecords = 250

// collectItems reads the batches from the start through the one at start so that Load more answers the whole list,
// longer. It stops at maxViewRecords to keep the view within the SDK's component limit, and reports whether another
// batch can be loaded and where it starts.
func (a *app) collectItems(ctx context.Context, userID, view, query string, start int) ([]mediaItem, int, bool, error) {
	var out []mediaItem
	seen := map[string]bool{}
	next, more := 0, false
	for pos := 0; len(out) < maxViewRecords; pos = next {
		page, err := a.client.items(ctx, userID, view, query, pos)
		if err != nil {
			return nil, 0, false, err
		}
		for _, item := range page.Items {
			if item.ID != "" {
				if seen[item.ID] {
					continue
				}
				seen[item.ID] = true
			}
			out = append(out, item)
		}
		next = pos + len(page.Items)
		more = len(page.Items) > 0 && next < page.TotalRecordCount
		if !more || pos >= start {
			break
		}
	}
	if len(out) > maxViewRecords {
		out = out[:maxViewRecords]
	}
	return out, next, more && len(out) < maxViewRecords, nil
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if a.client.base == "" || a.client.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	me, err := a.client.currentUser(ctx)
	if err != nil {
		return nil, err
	}
	if st.Mode == "detail" {
		item, err := a.client.item(ctx, me.ID, st.ItemID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		st.DraftID = randomID()
		return detailView(r, st, item), nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid media view")
	}
	if st.View == "" {
		st.View = "resume"
	}
	if st.View != "resume" && st.View != "favorites" && st.View != "search" {
		return nil, errors.New("invalid media view")
	}
	var items []mediaItem
	var next int
	var more bool
	if st.View != "search" || strings.TrimSpace(st.Query) != "" {
		items, next, more, err = a.collectItems(ctx, me.ID, st.View, st.Query, st.StartIndex)
		a.record(err)
		if err != nil {
			return nil, err
		}
	}
	v := &wire.View{Title: t.watching, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}, Filter: &wire.Filter{Name: "view", Title: t.watching, Value: st.View, Action: wire.Action{ID: "view"}, Choices: []wire.Choice{{Value: "resume", Title: t.resume}, {Value: "favorites", Title: t.favorites}, {Value: "search", Title: t.searchView}}}}
	section := wire.Section{Title: t.items}
	for _, item := range items {
		if !validMediaID(item.ID) || item.Name == "" {
			continue
		}
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "item-" + item.ID, Title: clipText(item.Name, 1800), Subtitle: clipText(fmt.Sprintf("%s · %.0f%% · %s", item.Type, item.UserData.PlayedPercentage, item.SeriesName), 1800), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID}}})
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noItems})
	}
	if more {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more", Values: map[string]string{"start_index": fmt.Sprint(next)}}}
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

func detailView(r *ext.UIRequest, st pageState, item mediaItem) *wire.View {
	t := textsFor(r.Locale)
	name := clipText(strings.TrimSpace(item.Name), 1800)
	if name == "" {
		name = clipText(strings.TrimSpace(item.ID), 1800)
	}
	if name == "" {
		name = t.item
	}
	items := []wire.Component{{Type: wire.ComponentText, Text: clipText(fmt.Sprintf("%s · %d · %.1f%%", item.Type, item.ProductionYear, item.UserData.PlayedPercentage), 1800)}}
	if item.SeriesName != "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(fmt.Sprintf("%s · %s · S%02dE%02d", item.SeriesName, item.SeasonName, item.ParentIndexNumber, item.IndexNumber), 1800)})
	}
	title := t.favorite
	favorite := true
	if item.UserData.IsFavorite {
		title, favorite = t.unfavorite, false
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: title, Action: &wire.Action{ID: "favorite", Values: map[string]string{"favorite": strconv.FormatBool(favorite)}, Confirm: clipText(title+"\n"+name, 1800)}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	v := &wire.View{Title: name, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.item, Items: items}}}
	if item.Overview != "" {
		v.Sections = append(v.Sections, wire.Section{Title: t.overview, Items: textComponents(clipText(item.Overview, 20000))})
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
