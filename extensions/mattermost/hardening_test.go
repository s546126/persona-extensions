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

func TestUnreadAndChannelsDedupCapEmptyNameAndLongType(t *testing.T) {
	unreadHits := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/users/me/teams":
			_ = json.NewEncoder(w).Encode([]team{
				{ID: "team-1", DisplayName: "Team One"},
				{ID: "team-2", DisplayName: "Team Two"},
			})
		case "/api/v4/users/me/teams/team-1/channels":
			out := []channel{
				{ID: "dm-shared", TeamID: "", DisplayName: "", Name: "", Type: strings.Repeat("D", 2500)},
				{ID: "dm-shared", TeamID: "", DisplayName: "", Name: "", Type: "D"},
			}
			for i := 1; i <= 310; i++ {
				out = append(out, channel{ID: fmt.Sprintf("ch-%d", i), TeamID: "team-1", DisplayName: fmt.Sprintf("Channel %d", i), Type: "O"})
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/api/v4/users/me/teams/team-2/channels":
			// Mattermost returns the same DM channel under every team the user belongs to.
			_ = json.NewEncoder(w).Encode([]channel{
				{ID: "dm-shared", TeamID: "", DisplayName: "", Name: "", Type: "D"},
			})
		default:
			if strings.HasPrefix(r.URL.Path, "/api/v4/users/me/channels/") && strings.HasSuffix(r.URL.Path, "/unread") {
				id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v4/users/me/channels/"), "/unread")
				if id == "dm-shared" {
					mu.Lock()
					unreadHits++
					mu.Unlock()
				}
				_ = json.NewEncoder(w).Encode(unread{ChannelID: id, MessageCount: 2, MentionCount: 1})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL, testToken)}
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}

	inboxView, err := a.page(t.Context(), req, pageState{})
	if err != nil {
		t.Fatalf("inbox page: %v", err)
	}
	if err := wire.CheckView(inboxView); err != nil {
		t.Fatalf("inbox CheckView: %v", err)
	}
	if len(inboxView.Sections[0].Items) > maxViewItems {
		t.Fatalf("inbox items %d exceed cap %d", len(inboxView.Sections[0].Items), maxViewItems)
	}
	if inboxView.Sections[0].Items[0].Title == "" {
		t.Fatal("blank channel name produced empty row title")
	}
	mu.Lock()
	hits := unreadHits
	mu.Unlock()
	if hits != 1 {
		t.Fatalf("dm-shared unread fetched %d times, want 1 (deduplicated across teams)", hits)
	}

	channelsView, err := a.page(t.Context(), req, pageState{Mode: "channels", TeamID: "team-1"})
	if err != nil {
		t.Fatalf("channels page: %v", err)
	}
	if err := wire.CheckView(channelsView); err != nil {
		t.Fatalf("channels CheckView: %v", err)
	}
	if len(channelsView.Sections[0].Items) > maxViewItems {
		t.Fatalf("channels items %d exceed cap %d", len(channelsView.Sections[0].Items), maxViewItems)
	}
}

func TestPostsCursorCycleDedupAndBackResetsState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/users/user-2":
			_ = json.NewEncoder(w).Encode(user{ID: "user-2", Username: "reviewer"})
		case "/api/v4/channels/channel-1/posts":
			before := r.URL.Query().Get("before")
			if before == "" {
				_ = json.NewEncoder(w).Encode(postList{
					Order: []string{"post-2", "post-1"},
					Posts: map[string]post{
						"post-2": {ID: "post-2", UserID: "user-2", Message: "second"},
						"post-1": {ID: "post-1", UserID: "user-2", Message: "first"},
					},
					PrevPostID: "post-0",
				})
				return
			}
			// Simulate overlapping post-1 and a cursor cycle back to post-1.
			_ = json.NewEncoder(w).Encode(postList{
				Order: []string{"post-1"},
				Posts: map[string]post{
					"post-1": {ID: "post-1", UserID: "user-2", Message: "first"},
				},
				PrevPostID: "post-0",
			})
		case "/api/v4/users/me/teams":
			_ = json.NewEncoder(w).Encode([]team{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := &app{client: newClient(srv.URL, testToken)}
	req := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	v, err := a.page(t.Context(), req, pageState{Mode: "posts", ChannelID: "channel-1"})
	if err != nil {
		t.Fatalf("initial posts: %v", err)
	}
	if v.Sections[0].More == nil {
		t.Fatal("expected More on first page")
	}
	v2 := loadMore(t, a, v)
	if len(v2.Sections[0].Items) != 2 {
		t.Fatalf("duplicate post across pages was not deduplicated: got %d items", len(v2.Sections[0].Items))
	}
	if v2.Sections[0].More != nil {
		t.Fatalf("cursor cycle still offered More: %+v", v2.Sections[0].More)
	}

	// Going back from posts must clear Pages, TeamID, ChannelID, Before, and Page.
	backRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6, Action: "back", State: v2.State}})
	if err != nil {
		t.Fatalf("back action: %v", err)
	}
	var st pageState
	if err := (&ext.UIRequest{UIParams: wire.UIParams{State: backRes.View.State}}).DecodeState(&st); err != nil {
		t.Fatalf("decode back state: %v", err)
	}
	if len(st.Pages) != 0 || st.TeamID != "" || st.ChannelID != "" || st.Before != "" || st.Mode != "" {
		t.Fatalf("back left stale state fields: %+v", st)
	}

	// readState must reject invalid cursors inside Pages and invalid modes.
	for _, bad := range []pageState{
		{Mode: "posts", ChannelID: "channel-1", Pages: []string{"../bad"}},
		{Mode: "posts", ChannelID: "channel-1", Pages: []string{"post-1", "post-1"}},
		{Mode: "unknown"},
	} {
		if _, err := readState(&ext.UIRequest{UIParams: wire.UIParams{State: ext.EncodeState(bad)}}); err == nil {
			t.Errorf("readState accepted invalid state %+v", bad)
		}
	}
}

func TestConcurrentInitAndReadsNoRace(t *testing.T) {
	f := newFakeMattermost(t)
	a := f.app()
	in := &ext.Init{InitializeParams: wire.InitializeParams{
		Config:  json.RawMessage(fmt.Sprintf(`{"instance_url":%q}`, f.srv.URL)),
		Secrets: map[string]string{"api_token": testToken},
	}}

	var wg sync.WaitGroup
	ctx := context.Background()
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 15 {
				_, _ = a.init(ctx, in, nil)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 15 {
				_, _ = a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
				_, _ = a.callTool(ctx, "mattermost_unread", json.RawMessage(`{}`))
			}
		}()
	}
	wg.Wait()
}
