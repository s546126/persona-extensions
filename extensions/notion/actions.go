package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	var summary string
	switch name {
	case "notion_append":
		var in struct {
			PageID string `json:"page_id"`
			Kind   string `json:"kind"`
			Text   string `json:"text"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := validNotionID(in.PageID); err != nil {
			return nil, err
		}
		if in.Kind != "paragraph" && in.Kind != "todo" {
			return nil, errors.New("kind must be paragraph or todo")
		}
		if err := validNote(in.Text, 2000); err != nil {
			return nil, err
		}
		summary = "Append " + in.Kind + " to Notion page " + in.PageID + ":\n" + in.Text
	case "notion_create":
		var in struct {
			ParentID string `json:"parent_id"`
			Title    string `json:"title"`
			Content  string `json:"content"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := validNotionID(in.ParentID); err != nil {
			return nil, err
		}
		if err := validNote(in.Title, 500); err != nil {
			return nil, err
		}
		if in.Content != "" {
			if err := validNote(in.Content, 2000); err != nil {
				return nil, err
			}
		}
		summary = "Create Notion child page: " + in.Title + "\nParent ID: " + in.ParentID
		if in.Content != "" {
			summary += "\n" + in.Content
		}
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": summary}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	var key string
	var fn func() (string, error)
	switch name {
	case "notion_append":
		var in struct {
			PageID string `json:"page_id"`
			Kind   string `json:"kind"`
			Text   string `json:"text"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := validNotionID(in.PageID); err != nil {
			return nil, err
		}
		if in.Kind != "paragraph" && in.Kind != "todo" {
			return nil, errors.New("kind must be paragraph or todo")
		}
		if err := validNote(in.Text, 2000); err != nil {
			return nil, err
		}
		keyData, _ := json.Marshal(map[string]any{"action": name, "page_id": in.PageID, "kind": in.Kind, "text": in.Text})
		key = string(keyData)
		fn = func() (string, error) {
			child := paragraphRequest(in.Text)
			if in.Kind == "todo" {
				child = todoRequest(in.Text)
			}
			if err := a.client.append(ctx, in.PageID, []map[string]any{child}); err != nil {
				return "", err
			}
			return "Appended " + in.Kind + " to Notion page " + in.PageID, nil
		}
	case "notion_create":
		var in struct {
			ParentID string `json:"parent_id"`
			Title    string `json:"title"`
			Content  string `json:"content"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := validNotionID(in.ParentID); err != nil {
			return nil, err
		}
		if err := validNote(in.Title, 500); err != nil {
			return nil, err
		}
		if in.Content != "" {
			if err := validNote(in.Content, 2000); err != nil {
				return nil, err
			}
		}
		keyData, _ := json.Marshal(map[string]any{"action": name, "parent_id": in.ParentID, "title": in.Title, "content": in.Content})
		key = string(keyData)
		fn = func() (string, error) {
			page, err := a.client.create(ctx, in.ParentID, in.Title, in.Content)
			if err != nil {
				return "", err
			}
			if page.ID == "" {
				return "", errors.New("Notion returned no page ID")
			}
			return "Created Notion page: " + pageTitle(page), nil
		}
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	text, err := a.receipts.commit(requestID, key, fn)
	if err != nil {
		return nil, err
	}
	a.record(nil)
	if a.hub != nil {
		_ = a.hub.Invalidate(pageUI)
	}
	return map[string]any{"text": text}, nil
}

var _ = strings.TrimSpace
