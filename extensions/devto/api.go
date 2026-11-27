package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiBase = "https://dev.to/api"

var errUnavailable = errors.New("DEV Community could not be reached; try again later")
var errNotFound = errors.New("DEV Community could not find that article")

type client struct {
	base  string
	http  *http.Client
	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	data []byte
	at   time.Time
}

func newClient(base string) *client {
	return &client{base: strings.TrimRight(base, "/"), http: &http.Client{
		Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, cache: map[string]cached{}}
}

func (c *client) get(ctx context.Context, path string, fresh time.Duration) ([]byte, error) {
	data, freshHit, err := c.fetch(ctx, path, fresh)
	if err != nil {
		return data, err
	}
	if !freshHit {
		c.storeCache(path, data)
	}
	return data, nil
}

func (c *client) fetch(ctx context.Context, path string, fresh time.Duration) ([]byte, bool, error) {
	c.mu.Lock()
	old, ok := c.cache[path]
	c.mu.Unlock()
	if ok && time.Since(old.at) < fresh {
		return old.data, true, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		if ok {
			return old.data, false, fmt.Errorf("cached DEV data is stale: %w", errUnavailable)
		}
		return nil, false, fmt.Errorf("%w: %v", errUnavailable, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20+1))
	if err != nil {
		return nil, false, errUnavailable
	}
	if len(data) > 4<<20 {
		return nil, false, errors.New("DEV returned an oversized response")
	}
	if res.StatusCode == http.StatusNotFound {
		return nil, false, errNotFound
	}
	if res.StatusCode != http.StatusOK {
		if ok {
			return old.data, false, fmt.Errorf("cached DEV data is stale: HTTP %d", res.StatusCode)
		}
		return nil, false, fmt.Errorf("DEV returned HTTP %d", res.StatusCode)
	}
	if !json.Valid(data) {
		return nil, false, errors.New("DEV returned invalid JSON")
	}
	return data, false, nil
}

func (c *client) storeCache(path string, data []byte) {
	c.mu.Lock()
	c.cache[path] = cached{data: data, at: time.Now()}
	if len(c.cache) > 64 {
		var oldest string
		var at time.Time
		for path, entry := range c.cache {
			if oldest == "" || entry.at.Before(at) {
				oldest, at = path, entry.at
			}
		}
		delete(c.cache, oldest)
	}
	c.mu.Unlock()
}

func (c *client) getJSON(ctx context.Context, path string, fresh time.Duration, out any, badShape string) error {
	data, freshHit, err := c.fetch(ctx, path, fresh)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errors.New(badShape)
	}
	if !freshHit {
		c.storeCache(path, data)
	}
	return nil
}

func (c *client) articles(ctx context.Context, tag string, page int) ([]article, error) {
	if page < 1 || page > 20 {
		return nil, errors.New("page must be between 1 and 20")
	}
	q := url.Values{"per_page": {"30"}, "page": {strconv.Itoa(page)}}
	if tag != "" {
		q.Set("tag", tag)
	}
	var out []article
	if err := c.getJSON(ctx, "/articles?"+q.Encode(), 2*time.Minute, &out, "DEV returned an unexpected article list"); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *client) article(ctx context.Context, id int64) (article, error) {
	if id <= 0 {
		return article{}, errors.New("invalid article ID")
	}
	var out article
	if err := c.getJSON(ctx, "/articles/"+strconv.FormatInt(id, 10), 5*time.Minute, &out, "DEV returned an unexpected article"); err != nil {
		return article{}, err
	}
	return out, nil
}

func (c *client) comments(ctx context.Context, id int64) ([]comment, error) {
	var out []comment
	if err := c.getJSON(ctx, "/comments?a_id="+strconv.FormatInt(id, 10), 5*time.Minute, &out, "DEV returned an unexpected comment thread"); err != nil {
		return nil, err
	}
	return out, nil
}

func clipText(text string, limit int) string {
	r := []rune(text)
	if len(r) > limit {
		return string(r[:max(0, limit-1)]) + "…"
	}
	return text
}
