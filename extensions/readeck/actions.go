package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// maxLabels and maxLabelChars bound a bookmark's labels. The page state holds them joined with ", ", so it allows
// maxLabelsStateChars characters.
const (
	maxLabels           = 20
	maxLabelChars       = 100
	maxLabelsStateChars = maxLabels*maxLabelChars + (maxLabels-1)*len(", ")
)

// maxTitleChars is the longest bookmark title: the validation, the tool schema and the page state check use it.
const maxTitleChars = 1000

type saveArgs struct {
	URL    string   `json:"url"`
	Title  string   `json:"title,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

type updateArgs struct {
	BookmarkID   string  `json:"bookmark_id"`
	ReadProgress *int    `json:"read_progress,omitempty"`
	IsMarked     *bool   `json:"is_marked,omitempty"`
	Note         *string `json:"note,omitempty"`
}

func (in saveArgs) validate() error {
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || len(in.URL) > 1024 {
		return errors.New("bookmark URL must be a clean HTTP(S) address of at most 1024 characters")
	}
	if len([]rune(in.Title)) > maxTitleChars || strings.ContainsFunc(in.Title, unicode.IsControl) {
		return errors.New("title must be at most 1000 characters without control characters")
	}
	if len(in.Labels) > maxLabels {
		return errors.New("at most 20 labels are allowed")
	}
	seen := map[string]bool{}
	for _, label := range in.Labels {
		label = strings.TrimSpace(label)
		if label == "" || len([]rune(label)) > maxLabelChars || strings.ContainsFunc(label, unicode.IsControl) || seen[label] {
			return errors.New("labels must be unique, nonempty text of at most 100 characters")
		}
		seen[label] = true
	}
	return nil
}

func (in updateArgs) validate() error {
	if !validBookmarkID(in.BookmarkID) || in.ReadProgress == nil && in.IsMarked == nil && in.Note == nil {
		return errors.New("a valid bookmark ID and at least one reading update are required")
	}
	if in.ReadProgress != nil && (*in.ReadProgress < 0 || *in.ReadProgress > 100) {
		return errors.New("read progress must be between 0 and 100")
	}
	if in.Note != nil && (len([]rune(*in.Note)) > 30000 || strings.ContainsFunc(*in.Note, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' })) {
		return errors.New("note must be at most 30000 characters without unsupported control characters")
	}
	return nil
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	var summary string
	switch name {
	case "readeck_save_bookmark":
		var in saveArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		summary = "Save webpage to Readeck\nURL: " + in.URL
		if in.Title != "" {
			summary += "\nTitle: " + in.Title
		}
		if len(in.Labels) > 0 {
			summary += "\nLabels: " + strings.Join(in.Labels, ", ")
		}
	case "readeck_update_reading":
		var in updateArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		summary = "Update Readeck reading\nBookmark: " + in.BookmarkID
		if in.ReadProgress != nil {
			summary += fmt.Sprintf("\nReading progress: %d%%", *in.ReadProgress)
		}
		if in.IsMarked != nil {
			summary += fmt.Sprintf("\nFavorite: %v", *in.IsMarked)
		}
		if in.Note != nil {
			summary += "\nPersonal note: " + *in.Note
		}
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": summary}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	var text string
	var err error
	switch name {
	case "readeck_save_bookmark":
		var in saveArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		keyData, _ := json.Marshal(in)
		text, err = a.receipts.commit(requestID, name+":"+string(keyData), func() (string, error) {
			id, err := a.client.save(ctx, in.URL, in.Title, in.Labels)
			if err != nil {
				return "", err
			}
			return "Readeck accepted bookmark " + id + "; article extraction may still be in progress", nil
		})
	case "readeck_update_reading":
		var in updateArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		keyData, _ := json.Marshal(in)
		text, err = a.receipts.commit(requestID, name+":"+string(keyData), func() (string, error) {
			if err := a.client.updateReading(ctx, in.BookmarkID, in.ReadProgress, in.IsMarked, in.Note); err != nil {
				return "", err
			}
			return "Updated Readeck bookmark " + in.BookmarkID, nil
		})
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	if err != nil {
		return nil, err
	}
	a.record(nil)
	if a.hub != nil {
		_ = a.hub.Invalidate(pageUI)
	}
	return map[string]any{"text": text}, nil
}
