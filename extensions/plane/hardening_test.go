package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestStatesRejectsMultiStepCursorCycle(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n > 8 {
			t.Errorf("client.states looped %d times without detecting cursor cycle", n)
			_ = json.NewEncoder(w).Encode(page[workflowState]{})
			return
		}
		cursor := r.URL.Query().Get("cursor")
		next := "100:1:0"
		if cursor == "100:1:0" {
			next = "100:2:0"
		} else if cursor == "100:2:0" {
			next = "100:1:0"
		}
		_ = json.NewEncoder(w).Encode(page[workflowState]{
			Results:         []workflowState{{ID: fmt.Sprintf("s-%d", n), Name: "State"}},
			NextPageResults: true,
			NextCursor:      next,
		})
	}))
	defer srv.Close()
	c := newClient(srv.URL, "tok")
	if _, err := c.states(t.Context(), "acme", "proj-1"); err == nil {
		t.Fatal("client.states succeeded on 2-step cursor cycle instead of returning an error")
	}
}

func TestCreateViewBackLongDescriptionStateMostAndDraftID(t *testing.T) {
	f := newFakePlane(t)
	a := f.app()

	rev, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", ProjectID: "project-1", ProjectName: "Reading", Identifier: "READ", DraftID: "d1"}),
		Values: map[string]string{
			"name":        "Long item",
			"description": strings.Repeat("d", 2500),
			"priority":    "high",
			"state_id":    "state-backlog",
		},
	}})
	if err != nil || rev.View == nil {
		t.Fatalf("review-create 2500-char description: %v", err)
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

	// 6000 Han runes (18,000 UTF-8 bytes) exceed wire.StateMost and must return a clean error.
	if huge, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "zh-Hans",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", ProjectID: "project-1", ProjectName: "Reading", Identifier: "READ", DraftID: "d2"}),
		Values: map[string]string{
			"name":        "Huge item",
			"description": strings.Repeat("字", 6000),
			"priority":    "low",
		},
	}}); err == nil && (huge == nil || huge.View == nil || wire.CheckView(huge.View) != nil) {
		t.Fatal("review-create with >16 KiB CJK description returned invalid view instead of error")
	}

	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "confirm-create",
		State:     ext.EncodeState(pageState{Mode: "review-create", ProjectID: "project-1", Name: "N"}),
	}}); err == nil {
		t.Fatal("confirm-create accepted empty DraftID")
	}
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "confirm-edit",
		State:     ext.EncodeState(pageState{Mode: "review-edit", ProjectID: "project-1", WorkItemID: "item-1", Priority: "low"}),
	}}); err == nil {
		t.Fatal("confirm-edit accepted empty DraftID")
	}
}

func TestLongProjectIDZeroSequenceDupItemsAndCanonicalReceiptKey(t *testing.T) {
	longProjID := strings.Repeat("p", 128)
	longItemID := strings.Repeat("i", 128)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/workspaces/acme/projects/":
			_ = json.NewEncoder(w).Encode(page[project]{
				Results: []project{
					{ID: longProjID, Name: "Long Project", Identifier: "LONG"},
					{ID: longProjID, Name: "Long Project Duplicate", Identifier: "LONG"},
				},
			})
		case "/workspaces/acme/projects/project-1/work-items/":
			_ = json.NewEncoder(w).Encode(page[workItem]{
				Results: []workItem{
					{ID: longItemID, Name: "First zero seq", SequenceID: 0, Priority: "low"},
					{ID: "item-2", Name: "Second zero seq", SequenceID: 0, Priority: "high"},
					{ID: "item-2", Name: "Second zero seq dup", SequenceID: 0, Priority: "high"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	a := &app{client: newClient(srv.URL, "tok"), workspace: "acme", receipts: newWriteStore("")}

	pv, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
	if err != nil {
		t.Fatalf("render projects: %v", err)
	}
	if err := wire.CheckView(pv); err != nil {
		t.Fatalf("projects with 128-char duplicate ID failed CheckView: %v", err)
	}

	iv, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Mode: "items", ProjectID: "project-1", ProjectName: "Reading", Identifier: "READ"}),
	}})
	if err != nil {
		t.Fatalf("render items: %v", err)
	}
	if err := wire.CheckView(iv); err != nil {
		t.Fatalf("items with zero SequenceID and duplicate ID failed CheckView: %v", err)
	}
	if len(iv.Sections[0].Items) != 2 {
		t.Fatalf("items rendered = %d, want 2 deduplicated rows", len(iv.Sections[0].Items))
	}
	if iv.Sections[0].Title != fmt.Sprintf(textsFor("en").items, 2) {
		t.Fatalf("items section title = %q, want %q", iv.Sections[0].Title, fmt.Sprintf(textsFor("en").items, 2))
	}

	dv := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "detail", ProjectID: "project-1", WorkItemID: "item-1"}, workItem{
		ID:         "item-1",
		Name:       "",
		SequenceID: 7,
	}, "")
	if err := wire.CheckView(dv); err != nil {
		t.Fatalf("detailView empty name CheckView: %v", err)
	}
	if strings.TrimSpace(dv.Title) == "" {
		t.Fatal("detailView left empty title")
	}

	f := newFakePlane(t)
	fa := f.app()
	if _, err := fa.doAction(t.Context(), "canon-1", "plane_create_work_item", []byte(`{"project_id":"project-1","name":"Task"}`)); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := fa.doAction(t.Context(), "canon-1", "plane_create_work_item", []byte(`{ "name" : "Task" , "project_id" : "project-1" }`)); err != nil {
		t.Fatalf("whitespace-equivalent retry failed: %v", err)
	}
	f.mu.Lock()
	creates := f.creates
	f.mu.Unlock()
	if creates != 1 {
		t.Fatalf("creates = %d, want 1", creates)
	}
}
