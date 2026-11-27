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

func TestUndatedNoIdentifierTaskDetailAndDueSubtitle(t *testing.T) {
	undated := task{ID: 55, Identifier: "", Title: "Plain task", DueDate: "0001-01-01T00:00:00Z"}
	v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "detail", TaskID: 55, DraftID: "d1"}, undated)
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("detailView with empty identifier and zero due date failed CheckView: %v", err)
	}

	dated := task{ID: 56, Identifier: "", Title: "Dated task", DueDate: "2026-12-01T12:00:00Z"}
	if got := dated.subtitle(); strings.HasPrefix(strings.TrimSpace(got), "·") || got != "2026-12-01T12:00:00Z" {
		t.Fatalf("subtitle with empty identifier = %q, want %q", got, "2026-12-01T12:00:00Z")
	}
}

func TestCreateViewBackLongDescStateMostAndSearchMode(t *testing.T) {
	f := newFakeVikunja(t)
	a := f.app()

	rev, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", ProjectID: 7, DraftID: "d1"}),
		Values: map[string]string{
			"title":       "Long description task",
			"description": strings.Repeat("d", 3000),
		},
	}})
	if err != nil || rev.View == nil {
		t.Fatalf("review-create 3000 chars: %v", err)
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
		t.Fatalf("createView after back CheckView: %v", err)
	}

	if v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "review-create", ProjectID: 7, DraftID: "d2"}); err == nil && wire.CheckView(v) != nil {
		t.Fatal("review-create with empty title returned invalid view instead of error")
	}

	// Triggering search from detail mode must return to the task list view.
	res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "search",
		State:     ext.EncodeState(pageState{Mode: "detail", TaskID: 42}),
		Values:    map[string]string{"q": "reading"},
	}})
	if err != nil || res.View == nil || res.View.Search == nil {
		t.Fatalf("search from detail mode did not return list view: err=%v view=%+v", err, res)
	}
}

func TestListDedupLongProjectSubtitleAndCanonicalReceiptKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/user":
			_ = json.NewEncoder(w).Encode(user{ID: 1, Username: "reader"})
		case "/api/v1/tasks":
			w.Header().Set("X-Pagination-Total", "2")
			w.Header().Set("X-Pagination-Total-Pages", "2")
			_ = json.NewEncoder(w).Encode([]task{
				{ID: 42, Identifier: "READ-42", Title: "Overlap task"},
			})
		case "/api/v1/projects":
			_ = json.NewEncoder(w).Encode([]project{
				{ID: 7, Title: "Reading", Identifier: strings.Repeat("I", 2200)},
				{ID: 7, Title: "Reading duplicate", Identifier: "DUP"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}

	tv, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Page: 2})
	if err != nil {
		t.Fatalf("tasks page 2: %v", err)
	}
	if err := wire.CheckView(tv); err != nil {
		t.Fatalf("paginated duplicate tasks CheckView: %v", err)
	}

	pv, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "projects"})
	if err != nil {
		t.Fatalf("projects page: %v", err)
	}
	if err := wire.CheckView(pv); err != nil {
		t.Fatalf("projects long identifier + duplicate CheckView: %v", err)
	}

	f := newFakeVikunja(t)
	fa := f.app()
	first := []byte(`{"project_id":7,"title":"Canon","description":"Keep"}`)
	reordered := []byte(`{ "description": "Keep", "title": "Canon", "project_id": 7 }`)
	if _, err := fa.doAction(t.Context(), "canon-1", "vikunja_create_task", first); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := fa.doAction(t.Context(), "canon-1", "vikunja_create_task", reordered); err != nil {
		t.Fatalf("reordered retry failed: %v", err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("reordered retry created twice: %d", len(f.creates))
	}
}
