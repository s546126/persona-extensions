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
	"sort"
	"strconv"
	"strings"
	"time"
)

// Track is the core's stable, headless space record. The extension never
// duplicates it in a private data file.
type Track struct {
	Slug              string   `json:"slug"`
	Kind              string   `json:"kind"`
	Title             string   `json:"title"`
	Icon              string   `json:"icon"`
	Description       string   `json:"description"`
	DefaultCategories []string `json:"default_categories"`
	Archived          bool     `json:"archived"`
	Builtin           bool     `json:"builtin"`
	Revision          int64    `json:"revision"`
	Config            struct {
		Sidebar struct {
			Group string `json:"group"`
			Icon  string `json:"icon"`
		} `json:"sidebar"`
	} `json:"config"`
}

type Conversation struct {
	SessionID string `json:"session_id"`
	MachineID string `json:"machine_id"`
	Title     string `json:"title"`
	LastText  string `json:"last_text"`
	UpdatedAt string `json:"updated_at"`
	Space     string `json:"space"`
}

type SpaceMD struct {
	Content string `json:"content"`
	Digest  string `json:"digest"`
	State   string `json:"state"`
}

// TrackLaunchers is GET /api/v1/assistant/tracks/launchers's answer.
type TrackLaunchers struct {
	Default   TrackLauncherDefault `json:"default"`
	Launchers []TrackLauncher      `json:"launchers"`
}

type TrackLauncherDefault struct {
	Launcher string `json:"launcher,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

type TrackLauncher struct {
	Name         string   `json:"name"`
	DisplayName  string   `json:"display_name,omitempty"`
	Agent        string   `json:"agent,omitempty"`
	DefaultModel string   `json:"default_model,omitempty"`
	Models       []string `json:"models,omitempty"`
}

// StartOptions holds optional launcher, model and effort overrides for a new space conversation.
type StartOptions struct {
	Launcher string
	Model    string
	Effort   string
}

// HTTPError is a hub answer outside 2xx, with its status and refused scope kept so callers can tell an older hub apart.
type HTTPError struct {
	Status  int
	Message string
	// Scope is the permission a 403 names as missing; empty when the hub refused a route it does not know.
	Scope string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Hub 返回 HTTP %d：%s", e.Status, e.Message)
}

// missingRoute reports whether err is a hub that does not have the route: the hub refuses an extension's call to a
// route it does not know with 403 and no scope, and an older assistant manager behind a newer hub answers 404 or 405.
func missingRoute(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) {
		return false
	}
	switch he.Status {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return true
	case http.StatusForbidden:
		return he.Scope == ""
	}
	return false
}

type Client struct {
	base, token string
	http        *http.Client
}

func NewClient(base, token string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	if c == nil || c.base == "" || c.token == "" {
		return errors.New("空间对话：Hub 尚未提供访问凭据")
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
			Scope   string `json:"scope"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error
		if msg == "" {
			msg = e.Message
		}
		return &HTTPError{Status: resp.StatusCode, Message: msg, Scope: e.Scope}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *Client) Tracks(ctx context.Context) ([]Track, error) {
	var out struct {
		Tracks []Track `json:"tracks"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/assistant/tracks", nil, &out); err != nil {
		return nil, err
	}
	return out.Tracks, nil
}

func (c *Client) Conversations(ctx context.Context, slug string) (string, []Conversation, error) {
	var out struct {
		MachineID     string         `json:"machine_id"`
		Conversations []Conversation `json:"conversations"`
	}
	err := c.call(ctx, http.MethodGet, "/api/v1/assistant?space="+url.QueryEscape(slug), nil, &out)
	return out.MachineID, out.Conversations, err
}

// SpaceConversations lists a space's open conversations, newest first, with how many it has before limit. A hub
// without the scoped route (missingRoute) is asked through the older assistant listing, whose length is the total.
func (c *Client) SpaceConversations(ctx context.Context, slug string, limit int) (string, int, []Conversation, error) {
	var out struct {
		MachineID     string         `json:"machine_id"`
		Total         int            `json:"total"`
		Conversations []Conversation `json:"conversations"`
	}
	path := "/api/v1/assistant/tracks/" + url.PathEscape(slug) + "/conversations"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	err := c.call(ctx, http.MethodGet, path, nil, &out)
	if missingRoute(err) {
		machine, list, err := c.Conversations(ctx, slug)
		if err != nil {
			return "", 0, nil, err
		}
		sortNewestFirst(list)
		total := len(list)
		if limit > 0 && len(list) > limit {
			list = list[:limit]
		}
		return machine, total, list, nil
	}
	if err != nil {
		return "", 0, nil, err
	}
	sortNewestFirst(out.Conversations)
	return out.MachineID, max(out.Total, len(out.Conversations)), out.Conversations, nil
}

// Updated is when the conversation last changed; the zero time when the hub did not say or wrote it otherwise.
func (c Conversation) Updated() time.Time {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(c.UpdatedAt))
	if err != nil {
		return time.Time{}
	}
	return t
}

// sortNewestFirst orders conversations by updated_at, newest first; those without a time keep their order, last.
func sortNewestFirst(list []Conversation) {
	sort.SliceStable(list, func(i, j int) bool { return list[i].Updated().After(list[j].Updated()) })
}

func (c *Client) SpaceMD(ctx context.Context, slug string) (SpaceMD, error) {
	var out SpaceMD
	err := c.call(ctx, http.MethodGet, "/api/v1/assistant/tracks/"+url.PathEscape(slug)+"/space-md", nil, &out)
	return out, err
}

func (c *Client) PutSpaceMD(ctx context.Context, slug, content, digest string) error {
	return c.call(ctx, http.MethodPut, "/api/v1/assistant/tracks/"+url.PathEscape(slug)+"/space-md", map[string]string{"content": content, "base_digest": digest}, nil)
}

func (c *Client) Create(ctx context.Context, title, slug, description string, categories []string) (Track, error) {
	var out Track
	err := c.call(ctx, http.MethodPost, "/api/v1/assistant/tracks", map[string]any{
		"kind": "space", "slug": slug, "title": title, "description": description, "default_categories": categories,
	}, &out)
	return out, err
}

func (c *Client) Archive(ctx context.Context, track Track, archived bool) error {
	return c.call(ctx, http.MethodPut, "/api/v1/assistant/tracks/"+url.PathEscape(track.Slug), map[string]any{"revision": track.Revision, "archived": archived}, nil)
}

// TrackLaunchers reads the healthy assistant launchers available for starting a space conversation.
// Older hubs that do not have the route answer 403/404/405; callers treat that as no choices.
func (c *Client) TrackLaunchers(ctx context.Context) (TrackLaunchers, error) {
	var out TrackLaunchers
	err := c.call(ctx, http.MethodGet, "/api/v1/assistant/tracks/launchers", nil, &out)
	return out, err
}

func (c *Client) Start(ctx context.Context, slug, prompt string, opts StartOptions) (string, string, error) {
	var out struct {
		Session Conversation `json:"session"`
	}
	body := map[string]any{"space": slug, "prompt": prompt}
	if v := strings.TrimSpace(opts.Launcher); v != "" {
		body["launcher"] = v
	}
	if v := strings.TrimSpace(opts.Model); v != "" {
		body["model"] = v
	}
	if v := strings.TrimSpace(opts.Effort); v != "" {
		body["effort"] = v
	}
	err := c.call(ctx, http.MethodPost, "/api/v1/assistant/conversations?space="+url.QueryEscape(slug), body, &out)
	return out.Session.MachineID, out.Session.SessionID, err
}
