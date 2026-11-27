package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestDetailViewHandlesHTTPBookmarkAndUnmatchedHighlights(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/bookmarks/http-bm":
			fmt.Fprint(w, `{"id":"http-bm","title":"HTTP article","content":{"type":"link","url":"http://example.com/article","text":"Saved body"}}`)
		case "/bookmarks/plain-bm":
			fmt.Fprint(w, `{"id":"plain-bm","title":"Plain article","content":{"type":"link","url":"https://example.com/plain","text":"Saved body"}}`)
		case "/bookmarks/https-bm":
			fmt.Fprint(w, `{"id":"https-bm","title":"HTTPS article","content":{"type":"link","url":"https://example.com/https","text":"Saved body"}}`)
		case "/bookmarks/http-bm/highlights":
			fmt.Fprint(w, `{"highlights":[{"id":"h1","bookmarkId":"http-bm","text":"Highlighted HTTP"}]}`)
		case "/bookmarks/plain-bm/highlights":
			fmt.Fprint(w, `{"highlights":[{"id":"h-other","bookmarkId":"other-bm","text":"Unmatched"}]}`)
		case "/bookmarks/https-bm/highlights":
			fmt.Fprint(w, `{"highlights":[{"id":"h2","bookmarkId":"https-bm","text":"Highlighted HTTPS"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	a := &app{client: newClient(srv.URL, testKey), receipts: newWriteStore("")}

	for _, tc := range []struct {
		name          string
		id            string
		wantLink      bool
		wantHighlight bool
	}{
		{"http bookmark", "http-bm", false, true},
		{"unmatched highlights", "plain-bm", true, false},
		{"https bookmark with highlight", "https-bm", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6, Action: "detail", Values: map[string]string{"id": tc.id}}}
			res, err := a.action(t.Context(), r)
			if err != nil || res.View == nil {
				t.Fatalf("detail: %+v, %v", res, err)
			}
			if err := wire.CheckView(res.View); err != nil {
				t.Fatalf("CheckView: %v", err)
			}
			hasLink := slices.ContainsFunc(res.View.Sections[0].Items, func(c wire.Component) bool {
				return c.Type == wire.ComponentLink
			})
			if hasLink != tc.wantLink {
				t.Fatalf("hasLink=%v, want %v", hasLink, tc.wantLink)
			}
			if got := len(res.View.Sections) == 3; got != tc.wantHighlight {
				t.Fatalf("hasHighlightSection=%v, want %v (sections=%d)", got, tc.wantHighlight, len(res.View.Sections))
			}
		})
	}
}

func TestArchiveSendsTrueAndDetailAssignsFreshDraftID(t *testing.T) {
	var mu sync.Mutex
	var patches []struct {
		path string
		body map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode patch: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			patches = append(patches, struct {
				path string
				body map[string]any
			}{path: r.URL.Path, body: body})
			mu.Unlock()
			fmt.Fprint(w, `{}`)
			return
		}
		switch r.URL.Path {
		case "/bookmarks/bm-1":
			fmt.Fprint(w, `{"id":"bm-1","title":"First","content":{"type":"link","url":"https://example.com/1"}}`)
		case "/bookmarks/bm-2":
			fmt.Fprint(w, `{"id":"bm-2","title":"Second","content":{"type":"link","url":"https://example.com/2"}}`)
		case "/bookmarks/bm-1/highlights", "/bookmarks/bm-2/highlights":
			fmt.Fprint(w, `{"highlights":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	a := &app{client: newClient(srv.URL, testKey), receipts: newWriteStore("")}

	for _, id := range []string{"bm-1", "bm-2"} {
		detailRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "detail", Values: map[string]string{"id": id},
		}})
		if err != nil || detailRes.View == nil {
			t.Fatalf("detail %s: %+v, %v", id, detailRes, err)
		}
		archRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "archive", State: detailRes.View.State,
		}})
		if err != nil || !archRes.Pop {
			t.Fatalf("archive %s: %+v, %v", id, archRes, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(patches) != 2 {
		t.Fatalf("got %d patches, want 2: %+v", len(patches), patches)
	}
	for i, wantPath := range []string{"/bookmarks/bm-1", "/bookmarks/bm-2"} {
		if patches[i].path != wantPath || patches[i].body["archived"] != true || len(patches[i].body) != 1 {
			t.Fatalf("patch %d = %+v, want path=%s archived=true", i, patches[i], wantPath)
		}
	}

	// Missing DraftID must be refused rather than sharing an empty receipt key.
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, Action: "archive", State: ext.EncodeState(pageState{Mode: "detail", BookmarkID: "bm-1"}),
	}}); err == nil {
		t.Fatal("archive with empty DraftID succeeded")
	}
}

func TestDetailReviewSaveAndListCheckViewBounds(t *testing.T) {
	// Empty/newline-only note and content text must not emit empty sections.
	emptyDetail := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}},
		pageState{Mode: "detail", BookmarkID: "bm-1", DraftID: "d1"},
		bookmark{ID: "bm-1", Title: "Title", Note: "\n\n", Content: bookmarkContent{Type: "text", Text: "\n"}},
		[]highlight{{ID: "h1", BookmarkID: "bm-1", Text: "\n", Note: ""}},
	)
	if err := wire.CheckView(emptyDetail); err != nil {
		t.Fatalf("whitespace-only detailView failed CheckView: %v", err)
	}

	// Oversized multi-line note, text, highlights, and 2500-rune title must stay within CheckView limits.
	manyLines := strings.Repeat("line\n", 350)
	var manyHighlights []highlight
	for i := range 100 {
		manyHighlights = append(manyHighlights, highlight{ID: fmt.Sprintf("h-%d", i), BookmarkID: "bm-big", Text: "Highlight line\nsecond line", Note: "Note"})
	}
	bigDetail := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}},
		pageState{Mode: "detail", BookmarkID: "bm-big", DraftID: "d2"},
		bookmark{
			ID:      "bm-big",
			Title:   strings.Repeat("T", 2500),
			Note:    manyLines,
			Content: bookmarkContent{Type: "link", URL: "https://example.com/a", Text: manyLines},
		},
		manyHighlights,
	)
	if err := wire.CheckView(bigDetail); err != nil {
		t.Fatalf("large detailView failed CheckView: %v", err)
	}

	// Long SaveURL (> 2000 chars, <= 8192 bytes) in reviewSaveView must pass CheckView.
	longURL := "https://example.com/" + strings.Repeat("a", 2500)
	review := reviewSaveView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}},
		pageState{Mode: "review-save", DraftID: "d3", SaveURL: longURL})
	if err := wire.CheckView(review); err != nil {
		t.Fatalf("reviewSaveView with long URL failed CheckView: %v", err)
	}

	// List view with 128-char bookmark IDs and duplicate IDs must pass CheckView.
	longID := strings.Repeat("x", 125)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"bookmarks":[{"id":%q,"title":"One","content":{"type":"link","url":"https://example.com/1"}},{"id":%q,"title":"Dup","content":{"type":"link","url":"https://example.com/2"}}]}`, longID, longID)
	}))
	t.Cleanup(srv.Close)
	a := &app{client: newClient(srv.URL, testKey), receipts: newWriteStore("")}
	listView, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
	if err != nil {
		t.Fatalf("list render: %v", err)
	}
	if err := wire.CheckView(listView); err != nil {
		t.Fatalf("list view with long/duplicate IDs failed CheckView: %v", err)
	}

	// Overlong search query in pageState must be rejected.
	if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(pageState{Query: strings.Repeat("q", 250)})}}); err == nil {
		t.Fatal("readState accepted query over 200 characters")
	}
}
