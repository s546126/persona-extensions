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
	WorkspaceSlug string `json:"workspace_slug,omitempty"`
	ProjectID     string `json:"project_id"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	Priority      string `json:"priority,omitempty"`
	TargetDate    string `json:"target_date,omitempty"`
	StateID       string `json:"state_id,omitempty"`
}

type updateArgs struct {
	WorkspaceSlug string `json:"workspace_slug,omitempty"`
	ProjectID     string `json:"project_id"`
	WorkItemID    string `json:"work_item_id"`
	Priority      string `json:"priority,omitempty"`
	StateID       string `json:"state_id,omitempty"`
}

const (
	maxProjectNameChars = 255
	maxNameChars        = 255
	maxDescriptionChars = 10000
)

func validText(value string, limit int, multiline bool) error {
	if len([]rune(value)) > limit {
		return errors.New("work item text exceeds the extension limit")
	}
	for _, r := range value {
		if r == 0 || unicode.IsControl(r) && (!multiline || r != '\n' && r != '\t') {
			return errors.New("work item text contains unsupported control characters")
		}
	}
	return nil
}

func (in createArgs) validate() error {
	if err := validID(in.ProjectID); err != nil {
		return err
	}
	if strings.TrimSpace(in.Name) == "" || validText(in.Name, maxNameChars, false) != nil || validText(in.Description, maxDescriptionChars, true) != nil {
		return errors.New("name must be nonempty, one line and at most 255 characters; description is at most 10000 characters")
	}
	if in.Priority != "" && !validPriority(in.Priority) {
		return errors.New("invalid priority")
	}
	if err := validDate(in.TargetDate); err != nil {
		return err
	}
	if in.StateID != "" {
		return validID(in.StateID)
	}
	return nil
}

func (in updateArgs) validate() error {
	if err := validID(in.ProjectID); err != nil {
		return err
	}
	if err := validID(in.WorkItemID); err != nil {
		return err
	}
	if in.Priority == "" && in.StateID == "" {
		return errors.New("at least one of priority or state_id is required")
	}
	if in.Priority != "" && !validPriority(in.Priority) {
		return errors.New("invalid priority")
	}
	if in.StateID != "" {
		return validID(in.StateID)
	}
	return nil
}

func (in createArgs) body() map[string]any {
	out := map[string]any{"name": in.Name}
	if in.Description != "" {
		out["description_html"] = htmlFromPlain(in.Description)
	}
	if in.Priority != "" {
		out["priority"] = in.Priority
	}
	if in.TargetDate != "" {
		out["target_date"] = in.TargetDate
	}
	if in.StateID != "" {
		out["state"] = in.StateID
	}
	return out
}

func (in updateArgs) body() map[string]any {
	out := map[string]any{}
	if in.Priority != "" {
		out["priority"] = in.Priority
	}
	if in.StateID != "" {
		out["state"] = in.StateID
	}
	return out
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	var summary string
	switch name {
	case "plane_create_work_item":
		var in createArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		if in.WorkspaceSlug != "" {
			if err := validSlug(in.WorkspaceSlug); err != nil {
				return nil, err
			}
		}
		summary = "Create Plane work item in project " + in.ProjectID + "\nTitle: " + in.Name
		if in.Description != "" {
			summary += "\nDescription: " + in.Description
		}
		if in.Priority != "" {
			summary += "\nPriority: " + in.Priority
		}
		if in.TargetDate != "" {
			summary += "\nDue: " + in.TargetDate
		}
	case "plane_update_work_item":
		var in updateArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		if in.WorkspaceSlug != "" {
			if err := validSlug(in.WorkspaceSlug); err != nil {
				return nil, err
			}
		}
		summary = "Update Plane work item " + in.WorkItemID + " in project " + in.ProjectID
		if in.Priority != "" {
			summary += "\nPriority: " + in.Priority
		}
		if in.StateID != "" {
			summary += "\nState ID: " + in.StateID
		}
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": summary}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	var workspace string
	var body map[string]any
	var keyArgs any
	var fn func() (string, error)
	switch name {
	case "plane_create_work_item":
		var in createArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		var err error
		workspace, err = a.workspaceName(in.WorkspaceSlug)
		if err != nil {
			return nil, err
		}
		if in.StateID != "" {
			if err := a.checkState(ctx, workspace, in.ProjectID, in.StateID); err != nil {
				return nil, err
			}
		}
		body = in.body()
		keyArgs = in
		fn = func() (string, error) {
			item, err := a.client.create(ctx, workspace, in.ProjectID, body)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Created Plane work item #%d: %s", item.SequenceID, item.Name), nil
		}
	case "plane_update_work_item":
		var in updateArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		var err error
		workspace, err = a.workspaceName(in.WorkspaceSlug)
		if err != nil {
			return nil, err
		}
		if in.StateID != "" {
			if err := a.checkState(ctx, workspace, in.ProjectID, in.StateID); err != nil {
				return nil, err
			}
		}
		body = in.body()
		keyArgs = in
		fn = func() (string, error) {
			if _, err := a.client.update(ctx, workspace, in.ProjectID, in.WorkItemID, body); err != nil {
				return "", err
			}
			return "Updated Plane work item " + in.WorkItemID, nil
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

func (a *app) checkState(ctx context.Context, workspace, project, id string) error {
	states, err := a.client.states(ctx, workspace, project)
	if err != nil {
		return err
	}
	for _, state := range states {
		if state.ID == id {
			return nil
		}
	}
	return errors.New("state ID does not belong to this Plane project; no work item was changed")
}
