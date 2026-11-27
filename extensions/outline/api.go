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
	errToken       = errors.New("Outline rejected the API key; replace it in extension settings")
	errUnavailable = errors.New("Outline could not be reached; try again later")
	errNotFound    = errors.New("Outline could not find that document or collection")
	errRateLimited = errors.New("Outline rate limit reached; try again later")
)

type client struct {
	base, token string
	http        *http.Client
}

func newClient(base, token string) *client {
	base = strings.TrimRight(base, "/")
	if base != "" && !strings.HasSuffix(base, "/api") {
		base += "/api"
	}
	return &client{base: base, token: strings.TrimSpace(token), http: &http.Client{
		Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *client) call(ctx context.Context, path string, in, out any) error {
	if c.token == "" {
		return errors.New("Outline API key is not configured")
	}
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errUnavailable, err)
	}
	defer res.Body.Close()
	data, err = io.ReadAll(io.LimitReader(res.Body, 4<<20+1))
	if err != nil {
		return errUnavailable
	}
	if len(data) > 4<<20 {
		return errors.New("Outline returned an oversized response")
	}
	switch res.StatusCode {
	case http.StatusOK, http.StatusCreated:
	case http.StatusUnauthorized, http.StatusForbidden:
		return errToken
	case http.StatusNotFound:
		return errNotFound
	case http.StatusTooManyRequests:
		return errRateLimited
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return errors.New("Outline rejected the operation; check the fields and permissions")
	default:
		return fmt.Errorf("Outline returned HTTP %d", res.StatusCode)
	}
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &status); err != nil || !status.OK {
		return errors.New("Outline returned an unsuccessful or invalid JSON response")
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return errors.New("Outline returned an invalid response schema")
		}
	}
	return nil
}

func pageParams(offset int) (map[string]any, error) {
	if offset < 0 || offset > 1000000 {
		return nil, errors.New("invalid pagination offset")
	}
	return map[string]any{"offset": offset, "limit": 25}, nil
}

// maxQueryChars is the longest search query, checked by the client, clipped by the page and kept in the page state.
const maxQueryChars = 200

func (c *client) search(ctx context.Context, query string, offset int) (envelope[[]searchResult], error) {
	params, err := pageParams(offset)
	if err != nil {
		return envelope[[]searchResult]{}, err
	}
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > maxQueryChars {
		return envelope[[]searchResult]{}, errors.New("search requires at most 200 characters")
	}
	params["query"] = query
	params["snippetMinWords"] = 10
	params["snippetMaxWords"] = 35
	var out envelope[[]searchResult]
	err = c.call(ctx, "/documents.search", params, &out)
	return out, err
}

func (c *client) document(ctx context.Context, id string) (document, error) {
	if !validDocumentID(id) {
		return document{}, errors.New("invalid Outline document ID")
	}
	var out envelope[document]
	err := c.call(ctx, "/documents.info", map[string]string{"id": id}, &out)
	return out.Data, err
}

func (c *client) collections(ctx context.Context, offset int) (envelope[[]collection], error) {
	params, err := pageParams(offset)
	if err != nil {
		return envelope[[]collection]{}, err
	}
	params["filters"] = []map[string]any{{"field": "archivedAt", "operator": "isNull"}}
	var out envelope[[]collection]
	err = c.call(ctx, "/collections.list", params, &out)
	return out, err
}

func (c *client) create(ctx context.Context, collectionID, title, text string) (document, error) {
	if collectionID != "personal" && !uuidPattern.MatchString(collectionID) {
		return document{}, errors.New("collection must be a discovered UUID or personal")
	}
	var out envelope[document]
	if err := c.call(ctx, "/documents.create", map[string]any{"collectionId": collectionID, "title": title, "text": text, "publish": false}, &out); err != nil {
		return document{}, err
	}
	if out.Data.ID == "" {
		return document{}, errors.New("Outline did not return the created document ID")
	}
	return out.Data, nil
}

func nextOffset(p pagination) (int, bool, error) {
	if p.NextPath == "" {
		return 0, false, nil
	}
	u, err := url.Parse(p.NextPath)
	if err != nil {
		return 0, false, errors.New("Outline returned an invalid nextPath")
	}
	next, err := strconv.Atoi(u.Query().Get("offset"))
	if err != nil || next <= p.Offset || next > 1000000 {
		return 0, false, errors.New("Outline returned a non-advancing nextPath")
	}
	return next, true, nil
}

func clipText(text string, limit int) string {
	r := []rune(text)
	if len(r) > limit {
		return string(r[:max(0, limit-1)]) + "…"
	}
	return text
}
