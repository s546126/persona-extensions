package main

import (
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

func TestUIDetailGeneratesDraftIDForMultipleApprovals(t *testing.T) {
	var mu sync.Mutex
	approved := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/projects/ENG/repos/reader/pull-requests/1":
			_ = json.NewEncoder(w).Encode(pullRequest{ID: 1, Title: "First PR"})
		case "/projects/ENG/repos/reader/pull-requests/2":
			_ = json.NewEncoder(w).Encode(pullRequest{ID: 2, Title: "Second PR"})
		case "/projects/ENG/repos/reader/pull-requests/1/activities", "/projects/ENG/repos/reader/pull-requests/2/activities":
			fmt.Fprint(w, `{"values":[]}`)
		case "/projects/ENG/repos/reader/pull-requests/1/approve", "/projects/ENG/repos/reader/pull-requests/2/approve":
			mu.Lock()
			approved = append(approved, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	d1, err := a.page(t.Context(), r, pageState{Mode: "detail", Project: "ENG", Repo: "reader", PRID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6, Action: "approve", State: d1.State}}); err != nil {
		t.Fatalf("first UI approve failed: %v", err)
	}

	d2, err := a.page(t.Context(), r, pageState{Mode: "detail", Project: "ENG", Repo: "reader", PRID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6, Action: "approve", State: d2.State}}); err != nil {
		t.Fatalf("second UI approve failed (missing per-detail DraftID): %v", err)
	}
	if len(approved) != 2 {
		t.Fatalf("approved=%d, want 2", len(approved))
	}

	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, Action: "approve",
		State: ext.EncodeState(pageState{Mode: "detail", Project: "ENG", Repo: "reader", PRID: 1}),
	}}); err == nil {
		t.Fatal("approve accepted empty DraftID")
	}
}

func TestDetailViewEmptyAndOversizedFields(t *testing.T) {
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	t.Run("empty title, reviewer name, and comment author pass CheckView", func(t *testing.T) {
		pr := pullRequest{ID: 7}
		pr.Reviewers = []struct {
			User     user   `json:"user"`
			Approved bool   `json:"approved"`
			Status   string `json:"status"`
		}{{User: user{Name: ""}, Approved: false, Status: "UNAPPROVED"}}
		var act activity
		act.Comment.Text = "A comment without author displayName"
		v := detailView(r, pageState{Mode: "detail", Project: "ENG", Repo: "reader", PRID: 7, DraftID: "d1"}, pr, []activity{act})
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("detailView with empty fields failed CheckView: %v", err)
		}
	})

	t.Run("oversized title, author, branch, reviewer, and comment author pass CheckView", func(t *testing.T) {
		pr := pullRequest{ID: 8, Title: strings.Repeat("T", 2100)}
		pr.Author.User.DisplayName = strings.Repeat("A", 2100)
		pr.FromRef.DisplayID = strings.Repeat("F", 2100)
		pr.ToRef.DisplayID = strings.Repeat("B", 2100)
		pr.Reviewers = []struct {
			User     user   `json:"user"`
			Approved bool   `json:"approved"`
			Status   string `json:"status"`
		}{{User: user{DisplayName: strings.Repeat("R", 2100)}, Approved: true, Status: strings.Repeat("S", 2100)}}
		var act activity
		act.Comment.Author.DisplayName = strings.Repeat("C", 2100)
		act.Comment.Text = "Body"
		v := detailView(r, pageState{Mode: "detail", Project: "ENG", Repo: "reader", PRID: 8, DraftID: "d2"}, pr, []activity{act})
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("detailView with oversized fields failed CheckView: %v", err)
		}
	})
}

func TestListDedupSubtitleClipSearchBoundsAndMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next := 50
		pr1 := pullRequest{ID: 42, Title: "PR one"}
		pr1.Author.User.DisplayName = strings.Repeat("A", 2100)
		pr1.FromRef.DisplayID = "feat"
		if r.URL.Query().Get("start") == "0" {
			_ = json.NewEncoder(w).Encode(prPage{Values: []pullRequest{pr1}, Size: 1, Limit: 50, Start: 0, IsLastPage: false, NextPageStart: &next})
		} else {
			pr2 := pullRequest{ID: 43, Title: "PR two"}
			pr2.Author.User.DisplayName = "Bob"
			pr2.FromRef.DisplayID = "fix"
			_ = json.NewEncoder(w).Encode(prPage{Values: []pullRequest{pr1, pr2}, Size: 2, Limit: 50, Start: 50, IsLastPage: true})
		}
	}))
	t.Cleanup(srv.Close)

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	v, err := a.page(t.Context(), r, pageState{Project: "ENG", Repo: "reader", Start: 50})
	if err != nil {
		t.Fatalf("list page failed: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("list view with duplicate PR and oversized subtitle failed CheckView: %v", err)
	}
	if len(v.Sections[0].Items) != 2 {
		t.Fatalf("got %d rows, want 2 deduped rows", len(v.Sections[0].Items))
	}

	searchRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, Action: "search",
		State:  ext.EncodeState(pageState{Project: "ENG", Repo: "reader"}),
		Values: map[string]string{"q": strings.Repeat("q", 2100)},
	}})
	if err != nil || searchRes.View == nil {
		t.Fatalf("search with 2100-char query failed: %v", err)
	}
	if err := wire.CheckView(searchRes.View); err != nil {
		t.Fatalf("search view with 2100-char query failed CheckView: %v", err)
	}

	if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(pageState{Mode: "bogus"})}}); err == nil {
		t.Fatal("readState accepted unknown mode")
	}
}
