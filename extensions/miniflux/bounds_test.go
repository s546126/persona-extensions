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

func TestReceiptKeyDistinguishesActionsAndWhitespace(t *testing.T) {
	f := newFakeMiniflux(t)
	a := f.app()

	if _, err := a.doAction(t.Context(), "req-1", "miniflux_mark_read", []byte(`{"entry_id":42}`)); err != nil {
		t.Fatalf("mark_read: %v", err)
	}
	// Whitespace-only variation of the same action and args must replay idempotently.
	if _, err := a.doAction(t.Context(), "req-1", "miniflux_mark_read", []byte(`{ "entry_id" : 42 }`)); err != nil {
		t.Fatalf("whitespace replay failed: %v", err)
	}
	// Reusing the same request_id for a different action with identical args must conflict.
	if _, err := a.doAction(t.Context(), "req-1", "miniflux_toggle_bookmark", []byte(`{"entry_id":42}`)); err == nil {
		t.Fatal("reusing request_id across mark_read and toggle_bookmark silently replayed instead of failing")
	}
	if len(f.reads) != 1 || f.bookmarks != 0 {
		t.Fatalf("unexpected writes: reads=%d bookmarks=%d", len(f.reads), f.bookmarks)
	}
}

func TestListDeduplicatesAcrossOffsetsAndCountsRenderedRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/entries" {
			var entries []entry
			if r.URL.Query().Get("offset") == "0" {
				for i := 1; i <= 50; i++ {
					entries = append(entries, entry{ID: int64(i), Title: "Entry", Status: "unread"})
				}
			} else {
				// Shifted duplicate ID 50 + invalid empty-title entry + one new entry 51.
				entries = []entry{
					{ID: 50, Title: "Shifted duplicate", Status: "unread"},
					{ID: 99, Title: "", Status: "unread"},
					{ID: 51, Title: "Entry 51", Status: "unread"},
				}
			}
			_ = json.NewEncoder(w).Encode(entryPage{Total: 52, Entries: entries})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{View: "unread", Offset: 50}),
	}})
	if err != nil {
		t.Fatalf("render list: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView(list): %v", err)
	}
	if v.Sections[0].Title != "51 entries" {
		t.Fatalf("section title = %q, want %q", v.Sections[0].Title, "51 entries")
	}
}

func TestDetailViewClipsConfirmAndEmptyTitle(t *testing.T) {
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	longTitle := strings.Repeat("T", 2500)
	v := detailView(req, pageState{Mode: "detail", EntryID: 42, DraftID: "d1"}, entry{
		ID:     42,
		Title:  longTitle,
		Status: "unread",
	}, "Body")
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView(long title): %v", err)
	}
	for _, item := range v.Sections[0].Items {
		if item.Action != nil && len([]rune(item.Action.Confirm)) > 2000 {
			t.Fatalf("confirm length = %d runes, want <= 2000", len([]rune(item.Action.Confirm)))
		}
	}

	v2 := detailView(req, pageState{Mode: "detail", EntryID: 42, DraftID: "d2"}, entry{
		ID:     42,
		Title:  "",
		Status: "read",
	}, "")
	if err := wire.CheckView(v2); err != nil {
		t.Fatalf("CheckView(empty title): %v", err)
	}
	if strings.TrimSpace(v2.Title) == "" {
		t.Fatal("empty entry title left View.Title empty")
	}
}
