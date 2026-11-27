package main

import (
	"encoding/json"
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

const testToken = "ol_api_synthetic_token"

type fakeOutline struct {
	srv       *httptest.Server
	mu        sync.Mutex
	requests  []map[string]any
	creates   []map[string]any
	failWrite bool
}

const collectionID = "123e4567-e89b-42d3-a456-426614174000"
const documentID = "223e4567-e89b-42d3-a456-426614174000"

func sampleDocument() document {
	return document{ID: documentID, CollectionID: collectionID, Title: "Reading knowledge", Text: "# Context\n\nKeep source material beside the next decision.", URLID: "reading-context", UpdatedAt: "2026-11-10T10:00:00Z", PublishedAt: "2026-11-10T09:00:00Z"}
}

func newFakeOutline(t *testing.T) *fakeOutline {
	f := &fakeOutline{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, map[string]any{"path": r.URL.Path, "body": in})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/documents.search":
			if in["query"] == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": []searchResult{{Context: "Keep the source context nearby.", Ranking: 1.5, Document: sampleDocument()}}, "pagination": map[string]any{"offset": in["offset"], "limit": 25, "nextPath": "/api/documents.search?offset=25&limit=25"}})
		case "/api/documents.info":
			if in["id"] != documentID && in["id"] != "reading-context" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": sampleDocument()})
		case "/api/collections.list":
			filters, ok := in["filters"].([]any)
			if !ok || len(filters) != 1 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": []collection{{ID: collectionID, Name: "Reading notes", Permission: "read_write"}}, "pagination": map[string]any{"offset": in["offset"], "limit": 25}})
		case "/api/documents.create":
			if f.failWrite {
				http.Error(w, "synthetic interrupted write", http.StatusServiceUnavailable)
				return
			}
			if in["publish"] != false || in["title"] == "" || in["text"] == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.creates = append(f.creates, in)
			f.mu.Unlock()
			item := sampleDocument()
			item.ID, item.Title, item.PublishedAt = "323e4567-e89b-42d3-a456-426614174000", in["title"].(string), ""
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": item})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOutline) app() *app {
	return &app{client: newClient(f.srv.URL, testToken), receipts: newWriteStore("")}
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeOutline(t).app())))
}

func TestManifestAndURL(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "outline" || len(m.Secrets) != 1 || len(m.Contributes.Connectors[0].ConfirmAlways) != 1 || !slices.Equal(m.Permissions.Network, []string{}) {
		t.Fatalf("%+v", m)
	}
	for _, raw := range []string{"https://app.getoutline.com", "http://127.0.0.1:3000/", "https://[::1]:3000"} {
		if _, err := instanceURL(raw); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"http://outline.example.com", "https://me@example.com", "not-a-url"} {
		if _, err := instanceURL(raw); err == nil {
			t.Fatal("accepted " + raw)
		}
	}
	if newClient("https://app.getoutline.com", "x").base != "https://app.getoutline.com/api" {
		t.Fatal("missing API base path")
	}
}

func TestSearchReadAndCollectionFilters(t *testing.T) {
	f := newFakeOutline(t)
	a := f.app()
	text, err := a.callTool(t.Context(), "outline_search", json.RawMessage(`{"query":"context"}`))
	if err != nil || !strings.Contains(text, documentID) || !strings.Contains(text, "Next offset: 25") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "outline_document", json.RawMessage(`{"document_id":"reading-context"}`))
	if err != nil || !strings.Contains(text, "Keep source material") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "outline_collections", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(text, collectionID) {
		t.Fatalf("%v %s", err, text)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	foundFilter := false
	for _, r := range f.requests {
		if r["path"] != "/api/collections.list" {
			continue
		}
		filters := r["body"].(map[string]any)["filters"].([]any)
		filter := filters[0].(map[string]any)
		foundFilter = filter["field"] == "archivedAt" && filter["operator"] == "isNull"
	}
	if !foundFilter {
		t.Fatal("collection query did not exclude archived collections")
	}
}

func TestCreateDraftReplayAndValidation(t *testing.T) {
	f := newFakeOutline(t)
	a := f.app()
	args := []byte(`{"collection_id":"` + collectionID + `","title":"Reading decision","text":"Keep the source context."}`)
	for range 2 {
		out, err := a.doAction(t.Context(), "create-1", "outline_create_document", args)
		if err != nil || !strings.Contains(out["text"].(string), "unpublished") {
			t.Fatalf("%v %v", err, out)
		}
	}
	if len(f.creates) != 1 || f.creates[0]["publish"] != false {
		t.Fatal("draft was published or replay repeated creation")
	}
	if _, err := describeAction("outline_create_document", args); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"collection_id":"../../admin","title":"Bad","text":"Bad"}`, `{"collection_id":"` + collectionID + `","title":"Bad","text":""}`} {
		if _, err := describeAction("outline_create_document", []byte(bad)); err == nil {
			t.Fatal("accepted invalid draft " + bad)
		}
	}
	if _, err := a.doAction(t.Context(), "create-1", "outline_create_document", []byte(`{"collection_id":"`+collectionID+`","title":"Different","text":"Different"}`)); err == nil {
		t.Fatal("request ID accepted a different document")
	}
}

func TestPersistentReplayAndUnknownOutcome(t *testing.T) {
	f := newFakeOutline(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	args := []byte(`{"collection_id":"personal","title":"Private reading","text":"Keep the source."}`)
	if _, err := a.doAction(t.Context(), "persist-1", "outline_create_document", args); err != nil {
		t.Fatal(err)
	}
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "persist-1", "outline_create_document", args); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 {
		t.Fatal("restart repeated draft creation")
	}
	key, _ := json.Marshal(createArgs{CollectionID: "personal", Title: "Private reading", Text: "Keep the source."})
	raw, _ := json.Marshal(map[string]writeReceipt{"persist-1": {Key: "outline_create_document:" + string(key), Status: "pending", StartedAt: time.Now()}})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.receipts, _ = openWriteStore(filepath.Dir(store.path))
	if _, err := a.doAction(t.Context(), "persist-1", "outline_create_document", args); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatal("unknown creation was retried")
	}
}

func TestLocalizedViewsAndBounds(t *testing.T) {
	f := newFakeOutline(t)
	a := f.app()
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, ui := range []int{1, 3, 6} {
			for _, mode := range []string{"", "detail", "collections", "create", "review-create"} {
				r := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: ui}}
				st := pageState{Mode: mode, Query: "context", DocumentID: documentID, CollectionID: collectionID, Title: "Reading decision", Text: "Keep the source.", DraftID: "draft-1"}
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
	item := sampleDocument()
	item.Text = strings.Repeat("knowledge\n", 10000)
	v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{DocumentID: documentID}, item)
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	if newApp().status()["state"] != "not_configured" {
		t.Fatal("unconfigured extension was ready")
	}
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6, Values: map[string]string{"title": "阅读笔记", "text": strings.Repeat("上下文", 400)}}}
	r.Action = "review-create"
	r.State = ext.EncodeState(pageState{Mode: "create", CollectionID: "personal", DraftID: "draft-1"})
	result, err := a.action(t.Context(), r)
	if err != nil || wire.CheckView(result.View) != nil || len(f.creates) != 0 {
		t.Fatalf("UTF-8 draft exceeded navigation state: %v", err)
	}
	r.Action, r.State = "confirm-create", result.View.State
	result, err = a.action(t.Context(), r)
	if err != nil || !result.Pop || len(f.creates) != 1 {
		t.Fatalf("confirm draft: %v %+v", err, result)
	}
	r.Action = "confirm-create"
	r.State = ext.EncodeState(pageState{Mode: "create", CollectionID: "personal", DraftID: "different", Title: "Not reviewed", Text: "Must not be written"})
	if _, err := a.action(t.Context(), r); err == nil || len(f.creates) != 1 {
		t.Fatal("created an unreviewed draft")
	}
}

func TestUnsuccessfulResponseAndFailedWrite(t *testing.T) {
	f := newFakeOutline(t)
	f.failWrite = true
	if _, err := f.app().doAction(t.Context(), "failed-1", "outline_create_document", []byte(`{"collection_id":"personal","title":"Draft","text":"Source"}`)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatal("failed write was safe to retry")
	}
}
