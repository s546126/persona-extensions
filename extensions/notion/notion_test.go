package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

const testSecret = "synthetic-notion-secret"

type fakeNotion struct {
	srv     *httptest.Server
	mu      sync.Mutex
	paths   []string
	patches []map[string]any
	creates []map[string]any
}

func newFakeNotion(t *testing.T) *fakeNotion {
	f := &fakeNotion{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testSecret || r.Header.Get("Notion-Version") != notionVersion {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search":
			fmt.Fprint(w, `{"results":[{"object":"page","id":"page-1","url":"https://www.notion.so/weekly-notes","last_edited_time":"2026-11-09T10:00:00.000Z","properties":{"title":{"title":[{"plain_text":"Weekly notes"}]}}}],"has_more":true,"next_cursor":"next-page"}`)
		case "/pages/page-1":
			fmt.Fprint(w, `{"object":"page","id":"page-1","url":"https://www.notion.so/weekly-notes","properties":{"title":{"title":[{"plain_text":"Weekly notes"}]}}}`)
		case "/blocks/page-1/children":
			if r.Method == http.MethodPatch {
				var in map[string]any
				_ = json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&in)
				if _, ok := in["children"]; !ok || !validWrite(in) {
					rejectWrite(w)
					return
				}
				f.mu.Lock()
				f.patches = append(f.patches, in)
				f.mu.Unlock()
				fmt.Fprint(w, `{}`)
				return
			}
			fmt.Fprint(w, `{"results":[{"id":"block-heading","type":"heading_1","has_children":false,"heading_1":{"rich_text":[{"plain_text":"Weekly notes"}]}},{"id":"block-todo","type":"to_do","has_children":true,"to_do":{"rich_text":[{"plain_text":"Review reading notes"}],"checked":false}},{"id":"block-unknown","type":"unsupported_widget","has_children":false}],"has_more":true,"next_cursor":null}`)
		case "/blocks/block-todo/children":
			fmt.Fprint(w, `{"results":[{"id":"block-child","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"Keep the original context"}]}}],"has_more":true,"next_cursor":null}`)
		case "/pages":
			var in map[string]any
			_ = json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&in)
			if _, ok := in["properties"]; !ok || !validWrite(in) {
				rejectWrite(w)
				return
			}
			f.mu.Lock()
			f.creates = append(f.creates, in)
			f.mu.Unlock()
			fmt.Fprint(w, `{"object":"page","id":"new-page","url":"https://www.notion.so/new-page","properties":{"title":{"title":[{"plain_text":"New child page"}]}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func rejectWrite(w http.ResponseWriter) {
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprint(w, `{"object":"error","status":400,"code":"validation_error","message":"body failed validation"}`)
}

// validWrite checks a write body against Notion's documented request schema:
// each child block holds only object, type and the body named by type, the
// body holds only request fields, and each rich text or title item is a text
// request with content.
func validWrite(in map[string]any) bool {
	if raw, ok := in["children"]; ok {
		children, _ := raw.([]any)
		if len(children) == 0 {
			return false
		}
		for _, raw := range children {
			child, _ := raw.(map[string]any)
			kind, _ := child["type"].(string)
			body, _ := child[kind].(map[string]any)
			if body == nil || !validRichText(body["rich_text"]) {
				return false
			}
			for key, value := range child {
				if key == "object" && value != "block" || key != "object" && key != "type" && key != kind {
					return false
				}
			}
			for key := range body {
				if key != "rich_text" && (key != "checked" || kind != "to_do") {
					return false
				}
			}
		}
	}
	if raw, ok := in["properties"]; ok {
		props, _ := raw.(map[string]any)
		title, _ := props["title"].(map[string]any)
		if title == nil || !validRichText(title["title"]) {
			return false
		}
	}
	return true
}

func validRichText(raw any) bool {
	items, _ := raw.([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		_, response := item["plain_text"]
		if item["type"] != "text" || response || textContent([]any{item}) == "" {
			return false
		}
	}
	return len(items) > 0
}

// textContent joins the text.content of rich text request items.
func textContent(raw any) string {
	var b strings.Builder
	items, _ := raw.([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		text, _ := item["text"].(map[string]any)
		content, _ := text["content"].(string)
		b.WriteString(content)
	}
	return b.String()
}

func (f *fakeNotion) app() *app {
	return &app{client: newClient(f.srv.URL, testSecret), receipts: newWriteStore("")}
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeNotion(t).app())))
}

func TestManifest(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "notion" || len(m.Secrets) != 1 || m.Secrets[0].Name != "integration_secret" {
		t.Fatalf("%+v", m)
	}
	if !slices.Equal(m.Permissions.Network, []string{"api.notion.com"}) {
		t.Fatal(m.Permissions.Network)
	}
	if len(m.Contributes.Connectors[0].ConfirmAlways) != 2 {
		t.Fatal("append and create need confirmation")
	}
}

func TestSearchAndNestedBlocks(t *testing.T) {
	f := newFakeNotion(t)
	a := f.app()
	search, err := a.callTool(t.Context(), "notion_search", json.RawMessage(`{"query":"weekly","cursor":"cursor-old"}`))
	if err != nil || !strings.Contains(search, "Weekly notes") || !strings.Contains(search, "next-page") {
		t.Fatalf("%v %s", err, search)
	}
	text, err := a.callTool(t.Context(), "notion_read", json.RawMessage(`{"id":"page-1"}`))
	if err != nil || !strings.Contains(text, "# Weekly notes") || !strings.Contains(text, "- [ ] Review reading notes") || !strings.Contains(text, "Keep the original context") || !strings.Contains(text, "unsupported_widget") {
		t.Fatalf("%v %s", err, text)
	}
}

func TestAppendTodoUsesRealBlockTypeAndReplay(t *testing.T) {
	f := newFakeNotion(t)
	a := f.app()
	args := []byte(`{"page_id":"page-1","kind":"todo","text":"Review tomorrow"}`)
	for range 2 {
		if _, err := a.doAction(t.Context(), "append-1", "notion_append", args); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.patches) != 1 {
		t.Fatal("append replay wrote twice")
	}
	children := f.patches[0]["children"].([]any)
	first := children[0].(map[string]any)
	todo, _ := first["to_do"].(map[string]any)
	if len(children) != 1 || first["type"] != "to_do" || todo["checked"] != false || textContent(todo["rich_text"]) != "Review tomorrow" {
		t.Fatal(first)
	}
}

func TestCreateParentAndTitleAreSeparate(t *testing.T) {
	f := newFakeNotion(t)
	a := f.app()
	args := []byte(`{"parent_id":"page-1","title":"New child page","content":"Read notes"}`)
	if _, err := a.doAction(t.Context(), "create-1", "notion_create", args); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 {
		t.Fatal(f.creates)
	}
	in := f.creates[0]
	title, _ := in["properties"].(map[string]any)["title"].(map[string]any)
	children, _ := in["children"].([]any)
	if in["parent"].(map[string]any)["page_id"] != "page-1" || textContent(title["title"]) != "New child page" || len(children) != 1 {
		t.Fatal(in)
	}
	if paragraph, _ := children[0].(map[string]any)["paragraph"].(map[string]any); textContent(paragraph["rich_text"]) != "Read notes" {
		t.Fatal(children)
	}
}

func TestInvalidWritesDoNotCallAPI(t *testing.T) {
	f := newFakeNotion(t)
	a := f.app()
	for _, tc := range []struct{ name, args string }{
		{"notion_append", `{"page_id":"page-1","kind":"todo","text":""}`},
		{"notion_append", `{"page_id":"page-1","kind":"delete","text":"Never"}`},
		{"notion_append", `{"page_id":"../page","kind":"paragraph","text":"Never"}`},
		{"notion_create", `{"parent_id":"page-1","title":""}`},
	} {
		if _, err := a.doAction(t.Context(), "bad-"+tc.args, tc.name, []byte(tc.args)); err == nil {
			t.Fatal("accepted " + tc.args)
		}
	}
	if len(f.patches) != 0 || len(f.creates) != 0 {
		t.Fatal("invalid input reached API")
	}
}

func TestPersistentReplayAndUnknownOutcome(t *testing.T) {
	f := newFakeNotion(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	args := []byte(`{"parent_id":"page-1","title":"Persistent note"}`)
	if _, err := a.doAction(t.Context(), "persist-1", "notion_create", args); err != nil {
		t.Fatal(err)
	}
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "persist-1", "notion_create", args); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 {
		t.Fatal("restart repeated page creation")
	}
	key, _ := json.Marshal(map[string]any{"action": "notion_create", "parent_id": "page-1", "title": "Persistent note", "content": ""})
	raw, _ := json.Marshal(map[string]writeReceipt{"persist-1": {Key: string(key), Status: "pending", StartedAt: time.Now()}})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.receipts, _ = openWriteStore(filepath.Dir(store.path))
	if _, err := a.doAction(t.Context(), "persist-1", "notion_create", args); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatal("unknown write was retried")
	}
	if len(f.creates) != 1 {
		t.Fatal("unknown write caused a second create")
	}
}

func TestLocalizedViewsAndCreateReview(t *testing.T) {
	f := newFakeNotion(t)
	a := f.app()
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, ui := range []int{1, 3, 6} {
			r := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: ui}}
			v, err := a.render(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			if err := wire.CheckView(v); err != nil {
				t.Fatalf("%s %d: %v", locale, ui, err)
			}
		}
	}
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6, State: ext.EncodeState(pageState{Mode: "create", DraftID: "draft-1"}), Action: "review-create", Values: map[string]string{"parent_id": "page-1", "title": "New note", "content": "Details"}}}
	res, err := a.action(t.Context(), r)
	if err != nil || res.View == nil || !strings.Contains(res.View.State, "review") {
		t.Fatalf("%v %+v", err, res)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatal(err)
	}
	r.State, r.Action = res.View.State, "confirm-create"
	res, err = a.action(t.Context(), r)
	if err != nil || !res.Pop || res.Toast == "" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestAppendWorkflow(t *testing.T) {
	f := newFakeNotion(t)
	a := f.app()
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6, State: ext.EncodeState(pageState{Mode: "append", PageID: "page-1", DraftID: "append-1"}), Action: "review-append", Values: map[string]string{"kind": "todo", "text": "Review tomorrow"}}}
	res, err := a.action(t.Context(), r)
	if err != nil || res.View == nil {
		t.Fatalf("%v %+v", err, res)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatal(err)
	}
	r.State, r.Action = res.View.State, "confirm-append"
	res, err = a.action(t.Context(), r)
	if err != nil || !res.Pop || res.Toast == "" {
		t.Fatalf("%v %+v", err, res)
	}
	if len(f.patches) != 1 {
		t.Fatal("confirmed append did not reach the selected page")
	}
}

func TestDetailBoundsAndNoToken(t *testing.T) {
	var page notionPage
	page.URL = "https://www.notion.so/example"
	page.Properties = map[string]json.RawMessage{"title": json.RawMessage(`{"title":[{"plain_text":"Note"}]}`)}
	v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{PageID: "page-1", Mode: "detail"}, page, []string{"# Note", "- [ ] Follow up"})
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	a := newApp()
	if a.status()["state"] != "not_configured" {
		t.Fatal(a.status())
	}
	v, err := a.render(t.Context(), &ext.UIRequest{})
	if err != nil || !strings.Contains(v.Sections[0].Items[0].Title, "secret") {
		t.Fatalf("%v %+v", err, v)
	}
}
