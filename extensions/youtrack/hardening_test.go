package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestReviewCreateStateMostCreateViewBackLongDescAndDraftID(t *testing.T) {
	f := newFakeYouTrack(t)
	a := f.app()

	t.Run("review-create rejects description exceeding wire.StateMost", func(t *testing.T) {
		res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "zh-Hans", UIVersion: 6, Action: "review-create",
			State:  ext.EncodeState(pageState{Mode: "create", ProjectID: "0-7", ProjectName: "Reading", DraftID: "draft-big"}),
			Values: map[string]string{"project_id": "0-7", "summary": "Big draft", "description": strings.Repeat("文", 6000)},
		}})
		if err == nil {
			if res == nil || res.View == nil || wire.CheckView(res.View) != nil {
				t.Fatalf("review-create returned an oversized View without error: %v", wire.CheckView(res.View))
			}
			t.Fatal("expected error for >16KB review state")
		}
	})

	t.Run("Back from review-create with >2000-char description passes CheckView", func(t *testing.T) {
		review, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "review-create",
			State:  ext.EncodeState(pageState{Mode: "create", ProjectID: "0-7", ProjectName: "Reading", DraftID: "draft-long"}),
			Values: map[string]string{"project_id": "0-7", "summary": "Long desc", "description": strings.Repeat("d", 2500)},
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
			t.Fatalf("createView after Back with 2500-char description failed CheckView: %v", err)
		}
	})

	t.Run("confirm-create rejects empty DraftID", func(t *testing.T) {
		if _, err := a.action(t.Context(), withAction(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, "confirm-create", pageState{
			Mode: "review-create", ProjectID: "0-7", Summary: "No draft",
		})); err == nil {
			t.Fatal("confirm-create accepted empty DraftID")
		}
	})
}

func TestProjectsBoundsDedupLongFieldsAndBoundedKey(t *testing.T) {
	longProject := strings.Repeat("P", 2100)
	longShort := strings.Repeat("S", 2100)
	longID := "proj-" + strings.Repeat("x", 150)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/admin/projects" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		projects := make([]project, 0, 315)
		projects = append(projects,
			project{ID: "0-7", Name: longProject, ShortName: longShort},
			project{ID: "0-7", Name: longProject, ShortName: longShort},
			project{ID: longID, Name: "Long ID project", ShortName: "LONG"},
		)
		for i := 10; i < 320; i++ {
			projects = append(projects, project{ID: fmt.Sprintf("0-%d", i), Name: fmt.Sprintf("Project %d", i), ShortName: fmt.Sprintf("P%d", i)})
		}
		_ = json.NewEncoder(w).Encode(projects)
	}))
	t.Cleanup(srv.Close)

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "projects"})
	if err != nil {
		t.Fatalf("projects page error: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("projects view with 315 projects, duplicates, long fields, and long ID failed CheckView: %v", err)
	}
	if len(v.Sections[0].Items) > maxViewRecords {
		t.Fatalf("projects count=%d exceeds %d", len(v.Sections[0].Items), maxViewRecords)
	}
	pushState := v.Sections[0].Items[0].Action.Push.State
	createV, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6, State: pushState}})
	if err != nil {
		t.Fatalf("rendering createView from long-named project Push.State failed: %v", err)
	}
	if err := wire.CheckView(createV); err != nil {
		t.Fatalf("createView for long-named project failed CheckView: %v", err)
	}
}

func TestPaginatedIssuesDedupLongIssueKeyAndCanonicalReceiptKey(t *testing.T) {
	t.Run("paginated issues deduplicate and bound long issue ID keys", func(t *testing.T) {
		longIssueID := "issue-" + strings.Repeat("y", 150)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			skip, _ := strconv.Atoi(r.URL.Query().Get("$skip"))
			var items []issue
			if skip == 0 {
				for i := 1; i <= 50; i++ {
					items = append(items, issue{ID: fmt.Sprintf("2-%d", i), IDReadable: fmt.Sprintf("READ-%d", i), Summary: fmt.Sprintf("Issue %d", i), Project: project{ID: "0-7", Name: "Reading"}})
				}
			} else {
				items = []issue{
					{ID: "2-50", IDReadable: "READ-50", Summary: "Issue 50 overlap", Project: project{ID: "0-7", Name: "Reading"}},
					{ID: longIssueID, IDReadable: "READ-LONG", Summary: "Long ID issue", Project: project{ID: "0-7", Name: "Reading"}},
				}
			}
			_ = json.NewEncoder(w).Encode(items)
		}))
		t.Cleanup(srv.Close)

		a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
		v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Skip: 50})
		if err != nil {
			t.Fatalf("issues page error: %v", err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("paginated issues with overlapping ID and long ID failed CheckView: %v", err)
		}
		if len(v.Sections[0].Items) != 51 {
			t.Fatalf("got %d rows, want 51 unique rows", len(v.Sections[0].Items))
		}
	})

	t.Run("canonical action-scoped receipt key", func(t *testing.T) {
		f := newFakeYouTrack(t)
		a := f.app()
		store, err := openWriteStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		a.receipts = store
		if _, err := a.doAction(t.Context(), "canon-1", "youtrack_create_issue", []byte(`{"project_id":"0-7","summary":"Keep context","description":"Desc"}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.doAction(t.Context(), "canon-1", "youtrack_create_issue", []byte(`{ "description" : "Desc" , "summary" : "Keep context" , "project_id" : "0-7" }`)); err != nil {
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
		if !strings.HasPrefix(saved["canon-1"].Key, "youtrack_create_issue:") {
			t.Fatalf("receipt key %q is not action-scoped", saved["canon-1"].Key)
		}
	})
}
