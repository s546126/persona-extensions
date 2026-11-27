package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestCreatePreservesUpstreamErrorsAndCanonicalReceiptKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, errToken},
		{"rate_limited", http.StatusTooManyRequests, errRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			c := newClient(srv.URL, "tok")
			_, err := c.create(t.Context(), "personal", "Title", "Body")
			if !errors.Is(err, tc.want) {
				t.Fatalf("create HTTP %d err = %v, want %v", tc.status, err, tc.want)
			}
		})
	}

	f := newFakeOutline(t)
	a := f.app()
	if _, err := a.doAction(t.Context(), "canon-1", "outline_create_document", []byte(`{"collection_id":"personal","title":"Note","text":"Body"}`)); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := a.doAction(t.Context(), "canon-1", "outline_create_document", []byte(`{ "text" : "Body" , "title" : "Note" , "collection_id" : "personal" }`)); err != nil {
		t.Fatalf("whitespace-equivalent retry failed: %v", err)
	}
	f.mu.Lock()
	creates := len(f.creates)
	f.mu.Unlock()
	if creates != 1 {
		t.Fatalf("creates = %d, want 1", creates)
	}
}

func TestCreateViewBackLongDraftAndEmptyTitles(t *testing.T) {
	f := newFakeOutline(t)
	a := f.app()

	rev, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", CollectionID: "personal", DraftID: "d1"}),
		Values: map[string]string{
			"title": "Long draft",
			"text":  strings.Repeat("x", 3000),
		},
	}})
	if err != nil || rev.View == nil {
		t.Fatalf("review-create 3000-byte text: %v", err)
	}
	if err := wire.CheckView(rev.View); err != nil {
		t.Fatalf("review-create CheckView: %v", err)
	}
	back, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "create",
		State:     rev.View.State,
	}})
	if err != nil || back.View == nil {
		t.Fatalf("back to createView: %v", err)
	}
	if err := wire.CheckView(back.View); err != nil {
		t.Fatalf("createView after back failed CheckView: %v", err)
	}

	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	if err := wire.CheckView(createView(req, pageState{Mode: "review-create", Title: "", Text: "Body"})); err != nil {
		t.Fatalf("review-create empty title CheckView: %v", err)
	}
	dv := detailView(req, pageState{Mode: "detail", DocumentID: "doc-1"}, document{ID: "doc-1", Title: "", Text: "Body"})
	if err := wire.CheckView(dv); err != nil {
		t.Fatalf("detailView empty title CheckView: %v", err)
	}
	if strings.TrimSpace(dv.Title) == "" {
		t.Fatal("detailView left empty title")
	}
}

func TestLongDocumentIDDuplicatePagesAndCollections(t *testing.T) {
	longID := strings.Repeat("d", 128)
	secondID := strings.Repeat("e", 128)
	colUUID := "11111111-2222-3333-4444-555555555555"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/documents.search":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			offset, _ := body["offset"].(float64)
			if offset == 0 {
				_ = json.NewEncoder(w).Encode(envelope[[]searchResult]{
					OK: true,
					Data: []searchResult{
						{Document: document{ID: longID, Title: "First long doc"}, Context: "Snippet 1"},
					},
					Pagination: pagination{Offset: 0, Limit: 25, NextPath: "/api/documents.search?offset=25&limit=25"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(envelope[[]searchResult]{
				OK: true,
				Data: []searchResult{
					{Document: document{ID: longID, Title: "First long doc duplicate"}, Context: "Snippet 1"},
					{Document: document{ID: secondID, Title: "Second long doc"}, Context: "Snippet 2"},
				},
				Pagination: pagination{Offset: 25, Limit: 25},
			})
		case "/api/collections.list":
			_ = json.NewEncoder(w).Encode(envelope[[]collection]{
				OK: true,
				Data: []collection{
					{ID: colUUID, Name: "", Permission: "read_write"},
					{ID: colUUID, Name: "Duplicate", Permission: "read_write"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	a := &app{client: newClient(srv.URL, "tok"), receipts: newWriteStore("")}

	v1, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Query: "context", Offset: 25}),
	}})
	if err != nil {
		t.Fatalf("render search page: %v", err)
	}
	if err := wire.CheckView(v1); err != nil {
		t.Fatalf("search with 128-char duplicate IDs failed CheckView: %v", err)
	}

	cols, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Mode: "collections"}),
	}})
	if err != nil {
		t.Fatalf("render collections: %v", err)
	}
	if err := wire.CheckView(cols); err != nil {
		t.Fatalf("collections with nameless/duplicate UUID failed CheckView: %v", err)
	}
}
