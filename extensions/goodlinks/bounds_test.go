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

func TestDetailViewCapsArticleAndHighlightsWithinItemsMost(t *testing.T) {
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	article := strings.Repeat("article line\n", 220)
	var hs []highlight
	note := strings.Repeat("n", 2200)
	for i := range 30 {
		hs = append(hs, highlight{
			ID:      "h" + string(rune('a'+i%26)),
			Content: strings.Repeat("highlight line\n", 12),
			Note:    &note,
		})
	}
	v := detailView(req, pageState{Mode: "detail", LinkID: "link-1", DraftID: "d1"}, link{
		ID:    "link-1",
		Title: strings.Repeat("T", 2500),
	}, article, hs)
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView(long article + 30 highlights): %v", err)
	}
	for _, item := range v.Sections[0].Items {
		if item.Action != nil && len([]rune(item.Action.Confirm)) > 2000 {
			t.Fatalf("confirm length = %d runes, want <= 2000", len([]rune(item.Action.Confirm)))
		}
	}

	v2 := detailView(req, pageState{Mode: "detail", LinkID: "link-2", DraftID: "d2"}, link{
		ID:    "link-2",
		Title: "",
	}, "", nil)
	if err := wire.CheckView(v2); err != nil {
		t.Fatalf("CheckView(empty title): %v", err)
	}
	if strings.TrimSpace(v2.Title) == "" {
		t.Fatal("empty link title left View.Title empty")
	}
}

func TestToggleStarRequiresStarredAndReceiptKeyIncludesAction(t *testing.T) {
	f := newFakeGoodLinks(t)
	a := f.app()

	// Missing required "starred" field must be rejected in both describeAction and doAction.
	if _, err := describeAction("goodlinks_toggle_star", []byte(`{"link_id":"link-1"}`)); err == nil {
		t.Fatal("describeAction accepted goodlinks_toggle_star without required starred field")
	}
	if _, err := a.doAction(t.Context(), "missing-star", "goodlinks_toggle_star", []byte(`{"link_id":"link-1"}`)); err == nil {
		t.Fatal("doAction accepted goodlinks_toggle_star without required starred field")
	}

	// Receipt key must distinguish actions and normalize whitespace.
	if _, err := a.doAction(t.Context(), "req-1", "goodlinks_mark_read", []byte(`{"link_id":"link-1"}`)); err != nil {
		t.Fatalf("mark_read: %v", err)
	}
	if _, err := a.doAction(t.Context(), "req-1", "goodlinks_mark_read", []byte(`{ "link_id" : "link-1" }`)); err != nil {
		t.Fatalf("whitespace replay failed: %v", err)
	}
	if _, err := a.doAction(t.Context(), "req-1", "goodlinks_toggle_star", []byte(`{"link_id":"link-1","starred":true}`)); err == nil {
		t.Fatal("reusing request_id across mark_read and toggle_star should conflict")
	}
}

func TestListDeduplicatesBoundsLongKeysAndCountsRenderedRows(t *testing.T) {
	longID := strings.Repeat("l", 140) // "link-" + 140 = 145 > 128 wire key limit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/lists/unread" {
			if r.URL.Query().Get("offset") == "0" {
				_ = json.NewEncoder(w).Encode(linkPage{
					Data: []link{
						{ID: longID, Title: "Long ID article"},
						{ID: "dup-1", Title: "Duplicate article"},
					},
					HasMore: true,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(linkPage{
				Data: []link{
					{ID: "dup-1", Title: "Duplicate on page 2"},
					{ID: "empty-title", Title: ""},
				},
				HasMore: false,
			})
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
	if v.Sections[0].Title != "2 links" {
		t.Fatalf("section title = %q, want %q", v.Sections[0].Title, "2 links")
	}

	// Invalid st.View must return error rather than failing CheckView.
	if _, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{View: "bogus"}),
	}}); err == nil {
		t.Fatal("render accepted invalid view state")
	}
}
