package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestDetailViewEmptySummaryAndLongFieldBounds(t *testing.T) {
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	t.Run("empty summary passes CheckView", func(t *testing.T) {
		var it issue
		it.Key = "ENG-1"
		v := detailView(r, pageState{Mode: "detail", IssueKey: "ENG-1"}, it)
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("detailView with empty summary failed CheckView: %v", err)
		}
	})

	t.Run("oversized summary, project, assignee, and comment author pass CheckView", func(t *testing.T) {
		var it issue
		it.Key = "ENG-2"
		it.Fields.Summary = strings.Repeat("S", 2100)
		it.Fields.Project.Name = strings.Repeat("P", 2100)
		it.Fields.Status.Name = strings.Repeat("T", 2100)
		it.Fields.Assignee = &user{DisplayName: strings.Repeat("A", 2100)}
		body, _ := json.Marshal("note")
		it.Fields.Comment.Comments = []comment{{ID: "1", Body: body, Author: user{DisplayName: strings.Repeat("R", 2100)}, Created: "2026-11-09T11:00:00Z"}}
		v := detailView(r, pageState{Mode: "detail", IssueKey: "ENG-2"}, it)
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("detailView with oversized fields failed CheckView: %v", err)
		}
	})
}

func TestReviewCreateStateMostMultilineCapAndValidation(t *testing.T) {
	a := newFakeJira(t).app()

	t.Run("review-create rejects >16KB description exceeding wire.StateMost", func(t *testing.T) {
		res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "zh-Hans", UIVersion: 6, Action: "review-create",
			State:  ext.EncodeState(pageState{Mode: "create", DraftID: "draft-big"}),
			Values: map[string]string{"project_id": "project-eng", "issuetype_id": "task", "summary": "Big", "description": strings.Repeat("文", 6000)},
		}})
		if err == nil {
			if res == nil || res.View == nil || wire.CheckView(res.View) != nil {
				t.Fatalf("review-create returned an oversized View without error: %v", wire.CheckView(res.View))
			}
			t.Fatal("expected error for >16KB review state")
		}
	})

	t.Run("review-create caps 320-line description within wire.ItemsMost", func(t *testing.T) {
		res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "review-create",
			State:  ext.EncodeState(pageState{Mode: "create", DraftID: "draft-lines"}),
			Values: map[string]string{"project_id": "project-eng", "issuetype_id": "task", "summary": "Many lines", "description": strings.Repeat("a\n", 320)},
		}})
		if err != nil || res.View == nil {
			t.Fatalf("review-create with 320 lines failed: %v %+v", err, res)
		}
		if err := wire.CheckView(res.View); err != nil {
			t.Fatalf("review-create with 320 lines failed CheckView: %v", err)
		}
	})

	t.Run("reviewCreateView rejects invalid empty draft", func(t *testing.T) {
		if v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "review", DraftID: "d1"}); err == nil {
			if cerr := wire.CheckView(v); cerr != nil {
				t.Fatalf("review mode with empty draft returned invalid View: %v", cerr)
			}
			t.Fatal("expected error for invalid review draft")
		}
	})
}

func TestCreateViewProjectChoicesListBoundsSearchClipAndMoreGuards(t *testing.T) {
	oversizedToken := strings.Repeat("t", 2020)
	longKey := "KEY-" + strings.Repeat("9", 150)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/project":
			projects := make([]map[string]any, 0, 65)
			projects = append(projects,
				map[string]any{"name": "Ghost without ID"},
				map[string]any{"id": "p1", "name": strings.Repeat("N", 2100)},
				map[string]any{"id": "p1", "name": "Duplicate p1"},
			)
			for i := 2; i < 64; i++ {
				projects = append(projects, map[string]any{"id": fmt.Sprintf("p%d", i), "name": fmt.Sprintf("Project %d", i)})
			}
			_ = json.NewEncoder(w).Encode(projects)
		case "/search/jql":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issues": []map[string]any{
					{"id": "1", "key": "", "fields": map[string]any{"summary": "Empty key"}},
					{"id": "2", "key": longKey, "fields": map[string]any{"summary": "Long key issue", "status": map[string]any{"name": strings.Repeat("S", 2100)}, "project": map[string]any{"key": "ENG"}}},
				},
				"nextPageToken": oversizedToken,
				"isLast":        false,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	createV, err := a.page(t.Context(), r, pageState{Mode: "create", DraftID: "d1"})
	if err != nil {
		t.Fatalf("createView error: %v", err)
	}
	if err := wire.CheckView(createV); err != nil {
		t.Fatalf("createView with ghost/dup/long/65 projects failed CheckView: %v", err)
	}
	for _, ch := range createV.Sections[0].Items[0].Choices {
		if ch.Value == "<nil>" || ch.Value == "" {
			t.Fatalf("invalid project choice rendered: %+v", ch)
		}
	}

	listV, err := a.page(t.Context(), r, pageState{})
	if err != nil {
		t.Fatalf("list view error: %v", err)
	}
	if err := wire.CheckView(listV); err != nil {
		t.Fatalf("list view with empty/long key, long status, and >2000-char token failed CheckView: %v", err)
	}

	searchRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, Action: "search", Values: map[string]string{"q": strings.Repeat("q", 2100)},
	}})
	if err != nil || searchRes.View == nil {
		t.Fatalf("search with 2100-char query failed: %v", err)
	}
	if err := wire.CheckView(searchRes.View); err != nil {
		t.Fatalf("search view with 2100-char query failed CheckView: %v", err)
	}

	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, Action: "more",
		State:  ext.EncodeState(pageState{Token: "tok-1"}),
		Values: map[string]string{"token": "tok-1"},
	}}); err == nil {
		t.Fatal("more accepted repeated page token")
	}

	// 8 tokens of 1902 bytes each fit in wire.StateMost, and appending a 9th exceeds wire.StateMost (16384 bytes).
	bigHist := make([]string, 8)
	for i := range bigHist {
		bigHist[i] = fmt.Sprintf("%d-%s", i, strings.Repeat("x", 1900))
	}
	if res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, Action: "more",
		State:  ext.EncodeState(pageState{History: bigHist[:7], Token: bigHist[7]}),
		Values: map[string]string{"token": "8-" + strings.Repeat("x", 1900)},
	}}); err == nil {
		if res == nil || res.View == nil || wire.CheckView(res.View) != nil {
			t.Fatalf("more returned state exceeding wire.StateMost without error: %v", wire.CheckView(res.View))
		}
		t.Fatal("expected error for oversized history state")
	}
}
