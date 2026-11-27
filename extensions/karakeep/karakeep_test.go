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

const testKey = "synthetic-karakeep-key"

type fakeKarakeep struct {
	srv     *httptest.Server
	mu      sync.Mutex
	paths   []string
	posts   []map[string]any
	patches []map[string]any
}

// Synthetic bodies shaped like Karakeep's OpenAPI spec: a crawled link bookmark
// whose own title is null, and a text bookmark that keeps its text in content.
const (
	linkBookmarkJSON = `{"id":"bookmark-1","createdAt":"2026-01-02T03:04:05.000Z","modifiedAt":"2026-01-02T03:04:05.000Z","title":null,"archived":false,"favourited":false,"taggingStatus":"success","summarizationStatus":null,"embeddingStatus":"success","note":"Why this matters","summary":null,"source":"api","userId":"user-1","tags":[{"id":"tag-1","name":"reading","attachedBy":"human"}],"content":{"type":"link","url":"https://example.com/article","title":"Reading with context","description":"A saved article","imageUrl":null,"favicon":null,"htmlContent":null,"crawledAt":"2026-01-02T03:05:00.000Z","crawlStatus":"success","author":null,"publisher":null,"datePublished":null,"dateModified":null},"assets":[]}`
	textBookmarkJSON = `{"id":"bookmark-text","createdAt":"2026-01-03T03:04:05.000Z","modifiedAt":null,"title":null,"archived":false,"favourited":true,"taggingStatus":"pending","summarizationStatus":null,"embeddingStatus":null,"note":null,"summary":null,"source":"web","userId":"user-1","tags":[],"content":{"type":"text","text":"A quoted paragraph worth keeping","sourceUrl":"https://example.com/source"},"assets":[]}`
	highlightJSON    = `{"bookmarkId":%q,"startOffset":0,"endOffset":21,"color":"yellow","text":%q,"prefix":null,"suffix":null,"note":%q,"id":%q,"userId":"user-1","createdAt":"2026-01-02T04:00:00.000Z"}`
)

func newFakeKarakeep(t *testing.T) *fakeKarakeep {
	f := &fakeKarakeep{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/bookmarks/search":
			fmt.Fprintf(w, `{"bookmarks":[%s],"nextCursor":"next-page"}`, linkBookmarkJSON)
		case "/api/v1/bookmarks":
			if r.Method == http.MethodPost {
				var in map[string]any
				_ = json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&in)
				f.mu.Lock()
				f.posts = append(f.posts, in)
				f.mu.Unlock()
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, strings.Replace(linkBookmarkJSON, `"bookmark-1"`, `"new-bookmark"`, 1))
				return
			}
			if r.URL.Query().Get("limit") == "1" {
				fmt.Fprintf(w, `{"bookmarks":[%s],"nextCursor":"after-first"}`, linkBookmarkJSON)
				return
			}
			fmt.Fprintf(w, `{"bookmarks":[%s,%s],"nextCursor":null}`, linkBookmarkJSON, textBookmarkJSON)
		case "/api/v1/bookmarks/bookmark-1":
			if r.Method == http.MethodPatch {
				var in map[string]any
				_ = json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&in)
				f.mu.Lock()
				f.patches = append(f.patches, in)
				f.mu.Unlock()
				fmt.Fprint(w, `{"id":"bookmark-1","createdAt":"2026-01-02T03:04:05.000Z","modifiedAt":"2026-01-04T03:04:05.000Z","title":null,"archived":false,"favourited":true,"taggingStatus":"success","summarizationStatus":null,"embeddingStatus":"success","note":"Why this matters","summary":null,"source":"api","userId":"user-1"}`)
				return
			}
			fmt.Fprint(w, linkBookmarkJSON)
		case "/api/v1/bookmarks/bookmark-text":
			fmt.Fprint(w, textBookmarkJSON)
		case "/api/v1/bookmarks/bookmark-1/highlights":
			fmt.Fprintf(w, `{"highlights":[`+highlightJSON+`]}`, "bookmark-1", "A highlighted passage", "A note", "highlight-1")
		case "/api/v1/bookmarks/bookmark-text/highlights":
			fmt.Fprint(w, `{"highlights":[]}`)
		case "/api/v1/highlights":
			fmt.Fprintf(w, `{"highlights":[`+highlightJSON+`],"nextCursor":null}`, "other", "Another bookmark", "", "highlight-other")
		case "/api/v1/lists":
			fmt.Fprint(w, `{"lists":[{"id":"list-1","name":"Research","description":null,"icon":"book","parentId":null,"type":"manual","query":null,"public":false,"hasCollaborators":false,"userRole":"owner"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"code":"NOT_FOUND","message":"Not found"}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// app configures the client through instanceURL, as init does for a real instance.
func (f *fakeKarakeep) app() *app {
	base, err := instanceURL(f.srv.URL)
	if err != nil {
		panic(err)
	}
	return &app{client: newClient(base, testKey), receipts: newWriteStore("")}
}

func (f *fakeKarakeep) requested(want string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.paths, want)
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeKarakeep(t).app())))
}

func TestManifest(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "karakeep" || len(m.Secrets) != 1 || m.Secrets[0].Name != "api_key" {
		t.Fatalf("%+v", m)
	}
	if len(m.Contributes.Connectors[0].ConfirmAlways) != 2 {
		t.Fatal("save and update must be confirmed")
	}
	if !slices.Equal(m.Permissions.Network, []string{}) {
		t.Fatal(m.Permissions.Network)
	}
}

func TestInstanceURLValidation(t *testing.T) {
	for raw, want := range map[string]string{
		"https://karakeep.example.com":         "https://karakeep.example.com/api/v1",
		"https://karakeep.example.com/":        "https://karakeep.example.com/api/v1",
		"http://127.0.0.1:3000":                "http://127.0.0.1:3000/api/v1",
		"https://karakeep.example.com/api":     "https://karakeep.example.com/api/v1",
		"https://karakeep.example.com/api/v1/": "https://karakeep.example.com/api/v1",
		"https://example.com/karakeep":         "https://example.com/karakeep/api/v1",
		"https://example.com/karakeep/api":     "https://example.com/karakeep/api/v1",
		"https://example.com/karakeep/api/v1":  "https://example.com/karakeep/api/v1",
	} {
		got, err := instanceURL(raw)
		if err != nil || got != want {
			t.Fatalf("%q: %v %q", raw, err, got)
		}
	}
	for _, raw := range []string{"http://karakeep.example.com", "https://me@example.com", "not-a-url"} {
		if _, err := instanceURL(raw); err == nil {
			t.Fatal("accepted " + raw)
		}
	}
}

func TestSearchAndListShowCrawledTitleAndURL(t *testing.T) {
	f := newFakeKarakeep(t)
	for _, args := range []string{`{"query":"context","cursor":"old"}`, `{}`} {
		text, err := f.app().callTool(t.Context(), "karakeep_bookmarks", json.RawMessage(args))
		if err != nil || !strings.Contains(text, "- Reading with context\n  ID: bookmark-1 | type: link") || !strings.Contains(text, "https://example.com/article") || !strings.Contains(text, "A saved article") {
			t.Fatalf("%s: %v %s", args, err, text)
		}
	}
	if !f.requested("/api/v1/bookmarks/search?cursor=old&limit=20&q=context") {
		t.Fatal(f.paths)
	}
	text, _ := f.app().callTool(t.Context(), "karakeep_bookmarks", json.RawMessage(`{}`))
	if !strings.Contains(text, "- A quoted paragraph worth keeping\n  ID: bookmark-text | type: text") || !strings.Contains(text, "https://example.com/source") {
		t.Fatal(text)
	}
}

func TestReadShowsNoteTextAndOwnHighlights(t *testing.T) {
	f := newFakeKarakeep(t)
	text, err := f.app().callTool(t.Context(), "karakeep_read", json.RawMessage(`{"id":"bookmark-1"}`))
	if err != nil || !strings.Contains(text, "Reading with context") || !strings.Contains(text, "Why this matters") || !strings.Contains(text, "A highlighted passage") || !strings.Contains(text, "A note") || strings.Contains(text, "Another bookmark") {
		t.Fatalf("%v %s", err, text)
	}
	if !f.requested("/api/v1/bookmarks/bookmark-1/highlights") {
		t.Fatal(f.paths)
	}
	text, err = f.app().callTool(t.Context(), "karakeep_read", json.RawMessage(`{"id":"bookmark-text"}`))
	if err != nil || !strings.Contains(text, "Text:\nA quoted paragraph worth keeping") || strings.Contains(text, "Highlight") {
		t.Fatalf("%v %s", err, text)
	}
}

func TestListsDecodeWrappedResponse(t *testing.T) {
	text, err := newFakeKarakeep(t).app().callTool(t.Context(), "karakeep_lists", nil)
	if err != nil || text != "- Research (manual, id list-1)\n" {
		t.Fatalf("%v %q", err, text)
	}
}

func TestLimitIsSentAndServerCursorContinues(t *testing.T) {
	f := newFakeKarakeep(t)
	text, err := f.app().callTool(t.Context(), "karakeep_bookmarks", json.RawMessage(`{"limit":1}`))
	if err != nil || !strings.Contains(text, "bookmark-1") || strings.Contains(text, "bookmark-text") || !strings.Contains(text, "Next cursor: after-first") {
		t.Fatalf("%v %s", err, text)
	}
	if !f.requested("/api/v1/bookmarks?limit=1") {
		t.Fatal(f.paths)
	}
}

func TestDisplayTitleFallbacks(t *testing.T) {
	for want, item := range map[string]bookmark{
		"Own title":                {Title: "Own title", Content: bookmarkContent{Type: "link", URL: "https://example.com/a", Title: "Crawled"}},
		"https://example.com/b":    {Content: bookmarkContent{Type: "link", URL: "https://example.com/b"}},
		"scan.pdf":                 {Content: bookmarkContent{Type: "asset", FileName: "scan.pdf", SourceURL: "https://example.com/scan"}},
		"First line":               {Content: bookmarkContent{Type: "text", Text: "First line\nSecond line"}},
		"Untitled":                 {Content: bookmarkContent{Type: "unknown"}},
		"https://example.com/text": {Content: bookmarkContent{Type: "text", Text: "  ", SourceURL: "https://example.com/text"}},
	} {
		if got := item.displayTitle(); got != want {
			t.Fatalf("%+v: %q", item, got)
		}
	}
}

func TestSaveAndPartialUpdateReplay(t *testing.T) {
	f := newFakeKarakeep(t)
	a := f.app()
	for range 2 {
		if _, err := a.doAction(t.Context(), "save-1", "karakeep_save", []byte(`{"url":"https://example.com/new#part"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.posts) != 1 || f.posts[0]["url"] != "https://example.com/new" {
		t.Fatal(f.posts)
	}
	if _, err := a.doAction(t.Context(), "fav-1", "karakeep_update", []byte(`{"id":"bookmark-1","favourited":true}`)); err != nil {
		t.Fatal(err)
	}
	if len(f.patches) != 1 || f.patches[0]["favourited"] != true || len(f.patches[0]) != 1 {
		t.Fatal(f.patches)
	}
}

func TestPersistentReplayAndUnknownOutcome(t *testing.T) {
	f := newFakeKarakeep(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	args := []byte(`{"url":"https://example.com/persisted"}`)
	if _, err := a.doAction(t.Context(), "persist-1", "karakeep_save", args); err != nil {
		t.Fatal(err)
	}
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "persist-1", "karakeep_save", args); err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 1 {
		t.Fatal("restart repeated the save")
	}
	key, _ := json.Marshal(map[string]any{"action": "karakeep_save", "url": "https://example.com/unknown"})
	raw, _ := json.Marshal(map[string]writeReceipt{"persist-1": {Key: string(key), Status: "pending", StartedAt: time.Now()}})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.receipts, _ = openWriteStore(filepath.Dir(store.path))
	if _, err := a.doAction(t.Context(), "persist-1", "karakeep_save", []byte(`{"url":"https://example.com/unknown"}`)); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatal("unknown write was retried")
	}
	if len(f.posts) != 1 {
		t.Fatal("unknown write caused another POST")
	}
}

func TestLocalizedViewsAndSaveReview(t *testing.T) {
	f := newFakeKarakeep(t)
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
	st := pageState{Mode: "save", DraftID: "save-1"}
	v := saveView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6}}, st)
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	st.SaveURL, st.Mode = "https://example.com/article", "review-save"
	v = reviewSaveView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6}}, st)
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.Sections[0].Items[1].Action.Confirm, st.SaveURL) {
		t.Fatal("confirmation lacks URL")
	}
}

func TestMoreKeepsExistingBookmarksAndUsesCursor(t *testing.T) {
	f := newFakeKarakeep(t)
	a := f.app()
	v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6, State: ext.EncodeState(pageState{Cursor: "next-page"})}})
	if err != nil {
		t.Fatal(err)
	}
	if v == nil || len(v.Sections[0].Items) == 0 {
		t.Fatal("existing bookmark was not retained")
	}
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6, State: v.State, Action: "more"}}
	if _, err := a.action(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for _, path := range f.paths {
		if strings.Contains(path, "cursor=next-page") {
			found = true
		}
	}
	if !found {
		t.Fatal("next cursor was not carried forward")
	}
}

func TestDetailPageShowsBookmarkContentAndOwnHighlights(t *testing.T) {
	f := newFakeKarakeep(t)
	for id, want := range map[string]string{"bookmark-1": "A highlighted passage", "bookmark-text": "A quoted paragraph worth keeping"} {
		st := ext.EncodeState(pageState{Mode: "detail", BookmarkID: id})
		v, err := f.app().render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6, State: st}})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		raw, _ := json.Marshal(v)
		if !strings.Contains(string(raw), want) || strings.Contains(string(raw), "Another bookmark") {
			t.Fatalf("%s: %s", id, raw)
		}
		if id == "bookmark-1" && (v.Title != "Reading with context" || !strings.Contains(string(raw), "https://example.com/article")) {
			t.Fatalf("%s: %s", id, raw)
		}
	}
	if !f.requested("/api/v1/bookmarks/bookmark-1/highlights") {
		t.Fatal(f.paths)
	}
}

func TestDetailBoundsAndNoConfig(t *testing.T) {
	v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{BookmarkID: "bookmark-1", Mode: "detail"}, bookmark{ID: "bookmark-1", Title: "Article", Content: bookmarkContent{Type: "text", Text: strings.Repeat("text ", 3000), SourceURL: "https://example.com"}}, nil)
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	a := newApp()
	if a.status()["state"] != "not_configured" {
		t.Fatal(a.status())
	}
	v, err := a.render(t.Context(), &ext.UIRequest{})
	if err != nil || !strings.Contains(v.Sections[0].Items[0].Title, "instance URL") {
		t.Fatalf("%v %+v", err, v)
	}
}
