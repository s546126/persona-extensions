package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestCreateViewBackLongDescDraftIDAndUnclippedDetailDates(t *testing.T) {
	a := newFakeOpenProject(t).app()

	t.Run("Back from review-create with >2000-char description passes CheckView", func(t *testing.T) {
		review, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "review-create",
			State: ext.EncodeState(pageState{
				Mode: "create", ProjectHref: "/api/v3/projects/7", ProjectName: "Reading",
				TypeHref: "/api/v3/types/1", TypeName: "Task", DraftID: "draft-long",
			}),
			Values: map[string]string{"subject": "Long desc", "description": strings.Repeat("d", 2500)},
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
			Mode: "review-create", ProjectHref: "/api/v3/projects/7", TypeHref: "/api/v3/types/1", Subject: "No draft",
		})); err == nil {
			t.Fatal("confirm-create accepted empty DraftID")
		}
	})

	t.Run("detailView clips oversized dates", func(t *testing.T) {
		it := sampleWorkPackage()
		it.StartDate = strings.Repeat("2", 2100)
		v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "detail", WorkID: 42}, it)
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("detailView with oversized date failed CheckView: %v", err)
		}
	})
}

func TestProjectsDedupInvalidTypeAndPaginatedWorkPackagesDedup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/hal+json")
		switch r.URL.Path {
		case "/api/v3/users/me":
			_ = json.NewEncoder(w).Encode(currentUser{ID: 42, Name: "Reader"})
		case "/api/v3/projects":
			var out collection[project]
			out.Embedded.Elements = []project{
				{ID: 7, Name: "Reading", Active: true, Links: struct {
					Self link `json:"self"`
				}{Self: link{Href: "/api/v3/projects/7"}}},
				{ID: 7, Name: "Reading dup", Active: true, Links: struct {
					Self link `json:"self"`
				}{Self: link{Href: "/api/v3/projects/7"}}},
				{ID: 0, Name: "Zero ID", Active: true, Links: struct {
					Self link `json:"self"`
				}{Self: link{Href: "/api/v3/projects/0"}}},
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/api/v3/types":
			// Only milestones: no valid non-milestone type for creation.
			var out collection[workType]
			out.Embedded.Elements = []workType{
				{ID: 9, Name: "Milestone", IsMilestone: true, Links: struct {
					Self link `json:"self"`
				}{Self: link{Href: "/api/v3/types/9"}}},
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/api/v3/work_packages":
			var out collection[workPackage]
			if r.URL.Query().Get("offset") == "1" {
				out.Embedded.Elements = []workPackage{sampleWorkPackage()}
				out.Links.NextByOffset = link{Href: "/api/v3/work_packages?offset=2"}
			} else {
				wp2 := sampleWorkPackage()
				wp2.ID, wp2.Subject = 43, "Second item"
				out.Embedded.Elements = []workPackage{sampleWorkPackage(), wp2}
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	projV, err := a.page(t.Context(), r, pageState{Mode: "projects"})
	if err != nil {
		t.Fatalf("projects page error: %v", err)
	}
	if err := wire.CheckView(projV); err != nil {
		t.Fatalf("projects view with duplicate/zero IDs failed CheckView: %v", err)
	}
	for _, comp := range projV.Sections[0].Items {
		if comp.Action != nil && comp.Action.ID == "create" && !typeHrefPattern.MatchString(comp.Action.Values["type_href"]) {
			t.Fatalf("projects view emitted unclickable create action with invalid type_href %q", comp.Action.Values["type_href"])
		}
	}

	wpV, err := a.page(t.Context(), r, pageState{Offset: 2})
	if err != nil {
		t.Fatalf("work packages page error: %v", err)
	}
	if err := wire.CheckView(wpV); err != nil {
		t.Fatalf("paginated work packages with overlapping ID failed CheckView: %v", err)
	}
	if len(wpV.Sections[0].Items) != 2 {
		t.Fatalf("got %d rows, want 2 unique rows: %+v", len(wpV.Sections[0].Items), wpV.Sections[0].Items)
	}
	_ = fmt.Sprint(wpV.Title)
}

func TestCanonicalActionScopedReceiptKey(t *testing.T) {
	f := newFakeOpenProject(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	if _, err := a.doAction(t.Context(), "canon-1", "openproject_create_work_package", []byte(`{"project_href":"/api/v3/projects/7","type_href":"/api/v3/types/1","subject":"Keep context"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "canon-1", "openproject_create_work_package", []byte(`{ "subject" : "Keep context" , "type_href" : "/api/v3/types/1" , "project_href" : "/api/v3/projects/7" }`)); err != nil {
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
	if !strings.HasPrefix(saved["canon-1"].Key, "openproject_create_work_package:") {
		t.Fatalf("receipt key %q is not action-scoped", saved["canon-1"].Key)
	}
}
