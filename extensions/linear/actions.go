package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type createArgs struct {
	TeamID      string `json:"team_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	AssigneeID  string `json:"assignee_id"`
	Priority    *int   `json:"priority"`
	StateID     string `json:"state_id"`
}

type updateArgs struct {
	ID          string  `json:"id"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Priority    *int    `json:"priority"`
	StateID     *string `json:"state_id"`
	AssigneeID  *string `json:"assignee_id"`
}

func validText(text string, max int, multiline bool) error {
	if len([]rune(text)) > max {
		return errors.New("Linear text exceeds its documented limit")
	}
	for _, r := range text {
		if r == 0 || r < 32 && (!multiline || r != '\n' && r != '\t') {
			return errors.New("Linear text contains control characters")
		}
	}
	return nil
}

func (a *app) teamsRead(ctx context.Context) ([]team, error) {
	a.mu.RLock()
	items, at := a.teams, a.teamsAt
	a.mu.RUnlock()
	if !at.IsZero() && time.Since(at) < 2*time.Minute {
		return items, nil
	}
	items, err := a.client.teams(ctx)
	a.record(err)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.teams, a.teamsAt = items, time.Now()
	a.mu.Unlock()
	return items, nil
}

func validState(teams []team, teamID, stateID string) error {
	for _, t := range teams {
		if t.ID != teamID {
			continue
		}
		for _, s := range t.States.Nodes {
			if s.ID == stateID {
				return nil
			}
		}
		return errors.New("workflow state does not belong to the issue's team")
	}
	return errors.New("team not found; read Linear teams again")
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	var summary string
	switch name {
	case "linear_create":
		var in createArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := validID(in.TeamID); err != nil {
			return nil, err
		}
		if err := validText(in.Title, 255, false); err != nil || strings.TrimSpace(in.Title) == "" {
			return nil, errors.New("title is required and at most 255 characters")
		}
		if err := validText(in.Description, 100000, true); err != nil {
			return nil, err
		}
		if in.Priority != nil && (*in.Priority < 0 || *in.Priority > 4) {
			return nil, errors.New("priority must be 0–4")
		}
		if in.AssigneeID != "" {
			if err := validID(in.AssigneeID); err != nil {
				return nil, err
			}
		}
		if in.StateID != "" {
			if err := validID(in.StateID); err != nil {
				return nil, err
			}
		}
		summary = "Create Linear issue: " + in.Title + "\nTeam ID: " + in.TeamID
	case "linear_update":
		var in updateArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		data, _ := json.Marshal(in)
		summary = "Update Linear issue " + in.ID + "\nOnly these fields: " + string(data)
	case "linear_comment":
		var in struct{ ID, Body string }
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := validID(in.ID); err != nil {
			return nil, err
		}
		if err := validText(in.Body, 100000, true); err != nil || strings.TrimSpace(in.Body) == "" {
			return nil, errors.New("comment body is required")
		}
		summary = "Comment on Linear issue " + in.ID + ":\n" + in.Body
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": summary}, nil
}

func (in updateArgs) validate() error {
	if err := validID(in.ID); err != nil {
		return err
	}
	if in.Title != nil {
		if err := validText(*in.Title, 255, false); err != nil || strings.TrimSpace(*in.Title) == "" {
			return errors.New("invalid title")
		}
	}
	if in.Description != nil {
		if err := validText(*in.Description, 100000, true); err != nil {
			return err
		}
	}
	if in.Priority != nil && (*in.Priority < 0 || *in.Priority > 4) {
		return errors.New("priority must be 0–4")
	}
	if in.StateID != nil {
		if err := validID(*in.StateID); err != nil {
			return err
		}
	}
	if in.AssigneeID != nil {
		if err := validID(*in.AssigneeID); err != nil {
			return err
		}
	}
	if in.Title == nil && in.Description == nil && in.Priority == nil && in.StateID == nil && in.AssigneeID == nil {
		return errors.New("supply at least one field to change; omitted fields remain unchanged")
	}
	return nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	var key string
	var fn func() (string, error)
	switch name {
	case "linear_create":
		var in createArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := validID(in.TeamID); err != nil {
			return nil, err
		}
		if err := validText(in.Title, 255, false); err != nil || strings.TrimSpace(in.Title) == "" {
			return nil, errors.New("invalid title")
		}
		if err := validText(in.Description, 100000, true); err != nil {
			return nil, err
		}
		if in.Priority != nil && (*in.Priority < 0 || *in.Priority > 4) {
			return nil, errors.New("invalid priority")
		}
		input := map[string]any{"teamId": in.TeamID, "title": in.Title}
		if in.Description != "" {
			input["description"] = in.Description
		}
		if in.AssigneeID != "" {
			if err := validID(in.AssigneeID); err != nil {
				return nil, err
			}
			input["assigneeId"] = in.AssigneeID
		}
		if in.Priority != nil {
			input["priority"] = *in.Priority
		}
		if in.StateID != "" {
			if err := validID(in.StateID); err != nil {
				return nil, err
			}
			teams, err := a.teamsRead(ctx)
			if err != nil {
				return nil, err
			}
			if err := validState(teams, in.TeamID, in.StateID); err != nil {
				return nil, err
			}
			input["stateId"] = in.StateID
		}
		keyData, _ := json.Marshal(map[string]any{"action": name, "input": input})
		key = string(keyData)
		fn = func() (string, error) {
			item, err := a.client.create(ctx, input)
			if err != nil {
				return "", err
			}
			if item.ID == "" {
				return "", errors.New("Linear returned no issue ID")
			}
			return "Created Linear issue " + item.Identifier + ": " + item.Title, nil
		}
	case "linear_update":
		var in updateArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		input := map[string]any{}
		if in.Title != nil {
			input["title"] = *in.Title
		}
		if in.Description != nil {
			input["description"] = *in.Description
		}
		if in.Priority != nil {
			input["priority"] = *in.Priority
		}
		if in.AssigneeID != nil {
			input["assigneeId"] = *in.AssigneeID
		}
		if in.StateID != nil {
			item, err := a.client.issue(ctx, in.ID)
			if err != nil {
				return nil, err
			}
			teams, err := a.teamsRead(ctx)
			if err != nil {
				return nil, err
			}
			if err := validState(teams, item.Team.ID, *in.StateID); err != nil {
				return nil, err
			}
			input["stateId"] = *in.StateID
		}
		keyData, _ := json.Marshal(map[string]any{"action": name, "id": in.ID, "input": input})
		key = string(keyData)
		fn = func() (string, error) {
			if err := a.client.update(ctx, in.ID, input); err != nil {
				return "", err
			}
			return "Updated Linear issue " + in.ID, nil
		}
	case "linear_comment":
		var in struct{ ID, Body string }
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := validID(in.ID); err != nil {
			return nil, err
		}
		if err := validText(in.Body, 100000, true); err != nil || strings.TrimSpace(in.Body) == "" {
			return nil, errors.New("comment body is required")
		}
		keyData, _ := json.Marshal(map[string]any{"action": name, "id": in.ID, "body": in.Body})
		key = string(keyData)
		fn = func() (string, error) {
			if err := a.client.comment(ctx, in.ID, in.Body); err != nil {
				return "", err
			}
			return "Commented on Linear issue " + in.ID, nil
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
