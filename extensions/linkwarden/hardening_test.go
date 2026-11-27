package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestZeroCollectionsListViewPassesCheckView(t *testing.T) {
	a := serveCollections(t, 0)
	v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "list"})
	if err != nil {
		t.Fatalf("page with zero collections: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("list view with zero collections failed CheckView: %v", err)
	}
	if v.Filter != nil {
		t.Fatalf("expected nil filter when fewer than 2 choices, got %+v", v.Filter)
	}
}

func TestLongSearchQueryDuplicateLinksAndPrettyJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search":
			// Pretty-printed JSON with leading whitespace before '{' and duplicate link ID 101.
			fmt.Fprint(w, "{\n  \"data\": \n    {\n      \"links\": [\n        {\"id\": 101, \"name\": \"First\", \"url\": \"https://example.com/1\"},\n        {\"id\": 101, \"name\": \"First duplicate\", \"url\": \"https://example.com/1\"},\n        {\"id\": 102, \"name\": \"Second\", \"url\": \"https://example.com/2\"}\n      ]\n    },\n  \"success\": true\n}")
		case "/collections":
			fmt.Fprint(w, `{"response":[{"id":5,"name":"Research"},{"id":5,"name":"Research duplicate"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	a := &app{client: newClient(srv.URL, testKey), receipts: newWriteStore("")}

	items, err := a.client.search(t.Context(), "context", 0)
	if err != nil {
		t.Fatalf("client.search pretty JSON: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("client.search returned %d links on pretty-printed JSON, want 3", len(items))
	}

	res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "search",
		Values:    map[string]string{"q": strings.Repeat("q", 2500)},
	}})
	if err != nil || res.View == nil {
		t.Fatalf("search action: %v %+v", err, res)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("search with long query and duplicate links failed CheckView: %v", err)
	}
	// Save button + 2 deduplicated links = 3 items.
	if len(res.View.Sections[0].Items) != 3 {
		t.Fatalf("section items = %d, want 3 (save button + 2 deduplicated links)", len(res.View.Sections[0].Items))
	}

	if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{
		State: ext.EncodeState(pageState{Query: strings.Repeat("q", 250)}),
	}}); err == nil {
		t.Fatal("readState accepted Query > 200 runes")
	}
}

func TestDetailViewLongCollectionUnnamedConfirmAndGuards(t *testing.T) {
	f := newFakeLinkwarden(t)
	a := f.app()

	dv := detailView(
		&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}},
		pageState{Mode: "detail", LinkID: 101, DraftID: "d1"},
		link{
			ID:         101,
			Name:       "",
			URL:        "https://example.com/untitled",
			Collection: &collection{ID: 5, Name: strings.Repeat("c", 2500)},
		},
		nil,
	)
	if err := wire.CheckView(dv); err != nil {
		t.Fatalf("detailView long collection name CheckView: %v", err)
	}
	var confirm string
	for _, it := range dv.Sections[0].Items {
		if it.Action != nil && it.Action.ID == "archive" {
			confirm = it.Action.Confirm
		}
	}
	if !strings.Contains(confirm, "https://example.com/untitled") {
		t.Fatalf("archive confirm missing fallback linkTitle: %q", confirm)
	}

	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "archive",
		State:     ext.EncodeState(pageState{Mode: "detail", LinkID: 101}),
	}}); err == nil {
		t.Fatal("archive action accepted empty DraftID")
	}

	if _, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Mode: "detail", LinkID: 0}),
	}}); err == nil {
		t.Fatal("detail render accepted non-positive LinkID")
	}
}
