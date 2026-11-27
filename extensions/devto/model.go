package main

import (
	"encoding/json"
	"strings"
)

type article struct {
	ID             int64                           `json:"id"`
	Title          string                          `json:"title"`
	Description    string                          `json:"description"`
	URL            string                          `json:"url"`
	CommentsCount  int                             `json:"comments_count"`
	ReactionsCount int                             `json:"public_reactions_count"`
	PublishedAt    string                          `json:"published_at"`
	ReadingMinutes int                             `json:"reading_time_minutes"`
	TagList        json.RawMessage                 `json:"tag_list"`
	Tags           json.RawMessage                 `json:"tags"`
	BodyMarkdown   string                          `json:"body_markdown"`
	User           struct{ Name, Username string } `json:"user"`
}

type comment struct {
	ID        string                `json:"id_code"`
	CreatedAt string                `json:"created_at"`
	BodyHTML  string                `json:"body_html"`
	User      struct{ Name string } `json:"user"`
	Children  []comment             `json:"children"`
}

type config struct {
	Tags         []string `json:"tags"`
	MinReactions int      `json:"min_reactions"`
}

// tagNames returns the article tags. DEV sends tag_list and tags in opposite
// shapes per endpoint (the list has a tag_list array and a comma-separated tags
// string, a single article swaps them), so either field may be either shape.
func (a article) tagNames() []string {
	if tags := parseTags(a.Tags); len(tags) > 0 {
		return tags
	}
	return parseTags(a.TagList)
}

func parseTags(raw json.RawMessage) []string {
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		var joined string
		if json.Unmarshal(raw, &joined) != nil {
			return nil
		}
		list = strings.Split(joined, ",")
	}
	var out []string
	for _, tag := range list {
		if tag = strings.TrimSpace(tag); tag != "" {
			out = append(out, tag)
		}
	}
	return out
}
