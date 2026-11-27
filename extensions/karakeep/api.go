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
	"strconv"
	"strings"
	"time"
)

var (
	errToken       = errors.New("Karakeep rejected the API key; replace it in extension settings")
	errUnavailable = errors.New("Karakeep could not be reached; try again later")
	errNotFound    = errors.New("Karakeep could not find that bookmark")
	errRateLimited = errors.New("Karakeep rate limit reached; try again later")
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
		return errors.New("Karakeep API key is not configured")
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
		return errors.New("Karakeep returned an oversized response")
	}
	switch res.StatusCode {
	case http.StatusOK, http.StatusCreated:
		if out == nil || len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return errors.New("Karakeep returned invalid JSON")
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
		return errors.New("Karakeep rejected the bookmark fields")
	default:
		return fmt.Errorf("Karakeep returned HTTP %d", res.StatusCode)
	}
}

// bookmarks requests exactly limit items so nextCursor continues right after
// the last one shown.
func (c *client) bookmarks(ctx context.Context, query, cursor string, limit int) (bookmarkPage, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if query != "" {
		q.Set("q", query)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var out bookmarkPage
	path := "/bookmarks"
	if query != "" {
		path = "/bookmarks/search"
	}
	err := c.do(ctx, http.MethodGet, path, q, nil, &out)
	return out, err
}

func (c *client) bookmark(ctx context.Context, id string) (bookmark, error) {
	var out bookmark
	err := c.do(ctx, http.MethodGet, "/bookmarks/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

func (c *client) lists(ctx context.Context) ([]bookmarkList, error) {
	var out struct {
		Lists []bookmarkList `json:"lists"`
	}
	err := c.do(ctx, http.MethodGet, "/lists", nil, nil, &out)
	return out.Lists, err
}

func (c *client) bookmarkHighlights(ctx context.Context, id string) ([]highlight, error) {
	var out struct {
		Highlights []highlight `json:"highlights"`
	}
	err := c.do(ctx, http.MethodGet, "/bookmarks/"+url.PathEscape(id)+"/highlights", nil, nil, &out)
	return out.Highlights, err
}

func (c *client) save(ctx context.Context, rawURL string) (bookmark, error) {
	var out bookmark
	err := c.do(ctx, http.MethodPost, "/bookmarks", nil, map[string]any{"type": "link", "url": rawURL}, &out)
	return out, err
}

func (c *client) update(ctx context.Context, id string, patch updatePatch) error {
	return c.do(ctx, http.MethodPatch, "/bookmarks/"+url.PathEscape(id), nil, patch, nil)
}

func clipText(text string, limit int) string {
	r := []rune(text)
	if len(r) > limit {
		return string(r[:max(0, limit-1)]) + "…"
	}
	return text
}
