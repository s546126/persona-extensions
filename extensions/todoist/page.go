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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const maxViewItems = 250

type pageState struct {
	View     string     `json:"v,omitempty"`
	Query    string     `json:"q,omitempty"`
	Pages    int        `json:"n,omitempty"`
	TaskID   string     `json:"t,omitempty"`
	Mode     string     `json:"m,omitempty"`
	CreateID string     `json:"c,omitempty"`
	Create   createArgs `json:"d,omitempty"`
}

func (a *app) state(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Pages < 0 || st.Pages > 5 || len([]rune(st.Query)) > 200 || len(st.TaskID) > 128 || len(st.CreateID) > 128 {
		return pageState{}, errors.New("invalid page state")
	}
	return st, nil
}

func (a *app) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	st, err := a.state(r)
	if err != nil {
		return nil, err
	}
	return a.page(ctx, r, st)
}

func (a *app) action(ctx context.Context, r *ext.UIRequest) (result *wire.ActionResult, err error) {
	st, err := a.state(r)
	if err != nil {
		return nil, err
	}
	switch r.Action {
	case "search":
		if len([]rune(r.Values["q"])) > 200 {
			return nil, errors.New("search query is too long")
		}
		st.View, st.Query, st.Pages, st.Mode = "search", r.Values["q"], 0, "list"
	case "view":
		st.View, st.Pages, st.Mode = r.Values["view"], 0, "list"
	case "more":
		st.Pages = min(st.Pages+1, 5)
	case "detail":
		if err := validID(r.Values["id"]); err != nil {
			return nil, err
		}
		st.TaskID, st.Mode = r.Values["id"], "detail"
		st.CreateID = randomID()
	case "back":
		st.Mode, st.TaskID = "list", ""
	case "create":
		st.Mode, st.CreateID = "create", randomID()
	case "submit-create":
		priority, err := strconv.Atoi(r.Values["priority"])
		if err != nil {
			return nil, errors.New("invalid priority")
		}
		in := createArgs{Content: r.Values["content"], Description: r.Values["description"], DueDate: r.Values["due_date"], ProjectID: r.Values["project_id"], Priority: priority}
		if raw := strings.TrimSpace(r.Values["labels"]); raw != "" {
			for _, part := range strings.Split(raw, ",") {
				if label := strings.TrimSpace(part); label != "" {
					in.Labels = append(in.Labels, label)
				}
			}
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		st.Create, st.Mode = in, "review"
		if st.CreateID == "" {
			st.CreateID = randomID()
		}
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("task draft exceeds page state limit")
		}
	case "confirm-create":
		if st.CreateID == "" {
			return nil, errors.New("the create form expired; reopen it")
		}
		raw, _ := json.Marshal(st.Create)
		result, err := a.doAction(ctx, "ui-create:"+st.CreateID, "todoist_create", raw)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: result["text"].(string), Pop: true}, nil
	case "complete":
		if err := validID(st.TaskID); err != nil {
			return nil, err
		}
		if st.CreateID == "" {
			return nil, errors.New("the task view expired; reopen it")
		}
		result, err := a.doAction(ctx, "ui-complete:"+st.CreateID, "todoist_complete", []byte(`{"id":`+strconv.Quote(st.TaskID)+`}`))
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: result["text"].(string), Pop: true}, nil
	default:
		return nil, fmt.Errorf("unknown page action %q", r.Action)
	}
	v, err := a.page(ctx, r, st)
	return &wire.ActionResult{View: v}, err
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if a.client.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingToken}}}}}, nil
	}
	if st.Mode == "create" {
		return a.createView(ctx, r, st)
	}
	if st.Mode == "review" {
		return a.reviewCreateView(ctx, r, st)
	}
	now, err := timeInZone(r.TZ)
	if err != nil {
		return nil, err
	}
	if st.Mode == "detail" {
		item, err := a.client.task(ctx, st.TaskID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		data, err := a.contextRead(ctx)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item, data, now.Location()), nil
	}
	switch st.View {
	case "today", "overdue", "upcoming", "all", "search":
	default:
		st.View = "today"
	}
	items, err := a.client.tasks(ctx, "", map[bool]string{true: st.Query}[st.View == "search"], st.Pages+1)
	a.record(err)
	if err != nil {
		return nil, err
	}
	filtered := filterTasks(items, st.View, st.Query, now)
	dedup := make([]task, 0, min(len(filtered), 50))
	seen := map[string]bool{}
	for _, item := range filtered {
		if seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		dedup = append(dedup, item)
		if len(dedup) == 50 {
			break
		}
	}
	data, err := a.contextRead(ctx)
	if err != nil {
		return nil, err
	}
	v := &wire.View{Title: t.today, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	choices := []wire.Choice{{Value: "today", Title: t.today}, {Value: "overdue", Title: t.overdue}, {Value: "upcoming", Title: t.upcoming}, {Value: "all", Title: t.all}}
	if st.View == "search" {
		choices = append(choices, wire.Choice{Value: "search", Title: t.search})
	}
	v.Filter = &wire.Filter{Name: "view", Title: t.tasks, Value: st.View, Choices: choices, Action: wire.Action{ID: "view"}}
	section := wire.Section{Title: fmt.Sprintf(t.tasks, len(dedup)), Items: []wire.Component{{
		Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "create", Push: &wire.Push{State: ext.EncodeState(pageState{Mode: "create", CreateID: randomID()}), Title: t.create}},
	}}}
	for _, item := range dedup {
		section.Items = append(section.Items, taskRow(item, data, now.Location()))
	}
	if len(dedup) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noTasks})
	}
	if len(items) >= 50*(st.Pages+1) && st.Pages < 5 {
		section.More = &wire.More{Title: textsFor(r.Locale).more, Action: wire.Action{ID: "more"}}
	}
	v.Sections = []wire.Section{section}
	return v, nil
}

func timeInZone(zone string) (time.Time, error) {
	if zone == "" {
		zone = "UTC"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, errors.New("invalid time zone")
	}
	return time.Now().In(loc), nil
}

func taskTitle(item task) string {
	if title := clipText(strings.TrimSpace(item.Content), 2000); title != "" {
		return title
	}
	if id := clipText(strings.TrimSpace(item.ID), 2000); id != "" {
		return id
	}
	return "Task"
}

func taskRow(item task, data contextData, loc *time.Location) wire.Component {
	parts := []string{}
	if p, ok := data.Projects[item.ProjectID]; ok {
		parts = append(parts, p.Name)
	}
	if item.Due != nil {
		parts = append(parts, item.Due.label(loc))
		if item.Due.IsRecurring {
			parts = append(parts, "↻")
		}
	}
	if len(item.Labels) > 0 {
		parts = append(parts, strings.Join(item.Labels, ", "))
	}
	return wire.Component{Type: wire.ComponentRow, Key: boundedKey("task-", item.ID), Title: taskTitle(item),
		Subtitle: clipText(strings.Join(parts, " · "), 2000), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID}}}
}

func detailView(r *ext.UIRequest, st pageState, item task, data contextData, loc *time.Location) *wire.View {
	t := textsFor(r.Locale)
	title := taskTitle(item)
	items := []wire.Component{{Type: wire.ComponentText, Text: title}}
	if p, ok := data.Projects[item.ProjectID]; ok {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(t.project+": "+p.Name, 2000), Style: wire.TextStyleSecondary})
	}
	if item.Due != nil {
		due := t.due + ": " + item.Due.label(loc)
		if item.Due.IsRecurring {
			due += " · " + t.recurring
		}
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(due, 2000)})
	}
	if len(item.Labels) > 0 {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(t.labels+": "+strings.Join(item.Labels, ", "), 2000)})
	}
	link := "https://app.todoist.com/app/task/" + url.PathEscape(item.ID)
	items = append(items,
		wire.Component{Type: wire.ComponentLink, Title: t.open, URL: link},
		wire.Component{Type: wire.ComponentButton, Title: t.complete, Action: &wire.Action{ID: "complete", Confirm: clipText(t.complete+"\n"+title, 1800)}},
		wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	)
	v := &wire.View{Title: title, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.detail, Items: items}}}
	if item.Description != "" {
		desc := capComponents(textComponents(item.Description), maxViewItems-len(items))
		if len(desc) > 0 {
			v.Sections = append(v.Sections, wire.Section{Title: t.description, Items: desc})
		}
	}
	return v
}

func (a *app) reviewCreateView(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	data, err := a.contextRead(ctx)
	if err != nil {
		return nil, err
	}
	title := clipText(strings.TrimSpace(st.Create.Content), 2000)
	if title == "" {
		title = t.create
	}
	items := []wire.Component{{Type: wire.ComponentText, Text: title}}
	if st.Create.ProjectID != "" {
		name := data.Projects[st.Create.ProjectID].Name
		if name == "" {
			name = st.Create.ProjectID
		}
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(t.project+": "+name, 2000)})
	}
	if st.Create.DueDate != "" {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(t.due+": "+st.Create.DueDate, 2000)})
	}
	if len(st.Create.Labels) > 0 {
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(t.labels+": "+strings.Join(st.Create.Labels, ", "), 2000)})
	}
	if st.Create.Description != "" {
		items = append(items, capComponents(textComponents(st.Create.Description), maxViewItems-len(items)-1)...)
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.confirm, Action: &wire.Action{ID: "confirm-create", Confirm: clipText(t.create+"\n"+title, 1800)}})
	return &wire.View{Title: t.create, State: ext.EncodeState(st), Sections: []wire.Section{{Items: items}}}, nil
}

func (a *app) createView(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	data, err := a.contextRead(ctx)
	if err != nil {
		return nil, err
	}
	list := make([]project, 0, len(data.Projects))
	for _, p := range data.Projects {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Inbox != list[j].Inbox {
			return list[i].Inbox
		}
		ni, nj := strings.ToLower(list[i].Name), strings.ToLower(list[j].Name)
		if ni != nj {
			return ni < nj
		}
		return list[i].ID < list[j].ID
	})
	projects := make([]wire.Choice, 0, len(list))
	for _, p := range list {
		name := clipText(strings.TrimSpace(p.Name), 80)
		if name == "" {
			name = clipText(p.ID, 80)
		}
		projects = append(projects, wire.Choice{Value: p.ID, Title: name})
	}
	items := []wire.Component{
		{Type: wire.ComponentField, Kind: "text", Name: "content", Label: t.detail},
		{Type: wire.ComponentField, Kind: "text", Name: "description", Label: t.description},
		{Type: wire.ComponentField, Kind: "text", Name: "labels", Label: t.labels, Placeholder: "research, planning"},
		{Type: wire.ComponentField, Kind: "text", Name: "due_date", Label: t.due, Placeholder: "YYYY-MM-DD"},
		{Type: wire.ComponentField, Name: "priority", Kind: "choice", Label: t.priority, Value: "4", Choices: []wire.Choice{{Value: "1", Title: "1"}, {Value: "2", Title: "2"}, {Value: "3", Title: "3"}, {Value: "4", Title: "4"}}},
	}
	if len(projects) > 0 {
		items = append(items, wire.Component{Type: wire.ComponentField, Kind: "choice", Name: "project_id", Label: t.project, Value: projects[0].Value, Choices: projects})
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "submit-create", Submit: true}})
	return &wire.View{Title: t.create, State: ext.EncodeState(st), Sections: []wire.Section{{Items: items}}}, nil
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

func jsonValues(values map[string]string) []byte {
	out, _ := json.Marshal(values)
	return out
}

func randomID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes[:])
}
