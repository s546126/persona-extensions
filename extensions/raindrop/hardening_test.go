package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestReviewSaveAndSaveViewLongURLNoteAndStateMost(t *testing.T) {
	f := newFakeRaindrop(t)
	a := f.app()
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	longURL := "https://example.com/article?q=" + strings.Repeat("x", 2500)
	longNote := strings.Repeat("n", 2500)

	rev, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-save",
		State:     ext.EncodeState(pageState{Mode: "form", DraftID: "d1"}),
		Values: map[string]string{
			"url":        longURL,
			"title":      "Long save",
			"note":       longNote,
			"collection": "-1",
		},
	}})
	if err != nil || rev.View == nil {
		t.Fatalf("review-save long URL: %v", err)
	}
	if err := wire.CheckView(rev.View); err != nil {
		t.Fatalf("reviewSaveView long URL CheckView: %v", err)
	}

	formView, err := a.saveView(t.Context(), req, pageState{
		Mode:    "form",
		DraftID: "d1",
		Save:    saveArgs{URL: longURL, Title: "Long save", Note: longNote, CollectionID: -1},
	})
	if err != nil {
		t.Fatalf("saveView long URL/Note: %v", err)
	}
	if err := wire.CheckView(formView); err != nil {
		t.Fatalf("saveView long URL/Note CheckView: %v", err)
	}

	// 7000 Han runes (21,000 bytes) exceed wire.StateMost and must fail cleanly rather than returning an invalid view.
	if huge, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "zh-Hans",
		UIVersion: 6,
		Action:    "review-save",
		State:     ext.EncodeState(pageState{Mode: "form", DraftID: "d2"}),
		Values: map[string]string{
			"url":        "https://example.com/han",
			"note":       strings.Repeat("字", 7000),
			"collection": "-1",
		},
	}}); err == nil && (huge == nil || huge.View == nil || wire.CheckView(huge.View) != nil) {
		t.Fatal("review-save with >16 KiB CJK note returned invalid view instead of error")
	}
}

func TestUntitledBookmarkDuplicatePagesAndEmptyTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/rest/v1/collections", "/rest/v1/collections/childrens":
			_ = json.NewEncoder(w).Encode(map[string]any{"result": true, "items": []any{}})
		case "/rest/v1/raindrops/0":
			if r.URL.Query().Get("page") == "0" {
				items := make([]raindrop, 50)
				items[0] = raindrop{ID: 42, Title: "", Link: "https://example.com/untitled", Domain: "example.com"}
				for i := 1; i < 50; i++ {
					items[i] = raindrop{ID: int64(100 + i), Title: "Item", Link: "https://example.com/item", Domain: "example.com"}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"result": true, "items": items, "count": 52, "page": 0})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": true,
				"items": []raindrop{
					{ID: 42, Title: "", Link: "https://example.com/untitled", Domain: "example.com"},
					{ID: 200, Title: "Second page item", Link: "https://example.com/200", Domain: "example.com"},
				},
				"count": 52,
				"page":  1,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	a := &app{api: newAPIClient(srv.URL, "tok"), receipts: newSaveStore("")}

	v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "list", Pages: 1})
	if err != nil {
		t.Fatalf("list with untitled + duplicate IDs: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("list with untitled + duplicate IDs failed CheckView: %v", err)
	}

	dv := a.detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "detail", Bookmark: 42}, raindrop{
		ID:     42,
		Title:  "",
		Link:   "https://example.com/untitled",
		Domain: "example.com",
		Tags:   []string{"", "   "},
	})
	if err := wire.CheckView(dv); err != nil {
		t.Fatalf("detailView blank tags failed CheckView: %v", err)
	}
}

func TestLongSearchQueryDetailGuardAndHanToolQuery(t *testing.T) {
	f := newFakeRaindrop(t)
	a := f.app()

	res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "search",
		Values:    map[string]string{"q": strings.Repeat("q", 2500)},
	}})
	if err != nil || res.View == nil {
		t.Fatalf("long search action: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("long search view failed CheckView: %v", err)
	}

	if _, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "detail", Bookmark: 0}); err == nil {
		t.Fatal("detail mode accepted Bookmark == 0")
	}

	raw, _ := json.Marshal(map[string]any{"query": strings.Repeat("字", 200)})
	if _, err := a.callTool(t.Context(), "raindrop_search", raw); err != nil {
		t.Fatalf("raindrop_search rejected 200-rune Han query: %v", err)
	}
}
