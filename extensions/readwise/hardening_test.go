package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestDetailViewProgressFormatClampAndAuthorClipping(t *testing.T) {
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	v, err := detailView(req, pageState{Mode: "detail", DocumentID: "doc-1"}, document{
		ID:              "doc-1",
		Title:           "Reading",
		Author:          strings.Repeat("a", 1500),
		SiteName:        strings.Repeat("b", 1500),
		SourceURL:       "https://example.com/article",
		ReadingProgress: 0.4,
		HTMLContent:     "<p>Body</p>",
	}, nil)
	if err != nil {
		t.Fatalf("detailView: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("detailView long author+site CheckView: %v", err)
	}
	var progressLabel string
	for _, item := range v.Sections[0].Items {
		if item.Type == wire.ComponentProgress {
			progressLabel = item.Label
		}
	}
	if strings.Contains(progressLabel, "%.0f") || !strings.Contains(progressLabel, "40%") {
		t.Fatalf("progress label = %q, want formatted 40%%", progressLabel)
	}

	for _, rawProgress := range []float64{-0.25, 1.5, math.NaN()} {
		dv, err := detailView(req, pageState{Mode: "detail", DocumentID: "doc-1"}, document{
			ID:              "doc-1",
			Title:           "Reading",
			SourceURL:       "https://example.com/article",
			ReadingProgress: rawProgress,
			HTMLContent:     "<p>Body</p>",
		}, nil)
		if err != nil {
			t.Fatalf("detailView progress=%v: %v", rawProgress, err)
		}
		if err := wire.CheckView(dv); err != nil {
			t.Fatalf("detailView progress=%v CheckView: %v", rawProgress, err)
		}
	}
}

func TestReviewSaveViewLongAndEmptyURLPassesCheckView(t *testing.T) {
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	longURL := "https://example.com/article?q=" + strings.Repeat("x", 2500)
	v := reviewSaveView(req, pageState{Mode: "review-save", DraftID: "d1", SaveURL: longURL})
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("reviewSaveView long URL CheckView: %v", err)
	}

	empty := reviewSaveView(req, pageState{Mode: "review-save", DraftID: "d1", SaveURL: ""})
	if err := wire.CheckView(empty); err != nil {
		t.Fatalf("reviewSaveView empty URL CheckView: %v", err)
	}
}

func TestLongDocumentIDDuplicatePagesInvalidLocationAndGuards(t *testing.T) {
	longID := strings.Repeat("d", 128)
	secondID := strings.Repeat("e", 128)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageCursor") == "" {
			_ = json.NewEncoder(w).Encode(listPage{
				Count:  2,
				Cursor: "page-2",
				Results: []document{
					{ID: longID, Title: "First long doc"},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(listPage{
			Count: 2,
			Results: []document{
				{ID: longID, Title: "First long doc duplicate"},
				{ID: secondID, Title: "Second long doc"},
			},
		})
	}))
	defer srv.Close()
	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}

	v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Location: "new", Page: 1}),
	}})
	if err != nil {
		t.Fatalf("render page 1: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("list with 128-char duplicate IDs failed CheckView: %v", err)
	}
	// Section 0 has 1 Save button + 2 deduplicated document rows = 3 items.
	if len(v.Sections[0].Items) != 3 {
		t.Fatalf("items = %d, want 3 (save button + 2 deduplicated docs)", len(v.Sections[0].Items))
	}

	if _, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		State:     ext.EncodeState(pageState{Location: "bogus"}),
	}}); err == nil {
		t.Fatal("page accepted invalid st.Location")
	}

	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "shortlist",
		State:     ext.EncodeState(pageState{Mode: "detail", DocumentID: "doc-1"}),
	}}); err == nil {
		t.Fatal("shortlist action accepted empty DraftID")
	}

	if _, err := a.callTool(t.Context(), "readwise_documents", json.RawMessage(`{"cursor":"`+strings.Repeat("c", 300)+`"}`)); err == nil {
		t.Fatal("readwise_documents accepted cursor > 256 bytes")
	}
}
