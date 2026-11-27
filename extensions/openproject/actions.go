package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// maxSubjectChars and maxDescriptionChars bound work package fields in characters.
const (
	maxSubjectChars     = 255
	maxDescriptionChars = 30000
)

type createArgs struct {
	ProjectHref string `json:"project_href"`
	TypeHref    string `json:"type_href"`
	Subject     string `json:"subject"`
	Description string `json:"description,omitempty"`
	StartDate   string `json:"start_date,omitempty"`
	DueDate     string `json:"due_date,omitempty"`
}

var (
	projectHrefPattern = regexp.MustCompile(`^/api/v3/projects/[1-9][0-9]*$`)
	typeHrefPattern    = regexp.MustCompile(`^/api/v3/types/[1-9][0-9]*$`)
)

func (in createArgs) validate() error {
	if !projectHrefPattern.MatchString(in.ProjectHref) || !typeHrefPattern.MatchString(in.TypeHref) {
		return errors.New("project and type must be exact numeric HAL links from OpenProject discovery")
	}
	if strings.TrimSpace(in.Subject) == "" || len([]rune(in.Subject)) > maxSubjectChars || len([]rune(in.Description)) > maxDescriptionChars {
		return errors.New("subject must be nonempty and at most 255 characters; description is at most 30000 characters")
	}
	for _, r := range in.Subject {
		if r == 0 || unicode.IsControl(r) {
			return errors.New("subject contains unsupported control characters")
		}
	}
	for _, r := range in.Description {
		if r == 0 || unicode.IsControl(r) && r != '\n' && r != '\t' {
			return errors.New("description contains unsupported control characters")
		}
	}
	for _, value := range []string{in.StartDate, in.DueDate} {
		if value != "" {
			if _, err := time.Parse("2006-01-02", value); err != nil {
				return errors.New("start/due dates must be valid YYYY-MM-DD dates")
			}
		}
	}
	if in.StartDate != "" && in.DueDate != "" && in.StartDate > in.DueDate {
		return errors.New("start date must not be after due date")
	}
	return nil
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	if name != "openproject_create_work_package" {
		return nil, fmt.Errorf("unknown action %q", name)
	}
	var in createArgs
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	summary := "Create OpenProject work package\nProject: " + in.ProjectHref + "\nType: " + in.TypeHref + "\nSubject: " + in.Subject
	if in.Description != "" {
		summary += "\nDescription: " + in.Description
	}
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": summary}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	if name != "openproject_create_work_package" {
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
		item, err := a.client.createWorkPackage(ctx, in.ProjectHref, in.TypeHref, in.Subject, in.Description, in.StartDate, in.DueDate)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Created OpenProject work package #%d: %s", item.ID, item.Subject), nil
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
