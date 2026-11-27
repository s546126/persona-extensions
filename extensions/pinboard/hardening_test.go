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

func TestViewFilterRecentAndWrappedTagList(t *testing.T) {
	f := newFakePinboard(t)
	a := f.app()

	res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "view",
		Values:    map[string]string{"view": "recent"},
	}})
	if err != nil || res.View == nil {
		t.Fatalf("switching view filter to recent failed: %v %+v", err, res)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("recent view CheckView: %v", err)
	}

	var tags tagList
	if err := json.Unmarshal([]byte(`{"tags":[{"tag":"golang","count":4},{"tag":"reading","count":2}]}`), &tags); err != nil {
		t.Fatalf("unmarshal wrapped tagList: %v", err)
	}
	if len(tags) != 2 || tags[0].Tag != "golang" || tags[0].Count != 4 {
		t.Fatalf("wrapped tagList decoded as %+v, want 2 tags", tags)
	}
}

func TestCreateViewStateBoundsCJKTagsAndDraftID(t *testing.T) {
	f := newFakePinboard(t)
	a := f.app()

	// 3000-rune ASCII notes fit StateMost in review-create, and pressing Back must still pass CheckView.
	rev, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", DraftID: "d1"}),
		Values: map[string]string{
			"url":   "https://articles.example.com/" + strings.Repeat("a", 2100),
			"title": "Long bookmark",
			"notes": strings.Repeat("n", 3000),
			"tags":  "reading",
		},
	}})
	if err != nil || rev.View == nil {
		t.Fatalf("review-create 3000-rune notes: %v", err)
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

	// 20,000-rune notes exceed wire.StateMost (16 KiB) and must be rejected cleanly before returning a broken view.
	if huge, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", DraftID: "d2"}),
		Values: map[string]string{
			"url":   "https://articles.example.com/huge",
			"title": "Huge note",
			"notes": strings.Repeat("x", 20000),
		},
	}}); err == nil && (huge == nil || huge.View == nil || wire.CheckView(huge.View) != nil) {
		t.Fatalf("huge review-create returned invalid view instead of error")
	}

	// 3 CJK tags totaling 120 runes (360 UTF-8 bytes) are valid under validTags and must survive Load more.
	cjkTags := strings.Repeat("字", 40) + " " + strings.Repeat("文", 40) + " " + strings.Repeat("签", 40)
	searched, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "zh-Hans",
		UIVersion: 6,
		Action:    "search",
		Values:    map[string]string{"q": cjkTags},
	}})
	if err != nil || searched.View == nil {
		t.Fatalf("search CJK tags: %v", err)
	}
	if err := wire.CheckView(searched.View); err != nil {
		t.Fatalf("search CJK tags CheckView: %v", err)
	}
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "zh-Hans",
		UIVersion: 6,
		Action:    "more",
		State:     searched.View.State,
	}}); err != nil {
		t.Fatalf("more after CJK tag search failed: %v", err)
	}

	// confirm-create must reject empty DraftID.
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "confirm-create",
		State:     ext.EncodeState(pageState{Mode: "review-create", URL: "https://articles.example.com/a", Title: "T"}),
	}}); err == nil {
		t.Fatal("confirm-create accepted empty DraftID")
	}
}

func TestListItemCapAndCanonicalReceiptKey(t *testing.T) {
	const tok = "user:hardening-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("auth_token") != tok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		items := make([]bookmark, 320)
		for i := range items {
			items[i] = bookmark{
				Href:        fmt.Sprintf("https://example.com/%d", i),
				Description: fmt.Sprintf("Bookmark %d", i),
				Time:        "2026-11-10T00:00:00Z",
			}
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer srv.Close()
	a := &app{client: newClient(srv.URL, tok), receipts: newWriteStore("")}

	v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
	if err != nil {
		t.Fatalf("render 320 bookmarks: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("320 bookmarks exceeded CheckView limit: %v", err)
	}

	f := newFakePinboard(t)
	fa := f.app()
	if _, err := fa.doAction(t.Context(), "canon-1", "pinboard_add", []byte(`{"url":"https://articles.example.com/1","title":"One"}`)); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if _, err := fa.doAction(t.Context(), "canon-1", "pinboard_add", []byte(`{ "title" : "One" , "url" : "https://articles.example.com/1" }`)); err != nil {
		t.Fatalf("whitespace-equivalent retry failed: %v", err)
	}
	f.mu.Lock()
	adds := f.adds
	f.mu.Unlock()
	if adds != 1 {
		t.Fatalf("adds = %d, want 1", adds)
	}
}
