package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestViewBoundsDuplicateSlugsLongSlugAndOversizedEditFitCheckView(t *testing.T) {
	longSlug := "long-" + strings.Repeat("a", 135) // 140 chars; "pick:" + slug > 128
	hugeMD := strings.Repeat("# Line\nBody text ", 200)
	var spaceTrack = func(slug, title string) Track {
		var tr Track
		tr.Slug = slug
		tr.Title = title
		tr.Kind = "space"
		tr.Config.Sidebar.Group = "spaces"
		return tr
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/assistant/tracks":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tracks": []Track{
					spaceTrack("", ""),
					spaceTrack("reading", "Reading"),
					spaceTrack("reading", "Reading Duplicate"),
					spaceTrack(longSlug, "Long Slug Space"),
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/assistant/tracks/launchers":
			_ = json.NewEncoder(w).Encode(TrackLaunchers{
				Launchers: []TrackLauncher{
					{Name: "", DisplayName: "Blank"},
					{Name: "claude", DisplayName: "Claude"},
					{Name: "claude", DisplayName: "Claude Dup"},
				},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/conversations"):
			_ = json.NewEncoder(w).Encode(map[string]any{"machine_id": "m1", "total": 0, "conversations": []Conversation{}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/space-md"):
			_ = json.NewEncoder(w).Encode(SpaceMD{Content: hugeMD, Digest: strings.Repeat("d", 2500)})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := &App{client: NewClient(srv.URL, "test-token")}
	for _, uiVer := range []int{2, 6} {
		for _, sel := range []string{"", "reading"} {
			v, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
				Contribution: "home", State: sel, UIVersion: uiVer,
			}})
			if err != nil {
				t.Fatalf("render ui=%d sel=%q: %v", uiVer, sel, err)
			}
			if err := wire.CheckView(v); err != nil {
				t.Fatalf("CheckView ui=%d sel=%q: %v", uiVer, sel, err)
			}
		}
	}

	// editView with >2000-char SPACE.md must pass wire.CheckView without silently truncating into a saveable field.
	edit, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Contribution: "edit", State: "reading", UIVersion: 6,
	}})
	if err != nil {
		t.Fatalf("edit render: %v", err)
	}
	if err := wire.CheckView(edit); err != nil {
		t.Fatalf("edit CheckView with >2000-char SPACE.md: %v", err)
	}

	// Oversized invalid state on edit must not echo >16 KiB back into View.State.
	oversizedEdit, err := a.render(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Contribution: "edit", State: strings.Repeat("s", 17000), UIVersion: 6,
	}})
	if err == nil {
		if cerr := wire.CheckView(oversizedEdit); cerr != nil {
			t.Fatalf("oversized edit state failed CheckView: %v", cerr)
		}
	}
}

func TestArchiveRefusesNonSpaceTracksAndClientRejectsOversizedBody(t *testing.T) {
	archivedBuiltin := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/assistant/tracks":
			if r.Header.Get("X-Oversized") == "1" {
				_, _ = w.Write([]byte(`{"tracks":[]}` + strings.Repeat(" ", 2<<20)))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tracks": []Track{
					{Slug: "main", Title: "Main", Builtin: true},
				},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/assistant/tracks/main":
			archivedBuiltin = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := &App{client: NewClient(srv.URL, "test-token")}
	if _, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "archive",
		Values: map[string]string{"slug": "main"},
	}}); err != nil {
		t.Fatalf("archive non-space returned error: %v", err)
	}
	if archivedBuiltin {
		t.Fatal("archive modified builtin non-space track 'main'")
	}

	// Client must reject >2 MiB responses rather than silently truncating valid JSON + trailing padding.
	overSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tracks":[]}` + strings.Repeat(" ", 2<<20)))
	}))
	defer overSrv.Close()
	c := NewClient(overSrv.URL, "test-token")
	if _, err := c.Tracks(t.Context()); err == nil {
		t.Fatal("client silently accepted oversized >2 MiB response")
	}
}
