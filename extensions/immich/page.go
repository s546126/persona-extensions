package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Mode     string   `json:"m,omitempty"`
	View     string   `json:"v,omitempty"`
	Query    string   `json:"q,omitempty"`
	Cursor   string   `json:"c,omitempty"`
	Pages    []string `json:"p,omitempty"`
	AssetID  string   `json:"i,omitempty"`
	DraftID  string   `json:"d,omitempty"`
	Favorite bool     `json:"f,omitempty"`
}

func cursorToken(cursor string) string {
	sum := sha256.Sum256([]byte(cursor))
	return "sha256:" + hex.EncodeToString(sum[:16])
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || len([]rune(st.Query)) > maxQueryChars || len(st.Cursor) > 4096 || len(st.Pages) >= maxViewPages || len(st.AssetID) > 128 || len(st.DraftID) > 128 || len(st.View) > 30 || len(st.Mode) > 30 {
		return pageState{}, errors.New("invalid page state")
	}
	seen := map[string]bool{}
	for _, c := range st.Pages {
		if c == "" || len(c) > 4096 || seen[c] {
			return pageState{}, errors.New("invalid page state")
		}
		seen[c] = true
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
		if r.Values["view"] != "favorites" && r.Values["view"] != "search" && r.Values["view"] != "smart" {
			return nil, errors.New("invalid library view")
		}
		st.View, st.Cursor, st.Pages = r.Values["view"], "", nil
	case "search":
		st.Query, st.Cursor, st.Pages = strings.TrimSpace(clipText(r.Values["q"], maxQueryChars)), "", nil
		if st.Query == "" {
			st.View = "favorites"
		} else if st.View != "smart" {
			st.View = "search"
		}
	case "more":
		got := r.Values["cursor"]
		if st.Cursor == "" || (got != st.Cursor && got != cursorToken(st.Cursor)) || slices.Contains(st.Pages, st.Cursor) {
			return nil, errors.New("invalid next cursor")
		}
		st.Pages = append(st.Pages, st.Cursor)
		st.Cursor = ""
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("page state limit reached")
		}
	case "detail":
		if !validAssetID(r.Values["id"]) {
			return nil, errors.New("invalid asset ID")
		}
		st.Mode, st.AssetID = "detail", r.Values["id"]
	case "back":
		st.Mode, st.AssetID, st.DraftID = "", "", ""
	case "favorite":
		if st.Mode != "detail" || !validAssetID(st.AssetID) || st.DraftID == "" {
			return nil, errors.New("an asset favorite draft is required")
		}
		favorite, err := strconv.ParseBool(r.Values["favorite"])
		if err != nil {
			return nil, errors.New("invalid favorite value")
		}
		raw, _ := json.Marshal(favoriteArgs{AssetID: st.AssetID, Favorite: &favorite})
		out, err := a.doAction(ctx, "ui-favorite:"+st.DraftID, "immich_set_favorite", raw)
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

// collectAssets reads the first page and then the pages at the cursors in Pages, so that Load more answers the whole
// list, longer. It stops at maxViewRecords to keep the view within the SDK's component limit, and the response it
// returns carries a NextCursor only while another page can be loaded.
func (a *app) collectAssets(ctx context.Context, st pageState) (searchResponse, error) {
	var out searchResponse
	seen := map[string]bool{}
	for _, cursor := range append([]string{""}, st.Pages...) {
		if len(out.Assets.Items) >= maxViewRecords {
			break
		}
		batch, err := a.client.search(ctx, st.Query, st.View == "favorites", cursor)
		if err != nil {
			return searchResponse{}, err
		}
		for _, item := range batch.Assets.Items {
			if item.ID != "" {
				if seen[item.ID] {
					continue
				}
				seen[item.ID] = true
			}
			out.Assets.Items = append(out.Assets.Items, item)
		}
		out.Assets.NextCursor = batch.Assets.NextCursor
	}
	if len(out.Assets.Items) > maxViewRecords {
		out.Assets.Items = out.Assets.Items[:maxViewRecords]
	}
	if len(out.Assets.Items) >= maxViewRecords || len(st.Pages)+1 >= maxViewPages {
		out.Assets.NextCursor = nil
	}
	if out.Assets.NextCursor != nil {
		next := *out.Assets.NextCursor
		if next == "" || len(next) > 4096 || slices.Contains(st.Pages, next) {
			out.Assets.NextCursor = nil
		}
	}
	return out, nil
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if a.client.base == "" || a.client.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "detail" {
		item, err := a.client.asset(ctx, st.AssetID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		st.DraftID = randomID()
		return detailView(r, st, item), nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid library view")
	}
	if st.View == "" {
		st.View = "favorites"
	}
	if st.View != "favorites" && st.View != "search" && st.View != "smart" {
		return nil, errors.New("invalid library view")
	}
	var page searchResponse
	var err error
	if st.View == "favorites" || strings.TrimSpace(st.Query) != "" {
		if st.View == "smart" {
			page, err = a.client.smartSearch(ctx, st.Query)
		} else {
			page, err = a.collectAssets(ctx, st)
		}
		a.record(err)
		if err != nil {
			return nil, err
		}
	}
	st.Cursor = ""
	v := &wire.View{Title: t.library, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}, Filter: &wire.Filter{Name: "view", Title: t.library, Value: st.View, Action: wire.Action{ID: "view"}, Choices: []wire.Choice{{Value: "favorites", Title: t.favorites}, {Value: "search", Title: t.filename}, {Value: "smart", Title: t.smart}}}}
	section := wire.Section{Title: t.results}
	seen := map[string]bool{}
	for _, item := range page.Assets.Items {
		if !validAssetID(item.ID) || item.OriginalFileName == "" || item.IsTrashed || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "asset-" + item.ID, Title: clipText(item.OriginalFileName, 1800), Subtitle: clipText(item.Type+" · "+item.FileCreatedAt, 1800), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID}}})
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noAssets})
	}
	if st.View != "smart" && page.Assets.NextCursor != nil && *page.Assets.NextCursor != "" {
		next := *page.Assets.NextCursor
		withCursor := st
		withCursor.Cursor = next
		afterMore := st
		afterMore.Pages = append(slices.Clone(st.Pages), next)
		if encoded := ext.EncodeState(withCursor); len(encoded) <= wire.StateMost && len(ext.EncodeState(afterMore)) <= wire.StateMost {
			v.State = encoded
			val := next
			if len([]rune(val)) > 2000 {
				val = cursorToken(next)
			}
			section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more", Values: map[string]string{"cursor": val}}}
		}
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func detailView(r *ext.UIRequest, st pageState, item asset) *wire.View {
	t := textsFor(r.Locale)
	name := clipText(strings.TrimSpace(item.OriginalFileName), 1800)
	if name == "" {
		name = clipText(strings.TrimSpace(item.ID), 1800)
	}
	if name == "" {
		name = t.asset
	}
	items := []wire.Component{{Type: wire.ComponentText, Text: clipText(item.Type+" · "+item.FileCreatedAt, 1800)}}
	if loc := joinNonEmpty(", ", item.Exif.City, item.Exif.Country); loc != "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(loc, 1800)})
	}
	if cam := joinNonEmpty(" ", item.Exif.Make, item.Exif.Model); cam != "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(cam, 1800)})
	}
	title := t.favorite
	favorite := true
	if item.IsFavorite {
		title, favorite = t.unfavorite, false
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: title, Action: &wire.Action{ID: "favorite", Values: map[string]string{"favorite": strconv.FormatBool(favorite)}, Confirm: clipText(title+"\n"+name, 1800)}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	v := &wire.View{Title: name, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.asset, Items: items}}}
	if item.Exif.Description != "" {
		v.Sections = append(v.Sections, wire.Section{Title: t.description, Items: textComponents(clipText(item.Exif.Description, 12000))})
	}
	var people []string
	for _, person := range item.People {
		if person.Name != "" {
			people = append(people, person.Name)
		}
	}
	if len(people) > 0 {
		v.Sections = append(v.Sections, wire.Section{Title: t.people, Items: []wire.Component{{Type: wire.ComponentText, Text: clipText(strings.Join(people, ", "), 1800)}}})
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
