// Command space-conversations is the optional domain-space product surface.
// Core retains track execution and privacy policy; this program draws pages.
package main

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/persona-extensions/pluginkit"
)

var manifest = pluginkit.PackagedManifest(authorManifest)

var about = func() (out struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}) {
	_ = json.Unmarshal(manifest, &out)
	return out
}()

//go:embed persona-extension.json
var authorManifest json.RawMessage

func main() { ext.Main(program()) }

type App struct {
	mu     sync.RWMutex
	client *Client
}

func program() ext.Program {
	a := &App{}
	return ext.Program{
		ID: about.ID, Version: about.Version, Manifest: manifest,
		Init: func(_ context.Context, in *ext.Init, _ *ext.Hub) (map[string]any, error) {
			a.mu.Lock()
			a.client = NewClient(in.Hub.URL, in.Hub.Token)
			a.mu.Unlock()
			return nil, nil
		},
		UI: map[string]ext.Page{
			"home":  {Render: a.render, Action: a.action},
			"space": {Render: a.render, Action: a.action},
			"new":   {Render: a.render, Action: a.action},
			"edit":  {Render: a.render, Action: a.action},
		},
	}
}

func (a *App) current() (*Client, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.client == nil {
		return nil, errors.New("空间对话：扩展尚未初始化")
	}
	return a.client, nil
}

func (a *App) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	c, err := a.current()
	if err != nil {
		return nil, err
	}
	slug := strings.TrimSpace(r.State)
	switch r.Contribution {
	case "new":
		return newView(), nil
	case "edit":
		if slug == "" || slug == allValue || len(slug) > 120 || strings.ContainsAny(slug, "/\x00\r\n") {
			// Opened as an entry of its own (an extension's settings list every page): no space is chosen yet.
			return pickerView(ctx, c, "编辑 SPACE.md", "选择要编辑 SPACE.md 的空间。", "edit")
		}
		return editView(ctx, c, slug)
	default:
		// home and space draw the same page; the state is the space chosen in its switcher ("" or all: every space).
		return pageView(ctx, c, envOf(r), slug)
	}
}

// actionSlug is the space an action is about: the action's own value (UI version 4 and later), else the page's
// state, which every client sends back with an action.
func actionSlug(r *ext.UIRequest) string {
	state := strings.TrimSpace(r.State)
	if state == allValue {
		state = ""
	}
	return cmp.Or(strings.TrimSpace(r.Values["slug"]), state)
}

// notice answers an action with a hint instead of an error: an action error reaches the app as a failed page.
func notice(text string) (*wire.ActionResult, error) {
	return &wire.ActionResult{Toast: text}, nil
}

func action(id string, values map[string]string) *wire.Action {
	return &wire.Action{ID: id, Values: values}
}

func button(id, title string, values map[string]string) wire.Component {
	return wire.Component{Type: wire.ComponentButton, Title: title, Action: action(id, values)}
}

func secondary(text string) wire.Component {
	return wire.Component{Type: wire.ComponentText, Text: clipRunes(text, wire.StringMost), Style: "secondary"}
}

// isSpace says whether a track is a domain space this extension draws: not built in, and in the sidebar's spaces group.
func isSpace(t Track) bool { return !t.Builtin && t.Config.Sidebar.Group == "spaces" }

// isActiveSpace is a space that is not archived: the ones the switcher and the pickers offer.
func isActiveSpace(t Track) bool { return isSpace(t) && !t.Archived }

func spaceTitle(t Track) string {
	return clipRunes(cmp.Or(strings.TrimSpace(t.Title), strings.TrimSpace(t.Slug), "未命名空间"), wire.StringMost)
}

// spaceRows appends a row per active space, each sending actionID with its slug, or an empty state when there is none.
func spaceRows(items []wire.Component, tracks []Track, actionID string) []wire.Component {
	count := 0
	seen := make(map[string]struct{}, len(tracks))
	for _, t := range tracks {
		slug := strings.TrimSpace(t.Slug)
		if !isActiveSpace(t) || slug == "" {
			continue
		}
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}
		if len(items) >= wire.ItemsMost {
			break
		}
		items = append(items, wire.Component{
			Type:     wire.ComponentRow,
			Title:    spaceTitle(t),
			Subtitle: clipRunes(strings.TrimSpace(t.Description), wire.StringMost),
			Icon:     "chat",
			Action:   action(actionID, map[string]string{"slug": clipRunes(slug, wire.StringMost)}),
		})
		count++
	}
	if count == 0 {
		items = append(items, noSpaces())
	}
	return items
}

func noSpaces() wire.Component {
	return wire.Component{Type: wire.ComponentEmpty, Title: "还没有空间", Detail: "为一件长期关心的事建立空间，相关对话和 SPACE.md 会留在这里。"}
}

// pickerView is a page that needs a space but was opened without one: the spaces to choose from.
func pickerView(ctx context.Context, c *Client, title, hint, actionID string) (*wire.View, error) {
	tracks, err := c.Tracks(ctx)
	if err != nil {
		return nil, err
	}
	items := spaceRows([]wire.Component{secondary(hint)}, tracks, actionID)
	if len(items) < wire.ItemsMost {
		items = append(items, button("new", "新建空间", nil))
	}
	return &wire.View{Title: title, Sections: []wire.Section{{Items: items}}, RefreshAfter: 60}, nil
}

func newView() *wire.View {
	return &wire.View{Title: "新建空间", Sections: []wire.Section{{Items: []wire.Component{
		{Type: wire.ComponentField, Name: "title", Label: "名称", Kind: wire.FieldKindText, Placeholder: "例如：阅读、旅行、家庭"},
		{Type: wire.ComponentField, Name: "slug", Label: "标识", Kind: wire.FieldKindText, Placeholder: "小写字母、数字和连字符"},
		{Type: wire.ComponentField, Name: "description", Label: "简介", Kind: wire.FieldKindMultiline, Placeholder: "这个空间用来做什么"},
		{Type: wire.ComponentField, Name: "categories", Label: "信息分类（可选）", Kind: wire.FieldKindText, Placeholder: "finance, relationships, emotion, legal_id"},
		{Type: wire.ComponentButton, Title: "创建空间", Action: &wire.Action{ID: "create", Submit: true}},
	}}}}
}

func editView(ctx context.Context, c *Client, slug string) (*wire.View, error) {
	md, err := c.SpaceMD(ctx, slug)
	if err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(md.Content) > wire.StringMost {
		return &wire.View{Title: "编辑 SPACE.md", State: slug, Sections: []wire.Section{{Items: []wire.Component{
			{Type: wire.ComponentText, Text: fmt.Sprintf("SPACE.md 超过 %d 字，暂不支持在页面内直接编辑，避免保存时截断内容。请在助手所在电脑上编辑该文件。", wire.StringMost), Style: "secondary"},
			{Type: wire.ComponentText, Text: clipRunes(strings.TrimSpace(md.Content), 1800)},
			button("open", "返回空间", map[string]string{"slug": slug}),
		}}}}, nil
	}
	return &wire.View{Title: "编辑 SPACE.md", State: slug, Sections: []wire.Section{{Items: []wire.Component{
		{Type: wire.ComponentText, Text: "SPACE.md 是这个空间长期保留的说明。Persona 管理的状态块不可在这里修改。", Style: "secondary"},
		{Type: wire.ComponentField, Name: "content", Label: "SPACE.md", Kind: wire.FieldKindMultiline, Value: md.Content},
		{Type: wire.ComponentButton, Title: "保存", Action: &wire.Action{ID: "save", Submit: true, Values: map[string]string{"slug": slug, "digest": clipRunes(md.Digest, wire.StringMost)}}},
	}}}}, nil
}

func (a *App) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	c, err := a.current()
	if err != nil {
		return nil, err
	}
	slug := actionSlug(r)
	switch r.Action {
	case "home":
		return &wire.ActionResult{Push: &wire.Push{UI: "home"}}, nil
	case "new":
		return &wire.ActionResult{Push: &wire.Push{UI: "new"}}, nil
	case "open":
		return &wire.ActionResult{Push: &wire.Push{UI: "space", State: slug}}, nil
	case "edit":
		return &wire.ActionResult{Push: &wire.Push{UI: "edit", State: slug}}, nil
	case "pick":
		if strings.TrimSpace(r.Values["space"]) == newValue {
			return &wire.ActionResult{Push: &wire.Push{UI: "new"}}, nil
		}
		v, err := pageView(ctx, c, envOf(r), r.Values["space"])
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{View: v}, nil
	case "chat":
		return &wire.ActionResult{OpenURL: conversationURL(r.Values["mid"], slug, r.Values["sid"])}, nil
	case "create":
		title := strings.TrimSpace(r.Values["title"])
		if title == "" || utf8.RuneCountInString(title) > 80 {
			return notice("请输入不超过 80 个字符的空间名称")
		}
		slug = strings.TrimSpace(r.Values["slug"])
		if !validSlug(slug) {
			return notice("标识须以字母开头，只含小写字母、数字和连字符，且不能使用保留名称")
		}
		var cats []string
		for _, cat := range strings.Split(r.Values["categories"], ",") {
			if cat = strings.TrimSpace(cat); cat != "" {
				cats = append(cats, cat)
			}
		}
		track, err := c.Create(ctx, title, slug, strings.TrimSpace(r.Values["description"]), cats)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{Push: &wire.Push{UI: "space", State: track.Slug}, Toast: "空间已创建"}, nil
	case "save":
		if slug == "" {
			return notice("先选择一个空间")
		}
		if err := c.PutSpaceMD(ctx, slug, r.Values["content"], r.Values["digest"]); err != nil {
			return nil, err
		}
		return &wire.ActionResult{Push: &wire.Push{UI: "space", State: slug}, Toast: "SPACE.md 已保存"}, nil
	case "archive", "unarchive":
		tracks, err := c.Tracks(ctx)
		if err != nil {
			return nil, err
		}
		archived := r.Action == "archive"
		for _, track := range tracks {
			if isSpace(track) && track.Slug == slug {
				if err := c.Archive(ctx, track, archived); err != nil {
					return nil, err
				}
				// Archiving leaves the space's page for every space; restoring stays on the space, now active again.
				sel, toast := "", "空间已归档"
				if !archived {
					sel, toast = slug, "空间已恢复"
				}
				v, err := pageView(ctx, c, envOf(r), sel)
				if err != nil {
					return nil, err
				}
				return &wire.ActionResult{View: v, Toast: toast}, nil
			}
		}
		return notice("空间不存在")
	case "start":
		prompt := strings.TrimSpace(r.Values["prompt"])
		if prompt == "" {
			return notice("先写一条消息")
		}
		if slug == "" {
			return notice("先选择一个空间")
		}
		opts := StartOptions{
			Launcher: strings.TrimSpace(r.Values["launcher"]),
			Model:    strings.TrimSpace(r.Values["model"]),
			Effort:   strings.TrimSpace(r.Values["effort"]),
		}
		if _, _, err := c.Start(ctx, slug, prompt, opts); err != nil {
			return nil, err
		}
		// The refreshed page lists the new conversation; its row opens it. No persona://tracks link here: hubs before
		// the handoff was accepted refuse the whole answer, which would fail a conversation that did start.
		v, err := pageView(ctx, c, envOf(r), slug)
		if err != nil {
			return nil, err
		}
		return &wire.ActionResult{View: v, Toast: "对话已开始"}, nil
	default:
		// Apps before UI version 4 send no action values: a switcher row there names its space in its id.
		if sel, ok := strings.CutPrefix(r.Action, pickPrefix); ok {
			if sel == newValue {
				return &wire.ActionResult{Push: &wire.Push{UI: "new"}}, nil
			}
			v, err := pageView(ctx, c, envOf(r), sel)
			if err != nil {
				return nil, err
			}
			return &wire.ActionResult{View: v}, nil
		}
		return nil, fmt.Errorf("未知操作 %q", r.Action)
	}
}

func clipRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

func conversationURL(machine, slug, sid string) string {
	return "persona://tracks/" + url.PathEscape(machine) + "/" + url.PathEscape(slug) + "/" + url.PathEscape(sid)
}

func validSlug(slug string) bool {
	if len(slug) < 2 || len(slug) > 32 || slug[0] < 'a' || slug[0] > 'z' {
		return false
	}
	for _, c := range slug {
		if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	switch slug {
	case "main", "health", "assistant", "guest", "spaces", "tracks", "new", allValue:
		return false
	}
	return true
}
