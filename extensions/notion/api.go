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
	"strings"
	"time"
)

const apiBase = "https://api.notion.com/v1"
const notionVersion = "2022-06-28"

var (
	errToken       = errors.New("Notion rejected the integration secret or it cannot access that page")
	errUnavailable = errors.New("Notion could not be reached; try again later")
	errNotFound    = errors.New("Notion could not find that shared page")
	errRateLimited = errors.New("Notion rate limit reached; try again later")
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
		return errors.New("Notion integration secret is not configured")
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
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", notionVersion)
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
		return errors.New("Notion returned an oversized response")
	}
	switch res.StatusCode {
	case http.StatusOK, http.StatusCreated:
		if out == nil || len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return errors.New("Notion returned invalid JSON")
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
		return errors.New("Notion rejected the page or block fields")
	default:
		return fmt.Errorf("Notion returned HTTP %d", res.StatusCode)
	}
}

func (c *client) search(ctx context.Context, query, cursor string) (notionList[notionPage], error) {
	in := map[string]any{"filter": map[string]string{"property": "object", "value": "page"}, "page_size": 50, "sort": map[string]string{"direction": "descending", "timestamp": "last_edited_time"}}
	if query != "" {
		in["query"] = query
	}
	if cursor != "" {
		in["start_cursor"] = cursor
	}
	var out notionList[notionPage]
	err := c.do(ctx, http.MethodPost, "/search", nil, in, &out)
	return out, err
}

func (c *client) children(ctx context.Context, id, cursor string) (notionList[block], error) {
	q := url.Values{"page_size": {"100"}}
	if cursor != "" {
		q.Set("start_cursor", cursor)
	}
	var out notionList[block]
	err := c.do(ctx, http.MethodGet, "/blocks/"+url.PathEscape(id)+"/children", q, nil, &out)
	return out, err
}

func (c *client) page(ctx context.Context, id string) (notionPage, error) {
	var out notionPage
	err := c.do(ctx, http.MethodGet, "/pages/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

func (c *client) append(ctx context.Context, id string, children []map[string]any) error {
	return c.do(ctx, http.MethodPatch, "/blocks/"+url.PathEscape(id)+"/children", nil, map[string]any{"children": children}, nil)
}

func (c *client) create(ctx context.Context, parentID, title, content string) (notionPage, error) {
	in := map[string]any{
		"parent":     map[string]string{"type": "page_id", "page_id": parentID},
		"properties": map[string]any{"title": map[string]any{"title": textItems(title)}},
	}
	if content != "" {
		in["children"] = []map[string]any{paragraphRequest(content)}
	}
	var out notionPage
	err := c.do(ctx, http.MethodPost, "/pages", nil, in, &out)
	return out, err
}

func clipText(text string, limit int) string {
	r := []rune(text)
	if len(r) > limit {
		return string(r[:max(0, limit-1)]) + "…"
	}
	return text
}
