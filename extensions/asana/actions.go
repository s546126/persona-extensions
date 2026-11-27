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
	WorkspaceGID string `json:"workspace_gid"`
	ProjectGID   string `json:"project_gid,omitempty"`
	Name         string `json:"name"`
	Notes        string `json:"notes,omitempty"`
	DueOn        string `json:"due_on,omitempty"`
}

type completeArgs struct {
	TaskGID string `json:"task_gid"`
}

func validText(value string, limit int, multiline bool) error {
	if len([]rune(value)) > limit {
		return errors.New("task text exceeds the extension limit")
	}
	for _, r := range value {
		if r == 0 || unicode.IsControl(r) && (!multiline || r != '\n' && r != '\t') {
			return errors.New("task text contains unsupported control characters")
		}
	}
	return nil
}

// The longest task title and workspace name: the validation, the tool schema and the page checks use them.
const (
	maxNameChars          = 500
	maxWorkspaceNameChars = 500
)

func (in createArgs) validate() error {
	if err := validGID(in.WorkspaceGID); err != nil {
		return err
	}
	if in.ProjectGID != "" {
		if err := validGID(in.ProjectGID); err != nil {
			return err
		}
	}
	if strings.TrimSpace(in.Name) == "" || validText(in.Name, maxNameChars, false) != nil || validText(in.Notes, 10000, true) != nil {
		return errors.New("task title must be nonempty, one line and at most 500 characters; notes are at most 10000 characters")
	}
	return validDueDate(in.DueOn)
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	var summary string
	switch name {
	case "asana_create_task":
		var in createArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		summary = "Create Asana task assigned to you\nTitle: " + in.Name + "\nWorkspace GID: " + in.WorkspaceGID
		if in.ProjectGID != "" {
			summary += "\nProject GID: " + in.ProjectGID
		}
		if in.DueOn != "" {
			summary += "\nDue: " + in.DueOn
		}
		if in.Notes != "" {
			summary += "\nNotes: " + in.Notes
		}
	case "asana_complete_task":
		var in completeArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := validGID(in.TaskGID); err != nil {
			return nil, err
		}
		summary = "Mark Asana task " + in.TaskGID + " complete"
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": summary}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	var fn func() (string, error)
	var keyArgs any
	switch name {
	case "asana_create_task":
		var in createArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		if in.ProjectGID != "" {
			project, err := a.client.project(ctx, in.ProjectGID)
			if err != nil {
				return nil, err
			}
			if project.Archived || project.Workspace == nil || project.Workspace.GID != in.WorkspaceGID {
				return nil, errors.New("project must be active and belong to the selected workspace; no task was created")
			}
		}
		keyArgs = in
		fn = func() (string, error) {
			item, err := a.client.createTask(ctx, in.WorkspaceGID, in.ProjectGID, in.Name, in.Notes, in.DueOn)
			if err != nil {
				return "", err
			}
			return "Created Asana task: " + item.Name, nil
		}
	case "asana_complete_task":
		var in completeArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := validGID(in.TaskGID); err != nil {
			return nil, err
		}
		keyArgs = in
		fn = func() (string, error) {
			if _, err := a.client.completeTask(ctx, in.TaskGID); err != nil {
				return "", err
			}
			return "Marked Asana task complete: " + in.TaskGID, nil
		}
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	keyData, _ := json.Marshal(keyArgs)
	text, err := a.receipts.commit(requestID, name+":"+string(keyData), fn)
	if err != nil {
		return nil, err
	}
	a.record(nil)
	if a.hub != nil {
		_ = a.hub.Invalidate(pageUI)
	}
	return map[string]any{"text": text}, nil
}
