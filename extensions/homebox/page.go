package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Mode         string  `json:"m,omitempty"`
	Query        string  `json:"q,omitempty"`
	Page         int     `json:"p,omitempty"`
	EntityID     string  `json:"i,omitempty"`
	EntityTypeID string  `json:"y,omitempty"`
	LocationID   string  `json:"l,omitempty"`
	DraftID      string  `json:"d,omitempty"`
	Name         string  `json:"n,omitempty"`
	Description  string  `json:"b,omitempty"`
	Quantity     float64 `json:"u,omitempty"`
}

func validMode(mode string) bool {
	switch mode {
	case "", "detail", "create", "review-create":
		return true
	default:
		return false
	}
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || !validMode(st.Mode) || st.Page < 0 || len([]rune(st.Query)) > maxQueryChars || len(st.EntityID) > 128 || len(st.EntityTypeID) > 128 || len(st.LocationID) > 128 || len(st.DraftID) > 128 || len([]rune(st.Name)) > maxNameChars || len([]rune(st.Description)) > maxDescriptionChars || math.IsNaN(st.Quantity) || math.IsInf(st.Quantity, 0) || st.Quantity < 0 || st.Quantity > 1000000 {
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
		st.Query, st.Page = strings.TrimSpace(clipText(r.Values["q"], maxQueryChars)), 1
	case "more":
		st.Page, err = strconv.Atoi(r.Values["page"])
		if err != nil || st.Page < 2 {
			return nil, errors.New("invalid next page")
		}
	case "detail":
		if !validEntityID(r.Values["id"]) {
			return nil, errors.New("invalid entity ID")
		}
		st.Mode, st.EntityID = "detail", r.Values["id"]
	case "back":
		st.Mode, st.EntityID, st.DraftID = "", "", ""
	case "create":
		if st.Mode == "review-create" {
			st.Mode = "create"
		} else {
			if !validEntityID(r.Values["entity_type_id"]) || !validEntityID(r.Values["location_id"]) {
				return nil, errors.New("valid entity type and location UUIDs are required")
			}
			st = pageState{Mode: "create", EntityTypeID: r.Values["entity_type_id"], LocationID: r.Values["location_id"], DraftID: randomID()}
		}
	case "review-create":
		quantity, err := strconv.ParseFloat(r.Values["quantity"], 64)
		in := createArgs{EntityTypeID: st.EntityTypeID, LocationID: st.LocationID, Name: strings.TrimSpace(r.Values["name"]), Description: strings.TrimSpace(r.Values["description"]), Quantity: quantity}
		if err != nil || in.validate() != nil {
			return nil, errors.New("a valid item name, description, quantity, type and location are required")
		}
		st.Name, st.Description, st.Quantity, st.Mode = in.Name, in.Description, in.Quantity, "review-create"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("item draft exceeds page state limit")
		}
	case "confirm-create":
		in := createArgs{EntityTypeID: st.EntityTypeID, LocationID: st.LocationID, Name: st.Name, Description: st.Description, Quantity: st.Quantity}
		if err := in.validate(); err != nil || st.Mode != "review-create" || st.DraftID == "" {
			return nil, errors.New("a valid reviewed item draft is required")
		}
		raw, _ := json.Marshal(in)
		out, err := a.doAction(ctx, "ui-create:"+st.DraftID, "homebox_create_item", raw)
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

// collectEntities reads page 1 through the current page so that Load more answers the whole list, longer. The page it
// returns holds every entity read and the paging fields of the last page. It stops at maxViewRecords to keep the view
// within the SDK's component limit, and reports whether another page can be loaded.
func (a *app) collectEntities(ctx context.Context, query string, current int) (entityPage, bool, error) {
	var out entityPage
	for pageNumber := 1; pageNumber <= current && len(out.Items) < maxViewRecords; pageNumber++ {
		batch, err := a.client.entities(ctx, query, pageNumber)
		if err != nil {
			return entityPage{}, false, err
		}
		out.Items = append(out.Items, batch.Items...)
		out.Page, out.PageSize, out.Total = batch.Page, batch.PageSize, batch.Total
		if batch.Page*batch.PageSize >= batch.Total {
			break
		}
	}
	if len(out.Items) > maxViewRecords {
		out.Items = out.Items[:maxViewRecords]
	}
	return out, out.Page*out.PageSize < out.Total && len(out.Items) < maxViewRecords, nil
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if a.client.base == "" || a.client.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "create" || st.Mode == "review-create" {
		return createView(r, st)
	}
	if st.Mode == "detail" {
		item, err := a.client.entity(ctx, st.EntityID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item), nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid inventory view")
	}
	if st.Page == 0 {
		st.Page = 1
	}
	page, more, err := a.collectEntities(ctx, st.Query, st.Page)
	a.record(err)
	if err != nil {
		return nil, err
	}
	v := &wire.View{Title: t.inventory, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	section := wire.Section{Title: fmt.Sprintf(t.results, page.Total)}
	seen := map[string]bool{}
	for _, item := range page.Items {
		if !validEntityID(item.ID) || strings.TrimSpace(item.Name) == "" || item.Archived || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		if len(section.Items) >= maxViewRecords {
			break
		}
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "entity-" + item.ID, Title: clipText(item.Name, 1800), Subtitle: clipText(fmt.Sprintf("%g · %s · %s", item.Quantity, item.AssetID, locationName(item)), 1800), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID}}})
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noItems})
	}
	if more {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more", Values: map[string]string{"page": fmt.Sprint(page.Page + 1)}}}
	}
	create, err := a.createChoices(ctx, r.Locale)
	if err != nil {
		return nil, err
	}
	v.Sections = []wire.Section{section, create}
	return v, nil
}

func (a *app) createChoices(ctx context.Context, locale string) (wire.Section, error) {
	t := textsFor(locale)
	types, err := a.client.entityTypes(ctx)
	if err != nil {
		return wire.Section{}, err
	}
	var typeChoice *entityType
	for _, item := range types {
		if !item.IsLocation && validEntityID(item.ID) {
			typeChoice = &item
			break
		}
	}
	locations, err := a.client.locations(ctx)
	if err != nil {
		return wire.Section{}, err
	}
	flat := flattenLocations(locations, 0)
	if typeChoice == nil || len(flat) == 0 {
		return wire.Section{Title: t.create, Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.noCreateChoices}}}, nil
	}
	section := wire.Section{Title: t.create}
	seen := map[string]bool{}
	subtitle := clipText(typeChoice.Name, 500)
	for _, location := range flat {
		if !validEntityID(location.ID) || strings.TrimSpace(location.Name) == "" || seen[location.ID] {
			continue
		}
		seen[location.ID] = true
		if len(section.Items) >= 20 {
			break
		}
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "create-" + location.ID, Title: clipText(location.Name, 1800), Subtitle: subtitle, Action: &wire.Action{ID: "create", Values: map[string]string{"entity_type_id": typeChoice.ID, "location_id": location.ID}, Push: &wire.Push{Title: t.create, State: ext.EncodeState(pageState{Mode: "create", EntityTypeID: typeChoice.ID, LocationID: location.ID, DraftID: randomID()})}}})
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noCreateChoices})
	}
	return section, nil
}

func createView(r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if st.Mode == "review-create" {
		in := createArgs{EntityTypeID: st.EntityTypeID, LocationID: st.LocationID, Name: st.Name, Description: st.Description, Quantity: st.Quantity}
		if err := in.validate(); err != nil {
			return nil, err
		}
		items := []wire.Component{{Type: wire.ComponentText, Text: clipText(st.Name, 1800)}, {Type: wire.ComponentText, Text: fmt.Sprintf("%g", st.Quantity)}}
		if st.Description != "" {
			items = append(items, textComponents(st.Description)...)
		}
		items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "confirm-create", Confirm: clipText(t.create+"\n"+st.Name, wire.StringMost)}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "create"}})
		return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.create, Items: items}}}, nil
	}
	quantity := ""
	if st.Quantity > 0 {
		quantity = fmt.Sprintf("%g", st.Quantity)
	}
	return &wire.View{Title: t.create, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.create, Items: []wire.Component{
		{Type: wire.ComponentField, Kind: "text", Name: "name", Label: t.name, Value: clipText(st.Name, wire.StringMost)},
		{Type: wire.ComponentField, Kind: "text", Name: "description", Label: t.description, Value: clipText(st.Description, wire.StringMost)},
		{Type: wire.ComponentField, Kind: "text", Name: "quantity", Label: t.quantity, Value: quantity},
		{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "review-create", Submit: true}},
		{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	}}}}, nil
}

const (
	maxViewItems        = 250
	maxDescriptionItems = 120
)

func capComponents(items []wire.Component, n int) []wire.Component {
	if len(items) <= n {
		return items
	}
	items = items[:n]
	items[n-1].Text = clipText(items[n-1].Text, 1700) + " […]"
	return items
}

func detailView(r *ext.UIRequest, st pageState, item entity) *wire.View {
	t := textsFor(r.Locale)
	items := []wire.Component{{Type: wire.ComponentText, Text: clipText(fmt.Sprintf("%g · %s · %s", item.Quantity, item.AssetID, locationName(item)), 1800)}, {Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}}}
	for _, field := range []struct{ label, value string }{
		{t.serial, item.SerialNumber},
		{t.model, item.ModelNumber},
		{t.manufacturer, item.Manufacturer},
		{t.warranty, item.WarrantyDetails + " " + item.WarrantyExpires},
	} {
		if strings.TrimSpace(field.value) != "" {
			items = append(items, wire.Component{Type: wire.ComponentText, Text: clipText(field.label+": "+strings.TrimSpace(field.value), 1800)})
		}
	}
	title := clipText(item.Name, 1800)
	if title == "" {
		title = clipText(item.ID, 1800)
	}
	v := &wire.View{Title: title, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.inventory, Items: items}}}
	used := len(items)
	if item.Description != "" {
		desc := capComponents(textComponents(clipText(item.Description, 12000)), maxDescriptionItems)
		used += len(desc)
		v.Sections = append(v.Sections, wire.Section{Title: t.description, Items: desc})
	}
	if item.Notes != "" {
		notes := capComponents(textComponents(clipText(item.Notes, 12000)), maxViewItems-used)
		v.Sections = append(v.Sections, wire.Section{Title: t.notes, Items: notes})
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
