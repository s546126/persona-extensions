package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func hasButtonAction(v *wire.View, actionID string) bool {
	for _, s := range v.Sections {
		for _, item := range s.Items {
			if item.Type == wire.ComponentButton && item.Action != nil && item.Action.ID == actionID {
				return true
			}
		}
	}
	return false
}

func TestSubmitCreateRendersReviewViewAndTrimsLabels(t *testing.T) {
	f := newFakeTodoist(t)
	a := f.app()
	r := &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		TZ:        "Asia/Shanghai",
		State:     ext.EncodeState(pageState{Mode: "create", CreateID: "draft-1"}),
		Action:    "submit-create",
		Values: map[string]string{
			"content":     "Plan sprint",
			"description": "Review backlog",
			"due_date":    "2026-11-15",
			"priority":    "1",
			"project_id":  "project-work",
			"labels":      "research, planning, ",
		},
	}}
	res, err := a.action(t.Context(), r)
	if err != nil {
		t.Fatalf("submit-create: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("CheckView(review): %v", err)
	}
	if !hasButtonAction(res.View, "confirm-create") {
		t.Fatalf("submit-create did not render reviewCreateView with confirm-create button: %+v", res.View)
	}
	var st pageState
	if err := json.Unmarshal([]byte(res.View.State), &st); err != nil {
		t.Fatalf("decode review state: %v", err)
	}
	if !slices.Equal(st.Create.Labels, []string{"research", "planning"}) {
		t.Fatalf("labels = %q, want trimmed [research planning]", st.Create.Labels)
	}
}

func TestSearchActionPassesCheckViewAndBoundsQuery(t *testing.T) {
	f := newFakeTodoist(t)
	a := f.app()

	// Normal search action must produce a valid wire.View whose Filter.Value is in Filter.Choices.
	r := &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		TZ:        "Asia/Shanghai",
		Action:    "search",
		Values:    map[string]string{"q": "release"},
	}}
	res, err := a.action(t.Context(), r)
	if err != nil {
		t.Fatalf("search action: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("CheckView(search): %v", err)
	}

	// 120-rune CJK query (360 bytes) must be accepted.
	cjk := strings.Repeat("计", 120)
	r.Values = map[string]string{"q": cjk}
	res, err = a.action(t.Context(), r)
	if err != nil {
		t.Fatalf("search CJK: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("CheckView(search CJK): %v", err)
	}

	// Overlong query (>200 runes) must be rejected in both search action and state decode.
	tooLong := strings.Repeat("a", 201)
	r.Values = map[string]string{"q": tooLong}
	if _, err := a.action(t.Context(), r); err == nil {
		t.Fatal("search accepted query > 200 runes")
	}
	if _, err := a.state(&ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(pageState{Query: tooLong})}}); err == nil {
		t.Fatal("state accepted query > 200 runes")
	}
}

func TestCompleteRequiresCreateID(t *testing.T) {
	f := newFakeTodoist(t)
	a := f.app()
	r := &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		TZ:        "Asia/Shanghai",
		State:     ext.EncodeState(pageState{Mode: "detail", TaskID: "opaque-long-task-1"}),
		Action:    "complete",
	}}
	if _, err := a.action(t.Context(), r); err == nil {
		t.Fatal("complete without CreateID should fail instead of colliding on empty receipt key")
	}
}

func TestDetailAndReviewViewBounds(t *testing.T) {
	f := newFakeTodoist(t)
	a := f.app()
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6, TZ: "Asia/Shanghai"}}

	// Empty-line description must not emit an empty section.
	v := detailView(req, pageState{TaskID: "t1", Mode: "detail", CreateID: "c1"}, task{
		ID:          "t1",
		Content:     strings.Repeat("x", 2500),
		Description: "\n\n",
		ProjectID:   "p1",
		Labels:      []string{strings.Repeat("lbl", 800)},
	}, contextData{Projects: map[string]project{"p1": {ID: "p1", Name: strings.Repeat("proj", 600)}}}, time.UTC)
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("detailView empty-line desc & long fields: %v", err)
	}

	// Huge multiline description must stay under wire.ItemsMost.
	v = detailView(req, pageState{TaskID: "t2", Mode: "detail", CreateID: "c2"}, task{
		ID:          "t2",
		Content:     "",
		Description: strings.Repeat("line\n", 400),
	}, contextData{}, time.UTC)
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("detailView 400-line desc: %v", err)
	}

	// reviewCreateView with 350 short lines must stay under wire.ItemsMost.
	rv, err := a.reviewCreateView(t.Context(), req, pageState{
		Mode:     "review",
		CreateID: "c3",
		Create: createArgs{
			Content:     strings.Repeat("task ", 500),
			Description: strings.Repeat("d\n", 350),
			ProjectID:   "project-work",
			Labels:      []string{"a", "b"},
		},
	})
	if err != nil {
		t.Fatalf("reviewCreateView: %v", err)
	}
	if err := wire.CheckView(rv); err != nil {
		t.Fatalf("CheckView(reviewCreateView 350 lines): %v", err)
	}

	// submit-create with huge description exceeding wire.StateMost must fail cleanly instead of returning invalid View.
	res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		TZ:        "Asia/Shanghai",
		State:     ext.EncodeState(pageState{Mode: "create", CreateID: "c4"}),
		Action:    "submit-create",
		Values: map[string]string{
			"content":     "Huge draft",
			"description": strings.Repeat("a", 16300),
			"priority":    "4",
		},
	}})
	if err == nil {
		if checkErr := wire.CheckView(res.View); checkErr != nil {
			t.Fatalf("submit-create returned View violating CheckView: %v", checkErr)
		}
		t.Fatal("submit-create should reject draft exceeding wire.StateMost")
	}
}

func TestListDeduplicatesAndBoundsLongTaskKeys(t *testing.T) {
	longID := strings.Repeat("t", 126) // "task-" + 126 = 131 > 128 wire key limit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tasks":
			if r.URL.Query().Get("cursor") == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"results": []task{
						{ID: longID, Content: "Long ID task"},
						{ID: "dup-1", Content: "Duplicate task"},
					},
					"next_cursor": "p2",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"results": []task{
					{ID: "dup-1", Content: "Duplicate task on page 2"},
					{ID: "empty-content", Content: ""},
				},
				"next_cursor": "",
			})
		case "/projects", "/sections", "/labels":
			_, _ = w.Write([]byte(`{"results":[],"next_cursor":""}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		TZ:        "UTC",
		State:     ext.EncodeState(pageState{View: "all", Pages: 1}),
	}})
	if err != nil {
		t.Fatalf("render list: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("CheckView(list): %v", err)
	}
}
