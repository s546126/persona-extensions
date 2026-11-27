package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestDetailViewBudgetWithReviewersAndComments(t *testing.T) {
	item := mergeRequest{
		IID:         7,
		ProjectID:   17,
		Title:       "Release 5.0",
		Description: changelog(400),
		WebURL:      "https://gitlab.example.com/acme/app/-/merge_requests/7",
	}
	for i := range 100 {
		item.Reviewers = append(item.Reviewers, user{
			Name:     fmt.Sprintf("Reviewer %d", i),
			Username: fmt.Sprintf("rev%d", i),
		})
	}
	var notes []note
	for i := range 150 {
		notes = append(notes, note{
			ID:     int64(i + 1),
			Body:   fmt.Sprintf("Comment line %d", i),
			Author: user{Name: "Ann", Username: "ann"},
		})
	}
	v := detailView(
		&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}},
		pageState{Project: "17", IID: 7, Mode: "detail", DraftID: "draft-1"},
		item,
		notes,
		"6",
	)
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("detailView with 400-line desc + 100 reviewers + 150 notes + next failed CheckView (items=%d): %v", viewItemCount(v), err)
	}
	if n := viewItemCount(v); n > wire.ItemsMost {
		t.Fatalf("detailView exceeded wire.ItemsMost: %d > %d", n, wire.ItemsMost)
	}
}

func TestDetailViewEmptyTitleLongConfirmAndReviewerUsername(t *testing.T) {
	t.Run("empty title falls back to non-empty text", func(t *testing.T) {
		item := mergeRequest{
			IID:       9,
			ProjectID: 17,
			Title:     "   ",
			Reviewers: []user{{Name: "Alice", Username: ""}},
		}
		v := detailView(
			&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}},
			pageState{Project: "17", IID: 9, Mode: "detail", DraftID: "draft-empty"},
			item,
			nil,
		)
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("empty title detailView failed CheckView: %v", err)
		}
		for _, sec := range v.Sections {
			if sec.Title == textsFor("en").reviewers {
				for _, comp := range sec.Items {
					if comp.Subtitle == "@" {
						t.Fatalf("reviewer without username should not render bare '@': %+v", comp)
					}
				}
			}
		}
	})

	t.Run("oversized title and reviewer username stay within CheckView limits", func(t *testing.T) {
		item := mergeRequest{
			IID:       10,
			ProjectID: 17,
			Title:     strings.Repeat("标题", 1500),
			Reviewers: []user{{Name: "Bob", Username: strings.Repeat("u", 2500)}},
		}
		v := detailView(
			&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}},
			pageState{Project: "17", IID: 10, Mode: "detail", DraftID: "draft-long"},
			item,
			nil,
		)
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("oversized title/username detailView failed CheckView: %v", err)
		}
	})
}

func TestCommentViewBackLongDraftAndConfirmRequiresDraftID(t *testing.T) {
	f := newFakeGitLab(t)
	a := f.app()

	longBody := strings.Repeat("a", 2500)
	reviewRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-comment",
		State:     ext.EncodeState(pageState{Scope: "assigned_to_me", Project: "17", IID: 42, Mode: "comment", DraftID: "draft-long"}),
		Values:    map[string]string{"body": longBody},
	}})
	if err != nil || reviewRes.View == nil {
		t.Fatalf("review-comment failed: %v %+v", err, reviewRes)
	}
	if err := wire.CheckView(reviewRes.View); err != nil {
		t.Fatalf("review-comment CheckView failed: %v", err)
	}

	backRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "edit-comment",
		State:     reviewRes.View.State,
	}})
	if err != nil || backRes.View == nil {
		t.Fatalf("edit-comment failed: %v %+v", err, backRes)
	}
	if err := wire.CheckView(backRes.View); err != nil {
		t.Fatalf("edit-comment with 2500-char draft failed CheckView: %v", err)
	}

	// confirm-comment must reject an empty DraftID rather than colliding on "ui-comment:".
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "confirm-comment",
		State:     ext.EncodeState(pageState{Project: "17", IID: 42, Mode: "review-comment", Body: "hello", DraftID: ""}),
	}}); err == nil {
		t.Fatal("confirm-comment with empty DraftID unexpectedly succeeded")
	}
}

func TestListDedupAcrossPagesCountAndInvalidNext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			_ = json.NewEncoder(w).Encode([]mergeRequest{
				{ID: 100, IID: 1, ProjectID: 17, Title: "First MR", Author: user{Name: "Ann"}},
				{ID: 101, IID: 0, ProjectID: 0, Title: "Invalid MR"},
			})
		case "2":
			// Repeats ID 100 and returns a non-advancing X-Next-Page "2".
			w.Header().Set("X-Next-Page", "2")
			_ = json.NewEncoder(w).Encode([]mergeRequest{
				{ID: 100, IID: 1, ProjectID: 17, Title: "First MR", Author: user{Name: "Ann"}},
				{ID: 102, IID: 2, ProjectID: 17, Title: "Second MR", Author: user{Name: "Bob"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	a := &app{client: newClient(srv.URL+"/api/v4", "tok"), receipts: newWriteStore("")}
	v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Scope: "assigned_to_me", Page: 2})
	if err != nil {
		t.Fatalf("page failed: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("paginated duplicate list failed CheckView: %v", err)
	}
	if got := len(v.Sections[0].Items); got != 2 {
		t.Fatalf("expected 2 deduplicated valid rows, got %d", got)
	}
	if want := fmt.Sprintf(textsFor("en").results, 2); v.Sections[0].Title != want {
		t.Fatalf("section title = %q, want %q", v.Sections[0].Title, want)
	}
	if v.Sections[0].More != nil {
		t.Fatal("non-advancing X-Next-Page must not expose More")
	}
}

func TestConcurrentInitAndReadsNoRace(t *testing.T) {
	f := newFakeGitLab(t)
	a := f.app()

	initCfg, _ := json.Marshal(map[string]string{"instance_url": f.srv.URL})
	initReq := &ext.Init{
		InitializeParams: wire.InitializeParams{
			Config:  initCfg,
			Secrets: map[string]string{"access_token": "tok-1"},
		},
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for range 10 {
			_, _ = a.init(context.Background(), initReq, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for range 10 {
			_, _ = a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
		}
	}()
	go func() {
		defer wg.Done()
		for range 10 {
			_, _ = a.callTool(context.Background(), "gitlab_merge_requests", json.RawMessage(`{}`))
			_ = a.status()
		}
	}()
	wg.Wait()
}
