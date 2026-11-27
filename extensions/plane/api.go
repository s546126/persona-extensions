package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	errToken       = errors.New("Plane rejected the API key; replace it in extension settings")
	errUnavailable = errors.New("Plane could not be reached; try again later")
	errNotFound    = errors.New("Plane could not find that project or work item")
	errRateLimited = errors.New("Plane rate limit reached; try again later")
)

type client struct {
	base, token string
	http        *http.Client
}

func newClient(base, token string) *client {
	return &client{base: strings.TrimRight(base, "/"), token: strings.TrimSpace(token), http: &http.Client{
		Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	if c.token == "" {
		return errors.New("Plane API key is not configured")
	}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if query != nil {
		req.URL.RawQuery = query.Encode()
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errUnavailable, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20+1))
	if err != nil {
		return errUnavailable
	}
	if len(data) > 4<<20 {
		return errors.New("Plane returned an oversized response")
	}
	switch res.StatusCode {
	case http.StatusOK, http.StatusCreated:
		if out == nil || len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return errors.New("Plane returned invalid JSON")
		}
		return nil
	case http.StatusNoContent:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return errToken
	case http.StatusNotFound:
		return errNotFound
	case http.StatusTooManyRequests:
		return errRateLimited
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return errors.New("Plane rejected the operation; check the fields and permissions")
	default:
		return fmt.Errorf("Plane returned HTTP %d", res.StatusCode)
	}
}

var slugPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)
var cursorPattern = regexp.MustCompile(`^[1-9][0-9]{0,2}:[0-9]{1,8}:[01]$`)

func validSlug(value string) error {
	if !slugPattern.MatchString(value) {
		return errors.New("workspace slug must contain only letters, numbers, underscores and hyphens")
	}
	return nil
}

func validID(value string) error {
	if len(value) == 0 || len(value) > 128 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n/?#") {
		return errors.New("project or work item ID is invalid")
	}
	return nil
}

func validCursor(value string) error {
	if value != "" && !cursorPattern.MatchString(value) {
		return errors.New("invalid Plane pagination cursor; use the returned next_cursor")
	}
	return nil
}

func (c *client) projectPath(workspace, project string) string {
	return "/workspaces/" + url.PathEscape(workspace) + "/projects/" + url.PathEscape(project)
}

func (c *client) itemPath(workspace, project, id string) string {
	return c.projectPath(workspace, project) + "/work-items/" + url.PathEscape(id) + "/"
}

func (c *client) projects(ctx context.Context, workspace, cursor string) (page[project], error) {
	if err := validSlug(workspace); err != nil {
		return page[project]{}, err
	}
	if err := validCursor(cursor); err != nil {
		return page[project]{}, err
	}
	q := url.Values{"per_page": {"100"}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var out page[project]
	err := c.do(ctx, http.MethodGet, "/workspaces/"+url.PathEscape(workspace)+"/projects/", q, nil, &out)
	return out, err
}

func (c *client) workItems(ctx context.Context, workspace, project, cursor string) (page[workItem], error) {
	if err := validSlug(workspace); err != nil {
		return page[workItem]{}, err
	}
	if err := validID(project); err != nil {
		return page[workItem]{}, err
	}
	if err := validCursor(cursor); err != nil {
		return page[workItem]{}, err
	}
	q := url.Values{"per_page": {"50"}, "expand": {"state"}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var out page[workItem]
	err := c.do(ctx, http.MethodGet, c.projectPath(workspace, project)+"/work-items/", q, nil, &out)
	return out, err
}

func (c *client) workItem(ctx context.Context, workspace, project, id string) (workItem, error) {
	var out workItem
	if err := validSlug(workspace); err != nil {
		return out, err
	}
	if err := validID(project); err != nil {
		return out, err
	}
	if err := validID(id); err != nil {
		return out, err
	}
	err := c.do(ctx, http.MethodGet, c.itemPath(workspace, project, id), url.Values{"expand": {"state"}}, nil, &out)
	return out, err
}

func (c *client) states(ctx context.Context, workspace, project string) ([]workflowState, error) {
	if err := validSlug(workspace); err != nil {
		return nil, err
	}
	if err := validID(project); err != nil {
		return nil, err
	}
	var out []workflowState
	seen := map[string]bool{"": true}
	const maxStatePages = 20
	for cursor, pages := "", 0; ; pages++ {
		if pages >= maxStatePages {
			return nil, errors.New("Plane returned too many state pages")
		}
		if err := validCursor(cursor); err != nil {
			return nil, err
		}
		q := url.Values{"per_page": {"100"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page page[workflowState]
		if err := c.do(ctx, http.MethodGet, c.projectPath(workspace, project)+"/states/", q, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Results...)
		if !page.NextPageResults {
			return out, nil
		}
		if page.NextCursor == "" || seen[page.NextCursor] {
			return nil, errors.New("Plane returned a stuck state pagination cursor")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

func (c *client) create(ctx context.Context, workspace, project string, body map[string]any) (workItem, error) {
	var out workItem
	if err := validSlug(workspace); err != nil {
		return out, err
	}
	if err := validID(project); err != nil {
		return out, err
	}
	err := c.do(ctx, http.MethodPost, c.projectPath(workspace, project)+"/work-items/", nil, body, &out)
	return out, err
}

func (c *client) update(ctx context.Context, workspace, project, id string, body map[string]any) (workItem, error) {
	var out workItem
	if err := validSlug(workspace); err != nil {
		return out, err
	}
	if err := validID(project); err != nil {
		return out, err
	}
	if err := validID(id); err != nil {
		return out, err
	}
	err := c.do(ctx, http.MethodPatch, c.itemPath(workspace, project, id), nil, body, &out)
	return out, err
}

func (c *client) itemURL(item workItem) string {
	u, err := url.Parse(item.WebURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return ""
	}
	return item.WebURL
}

func clipText(text string, limit int) string {
	r := []rune(text)
	if len(r) > limit {
		return string(r[:max(0, limit-1)]) + "…"
	}
	return text
}
