package main

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestSearchLongHrefEmptyDisplayNameAndListCap(t *testing.T) {
	longPath := "Documents/2026/Engineering/Architecture/Subsystems/DistributedCache/ReviewNotes/iteration-04-final-summary-appendix.md"
	entries := []fileEntry{
		{
			Href:          "/remote.php/dav/files/reader/" + longPath,
			DisplayName:   "iteration-04-final-summary-appendix.md",
			ContentType:   "text/markdown",
			ContentLength: 120,
		},
		{
			Href:          "/remote.php/dav/files/reader/" + longPath,
			DisplayName:   "duplicate.md",
			ContentType:   "text/markdown",
			ContentLength: 120,
		},
		{
			Href:          "/remote.php/dav/files/reader/Notes/unnamed.md",
			DisplayName:   "",
			ContentType:   "text/markdown",
			ContentLength: 40,
		},
	}
	for i := range 310 {
		entries = append(entries, fileEntry{
			Href:          fmt.Sprintf("/remote.php/dav/files/reader/Notes/file-%03d.md", i),
			DisplayName:   fmt.Sprintf("file-%03d.md", i),
			ContentType:   "text/markdown",
			ContentLength: 10,
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		data, _ := xml.Marshal(multistatus{Responses: entries})
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL, testUsername, testPassword), receipts: newWriteStore("")}
	v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Query: "notes"})
	if err != nil {
		t.Fatalf("search page: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("search page CheckView: %v", err)
	}
}

func TestCreateViewBackLongTextStateMostAndReviewGuard(t *testing.T) {
	f := newFakeNextcloud(t)
	a := f.app()

	rev, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", DraftID: "d1"}),
		Values: map[string]string{
			"path": "Notes/long-note.md",
			"text": strings.Repeat("a", 3000),
		},
	}})
	if err != nil || rev.View == nil {
		t.Fatalf("review-create 3000 chars: %v", err)
	}
	if err := wire.CheckView(rev.View); err != nil {
		t.Fatalf("review-create CheckView: %v", err)
	}
	back, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "create",
		State:     rev.View.State,
	}})
	if err != nil || back.View == nil {
		t.Fatalf("back to createView: %v", err)
	}
	if err := wire.CheckView(back.View); err != nil {
		t.Fatalf("createView after back CheckView: %v", err)
	}

	// 10,000 quotes escape to >20 KiB in JSON state and exceed wire.StateMost.
	if huge, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", DraftID: "d2"}),
		Values: map[string]string{
			"path": "Notes/escaped.md",
			"text": strings.Repeat(`"`, 10000),
		},
	}}); err == nil && (huge == nil || huge.View == nil || wire.CheckView(huge.View) != nil) {
		t.Fatal("review-create with >16 KiB encoded state returned invalid view instead of error")
	}

	if v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "review-create", DraftID: "d3"}); err == nil && wire.CheckView(v) != nil {
		t.Fatal("review-create with empty path returned invalid view instead of error")
	}
}

func TestCanonicalActionScopedReceiptKey(t *testing.T) {
	f := newFakeNextcloud(t)
	a := f.app()
	first := []byte(`{"path":"Notes/new-note.md","text":"Keep context."}`)
	reordered := []byte(`{ "text": "Keep context.", "path": "Notes/new-note.md" }`)
	if _, err := a.doAction(t.Context(), "canon-1", "nextcloud_create_note", first); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if _, err := a.doAction(t.Context(), "canon-1", "nextcloud_create_note", reordered); err != nil {
		t.Fatalf("reordered retry failed: %v", err)
	}
	if len(f.uploads) != 1 {
		t.Fatalf("reordered retry uploaded twice: %d", len(f.uploads))
	}
}
