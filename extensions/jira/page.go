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
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Query    string     `json:"q,omitempty"`
	Token    string     `json:"t,omitempty"`
	History  []string   `json:"h,omitempty"`
	IssueKey string     `json:"k,omitempty"`
	Mode     string     `json:"m,omitempty"`
	DraftID  string     `json:"d,omitempty"`
	Create   createArgs `json:"c,omitempty"`
}

const maxQueryChars = 200

func validMode(mode string) bool {
	switch mode {
	case "", "list", "detail", "create", "review":
		return true
	default:
		return false
	}
}

func boundedKey(prefix, id string) string {
	key := prefix + id
	if len(key) <= 128 && utf8.RuneCountInString(key) <= 128 {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "k-" + hex.EncodeToString(sum[:16])
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || !validMode(st.Mode) || utf8.RuneCountInString(st.Query) > maxQueryChars || len(st.Token) > maxPageToken || len(st.History) >= maxViewPages || len(st.IssueKey) > 64 || len(st.DraftID) > 128 {
		return pageState{}, errors.New("invalid page state")
	}
	for _, h := range st.History {
		if len(h) > maxPageToken {
			return pageState{}, errors.New("invalid page state")
		}
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
		st.Query, st.Token, st.History, st.Mode = clipText(r.Values["q"], maxQueryChars), "", nil, "list"
	case "more":
		token := r.Values["token"]
		if token == "" || token == st.Token || slices.Contains(st.History, token) || len(token) > maxPageToken || utf8.RuneCountInString(token) > wire.StringMost {
			return nil, errors.New("invalid page token")
		}
		st.History = append(st.History, st.Token)
		st.Token = token
		if len(st.History) >= maxViewPages || len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("pagination state exceeds page limit")
		}
	case "detail":
		if err := validKey(r.Values["id"]); err != nil {
			return nil, err
		}
		st.IssueKey, st.Mode = r.Values["id"], "detail"
	case "back":
		st.Mode, st.IssueKey = "list", ""
	case "create":
		st.Mode, st.DraftID = "create", randomID()
	case "review-create":
		in := createArgs{ProjectID: r.Values["project_id"], IssueTypeID: r.Values["issuetype_id"], Summary: r.Values["summary"], Description: r.Values["description"]}
		if err := in.validate(); err != nil {
			return nil, err
		}
		st.Create, st.Mode = in, "review"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("issue draft exceeds page state limit")
		}
	case "confirm-create":
		if st.DraftID == "" {
			return nil, errors.New("the create form expired")
		}
		raw, _ := json.Marshal(st.Create)
		out, err := a.doAction(ctx, "ui-create:"+st.DraftID, "jira_create", raw)
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
	if st.Mode == "create" {
		return a.createView(ctx, r, st)
	}
	if st.Mode == "review" {
		return a.reviewCreateView(ctx, r, st)
	}
	if st.Mode == "detail" {
		item, err := a.client.issue(ctx, st.IssueKey)
		a.record(err)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item), nil
	}
	issues, next, err := a.collectIssues(ctx, st)
	a.record(err)
	if err != nil {
		return nil, err
	}
	v := &wire.View{Title: t.issues, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	section := wire.Section{Title: t.recent, Items: []wire.Component{{
		Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "create", Push: &wire.Push{State: ext.EncodeState(pageState{Mode: "create", DraftID: randomID()}), Title: t.create}},
	}}}
	for _, item := range issues {
		if item.Key == "" {
			continue
		}
		if st.Query != "" && !strings.Contains(strings.ToLower(item.Fields.Summary+" "+item.Key), strings.ToLower(st.Query)) {
			continue
		}
		title := clipText(strings.TrimSpace(item.Key+" "+item.Fields.Summary), 1800)
		if title == "" {
			title = clipText(item.Key, 1800)
		}
		row := wire.Component{Type: wire.ComponentRow, Key: boundedKey("issue-", item.Key), Title: title, Subtitle: clipText(item.Fields.Project.Key+" · "+item.Fields.Status.Name, 1800)}
		if validKey(item.Key) == nil {
			row.Action = &wire.Action{ID: "detail", Values: map[string]string{"id": item.Key}}
		}
		section.Items = append(section.Items, row)
	}
	if len(section.Items) == 1 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noIssues})
	}
	if next != "" && next != st.Token && !slices.Contains(st.History, next) && len(next) <= maxPageToken && utf8.RuneCountInString(next) <= wire.StringMost {
		nextState := st
		nextState.History = append(slices.Clone(st.History), st.Token)
		nextState.Token = next
		if len(nextState.History) < maxViewPages && len(ext.EncodeState(nextState)) <= wire.StateMost {
			section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more", Values: map[string]string{"token": next}}}
		}
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

const (
	maxViewIssues = 250
	maxViewPages  = 10
)

// collectIssues reads the pages the view has gone through, the tokens in History and then Token, so that Load more
// answers the whole list, longer. It stops at maxViewIssues to keep the view within the SDK's component limit, and
// returns a next token only while another page can be loaded. An issue that moved between two reads (the query orders
// by update time) is listed once.
func (a *app) collectIssues(ctx context.Context, st pageState) ([]issue, string, error) {
	var out []issue
	seen := map[string]bool{}
	next := ""
	for _, token := range append(slices.Clone(st.History), st.Token) {
		if len(out) >= maxViewIssues {
			break
		}
		batch, err := a.client.search(ctx, token)
		if err != nil {
			return nil, "", err
		}
		for _, item := range batch.Issues {
			if !seen[item.Key] {
				seen[item.Key] = true
				out = append(out, item)
			}
		}
		next = batch.next()
	}
	if len(out) > maxViewIssues {
		out = out[:maxViewIssues]
	}
	if len(out) >= maxViewIssues || len(st.History)+1 >= maxViewPages {
		next = ""
	}
	return out, next, nil
}

// httpsLink reports whether a link component can carry raw: the view check refuses the whole view for any other.
func httpsLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

func detailView(r *ext.UIRequest, st pageState, item issue) *wire.View {
	t := textsFor(r.Locale)
	var items []wire.Component
	if summary := clipText(strings.TrimSpace(item.Fields.Summary), 1800); summary != "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: summary})
	}
	items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(t.project+": "+item.Fields.Project.Name+" · "+t.status+": "+item.Fields.Status.Name, 1800)})
	if item.Fields.Assignee != nil {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(t.assignee+": "+item.Fields.Assignee.DisplayName, 1800)})
	}
	if httpsLink(item.URL) {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: item.URL})
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	title := clipText(item.Key, 1800)
	if title == "" {
		title = t.issues
	}
	v := &wire.View{Title: title, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.issues, Items: items}}}
	used := len(items)
	if text := richText(item.Fields.Description); text != "" {
		description := capComponents(textComponents(text), maxDescriptionItems)
		used += len(description)
		v.Sections = append(v.Sections, wire.Section{Title: t.description, Items: description})
	}
	if len(item.Fields.Comment.Comments) > 0 {
		section := wire.Section{Title: t.comments}
		for _, c := range item.Fields.Comment.Comments {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(c.Author.DisplayName+" · "+c.Created, 1800), Style: wire.TextStyleSecondary})
			section.Items = append(section.Items, textComponents(richText(c.Body))...)
		}
		section.Items = capComponents(section.Items, maxViewItems-used)
		v.Sections = append(v.Sections, section)
	}
	return v
}

const (
	// maxViewItems keeps the issue view within the SDK's 300 component limit, whatever the issue holds.
	maxViewItems = 250
	// maxDescriptionItems leaves the comments room when the description is long.
	maxDescriptionItems = 100
)

// capComponents keeps the first n components and shows the cut on the last one.
func capComponents(items []wire.Component, n int) []wire.Component {
	if len(items) <= n {
		return items
	}
	items = items[:n]
	items[n-1].Text = clipText(items[n-1].Text, 1700) + " […]"
	return items
}

func (a *app) createView(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	projects, err := a.client.projects(ctx)
	if err != nil {
		return nil, err
	}
	choices := []wire.Choice{}
	seen := map[string]bool{}
	for _, project := range projects {
		id, ok := project["id"].(string)
		id = strings.TrimSpace(id)
		if !ok || id == "" || len(id) > 64 || strings.ContainsAny(id, "\x00\r\n") || seen[id] {
			continue
		}
		seen[id] = true
		name, _ := project["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			name = id
		}
		choices = append(choices, wire.Choice{Value: id, Title: clipText(name, 200)})
		if len(choices) >= 50 {
			break
		}
	}
	if len(choices) == 0 {
		return nil, errors.New("no accessible Jira project")
	}
	items := []wire.Component{
		{Type: wire.ComponentField, Kind: "choice", Name: "project_id", Label: t.project, Value: choices[0].Value, Choices: choices},
		{Type: wire.ComponentField, Kind: "text", Name: "issuetype_id", Label: "Issue type ID"},
		{Type: wire.ComponentField, Kind: "text", Name: "summary", Label: t.issues},
		{Type: wire.ComponentField, Kind: "text", Name: "description", Label: t.description},
		{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "review-create", Submit: true}},
	}
	return &wire.View{Title: t.create, State: ext.EncodeState(st), Sections: []wire.Section{{Items: items}}}, nil
}

func (a *app) reviewCreateView(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	if err := st.Create.validate(); err != nil {
		return nil, err
	}
	t := textsFor(r.Locale)
	items := []wire.Component{
		{Type: wire.ComponentText, Text: clipText(st.Create.Summary, 1800)},
		{Type: wire.ComponentText, Text: clipText(t.project+": "+st.Create.ProjectID+" · "+st.Create.IssueTypeID, 1800)},
	}
	if st.Create.Description != "" {
		items = append(items, capComponents(textComponents(st.Create.Description), maxViewItems-3)...)
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.confirm, Action: &wire.Action{ID: "confirm-create", Confirm: clipText(t.create+"\n"+st.Create.Summary, wire.StringMost)}})
	return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Items: items}}}, nil
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
