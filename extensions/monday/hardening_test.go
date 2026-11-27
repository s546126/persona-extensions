package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestBoardAPIErrorsAndBoardsPaginationLoop(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr error
		wantSub string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantErr: errToken},
		{name: "rate_limited", status: http.StatusTooManyRequests, wantErr: errRateLimited},
		{name: "graphql_error", status: http.StatusOK, body: `{"data":{},"errors":[{"message":"synthetic application error"}]}`, wantSub: "synthetic application error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
				}
			}))
			t.Cleanup(srv.Close)
			c := newClient(srv.URL, testToken)
			_, err := c.board(t.Context(), 7)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("board() error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("board() error = %v, want substring %q", err, tc.wantSub)
			}
		})
	}

	t.Run("boards breaks repeated page loop", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) > 10 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			boards := make([]map[string]string, 50)
			for i := range boards {
				boards[i] = map[string]string{
					"id":   fmt.Sprint(i + 1),
					"name": fmt.Sprintf("Board %d", i+1),
					"url":  fmt.Sprintf("https://example.monday.com/boards/%d", i+1),
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"boards": boards}})
		}))
		t.Cleanup(srv.Close)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		boards, err := newClient(srv.URL, testToken).boards(ctx)
		if err != nil {
			t.Fatalf("boards() error on repeated page: %v (calls=%d)", err, calls.Load())
		}
		if len(boards) != 50 || calls.Load() > 3 {
			t.Fatalf("boards() len=%d calls=%d, want 50 boards and <=3 calls", len(boards), calls.Load())
		}
	})
}

func TestDetailEmptyUpdatedAtStatusChoicesAndReviewGuards(t *testing.T) {
	t.Run("detailView omits empty UpdatedAt", func(t *testing.T) {
		it := sampleItem()
		it.UpdatedAt = ""
		v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "detail", BoardID: 7, ItemID: 42}, sampleBoard(), it)
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("detailView with empty UpdatedAt failed CheckView: %v", err)
		}
	})

	t.Run("statusChoices deterministic deduped and edit preserves st.Label", func(t *testing.T) {
		b := sampleBoard()
		b.Columns[0].SettingsStr = `{"labels":{"0":"To Do","1":"In Progress","2":"Done","3":"Done","10":"Blocked"}}`
		first := statusChoices(b, "status")
		if len(first) != 4 {
			t.Fatalf("statusChoices len=%d, want 4 deduped choices: %+v", len(first), first)
		}
		for range 20 {
			got := statusChoices(b, "status")
			if !slices.Equal(got, first) {
				t.Fatalf("non-deterministic statusChoices: %+v vs %+v", got, first)
			}
		}

		f := newFakeMonday(t)
		a := f.app()
		v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{
			Mode:     "edit",
			BoardID:  7,
			ItemID:   42,
			ColumnID: "status",
			Label:    "Done",
			DraftID:  "draft-1",
		})
		if err != nil || wire.CheckView(v) != nil {
			t.Fatalf("edit view failed: %v", err)
		}
		if v.Sections[0].Items[0].Value != "Done" {
			t.Fatalf("edit choice Value = %q, want %q", v.Sections[0].Items[0].Value, "Done")
		}
	})

	t.Run("review empty summary, missing DraftID, and repeated more cursor rejected", func(t *testing.T) {
		f := newFakeMonday(t)
		a := f.app()
		r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

		if v, err := a.page(t.Context(), r, pageState{Mode: "review-create", BoardID: 7, DraftID: "draft-1"}); err == nil {
			if cerr := wire.CheckView(v); cerr != nil {
				t.Fatalf("review-create with empty Name returned invalid View: %v", cerr)
			}
			t.Fatal("expected error for empty review-create draft")
		}

		if _, err := a.action(t.Context(), withAction(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, "confirm-create", pageState{
			Mode: "review-create", BoardID: 7, Name: "Missing draft",
		})); err == nil {
			t.Fatal("confirm-create accepted empty DraftID")
		}

		if _, err := a.action(t.Context(), withAction(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, "confirm-edit", pageState{
			Mode: "review-edit", BoardID: 7, ItemID: 42, ColumnID: "status", Label: "Done",
		})); err == nil {
			t.Fatal("confirm-edit accepted empty DraftID")
		}

		if _, err := a.action(t.Context(), withAction(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, "more", pageState{
			Mode: "items", BoardID: 7, BoardName: "Reading", Cursor: "c1", Next: "c1",
		})); err == nil {
			t.Fatal("more accepted repeated cursor st.Next == st.Cursor")
		}
	})
}

func TestLongBoardNameItemsDedupAndCanonicalReceiptKey(t *testing.T) {
	t.Run("long board name and duplicate oversized items pass CheckView", func(t *testing.T) {
		longName := strings.Repeat("B", 2100)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Query string `json:"query"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(in.Query, "items_page") {
				items := make([]item, 0, 310)
				dup := sampleItem()
				items = append(items, dup, dup)
				for i := 100; i < 405; i++ {
					it := sampleItem()
					it.ID = int64(i)
					it.Name = fmt.Sprintf("Item %d", i)
					items = append(items, it)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"boards": []map[string]any{{"items_page": itemsPage{Cursor: "same-cursor", Items: items}}}}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": boardPage{Boards: []board{{ID: 7, Name: longName, URL: "https://example.monday.com/boards/7"}}}})
		}))
		t.Cleanup(srv.Close)

		a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
		r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
		v, err := a.page(t.Context(), r, pageState{})
		if err != nil {
			t.Fatalf("boards page error: %v", err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("boards view with 2100-char name failed CheckView: %v", err)
		}
		row := v.Sections[0].Items[0]
		openRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: row.Action.ID, State: v.State, Values: row.Action.Values,
		}})
		if err != nil || openRes.View == nil {
			t.Fatalf("opening long-named board failed: %v %+v", err, openRes)
		}
		if err := wire.CheckView(openRes.View); err != nil {
			t.Fatalf("items view with duplicates and >300 items failed CheckView: %v", err)
		}
	})

	t.Run("canonical action-scoped receipt key", func(t *testing.T) {
		f := newFakeMonday(t)
		a := f.app()
		store, err := openWriteStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		a.receipts = store
		if _, err := a.doAction(t.Context(), "canon-1", "monday_create_item", []byte(`{"board_id":7,"name":"Keep context"}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.doAction(t.Context(), "canon-1", "monday_create_item", []byte(`{ "name" : "Keep context" , "board_id" : 7 }`)); err != nil {
			t.Fatalf("replayed canonical JSON rejected: %v", err)
		}
		if f.creates != 1 {
			t.Fatalf("creates=%d, want 1", f.creates)
		}
		data, err := os.ReadFile(store.path)
		if err != nil {
			t.Fatal(err)
		}
		var saved map[string]writeReceipt
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(saved["canon-1"].Key, "monday_create_item:") {
			t.Fatalf("receipt key %q is not action-scoped", saved["canon-1"].Key)
		}
	})
}
