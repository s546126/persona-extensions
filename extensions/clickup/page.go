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

type pageState struct {
	Mode        string `json:"m,omitempty"`
	TeamID      string `json:"t,omitempty"`
	TeamName    string `json:"n,omitempty"`
	SpaceID     string `json:"s,omitempty"`
	SpaceName   string `json:"p,omitempty"`
	ListID      string `json:"l,omitempty"`
	ListName    string `json:"b,omitempty"`
	TaskID      string `json:"i,omitempty"`
	Page        int    `json:"g,omitempty"`
	DraftID     string `json:"d,omitempty"`
	Name        string `json:"a,omitempty"`
	Description string `json:"e,omitempty"`
	Status      string `json:"y,omitempty"`
}

func validMode(mode string) bool {
	switch mode {
	case "", "tasks", "detail", "spaces", "lists", "create", "review-create", "edit", "review-edit":
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
	if err := r.DecodeState(&st); err != nil || !validMode(st.Mode) || st.Page < 0 || len(st.TeamID) > 100 || len([]rune(st.TeamName)) > maxNameChars || len(st.SpaceID) > 100 || len([]rune(st.SpaceName)) > maxNameChars || len(st.ListID) > 100 || len([]rune(st.ListName)) > maxNameChars || len(st.TaskID) > 100 || len(st.DraftID) > 128 || len([]rune(st.Name)) > maxNameChars || len([]rune(st.Description)) > maxDescriptionChars || len([]rune(st.Status)) > maxStatusChars {
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
	case "team":
		if err := validID(r.Values["id"]); err != nil || strings.TrimSpace(r.Values["name"]) == "" {
			return nil, errors.New("invalid workspace")
		}
		st = pageState{Mode: "tasks", TeamID: r.Values["id"], TeamName: clipText(r.Values["name"], maxNameChars)}
	case "teams":
		st = pageState{}
	case "spaces":
		st = pageState{Mode: "spaces", TeamID: st.TeamID, TeamName: st.TeamName}
	case "lists":
		if err := validID(r.Values["id"]); err != nil || strings.TrimSpace(r.Values["name"]) == "" {
			return nil, errors.New("invalid space")
		}
		st = pageState{Mode: "lists", TeamID: st.TeamID, TeamName: st.TeamName, SpaceID: r.Values["id"], SpaceName: clipText(r.Values["name"], maxNameChars)}
	case "create":
		if st.Mode != "review-create" {
			if err := validID(r.Values["id"]); err != nil || strings.TrimSpace(r.Values["name"]) == "" {
				return nil, errors.New("invalid list")
			}
			st = pageState{Mode: "create", TeamID: st.TeamID, TeamName: st.TeamName, SpaceID: st.SpaceID, SpaceName: st.SpaceName, ListID: r.Values["id"], ListName: clipText(r.Values["name"], maxNameChars), DraftID: randomID()}
		} else {
			st.Mode = "create"
		}
	case "more":
		st.Page++
	case "detail":
		if err := validID(r.Values["id"]); err != nil {
			return nil, err
		}
		st.Mode, st.TaskID = "detail", r.Values["id"]
	case "edit":
		st.Mode = "edit"
		if st.DraftID == "" {
			st.DraftID = randomID()
		}
	case "back":
		if st.TeamID != "" {
			st.Mode = "tasks"
		} else {
			st.Mode = ""
		}
		st.TaskID, st.DraftID = "", ""
		st.Name, st.Description, st.Status = "", "", ""
	case "review-create":
		in := createArgs{ListID: st.ListID, Name: r.Values["name"], Description: r.Values["description"]}
		if err := in.validate(); err != nil {
			return nil, err
		}
		st.Name, st.Description, st.Mode = in.Name, in.Description, "review-create"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("task draft exceeds page state limit")
		}
	case "review-edit":
		in := statusArgs{TaskID: st.TaskID, Status: r.Values["status"]}
		if err := in.validate(); err != nil {
			return nil, err
		}
		st.Status, st.Mode = in.Status, "review-edit"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("status draft exceeds page state limit")
		}
	case "confirm-create":
		if strings.TrimSpace(st.DraftID) == "" {
			return nil, errors.New("missing draft ID")
		}
		raw, _ := json.Marshal(createArgs{ListID: st.ListID, Name: st.Name, Description: st.Description})
		out, err := a.doAction(ctx, "ui-create:"+st.DraftID, "clickup_create_task", raw)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Toast: out["text"].(string), Pop: true}, nil
	case "confirm-edit":
		if strings.TrimSpace(st.DraftID) == "" {
			return nil, errors.New("missing draft ID")
		}
		raw, _ := json.Marshal(statusArgs{TaskID: st.TaskID, Status: st.Status})
		out, err := a.doAction(ctx, "ui-edit:"+st.DraftID, "clickup_update_status", raw)
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
	taskBatch    = 100 // tasks per ClickUp page
	maxViewTasks = 250 // keeps the accumulated list within the SDK's component limit
)

// collectTasks reads pages 0 through page so that Load more extends the list instead of replacing it. It reads
// whole pages only, so it stops before a page that would push the list past the cap, and it reports whether
// ClickUp has more tasks that the cap still leaves room to show.
func (a *app) collectTasks(ctx context.Context, teamID string, userID int64, page int) ([]task, bool, error) {
	var out []task
	more := false
	for p := 0; p <= page && len(out)+taskBatch <= maxViewTasks; p++ {
		batch, err := a.client.tasks(ctx, teamID, userID, p)
		if err != nil {
			return nil, false, err
		}
		out = append(out, batch.Tasks...)
		more = !batch.Last && len(batch.Tasks) == taskBatch
		if !more {
			break
		}
	}
	return out, more && len(out)+taskBatch <= maxViewTasks, nil
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if a.client.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "create" || st.Mode == "review-create" || st.Mode == "edit" || st.Mode == "review-edit" {
		return a.editorView(r, st)
	}
	if st.Mode == "detail" {
		item, err := a.client.task(ctx, st.TaskID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		st.DraftID = randomID()
		return detailView(r, st, item), nil
	}
	if st.Mode == "spaces" {
		items, err := a.client.spaces(ctx, st.TeamID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{Title: t.spaces}
		seen := map[string]bool{}
		for _, item := range items {
			if item.ID == "" || item.Name == "" || seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			if len(section.Items) >= maxViewTasks {
				break
			}
			row := wire.Component{Type: wire.ComponentRow, Key: boundedKey("space-", item.ID), Title: clipText(item.Name, 1800), Subtitle: clipText(item.ID, 500)}
			if validID(item.ID) == nil {
				row.Action = &wire.Action{ID: "lists", Values: map[string]string{"id": item.ID, "name": clipText(item.Name, maxNameChars)}}
			}
			section.Items = append(section.Items, row)
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.spaces})
		}
		return &wire.View{Title: t.spaces, State: ext.EncodeState(st), Sections: []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "teams"}}}}}}, nil
	}
	if st.Mode == "lists" {
		items, err := a.client.lists(ctx, st.SpaceID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{Title: t.lists}
		seen := map[string]bool{}
		for _, item := range items {
			if item.ID == "" || item.Name == "" || item.Archived || seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			if len(section.Items) >= maxViewTasks {
				break
			}
			row := wire.Component{Type: wire.ComponentRow, Key: boundedKey("list-", item.ID), Title: clipText(item.Name, 1800), Subtitle: clipText(item.ID, 500)}
			if validID(item.ID) == nil {
				row.Action = &wire.Action{ID: "create", Values: map[string]string{"id": item.ID, "name": clipText(item.Name, maxNameChars)}}
			}
			section.Items = append(section.Items, row)
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.lists})
		}
		return &wire.View{Title: t.lists, State: ext.EncodeState(st), Sections: []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "spaces"}}}}}}, nil
	}
	if st.Mode == "tasks" {
		me, err := a.client.user(ctx)
		if err != nil {
			a.record(err)
			return nil, err
		}
		tasks, more, err := a.collectTasks(ctx, st.TeamID, me.ID, st.Page)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{Title: t.tasks}
		seen := map[string]bool{}
		for _, item := range tasks {
			if item.ID == "" || item.Name == "" || seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			if len(section.Items) >= maxViewTasks {
				break
			}
			row := wire.Component{Type: wire.ComponentRow, Key: boundedKey("task-", item.ID), Title: clipText(item.Name, 1800), Subtitle: clipText(item.Status.Status+" · "+item.List.Name+" · "+item.DueDate, 1800)}
			if validID(item.ID) == nil {
				row.Action = &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID}}
			}
			section.Items = append(section.Items, row)
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noTasks})
		}
		if more {
			section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
		}
		v := &wire.View{Title: clipText(st.TeamName, 1800), State: ext.EncodeState(st), Sections: []wire.Section{section, {Title: t.tasks, Items: []wire.Component{
			{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "spaces", Push: &wire.Push{Title: t.spaces, State: ext.EncodeState(pageState{Mode: "spaces", TeamID: st.TeamID, TeamName: st.TeamName})}}},
			{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "teams"}},
		}}}}
		return v, nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid task view")
	}
	teams, err := a.client.teams(ctx)
	a.record(err)
	if err != nil {
		return nil, err
	}
	section := wire.Section{Title: t.teams}
	seen := map[string]bool{}
	for _, item := range teams {
		if item.ID == "" || item.Name == "" || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		if len(section.Items) >= maxViewTasks {
			break
		}
		row := wire.Component{Type: wire.ComponentRow, Key: boundedKey("team-", item.ID), Title: clipText(item.Name, 1800), Subtitle: clipText(item.ID, 500)}
		if validID(item.ID) == nil {
			row.Action = &wire.Action{ID: "team", Values: map[string]string{"id": item.ID, "name": clipText(item.Name, maxNameChars)}}
		}
		section.Items = append(section.Items, row)
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.teams})
	}
	return &wire.View{Title: t.tasks, State: ext.EncodeState(st), Sections: []wire.Section{section}}, nil
}

func (a *app) editorView(r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if st.Mode == "review-create" || st.Mode == "review-edit" {
		action, back := "confirm-create", "create"
		headline := st.Name
		if st.Mode == "review-edit" {
			if err := (statusArgs{TaskID: st.TaskID, Status: st.Status}).validate(); err != nil {
				return nil, err
			}
			action, back = "confirm-edit", "edit"
			headline = st.Status
		} else {
			if err := (createArgs{ListID: st.ListID, Name: st.Name, Description: st.Description}).validate(); err != nil {
				return nil, err
			}
		}
		items := []wire.Component{{Type: wire.ComponentText, Text: clipText(headline, 1800)}}
		if st.Mode == "review-create" && st.Description != "" {
			items = append(items, textComponents(st.Description)...)
		}
		items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: action, Confirm: clipText(t.review+"\n"+headline, wire.StringMost)}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: back}})
		return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.review, Items: items}}}, nil
	}
	var items []wire.Component
	action := "review-create"
	if st.Mode == "edit" {
		action = "review-edit"
		items = []wire.Component{{Type: wire.ComponentField, Kind: "text", Name: "status", Label: t.status, Value: clipText(st.Status, wire.StringMost)}}
	} else {
		items = []wire.Component{{Type: wire.ComponentField, Kind: "text", Name: "name", Label: t.name, Value: clipText(st.Name, wire.StringMost)}, {Type: wire.ComponentField, Kind: "text", Name: "description", Label: t.description, Value: clipText(st.Description, wire.StringMost)}}
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: action, Submit: true}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Items: items}}}, nil
}

func detailView(r *ext.UIRequest, st pageState, item task) *wire.View {
	t := textsFor(r.Locale)
	items := []wire.Component{{Type: wire.ComponentText, Text: clipText(t.status+": "+item.Status.Status+" · "+t.list+": "+item.List.Name, 1800)}}
	if link := safeTaskLink(item.URL); link != "" {
		items = append(items, wire.Component{Type: wire.ComponentLink, Title: t.open, URL: link})
	}
	items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.update, Action: &wire.Action{ID: "edit", Push: &wire.Push{Title: t.update, State: ext.EncodeState(pageState{Mode: "edit", TeamID: st.TeamID, TeamName: st.TeamName, TaskID: item.ID, Status: clipText(item.Status.Status, maxStatusChars), DraftID: randomID()})}}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}})
	title := clipText(item.Name, 1800)
	if title == "" {
		title = clipText(item.ID, 1800)
	}
	v := &wire.View{Title: title, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.tasks, Items: items}}}
	if item.Description != "" {
		v.Sections = append(v.Sections, wire.Section{Title: t.description, Items: textComponents(clipText(item.Description, 60000))})
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
