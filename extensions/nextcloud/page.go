package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

const maxViewRecords = 250

func boundedKey(prefix, raw string) string {
	key := prefix + raw
	if len(key) <= 128 {
		return key
	}
	sum := sha256.Sum256([]byte(raw))
	return prefix + hex.EncodeToString(sum[:])
}

func fileTitle(displayName, relPath string) string {
	if s := strings.TrimSpace(displayName); s != "" {
		return s
	}
	if base := strings.TrimSpace(path.Base(relPath)); base != "" && base != "." && base != "/" {
		return base
	}
	if s := strings.TrimSpace(relPath); s != "" {
		return s
	}
	return "File"
}

type pageState struct {
	Mode    string `json:"m,omitempty"`
	Query   string `json:"q,omitempty"`
	Path    string `json:"p,omitempty"`
	DraftID string `json:"d,omitempty"`
	Text    string `json:"t,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || len([]rune(st.Query)) > maxQueryChars || len(st.Path) > 550 || len(st.DraftID) > 128 || len(st.Text) > 11000 || len(st.Mode) > 30 {
		return pageState{}, errors.New("invalid page state")
	}
	switch st.Mode {
	case "", "detail", "create", "review-create":
	default:
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
		st.Query, st.Mode = strings.TrimSpace(clipText(r.Values["q"], maxQueryChars)), ""
	case "detail":
		if !validRemotePath(r.Values["path"]) {
			return nil, errors.New("invalid file path")
		}
		st.Mode, st.Path = "detail", r.Values["path"]
	case "back":
		st.Mode, st.Path, st.DraftID, st.Text = "", "", "", ""
	case "create":
		if st.Mode == "review-create" {
			st.Mode = "create"
		} else {
			st = pageState{Mode: "create", DraftID: randomID()}
		}
	case "review-create":
		in := createArgs{Path: strings.TrimSpace(r.Values["path"]), Text: strings.TrimSpace(r.Values["text"])}
		if err := in.validate(); err != nil || len(in.Text) > 10000 {
			return nil, errors.New("the Web note must have a new .md path and at most 10000 UTF-8 bytes")
		}
		st.Path, st.Text, st.Mode = in.Path, in.Text, "review-create"
		if len(ext.EncodeState(st)) > wire.StateMost {
			return nil, errors.New("note draft exceeds page state limit; use the nextcloud_create_note action")
		}
	case "confirm-create":
		in := createArgs{Path: st.Path, Text: st.Text}
		if err := in.validate(); err != nil || st.Mode != "review-create" || st.DraftID == "" || len(st.Text) > 10000 {
			return nil, errors.New("a valid reviewed note draft is required")
		}
		raw, _ := json.Marshal(in)
		out, err := a.doAction(ctx, "ui-create:"+st.DraftID, "nextcloud_create_note", raw)
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
	if a.client.base == "" || a.client.username == "" || a.client.password == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "create" || st.Mode == "review-create" {
		if st.Mode == "review-create" {
			if err := (createArgs{Path: st.Path, Text: st.Text}).validate(); err != nil || len(st.Text) > 10000 {
				return nil, errors.New("a valid reviewed note draft is required")
			}
		}
		return createView(r, st), nil
	}
	if st.Mode == "detail" {
		item, text, err := a.client.file(ctx, a.client.username, st.Path)
		a.record(err)
		if err != nil {
			return nil, err
		}
		return detailView(r, st, item, text), nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid file view")
	}
	v := &wire.View{Title: t.files, State: ext.EncodeState(st), Search: &wire.Search{Placeholder: t.search, Value: st.Query, Action: wire.Action{ID: "search"}}}
	section := wire.Section{Title: t.files}
	if st.Query == "" {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.searchHint})
	} else {
		items, err := a.client.search(ctx, a.client.username, st.Query)
		a.record(err)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		prefix := "/remote.php/dav/files/" + a.client.username + "/"
		for _, item := range items {
			if item.Href == "" || seen[item.Href] {
				continue
			}
			seen[item.Href] = true
			relPath := strings.TrimPrefix(item.Href, prefix)
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: boundedKey("file-", item.Href), Title: clipText(fileTitle(item.DisplayName, relPath), 1800), Subtitle: clipText(fmt.Sprintf("%s · %d bytes · %s", item.ContentType, item.ContentLength, item.LastModified), 1800), Action: &wire.Action{ID: "detail", Values: map[string]string{"path": relPath}}})
			if len(section.Items) >= maxViewRecords {
				break
			}
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noFiles})
		}
	}
	v.Sections = []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "create", Push: &wire.Push{Title: t.create, State: ext.EncodeState(pageState{Mode: "create", DraftID: randomID()})}}}}}}
	return v, nil
}

func createView(r *ext.UIRequest, st pageState) *wire.View {
	t := textsFor(r.Locale)
	if st.Mode == "review-create" {
		items := []wire.Component{{Type: wire.ComponentText, Text: clipText(st.Path, 1800)}, {Type: wire.ComponentText, Text: t.noOverwrite}}
		items = append(items, textComponents(st.Text)...)
		items = append(items, wire.Component{Type: wire.ComponentButton, Title: t.create, Action: &wire.Action{ID: "confirm-create", Confirm: t.create + "\n" + st.Path + "\n" + t.noOverwrite}}, wire.Component{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "create"}})
		return &wire.View{Title: t.review, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.create, Items: items}}}
	}
	return &wire.View{Title: t.create, State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.noOverwrite, Items: []wire.Component{
		{Type: wire.ComponentField, Kind: "text", Name: "path", Label: t.path, Value: clipText(st.Path, wire.StringMost)},
		{Type: wire.ComponentField, Kind: "text", Name: "text", Label: t.text, Value: clipText(st.Text, wire.StringMost)},
		{Type: wire.ComponentButton, Title: t.review, Action: &wire.Action{ID: "review-create", Submit: true}},
		{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	}}}}
}

func detailView(r *ext.UIRequest, st pageState, item fileEntry, text []byte) *wire.View {
	t := textsFor(r.Locale)
	v := &wire.View{Title: clipText(fileTitle(item.DisplayName, st.Path), 1800), State: ext.EncodeState(st), Sections: []wire.Section{{Title: t.files, Items: []wire.Component{
		{Type: wire.ComponentText, Text: clipText(fmt.Sprintf("%s · %d bytes · %s", item.ContentType, item.ContentLength, item.LastModified), 1800)},
		{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}},
	}}}}
	v.Sections = append(v.Sections, wire.Section{Title: t.text, Items: textComponents(clipText(string(text), 40000))})
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
