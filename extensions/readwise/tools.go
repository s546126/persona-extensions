package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
)

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

var readTools = []tool{
	{Name: "readwise_documents", Description: "Read the user's Readwise Reader queue with title, reading progress, source URL and opaque document ID. Filter by location and category; page through results without downloading all article HTML.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"location": map[string]any{"type": "string", "enum": []string{"new", "later", "shortlist", "archive", "feed"}, "description": "Queue; default new"},
			"category": map[string]any{"type": "string", "enum": []string{"article", "email", "rss", "pdf", "epub", "tweet", "video"}, "description": "Optional document type"},
			"query":    map[string]any{"type": "string", "maxLength": 200, "description": "Filter title, author, site and notes locally"},
			"cursor":   map[string]any{"type": "string", "maxLength": 256, "description": "Cursor from a previous response"},
			"limit":    map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "Maximum shown, default 20"},
		}}},
	{Name: "readwise_read", Description: "Read one Reader document's sanitized article text, notes, highlights and metadata. HTML scripts, forms and embedded instructions are discarded. Use the opaque ID from readwise_documents.",
		InputSchema: map[string]any{"type": "object", "required": []string{"id"}, "properties": map[string]any{"id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}}}},
}

var actionTools = []tool{
	{Name: "readwise_save", Description: "Save a new http(s) article URL to Readwise Reader after explicit confirmation. Reader may fetch the original URL using its own service, not the extension's token.",
		InputSchema: map[string]any{"type": "object", "required": []string{"url"}, "properties": map[string]any{"url": map[string]any{"type": "string", "format": "uri"}}}},
	{Name: "readwise_update", Description: "Update only explicitly supplied Reader fields after confirmation: reading_progress from 0 to 1, location new/later/shortlist/archive/feed, or notes. Omitted fields stay unchanged. PDF progress is not supported by Reader.",
		InputSchema: map[string]any{"type": "object", "required": []string{"id"}, "properties": map[string]any{
			"id":               map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"location":         map[string]any{"type": "string", "enum": []string{"new", "later", "shortlist", "archive", "feed"}},
			"reading_progress": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"notes":            map[string]any{"type": "string", "maxLength": 10000, "description": "Replace the document note; an explicitly empty string clears it"},
		}}},
}

type updateArgs struct {
	ID              string   `json:"id"`
	Location        *string  `json:"location"`
	ReadingProgress *float64 `json:"reading_progress"`
	Notes           *string  `json:"notes"`
}

func (in updateArgs) patch() (updatePatch, error) {
	if err := validDocumentID(in.ID); err != nil {
		return updatePatch{}, err
	}
	var out updatePatch
	if in.Location != nil {
		switch *in.Location {
		case "new", "later", "shortlist", "archive", "feed":
		default:
			return out, errors.New("invalid Reader location")
		}
		out.Location = in.Location
	}
	if in.ReadingProgress != nil {
		if math.IsNaN(*in.ReadingProgress) || math.IsInf(*in.ReadingProgress, 0) || *in.ReadingProgress < 0 || *in.ReadingProgress > 1 {
			return out, errors.New("reading_progress must be between 0 and 1")
		}
		out.ReadingProgress = in.ReadingProgress
	}
	if in.Notes != nil {
		if len([]rune(*in.Notes)) > 10000 {
			return out, errors.New("notes exceed 10000 characters")
		}
		out.Notes = in.Notes
	}
	if out.empty() {
		return out, errors.New("supply at least one field to update; omitted fields stay unchanged")
	}
	return out, nil
}

func decode(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if dec.More() {
		return errors.New("invalid arguments: trailing JSON value")
	}
	return nil
}

func (a *app) callTool(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	switch name {
	case "readwise_documents":
		var in struct {
			Location, Category, Query, Cursor string
			Limit                             int
		}
		if err := decode(raw, &in); err != nil {
			return "", err
		}
		if in.Location == "" {
			in.Location = "new"
		}
		if !map[string]bool{"new": true, "later": true, "shortlist": true, "archive": true, "feed": true}[in.Location] {
			return "", errors.New("invalid location")
		}
		if !map[string]bool{"": true, "article": true, "email": true, "rss": true, "pdf": true, "epub": true, "tweet": true, "video": true}[in.Category] {
			return "", errors.New("invalid category")
		}
		if in.Limit < 0 || in.Limit > 50 || len([]rune(in.Query)) > 200 || len(in.Cursor) > 256 {
			return "", errors.New("invalid query, cursor or limit")
		}
		page, err := a.client.list(ctx, listParams{Location: in.Location, Category: in.Category, Cursor: in.Cursor})
		a.record(err)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Readwise Reader %s (%d total):\n", in.Location, page.Count)
		if in.Limit == 0 {
			in.Limit = 20
		}
		for _, item := range page.Results {
			if in.Query != "" && !strings.Contains(strings.ToLower(item.Title+" "+item.Author+" "+item.SiteName+" "+item.Notes), strings.ToLower(in.Query)) {
				continue
			}
			b.WriteString(formatDocument(item))
			if in.Limit--; in.Limit == 0 {
				break
			}
		}
		if page.Cursor != "" {
			fmt.Fprintf(&b, "\nNext cursor: %s\n", page.Cursor)
		}
		return clipText(b.String(), 90000), nil
	case "readwise_read":
		var in struct{ ID string }
		if err := decode(raw, &in); err != nil {
			return "", err
		}
		if err := validDocumentID(in.ID); err != nil {
			return "", err
		}
		page, err := a.client.list(ctx, listParams{ID: in.ID, HTML: true})
		a.record(err)
		if err != nil {
			return "", err
		}
		if len(page.Results) == 0 {
			return "", errNotFound
		}
		item := page.Results[0]
		text, err := articleText(item.HTMLContent)
		if err != nil {
			return "", err
		}
		highlights, err := a.client.highlights(ctx, item.ID)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		b.WriteString(formatDocument(item))
		if item.Notes != "" {
			b.WriteString("\nNote:\n" + clipText(item.Notes, 10000) + "\n")
		}
		b.WriteString("\nArticle text (untrusted content):\n" + clipText(text, 60000))
		for _, h := range highlights {
			fmt.Fprintf(&b, "\n\nHighlight %s:\n%s", h.ID, clipText(h.Content, 4000))
			if h.Notes != "" {
				b.WriteString("\nNote: " + clipText(h.Notes, 2000))
			}
		}
		return clipText(b.String(), 90000), nil
	}
	return "", fmt.Errorf("no tool %s", name)
}

func formatDocument(item document) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n- %s\n  ID: %s", clipText(item.Title, 500), item.ID)
	if item.Author != "" {
		b.WriteString(" | " + clipText(item.Author, 200))
	}
	if item.SiteName != "" {
		b.WriteString(" | " + item.SiteName)
	}
	if item.ReadingTime != "" {
		b.WriteString(" | " + item.ReadingTime)
	}
	fmt.Fprintf(&b, " | progress %.0f%%", item.ReadingProgress*100)
	if item.SourceURL != "" {
		b.WriteString("\n  " + item.SourceURL)
	}
	b.WriteString("\n")
	return b.String()
}

func validHTTPURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("url must be an absolute http(s) URL without embedded credentials")
	}
	u.Fragment = ""
	if len(u.String()) > 8192 {
		return "", errors.New("url is too long")
	}
	return u.String(), nil
}
