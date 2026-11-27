package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

func pageTitle(page notionPage) string {
	for _, name := range []string{"title", "Name", "name"} {
		var prop struct {
			Title []richText `json:"title"`
		}
		if raw, ok := page.Properties[name]; ok && json.Unmarshal(raw, &prop) == nil {
			if title := plain(prop.Title); title != "" {
				return title
			}
		}
	}
	keys := make([]string, 0, len(page.Properties))
	for k := range page.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var prop struct {
			Title []richText `json:"title"`
		}
		if json.Unmarshal(page.Properties[k], &prop) == nil {
			if title := plain(prop.Title); title != "" {
				return title
			}
		}
	}
	return "Untitled"
}

func plain(parts []richText) string {
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(part.PlainText)
	}
	return b.String()
}

func blockText(b block) string {
	var text string
	switch b.Type {
	case "paragraph":
		text = plain(b.Paragraph.RichText)
	case "heading_1":
		text = "# " + plain(b.Heading1.RichText)
	case "heading_2":
		text = "## " + plain(b.Heading2.RichText)
	case "heading_3":
		text = "### " + plain(b.Heading3.RichText)
	case "bulleted_list_item":
		text = "- " + plain(b.Bulleted.RichText)
	case "numbered_list_item":
		text = "1. " + plain(b.Numbered.RichText)
	case "quote":
		text = "> " + plain(b.Quote.RichText)
	case "callout":
		text = plain(b.Callout.RichText)
	case "toggle":
		text = plain(b.Toggle.RichText)
	case "code":
		text = plain(b.Code.RichText)
	case "child_page":
		text = plain(b.ChildPage.Title)
	case "to_do":
		text = "- [ ] " + plain(b.Todo.RichText)
		if b.Todo.Checked {
			text = "- [x] " + plain(b.Todo.RichText)
		}
	case "divider":
		return "---"
	default:
		return "[Notion " + b.Type + " block]"
	}
	return text
}

func (a *app) readBlocks(ctx context.Context, id string, depth int) ([]string, error) {
	if depth > 3 {
		return nil, nil
	}
	var lines []string
	cursor, seen := "", map[string]bool{}
	for range 4 {
		page, err := a.client.children(ctx, id, cursor)
		if err != nil {
			return nil, err
		}
		for _, b := range page.Results {
			lines = append(lines, blockText(b))
			if b.HasChildren && depth < 3 {
				children, err := a.readBlocks(ctx, b.ID, depth+1)
				if err != nil {
					return nil, err
				}
				for _, child := range children {
					lines = append(lines, "  "+child)
				}
			}
		}
		if !page.HasMore || page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		if seen[*page.NextCursor] {
			return nil, errors.New("Notion repeated its pagination cursor")
		}
		seen[*page.NextCursor], cursor = true, *page.NextCursor
	}
	return lines, nil
}

// Writes send Notion's request objects, not the response types in model.go:
// Notion rejects response-only fields such as plain_text, and a block that
// carries any key besides object, type and the body named by type.

// textItems returns a rich text request holding one text item.
func textItems(text string) []map[string]any {
	return []map[string]any{{"type": "text", "text": map[string]string{"content": text}}}
}

func paragraphRequest(text string) map[string]any {
	return map[string]any{"object": "block", "type": "paragraph", "paragraph": map[string]any{"rich_text": textItems(text)}}
}

func todoRequest(text string) map[string]any {
	return map[string]any{"object": "block", "type": "to_do", "to_do": map[string]any{"rich_text": textItems(text), "checked": false}}
}

func validNotionID(id string) error {
	if len(id) == 0 || len(id) > 128 || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\x00\r\n/") {
		return errors.New("Notion page ID is invalid")
	}
	return nil
}

func validNote(text string, max int) error {
	if len([]rune(text)) == 0 || strings.TrimSpace(text) == "" || len([]rune(text)) > max {
		return fmt.Errorf("note must be nonempty and at most %d characters", max)
	}
	for _, r := range text {
		if r == 0 || r < 32 && r != '\n' && r != '\t' {
			return errors.New("note contains control characters")
		}
	}
	return nil
}
