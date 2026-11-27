package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestDetailEmptyUpdatedAtUnformattedPagesTitleAndEmptyReviewEditName(t *testing.T) {
	t.Run("detailView omits empty UpdatedAt and renders no raw %d title", func(t *testing.T) {
		it := samplePage()
		it.UpdatedAt = ""
		v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "detail", PageID: 42}, it, it.Markdown)
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("detailView with empty UpdatedAt failed CheckView: %v", err)
		}
		for i, sec := range v.Sections {
			if strings.Contains(sec.Title, "%d") {
				t.Fatalf("detailView section %d has unformatted title %q", i, sec.Title)
			}
		}

		f := newFakeBookStack(t)
		pagesV, err := f.app().page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6}}, pageState{Mode: "pages", BookID: 7, BookName: "Reading"})
		if err != nil || wire.CheckView(pagesV) != nil {
			t.Fatalf("pages view failed: %v", err)
		}
		for i, sec := range pagesV.Sections {
			if strings.Contains(sec.Title, "%d") {
				t.Fatalf("pages view section %d has unformatted title %q", i, sec.Title)
			}
		}
	})

	t.Run("review-edit with empty Name passes CheckView", func(t *testing.T) {
		f := newFakeBookStack(t)
		a := f.app()
		r := &ext.UIRequest{UIParams: wire.UIParams{
			Locale:    "en",
			UIVersion: 6,
			Values:    map[string]string{"name": "", "markdown": "Updated markdown without renaming."},
		}}
		res, err := a.action(t.Context(), withAction(r, "review-edit", pageState{Mode: "edit", PageID: 42, DraftID: "draft-1"}))
		if err != nil || res.View == nil {
			t.Fatalf("review-edit with empty name failed: %v %+v", err, res)
		}
		if err := wire.CheckView(res.View); err != nil {
			t.Fatalf("review-edit with empty name failed CheckView: %v", err)
		}
	})
}

func TestEditorViewLongMarkdownAndDraftIDGuards(t *testing.T) {
	t.Run("edit push for 3500-Han Markdown renders valid editorView", func(t *testing.T) {
		it := samplePage()
		it.Markdown = strings.Repeat("章", 3500)
		v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6}}, pageState{
			Mode: "detail", BookID: 7, BookName: "Reading", PageID: 42,
		}, it, it.Markdown)
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("detailView CheckView failed: %v", err)
		}
		var pushState string
		for _, comp := range v.Sections[0].Items {
			if comp.Action != nil && comp.Action.ID == "edit" && comp.Action.Push != nil {
				pushState = comp.Action.Push.State
				break
			}
		}
		if pushState == "" {
			t.Fatal("missing edit push state")
		}
		a := newFakeBookStack(t).app()
		editV, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6, State: pushState}})
		if err != nil {
			t.Fatalf("render edit view failed: %v", err)
		}
		if err := wire.CheckView(editV); err != nil {
			t.Fatalf("edit view with 3500-Han Markdown failed CheckView: %v", err)
		}
	})

	t.Run("Back from review-create with >2000-char Markdown passes CheckView and missing DraftID rejected", func(t *testing.T) {
		a := newFakeBookStack(t).app()
		longMD := strings.Repeat("m", 2500)
		review, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "review-create",
			State:  ext.EncodeState(pageState{Mode: "create", BookID: 7, BookName: "Reading", DraftID: "draft-long"}),
			Values: map[string]string{"name": "Long note", "markdown": longMD},
		}})
		if err != nil || review.View == nil || wire.CheckView(review.View) != nil {
			t.Fatalf("review-create failed: %v", err)
		}
		back, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "create", State: review.View.State,
		}})
		if err != nil || back.View == nil {
			t.Fatalf("back from review-create failed: %v", err)
		}
		if err := wire.CheckView(back.View); err != nil {
			t.Fatalf("editorView after Back with 2500-char Markdown failed CheckView: %v", err)
		}

		if _, err := a.action(t.Context(), withAction(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, "confirm-create", pageState{
			Mode: "review-create", BookID: 7, Name: "Note", Markdown: "Body",
		})); err == nil {
			t.Fatal("confirm-create accepted empty DraftID")
		}
		if _, err := a.action(t.Context(), withAction(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, "confirm-edit", pageState{
			Mode: "review-edit", PageID: 42, Name: "Note", Markdown: "Body",
		})); err == nil {
			t.Fatal("confirm-edit accepted empty DraftID")
		}
	})
}

func TestLongBookNameListDedupBoundedSearchKeyAndCanonicalReceiptKey(t *testing.T) {
	t.Run("long book name, duplicate pages, and long search type key pass CheckView", func(t *testing.T) {
		longBook := strings.Repeat("K", 2100)
		longType := strings.Repeat("type", 40)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/books":
				_ = json.NewEncoder(w).Encode(listPage[book]{Data: []book{
					{ID: 7, Name: longBook, Description: "First"},
					{ID: 7, Name: longBook, Description: "Duplicate"},
				}, Total: 2})
			case "/api/pages":
				_ = json.NewEncoder(w).Encode(listPage[page]{Data: []page{
					{ID: 42, BookID: 7, Name: "Page A", UpdatedAt: "2026-11-10T10:00:00Z"},
					{ID: 42, BookID: 7, Name: "Page A duplicate", UpdatedAt: "2026-11-10T10:00:00Z"},
				}, Total: 2})
			case "/api/search":
				_ = json.NewEncoder(w).Encode(listPage[searchResult]{Data: []searchResult{
					{ID: 42, BookID: 7, Name: "Hit 1", Type: longType},
					{ID: 42, BookID: 7, Name: "Hit 1 dup", Type: longType},
				}, Total: 2})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)

		a := &app{client: newClient(srv.URL, testTokenID, testTokenSecret), receipts: newWriteStore("")}
		r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

		booksV, err := a.page(t.Context(), r, pageState{})
		if err != nil {
			t.Fatalf("books view error: %v", err)
		}
		if err := wire.CheckView(booksV); err != nil {
			t.Fatalf("books view with 2100-char name and duplicate ID failed CheckView: %v", err)
		}
		row := booksV.Sections[0].Items[0]
		openRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: row.Action.ID, State: booksV.State, Values: row.Action.Values,
		}})
		if err != nil || openRes.View == nil {
			t.Fatalf("opening long-named book failed: %v %+v", err, openRes)
		}
		if err := wire.CheckView(openRes.View); err != nil {
			t.Fatalf("pages view with duplicate ID failed CheckView: %v", err)
		}
		if openRes.View.Sections[0].Title != fmt.Sprintf(textsFor("en").pages, 1) {
			t.Fatalf("pages section title = %q, want %q", openRes.View.Sections[0].Title, fmt.Sprintf(textsFor("en").pages, 1))
		}

		searchV, err := a.page(t.Context(), r, pageState{Mode: "search", Search: "hit", SearchPage: 1})
		if err != nil {
			t.Fatalf("search view error: %v", err)
		}
		if err := wire.CheckView(searchV); err != nil {
			t.Fatalf("search view with duplicate ID and long Type failed CheckView: %v", err)
		}
	})

	t.Run("canonical action-scoped receipt key", func(t *testing.T) {
		f := newFakeBookStack(t)
		a := f.app()
		store, err := openWriteStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		a.receipts = store
		if _, err := a.doAction(t.Context(), "canon-1", "bookstack_create_page", []byte(`{"book_id":7,"name":"Note","markdown":"Body"}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.doAction(t.Context(), "canon-1", "bookstack_create_page", []byte(`{ "markdown" : "Body" , "name" : "Note" , "book_id" : 7 }`)); err != nil {
			t.Fatalf("replayed canonical JSON rejected: %v", err)
		}
		if len(f.creates) != 1 {
			t.Fatalf("creates=%d, want 1", len(f.creates))
		}
		data, err := os.ReadFile(store.path)
		if err != nil {
			t.Fatal(err)
		}
		var saved map[string]writeReceipt
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(saved["canon-1"].Key, "bookstack_create_page:") {
			t.Fatalf("receipt key %q is not action-scoped", saved["canon-1"].Key)
		}
	})
}
