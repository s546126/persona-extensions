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

func TestCreateViewBackLongDescReviewValidationAndConfirmClip(t *testing.T) {
	a := newFakeKimai(t).app()

	t.Run("Back from review-create with >2000-char description passes CheckView", func(t *testing.T) {
		review, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "review-create",
			State: ext.EncodeState(pageState{Mode: "create", ProjectID: 7, ActivityID: 3, DraftID: "draft-long"}),
			Values: map[string]string{
				"begin":       "2026-11-10T10:00:00Z",
				"end":         "2026-11-10T11:00:00Z",
				"description": strings.Repeat("d", 2500),
			},
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

	t.Run("review-create rejects invalid empty draft", func(t *testing.T) {
		if _, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{
			Mode: "review-create", DraftID: "d1",
		}); err == nil {
			t.Fatal("review-create accepted invalid empty draft")
		}
	})

	t.Run("activeView clips oversized Begin confirm text and deduplicates IDs", func(t *testing.T) {
		it := sampleTimesheet()
		it.Begin = strings.Repeat("2", 2100)
		v := activeView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "active"}, []timesheet{it, it})
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("activeView with oversized Begin failed CheckView: %v", err)
		}
		stopButtons := 0
		for _, comp := range v.Sections[0].Items {
			if comp.Action != nil && comp.Action.ID == "stop" {
				stopButtons++
			}
		}
		if stopButtons != 1 {
			t.Fatalf("stopButtons=%d, want 1 deduped active timer", stopButtons)
		}
	})
}

func TestProjectsActivitiesCapsDedupAndTimesheetDedup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/projects", "/api/activities":
			out := make([]namedResource, 0, 315)
			out = append(out,
				namedResource{ID: 0, Name: "Zero ID", Visible: true},
				namedResource{ID: 1, Name: "", Visible: true},
				namedResource{ID: 2, Name: "Valid", Visible: true},
				namedResource{ID: 2, Name: "Valid dup", Visible: true},
			)
			for i := 3; i <= 312; i++ {
				out = append(out, namedResource{ID: int64(i), Name: fmt.Sprintf("Item %d", i), Visible: true})
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/api/timesheets":
			out := make([]timesheet, 0, 30)
			if r.URL.Query().Get("page") == "1" {
				for i := 1; i <= 30; i++ {
					it := sampleTimesheet()
					it.ID = int64(i)
					out = append(out, it)
				}
			} else {
				// Page 2 overlaps ID 30 and adds ID 31.
				it30 := sampleTimesheet()
				it30.ID = 30
				it31 := sampleTimesheet()
				it31.ID = 31
				out = append(out, it30, it31)
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	for _, st := range []pageState{{Mode: "projects"}, {Mode: "activities", ProjectID: 2}} {
		v, err := a.page(t.Context(), r, st)
		if err != nil {
			t.Fatalf("mode %q failed: %v", st.Mode, err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("mode %q with >300/blank/dup items failed CheckView: %v", st.Mode, err)
		}
	}

	tsV, err := a.page(t.Context(), r, pageState{View: "timesheets", Page: 2})
	if err != nil {
		t.Fatalf("timesheets page 2 failed: %v", err)
	}
	if err := wire.CheckView(tsV); err != nil {
		t.Fatalf("timesheets CheckView failed: %v", err)
	}
	// 31 unique entries × 2 text components each = 62 components.
	if got := len(tsV.Sections[0].Items); got != 62 {
		t.Fatalf("timesheets components=%d, want 62 (31 deduped entries)", got)
	}
}

func TestCanonicalActionScopedReceiptKey(t *testing.T) {
	f := newFakeKimai(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	if _, err := a.doAction(t.Context(), "canon-1", "kimai_create_timesheet", []byte(`{"project_id":7,"activity_id":3,"begin":"2026-11-10T10:00:00Z"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "canon-1", "kimai_create_timesheet", []byte(`{ "begin" : "2026-11-10T10:00:00Z" , "activity_id" : 3 , "project_id" : 7 }`)); err != nil {
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
	if !strings.HasPrefix(saved["canon-1"].Key, "kimai_create_timesheet:") {
		t.Fatalf("receipt key %q is not action-scoped", saved["canon-1"].Key)
	}
}
