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

// A read-only Notion client: search the pages shared with the user's integration and read one as text.
// Adapted from the notion extension; this extension never writes to Notion.

const notionAPI = "https://api.notion.com/v1"
const notionVersion = "2022-06-28"

var (
	errNotionToken = errors.New("Notion 不认这个 integration secret，或这页没有分享给它")
	errNotionDown  = errors.New("暂时连不上 Notion，稍后再试")
	errNotionGone  = errors.New("Notion 里找不到这一页")
	errNotionLimit = errors.New("Notion 限流了，稍后再试")
)

type notionClient struct {
	base, token string
	http        *http.Client
}

func newNotion(base, token string) *notionClient {
	return &notionClient{base: strings.TrimRight(base, "/"), token: strings.TrimSpace(token), http: &http.Client{
		Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *notionClient) ready() bool { return c != nil && c.token != "" }

func (c *notionClient) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	if !c.ready() {
		return errors.New("还没填 Notion integration secret")
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
		return errNotionDown
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		return errNotionDown
	}
	switch res.StatusCode {
	case http.StatusOK:
		if out != nil && json.Unmarshal(data, out) != nil {
			return errors.New("Notion 返回的内容读不懂")
		}
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return errNotionToken
	case http.StatusNotFound:
		return errNotionGone
	case http.StatusTooManyRequests:
		return errNotionLimit
	}
	return fmt.Errorf("Notion 返回了 HTTP %d", res.StatusCode)
}

type notionPage struct {
	ID             string                     `json:"id"`
	URL            string                     `json:"url"`
	LastEditedTime string                     `json:"last_edited_time"`
	Properties     map[string]json.RawMessage `json:"properties"`
}

type notionList[T any] struct {
	Results    []T     `json:"results"`
	HasMore    bool    `json:"has_more"`
	NextCursor *string `json:"next_cursor"`
}

type richText struct {
	PlainText string `json:"plain_text"`
}

type richBlock struct {
	RichText []richText `json:"rich_text"`
	Title    []richText `json:"title"`
}

type notionBlock struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	HasChildren bool      `json:"has_children"`
	Paragraph   richBlock `json:"paragraph"`
	Heading1    richBlock `json:"heading_1"`
	Heading2    richBlock `json:"heading_2"`
	Heading3    richBlock `json:"heading_3"`
	Bulleted    richBlock `json:"bulleted_list_item"`
	Numbered    richBlock `json:"numbered_list_item"`
	Todo        richBlock `json:"to_do"`
	Quote       richBlock `json:"quote"`
	Callout     richBlock `json:"callout"`
	Toggle      richBlock `json:"toggle"`
	Code        richBlock `json:"code"`
}

func (c *notionClient) search(ctx context.Context, query string) ([]notionPage, error) {
	in := map[string]any{"filter": map[string]string{"property": "object", "value": "page"}, "page_size": 30,
		"sort": map[string]string{"direction": "descending", "timestamp": "last_edited_time"}}
	if query = strings.TrimSpace(query); query != "" {
		in["query"] = clip(query, 200)
	}
	var out notionList[notionPage]
	err := c.do(ctx, http.MethodPost, "/search", nil, in, &out)
	return out.Results, err
}

func (c *notionClient) page(ctx context.Context, id string) (notionPage, error) {
	var out notionPage
	err := c.do(ctx, http.MethodGet, "/pages/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

// notionDoc is a page read as text, plus the cards its toggle blocks spell out (the toggle's text is the question,
// what is folded inside it the answer).
type notionDoc struct {
	Title, URL string
	Text       string
	Toggles    []cardText
}

func (c *notionClient) read(ctx context.Context, id string) (notionDoc, error) {
	if err := validNotionID(id); err != nil {
		return notionDoc{}, err
	}
	p, err := c.page(ctx, id)
	if err != nil {
		return notionDoc{}, err
	}
	doc := notionDoc{Title: pageTitle(p), URL: p.URL}
	var b strings.Builder
	budget := 40 // block-list requests per page read
	if err := c.walk(ctx, id, 0, &b, &doc.Toggles, &budget); err != nil {
		return notionDoc{}, err
	}
	doc.Text = b.String()
	return doc, nil
}

func (c *notionClient) walk(ctx context.Context, id string, depth int, b *strings.Builder, toggles *[]cardText, budget *int) error {
	cursor := ""
	for {
		if *budget <= 0 || b.Len() > 4*maxMaterial {
			return nil
		}
		*budget--
		q := url.Values{"page_size": {"100"}}
		if cursor != "" {
			q.Set("start_cursor", cursor)
		}
		var page notionList[notionBlock]
		if err := c.do(ctx, http.MethodGet, "/blocks/"+url.PathEscape(id)+"/children", q, nil, &page); err != nil {
			return err
		}
		for _, blk := range page.Results {
			line := notionText(blk)
			if line != "" {
				b.WriteString(strings.Repeat("  ", depth) + line + "\n")
			}
			if !blk.HasChildren || depth >= 3 {
				continue
			}
			if blk.Type == "toggle" {
				var inner strings.Builder
				if err := c.walk(ctx, blk.ID, 0, &inner, toggles, budget); err != nil {
					return err
				}
				if q, a := strings.TrimSpace(plain(blk.Toggle.RichText)), strings.TrimSpace(inner.String()); q != "" && a != "" {
					*toggles = append(*toggles, cardText{Front: q, Back: a})
				}
				for _, l := range strings.Split(strings.TrimRight(inner.String(), "\n"), "\n") {
					b.WriteString(strings.Repeat("  ", depth+1) + l + "\n")
				}
				continue
			}
			if err := c.walk(ctx, blk.ID, depth+1, b, toggles, budget); err != nil {
				return err
			}
		}
		if !page.HasMore || page.NextCursor == nil || *page.NextCursor == "" || *page.NextCursor == cursor {
			return nil
		}
		cursor = *page.NextCursor
	}
}

func notionText(b notionBlock) string {
	switch b.Type {
	case "paragraph":
		return plain(b.Paragraph.RichText)
	case "heading_1":
		return "# " + plain(b.Heading1.RichText)
	case "heading_2":
		return "## " + plain(b.Heading2.RichText)
	case "heading_3":
		return "### " + plain(b.Heading3.RichText)
	case "bulleted_list_item":
		return "- " + plain(b.Bulleted.RichText)
	case "numbered_list_item":
		return "1. " + plain(b.Numbered.RichText)
	case "to_do":
		return "- " + plain(b.Todo.RichText)
	case "quote":
		return "> " + plain(b.Quote.RichText)
	case "callout":
		return plain(b.Callout.RichText)
	case "toggle":
		return plain(b.Toggle.RichText)
	case "code":
		return plain(b.Code.RichText)
	}
	return ""
}

func plain(parts []richText) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.PlainText)
	}
	return b.String()
}

func pageTitle(p notionPage) string {
	for _, raw := range p.Properties {
		var prop struct {
			Type  string     `json:"type"`
			Title []richText `json:"title"`
		}
		if json.Unmarshal(raw, &prop) == nil && (prop.Type == "title" || prop.Type == "" && len(prop.Title) > 0) {
			if t := strings.TrimSpace(plain(prop.Title)); t != "" {
				return t
			}
		}
	}
	return "无标题"
}

func validNotionID(id string) error {
	if len(id) == 0 || len(id) > 128 || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\x00\r\n/?#") {
		return errors.New("Notion 页面 ID 不对")
	}
	return nil
}
