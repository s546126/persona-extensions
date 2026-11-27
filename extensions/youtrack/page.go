package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const maxProjectNameChars = 500

type pageState struct {
	Mode        string `json:"m,omitempty"`
	IssueID     string `json:"i,omitempty"`
	Skip        int    `json:"s,omitempty"`
	ProjectID   string `json:"p,omitempty"`
	ProjectName string `json:"n,omitempty"`
	DraftID     string `json:"d,omitempty"`
	Summary     string `json:"a,omitempty"`
	Description string `json:"b,omitempty"`
}

func validMode(mode string) bool {
	switch mode {
	case "", "projects", "detail", "create", "review-create":
		return true
	default:
		return false
	}
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || !validMode(st.Mode) || st.Skip < 0 || len(st.IssueID) > 100 || len(st.ProjectID) > 100 || utf8.RuneCountInString(st.ProjectName) > maxProjectNameChars || len(st.DraftID) > 128 || len([]rune(st.Summary)) > maxSummaryChars || utf8.RuneCountInString(st.Description) > maxDescriptionChars {
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
	case "more":
		st.Skip += 50
	case "detail":
		if err := validResourceID(r.Values["id"]); err != nil {
			return nil, err
		}
		st.Mode, st.IssueID = "detail", r.Values["id"]
	case "projects":
		st = pageState{Mode: "projects"}
	case "create":
		if st.Mode != "review-create" {
			st = pageState{Mode: "create", ProjectID: st.ProjectID, ProjectName: st.ProjectName, DraftID: randomID()}
		} else {
			st.Mode = "create"
		}
	case "back":
		st.Mode, st.IssueID, st.DraftID, st.Summary, st.Description = "", "", "", "", ""
	case "review-create":
		in := createArgs{ProjectID: r.Values["project_id"], Summary: r.Values["summary"], Description: r.Values["description"]}
		if err := in.validate(); err != nil {
			return nil, err
		}
		st.ProjectID, st.Summary, st.Description, st.Mode = in.ProjectID, in.Summary, in.Description, "review-create"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("issue draft exceeds page state limit")
		}
	case "confirm-create":
		if strings.TrimSpace(st.DraftID) == "" {
			return nil, errors.New("missing draft ID")
		}
		raw, _ := json.Marshal(createArgs{ProjectID: st.ProjectID, Summary: st.Summary, Description: st.Description})
		out, err := a.doAction(ctx, "ui-create:"+st.DraftID, "youtrack_create_issue", raw)
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

// collectIssues reads the batches from the start through the one at skip so that Load more answers the whole list,
// longer. It stops at maxViewRecords to keep the view within the SDK's component limit, and reports whether another
// batch can be loaded.
func (a *app) collectIssues(ctx context.Context, skip int) ([]issue, bool, error) {
	var out []issue
	last := 0
	for pos := 0; pos <= skip && len(out) < maxViewRecords; pos += 50 {
		batch, err := a.client.issues(ctx, pos)
		if err != nil {
			return nil, false, err
		}
		last = len(batch)
		out = append(out, batch...)
		if last < 50 {
			break
		}
	}
	if len(out) > maxViewRecords {
		out = out[:maxViewRecords]
	}
	return out, last == 50 && len(out) < maxViewRecords, nil
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if a.client.base == "" || a.client.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "create" || st.Mode == "review-create" {
		return a.createView(ctx, r, st)
	}
	if st.Mode == "projects" {
		projects, err := a.client.projects(ctx, 0)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{Title: t.projects}
		seen := map[string]bool{}
		for _, project := range projects {
			if project.Archived || project.ID == "" || project.Name == "" || seen[project.ID] {
				continue
			}
			seen[project.ID] = true
			row := wire.Component{Type: wire.ComponentRow, Key: boundedKey("project-", project.ID), Title: clipText(project.Name, 1800), Subtitle: clipText(project.ShortName, 500)}
			if validResourceID(project.ID) == nil {
				row.Action = &wire.Action{ID: "create", Push: &wire.Push{Title: t.create, State: ext.EncodeState(pageState{Mode: "create", ProjectID: project.ID, ProjectName: clipText(project.Name, maxProjectNameChars), DraftID: randomID()})}}
			}
			section.Items = append(section.Items, row)
			if len(section.Items) >= maxViewRecords {
				break
			}
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noProjects})
		}
		return &wire.View{Title: t.projects, State: ext.EncodeState(st), Sections: []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}}}}}}, nil
	}
	if st.Mode == "detail" {
		item, err := a.client.issue(ctx, st.IssueID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		comments, err := a.client.comments(ctx, st.IssueID)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item, comments), nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid issue view")
	}
	items, more, err := a.collectIssues(ctx, st.Skip)
	a.record(err)
	if err != nil {
		return nil, err
	}
	section := wire.Section{Title: t.issues}
	seenIssues := map[string]bool{}
	for _, item := range items {
		if item.ID == "" || item.Summary == "" || seenIssues[item.ID] {
			continue
		}
		seenIssues[item.ID] = true
		row := wire.Component{Type: wire.ComponentRow, Key: boundedKey("issue-", item.ID), Title: clipText(item.Summary, 1800), Subtitle: clipText(item.IDReadable+" · "+item.Project.Name, 1800)}
		if validResourceID(item.ID) == nil {
			row.Action = &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID}}
		}
		section.Items = append(section.Items, row)
		if len(section.Items) >= maxViewRecords {
			break
		}
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noIssues})
	}
	if more {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
	}
	v := &wire.View{Title: t.issues, State: ext.EncodeState(st), Sections: []wire.Section{section, {Title: t.issues, Items: []wire.Component{
		{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "projects", Push: &wire.Push{Title: t.projects, State: ext.EncodeState(pageState{Mode: "projects"})}}},
	}}}}
	return v, nil
}

func (a *app) createView(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if st.Mode == "review-create" {
		in := createArgs{ProjectID: st.ProjectID, Summary: st.Summary, Description: st.Description}
		if err := in.validate(); err != nil {
			return nil, err
		}
		items := []wire.Component{{Type: wire.ComponentText, Text: clipText(st.Summary, 1800)}}
		if st.Description != "" {
			items = append(items, textComponents(st.Description)...)
		}
		items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "confirm-create", Confirm: clipText(t.create+"\n"+st.Summary, wire.StringMost)}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "create"}})
		return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.create, Items: items}}}, nil
	}
	if err := validResourceID(st.ProjectID); err != nil {
		return nil, err
	}
	return &wire.View{Title: t.create, State: ext.EncodeState(st), Sections: []wire.Section{{Title: clipText(st.ProjectName, 1800), Items: []wire.Component{
		{Type: wire.ComponentField, Kind: "text", Name: "project_id", Label: t.project, Value: clipText(st.ProjectID, wire.StringMost)},
		{Type: wire.ComponentField, Kind: "text", Name: "summary", Label: t.summary, Value: clipText(st.Summary, wire.StringMost)},
		{Type: wire.ComponentField, Kind: "text", Name: "description", Label: t.description, Value: clipText(st.Description, wire.StringMost)},
		{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "review-create", Submit: true}},
		{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	}}}}, nil
}

func detailView(r *ext.UIRequest, st pageState, item issue, comments []comment) *wire.View {
	t := textsFor(r.Locale)
	reporter := ""
	if item.Reporter != nil {
		reporter = item.Reporter.Name
	}
	items := []wire.Component{
		{Type: wire.ComponentText, Text: clipText(t.project+": "+item.Project.Name+" · "+t.reporter+": "+reporter, 1800)},
		{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	}
	title := clipText(strings.TrimSpace(item.IDReadable+" "+item.Summary), 1800)
	if title == "" {
		title = clipText(item.ID, 1800)
	}
	v := &wire.View{Title: title, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.issues, Items: items}}}
	if item.Description != "" {
		v.Sections = append(v.Sections, wire.Section{Title: t.description, Items: capComponents(textComponents(clipText(item.Description, 60000)), maxDescriptionItems)})
	}
	budget := maxViewItems - viewItemCount(v)
	section := wire.Section{Title: t.comments}
	for _, comment := range comments {
		if comment.Deleted || comment.Text == "" {
			continue
		}
		author := "YouTrack user"
		if comment.Author != nil && comment.Author.Name != "" {
			author = comment.Author.Name
		}
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: clipText(author, 1800), Style: wire.TextStyleSecondary})
		section.Items = append(section.Items, textComponents(clipText(comment.Text, 12000))...)
	}
	section.Items = capComponents(section.Items, budget)
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentText, Text: t.noComments, Style: wire.TextStyleSecondary})
	}
	v.Sections = append(v.Sections, section)
	return v
}

const (
	// maxViewItems keeps the issue view within the hub's 300 component limit, whatever the comments hold.
	maxViewItems = 250
	// maxDescriptionItems leaves the comments room in it.
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

func viewItemCount(v *wire.View) int {
	n := 0
	for _, section := range v.Sections {
		n += len(section.Items)
	}
	return n
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

func boundedKey(prefix, id string) string {
	key := prefix + id
	if len(key) <= 128 && utf8.RuneCountInString(key) <= 128 {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "k-" + hex.EncodeToString(sum[:16])
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
