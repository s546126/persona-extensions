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

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Mode      string `json:"m,omitempty"`
	SpaceID   string `json:"s,omitempty"`
	SpaceName string `json:"n,omitempty"`
	ObjectID  string `json:"i,omitempty"`
	Offset    int    `json:"o,omitempty"`
	Query     string `json:"q,omitempty"`
	DraftID   string `json:"d,omitempty"`
	Name      string `json:"a,omitempty"`
	Body      string `json:"b,omitempty"`
	TypeKey   string `json:"t,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Offset < 0 || len(st.SpaceID) > 256 || len([]rune(st.SpaceName)) > maxNameChars || len(st.ObjectID) > 256 || len(st.DraftID) > 128 || len([]rune(st.Name)) > maxNameChars || len([]rune(st.Body)) > maxBodyChars || len([]rune(st.Query)) > maxQueryChars {
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
		if strings.TrimSpace(r.Values["q"]) == "" {
			st = pageState{}
		} else {
			st = pageState{Mode: "search", Query: clipText(r.Values["q"], maxQueryChars)}
		}
	case "space":
		if err := validObjectID(r.Values["id"]); err != nil {
			return nil, err
		}
		if strings.TrimSpace(r.Values["name"]) == "" || len([]rune(r.Values["name"])) > maxNameChars {
			return nil, errors.New("invalid space name")
		}
		st = pageState{Mode: "objects", SpaceID: r.Values["id"], SpaceName: r.Values["name"]}
	case "spaces":
		st = pageState{}
	case "more":
		st.Offset += 50
	case "detail":
		if err := validObjectID(r.Values["id"]); err != nil {
			return nil, err
		}
		if r.Values["space"] != "" {
			if err := validObjectID(r.Values["space"]); err != nil {
				return nil, err
			}
			st.SpaceID = r.Values["space"]
		}
		st.Mode, st.ObjectID, st.DraftID, st.Name, st.Body = "detail", r.Values["id"], "", "", ""
	case "create":
		if st.Mode != "review-create" {
			st = pageState{Mode: "create", SpaceID: st.SpaceID, SpaceName: st.SpaceName, TypeKey: "page", DraftID: randomID()}
		} else {
			st.Mode = "create"
		}
	case "back":
		if st.SpaceID == "" {
			st = pageState{}
		} else {
			st.Mode, st.ObjectID, st.DraftID, st.Name, st.Body = "objects", "", "", "", ""
		}
	case "review-create":
		in := createArgs{SpaceID: st.SpaceID, TypeKey: r.Values["type_key"], Name: r.Values["name"], Body: r.Values["body"]}
		if err := in.validate(); err != nil {
			return nil, err
		}
		st.TypeKey, st.Name, st.Body, st.Mode = in.TypeKey, in.Name, in.Body, "review-create"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("object draft exceeds page state limit")
		}
	case "confirm-create":
		if strings.TrimSpace(st.DraftID) == "" {
			return nil, errors.New("draft ID is required")
		}
		raw, _ := json.Marshal(createArgs{SpaceID: st.SpaceID, TypeKey: st.TypeKey, Name: st.Name, Body: st.Body})
		out, err := a.doAction(ctx, "ui-create:"+st.DraftID, "anytype_create_object", raw)
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
	c, _, _ := a.deps()
	if c.base == "" || c.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "create" || st.Mode == "review-create" {
		return a.createView(ctx, r, st)
	}
	if st.Mode == "search" {
		if strings.TrimSpace(st.Query) == "" {
			st = pageState{}
		} else {
			return a.searchView(ctx, r, st)
		}
	}
	if st.Mode == "detail" {
		item, err := c.object(ctx, st.SpaceID, st.ObjectID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item), nil
	}
	if st.Mode == "objects" {
		if err := validObjectID(st.SpaceID); err != nil {
			return nil, err
		}
		objects, more, err := a.collectObjects(ctx, st.SpaceID, st.Offset)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{}
		seen := make(map[string]bool, len(objects))
		for _, item := range objects {
			if validObjectID(item.ID) != nil || strings.TrimSpace(item.Name) == "" {
				continue
			}
			key := rowKey("object", "object-"+item.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: key, Title: clipText(item.Name, 1800), Subtitle: clipText(item.Type.Name+" · "+item.Snippet, 1800), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID, "space": st.SpaceID}}})
		}
		section.Title = fmt.Sprintf(t.objects, len(section.Items))
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noObjects})
		}
		if more {
			section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
		}
		v := &wire.View{Title: clipText(st.SpaceName, 1800), State: ext.EncodeState(st), Sections: []wire.Section{section, {Title: t.objects, Items: []wire.Component{
			{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "create", Push: &wire.Push{Title: t.create, State: ext.EncodeState(pageState{Mode: "create", SpaceID: st.SpaceID, SpaceName: st.SpaceName, TypeKey: "page", DraftID: randomID()})}}},
			{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "spaces"}},
		}}}}
		return v, nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid knowledge view")
	}
	spaces, more, err := a.collectSpaces(ctx, st.Offset)
	a.record(err)
	if err != nil {
		return nil, err
	}
	section := wire.Section{Title: t.spaces}
	seen := make(map[string]bool, len(spaces))
	for _, item := range spaces {
		if validObjectID(item.ID) != nil || strings.TrimSpace(item.Name) == "" {
			continue
		}
		key := rowKey("space", "space-"+item.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: key, Title: clipText(item.Name, 1800), Subtitle: clipText(item.ID, 1800), Action: &wire.Action{ID: "space", Values: map[string]string{"id": item.ID, "name": clipText(item.Name, maxNameChars)}}})
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noSpaces})
	}
	if more {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
	}
	return &wire.View{Title: t.knowledge, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}, Sections: []wire.Section{section}}, nil
}

// maxViewObjects keeps the accumulated list within the SDK's component limit.
const maxViewObjects = 250

// collectObjects reads batches from offset 0 through offset so that Load more extends the list instead of
// replacing it, and reports whether Anytype has more objects that the cap still leaves room to show.
func (a *app) collectObjects(ctx context.Context, spaceID string, offset int) ([]knowledgeObject, bool, error) {
	c, _, _ := a.deps()
	var out []knowledgeObject
	seen := map[string]bool{}
	more := false
	for pos := 0; pos <= offset && len(out) < maxViewObjects; pos += 50 {
		batch, err := c.objects(ctx, spaceID, pos)
		if err != nil {
			return nil, false, err
		}
		if len(batch.Data) == 0 {
			more = false
			break
		}
		for _, item := range batch.Data {
			if validObjectID(item.ID) != nil || strings.TrimSpace(item.Name) == "" {
				continue
			}
			key := rowKey("object", "object-"+item.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, item)
			if len(out) >= maxViewObjects {
				break
			}
		}
		more = batch.Pagination.HasMore
		if !more {
			break
		}
	}
	return out, more && len(out) < maxViewObjects, nil
}

// collectSpaces reads batches from offset 0 through offset so that Load more extends the spaces list instead of
// replacing it, and reports whether Anytype has more spaces that the cap still leaves room to show.
func (a *app) collectSpaces(ctx context.Context, offset int) ([]space, bool, error) {
	c, _, _ := a.deps()
	var out []space
	seen := map[string]bool{}
	more := false
	for pos := 0; pos <= offset && len(out) < maxViewObjects; pos += 50 {
		batch, err := c.spaces(ctx, pos)
		if err != nil {
			return nil, false, err
		}
		if len(batch.Data) == 0 {
			more = false
			break
		}
		for _, item := range batch.Data {
			if validObjectID(item.ID) != nil || strings.TrimSpace(item.Name) == "" {
				continue
			}
			key := rowKey("space", "space-"+item.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, item)
			if len(out) >= maxViewObjects {
				break
			}
		}
		more = batch.Pagination.HasMore
		if !more {
			break
		}
	}
	return out, more && len(out) < maxViewObjects, nil
}

// collectSearch reads batches from offset 0 through offset so that Load more extends the results instead of
// replacing them, and reports whether Anytype has more matches that the cap still leaves room to show.
func (a *app) collectSearch(ctx context.Context, query string, offset int) ([]knowledgeObject, bool, error) {
	c, _, _ := a.deps()
	var out []knowledgeObject
	seen := map[string]bool{}
	more := false
	for pos := 0; pos <= offset && len(out) < maxViewObjects; pos += 50 {
		batch, err := c.search(ctx, query, pos)
		if err != nil {
			return nil, false, err
		}
		if len(batch.Data) == 0 {
			more = false
			break
		}
		for _, item := range batch.Data {
			if validObjectID(item.ID) != nil || validObjectID(item.SpaceID) != nil || strings.TrimSpace(item.Name) == "" {
				continue
			}
			key := rowKey("object", "object-"+item.SpaceID+"-"+item.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, item)
			if len(out) >= maxViewObjects {
				break
			}
		}
		more = batch.Pagination.HasMore
		if !more {
			break
		}
	}
	return out, more && len(out) < maxViewObjects, nil
}

func (a *app) searchView(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	results, more, err := a.collectSearch(ctx, st.Query, st.Offset)
	a.record(err)
	if err != nil {
		return nil, err
	}
	section := wire.Section{}
	seen := make(map[string]bool, len(results))
	for _, item := range results {
		if validObjectID(item.ID) != nil || validObjectID(item.SpaceID) != nil || strings.TrimSpace(item.Name) == "" {
			continue
		}
		key := rowKey("object", "object-"+item.SpaceID+"-"+item.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: key, Title: clipText(item.Name, 1800), Subtitle: clipText(item.Type.Name+" · "+item.Snippet, 1800), Action: &wire.Action{ID: "detail", Values: map[string]string{"id": item.ID, "space": item.SpaceID}}})
	}
	section.Title = fmt.Sprintf(t.objects, len(section.Items))
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noObjects})
	}
	if more {
		section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more"}}
	}
	return &wire.View{Title: t.search, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}, Sections: []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "spaces"}}}}}}, nil
}

func (a *app) createView(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	if st.Mode == "review-create" {
		in := createArgs{SpaceID: st.SpaceID, TypeKey: st.TypeKey, Name: st.Name, Body: st.Body}
		if err := in.validate(); err != nil {
			return nil, err
		}
		items := []wire.Component{{Type: wire.ComponentText, Text: clipText(st.Name, 1800)}}
		items = append(items, textComponents(st.Body)...)
		items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "confirm-create", Confirm: t.create + "\n" + clipText(st.Name, 1800)}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "create"}})
		return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.create, Items: items}}}, nil
	}
	if err := validObjectID(st.SpaceID); err != nil {
		return nil, err
	}
	return &wire.View{Title: t.create, State: ext.EncodeState(st), Sections: []wire.Section{{Title: clipText(st.SpaceName, 1800), Items: []wire.Component{
		{Type: wire.ComponentField, Kind: "text", Name: "name", Label: t.name, Value: clipText(st.Name, wire.StringMost)},
		{Type: wire.ComponentField, Kind: "text", Name: "body", Label: t.body, Value: clipText(st.Body, wire.StringMost)},
		{Type: wire.ComponentField, Kind: "choice", Name: "type_key", Label: t.typeLabel, Value: selectedType(st.TypeKey), Choices: []wire.Choice{{Value: "page", Title: t.pageType}, {Value: "note", Title: t.noteType}}},
		{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "review-create", Submit: true}},
		{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	}}}}, nil
}

func detailView(r *ext.UIRequest, st pageState, item knowledgeObject) *wire.View {
	t := textsFor(r.Locale)
	items := []wire.Component{{Type: wire.ComponentText, Text: clipText(item.Type.Name+" · "+item.Snippet, 1800)}, {Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}}}
	v := &wire.View{Title: clipText(item.Name, 1800), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.objects, Items: items}}}
	if text := objectText(item); text != "" {
		v.Sections = append(v.Sections, wire.Section{Title: t.body, Items: textComponents(clipText(text, 60000))})
	}
	return v
}

func selectedType(value string) string {
	if value == "note" {
		return "note"
	}
	return "page"
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

func rowKey(prefix, raw string) string {
	if len(raw) <= 128 && len([]rune(raw)) <= 128 {
		return raw
	}
	sum := sha256.Sum256([]byte(raw))
	return prefix + "-" + hex.EncodeToString(sum[:16])
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
