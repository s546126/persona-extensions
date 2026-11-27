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

func TestSwitchingToSearchViewWithEmptyQueryDoesNotError(t *testing.T) {
	f := newFakeJellyfin(t)
	a := f.app()

	res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "view",
		Values:    map[string]string{"view": "search"},
	}})
	if err != nil {
		t.Fatalf("switching to search view with empty query failed: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("CheckView(empty search view): %v", err)
	}
	if res.View.Filter == nil || res.View.Filter.Value != "search" {
		t.Fatalf("filter = %+v, want search", res.View.Filter)
	}
}

func TestSetFavoriteRequiresFavoriteField(t *testing.T) {
	f := newFakeJellyfin(t)
	a := f.app()

	raw := []byte(`{"item_id":"` + itemID + `"}`)
	if _, err := describeAction("jellyfin_set_favorite", raw); err == nil {
		t.Fatal("describeAction accepted jellyfin_set_favorite without required favorite field")
	}
	if _, err := a.doAction(t.Context(), "missing-fav", "jellyfin_set_favorite", raw); err == nil {
		t.Fatal("doAction accepted jellyfin_set_favorite without required favorite field")
	}
	if len(f.favorites) != 0 {
		t.Fatalf("unexpected favorite API calls: %d", len(f.favorites))
	}
}

func TestListDeduplicatesAcrossPagesAndRejectsInvalidView(t *testing.T) {
	id1 := "11111111-1111-1111-1111-111111111111"
	id2 := "22222222-2222-2222-2222-222222222222"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Users/Me":
			_ = json.NewEncoder(w).Encode(user{ID: userID, Name: "Tester"})
		case "/UserItems/Resume":
			if r.URL.Query().Get("startIndex") == "0" {
				_ = json.NewEncoder(w).Encode(itemPage{
					Items:            []mediaItem{{ID: id1, Name: "Film 1", Type: "Movie"}},
					TotalRecordCount: 2,
					StartIndex:       0,
				})
				return
			}
			// Page 2 repeats id1 (shifted live resume queue) plus id2.
			_ = json.NewEncoder(w).Encode(itemPage{
				Items:            []mediaItem{{ID: id1, Name: "Film 1 dup", Type: "Movie"}, {ID: id2, Name: "Film 2", Type: "Movie"}},
				TotalRecordCount: 2,
				StartIndex:       1,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{View: "resume", StartIndex: 1}),
	}})
	if err != nil {
		t.Fatalf("render paginated list: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView(paginated list): %v", err)
	}

	if _, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{View: "bogus"}),
	}}); err == nil {
		t.Fatal("render accepted invalid view state")
	}
}

func TestDetailViewClipsConfirmAndHandlesEmptyName(t *testing.T) {
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	v := detailView(req, pageState{Mode: "detail", ItemID: itemID, DraftID: "d1"}, mediaItem{
		ID:   itemID,
		Name: strings.Repeat("M", 2500),
		Type: "Movie",
	})
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView(long name): %v", err)
	}
	for _, item := range v.Sections[0].Items {
		if item.Action != nil && len([]rune(item.Action.Confirm)) > 2000 {
			t.Fatalf("confirm length = %d runes, want <= 2000", len([]rune(item.Action.Confirm)))
		}
	}

	v2 := detailView(req, pageState{Mode: "detail", ItemID: itemID, DraftID: "d2"}, mediaItem{
		ID:   itemID,
		Name: "",
		Type: "Movie",
	})
	if err := wire.CheckView(v2); err != nil {
		t.Fatalf("CheckView(empty name): %v", err)
	}
	if strings.TrimSpace(v2.Title) == "" {
		t.Fatal("empty item Name left View.Title empty")
	}
}
