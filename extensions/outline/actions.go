package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

type createArgs struct {
	CollectionID string `json:"collection_id"`
	Title        string `json:"title"`
	Text         string `json:"text"`
}

func (in createArgs) validate() error {
	if in.CollectionID != "personal" && !uuidPattern.MatchString(in.CollectionID) {
		return errors.New("collection must be a UUID returned by outline_collections or personal")
	}
	if strings.TrimSpace(in.Title) == "" || len([]rune(in.Title)) > 100 || strings.ContainsFunc(in.Title, unicode.IsControl) {
		return errors.New("title must be nonempty, at most 100 characters and without control characters")
	}
	if strings.TrimSpace(in.Text) == "" || len([]rune(in.Text)) > 100000 || strings.ContainsFunc(in.Text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return errors.New("document text must be nonempty, at most 100000 characters and without unsupported control characters")
	}
	return nil
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	if name != "outline_create_document" {
		return nil, fmt.Errorf("unknown action %q", name)
	}
	var in createArgs
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	summary := "Create unpublished Outline document\nCollection: " + in.CollectionID + "\nTitle: " + in.Title + "\nPublish: false\n\n" + in.Text
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": summary}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	if name != "outline_create_document" {
		return nil, fmt.Errorf("unknown action %q", name)
	}
	var in createArgs
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	keyData, _ := json.Marshal(in)
	text, err := a.receipts.commit(requestID, name+":"+string(keyData), func() (string, error) {
		item, err := a.client.create(ctx, in.CollectionID, in.Title, in.Text)
		if err != nil {
			return "", err
		}
		return "Created unpublished Outline document " + item.ID + ": " + item.Title, nil
	})
	if err != nil {
		return nil, err
	}
	a.record(nil)
	if a.hub != nil {
		_ = a.hub.Invalidate(pageUI)
	}
	return map[string]any{"text": text}, nil
}
