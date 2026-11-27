package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestReviewEmptyNoteAndEditorBackLongValues(t *testing.T) {
	f := newFakeLibrary(t)
	a := f.app()

	// Clearing a highlight note (empty note) must pass CheckView on review-note.
	revEmpty, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-note",
		State:     ext.EncodeState(pageState{Mode: "edit-note", HighlightID: 42, DraftID: "d-empty"}),
		Values:    map[string]string{"note": ""},
	}})
	if err != nil || revEmpty.View == nil {
		t.Fatalf("review-note empty: %v", err)
	}
	if err := wire.CheckView(revEmpty.View); err != nil {
		t.Fatalf("review-note empty CheckView: %v", err)
	}

	// Returning from review-create with >2000-char text and source_url must clip Field.Value.
	revCreate, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", DraftID: "d-long"}),
		Values: map[string]string{
			"text":       strings.Repeat("h", 2500),
			"title":      "Long highlight",
			"note":       strings.Repeat("n", 2500),
			"source_url": "https://example.com/" + strings.Repeat("u", 2005),
		},
	}})
	if err != nil || revCreate.View == nil {
		t.Fatalf("review-create long: %v", err)
	}
	if err := wire.CheckView(revCreate.View); err != nil {
		t.Fatalf("review-create long CheckView: %v", err)
	}
	backCreate, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "create",
		State:     revCreate.View.State,
	}})
	if err != nil || backCreate.View == nil {
		t.Fatalf("back to create: %v", err)
	}
	if err := wire.CheckView(backCreate.View); err != nil {
		t.Fatalf("create after back CheckView: %v", err)
	}

	// Returning from review-note with >2000-char note must clip Field.Value.
	revLongNote, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-note",
		State:     ext.EncodeState(pageState{Mode: "edit-note", HighlightID: 42, DraftID: "d-long-note"}),
		Values:    map[string]string{"note": strings.Repeat("n", 2500)},
	}})
	if err != nil || revLongNote.View == nil {
		t.Fatalf("review-note long: %v", err)
	}
	backNote, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "edit-note",
		State:     revLongNote.View.State,
	}})
	if err != nil || backNote.View == nil {
		t.Fatalf("back to edit-note: %v", err)
	}
	if err := wire.CheckView(backNote.View); err != nil {
		t.Fatalf("edit-note after back CheckView: %v", err)
	}
}

func TestConfirmRequiresDraftIDAndCanonicalReceiptKey(t *testing.T) {
	f := newFakeLibrary(t)
	a := f.app()

	for _, tc := range []struct {
		action string
		st     pageState
	}{
		{"confirm-create", pageState{Mode: "review-create", Text: "Text", Title: "Title"}},
		{"confirm-note", pageState{Mode: "review-note", HighlightID: 42, Note: "Note"}},
		{"confirm-tag", pageState{Mode: "review-tag", BookID: 7, TagName: "reading"}},
	} {
		if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale:    "en",
			UIVersion: 6,
			Action:    tc.action,
			State:     ext.EncodeState(tc.st),
		}}); err == nil {
			t.Fatalf("%s accepted empty DraftID", tc.action)
		}
	}

	first := []byte(`{"highlight_id":42,"note":"Updated"}`)
	reordered := []byte(`{ "note": "Updated", "highlight_id": 42 }`)
	if _, err := a.doAction(t.Context(), "canon-1", "readwise_library_update_note", first); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if _, err := a.doAction(t.Context(), "canon-1", "readwise_library_update_note", reordered); err != nil {
		t.Fatalf("reordered retry failed: %v", err)
	}
	if len(f.updates) != 1 {
		t.Fatalf("reordered retry updated twice: %d", len(f.updates))
	}
}

func TestBooksAndHighlightsBoundsDedupAndLongBookTitle(t *testing.T) {
	books := []book{
		{ID: 7, Title: strings.Repeat("B", 2200), Author: "Author", NumHighlights: 3},
		{ID: 7, Title: "Duplicate book", Author: "Author", NumHighlights: 3},
	}
	for i := range 310 {
		books = append(books, book{ID: int64(100 + i), Title: fmt.Sprintf("Book %d", i)})
	}
	highlights := []highlight{
		{ID: 42, Text: "Highlight one"},
		{ID: 42, Text: "Duplicate highlight"},
	}
	for i := range 310 {
		highlights = append(highlights, highlight{ID: int64(1000 + i), Text: fmt.Sprintf("Highlight %d", i)})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/books/":
			_ = json.NewEncoder(w).Encode(page[book]{Count: len(books), Results: books})
		case "/api/v2/highlights/":
			_ = json.NewEncoder(w).Encode(page[highlight]{Count: len(highlights), Results: highlights})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}

	bv, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
	if err != nil {
		t.Fatalf("render books: %v", err)
	}
	if err := wire.CheckView(bv); err != nil {
		t.Fatalf("books CheckView: %v", err)
	}
	hv, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "book",
		State:     bv.State,
		Values:    bv.Sections[0].Items[0].Action.Values,
	}})
	if err != nil || hv.View == nil {
		t.Fatalf("open long-titled book: %v", err)
	}
	if err := wire.CheckView(hv.View); err != nil {
		t.Fatalf("highlights CheckView: %v", err)
	}

	dv := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "detail", HighlightID: 42}, highlight{
		ID:           42,
		Text:         "Sample",
		Location:     5,
		LocationType: strings.Repeat("loc", 900),
	})
	if err := wire.CheckView(dv); err != nil {
		t.Fatalf("detailView long location CheckView: %v", err)
	}
}
