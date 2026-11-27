package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

type reviewArgs struct {
	Project string `json:"project"`
	IID     int64  `json:"iid"`
}

type commentArgs struct {
	Project string `json:"project"`
	IID     int64  `json:"iid"`
	Body    string `json:"body"`
}

func (in commentArgs) validate() error {
	if err := (reviewArgs{Project: in.Project, IID: in.IID}).validate(); err != nil {
		return err
	}
	return validComment(in.Body)
}

func (in reviewArgs) validate() error {
	if err := validProjectID(in.Project); err != nil {
		return err
	}
	if in.IID <= 0 {
		return errors.New("merge request IID must be positive")
	}
	return nil
}

func validComment(body string) error {
	if strings.TrimSpace(body) == "" || len([]rune(body)) > 10000 {
		return errors.New("comment must be nonempty and at most 10000 characters")
	}
	for _, r := range body {
		if r == 0 || unicode.IsControl(r) && r != '\n' && r != '\t' {
			return errors.New("comment contains unsupported control characters")
		}
	}
	return nil
}

func describeAction(name string, raw json.RawMessage) (map[string]any, error) {
	if name == "gitlab_comment" {
		var in commentArgs
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
		return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": fmt.Sprintf("Comment on MR !%d in project %s\n\n%s", in.IID, in.Project, in.Body)}, nil
	}
	var in reviewArgs
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	var summary string
	switch name {
	case "gitlab_approve":
		summary = fmt.Sprintf("Approve MR !%d in project %s", in.IID, in.Project)
	case "gitlab_unapprove":
		summary = fmt.Sprintf("Remove approval from MR !%d in project %s", in.IID, in.Project)
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	return map[string]any{"action": map[string]any{"name": name, "visible": "owner"}, "summary": summary}, nil
}

func (a *app) doAction(ctx context.Context, requestID, name string, raw json.RawMessage) (map[string]any, error) {
	var in reviewArgs
	var body string
	if name == "gitlab_comment" {
		var comment commentArgs
		if err := decode(raw, &comment); err != nil {
			return nil, err
		}
		if err := comment.validate(); err != nil {
			return nil, err
		}
		in, body = reviewArgs{Project: comment.Project, IID: comment.IID}, comment.Body
	} else {
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if err := in.validate(); err != nil {
			return nil, err
		}
	}
	var keyData []byte
	if name == "gitlab_comment" {
		keyData, _ = json.Marshal(struct {
			Action  string `json:"action"`
			Project string `json:"project"`
			IID     int64  `json:"iid"`
			Body    string `json:"body"`
		}{name, in.Project, in.IID, body})
	} else {
		keyData, _ = json.Marshal(struct {
			Action  string `json:"action"`
			Project string `json:"project"`
			IID     int64  `json:"iid"`
		}{name, in.Project, in.IID})
	}
	c, hub, receipts := a.deps()
	var fn func() (string, error)
	switch name {
	case "gitlab_approve":
		fn = func() (string, error) {
			if err := c.approve(ctx, in.Project, in.IID); err != nil {
				return "", err
			}
			return fmt.Sprintf("Approved MR !%d", in.IID), nil
		}
	case "gitlab_unapprove":
		fn = func() (string, error) {
			if err := c.unapprove(ctx, in.Project, in.IID); err != nil {
				return "", err
			}
			return fmt.Sprintf("Removed approval from MR !%d", in.IID), nil
		}
	case "gitlab_comment":
		fn = func() (string, error) {
			if err := c.comment(ctx, in.Project, in.IID, body); err != nil {
				return "", err
			}
			return fmt.Sprintf("Commented on MR !%d", in.IID), nil
		}
	default:
		return nil, fmt.Errorf("unknown action %q", name)
	}
	text, err := receipts.commit(requestID, string(keyData), fn)
	if err != nil {
		return nil, err
	}
	a.record(nil)
	if hub != nil {
		_ = hub.Invalidate(pageUI)
	}
	return map[string]any{"text": text}, nil
}
