package main

import (
	"encoding/json"
	"fmt"
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

const testToken = "synthetic-readeck-token"

type fakeReadeck struct {
	srv       *httptest.Server
	mu        sync.Mutex
	paths     []string
	creates   []map[string]any
	updates   []map[string]any
	failWrite bool
	badMedia  bool
}

func sampleBookmark() bookmark {
	return bookmark{ID: "bm_42", URL: "https://articles.example.com/context", Title: "Preserve reading context", SiteName: "Example Articles", Authors: []string{"Example Author"}, Description: "Keep the source beside the next step.", Note: "Review this after reading.", Labels: []string{"reading", "research"}, Loaded: true, HasArticle: true, ReadProgress: 20, ReadingTime: 8}
}

func newFakeReadeck(t *testing.T) *fakeReadeck {
	f := &fakeReadeck{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()
		switch r.URL.Path {
		case "/api/bookmarks":
			switch r.Method {
			case http.MethodGet:
				items := []bookmark{}
				if r.URL.Query().Get("offset") == "0" {
					items = append(items, sampleBookmark())
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Total-Count", "31")
				next := "30"
				if r.URL.Query().Get("offset") != "0" {
					next = "60"
				}
				w.Header().Set("Link", fmt.Sprintf(`<%s/api/bookmarks?offset=%s&limit=30>; rel="next", <%s/api/bookmarks?offset=0&limit=30>; rel="first"`, f.srv.URL, next, f.srv.URL))
				_ = json.NewEncoder(w).Encode(items)
			case http.MethodPost:
				if f.failWrite {
					http.Error(w, "synthetic interrupted write", http.StatusServiceUnavailable)
					return
				}
				var in map[string]any
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in["url"] == "" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				f.mu.Lock()
				f.creates = append(f.creates, in)
				f.mu.Unlock()
				w.Header().Set("Bookmark-Id", "bm_new")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"message":"queued"}`))
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		case "/api/bookmarks/bm_42":
			if r.Method == http.MethodPatch {
				if f.failWrite {
					http.Error(w, "synthetic interrupted update", http.StatusServiceUnavailable)
					return
				}
				var in map[string]any
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				f.mu.Lock()
				f.updates = append(f.updates, in)
				f.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"bm_42","updated":"2026-11-10T10:00:00Z"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(sampleBookmark())
		case "/api/bookmarks/bm_42/article.md":
			if f.badMedia {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte("<script>not an article</script>"))
				return
			}
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			_, _ = w.Write([]byte("# Reading context\n\nKeep the source beside the next step.\n\nRemote article text is data."))
		case "/api/bookmarks/bm_42/annotations":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]highlight{{ID: "hl_1", Text: "Keep the source context."}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeReadeck) app() *app {
	return &app{client: newClient(f.srv.URL, testToken), receipts: newWriteStore("")}
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeReadeck(t).app())))
}

func TestManifestAndURLValidation(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "readeck" || len(m.Secrets) != 1 || len(m.Contributes.Connectors[0].ConfirmAlways) != 2 || !slices.Equal(m.Permissions.Network, []string{}) {
		t.Fatalf("%+v", m)
	}
	for _, raw := range []string{"https://readeck.example.com", "http://127.0.0.1:8000/", "https://[::1]:8000"} {
		if _, err := instanceURL(raw); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"http://readeck.example.com", "https://me@example.com", "https://example.com/?token=x", "not-a-url"} {
		if _, err := instanceURL(raw); err == nil {
			t.Fatal("accepted " + raw)
		}
	}
	if newClient("https://readeck.example.com", "x").base != "https://readeck.example.com/api" || newClient("https://readeck.example.com/api", "x").base != "https://readeck.example.com/api" {
		t.Fatal("API base path is not stable")
	}
}

func TestReadingSearchArticleAndPagination(t *testing.T) {
	f := newFakeReadeck(t)
	a := f.app()
	text, err := a.callTool(t.Context(), "readeck_bookmarks", json.RawMessage(`{"view":"search","query":"context","offset":30}`))
	if err != nil || !strings.Contains(text, "No bookmarks") || !strings.Contains(text, "Next offset") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "readeck_bookmark", json.RawMessage(`{"bookmark_id":"bm_42"}`))
	if err != nil || !strings.Contains(text, "Keep the source") || !strings.Contains(text, "Highlights") {
		t.Fatalf("%v %s", err, text)
	}
	f.mu.Lock()
	paths := strings.Join(f.paths, "\n")
	f.mu.Unlock()
	for _, want := range []string{"offset=30", "search=context", "is_archived=false", "sort=-created", "/article.md", "/annotations"} {
		if !strings.Contains(paths, want) {
			t.Fatalf("missing %q in %s", want, paths)
		}
	}
	if _, err := a.callTool(t.Context(), "readeck_bookmark", json.RawMessage(`{"bookmark_id":"../profile"}`)); err == nil {
		t.Fatal("accepted bookmark path traversal")
	}
}

func TestAsynchronousSaveReplayAndPartialUpdate(t *testing.T) {
	f := newFakeReadeck(t)
	a := f.app()
	save := []byte(`{"url":"https://articles.example.com/new","title":"New reading","labels":["reading","research"]}`)
	for range 2 {
		out, err := a.doAction(t.Context(), "save-1", "readeck_save_bookmark", save)
		if err != nil || !strings.Contains(out["text"].(string), "may still be in progress") {
			t.Fatalf("%v %v", err, out)
		}
	}
	if len(f.creates) != 1 {
		t.Fatal("save replay repeated the request")
	}
	if _, err := describeAction("readeck_save_bookmark", save); err != nil {
		t.Fatal(err)
	}
	progress, marked := 100, true
	if err := a.client.updateReading(t.Context(), "bm_42", &progress, &marked, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 1 || len(f.updates[0]) != 2 || f.updates[0]["read_progress"] != float64(100) || f.updates[0]["is_marked"] != true {
		t.Fatal("partial update changed omitted fields")
	}
	for _, bad := range []string{`{"url":"file:///tmp/private"}`, `{"url":"https://me@example.com"}`, `{"url":"https://example.com","labels":["same","same"]}`} {
		if _, err := describeAction("readeck_save_bookmark", []byte(bad)); err == nil {
			t.Fatal("accepted invalid save " + bad)
		}
	}
	if _, err := describeAction("readeck_update_reading", []byte(`{"bookmark_id":"bm_42"}`)); err == nil {
		t.Fatal("accepted an empty update")
	}
}

func TestPersistentReplayAndUnknownOutcome(t *testing.T) {
	f := newFakeReadeck(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	args := []byte(`{"url":"https://articles.example.com/new"}`)
	if _, err := a.doAction(t.Context(), "persist-1", "readeck_save_bookmark", args); err != nil {
		t.Fatal(err)
	}
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "persist-1", "readeck_save_bookmark", args); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 {
		t.Fatal("restart repeated creation")
	}
	key, _ := json.Marshal(saveArgs{URL: "https://articles.example.com/new"})
	raw, _ := json.Marshal(map[string]writeReceipt{"persist-1": {Key: "readeck_save_bookmark:" + string(key), Status: "pending", StartedAt: time.Now()}})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.receipts, _ = openWriteStore(filepath.Dir(store.path))
	if _, err := a.doAction(t.Context(), "persist-1", "readeck_save_bookmark", args); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatal("unknown save was retried")
	}
}

func TestLocalizedViewsAndBounds(t *testing.T) {
	f := newFakeReadeck(t)
	a := f.app()
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, ui := range []int{1, 3, 6} {
			for _, mode := range []string{"", "detail", "save", "review-save", "review-update"} {
				r := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: ui}}
				st := pageState{Mode: mode, ID: "bm_42", DraftID: "draft-1", URL: "https://articles.example.com/new", Title: "New reading", Labels: "reading"}
				v, err := a.page(t.Context(), r, st)
				if err != nil {
					t.Fatalf("%s %d %s: %v", locale, ui, mode, err)
				}
				if err := wire.CheckView(v); err != nil {
					t.Fatalf("%s %d %s: %v", locale, ui, mode, err)
				}
			}
		}
	}
	item := sampleBookmark()
	item.Note = strings.Repeat("context\n", 2000)
	v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{ID: "bm_42"}, item, strings.Repeat("reading\n", 2000), nil)
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	if newApp().status()["state"] != "not_configured" {
		t.Fatal("unconfigured extension was ready")
	}
}

func TestReadingUpdateReviewBeforeWrite(t *testing.T) {
	f := newFakeReadeck(t)
	a := f.app()
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6, Values: map[string]string{"progress": "100", "marked": "true"}}}
	r.Action = "update"
	r.State = ext.EncodeState(pageState{Mode: "detail", ID: "bm_42", DraftID: "draft-1"})
	result, err := a.action(t.Context(), r)
	if err != nil || result.View == nil || len(f.updates) != 0 {
		t.Fatalf("review wrote before confirmation: %v %+v", err, result)
	}
	if !strings.Contains(result.View.Sections[0].Items[0].Text, "100%") || !strings.Contains(result.View.Sections[0].Items[0].Text, "true") {
		t.Fatal("review does not show the submitted values")
	}
	r.Action = "confirm-update"
	r.State = result.View.State
	result, err = a.action(t.Context(), r)
	if err != nil || !result.Pop || len(f.updates) != 1 {
		t.Fatalf("confirm: %v %+v %v", err, result, f.updates)
	}
}

func TestContentTypeAndFailedWrites(t *testing.T) {
	f := newFakeReadeck(t)
	f.badMedia = true
	if _, err := f.app().client.article(t.Context(), "bm_42"); err == nil {
		t.Fatal("accepted HTML as Markdown")
	}
	f.badMedia = false
	f.failWrite = true
	if _, err := f.app().doAction(t.Context(), "failed-1", "readeck_save_bookmark", []byte(`{"url":"https://articles.example.com/new"}`)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatal("failed save was reported as safe to retry")
	}
}
