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

func TestTaskSummaryDueOnAndDueAtNotConcatenated(t *testing.T) {
	item := task{
		GID:   "task-1",
		Name:  "Scheduled review",
		DueOn: "2026-12-01",
		DueAt: "2026-12-01T18:00:00.000Z",
	}
	got := taskSummary(item)
	if strings.Contains(got, "2026-12-012026-12-01") {
		t.Fatalf("taskSummary concatenated due_on and due_at without separator: %q", got)
	}
	if got != "2026-12-01T18:00:00.000Z" {
		t.Fatalf("taskSummary = %q, want %q", got, "2026-12-01T18:00:00.000Z")
	}
}

func TestReviewCreateCJKNotesMultilineBackAndDraftID(t *testing.T) {
	f := newFakeAsana(t)
	a := f.app()

	// 4000 Han runes (12,000 bytes, <16 KiB) are valid under the 10,000-rune limit and must survive review -> confirm.
	hanNotes := strings.Repeat("注", 4000)
	revHan, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "zh-Hans",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", WorkspaceGID: "workspace-1", DraftID: "d-han"}),
		Values: map[string]string{
			"name":  "中文备注测试",
			"notes": hanNotes,
		},
	}})
	if err != nil || revHan.View == nil {
		t.Fatalf("review-create 4000 Han notes: %v", err)
	}
	if err := wire.CheckView(revHan.View); err != nil {
		t.Fatalf("review-create 4000 Han CheckView: %v", err)
	}
	confirm, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "zh-Hans",
		UIVersion: 6,
		Action:    "confirm-create",
		State:     revHan.View.State,
	}})
	if err != nil || !confirm.Pop {
		t.Fatalf("confirm-create 4000 Han notes failed: %v", err)
	}

	// 6000 Han runes (~18 KiB) exceed wire.StateMost and must fail cleanly.
	if huge, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "zh-Hans",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", WorkspaceGID: "workspace-1", DraftID: "d-huge"}),
		Values: map[string]string{
			"name":  "超大备注",
			"notes": strings.Repeat("注", 6000),
		},
	}}); err == nil && (huge == nil || huge.View == nil || wire.CheckView(huge.View) != nil) {
		t.Fatal("review-create >16 KiB CJK notes returned invalid view instead of error")
	}

	// 350-line notes (>300 components if uncapped) + back to createView (Field.Value > 2000 if unclipped).
	longMulti := strings.Repeat("line\n", 350) + strings.Repeat("x", 2200)
	revMulti, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", WorkspaceGID: "workspace-1", DraftID: "d-multi"}),
		Values: map[string]string{
			"name":  "Multiline task",
			"notes": longMulti,
		},
	}})
	if err != nil || revMulti.View == nil {
		t.Fatalf("review-create multiline: %v", err)
	}
	if err := wire.CheckView(revMulti.View); err != nil {
		t.Fatalf("review-create multiline CheckView: %v", err)
	}
	back, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "create",
		State:     revMulti.View.State,
	}})
	if err != nil || back.View == nil {
		t.Fatalf("back to createView: %v", err)
	}
	if err := wire.CheckView(back.View); err != nil {
		t.Fatalf("createView after back CheckView: %v", err)
	}

	// Empty DraftID must be rejected by confirm-create.
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "confirm-create",
		State:     ext.EncodeState(pageState{Mode: "review-create", WorkspaceGID: "workspace-1", Name: "No draft"}),
	}}); err == nil {
		t.Fatal("confirm-create accepted empty DraftID")
	}
}

func TestListBoundsDedupLongWorkspaceAndCanonicalReceiptKey(t *testing.T) {
	ws := []workspace{
		{GID: "ws-long", Name: strings.Repeat("W", 2200)},
		{GID: "ws-long", Name: "Duplicate workspace"},
	}
	for i := range 310 {
		ws = append(ws, workspace{GID: fmt.Sprintf("ws-%03d", i), Name: fmt.Sprintf("Workspace %d", i)})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/workspaces":
			_ = json.NewEncoder(w).Encode(page[workspace]{Data: ws})
		case "/tasks":
			_ = json.NewEncoder(w).Encode(page[task]{Data: []task{{GID: "t1", Name: "Task 1"}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}

	v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
	if err != nil {
		t.Fatalf("render workspaces: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("workspaces CheckView: %v", err)
	}
	opened, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "workspace",
		State:     v.State,
		Values:    v.Sections[0].Items[0].Action.Values,
	}})
	if err != nil || opened.View == nil || wire.CheckView(opened.View) != nil {
		t.Fatalf("open long-named workspace failed: %v", err)
	}

	// Repeating the current offset in action("more") must be rejected.
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "more",
		State:     ext.EncodeState(pageState{Mode: "tasks", WorkspaceGID: "ws-long", WorkspaceName: "WS", Offset: "same-offset"}),
		Values:    map[string]string{"offset": "same-offset"},
	}}); err == nil {
		t.Fatal("more accepted repeating the current offset")
	}

	// Canonical action-scoped receipt key.
	f := newFakeAsana(t)
	fa := f.app()
	first := []byte(`{"workspace_gid":"workspace-1","name":"Canon","notes":"A"}`)
	reordered := []byte(`{ "notes": "A", "name": "Canon", "workspace_gid": "workspace-1" }`)
	if _, err := fa.doAction(t.Context(), "req-canon", "asana_create_task", first); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := fa.doAction(t.Context(), "req-canon", "asana_create_task", reordered); err != nil {
		t.Fatalf("reordered retry failed: %v", err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("reordered retry created twice: %d", len(f.creates))
	}
}
