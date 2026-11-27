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
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Mode        string `json:"m,omitempty"`
	WorkID      int64  `json:"i,omitempty"`
	Offset      int    `json:"o,omitempty"`
	ProjectHref string `json:"p,omitempty"`
	ProjectName string `json:"n,omitempty"`
	TypeHref    string `json:"t,omitempty"`
	TypeName    string `json:"y,omitempty"`
	DraftID     string `json:"d,omitempty"`
	Subject     string `json:"a,omitempty"`
	Description string `json:"b,omitempty"`
	StartDate   string `json:"s,omitempty"`
	DueDate     string `json:"e,omitempty"`
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
	if err := r.DecodeState(&st); err != nil || !validMode(st.Mode) || st.WorkID < 0 || st.Offset < 0 || len(st.ProjectHref) > 100 || utf8.RuneCountInString(st.ProjectName) > maxSubjectChars || len(st.TypeHref) > 100 || utf8.RuneCountInString(st.TypeName) > maxSubjectChars || len(st.DraftID) > 128 || utf8.RuneCountInString(st.Subject) > maxSubjectChars || utf8.RuneCountInString(st.Description) > maxDescriptionChars || len(st.StartDate) > 20 || len(st.DueDate) > 20 {
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
		st.Offset++
	case "detail":
		id, err := strconv.ParseInt(r.Values["id"], 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("invalid work package ID")
		}
		st.Mode, st.WorkID = "detail", id
	case "projects":
		st = pageState{Mode: "projects"}
	case "create":
		if st.Mode != "review-create" {
			projectHref, projectName := st.ProjectHref, st.ProjectName
			if r.Values["project_href"] != "" || r.Values["project_name"] != "" {
				projectHref = r.Values["project_href"]
				projectName = clipText(strings.TrimSpace(r.Values["project_name"]), maxSubjectChars)
			}
			typeHref, typeName := st.TypeHref, st.TypeName
			if r.Values["type_href"] != "" || r.Values["type_name"] != "" {
				typeHref = r.Values["type_href"]
				typeName = clipText(strings.TrimSpace(r.Values["type_name"]), maxSubjectChars)
			}
			if !projectHrefPattern.MatchString(projectHref) || !typeHrefPattern.MatchString(typeHref) {
				return nil, errors.New("project and type must be exact numeric HAL links from OpenProject discovery")
			}
			st = pageState{Mode: "create", ProjectHref: projectHref, ProjectName: projectName, TypeHref: typeHref, TypeName: typeName, DraftID: randomID()}
		} else {
			st.Mode = "create"
		}
	case "back":
		st = pageState{Offset: st.Offset}
	case "review-create":
		in := createArgs{ProjectHref: st.ProjectHref, TypeHref: st.TypeHref, Subject: r.Values["subject"], Description: r.Values["description"], StartDate: r.Values["start_date"], DueDate: r.Values["due_date"]}
		if err := in.validate(); err != nil {
			return nil, err
		}
		st.Subject, st.Description, st.StartDate, st.DueDate, st.Mode = in.Subject, in.Description, in.StartDate, in.DueDate, "review-create"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("work package draft exceeds page state limit")
		}
	case "confirm-create":
		if strings.TrimSpace(st.DraftID) == "" {
			return nil, errors.New("missing draft ID")
		}
		raw, _ := json.Marshal(createArgs{ProjectHref: st.ProjectHref, TypeHref: st.TypeHref, Subject: st.Subject, Description: st.Description, StartDate: st.StartDate, DueDate: st.DueDate})
		out, err := a.doAction(ctx, "ui-create:"+st.DraftID, "openproject_create_work_package", raw)
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
	if a.client.base == "" || a.client.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "create" || st.Mode == "review-create" {
		return a.createView(ctx, r, st)
	}
	if st.Mode == "projects" {
		page, err := a.client.projects(ctx, 1)
		a.record(err)
		if err != nil {
			return nil, err
		}
		types, err := a.client.types(ctx, 1)
		if err != nil {
			return nil, err
		}
		section := wire.Section{Title: t.projects}
		typeChoice := defaultType(types.Embedded.Elements)
		hasValidType := typeHrefPattern.MatchString(typeChoice.Links.Self.Href)
		seen := map[int64]bool{}
		for _, item := range page.Embedded.Elements {
			if !item.Active || item.ID <= 0 || item.Name == "" || seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			if len(section.Items) >= maxViewWorkPackages {
				break
			}
			row := wire.Component{Type: wire.ComponentRow, Key: "project-" + fmt.Sprint(item.ID), Title: clipText(item.Name, 1800), Subtitle: clipText(typeChoice.Name, 500)}
			if hasValidType && projectHrefPattern.MatchString(item.Links.Self.Href) {
				row.Action = &wire.Action{ID: "create", Values: map[string]string{"project_href": item.Links.Self.Href, "project_name": clipText(item.Name, maxSubjectChars), "type_href": typeChoice.Links.Self.Href, "type_name": clipText(typeChoice.Name, maxSubjectChars)}}
			}
			section.Items = append(section.Items, row)
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noProjects})
		}
		return &wire.View{Title: t.projects, State: ext.EncodeState(st), Sections: []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}}}}}}, nil
	}
	if st.Mode == "detail" {
		item, err := a.client.workPackage(ctx, st.WorkID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item), nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid work package view")
	}
	me, err := a.client.currentUser(ctx)
	if err != nil {
		a.record(err)
		return nil, err
	}
	st.Offset = max(1, st.Offset)
	items, more, err := a.collectWorkPackages(ctx, me.ID, st.Offset)
	a.record(err)
	if err != nil {
		return nil, err
	}
	section := wire.Section{Title: t.work}
	seen := map[int64]bool{}
	for _, item := range items {
		if item.ID <= 0 || item.Subject == "" || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		if len(section.Items) >= maxViewWorkPackages {
			break
		}
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "work-" + fmt.Sprint(item.ID), Title: clipText(item.Subject, 1800), Subtitle: clipText(item.Links.Project.Title+" · "+item.Links.Status.Title+" · "+item.DueDate, 1800), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": fmt.Sprint(item.ID)}}})
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noWork})
	}
	if more {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
	}
	v := &wire.View{Title: t.work, State: ext.EncodeState(st), Sections: []wire.Section{section, {Title: t.work, Items: []wire.Component{
		{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "projects", Push: &wire.Push{Title: t.projects, State: ext.EncodeState(pageState{Mode: "projects"})}}},
	}}}}
	return v, nil
}

// maxViewWorkPackages keeps the accumulated list within the SDK's component limit.
const maxViewWorkPackages = 250

// collectWorkPackages reads pages 1 through page so that Load more extends the list instead of replacing it,
// and reports whether OpenProject has more work packages that the cap still leaves room to show.
func (a *app) collectWorkPackages(ctx context.Context, userID int64, page int) ([]workPackage, bool, error) {
	var out []workPackage
	more := false
	for pos := 1; pos <= page && len(out) < maxViewWorkPackages; pos++ {
		batch, err := a.client.workPackages(ctx, userID, pos)
		if err != nil {
			return nil, false, err
		}
		out = append(out, batch.Embedded.Elements...)
		more = batch.Links.NextByOffset.Href != ""
		if !more {
			break
		}
	}
	if len(out) > maxViewWorkPackages {
		out = out[:maxViewWorkPackages]
	}
	return out, more && len(out) < maxViewWorkPackages, nil
}

func (a *app) createView(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if st.Mode == "review-create" {
		in := createArgs{ProjectHref: st.ProjectHref, TypeHref: st.TypeHref, Subject: st.Subject, Description: st.Description, StartDate: st.StartDate, DueDate: st.DueDate}
		if err := in.validate(); err != nil {
			return nil, err
		}
		items := []wire.Component{{Type: wire.ComponentText, Text: clipText(st.Subject, 1800)}}
		if st.Description != "" {
			items = append(items, textComponents(st.Description)...)
		}
		items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(t.project+": "+st.ProjectName+" · "+t.typeLabel+": "+st.TypeName, 1800)}, wire.Component{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "confirm-create", Confirm: clipText(t.create+"\n"+st.Subject, wire.StringMost)}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "create"}})
		return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.create, Items: items}}}, nil
	}
	return &wire.View{Title: t.create, State: ext.EncodeState(st), Sections: []wire.Section{{Title: clipText(st.ProjectName+" · "+st.TypeName, 1800), Items: []wire.Component{
		{Type: wire.ComponentField, Kind: "text", Name: "subject", Label: t.subject, Value: clipText(st.Subject, wire.StringMost)},
		{Type: wire.ComponentField, Kind: "text", Name: "description", Label: t.description, Value: clipText(st.Description, wire.StringMost)},
		{Type: wire.ComponentField, Kind: "text", Name: "start_date", Label: t.start, Value: clipText(st.StartDate, wire.StringMost)},
		{Type: wire.ComponentField, Kind: "text", Name: "due_date", Label: t.due, Value: clipText(st.DueDate, wire.StringMost)},
		{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "review-create", Submit: true}},
		{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	}}}}, nil
}

func detailView(r *ext.UIRequest, st pageState, item workPackage) *wire.View {
	t := textsFor(r.Locale)
	items := []wire.Component{
		{Type: wire.ComponentText, Text: clipText(t.project+": "+item.Links.Project.Title+" · "+item.Links.Type.Title+" · "+item.Links.Status.Title, 1800)},
		{Type: wire.ComponentText, Text: clipText(t.start+": "+item.StartDate+" · "+t.due+": "+item.DueDate, 1800)},
		{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	}
	title := clipText(item.Subject, 1800)
	if title == "" {
		title = fmt.Sprintf("#%d", item.ID)
	}
	v := &wire.View{Title: title, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.work, Items: items}}}
	if item.Description.Raw != "" {
		v.Sections = append(v.Sections, wire.Section{Title: t.description, Items: textComponents(clipText(item.Description.Raw, 60000))})
	}
	return v
}

func defaultType(types []workType) workType {
	for _, item := range types {
		if item.IsDefault && !item.IsMilestone {
			return item
		}
	}
	for _, item := range types {
		if !item.IsMilestone {
			return item
		}
	}
	return workType{}
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
