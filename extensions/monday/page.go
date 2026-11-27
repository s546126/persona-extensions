package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const maxBoardNameChars = 500

type pageState struct {
	Mode      string `json:"m,omitempty"`
	BoardID   int64  `json:"b,omitempty"`
	BoardName string `json:"n,omitempty"`
	Cursor    string `json:"c,omitempty"`
	Next      string `json:"x,omitempty"`
	DraftID   string `json:"d,omitempty"`
	Name      string `json:"a,omitempty"`
	ItemID    int64  `json:"i,omitempty"`
	ColumnID  string `json:"l,omitempty"`
	Label     string `json:"s,omitempty"`
}

func validMode(mode string) bool {
	switch mode {
	case "", "items", "detail", "create", "review-create", "edit", "review-edit":
		return true
	default:
		return false
	}
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || !validMode(st.Mode) || st.BoardID < 0 || st.ItemID < 0 || utf8.RuneCountInString(st.BoardName) > maxBoardNameChars || len(st.DraftID) > 128 || len([]rune(st.Name)) > maxNameChars || len(st.Cursor) > 2048 || len(st.Next) > 2048 || len(st.ColumnID) > 100 || len([]rune(st.Label)) > maxLabelChars {
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
	case "board":
		id, err := strconv.ParseInt(r.Values["id"], 10, 64)
		if err != nil || id <= 0 || r.Values["name"] == "" {
			return nil, errors.New("invalid board")
		}
		st = pageState{Mode: "items", BoardID: id, BoardName: clipText(r.Values["name"], maxBoardNameChars)}
	case "boards":
		st = pageState{}
	case "more":
		if st.Next == "" || st.Next == st.Cursor || len(st.Next) > 2048 || strings.ContainsAny(st.Next, "\x00\r\n\"'") {
			return nil, errors.New("invalid next cursor")
		}
		st.Cursor = st.Next
	case "detail":
		id, err := strconv.ParseInt(r.Values["id"], 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("invalid item ID")
		}
		st.Mode, st.ItemID = "detail", id
	case "create":
		if st.Mode != "review-create" {
			st = pageState{Mode: "create", BoardID: st.BoardID, BoardName: st.BoardName, DraftID: randomID()}
		} else {
			st.Mode = "create"
		}
	case "edit":
		st.Mode = "edit"
		if st.DraftID == "" {
			st.DraftID = randomID()
		}
	case "back":
		st.Mode, st.ItemID, st.DraftID, st.Name, st.ColumnID, st.Label = "items", 0, "", "", "", ""
	case "review-create":
		in := createArgs{BoardID: st.BoardID, Name: r.Values["name"]}
		if err := in.validate(); err != nil {
			return nil, err
		}
		st.Name, st.Mode = in.Name, "review-create"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("item draft exceeds page state limit")
		}
	case "review-edit":
		columnID := r.Values["column_id"]
		if columnID == "" {
			columnID = st.ColumnID
		}
		if columnID == "" && st.BoardID > 0 {
			board, err := a.client.board(ctx, st.BoardID)
			if err != nil {
				return nil, err
			}
			columnID = statusColumnID(board, "")
		}
		in := statusArgs{BoardID: st.BoardID, ItemID: st.ItemID, ColumnID: columnID, Label: r.Values["label"]}
		if err := in.validate(); err != nil {
			return nil, err
		}
		st.ColumnID, st.Label, st.Mode = in.ColumnID, in.Label, "review-edit"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("status draft exceeds page state limit")
		}
	case "confirm-create":
		if strings.TrimSpace(st.DraftID) == "" {
			return nil, errors.New("missing draft ID")
		}
		raw, _ := json.Marshal(createArgs{BoardID: st.BoardID, Name: st.Name})
		out, err := a.doAction(ctx, "ui-create:"+st.DraftID, "monday_create_item", raw)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	case "confirm-edit":
		if strings.TrimSpace(st.DraftID) == "" {
			return nil, errors.New("missing draft ID")
		}
		raw, _ := json.Marshal(statusArgs{BoardID: st.BoardID, ItemID: st.ItemID, ColumnID: st.ColumnID, Label: st.Label})
		out, err := a.doAction(ctx, "ui-edit:"+st.DraftID, "monday_update_status", raw)
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
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "create" || st.Mode == "review-create" || st.Mode == "edit" || st.Mode == "review-edit" {
		return a.editorView(ctx, r, st)
	}
	if st.Mode == "detail" {
		board, err := a.client.board(ctx, st.BoardID)
		if err != nil {
			return nil, err
		}
		item, err := a.client.item(ctx, st.BoardID, st.ItemID)
		if err != nil {
			return nil, err
		}
		st.DraftID = randomID()
		return detailView(r, st, board, item), nil
	}
	if st.Mode == "items" {
		page, err := a.client.items(ctx, st.BoardID, st.Cursor)
		a.record(err)
		if err != nil {
			return nil, err
		}
		st.Next = ""
		if page.Cursor != "" && page.Cursor != st.Cursor && len(page.Cursor) <= 2048 {
			st.Next = page.Cursor
		}
		section := wire.Section{Title: t.items}
		seen := map[int64]bool{}
		for _, item := range page.Items {
			if item.ID <= 0 || item.Name == "" || seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "item-" + fmt.Sprint(item.ID), Title: clipText(item.Name, 1800), Subtitle: clipText(item.UpdatedAt, 100), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": fmt.Sprint(item.ID)}}})
			if len(section.Items) >= maxViewItems {
				break
			}
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noItems})
		}
		if st.Next != "" {
			section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
		}
		v := &wire.View{Title: clipText(st.BoardName, 1800), State: ext.EncodeState(st), Sections: []wire.Section{section, {Title: t.items, Items: []wire.Component{
			{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "create", Push: &wire.Push{Title: t.create, State: ext.EncodeState(pageState{Mode: "create", BoardID: st.BoardID, BoardName: st.BoardName, DraftID: randomID()})}}},
			{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "boards"}},
		}}}}
		return v, nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid item view")
	}
	boards, err := a.client.boards(ctx)
	a.record(err)
	if err != nil {
		return nil, err
	}
	section := wire.Section{Title: t.boards}
	seenBoards := map[int64]bool{}
	for _, item := range boards {
		if item.ID <= 0 || item.Name == "" || seenBoards[item.ID] {
			continue
		}
		seenBoards[item.ID] = true
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "board-" + fmt.Sprint(item.ID), Title: clipText(item.Name, 1800), Subtitle: fmt.Sprint(item.ID), Action: &wire.Action{ID: "board", Values: map[string]string{"id": fmt.Sprint(item.ID), "name": clipText(item.Name, maxBoardNameChars)}}})
		if len(section.Items) >= maxViewItems {
			break
		}
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noBoards})
	}
	return &wire.View{Title: t.boards, State: ext.EncodeState(st), Sections: []wire.Section{section}}, nil
}

func (a *app) editorView(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if st.Mode == "review-create" || st.Mode == "review-edit" {
		action, back, summary := "confirm-create", "create", st.Name
		if st.Mode == "review-create" {
			if err := (createArgs{BoardID: st.BoardID, Name: st.Name}).validate(); err != nil {
				return nil, err
			}
		} else {
			action, back, summary = "confirm-edit", "edit", st.Label
			if err := (statusArgs{BoardID: st.BoardID, ItemID: st.ItemID, ColumnID: st.ColumnID, Label: st.Label}).validate(); err != nil {
				return nil, err
			}
		}
		items := []wire.Component{{Type: wire.ComponentText, Text: clipText(summary, 1800)}}
		items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: action, Confirm: clipText(t.review+"\n"+clipText(summary, 500), wire.StringMost)}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: back}})
		return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.review, Items: items}}}, nil
	}
	var items []wire.Component
	action := "review-create"
	if st.Mode == "edit" {
		action = "review-edit"
		board, err := a.client.board(ctx, st.BoardID)
		if err != nil {
			return nil, err
		}
		if colID := statusColumnID(board, st.ColumnID); colID != "" {
			st.ColumnID = colID
		}
		choices := statusChoices(board, st.ColumnID)
		if len(choices) > 0 {
			selected := choices[0].Value
			if st.Label != "" && slices.ContainsFunc(choices, func(c wire.Choice) bool { return c.Value == st.Label }) {
				selected = st.Label
			}
			items = append(items, wire.Component{Type: wire.ComponentField, Kind: "choice", Name: "label", Label: t.label, Value: selected, Choices: choices})
		} else {
			items = append(items, wire.Component{Type: wire.ComponentField, Kind: "text", Name: "label", Label: t.label, Value: clipText(st.Label, wire.StringMost)})
		}
	} else {
		items = []wire.Component{{Type: wire.ComponentField, Kind: "text", Name: "name", Label: t.name, Value: clipText(st.Name, wire.StringMost)}}
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: action, Submit: true}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Items: items}}}, nil
}

const maxViewItems = 250

func detailView(r *ext.UIRequest, st pageState, board board, item item) *wire.View {
	t := textsFor(r.Locale)
	var items []wire.Component
	if item.UpdatedAt != "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(item.UpdatedAt, 100)})
	}
	if link := safeBoardLink(item.URL); link != "" {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: link})
	}
	budget := maxViewItems - len(items) - 2
	var cols []wire.Component
	for _, column := range item.ColumnValues {
		if column.Text != "" {
			cols = append(cols, wire.Component{Type: wire.ComponentText, Text: clipText(column.ID+": "+column.Text, 1800)})
		}
	}
	if len(cols) > budget {
		cols = cols[:budget]
		cols[budget-1].Text = clipText(cols[budget-1].Text, 1700) + " […]"
	}
	items = append(items, cols...)
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.update, Action: &wire.Action{ID: "edit", Push: &wire.Push{Title: t.update, State: ext.EncodeState(pageState{Mode: "edit", BoardID: st.BoardID, ItemID: st.ItemID, ColumnID: statusColumnID(board, st.ColumnID), DraftID: randomID()})}}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	title := clipText(item.Name, 1800)
	if title == "" {
		title = fmt.Sprintf("#%d", item.ID)
	}
	return &wire.View{Title: title, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.items, Items: items}}}
}

func statusColumnID(board board, selected string) string {
	for _, column := range board.Columns {
		if column.Type != "status" || (selected != "" && column.ID != selected) {
			continue
		}
		if column.ID != "" {
			return column.ID
		}
	}
	return ""
}

func statusChoices(board board, selected string) []wire.Choice {
	for _, column := range board.Columns {
		if column.Type != "status" || (selected != "" && column.ID != selected) {
			continue
		}
		var settings struct {
			Labels map[string]string `json:"labels"`
		}
		if json.Unmarshal([]byte(column.SettingsStr), &settings) != nil {
			continue
		}
		keys := make([]string, 0, len(settings.Labels))
		for k := range settings.Labels {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b string) int {
			ai, aerr := strconv.Atoi(a)
			bi, berr := strconv.Atoi(b)
			if aerr == nil && berr == nil {
				if c := cmp.Compare(ai, bi); c != 0 {
					return c
				}
			}
			return cmp.Compare(a, b)
		})
		var out []wire.Choice
		seen := map[string]bool{}
		for _, k := range keys {
			name := settings.Labels[k]
			if name == "" || seen[name] || utf8.RuneCountInString(name) > wire.StringMost {
				continue
			}
			seen[name] = true
			out = append(out, wire.Choice{Value: name, Title: clipText(name, 200)})
			if len(out) >= 50 {
				break
			}
		}
		return out
	}
	return nil
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
