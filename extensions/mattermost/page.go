package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Mode      string   `json:"m,omitempty"`
	TeamID    string   `json:"t,omitempty"`
	ChannelID string   `json:"c,omitempty"`
	Before    string   `json:"b,omitempty"`
	Pages     []string `json:"g,omitempty"`
	Page      int      `json:"p,omitempty"`
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.Page < 0 || len(st.Pages) >= maxViewPages {
		return pageState{}, errors.New("invalid page state")
	}
	if st.Mode != "" && st.Mode != "channels" && st.Mode != "posts" {
		return pageState{}, errors.New("invalid page state")
	}
	if (st.TeamID != "" && !validID(st.TeamID)) || (st.ChannelID != "" && !validID(st.ChannelID)) || (st.Before != "" && !validID(st.Before)) {
		return pageState{}, errors.New("invalid page state")
	}
	if (st.Mode == "channels" && st.TeamID == "") || (st.Mode == "posts" && st.ChannelID == "") {
		return pageState{}, errors.New("invalid page state")
	}
	seen := make(map[string]bool, len(st.Pages))
	for _, id := range st.Pages {
		if !validID(id) || seen[id] {
			return pageState{}, errors.New("invalid page state")
		}
		seen[id] = true
	}
	if len(ext.EncodeState(st)) > wire.StateMost {
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
		if !validID(r.Values["team_id"]) {
			return nil, errors.New("invalid team ID")
		}
		st = pageState{Mode: "channels", TeamID: r.Values["team_id"]}
	case "channel":
		if !validID(r.Values["channel_id"]) {
			return nil, errors.New("invalid channel ID")
		}
		st.Mode, st.ChannelID, st.Before, st.Pages, st.Page = "posts", r.Values["channel_id"], "", nil, 0
	case "more":
		if st.Mode != "posts" || !validID(r.Values["before"]) || r.Values["before"] != st.Before || slices.Contains(st.Pages, st.Before) || len(st.Pages)+1 >= maxViewPages {
			return nil, errors.New("invalid previous post ID")
		}
		st.Pages = append(st.Pages, st.Before)
	case "back":
		st = pageState{}
	default:
		return nil, fmt.Errorf("unknown page action %q", r.Action)
	}
	v, err := a.page(ctx, r, st)
	return &wire.ActionResult{View: v}, err
}

// The messages view shows the batches the view has gone through, the first and then those at the ids in Pages, so that
// Load more answers the whole list, longer. It stops adding messages at maxViewItems components to keep the view within
// the SDK's component limit, and holds at most maxViewPages batches to keep the state small.
const (
	maxViewItems = 250
	maxViewPages = 10
)

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	c := a.currentClient()
	if c.base == "" || c.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "channels" {
		items, err := c.channels(ctx, st.TeamID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{Title: t.channels}
		seen := map[string]bool{}
		for _, item := range items {
			if !validID(item.ID) || item.DeleteAt != 0 || seen[item.ID] {
				continue
			}
			if len(section.Items) >= maxViewItems {
				break
			}
			seen[item.ID] = true
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "channel-" + item.ID, Title: clipText(channelName(item), 1800), Subtitle: clipText(item.Type, 1800), Action: &wire.Action{ID: "channel", Values: map[string]string{"channel_id": item.ID}}})
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noChannels})
		}
		return &wire.View{Title: t.channels, State: ext.EncodeState(st), Sections: []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}}}}}}, nil
	}
	if st.Mode == "posts" {
		users := map[string]user{}
		seenPosts := map[string]bool{}
		seenCursors := make(map[string]bool, len(st.Pages))
		for _, id := range st.Pages {
			seenCursors[id] = true
		}
		section := wire.Section{Title: t.messages}
		prev, full := "", false
		for _, before := range append([]string{""}, st.Pages...) {
			page, err := c.posts(ctx, st.ChannelID, before, st.Page)
			a.record(err)
			if err != nil {
				return nil, err
			}
			for _, id := range page.Order {
				if seenPosts[id] {
					continue
				}
				post, ok := page.Posts[id]
				if !ok || post.DeleteAt != 0 {
					continue
				}
				seenPosts[id] = true
				author, err := a.postAuthor(ctx, c, users, post.UserID)
				if err != nil {
					return nil, err
				}
				text := textComponents(clipText(author+": "+post.Message, 10000))
				if len(section.Items)+len(text) > maxViewItems {
					full = true
					break
				}
				section.Items = append(section.Items, text...)
			}
			if full {
				break
			}
			prev = olderCursor(page)
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noMessages})
		}
		st.Before = ""
		if validID(prev) && !seenCursors[prev] && !full && len(st.Pages)+1 < maxViewPages {
			st.Before = prev
			section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more", Values: map[string]string{"before": st.Before}}}
		}
		return &wire.View{Title: t.messages, State: ext.EncodeState(st), Sections: []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}}}}}}, nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid inbox view")
	}
	items, err := a.unreadChannels(ctx)
	a.record(err)
	if err != nil {
		return nil, err
	}
	section := wire.Section{Title: t.inbox}
	seen := map[string]bool{}
	for _, item := range items {
		if !validID(item.Channel.ID) || seen[item.Channel.ID] {
			continue
		}
		if len(section.Items) >= maxViewItems {
			break
		}
		seen[item.Channel.ID] = true
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: "channel-" + item.Channel.ID, Title: clipText(channelName(item.Channel), 1800), Subtitle: clipText(fmt.Sprintf(t.unread, item.Unread.MessageCount, item.Unread.MentionCount), 1800), Action: &wire.Action{ID: "channel", Values: map[string]string{"channel_id": item.Channel.ID}}})
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noUnread})
	}
	return &wire.View{Title: t.inbox, State: ext.EncodeState(st), Sections: []wire.Section{section}}, nil
}

func textComponents(text string) []wire.Component {
	var out []wire.Component
	for _, line := range strings.Split(text, "\n") {
		r := []rune(line)
		for len(r) > 0 {
			n := min(len(r), 1800)
			out = append(out, wire.Component{Type: wire.ComponentText, Text: string(r[:n])})
			r = r[n:]
			if len(out) >= 220 {
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
