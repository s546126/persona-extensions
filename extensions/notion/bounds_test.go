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

func TestReviewAppendValid2000RuneNotePassesCheckView(t *testing.T) {
	f := newFakeNotion(t)
	a := f.app()

	// Exact valid max (2000 runes) must render a CheckView-valid review-append view.
	note2000 := strings.Repeat("a", 2000)
	r := &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Mode: "append", PageID: "page-1", DraftID: "d1"}),
		Action:    "review-append",
		Values:    map[string]string{"kind": "paragraph", "text": note2000},
	}}
	res, err := a.action(t.Context(), r)
	if err != nil {
		t.Fatalf("review-append 2000 runes: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("CheckView(review-append 2000 runes): %v", err)
	}

	// Multiline 350 short lines (700 runes, valid) must stay within wire.ItemsMost.
	r.Values["text"] = strings.Repeat("x\n", 350)
	res, err = a.action(t.Context(), r)
	if err != nil {
		t.Fatalf("review-append 350 lines: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("CheckView(review-append 350 lines): %v", err)
	}
}

func TestReviewCreateValidatesContentAndCapsComponents(t *testing.T) {
	f := newFakeNotion(t)
	a := f.app()

	// Control character or >2000 runes in content must be rejected at review-create.
	for _, bad := range []string{"bad\x01ctrl", strings.Repeat("z", 2001)} {
		r := &ext.UIRequest{UIParams: wire.UIParams{
			Locale:    "en",
			UIVersion: 6,
			State:     ext.EncodeState(pageState{Mode: "create", DraftID: "d1"}),
			Action:    "review-create",
			Values:    map[string]string{"parent_id": "page-1", "title": "Note", "content": bad},
		}}
		if _, err := a.action(t.Context(), r); err == nil {
			t.Fatalf("review-create accepted invalid content (len=%d)", len(bad))
		}
	}

	// 350 short lines (< 2000 runes) must be capped within wire.ItemsMost.
	r := &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Mode: "create", DraftID: "d2"}),
		Action:    "review-create",
		Values:    map[string]string{"parent_id": "page-1", "title": "Note", "content": strings.Repeat("x\n", 350)},
	}}
	res, err := a.action(t.Context(), r)
	if err != nil {
		t.Fatalf("review-create 350 lines: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("CheckView(review-create 350 lines): %v", err)
	}
}

func TestDetailViewCapsBlocksAndGuardsURL(t *testing.T) {
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	lines := make([]string, 400)
	for i := range lines {
		lines[i] = "block line"
	}
	page := notionPage{
		ID:         "page-1",
		URL:        "http://notion.so/not-https",
		Properties: map[string]json.RawMessage{"title": json.RawMessage(`{"title":[{"plain_text":"` + strings.Repeat("T", 2500) + `"}]}`)},
	}
	v := detailView(req, pageState{PageID: "page-1", Mode: "detail"}, page, lines)
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView(detailView 400 blocks & bad URL): %v", err)
	}
}

func TestPageTitleFallbackAndListLongDuplicateKeys(t *testing.T) {
	// Custom title property with empty plain_text must fall back to "Untitled", not "".
	emptyTitlePage := notionPage{
		ID:         "p-empty",
		Properties: map[string]json.RawMessage{"CustomTitle": json.RawMessage(`{"title":[{"plain_text":""}]}`)},
	}
	if got := pageTitle(emptyTitlePage); got != "Untitled" {
		t.Fatalf("pageTitle = %q, want Untitled", got)
	}

	longID := strings.Repeat("p", 126) // "page-" + 126 = 131 > 128 wire key limit
	longTitle := strings.Repeat("N", 2500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"object": "page", "id": longID, "url": "https://www.notion.so/a", "last_edited_time": "2026-11-09T10:00:00Z", "properties": map[string]any{"title": map[string]any{"title": []map[string]string{{"plain_text": longTitle}}}}},
				{"object": "page", "id": longID, "url": "https://www.notion.so/a", "last_edited_time": "2026-11-09T10:00:00Z", "properties": map[string]any{"CustomTitle": map[string]any{"title": []map[string]string{{"plain_text": ""}}}}},
			},
			"has_more": false,
		})
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL, testSecret), receipts: newWriteStore("")}
	v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
	if err != nil {
		t.Fatalf("render list: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView(list): %v", err)
	}

	// readState rune limits.
	cjk := strings.Repeat("笔", 120)
	if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(pageState{Query: cjk})}}); err != nil {
		t.Fatalf("readState rejected 120-rune CJK query: %v", err)
	}
	if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(pageState{Query: strings.Repeat("a", 201)})}}); err == nil {
		t.Fatal("readState accepted >200-rune query")
	}
}
