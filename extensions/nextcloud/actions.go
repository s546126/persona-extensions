package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

const maxNameBytes = 250

type createArgs struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

func (in createArgs) validate() error {
	if !validRemotePath(in.Path) || !strings.HasSuffix(strings.ToLower(in.Path), ".md") || strings.TrimSpace(in.Text) == "" || len(in.Text) > 30000 {
		return errors.New("a new safe relative .md path and nonempty text of at most 30000 UTF-8 bytes are required")
	}
	// Nextcloud verifies every segment of a path it writes and refuses one longer than 250 bytes.
	for _, part := range strings.Split(in.Path, "/") {
		if len(part) > maxNameBytes {
			return errors.New("every file and folder name in the path must be at most 250 UTF-8 bytes")
		}
	}
	if strings.ContainsFunc(in.Text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return errors.New("note text contains unsupported control characters")
	}
	return nil
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	if name != "nextcloud_create_note" {
		return nil, fmt.Errorf("unknown action %q", name)
	}
	var in createArgs
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": "Create a new Nextcloud Markdown note without overwriting\nPath: " + in.Path + "\n\n" + in.Text}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	if name != "nextcloud_create_note" {
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
		if err := a.client.createNote(ctx, a.client.username, in.Path, []byte(in.Text)); err != nil {
			return "", err
		}
		return "Created Nextcloud Markdown note: " + in.Path, nil
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
