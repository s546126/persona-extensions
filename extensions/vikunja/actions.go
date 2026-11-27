package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

type createArgs struct {
	ProjectID   int64  `json:"project_id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	DueDate     string `json:"due_date,omitempty"`
}

// maxTitleChars is the longest task title: the validation, the tool schema and the page state check use it.
const maxTitleChars = 500

func (in createArgs) validate() error {
	if in.ProjectID <= 0 || strings.TrimSpace(in.Title) == "" || len([]rune(in.Title)) > maxTitleChars || len([]rune(in.Description)) > 30000 {
		return errors.New("a positive project ID, nonempty title and bounded description are required")
	}
	if strings.ContainsFunc(in.Title, unicode.IsControl) || strings.ContainsFunc(in.Description, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return errors.New("task text contains unsupported control characters")
	}
	if in.DueDate != "" {
		if _, err := time.Parse(time.RFC3339, in.DueDate); err != nil {
			return errors.New("due date must be an RFC3339 timestamp")
		}
	}
	return nil
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	var summary string
	switch name {
	case "vikunja_create_task":
		var in createArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		summary = fmt.Sprintf("Create Vikunja task\nProject: %d\nTitle: %s", in.ProjectID, in.Title)
		if in.Description != "" {
			summary += "\nDescription: " + in.Description
		}
		if in.DueDate != "" {
			summary += "\nDue: " + in.DueDate
		}
	case "vikunja_complete_task":
		var in struct {
			TaskID int64 `json:"task_id"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if in.TaskID <= 0 {
			return nil, errors.New("task ID must be positive")
		}
		summary = fmt.Sprintf("Complete Vikunja task #%d\nSend only done:true", in.TaskID)
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": summary}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	var text string
	var err error
	switch name {
	case "vikunja_create_task":
		var in createArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		keyData, _ := json.Marshal(in)
		text, err = a.receipts.commit(requestID, name+":"+string(keyData), func() (string, error) {
			item, err := a.client.createTask(ctx, in.ProjectID, in.Title, in.Description, in.DueDate)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Created Vikunja task #%d: %s", item.ID, item.Title), nil
		})
	case "vikunja_complete_task":
		var in struct {
			TaskID int64 `json:"task_id"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if in.TaskID <= 0 {
			return nil, errors.New("task ID must be positive")
		}
		keyData, _ := json.Marshal(in)
		text, err = a.receipts.commit(requestID, name+":"+string(keyData), func() (string, error) {
			if _, err := a.client.completeTask(ctx, in.TaskID); err != nil {
				return "", err
			}
			return fmt.Sprintf("Completed Vikunja task #%d", in.TaskID), nil
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
