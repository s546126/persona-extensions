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

func TestCreateViewBackLongDescEditBackPreservesStateAndConfirmClip(t *testing.T) {
	a := newFakeShortcut(t).app()

	t.Run("Back from review-create with >2000-char description passes CheckView", func(t *testing.T) {
		review, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "review-create",
			State:  ext.EncodeState(pageState{Mode: "create", DraftID: "draft-long"}),
			Values: map[string]string{"name": "Long desc", "description": strings.Repeat("d", 2500), "state_id": "10", "story_type": "feature"},
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

	t.Run("Back from review-edit preserves selected state and review-edit clips long confirm", func(t *testing.T) {
		review, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "review-edit",
			State:  ext.EncodeState(pageState{Mode: "edit", StoryID: 42, WorkflowStateID: 10, DraftID: "draft-edit"}),
			Values: map[string]string{"state_id": "11"},
		}})
		if err != nil || review.View == nil || wire.CheckView(review.View) != nil {
			t.Fatalf("review-edit failed: %v", err)
		}
		back, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
			Locale: "en", UIVersion: 6, Action: "edit", State: review.View.State,
		}})
		if err != nil || back.View == nil || wire.CheckView(back.View) != nil {
			t.Fatalf("Back from review-edit failed: %v", err)
		}
		if got := back.View.Sections[0].Items[0].Value; got != "11" {
			t.Fatalf("Back from review-edit reset state_id to %q, want %q", got, "11")
		}

		longStory := sampleStory()
		longStory.Name = strings.Repeat("N", 2100)
		v := editView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{
			Mode: "review-edit", StoryID: 42, WorkflowStateID: 11, DraftID: "draft-edit",
		}, longStory, sampleWorkflows())
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("editView review-edit with 2100-char story name failed CheckView: %v", err)
		}
	})

	t.Run("confirm actions reject empty DraftID", func(t *testing.T) {
		if _, err := a.action(t.Context(), withAction(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, "confirm-create", pageState{
			Mode: "review-create", Name: "No draft", WorkflowStateID: 10,
		})); err == nil {
			t.Fatal("confirm-create accepted empty DraftID")
		}
		if _, err := a.action(t.Context(), withAction(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, "confirm-edit", pageState{
			Mode: "review-edit", StoryID: 42, WorkflowStateID: 11,
		})); err == nil {
			t.Fatal("confirm-edit accepted empty DraftID")
		}
	})
}

func TestStoriesDedupLongSubtitleNextTokenBoundsAndMoreLoop(t *testing.T) {
	oversizedNext := "/api/v3/search/stories?next=" + strings.Repeat("a", 2000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/member":
			_ = json.NewEncoder(w).Encode(member{ID: "member-1", MentionName: "reader", Name: "Example User"})
		case "/search/stories":
			s1 := sampleStory()
			s1.Deadline = strings.Repeat("9", 2100)
			_ = json.NewEncoder(w).Encode(storyPage{Data: []story{s1, s1}, Next: oversizedNext})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{})
	if err != nil {
		t.Fatalf("stories page error: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("stories view with duplicate ID, long subtitle, and >2000-char Next failed CheckView: %v", err)
	}
	if len(v.Sections[0].Items) != 1 {
		t.Fatalf("got %d rows, want 1 deduped row", len(v.Sections[0].Items))
	}

	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, Action: "more",
		State:  ext.EncodeState(pageState{Next: "/api/v3/search/stories?next=same"}),
		Values: map[string]string{"next": "/api/v3/search/stories?next=same"},
	}}); err == nil {
		t.Fatal("more accepted repeated next token")
	}
}

func TestCanonicalActionScopedReceiptKey(t *testing.T) {
	f := newFakeShortcut(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	if _, err := a.doAction(t.Context(), "canon-1", "shortcut_update_story_state", []byte(`{"story_id":42,"workflow_state_id":11}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "canon-1", "shortcut_update_story_state", []byte(`{ "workflow_state_id" : 11 , "story_id" : 42 }`)); err != nil {
		t.Fatalf("replayed canonical JSON rejected: %v", err)
	}
	if len(f.updates) != 1 {
		t.Fatalf("updates=%d, want 1", len(f.updates))
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]writeReceipt
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(saved["canon-1"].Key, "shortcut_update_story_state:") {
		t.Fatalf("receipt key %q is not action-scoped", saved["canon-1"].Key)
	}
}
