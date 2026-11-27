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
	ActivityID  int64  `json:"activity_id"`
	Begin       string `json:"begin"`
	End         string `json:"end,omitempty"`
	Description string `json:"description,omitempty"`
}

func (in createArgs) validate() error {
	if in.ProjectID <= 0 || in.ActivityID <= 0 || len([]rune(in.Description)) > 5000 {
		return errors.New("discovered project/activity IDs and a bounded description are required")
	}
	begin, err := time.Parse(time.RFC3339, in.Begin)
	if err != nil {
		return errors.New("begin must be an RFC3339 timestamp with a timezone")
	}
	if in.End != "" {
		end, err := time.Parse(time.RFC3339, in.End)
		if err != nil || end.Before(begin) {
			return errors.New("end must be an RFC3339 timestamp not before begin")
		}
	}
	if strings.ContainsFunc(in.Description, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return errors.New("description contains unsupported control characters")
	}
	return nil
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	var summary string
	switch name {
	case "kimai_create_timesheet":
		var in createArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		summary = fmt.Sprintf("Create Kimai time entry\nProject: %d\nActivity: %d\nBegin: %s", in.ProjectID, in.ActivityID, in.Begin)
		if in.End == "" {
			summary += "\nEnd: running timer"
		} else {
			summary += "\nEnd: " + in.End
		}
		if in.Description != "" {
			summary += "\nDescription: " + in.Description
		}
	case "kimai_stop_timesheet":
		var in struct {
			TimesheetID int64 `json:"timesheet_id"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if in.TimesheetID <= 0 {
			return nil, errors.New("timesheet ID must be positive")
		}
		summary = fmt.Sprintf("Stop Kimai timer #%d", in.TimesheetID)
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": summary}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	var text string
	var err error
	switch name {
	case "kimai_create_timesheet":
		var in createArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		var begin, end string
		if begin, end, err = a.client.localTimes(ctx, in.Begin, in.End); err != nil {
			return nil, err
		}
		keyData, _ := json.Marshal(in)
		text, err = a.receipts.commit(requestID, name+":"+string(keyData), func() (string, error) {
			item, err := a.client.createTimesheet(ctx, in.ProjectID, in.ActivityID, begin, end, in.Description)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Created Kimai time entry #%d", item.ID), nil
		})
	case "kimai_stop_timesheet":
		var in struct {
			TimesheetID int64 `json:"timesheet_id"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if in.TimesheetID <= 0 {
			return nil, errors.New("timesheet ID must be positive")
		}
		keyData, _ := json.Marshal(in)
		text, err = a.receipts.commit(requestID, name+":"+string(keyData), func() (string, error) {
			if _, err := a.client.stopTimesheet(ctx, in.TimesheetID); err != nil {
				return "", err
			}
			return fmt.Sprintf("Stopped Kimai timer #%d", in.TimesheetID), nil
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
