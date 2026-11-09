package main

import (
	"strings"

	"github.com/nwverse/persona/sdk/go/wire"
)

// adapt fits a view to an app of UI version ver (0: the newest): what that app does not know is drawn with what it
// does. Tabs (4) become a segmented (2) or buttons; the filter and the search box (3) become a field and a button;
// load more (3) a button; markdown, table, empty and loading (3) text and rows; a stat grid and a segmented (2)
// metrics and buttons; rows lose their icons and components their keys (3).
func adapt(v *wire.View, ver int) *wire.View {
	if v == nil || ver <= 0 {
		return v
	}
	var pre []wire.Component
	if v.Tabs != nil && ver < 4 {
		t := v.Tabs
		choices := make([]wire.Choice, 0, len(t.Items))
		for _, it := range t.Items {
			title := firstNonEmpty(it.Title, it.Value)
			if it.Badge != "" {
				title += " " + it.Badge
			}
			choices = append(choices, wire.Choice{Value: it.Value, Title: title})
		}
		pre = append(pre, wire.Component{Type: wire.ComponentSegmented, Name: t.Name, Choices: choices, Value: t.Value, Action: &t.Action})
		v.Tabs = nil
	}
	if v.Filter != nil && ver < 3 {
		f := v.Filter
		pre = append(pre,
			wire.Component{Type: wire.ComponentField, Name: f.Name, Label: firstNonEmpty(f.Title, f.Name), Kind: wire.FieldKindChoice,
				Value: f.Value, Choices: f.Choices},
			wire.Component{Type: wire.ComponentButton, Title: "切换" + f.Title, Action: &wire.Action{ID: f.Action.ID, Submit: true}})
		v.Filter = nil
	}
	if v.Search != nil && ver < 3 {
		s := v.Search
		pre = append(pre,
			wire.Component{Type: wire.ComponentField, Name: "q", Label: "搜索", Kind: wire.FieldKindText, Value: s.Value, Placeholder: s.Placeholder},
			wire.Component{Type: wire.ComponentButton, Title: "搜索", Action: &wire.Action{ID: s.Action.ID, Submit: true}})
		v.Search = nil
	}
	if len(pre) > 0 {
		v.Sections = append([]wire.Section{{Items: pre}}, v.Sections...)
	}
	for i := range v.Sections {
		sec := &v.Sections[i]
		if ver < 4 {
			sec.Pane = ""
		}
		if sec.More != nil && ver < 3 {
			sec.Items = append(sec.Items, wire.Component{Type: wire.ComponentButton, Title: firstNonEmpty(sec.More.Title, "加载更多"),
				Action: &wire.Action{ID: sec.More.Action.ID}})
			sec.More = nil
		}
		out := make([]wire.Component, 0, len(sec.Items))
		for _, c := range sec.Items {
			out = append(out, older(c, ver)...)
		}
		sec.Items = out
	}
	return v
}

// older draws c with the components of UI version ver.
func older(c wire.Component, ver int) []wire.Component {
	if ver >= 3 {
		return []wire.Component{c}
	}
	c.Key = ""
	switch c.Type {
	case wire.ComponentRow:
		c.Icon, c.Avatar, c.Thumbnail = "", nil, nil
	case wire.ComponentMarkdown:
		text := strings.NewReplacer("**", "", "`", "", "# ", "").Replace(c.Text)
		return []wire.Component{{Type: wire.ComponentText, Text: truncate(text, 2000)}}
	case wire.ComponentEmpty:
		text := c.Title
		if c.Detail != "" {
			text += "：" + c.Detail
		}
		out := []wire.Component{{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: truncate(text, 2000)}}
		if c.Button != nil {
			act := c.Button.Action
			out = append(out, wire.Component{Type: wire.ComponentButton, Title: c.Button.Title, Action: &act})
		}
		return out
	case wire.ComponentLoading:
		return []wire.Component{{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: firstNonEmpty(c.Title, "加载中…")}}
	case wire.ComponentTable:
		var out []wire.Component
		for i, row := range c.Rows {
			if i == 50 {
				break
			}
			title := "—"
			if len(row) > 0 && strings.TrimSpace(row[0]) != "" {
				title = row[0]
			}
			var parts []string
			for j := 1; j < len(row) && j < len(c.Columns); j++ {
				if row[j] != "" {
					parts = append(parts, c.Columns[j].Title+"："+row[j])
				}
			}
			out = append(out, wire.Component{Type: wire.ComponentRow, Title: truncate(title, 200), Subtitle: truncate(strings.Join(parts, " · "), 2000)})
		}
		if c.Caption != "" {
			out = append(out, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleCaption, Text: c.Caption})
		}
		return out
	}
	if ver >= 2 {
		return []wire.Component{c}
	}
	switch c.Type {
	case wire.ComponentStatGrid:
		out := make([]wire.Component, 0, len(c.Stats))
		for _, s := range c.Stats {
			out = append(out, wire.Component{Type: wire.ComponentMetric, Label: s.Label, Value: s.Value, Detail: s.Detail, Tone: s.Tone})
		}
		return out
	case wire.ComponentSegmented:
		out := make([]wire.Component, 0, len(c.Choices))
		for _, ch := range c.Choices {
			title := firstNonEmpty(ch.Title, ch.Value)
			if ch.Value == c.Value {
				title = "· " + title + " ·"
			}
			id := ch.Value
			if c.Action != nil {
				id = c.Action.ID + ":" + ch.Value
			}
			out = append(out, wire.Component{Type: wire.ComponentButton, Title: title, Action: &wire.Action{ID: id}})
		}
		return out
	case wire.ComponentChart:
		return nil
	}
	return []wire.Component{c}
}
