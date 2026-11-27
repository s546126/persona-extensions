package main

import (
	"encoding/json"
	"strings"
)

// bookmark follows Karakeep's v1 Bookmark schema. Nullable strings decode to "".
type bookmark struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Note       string          `json:"note"`
	Summary    string          `json:"summary"`
	Favourited bool            `json:"favourited"`
	Archived   bool            `json:"archived"`
	CreatedAt  string          `json:"createdAt"`
	ModifiedAt string          `json:"modifiedAt"`
	Tags       json.RawMessage `json:"tags"`
	Content    bookmarkContent `json:"content"`
}

// bookmarkContent holds the fields this extension reads from the link, text
// and asset variants of the bookmark content union.
type bookmarkContent struct {
	Type        string `json:"type"`
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Text        string `json:"text"`
	SourceURL   string `json:"sourceUrl"`
	FileName    string `json:"fileName"`
}

// displayTitle prefers the user's title, then crawled or file metadata, the
// bookmarked URL, the first line of a text bookmark and finally its source.
func (b bookmark) displayTitle() string {
	firstLine, _, _ := strings.Cut(strings.TrimSpace(b.Content.Text), "\n")
	for _, s := range []string{b.Title, b.Content.Title, b.Content.FileName, b.Content.URL, clipText(firstLine, 120), b.Content.SourceURL} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return "Untitled"
}

// link is the bookmarked URL, or the source of a text or asset bookmark.
func (b bookmark) link() string {
	if b.Content.URL != "" {
		return b.Content.URL
	}
	return b.Content.SourceURL
}

func (b bookmark) kind() string { return b.Content.Type }

type bookmarkList struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	ParentID string `json:"parentId"`
}

type highlight struct {
	ID         string `json:"id"`
	BookmarkID string `json:"bookmarkId"`
	Text       string `json:"text"`
	Note       string `json:"note"`
	Color      string `json:"color"`
	CreatedAt  string `json:"createdAt"`
}

type bookmarkPage struct {
	Bookmarks  []bookmark `json:"bookmarks"`
	NextCursor *string    `json:"nextCursor"`
}

type updatePatch struct {
	Note       *string `json:"note,omitempty"`
	Favourited *bool   `json:"favourited,omitempty"`
	Archived   *bool   `json:"archived,omitempty"`
}

func (p updatePatch) empty() bool { return p.Note == nil && p.Favourited == nil && p.Archived == nil }
