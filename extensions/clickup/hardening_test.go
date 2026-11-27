package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestEditorViewBackLongDescEmptyReviewAndDraftID(t *testing.T) {
	a := newFakeClickUp(t).app()

	t.Run("Back from review-create with >2000-char description passes CheckView", func(t *testing.T) {
		review, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "review-create",
			State:  ext.EncodeState(pageState{Mode: "create", TeamID: "team-1", ListID: "list-1", DraftID: "draft-long"}),
			Values: map[string]string{"name": "Long desc", "description": strings.Repeat("d", 2500)},
		}})
		if err != nil || review.View == nil || wire.CheckView(review.View) != nil {
			t.Fatalf("review-create failed: %v", err)
		}
		back, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "create", State: review.View.State,
		}})
		if err != nil || back.View == nil {
			t.Fatalf("Back from review-create failed: %v", err)
		}
		if err := wire.CheckView(back.View); err != nil {
			t.Fatalf("editorView after Back with 2500-char description failed CheckView: %v", err)
		}
	})

	t.Run("empty review draft and empty DraftID rejected", func(t *testing.T) {
		if v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{
			Mode: "review-create", ListID: "list-1", DraftID: "d1",
		}); err == nil {
			if cerr := wire.CheckView(v); cerr != nil {
				t.Fatalf("review-create with empty Name returned invalid View: %v", cerr)
			}
			t.Fatal("expected error for empty review-create draft")
		}

		if _, err := a.action(t.Context(), withAction(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, "confirm-create", pageState{
			Mode: "review-create", ListID: "list-1", Name: "No draft",
		})); err == nil {
			t.Fatal("confirm-create accepted empty DraftID")
		}
		if _, err := a.action(t.Context(), withAction(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, "confirm-edit", pageState{
			Mode: "review-edit", TaskID: "task-1", Status: "done",
		})); err == nil {
			t.Fatal("confirm-edit accepted empty DraftID")
		}
	})

	t.Run("detailView clips oversized task status in edit push state", func(t *testing.T) {
		it := sampleTask()
		it.Status.Status = strings.Repeat("s", 150)
		v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{
			Mode: "detail", TeamID: "team-1", TaskID: "task-1",
		}, it)
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("detailView CheckView failed: %v", err)
		}
		var pushState string
		for _, comp := range v.Sections[0].Items {
			if comp.Action != nil && comp.Action.ID == "edit" && comp.Action.Push != nil {
				pushState = comp.Action.Push.State
				break
			}
		}
		editV, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6, State: pushState}})
		if err != nil {
			t.Fatalf("render edit push state with 150-char status failed: %v", err)
		}
		if err := wire.CheckView(editV); err != nil {
			t.Fatalf("edit view CheckView failed: %v", err)
		}
	})
}

func TestListDedupBoundedKeysAndLongIDSubtitle(t *testing.T) {
	longID := "id-" + strings.Repeat("z", 150)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"user": user{ID: 42, Username: "reader"}})
		case "/team":
			_ = json.NewEncoder(w).Encode(teamPage{Teams: []team{
				{ID: "team-1", Name: "Personal"},
				{ID: "team-1", Name: "Personal dup"},
				{ID: longID, Name: "Long team"},
			}})
		case "/team/team-1/space":
			_ = json.NewEncoder(w).Encode(map[string]any{"spaces": []map[string]string{
				{"id": "space-1", "name": "Knowledge"},
				{"id": "space-1", "name": "Knowledge dup"},
				{"id": longID, "name": "Long space"},
			}})
		case "/space/space-1/list":
			_ = json.NewEncoder(w).Encode(listPage{Lists: []taskList{
				{ID: "list-1", Name: "Reading"},
				{ID: "list-1", Name: "Reading dup"},
				{ID: longID, Name: "Long list"},
			}})
		case "/team/team-1/task":
			t1 := sampleTask()
			tLong := sampleTask()
			tLong.ID, tLong.Name = longID, "Long task"
			_ = json.NewEncoder(w).Encode(taskPage{Tasks: []task{t1, t1, tLong}, Last: true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	for _, st := range []pageState{
		{},
		{Mode: "spaces", TeamID: "team-1", TeamName: "Personal"},
		{Mode: "lists", TeamID: "team-1", SpaceID: "space-1", SpaceName: "Knowledge"},
		{Mode: "tasks", TeamID: "team-1", TeamName: "Personal"},
	} {
		v, err := a.page(t.Context(), r, st)
		if err != nil {
			t.Fatalf("mode %q error: %v", st.Mode, err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("mode %q failed CheckView: %v", st.Mode, err)
		}
		if len(v.Sections[0].Items) != 2 {
			t.Fatalf("mode %q items=%d, want 2 deduped items", st.Mode, len(v.Sections[0].Items))
		}
	}
}

func TestCanonicalActionScopedReceiptKey(t *testing.T) {
	f := newFakeClickUp(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	if _, err := a.doAction(t.Context(), "canon-1", "clickup_create_task", []byte(`{"list_id":"list-1","name":"Keep context","description":"Desc"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "canon-1", "clickup_create_task", []byte(`{ "description" : "Desc" , "name" : "Keep context" , "list_id" : "list-1" }`)); err != nil {
		t.Fatalf("replayed canonical JSON rejected: %v", err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("creates=%d, want 1", len(f.creates))
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]writeReceipt
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(saved["canon-1"].Key, "clickup_create_task:") {
		t.Fatalf("receipt key %q is not action-scoped", saved["canon-1"].Key)
	}
}
