package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestListFilteredCountLongKeysAndDedup(t *testing.T) {
	longID := strings.Repeat("x", 126) // "issue-" + 126 = 132 > 128 wire key limit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"viewer": map[string]any{
					"id": "u1",
					"assignedIssues": map[string]any{
						"nodes": []map[string]any{
							{"id": longID, "identifier": "ENG-1", "title": "Match one", "state": map[string]any{"id": "s1", "name": "In Progress", "type": "started"}, "team": map[string]any{"id": "t1", "key": "ENG", "name": "Engineering"}},
							{"id": longID, "identifier": "ENG-1", "title": "Match duplicate", "state": map[string]any{"id": "s1", "name": "In Progress", "type": "started"}, "team": map[string]any{"id": "t1", "key": "ENG", "name": "Engineering"}},
							{"id": "other-1", "identifier": "ENG-2", "title": "Unrelated", "state": map[string]any{"id": "s1", "name": "In Progress", "type": "started"}, "team": map[string]any{"id": "t1", "key": "ENG", "name": "Engineering"}},
						},
						"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
					},
				},
			},
		})
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL, testKey), receipts: newWriteStore("")}
	v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Query: "Match"}),
	}})
	if err != nil {
		t.Fatalf("render list: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView(list): %v", err)
	}
	if v.Sections[0].Title != "1 active issues" {
		t.Fatalf("section title = %q, want filtered count %q", v.Sections[0].Title, "1 active issues")
	}

	// 120-rune CJK query accepted; >200-rune query rejected.
	cjk := strings.Repeat("项", 120)
	if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(pageState{Query: cjk})}}); err != nil {
		t.Fatalf("readState rejected 120-rune CJK query: %v", err)
	}
	if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(pageState{Query: strings.Repeat("a", 201)})}}); err == nil {
		t.Fatal("readState accepted >200-rune query")
	}
}

func TestDetailViewBoundsEmptyDescBadURLAndComments(t *testing.T) {
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	// Bad URL + newline-only description + empty identifier + huge title.
	var item issue
	item.ID = "i1"
	item.Identifier = ""
	item.Title = strings.Repeat("t", 2500)
	item.Description = "\n\n"
	item.URL = "http://linear.app/not-https"
	item.Team.Name = strings.Repeat("team", 600)
	item.State.Name = "Started"
	item.State.Type = "started"
	item.Assignee = &user{DisplayName: strings.Repeat("u", 2500)}
	v := detailView(req, pageState{IssueID: "i1", Mode: "detail"}, item)
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("detailView bad URL / empty desc / long fields: %v", err)
	}

	// Huge multiline description + many comments including empty author/timestamp.
	item2 := issue{
		ID:          "i2",
		Identifier:  "ENG-2",
		Title:       "Large issue",
		Description: strings.Repeat("desc line\n", 320),
		URL:         "",
	}
	for range 80 {
		item2.Comments.Nodes = append(item2.Comments.Nodes, comment{
			ID:   "c",
			Body: "line 1\nline 2\nline 3",
		})
	}
	v2 := detailView(req, pageState{IssueID: "i2", Mode: "detail"}, item2)
	if err := wire.CheckView(v2); err != nil {
		t.Fatalf("detailView 320-line desc + 80 comments: %v", err)
	}
}

func TestReviewCreateValidatesDescriptionStateAndCapsView(t *testing.T) {
	f := newFakeLinear(t)
	a := f.app()

	// Control character in description must be rejected at review-create.
	r := &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Mode: "create", DraftID: "d1"}),
		Action:    "review-create",
		Values:    map[string]string{"team_id": "team-eng", "title": "Fix bug", "description": "bad\x01ctrl"},
	}}
	if _, err := a.action(t.Context(), r); err == nil {
		t.Fatal("review-create accepted control character in description")
	}

	// Huge description exceeding wire.StateMost must fail cleanly.
	r.Values["description"] = strings.Repeat("x", 20000)
	if res, err := a.action(t.Context(), r); err == nil {
		if checkErr := wire.CheckView(res.View); checkErr != nil {
			t.Fatalf("review-create returned View violating CheckView: %v", checkErr)
		}
		t.Fatal("review-create should reject draft exceeding wire.StateMost")
	}

	// 350 short lines (< 16KB state) must be capped within wire.ItemsMost.
	r.Values["description"] = strings.Repeat("a\n", 350)
	res, err := a.action(t.Context(), r)
	if err != nil {
		t.Fatalf("review-create 350 short lines: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("CheckView(review-create 350 lines): %v", err)
	}
}

func TestDescribeLinearCreateValidatesOptionalFields(t *testing.T) {
	for _, raw := range []string{
		`{"team_id":"team-eng","title":"Valid","priority":9}`,
		`{"team_id":"team-eng","title":"Valid","assignee_id":"bad\nid"}`,
		`{"team_id":"team-eng","title":"Valid","state_id":"bad\nid"}`,
	} {
		if _, err := describeAction("linear_create", json.RawMessage(raw)); err == nil {
			t.Fatalf("describeAction accepted invalid linear_create args: %s", raw)
		}
	}
}
