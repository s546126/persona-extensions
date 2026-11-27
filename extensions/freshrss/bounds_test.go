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

func TestCheckViewBoundsForDetailConfirmSubscriptionsAndDuplicateArticles(t *testing.T) {
	// 1. detailView with > 2000-rune title must clip Action.Confirm to pass CheckView.
	longTitle := strings.Repeat("Title ", 400)
	dv := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}},
		pageState{Mode: "detail", ArticleID: "tag:google.com,2005:reader/item/0001", DraftID: "d1"},
		article{ID: "tag:google.com,2005:reader/item/0001", Title: longTitle, Author: "Author"},
	)
	if err := wire.CheckView(dv); err != nil {
		t.Fatalf("detailView with long title failed CheckView: %v", err)
	}

	// 2. Subscriptions view with 320 feeds and article list with duplicate IDs across pages must pass CheckView.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/accounts/ClientLogin":
			fmt.Fprint(w, "Auth=reader/0123456789abcdef0123456789abcdef\n")
		case "/reader/api/0/subscription/list":
			w.Header().Set("Content-Type", "application/json")
			var b strings.Builder
			b.WriteString(`{"subscriptions":[`)
			for i := range 320 {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, `{"id":"feed/%d","title":"Feed %d"}`, i+1, i+1)
			}
			b.WriteString(`]}`)
			fmt.Fprint(w, b.String())
		case "/reader/api/0/stream/contents/reading-list":
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("c") == "1002" {
				fmt.Fprint(w, `{"items":[{"id":"tag:google.com,2005:reader/item/0001","title":"Duplicate across pages"},{"id":"tag:google.com,2005:reader/item/0002","title":"Second article"}]}`)
				return
			}
			fmt.Fprint(w, `{"continuation":"1002","items":[{"id":"tag:google.com,2005:reader/item/0001","title":"Duplicate across pages"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	a := &app{client: newClient(srv.URL, "reader", "pass"), receipts: newWriteStore("")}

	subsView, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, State: ext.EncodeState(pageState{Mode: "subscriptions"}),
	}})
	if err != nil {
		t.Fatalf("subscriptions render: %v", err)
	}
	if err := wire.CheckView(subsView); err != nil {
		t.Fatalf("subscriptions view with 320 feeds failed CheckView: %v", err)
	}

	listView, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, State: ext.EncodeState(pageState{View: "unread", Pages: []string{"1002"}}),
	}})
	if err != nil {
		t.Fatalf("list render: %v", err)
	}
	if err := wire.CheckView(listView); err != nil {
		t.Fatalf("article list with duplicate ID across pages failed CheckView: %v", err)
	}
}
