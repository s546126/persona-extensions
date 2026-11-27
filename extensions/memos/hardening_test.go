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

func TestTogglePinRequiresPinnedAndCanonicalReceiptKey(t *testing.T) {
	f := newFakeMemos(t)
	a := f.app()

	if _, err := describeAction("memos_toggle_pin", []byte(`{"memo_id":"memo-1"}`)); err == nil {
		t.Fatal("describeAction accepted omitted required pinned flag")
	}
	if _, err := a.doAction(t.Context(), "pin-missing", "memos_toggle_pin", []byte(`{"memo_id":"memo-1"}`)); err == nil {
		t.Fatal("doAction accepted omitted required pinned flag")
	}
	f.mu.Lock()
	pinnedCount := len(f.updates["pinned"])
	f.mu.Unlock()
	if pinnedCount != 0 {
		t.Fatalf("omitted pinned flag triggered %d upstream PATCHes", pinnedCount)
	}

	if _, err := a.doAction(t.Context(), "pin-canon", "memos_toggle_pin", []byte(`{"memo_id":"memo-1","pinned":true}`)); err != nil {
		t.Fatalf("first pin: %v", err)
	}
	if _, err := a.doAction(t.Context(), "pin-canon", "memos_toggle_pin", []byte(`{ "pinned" : true , "memo_id" : "memo-1" }`)); err != nil {
		t.Fatalf("whitespace-equivalent retry failed: %v", err)
	}
	f.mu.Lock()
	pinnedCount = len(f.updates["pinned"])
	f.mu.Unlock()
	if pinnedCount != 1 {
		t.Fatalf("reformatted retry sent %d upstream PATCHes, want 1", pinnedCount)
	}
}

func TestEditorAndDetailViewContentBoundsAndCJKState(t *testing.T) {
	f := newFakeMemos(t)
	a := f.app()
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	// Detail view for a 2500-rune memo must stay valid, and if an edit button is offered,
	// opening its pushed edit state must also satisfy wire.CheckView.
	longASCII := sampleMemo()
	longASCII.Content = strings.Repeat("a", 2500)
	longASCII.Snippet = ""
	dv := detailView(req, pageState{Mode: "detail", MemoID: "memo-1", DraftID: "d1"}, longASCII)
	if err := wire.CheckView(dv); err != nil {
		t.Fatalf("detailView long ASCII CheckView: %v", err)
	}
	if strings.TrimSpace(dv.Title) == "" {
		t.Fatal("detailView left empty title when snippet was blank")
	}
	for _, it := range dv.Sections[0].Items {
		if it.Action != nil && it.Action.ID == "edit" && it.Action.Push != nil {
			ev, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6, State: it.Action.Push.State}})
			if err != nil {
				t.Fatalf("render pushed edit state: %v", err)
			}
			if err := wire.CheckView(ev); err != nil {
				t.Fatalf("pushed edit view failed CheckView: %v", err)
			}
		}
	}

	// Reviewing a 2500-rune ASCII draft and pressing Back to editor must pass CheckView.
	reviewReq := &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", DraftID: "d2"}),
		Values:    map[string]string{"content": strings.Repeat("b", 2500)},
	}}
	rev, err := a.action(t.Context(), reviewReq)
	if err != nil || rev.View == nil {
		t.Fatalf("review-create 2500 ASCII: %v", err)
	}
	if err := wire.CheckView(rev.View); err != nil {
		t.Fatalf("review-create CheckView: %v", err)
	}
	backRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "create",
		State:     rev.View.State,
	}})
	if err != nil || backRes.View == nil {
		t.Fatalf("back to create editor: %v", err)
	}
	if err := wire.CheckView(backRes.View); err != nil {
		t.Fatalf("back to create editor CheckView: %v", err)
	}

	// A 4000-rune Han memo (12,000 UTF-8 bytes) is valid under validContent and fits within wire.StateMost,
	// so review-create -> confirm-create must succeed instead of failing readState's byte-length limit.
	han4000 := strings.Repeat("字", 4000)
	revHan, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "zh-Hans",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", DraftID: "d-han"}),
		Values:    map[string]string{"content": han4000},
	}})
	if err != nil || revHan.View == nil {
		t.Fatalf("review-create 4000 Han: %v", err)
	}
	if err := wire.CheckView(revHan.View); err != nil {
		t.Fatalf("review-create 4000 Han CheckView: %v", err)
	}
	confirmHan, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "zh-Hans",
		UIVersion: 6,
		Action:    "confirm-create",
		State:     revHan.View.State,
	}})
	if err != nil || !confirmHan.Pop {
		t.Fatalf("confirm-create 4000 Han failed: %v %+v", err, confirmHan)
	}

	// confirm-create and confirm-edit must reject an empty DraftID.
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "confirm-create",
		State:     ext.EncodeState(pageState{Mode: "review-create", Content: "hello"}),
	}}); err == nil {
		t.Fatal("confirm-create accepted empty DraftID")
	}
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "confirm-edit",
		State:     ext.EncodeState(pageState{Mode: "review-edit", MemoID: "memo-1", Content: "hello"}),
	}}); err == nil {
		t.Fatal("confirm-edit accepted empty DraftID")
	}
}

func TestPaginationDedupLongTokenStateGuardAndEmptySearch(t *testing.T) {
	longToken := strings.Repeat("t", 2040)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		tok := r.URL.Query().Get("pageToken")
		switch tok {
		case "":
			_ = json.NewEncoder(w).Encode(memoPage{
				Memos: []memo{
					{Name: "memos/m-1", Creator: "users/reader", Snippet: "One", UpdateTime: "2026-11-10T10:00:00Z"},
					{Name: "memos/m-2", Creator: "users/reader", Snippet: "Two", UpdateTime: "2026-11-10T09:00:00Z"},
					{Name: "memos/foreign", Creator: "users/other", Snippet: "Other", UpdateTime: "2026-11-10T08:00:00Z"},
				},
				NextPageToken: longToken,
			})
		case longToken:
			_ = json.NewEncoder(w).Encode(memoPage{
				Memos: []memo{
					{Name: "memos/m-2", Creator: "users/reader", Snippet: "Two duplicate", UpdateTime: "2026-11-10T09:00:00Z"},
					{Name: "memos/m-3", Creator: "users/reader", Snippet: "Three", UpdateTime: "2026-11-10T07:00:00Z"},
				},
				NextPageToken: "",
			})
		default:
			_ = json.NewEncoder(w).Encode(memoPage{})
		}
	}))
	defer srv.Close()
	a := &app{client: newClient(srv.URL, testToken), username: "reader", creator: "users/reader", receipts: newWriteStore("")}

	// Empty query with View=="search" must render a valid empty search view instead of crashing.
	emptySearch, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{View: "search", Query: ""}),
	}})
	if err != nil {
		t.Fatalf("empty search view returned error: %v", err)
	}
	if err := wire.CheckView(emptySearch); err != nil {
		t.Fatalf("empty search view CheckView: %v", err)
	}

	// Page 1 with 2040-byte NextPageToken must pass CheckView and count only rendered rows (2, excluding foreign).
	v1, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
	if err != nil {
		t.Fatalf("render page 1: %v", err)
	}
	if err := wire.CheckView(v1); err != nil {
		t.Fatalf("page 1 with 2040-byte token failed CheckView: %v", err)
	}
	if v1.Sections[0].Title != fmt.Sprintf(textsFor("en").results, 2) {
		t.Fatalf("page 1 section title = %q, want %q", v1.Sections[0].Title, fmt.Sprintf(textsFor("en").results, 2))
	}
	if v1.Sections[0].More == nil {
		t.Fatal("page 1 missing More button")
	}

	// Loading page 2 must deduplicate m-2 and pass CheckView with 3 rendered rows.
	res2, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     v1.State,
		Action:    "more",
		Values:    v1.Sections[0].More.Action.Values,
	}})
	if err != nil {
		t.Fatalf("more action: %v", err)
	}
	if err := wire.CheckView(res2.View); err != nil {
		t.Fatalf("page 2 CheckView: %v", err)
	}
	if len(res2.View.Sections[0].Items) != 3 {
		t.Fatalf("page 2 items = %d, want 3 deduplicated rows", len(res2.View.Sections[0].Items))
	}
	if res2.View.Sections[0].Title != fmt.Sprintf(textsFor("en").results, 3) {
		t.Fatalf("page 2 section title = %q, want %q", res2.View.Sections[0].Title, fmt.Sprintf(textsFor("en").results, 3))
	}
}
