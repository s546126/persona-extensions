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
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const (
	maxViewItems = 250
	maxDescItems = 120
)

type pageState struct {
	Team    string     `json:"t,omitempty"`
	Query   string     `json:"q,omitempty"`
	Cursor  string     `json:"c,omitempty"`
	IssueID string     `json:"i,omitempty"`
	Mode    string     `json:"m,omitempty"`
	DraftID string     `json:"d,omitempty"`
	Create  createArgs `json:"n,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || len([]rune(st.Query)) > 200 || len([]rune(st.Team)) > 200 || len(st.Cursor) > 256 || len(st.IssueID) > 128 || len(st.DraftID) > 128 {
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
		st.Query, st.Cursor, st.Mode = r.Values["q"], "", "list"
	case "team":
		if len([]rune(r.Values["team"])) > 200 {
			return nil, errors.New("team filter is too long")
		}
		st.Team, st.Cursor, st.Mode = r.Values["team"], "", "list"
	case "more":
		if st.Cursor == "" {
			return nil, errors.New("no further issue page")
		}
	case "detail":
		if err := validID(r.Values["id"]); err != nil {
			return nil, err
		}
		st.IssueID, st.Mode = r.Values["id"], "detail"
	case "back":
		st.Mode, st.IssueID = "list", ""
	case "create":
		st.Mode, st.DraftID = "create", randomID()
	case "review-create":
		teamID, title, desc := r.Values["team_id"], r.Values["title"], r.Values["description"]
		if err := validID(teamID); err != nil {
			return nil, err
		}
		if err := validText(title, 255, false); err != nil || strings.TrimSpace(title) == "" {
			return nil, errors.New("title is required")
		}
		if err := validText(desc, 100000, true); err != nil {
			return nil, err
		}
		st.Create = createArgs{TeamID: teamID, Title: title, Description: desc}
		st.Mode = "review"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("issue draft exceeds page state limit")
		}
	case "confirm-create":
		if st.DraftID == "" {
			return nil, errors.New("the create form expired")
		}
		raw, _ := json.Marshal(st.Create)
		out, err := a.doAction(ctx, "ui-create:"+st.DraftID, "linear_create", raw)
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
	if a.client.key == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingToken}}}}}, nil
	}
	if st.Mode == "create" {
		return a.createView(ctx, r, st)
	}
	if st.Mode == "review" {
		return a.reviewCreateView(ctx, r, st)
	}
	if st.Mode == "detail" {
		item, err := a.client.issue(ctx, st.IssueID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item), nil
	}
	page, err := a.client.assigned(ctx, st.Cursor)
	a.record(err)
	if err != nil {
		return nil, err
	}
	v := &wire.View{Title: t.assigned, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	rows := make([]wire.Component, 0, len(page.Nodes))
	seen := map[string]bool{}
	for _, item := range page.Nodes {
		if seen[item.ID] {
			continue
		}
		if st.Team != "" && !strings.EqualFold(item.Team.Key, st.Team) && !strings.EqualFold(item.Team.Name, st.Team) {
			continue
		}
		if st.Query != "" && !strings.Contains(strings.ToLower(item.Title+" "+item.Identifier), strings.ToLower(st.Query)) {
			continue
		}
		seen[item.ID] = true
		rows = append(rows, issueRow(item))
	}
	section := wire.Section{Title: fmt.Sprintf(t.results, len(rows)), Items: []wire.Component{{
		Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "create", Push: &wire.Push{State: ext.EncodeState(pageState{Mode: "create", DraftID: randomID()}), Title: t.create}},
	}}}
	section.Items = append(section.Items, rows...)
	if len(rows) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noIssues})
	}
	if page.PageInfo.HasNextPage && page.PageInfo.EndCursor != "" {
		st.Cursor = page.PageInfo.EndCursor
		v.State = ext.EncodeState(st)
		section.More = &wire.More{Title: "Load more", Action: wire.Action{ID: "more"}}
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

func issueRow(item issue) wire.Component {
	subtitle := item.Team.Key + " · " + item.State.Name
	if item.DueDate != "" {
		subtitle += " · " + item.DueDate
	}
	title := clipText(strings.TrimSpace(item.Identifier+" "+item.Title), 2000)
	if title == "" {
		title = clipText(strings.TrimSpace(item.ID), 2000)
	}
	if title == "" {
		title = "Issue"
	}
	return wire.Component{Type: wire.ComponentRow, Key: boundedKey("issue-", item.ID), Title: title, Subtitle: clipText(subtitle, 2000), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID}}}
}

func httpsLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

func detailView(r *ext.UIRequest, st pageState, item issue) *wire.View {
	t := textsFor(r.Locale)
	viewTitle := clipText(strings.TrimSpace(item.Identifier), 2000)
	if viewTitle == "" {
		viewTitle = clipText(strings.TrimSpace(item.Title), 2000)
	}
	if viewTitle == "" {
		viewTitle = clipText(strings.TrimSpace(item.ID), 2000)
	}
	if viewTitle == "" {
		viewTitle = t.assigned
	}
	items := []wire.Component{}
	if title := clipText(strings.TrimSpace(item.Title), 2000); title != "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: title})
	}
	items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(t.team+": "+item.Team.Name+" · "+t.state+": "+item.State.Name+" ("+item.State.Type+")", 2000)})
	if item.Assignee != nil {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(t.assignee+": "+item.Assignee.DisplayName, 2000)})
	}
	if item.DueDate != "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(t.due+": "+item.DueDate, 2000)})
	}
	if httpsLink(item.URL) {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: item.URL})
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	v := &wire.View{Title: viewTitle, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.assigned, Items: items}}}
	used := len(items)
	if item.Description != "" {
		desc := capComponents(textComponents(item.Description), min(maxDescItems, maxViewItems-used))
		if len(desc) > 0 {
			used += len(desc)
			v.Sections = append(v.Sections, wire.Section{Title: t.description, Items: desc})
		}
	}
	if len(item.Comments.Nodes) > 0 && used < maxViewItems {
		section := wire.Section{Title: t.comments}
		limit := maxViewItems - used
		for _, c := range item.Comments.Nodes {
			if len(section.Items) > limit {
				break
			}
			body := textComponents(c.Body)
			author := ""
			if c.User != nil && strings.TrimSpace(c.User.DisplayName) != "" {
				author = c.User.DisplayName
			}
			header := strings.TrimSpace(author)
			if ts := strings.TrimSpace(c.CreatedAt); ts != "" {
				if header != "" {
					header += " · " + ts
				} else {
					header = ts
				}
			}
			if header == "" && len(body) == 0 {
				continue
			}
			if header != "" {
				section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(header, 2000), Style: wire.TextStyleSecondary})
			}
			section.Items = append(section.Items, body...)
		}
		section.Items = capComponents(section.Items, limit)
		if len(section.Items) > 0 {
			v.Sections = append(v.Sections, section)
		}
	}
	return v
}

func (a *app) createView(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	teams, err := a.teamsRead(ctx)
	if err != nil {
		return nil, err
	}
	choices := []wire.Choice{}
	for _, team := range teams {
		name := clipText(strings.TrimSpace(team.Name), 2000)
		if name == "" {
			name = clipText(team.ID, 2000)
		}
		choices = append(choices, wire.Choice{Value: team.ID, Title: name})
	}
	if len(choices) == 0 {
		return nil, errors.New("Linear has no accessible team")
	}
	items := []wire.Component{
		{Type: wire.ComponentField, Kind: "choice", Name: "team_id", Label: t.team, Value: choices[0].Value, Choices: choices},
		{Type: wire.ComponentField, Kind: "text", Name: "title", Label: t.assigned},
		{Type: wire.ComponentField, Kind: "text", Name: "description", Label: t.description},
		{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "review-create", Submit: true}},
	}
	return &wire.View{Title: t.create, State: ext.EncodeState(st), Sections: []wire.Section{{Items: items}}}, nil
}

func (a *app) reviewCreateView(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	teams, err := a.teamsRead(ctx)
	if err != nil {
		return nil, err
	}
	teamName := st.Create.TeamID
	for _, team := range teams {
		if team.ID == st.Create.TeamID {
			teamName = team.Name
		}
	}
	title := clipText(strings.TrimSpace(st.Create.Title), 2000)
	if title == "" {
		title = t.create
	}
	items := []wire.Component{
		{Type: wire.ComponentText, Text: title},
		{Type: wire.ComponentText, Text: clipText(t.team+": "+teamName, 2000)},
	}
	items = append(items, capComponents(textComponents(st.Create.Description), maxViewItems-len(items)-1)...)
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.confirm, Action: &wire.Action{ID: "confirm-create", Confirm: clipText(t.create+"\n"+title, 1800)}})
	return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Items: items}}}, nil
}

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

func boundedKey(prefix, id string) string {
	key := prefix + id
	if len(key) <= 128 {
		return key
	}
	sum := sha256.Sum256([]byte(id))
	return prefix + hex.EncodeToString(sum[:16])
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
