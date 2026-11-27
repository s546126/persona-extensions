package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type pageState struct {
	Mode     string `json:"m,omitempty"`
	StreamID int64  `json:"s,omitempty"`
	Topic    string `json:"t,omitempty"`
	Anchor   int64  `json:"a,omitempty"`
}

func validTopic(topic string) bool {
	return len([]rune(topic)) <= maxTopicChars && !strings.ContainsAny(topic, "\x00\r\n")
}

func readState(r *ext.UIRequest) (pageState, error) {
	var st pageState
	if err := r.DecodeState(&st); err != nil || st.StreamID < 0 || st.Anchor < 0 || !validTopic(st.Topic) {
		return pageState{}, errors.New("invalid page state")
	}
	if st.Mode != "" && st.Mode != "topics" && st.Mode != "messages" {
		return pageState{}, errors.New("invalid page state")
	}
	if st.Mode == "topics" && st.StreamID <= 0 {
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
	case "stream":
		st.StreamID, err = parsePositiveID(r.Values["stream_id"])
		if err != nil {
			return nil, err
		}
		st.Mode, st.Topic, st.Anchor = "topics", "", 0
	case "topic":
		if !validTopic(r.Values["topic"]) {
			return nil, errors.New("invalid topic name")
		}
		st.Mode, st.Topic, st.Anchor = "messages", r.Values["topic"], 0
	case "more":
		st.Anchor, err = parsePositiveID(r.Values["anchor"])
		if err != nil || st.Anchor == 0 {
			return nil, errors.New("invalid older message anchor")
		}
	case "back":
		st = pageState{}
	default:
		return nil, fmt.Errorf("unknown page action %q", r.Action)
	}
	v, err := a.page(ctx, r, st)
	return &wire.ActionResult{View: v}, err
}

func (a *app) page(ctx context.Context, r *ext.UIRequest, st pageState) (*wire.View, error) {
	t := textsFor(r.Locale)
	c := a.currentClient()
	if c.base == "" || c.email == "" || c.token == "" {
		return &wire.View{Sections: []wire.Section{{Items: []wire.Component{{Type: wire.ComponentEmpty, Title: t.missingConfig}}}}}, nil
	}
	if st.Mode == "topics" {
		items, err := c.topics(ctx, st.StreamID)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{Title: t.topics}
		// Zulip lists the most recent topic first; the view keeps the first maxViewTopics.
		for i, item := range items[:min(len(items), maxViewTopics)] {
			title := strings.TrimSpace(item.Name)
			if title == "" {
				title = t.emptyTopic
			}
			row := wire.Component{Type: wire.ComponentRow, Key: fmt.Sprintf("topic-%d", i), Title: clipText(title, 1800), Subtitle: fmt.Sprintf(t.latest, item.MaxID)}
			if validTopic(item.Name) {
				row.Action = &wire.Action{ID: "topic", Values: map[string]string{"topic": item.Name}}
			}
			section.Items = append(section.Items, row)
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noTopics})
		}
		return &wire.View{Title: t.topics, State: ext.EncodeState(st), Sections: []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}}}}}}, nil
	}
	if st.Mode == "messages" {
		page, err := c.messages(ctx, st.StreamID, st.Topic, st.Anchor, st.Anchor == 0)
		a.record(err)
		if err != nil {
			return nil, err
		}
		section := wire.Section{Title: t.messages}
		var oldest int64
		var shown [][]wire.Component
		seen := map[int64]bool{}
		for _, item := range page.Messages {
			if item.ID > 0 {
				if seen[item.ID] {
					continue
				}
				seen[item.ID] = true
				if oldest == 0 || item.ID < oldest {
					oldest = item.ID
				}
			}
			shown = append(shown, textComponents(clipText(fmt.Sprintf("%s · %s\n%s", item.SenderFullName, item.Subject, item.Content), 12000)))
		}
		for _, message := range fitMessages(shown, maxViewItems) {
			section.Items = append(section.Items, message...)
		}
		if len(section.Items) == 0 {
			section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noMessages})
		}
		if !page.FoundOldest && oldest > 0 && (st.Anchor == 0 || oldest < st.Anchor) {
			st.Anchor = oldest
			section.More = &wire.More{Title: t.more, Action: wire.Action{ID: "more", Values: map[string]string{"anchor": fmt.Sprint(oldest)}}}
		}
		return &wire.View{Title: t.messages, State: ext.EncodeState(st), Sections: []wire.Section{section, {Items: []wire.Component{{Type: wire.ComponentButton, Title: t.back, Action: &wire.Action{ID: "back"}}}}}}, nil
	}
	if st.Mode != "" {
		return nil, errors.New("invalid inbox view")
	}
	items, err := c.streams(ctx)
	a.record(err)
	if err != nil {
		return nil, err
	}
	section := wire.Section{Title: t.streams}
	seen := map[int64]bool{}
	for _, item := range items {
		if item.IsArchived || item.ID <= 0 || seen[item.ID] {
			continue
		}
		if len(section.Items) >= maxViewItems {
			break
		}
		seen[item.ID] = true
		title := strings.TrimSpace(item.Name)
		if title == "" {
			title = fmt.Sprintf("#%d", item.ID)
		}
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentRow, Key: fmt.Sprintf("stream-%d", item.ID), Title: clipText(title, 1800), Subtitle: clipText(item.Description, 1800), Action: &wire.Action{ID: "stream", Values: map[string]string{"stream_id": fmt.Sprint(item.ID)}}})
	}
	if len(section.Items) == 0 {
		section.Items = append(section.Items, wire.Component{Type: wire.ComponentEmpty, Title: t.noStreams})
	}
	return &wire.View{Title: t.inbox, State: ext.EncodeState(st), Sections: []wire.Section{section}}, nil
}

// maxViewTopics keeps the topics page within the hub's 300 component limit, whatever the stream holds.
const maxViewTopics = 250

// maxViewItems keeps the messages page within the hub's 300 component limit, whatever the messages hold.
const maxViewItems = 250

// fitMessages cuts the longest messages to a common length, so that together they have at most n components and the
// short ones are whole. A cut message ends with a mark.
func fitMessages(messages [][]wire.Component, n int) [][]wire.Component {
	if n <= 0 {
		return nil
	}
	if len(messages) > n {
		messages = messages[:n]
	}
	limit := 1
	for l := 240; l > 1; l-- {
		total := 0
		for _, m := range messages {
			total += min(len(m), l)
		}
		if total <= n {
			limit = l
			break
		}
	}
	for i, m := range messages {
		if len(m) > limit {
			m = m[:limit]
			m[limit-1].Text = clipText(m[limit-1].Text, 1700) + " […]"
			messages[i] = m
		}
	}
	return messages
}

func parsePositiveID(raw string) (int64, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.HasPrefix(raw, "+") {
		return 0, errors.New("invalid positive ID")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid positive ID")
	}
	return id, nil
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
