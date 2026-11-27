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

func TestCommentBackPreservesDraftClipsLongBodyAndGuardsDraftID(t *testing.T) {
	a := newFakeGitea(t).app()

	t.Run("Back from review-comment preserves draft and clips >2000-char body for CheckView", func(t *testing.T) {
		longBody := strings.Repeat("c", 2500)
		review, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "review-comment",
			State:  ext.EncodeState(pageState{Mode: "comment", Owner: "reading", Repo: "notes", PRID: 42, DraftID: "draft-long"}),
			Values: map[string]string{"body": longBody},
		}})
		if err != nil || review.View == nil || wire.CheckView(review.View) != nil {
			t.Fatalf("review-comment failed: %v", err)
		}
		back, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "comment", State: review.View.State,
		}})
		if err != nil || back.View == nil {
			t.Fatalf("Back from review-comment failed: %v", err)
		}
		if err := wire.CheckView(back.View); err != nil {
			t.Fatalf("commentView after Back with 2500-char body failed CheckView: %v", err)
		}
		if back.View.Sections[0].Items[0].Value == "" {
			t.Fatal("Back from review-comment wiped the user's comment draft")
		}
	})

	t.Run("review-comment rejects empty body and confirm-comment rejects empty DraftID", func(t *testing.T) {
		if _, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{
			Mode: "review-comment", Owner: "reading", Repo: "notes", PRID: 42, DraftID: "d1",
		}); err == nil {
			t.Fatal("review-comment accepted empty comment body")
		}
		if _, err := a.action(t.Context(), withAction(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, "confirm-comment", pageState{
			Mode: "review-comment", Owner: "reading", Repo: "notes", PRID: 42, Body: "No draft ID",
		})); err == nil {
			t.Fatal("confirm-comment accepted empty DraftID")
		}
	})
}

func TestDetailViewEmptyLongTitleAndPaginatedPRDedup(t *testing.T) {
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	t.Run("detailView handles empty title and oversized title", func(t *testing.T) {
		emptyPR := samplePR()
		emptyPR.Title = ""
		v := detailView(r, pageState{Mode: "detail", Owner: "reading", Repo: "notes", PRID: 42, DraftID: "d1"}, emptyPR, nil)
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("detailView with empty title failed CheckView: %v", err)
		}

		longPR := samplePR()
		longPR.Title = strings.Repeat("T", 2100)
		v = detailView(r, pageState{Mode: "detail", Owner: "reading", Repo: "notes", PRID: 42, DraftID: "d2"}, longPR, nil)
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("detailView with 2100-char title failed CheckView: %v", err)
		}
	})

	t.Run("paginated PR list deduplicates overlapping PR numbers", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("page") == "1" {
				out := make([]pullRequest, 0, 50)
				for i := 1; i <= 50; i++ {
					pr := samplePR()
					pr.Number = int64(i)
					out = append(out, pr)
				}
				_ = json.NewEncoder(w).Encode(out)
				return
			}
			pr50 := samplePR()
			pr50.Number = 50
			pr51 := samplePR()
			pr51.Number = 51
			_ = json.NewEncoder(w).Encode([]pullRequest{pr50, pr51})
		}))
		t.Cleanup(srv.Close)

		a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
		v, err := a.page(t.Context(), r, pageState{Owner: "reading", Repo: "notes", Page: 2})
		if err != nil {
			t.Fatalf("PR list page 2 failed: %v", err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("paginated PR list with overlapping PR failed CheckView: %v", err)
		}
		if len(v.Sections[0].Items) != 51 {
			t.Fatalf("got %d rows, want 51 unique PR rows", len(v.Sections[0].Items))
		}
	})
}

func TestCanonicalActionScopedReceiptKey(t *testing.T) {
	f := newFakeGitea(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	if _, err := a.doAction(t.Context(), "canon-1", "gitea_approve", []byte(`{"owner":"reading","repo":"notes","number":42}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "canon-1", "gitea_approve", []byte(`{ "number" : 42 , "repo" : "notes" , "owner" : "reading" }`)); err != nil {
		t.Fatalf("replayed canonical JSON rejected: %v", err)
	}
	if f.approves != 1 {
		t.Fatalf("approves=%d, want 1", f.approves)
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]writeReceipt
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(saved["canon-1"].Key, "gitea_approve:") {
		t.Fatalf("receipt key %q is not action-scoped", saved["canon-1"].Key)
	}
}
