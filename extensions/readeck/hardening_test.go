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

func TestSearchResultViewPassesCheckViewAndEmptySearch(t *testing.T) {
	f := newFakeReadeck(t)
	a := f.app()

	res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "search",
		Values:    map[string]string{"q": "context"},
	}})
	if err != nil || res.View == nil {
		t.Fatalf("search action: %v %+v", err, res)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("search result view failed CheckView: %v", err)
	}

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
}

func TestLongBookmarkIDDuplicatePagesAndDetailEmptyTitle(t *testing.T) {
	longID := strings.Repeat("b", 128)
	secondID := strings.Repeat("c", 128)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("offset") {
		case "0":
			w.Header().Set("Total-Count", "2")
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/bookmarks?offset=30&limit=30>; rel="next"`, srv.URL))
			_ = json.NewEncoder(w).Encode([]bookmark{
				{ID: longID, Title: "First long ID", SiteName: "Example", ReadProgress: 10},
			})
		default:
			w.Header().Set("Total-Count", "2")
			_ = json.NewEncoder(w).Encode([]bookmark{
				{ID: longID, Title: "First long ID duplicate", SiteName: "Example", ReadProgress: 10},
				{ID: secondID, Title: "Second long ID", SiteName: "Example", ReadProgress: 50},
			})
		}
	}))
	defer srv.Close()
	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}

	v1, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
	if err != nil {
		t.Fatalf("render page 1: %v", err)
	}
	if err := wire.CheckView(v1); err != nil {
		t.Fatalf("page 1 with 128-char ID failed CheckView: %v", err)
	}
	if v1.Sections[0].More == nil {
		t.Fatal("page 1 missing More")
	}

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
		t.Fatalf("page 2 with duplicate long ID failed CheckView: %v", err)
	}
	if len(res2.View.Sections[0].Items) != 2 {
		t.Fatalf("page 2 items = %d, want 2 deduplicated rows", len(res2.View.Sections[0].Items))
	}

	dv := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "detail", ID: "bm_empty"}, bookmark{
		ID:  "bm_empty",
		URL: "https://articles.example.com/untitled",
	}, "", nil)
	if err := wire.CheckView(dv); err != nil {
		t.Fatalf("detailView empty title CheckView: %v", err)
	}
	if strings.TrimSpace(dv.Title) == "" {
		t.Fatal("detailView left empty title")
	}
}

func TestSaveViewBackWithTwentyLongLabelsPassesCheckView(t *testing.T) {
	suffixes := []rune("甲乙丙丁戊己庚辛壬癸子丑寅卯辰巳午未申酉")
	labels := make([]string, len(suffixes))
	for i, suffix := range suffixes {
		labels[i] = strings.Repeat("字", 99) + string(suffix)
	}
	f := newFakeReadeck(t)
	a := f.app()

	rev, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "zh-Hans",
		UIVersion: 6,
		Action:    "review-save",
		State:     ext.EncodeState(pageState{Mode: "save", DraftID: "draft-1"}),
		Values: map[string]string{
			"url":    "https://articles.example.com/a",
			"title":  "Long labels",
			"labels": strings.Join(labels, ", "),
		},
	}})
	if err != nil || rev.View == nil {
		t.Fatalf("review-save: %v", err)
	}
	if err := wire.CheckView(rev.View); err != nil {
		t.Fatalf("review-save CheckView: %v", err)
	}
	back, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "zh-Hans",
		UIVersion: 6,
		Action:    "save",
		State:     rev.View.State,
	}})
	if err != nil || back.View == nil {
		t.Fatalf("back to save form: %v", err)
	}
	if err := wire.CheckView(back.View); err != nil {
		t.Fatalf("save form after back failed CheckView: %v", err)
	}
}

func TestCanonicalActionScopedReceiptKey(t *testing.T) {
	f := newFakeReadeck(t)
	a := f.app()

	if _, err := a.doAction(t.Context(), "canon-1", "readeck_save_bookmark", []byte(`{"url":"https://articles.example.com/new","title":"T"}`)); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := a.doAction(t.Context(), "canon-1", "readeck_save_bookmark", []byte(`{ "title" : "T" , "url" : "https://articles.example.com/new" }`)); err != nil {
		t.Fatalf("whitespace-equivalent retry failed: %v", err)
	}
	f.mu.Lock()
	creates := len(f.creates)
	f.mu.Unlock()
	if creates != 1 {
		t.Fatalf("creates = %d, want 1", creates)
	}
}
