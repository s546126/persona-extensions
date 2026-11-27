package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestSwitchingToSearchOrSmartViewWithEmptyQueryDoesNotError(t *testing.T) {
	f := newFakeImmich(t)
	a := f.app()

	for _, view := range []string{"search", "smart"} {
		res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale:    "en",
			UIVersion: 6,
			Action:    "view",
			Values:    map[string]string{"view": view},
		}})
		if err != nil {
			t.Fatalf("switching to %q view with empty query failed: %v", view, err)
		}
		if err := wire.CheckView(res.View); err != nil {
			t.Fatalf("CheckView(%q empty query): %v", view, err)
		}
		if res.View.Filter == nil || res.View.Filter.Value != view {
			t.Fatalf("filter = %+v, want %q", res.View.Filter, view)
		}
	}
}

func TestPaginationStateRespectsStateMostDedupAndRepeatedCursor(t *testing.T) {
	longCursorPrefix := strings.Repeat("c", 3500)
	id1 := "11111111-1111-1111-1111-111111111111"
	id2 := "22222222-2222-2222-2222-222222222222"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		cur, _ := in["cursor"].(string)
		var out searchResponse
		switch {
		case cur == "":
			next := longCursorPrefix + "1"
			out.Assets.Items = []asset{{ID: id1, OriginalFileName: "a.jpg", Type: "IMAGE"}}
			out.Assets.NextCursor = &next
		case strings.HasSuffix(cur, "1"):
			next := longCursorPrefix + "2"
			// Repeat id1 across pages to test dedup.
			out.Assets.Items = []asset{{ID: id1, OriginalFileName: "a.jpg", Type: "IMAGE"}, {ID: id2, OriginalFileName: "b.jpg", Type: "IMAGE"}}
			out.Assets.NextCursor = &next
		default:
			idx, _ := strconv.Atoi(cur[len(cur)-1:])
			next := longCursorPrefix + strconv.Itoa(idx+1)
			out.Assets.Items = []asset{{ID: id2, OriginalFileName: "b.jpg", Type: "IMAGE"}}
			out.Assets.NextCursor = &next
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	v, err := a.render(t.Context(), req)
	if err != nil {
		t.Fatalf("initial render: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView(page 0): %v", err)
	}
	// Keep clicking Load more as long as More is offered; every View must satisfy wire.CheckView (<= wire.StateMost).
	for step := 1; step <= 6; step++ {
		more := v.Sections[0].More
		if more == nil {
			break
		}
		res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale:    "en",
			UIVersion: 6,
			State:     v.State,
			Action:    "more",
			Values:    more.Action.Values,
		}})
		if err != nil {
			t.Fatalf("more step %d: %v", step, err)
		}
		if err := wire.CheckView(res.View); err != nil {
			t.Fatalf("CheckView(step %d): %v", step, err)
		}
		v = res.View
	}

	// readState must reject oversized cursor inside st.Pages.
	if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{
		State: ext.EncodeState(pageState{Pages: []string{strings.Repeat("x", 4097)}}),
	}}); err == nil {
		t.Fatal("readState accepted cursor > 4096 bytes inside Pages")
	}
}

func TestSetFavoriteRequiresFavoriteField(t *testing.T) {
	f := newFakeImmich(t)
	a := f.app()

	raw := []byte(`{"asset_id":"` + assetID + `"}`)
	if _, err := describeAction("immich_set_favorite", raw); err == nil {
		t.Fatal("describeAction accepted immich_set_favorite without required favorite field")
	}
	if _, err := a.doAction(t.Context(), "missing-fav", "immich_set_favorite", raw); err == nil {
		t.Fatal("doAction accepted immich_set_favorite without required favorite field")
	}
	if len(f.favorites) != 0 {
		t.Fatalf("unexpected favorite API calls: %d", len(f.favorites))
	}
}

func TestDetailLocationFormatConfirmAndAllTrashedSearch(t *testing.T) {
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	var item asset
	item.ID = assetID
	item.OriginalFileName = strings.Repeat("P", 2500)
	item.Type = "IMAGE"
	item.Exif.Country = "Japan"
	v := detailView(req, pageState{Mode: "detail", AssetID: assetID, DraftID: "d1"}, item)
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView(long filename): %v", err)
	}
	for _, c := range v.Sections[0].Items {
		if strings.HasPrefix(c.Text, ", ") {
			t.Fatalf("location text has leading comma: %q", c.Text)
		}
		if c.Action != nil && len([]rune(c.Action.Confirm)) > 2000 {
			t.Fatalf("confirm length = %d runes, want <= 2000", len([]rune(c.Action.Confirm)))
		}
	}

	// Empty filename fallback.
	v2 := detailView(req, pageState{Mode: "detail", AssetID: assetID, DraftID: "d2"}, asset{ID: assetID, Type: "IMAGE"})
	if err := wire.CheckView(v2); err != nil {
		t.Fatalf("CheckView(empty filename): %v", err)
	}
	if strings.TrimSpace(v2.Title) == "" {
		t.Fatal("empty filename left View.Title empty")
	}

	// All trashed assets in immich_search must report "No assets matched." rather than empty string.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var out searchResponse
		out.Assets.Items = []asset{{ID: assetID, OriginalFileName: "trashed.jpg", IsTrashed: true}}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	text, err := a.callTool(t.Context(), "immich_search", json.RawMessage(`{"favorites_only":true}`))
	if err != nil {
		t.Fatalf("immich_search all trashed: %v", err)
	}
	if !strings.Contains(text, "No assets matched") {
		t.Fatalf("all-trashed search text = %q, want No assets matched", text)
	}
}
