package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestCheckViewBoundsForDetailLookupAndSearchHighlights(t *testing.T) {
	// 1. detailView with empty-text notes must omit the empty notes section.
	emptyNotes := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}},
		pageState{Mode: "detail", DocumentID: 1},
		document{ID: 1, Title: "Invoice", Content: "Body", Notes: []note{{ID: 1, Note: ""}}},
		"https://paperless.example.com/api/documents/1/download/?original=true",
	)
	if err := wire.CheckView(emptyNotes); err != nil {
		t.Fatalf("detailView with empty note failed CheckView: %v", err)
	}

	// 2. detailView with long multi-line OCR + 120 notes must stay within CheckView 300-item limit.
	notes := make([]note, 120)
	for i := range notes {
		notes[i] = note{ID: int64(i + 1), Note: fmt.Sprintf("Note %d", i+1), User: struct {
			Username string `json:"username"`
		}{Username: "alice"}}
	}
	bigDoc := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}},
		pageState{Mode: "detail", DocumentID: 2},
		document{ID: 2, Title: "Big Scan", Content: strings.Repeat("ocr line\n", 260), Notes: notes},
		"https://paperless.example.com/api/documents/2/download/?original=true",
	)
	if err := wire.CheckView(bigDoc); err != nil {
		t.Fatalf("large detailView failed CheckView: %v", err)
	}

	// 3. List view with blank HTML highlights and duplicate document IDs across pages must pass CheckView.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/documents/":
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `{"count":2,"next":null,"results":[{"id":1,"title":"Invoice","created":"2026-01-01","page_count":1,"__search_hit__":{"highlights":"<span>   </span>"}},{"id":2,"title":"Receipt","created":"2026-01-02","page_count":2}]}`)
				return
			}
			fmt.Fprint(w, `{"count":2,"next":"https://paperless.example.com/api/documents/?page=2","results":[{"id":1,"title":"Invoice","created":"2026-01-01","page_count":1,"__search_hit__":{"highlights":"<span>   </span>"}}]}`)
		case "/api/tags/":
			var b strings.Builder
			b.WriteString(`{"count":320,"next":null,"results":[`)
			for i := range 320 {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, `{"id":%d,"name":"tag-%d","document_count":1}`, i+1, i+1)
			}
			b.WriteString(`]}`)
			fmt.Fprint(w, b.String())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	a := &app{client: newClient(srv.URL, "synthetic-paperless-token")}

	listView, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, State: ext.EncodeState(pageState{Query: "invoice", Page: 2}),
	}})
	if err != nil {
		t.Fatalf("list render: %v", err)
	}
	if err := wire.CheckView(listView); err != nil {
		t.Fatalf("list view with blank highlight & duplicate ID failed CheckView: %v", err)
	}

	// 4. Tags lookup view with > 300 results must stay within CheckView item limit.
	tagsView, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, State: ext.EncodeState(pageState{Mode: "tags"}),
	}})
	if err != nil {
		t.Fatalf("tags render: %v", err)
	}
	if err := wire.CheckView(tagsView); err != nil {
		t.Fatalf("tags lookup with 320 items failed CheckView: %v", err)
	}
}
