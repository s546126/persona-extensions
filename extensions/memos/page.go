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
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	View      string   `json:"v,omitempty"`
	Query     string   `json:"q,omitempty"`
	PageToken string   `json:"p,omitempty"`
	NextToken string   `json:"n,omitempty"`
	History   []string `json:"h,omitempty"`
	MemoID    string   `json:"i,omitempty"`
	Mode      string   `json:"m,omitempty"`
	DraftID   string   `json:"d,omitempty"`
	Content   string   `json:"c,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || len(st.MemoID) > 80 || len(st.DraftID) > 128 || len([]rune(st.Content)) > maxContentStateChars || validPageToken(st.PageToken) != nil || validPageToken(st.NextToken) != nil || len([]rune(st.Query)) > maxQueryChars || len(st.History) >= maxViewPages {
		return pageState{}, errors.New("invalid page state")
	}
	if st.View != "" && st.View != "recent" && st.View != "pinned" && st.View != "search" {
		return pageState{}, errors.New("invalid page state")
	}
	seen := map[string]bool{}
	for _, tok := range st.History {
		if validPageToken(tok) != nil || seen[tok] {
			return pageState{}, errors.New("invalid page state")
		}
		seen[tok] = true
	}
	if len(st.History) > 0 && seen[st.PageToken] {
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
		st.Query, st.PageToken, st.NextToken, st.History = clipText(r.Values["q"], maxQueryChars), "", "", nil
		if strings.TrimSpace(st.Query) != "" {
			st.View = "search"
		} else {
			st.View = "recent"
		}
	case "view":
		if r.Values["view"] != "recent" && r.Values["view"] != "pinned" {
			return nil, errors.New("invalid notes view")
		}
		st.View, st.Query, st.PageToken, st.NextToken, st.History = r.Values["view"], "", "", "", nil
	case "more":
		tok := r.Values["token"]
		if st.NextToken != "" && tok == pageTokenRef(st.NextToken) {
			tok = st.NextToken
		}
		if err := validPageToken(tok); err != nil || tok == "" {
			return nil, errors.New("invalid next page token")
		}
		if len(st.History)+1 >= maxViewPages || tok == st.PageToken || slices.Contains(st.History, tok) {
			return nil, errors.New("invalid next page token")
		}
		st.History = append(st.History, st.PageToken)
		st.PageToken = tok
		st.NextToken = ""
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("page state limit reached")
		}
	case "detail":
		if err := validMemoID(r.Values["id"]); err != nil {
			return nil, err
		}
		st.Mode, st.MemoID = "detail", r.Values["id"]
	case "create":
		if st.Mode != "review-create" {
			st = pageState{Mode: "create", DraftID: randomID()}
		} else {
			st.Mode = "create"
		}
	case "edit":
		st.Mode = "edit"
		if st.DraftID == "" {
			st.DraftID = randomID()
		}
	case "back":
		st.Mode, st.MemoID, st.DraftID, st.Content = "", "", "", ""
	case "review-create":
		if err := validContent(r.Values["content"]); err != nil {
			return nil, err
		}
		st.Content, st.Mode = r.Values["content"], "review-create"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("note is too large for the page editor; use the memos_create action")
		}
	case "review-edit":
		if err := validContent(r.Values["content"]); err != nil {
			return nil, err
		}
		st.Content, st.Mode = r.Values["content"], "review-edit"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("note is too large for the page editor; use the memos_update action")
		}
	case "confirm-create":
		if st.DraftID == "" {
			return nil, errors.New("draft is required")
		}
		raw, _ := json.Marshal(createArgs{Content: st.Content})
		out, err := a.doAction(ctx, "ui-create:"+st.DraftID, "memos_create", raw)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	case "confirm-edit":
		if st.MemoID == "" || st.DraftID == "" {
			return nil, errors.New("memo and draft are required")
		}
		raw, _ := json.Marshal(updateArgs{MemoID: st.MemoID, Content: st.Content})
		out, err := a.doAction(ctx, "ui-edit:"+st.DraftID, "memos_update", raw)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	case "pin":
		if st.MemoID == "" || st.DraftID == "" {
			return nil, errors.New("memo and draft are required")
		}
		pinned, err := strconvBool(r.Values["pinned"])
		if err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(pinArgs{MemoID: st.MemoID, Pinned: &pinned})
		out, err := a.doAction(ctx, "ui-pin:"+st.DraftID, "memos_toggle_pin", raw)
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
	maxViewRecords       = 250
	maxViewPages         = 10
	maxEditContentChars  = 2000
	maxContentStateChars = 10000
)

func pageTokenRef(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func memoTitle(item memo, fallbackID string) string {
	for _, candidate := range []string{item.Snippet, item.Content} {
		if s := strings.TrimSpace(candidate); s != "" {
			return s
		}
	}
	if fallbackID != "" {
		return "#" + fallbackID
	}
	return "Memo"
}

// collectMemos reads the pages the view has gone through, the tokens in History and then PageToken, so that Load more
// answers the whole list, longer. It stops at maxViewRecords to keep the view within the SDK's component limit, and the
// page it returns carries a NextPageToken only while another page can be loaded.
func (a *app) collectMemos(ctx context.Context, st pageState) (memoPage, error) {
	var out memoPage
	seenIDs := map[string]bool{}
	seenTokens := map[string]bool{}
	tokens := append(slices.Clone(st.History), st.PageToken)
	for _, token := range tokens {
		seenTokens[token] = true
	}
	for _, token := range tokens {
		if len(out.Memos) >= maxViewRecords {
			break
		}
		batch, err := a.client.memos(ctx, a.creator, st.View, st.Query, token)
		if err != nil {
			return memoPage{}, err
		}
		for _, item := range batch.Memos {
			id := memoID(item.Name)
			if id == "" || item.Creator != a.creator || seenIDs[id] {
				continue
			}
			if strings.TrimSpace(item.Snippet) == "" && strings.TrimSpace(item.Content) == "" {
				continue
			}
			seenIDs[id] = true
			out.Memos = append(out.Memos, item)
			if len(out.Memos) >= maxViewRecords {
				break
			}
		}
		out.NextPageToken = batch.NextPageToken
	}
	if len(out.Memos) >= maxViewRecords || len(st.History)+1 >= maxViewPages || out.NextPageToken == "" || seenTokens[out.NextPageToken] || validPageToken(out.NextPageToken) != nil {
		out.NextPageToken = ""
	}
	return out, nil
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if a.client.base == "" || a.client.token == "" || a.creator == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	switch st.Mode {
	case "create", "review-create", "edit", "review-edit":
		return a.editorView(r, st), nil
	case "detail":
		if err := validMemoID(st.MemoID); err != nil {
			return nil, err
		}
		item, err := a.client.memo(ctx, st.MemoID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		if item.Creator != a.creator {
			return nil, errors.New("this note belongs to another user")
		}
		st.DraftID = randomID()
		return detailView(r, st, item), nil
	case "":
	default:
		return nil, errors.New("invalid notes view")
	}
	if st.View == "" {
		st.View = "recent"
	}
	if st.View != "recent" && st.View != "pinned" && st.View != "search" {
		return nil, errors.New("invalid notes view")
	}
	var page memoPage
	if st.View != "search" || strings.TrimSpace(st.Query) != "" {
		var err error
		page, err = a.collectMemos(ctx, st)
		a.record(err)
		if err != nil {
			return nil, err
		}
	}
	st.NextToken = ""
	var more *wire.More
	if page.NextPageToken != "" {
		nextSt := st
		nextSt.History = append(slices.Clone(st.History), st.PageToken)
		nextSt.PageToken = page.NextPageToken
		moreVal := page.NextPageToken
		if len([]rune(moreVal)) > wire.StringMost {
			moreVal = pageTokenRef(page.NextPageToken)
			st.NextToken = page.NextPageToken
		}
		if len(ext.EncodeState(st)) <= wire.StateMost && len(ext.EncodeState(nextSt)) <= wire.StateMost {
			more = &wire.More{Title: t.more, Action: wire.Action{ID: "more", Values: map[string]string{"token": moreVal}}}
		} else {
			st.NextToken = ""
		}
	}
	v := &wire.View{Title: t.notes, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}, Filter: &wire.Filter{
		Name: "view", Title: t.notes, Value: st.View, Action: wire.Action{ID: "view"},
		Choices: []wire.Choice{{Value: "recent", Title: t.recent}, {Value: "pinned", Title: t.pinned}},
	}}
	if st.View == "search" {
		v.Filter.Value = "recent"
	}
	section := wire.Section{}
	for _, item := range page.Memos {
		id := memoID(item.Name)
		if id == "" || item.Creator != a.creator {
			continue
		}
		title := item.Snippet
		if title == "" {
			title = item.Content
		}
		if title == "" {
			continue
		}
		subtitle := item.UpdateTime
		if item.Pinned {
			subtitle = "📌 " + subtitle
		}
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "memo-" + id, Title: clipText(title, 1800), Subtitle: clipText(subtitle, 1800), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": id}}})
	}
	section.Title = fmt.Sprintf(t.results, len(section.Items))
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noNotes})
	}
	if more != nil {
		section.More = more
	}
	v.Sections = []wire.Section{section, {Title: t.notes, Items: []wire.Component{{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "create", Push: &wire.Push{Title: t.create, State: ext.EncodeState(pageState{Mode: "create", DraftID: randomID()})}}}}}}
	return v, nil
}

func (a *app) editorView(r *ext.UIRequest, st pageState) *wire.View {
	t := textsFor(r.Locale)
	if st.Mode == "review-create" || st.Mode == "review-edit" {
		action := "confirm-create"
		if st.Mode == "review-edit" {
			action = "confirm-edit"
		}
		items := textComponents(st.Content)
		items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.save, Action: &wire.Action{ID: action, Confirm: t.save + "\n" + clipText(st.Content, 500)}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: map[bool]string{true: "edit", false: "create"}[st.Mode == "review-edit"]}})
		return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.review, Items: items}}}
	}
	action, title := "review-create", t.create
	if st.Mode == "edit" {
		action, title = "review-edit", t.edit
	}
	fieldValue := st.Content
	if len([]rune(fieldValue)) > maxEditContentChars {
		fieldValue = clipText(fieldValue, maxEditContentChars)
	}
	return &wire.View{Title: title, State: ext.EncodeState(st), Sections: []wire.Section{{Items: []wire.Component{
		{Type: wire.ComponentField, Kind: "text", Name: "content", Label: t.content, Value: fieldValue},
		{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: action, Submit: true}},
		{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	}}}}
}

func detailView(r *ext.UIRequest, st pageState, item memo) *wire.View {
	t := textsFor(r.Locale)
	pinTitle := t.pin
	pinned := true
	if item.Pinned {
		pinTitle, pinned = t.unpin, false
	}
	var items []wire.Component
	if strings.TrimSpace(item.UpdateTime) != "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(item.UpdateTime, 100)})
	}
	if len([]rune(item.Content)) <= maxEditContentChars {
		editState := ext.EncodeState(pageState{Mode: "edit", MemoID: st.MemoID, Content: item.Content, DraftID: randomID()})
		if len(editState) <= wire.StateMost {
			items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.edit, Action: &wire.Action{ID: "edit", Push: &wire.Push{Title: t.edit, State: editState}}})
		}
	}
	title := memoTitle(item, st.MemoID)
	items = append(items,
		wire.Component{Type: wire.ComponentButton, Title: pinTitle, Action: &wire.Action{ID: "pin", Values: map[string]string{"pinned": fmt.Sprint(pinned)}, Confirm: pinTitle + "\n" + clipText(title, 500)}},
		wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	)
	v := &wire.View{Title: clipText(title, 1800), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.notes, Items: items}}}
	if item.Content != "" {
		v.Sections = append(v.Sections, wire.Section{Title: t.content, Items: textComponents(clipText(item.Content, 60000))})
	} else {
		v.Sections = append(v.Sections, wire.Section{Title: t.content, Items: []wire.Component{{Type: wire.ComponentText, Text: t.noContent, Style: wire.TextStyleSecondary}}})
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

func strconvBool(value string) (bool, error) {
	if value == "true" {
		return true, nil
	}
	if value == "false" {
		return false, nil
	}
	return false, errors.New("invalid pinned flag")
}
