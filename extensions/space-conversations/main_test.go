package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestSpaceLifecycleUsesCoreRecords(t *testing.T) {
	created := false
	var saved map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("the extension's hub credential was not used")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/assistant/tracks":
			if r.Method == http.MethodPost {
				created = true
				_ = json.NewDecoder(r.Body).Decode(&saved)
				_, _ = w.Write([]byte(`{"slug":"reading","kind":"space","title":"阅读","revision":1,"config":{"sidebar":{"group":"spaces"}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"tracks":[{"slug":"reading","kind":"space","title":"阅读","revision":1,"config":{"sidebar":{"group":"spaces"}}}]}`))
		case "/api/v1/assistant":
			_, _ = w.Write([]byte(`{"machine_id":"computer-example","conversations":[{"session_id":"session-example","title":"第一本书","space":"reading"}]}`))
		case "/api/v1/assistant/tracks/reading/space-md":
			_, _ = w.Write([]byte(`{"content":"# 阅读\n","digest":"digest-example"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := &App{client: NewClient(srv.URL, "test-token")}
	home, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "home"}})
	if err != nil || len(home.Sections[0].Items) != 1 {
		t.Fatalf("home = %#v, %v", home, err)
	}
	createdResult, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Action: "create", Values: map[string]string{"title": "阅读", "slug": "reading", "description": "想读的书"}}})
	if err != nil || !created || createdResult.Push.State != "reading" || saved["kind"] != "space" {
		t.Fatalf("create = %#v, %v, body %#v", createdResult, err, saved)
	}
	page, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "space", State: "reading"}})
	if err != nil || len(page.Sections) != 3 {
		t.Fatalf("space = %#v, %v", page, err)
	}
	edit, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "edit", State: "reading"}})
	if err != nil || edit.Sections[0].Items[1].Value != "# 阅读\n" {
		t.Fatalf("edit = %#v, %v", edit, err)
	}
}

func TestConversationLinkAndSlugValidation(t *testing.T) {
	for _, slug := range []string{"reading", "family-2"} {
		if !validSlug(slug) {
			t.Errorf("%q should be valid", slug)
		}
	}
	for _, slug := range []string{"", "new", "health", "all", "2bad", "../secret", " spaces"} {
		if validSlug(slug) {
			t.Errorf("%q should be refused", slug)
		}
	}
	if got := conversationURL("computer-example", "reading", "session/example"); got != "persona://tracks/computer-example/reading/session%2Fexample" {
		t.Fatal(got)
	}
}

func TestArchiveAndUnarchiveSendExplicitRequestedState(t *testing.T) {
	tests := []struct {
		name         string
		initialState bool
		action       string
		wantArchived bool
	}{
		{name: "archive active track", initialState: false, action: "archive", wantArchived: true},
		{name: "archive already archived track stays archived", initialState: true, action: "archive", wantArchived: true},
		{name: "unarchive archived track", initialState: true, action: "unarchive", wantArchived: false},
		{name: "unarchive already active track stays active", initialState: false, action: "unarchive", wantArchived: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			archived := tc.initialState
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/api/v1/assistant/tracks" && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(map[string]any{
						"tracks": []map[string]any{{
							"slug":     "reading",
							"kind":     "space",
							"title":    "阅读",
							"archived": archived,
							"revision": 3,
							"config":   map[string]any{"sidebar": map[string]any{"group": "spaces"}},
						}},
					})
				case r.URL.Path == "/api/v1/assistant/tracks/reading" && r.Method == http.MethodPut:
					if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
						t.Fatalf("decode PUT body: %v", err)
					}
					if v, ok := gotBody["archived"].(bool); ok {
						archived = v
					}
					w.WriteHeader(http.StatusOK)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			a := &App{client: NewClient(srv.URL, "test-token")}
			res, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
				Action: tc.action,
				Values: map[string]string{"slug": "reading"},
			}})
			if err != nil {
				t.Fatalf("action(%q) error: %v", tc.action, err)
			}
			got, ok := gotBody["archived"].(bool)
			if !ok || got != tc.wantArchived {
				t.Fatalf("PUT archived = %#v, want %v (full body: %#v)", gotBody["archived"], tc.wantArchived, gotBody)
			}
			if res.View == nil {
				t.Fatalf("expected refreshed home view, got nil")
			}
			if err := wire.CheckView(res.View); err != nil {
				t.Fatalf("home view invalid: %v", err)
			}
		})
	}
}

func TestArchivePropagatesHomeRefreshError(t *testing.T) {
	getCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/assistant/tracks" && r.Method == http.MethodGet:
			getCount++
			if getCount == 1 {
				_, _ = w.Write([]byte(`{"tracks":[{"slug":"reading","kind":"space","title":"阅读","revision":1,"config":{"sidebar":{"group":"spaces"}}}]}`))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"database unavailable"}`))
		case r.URL.Path == "/api/v1/assistant/tracks/reading" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := &App{client: NewClient(srv.URL, "test-token")}
	res, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Action: "archive",
		Values: map[string]string{"slug": "reading"},
	}})
	if err == nil {
		t.Fatalf("expected homeView refresh error to be returned, got res=%#v, err=nil", res)
	}
}

func TestRenderedViewsPassWireValidationAndPreserveState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/assistant/tracks":
			tracks := []map[string]any{
				{
					"slug":        "no-desc",
					"kind":        "space",
					"title":       "无简介空间",
					"description": "",
					"revision":    1,
					"config":      map[string]any{"sidebar": map[string]any{"group": "spaces"}},
				},
				{
					"slug":        "reading",
					"kind":        "space",
					"title":       "",
					"description": "想读的书和笔记",
					"revision":    1,
					"config":      map[string]any{"sidebar": map[string]any{"group": "spaces"}},
				},
			}
			for i := 0; i < wire.ItemsMost+5; i++ {
				desc := ""
				if i == 0 {
					desc = strings.Repeat("长", wire.StringMost+50)
				}
				tracks = append(tracks, map[string]any{
					"slug":        fmt.Sprintf("extra-%d", i),
					"kind":        "space",
					"title":       fmt.Sprintf("空间 %d", i),
					"description": desc,
					"revision":    1,
					"config":      map[string]any{"sidebar": map[string]any{"group": "spaces"}},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tracks": tracks})
		case "/api/v1/assistant":
			convs := []map[string]any{
				{"session_id": "s-untitled", "title": "", "last_text": "第一条消息预览", "space": "no-desc"},
			}
			for i := 0; i < wire.ItemsMost+5; i++ {
				title := fmt.Sprintf("对话 %d", i)
				last := "短预览"
				if i == 0 {
					title = strings.Repeat("标", wire.StringMost+10)
					last = strings.Repeat("摘", wire.StringMost+10)
				}
				convs = append(convs, map[string]any{
					"session_id": fmt.Sprintf("s-%d", i),
					"title":      title,
					"last_text":  last,
					"space":      "no-desc",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"machine_id":    "computer-example",
				"conversations": convs,
			})
		case "/api/v1/assistant/tracks/no-desc/space-md":
			_, _ = w.Write([]byte(`{"content":"# 无简介空间\n","digest":"sha256-digest-123"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := &App{client: NewClient(srv.URL, "test-token")}

	t.Run("home", func(t *testing.T) {
		home, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "home"}})
		if err != nil {
			t.Fatalf("home render error: %v", err)
		}
		if err := wire.CheckView(home); err != nil {
			t.Fatalf("home view failed wire.CheckView: %v", err)
		}
		// Row for "reading" must use Subtitle (not Detail) for description and fallback title to slug.
		readingRow := home.Sections[0].Items[1]
		if readingRow.Title != "reading" {
			t.Fatalf("empty track title fallback = %q, want %q", readingRow.Title, "reading")
		}
		if readingRow.Subtitle != "想读的书和笔记" {
			t.Fatalf("home row Subtitle = %q (Detail=%q), want description in Subtitle", readingRow.Subtitle, readingRow.Detail)
		}
	})

	t.Run("space_empty_description_and_refresh_state", func(t *testing.T) {
		page, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "space", State: "no-desc"}})
		if err != nil {
			t.Fatalf("space render error: %v", err)
		}
		if err := wire.CheckView(page); err != nil {
			t.Fatalf("space view with empty description failed wire.CheckView: %v", err)
		}
		if page.State != "no-desc" {
			t.Fatalf("space view State = %q, want %q for RefreshAfter re-renders", page.State, "no-desc")
		}
		firstChatRow := page.Sections[1].Items[0]
		if firstChatRow.Subtitle != "第一条消息预览" {
			t.Fatalf("conversation row Subtitle = %q (Detail=%q), want last_text in Subtitle", firstChatRow.Subtitle, firstChatRow.Detail)
		}
		if firstChatRow.Title == "" {
			t.Fatalf("untitled conversation row must have non-empty Title")
		}
		refreshed, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "space", State: page.State}})
		if err != nil || refreshed.Title != "无简介空间" {
			t.Fatalf("re-render space with returned State failed: view=%#v err=%v", refreshed, err)
		}
	})

	t.Run("space_with_description", func(t *testing.T) {
		page, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "space", State: "reading"}})
		if err != nil {
			t.Fatalf("space render error: %v", err)
		}
		if err := wire.CheckView(page); err != nil {
			t.Fatalf("space view with description failed wire.CheckView: %v", err)
		}
		about := page.Sections[2]
		if about.Items[0].Type != wire.ComponentRow || about.Items[0].Subtitle == "" {
			t.Fatalf("expected folded SPACE.md summary row, got %#v", about.Items[0])
		}
	})

	t.Run("edit_preserves_slug_state", func(t *testing.T) {
		edit, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "edit", State: "no-desc"}})
		if err != nil {
			t.Fatalf("edit render error: %v", err)
		}
		if err := wire.CheckView(edit); err != nil {
			t.Fatalf("edit view failed wire.CheckView: %v", err)
		}
		if edit.State != "no-desc" {
			t.Fatalf("edit view State = %q, want %q", edit.State, "no-desc")
		}
		if _, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "edit", State: edit.State}}); err != nil {
			t.Fatalf("re-render edit with returned State failed: %v", err)
		}
	})

	for _, ui := range []int{1, 2, 3, 4, 6} {
		for _, state := range []string{"", "all", "no-desc", "reading", "gone"} {
			t.Run(fmt.Sprintf("ui%d_%s", ui, cmp.Or(state, "empty")), func(t *testing.T) {
				for _, contribution := range []string{"home", "space"} {
					v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: contribution, State: state, UIVersion: ui}})
					if err != nil {
						t.Fatalf("%s render: %v", contribution, err)
					}
					checkVersioned(t, v, ui)
				}
			})
		}
	}

	t.Run("new", func(t *testing.T) {
		newV, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "new"}})
		if err != nil {
			t.Fatalf("new render error: %v", err)
		}
		if err := wire.CheckView(newV); err != nil {
			t.Fatalf("new view failed wire.CheckView: %v", err)
		}
	})
}

func spacesHub(t *testing.T, calls *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/assistant/tracks" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"tracks":[{"slug":"reading","kind":"space","title":"阅读","revision":1,"config":{"sidebar":{"group":"spaces"}}}]}`))
		case r.URL.Path == "/api/v1/assistant/tracks/reading/conversations":
			_, _ = w.Write([]byte(`{"machine_id":"computer-example","total":1,"conversations":[{"session_id":"session-example","title":"第一本书","space":"reading"}]}`))
		case r.URL.Path == "/api/v1/assistant/conversations" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"session":{"session_id":"session-new","machine_id":"computer-example"}}`))
		case r.URL.Path == "/api/v1/assistant/tracks/reading/space-md":
			_, _ = w.Write([]byte(`{"content":"# 阅读\n","digest":"digest-example"}`))
		default:
			// An empty slug collapses /tracks//space-md to /tracks/space-md, which the hub refuses.
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"this extension was not granted this part of the hub's API"}`))
		}
	}))
}

// Every client lists each extension_page as an entry of its own, so space and edit open without a slug.
func TestPagesOpenedWithoutASlugOfferTheSpaces(t *testing.T) {
	var calls []string
	srv := spacesHub(t, &calls)
	defer srv.Close()
	a := &App{client: NewClient(srv.URL, "test-token")}
	for _, ui := range []string{"space", "edit"} {
		calls = nil
		v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: ui}})
		if err != nil {
			t.Fatalf("%s without a slug: %v", ui, err)
		}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("%s view invalid: %v", ui, err)
		}
		// space shows every space, each row choosing it in the switcher; edit lists the spaces to edit.
		want := map[string]string{"space": "pick", "edit": "edit"}[ui]
		key := map[string]string{"space": "space", "edit": "slug"}[ui]
		var row *wire.Component
		for i, it := range v.Sections[0].Items {
			if it.Type == wire.ComponentRow {
				row = &v.Sections[0].Items[i]
			}
		}
		if row == nil || row.Action.ID != want || row.Action.Values[key] != "reading" {
			t.Fatalf("%s should list the spaces with %q rows: %#v", ui, want, v)
		}
		for _, call := range calls {
			if strings.Contains(call, "/tracks/space-md") || strings.Contains(call, "//") {
				t.Fatalf("%s called the hub with an empty slug: %v", ui, calls)
			}
		}
	}
	// a row of the edit picker pushes the edit page for that space
	res, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "edit", Action: "edit", Values: map[string]string{"slug": "reading"}}})
	if err != nil || res.Push == nil || res.Push.UI != "edit" || res.Push.State != "reading" {
		t.Fatalf("edit pick = %#v, %v", res, err)
	}
}

// An action without its own values (an older app, or values lost on the way) still knows its space from the page state.
func TestActionsFallBackToThePageState(t *testing.T) {
	var calls []string
	srv := spacesHub(t, &calls)
	defer srv.Close()
	a := &App{client: NewClient(srv.URL, "test-token")}
	res, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "space", State: "reading", Action: "edit"}})
	if err != nil || res.Push == nil || res.Push.State != "reading" {
		t.Fatalf("edit from state = %#v, %v", res, err)
	}
}

// Validation problems are hints, not RPC errors: the hub turns an action error into a failed page (HTTP 502).
func TestValidationAnswersWithAToast(t *testing.T) {
	var calls []string
	srv := spacesHub(t, &calls)
	defer srv.Close()
	a := &App{client: NewClient(srv.URL, "test-token")}
	for _, r := range []wire.UIParams{
		{Contribution: "space", State: "reading", Action: "start", Values: map[string]string{"slug": "reading"}},
		{Contribution: "new", Action: "create", Values: map[string]string{"title": "", "slug": "reading"}},
		{Contribution: "new", Action: "create", Values: map[string]string{"title": "阅读", "slug": "new"}},
		{Contribution: "space", Action: "archive", Values: map[string]string{"slug": "missing"}},
		{Contribution: "edit", Action: "save", Values: map[string]string{"content": "x"}},
	} {
		res, err := a.action(context.Background(), &ext.UIRequest{UIParams: r})
		if err != nil || res == nil || res.Toast == "" {
			t.Fatalf("%s %v = %#v, %v; want a toast", r.Action, r.Values, res, err)
		}
	}
}

// Starting a conversation answers with the refreshed page and no persona: link, which older hubs refuse outright.
func TestStartAnswersWithTheRefreshedSpace(t *testing.T) {
	var calls []string
	srv := spacesHub(t, &calls)
	defer srv.Close()
	a := &App{client: NewClient(srv.URL, "test-token")}
	res, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "space", State: "reading", Action: "start", Values: map[string]string{"prompt": "今天读什么"}}})
	if err != nil || res.View == nil || res.OpenURL != "" || res.Toast == "" {
		t.Fatalf("start = %#v, %v", res, err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatal(err)
	}
}

// checkVersioned holds a view to the hub's validation and to what an app of UI version ui draws (0: the newest).
func checkVersioned(t *testing.T, v *wire.View, ui int) {
	t.Helper()
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("view failed wire.CheckView: %v", err)
	}
	if ui == 0 {
		return
	}
	if ui < 4 && v.Tabs != nil {
		t.Fatalf("UI version %d got tabs", ui)
	}
	if ui < 3 && v.Filter != nil {
		t.Fatalf("UI version %d got a filter", ui)
	}
	for _, s := range v.Sections {
		for _, it := range s.Items {
			if ui < 3 && it.Type == wire.ComponentMarkdown {
				t.Fatalf("UI version %d got markdown", ui)
			}
			if ui < 4 && it.Action != nil && it.Action.ID == "pick" {
				t.Fatalf("UI version %d got a pick that needs action values: %#v", ui, it)
			}
		}
	}
}

// fakeHub is a hub with spaces, their conversations and SPACE.md. A nonzero old is the status the scoped conversations
// route answers with, as a hub before it does: 403 without a scope from the hub, 404 from an older assistant manager.
type fakeHub struct {
	tracks    []map[string]any
	chats     map[string][]map[string]any
	totals    map[string]int
	md        map[string]string
	mdFail    map[string]bool
	launchers *TrackLaunchers
	old       int

	mu        sync.Mutex
	calls     []string
	startBody map[string]any
}

func spaceTrack(slug, title string) map[string]any {
	return map[string]any{"slug": slug, "kind": "space", "title": title, "revision": 1, "config": map[string]any{"sidebar": map[string]any{"group": "spaces"}}}
}

func chat(sid, title string, at time.Time) map[string]any {
	return map[string]any{"session_id": sid, "title": title, "last_text": title + " 的最后一句", "updated_at": at.Format(time.RFC3339)}
}

func (f *fakeHub) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeHub) start(t *testing.T) *App {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		rest, isTrack := strings.CutPrefix(r.URL.Path, "/api/v1/assistant/tracks/")
		switch {
		case r.URL.Path == "/api/v1/assistant/tracks" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"tracks": f.tracks})
		case r.URL.Path == "/api/v1/assistant/tracks/launchers" && r.Method == http.MethodGet:
			if f.old != 0 {
				w.WriteHeader(f.old)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "permission_denied"})
				return
			}
			if f.launchers != nil {
				_ = json.NewEncoder(w).Encode(f.launchers)
				return
			}
			_ = json.NewEncoder(w).Encode(TrackLaunchers{Launchers: []TrackLauncher{}})
		case isTrack && strings.HasSuffix(rest, "/conversations"):
			if f.old != 0 {
				w.WriteHeader(f.old)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "permission_denied"})
				return
			}
			slug := strings.TrimSuffix(rest, "/conversations")
			limit := 20
			if q := r.URL.Query().Get("limit"); q != "" {
				n, err := strconv.Atoi(q)
				if err != nil || n < 1 || n > 50 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				limit = n
			}
			list := f.chats[slug]
			total := len(list)
			if n, ok := f.totals[slug]; ok {
				total = n
			}
			if len(list) > limit {
				list = list[:limit]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"machine_id": "computer-example", "total": total, "conversations": list})
		case isTrack && strings.HasSuffix(rest, "/space-md"):
			slug := strings.TrimSuffix(rest, "/space-md")
			if f.mdFail[slug] {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":"the assistant's computer is offline"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"content": f.md[slug], "digest": "digest-example"})
		case r.URL.Path == "/api/v1/assistant" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"machine_id": "computer-example", "conversations": f.chats[r.URL.Query().Get("space")]})
		case r.URL.Path == "/api/v1/assistant/conversations" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.startBody = body
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"session":{"session_id":"session-new","machine_id":"computer-example"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &App{client: NewClient(srv.URL, "test-token")}
}

var testNow = time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)

func fixClock(t *testing.T) {
	t.Helper()
	prev := clock
	clock = func() time.Time { return testNow }
	t.Cleanup(func() { clock = prev })
}

func render(t *testing.T, a *App, contribution, state string) *wire.View {
	t.Helper()
	v, err := a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: contribution, State: state, TZ: "UTC"}})
	if err != nil {
		t.Fatalf("render %s %q: %v", contribution, state, err)
	}
	checkVersioned(t, v, 0)
	return v
}

func spacesNamed(n int) []map[string]any {
	var out []map[string]any
	for i := range n {
		out = append(out, spaceTrack(fmt.Sprintf("space-%d", i), fmt.Sprintf("空间 %d", i)))
	}
	return out
}

// Tabs hold eight items (All + spaces + "+ 新建空间"): six spaces are tabs, seven a drop-down.
func TestSwitcherIsTabsUpToEightItemsThenAFilter(t *testing.T) {
	for _, tc := range []struct {
		spaces int
		tabs   bool
	}{{6, true}, {7, false}} {
		a := (&fakeHub{tracks: spacesNamed(tc.spaces)}).start(t)
		for _, state := range []string{"", "space-3"} {
			v := render(t, a, "home", state)
			want := cmp.Or(state, allValue)
			switch {
			case tc.tabs && (v.Tabs == nil || v.Filter != nil || len(v.Tabs.Items) != 8 || v.Tabs.Value != want || v.Tabs.Name != "space" || v.Tabs.Action.ID != "pick" || v.Tabs.Items[len(v.Tabs.Items)-1].Value != newValue):
				t.Fatalf("%d spaces, %q: tabs = %#v, filter = %#v", tc.spaces, state, v.Tabs, v.Filter)
			case !tc.tabs && (v.Filter == nil || v.Tabs != nil || len(v.Filter.Choices) != 9 || v.Filter.Value != want || v.Filter.Action.ID != "pick" || v.Filter.Choices[len(v.Filter.Choices)-1].Value != newValue):
				t.Fatalf("%d spaces, %q: tabs = %#v, filter = %#v", tc.spaces, state, v.Tabs, v.Filter)
			}
		}
	}
}

// Choosing "+ 新建空间" in the switcher pushes the new-space form directly (both modern and pre-UI-4 actions).
func TestPickNewSpacePushesNewForm(t *testing.T) {
	a := (&fakeHub{tracks: spacesNamed(2)}).start(t)
	res, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "home", State: allValue, Action: "pick", Values: map[string]string{"space": newValue}}})
	if err != nil || res.Push == nil || res.Push.UI != "new" {
		t.Fatalf("pick new = %#v, %v", res, err)
	}
	oldRes, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "home", Action: pickPrefix + newValue, UIVersion: 2}})
	if err != nil || oldRes.Push == nil || oldRes.Push.UI != "new" {
		t.Fatalf("old pick new = %#v, %v", oldRes, err)
	}
}

// Choosing a space draws the page in place with the choice as its state, so a refresh keeps it.
func TestPickKeepsTheSelectionInState(t *testing.T) {
	fixClock(t)
	a := (&fakeHub{tracks: spacesNamed(3), chats: map[string][]map[string]any{
		"space-1": {chat("s-1", "读书", testNow.Add(-5*time.Minute))},
	}}).start(t)
	for _, contribution := range []string{"home", "space"} {
		res, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: contribution, State: "all", Action: "pick", Values: map[string]string{"space": "space-1"}}})
		if err != nil || res.View == nil || res.Push != nil {
			t.Fatalf("pick = %#v, %v", res, err)
		}
		checkVersioned(t, res.View, 0)
		if res.View.State != "space-1" || res.View.Tabs.Value != "space-1" || res.View.Title != "空间 1" {
			t.Fatalf("picked view state %q tabs %q title %q", res.View.State, res.View.Tabs.Value, res.View.Title)
		}
		again := render(t, a, contribution, res.View.State)
		if again.Tabs.Value != "space-1" || again.Sections[0].Title != "新对话" {
			t.Fatalf("refresh lost the selection: %#v", again)
		}
		back, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: contribution, State: "space-1", Action: "pick", Values: map[string]string{"space": "all"}}})
		if err != nil || back.View == nil || back.View.State != allValue || back.View.Tabs.Value != allValue {
			t.Fatalf("pick all = %#v, %v", back, err)
		}
	}
	// apps before UI version 4 send no action values: their switcher rows name the space in the id
	res, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: "home", Action: pickPrefix + "space-2", UIVersion: 2}})
	if err != nil || res.View == nil || res.View.State != "space-2" {
		t.Fatalf("old pick = %#v, %v", res, err)
	}
	checkVersioned(t, res.View, 2)
}

// The page of every space lists them with their counts and merges their conversations, newest first, each labeled.
func TestAllMergesConversationsLabeledBySpace(t *testing.T) {
	fixClock(t)
	f := &fakeHub{
		tracks: []map[string]any{spaceTrack("reading", "阅读"), spaceTrack("travel", "旅行"), spaceTrack("quiet", "安静")},
		chats: map[string][]map[string]any{
			"reading": {chat("r-new", "新书", testNow.Add(-2*time.Minute)), chat("r-old", "旧书", testNow.Add(-50*time.Hour))},
			"travel":  {chat("t-mid", "行程", testNow.Add(-3*time.Hour))},
		},
		totals: map[string]int{"reading": 150, "travel": 5},
		md: map[string]string{
			"reading": "# 阅读\n\n<!-- persona:managed:state:begin -->\n托管内容\n<!-- persona:managed:state:end -->\n每月读两本书。\n",
		},
	}
	a := f.start(t)
	v := render(t, a, "home", "")
	if v.State != allValue || v.Title != "空间对话" {
		t.Fatalf("all view state %q title %q", v.State, v.Title)
	}
	badges := map[string]string{}
	for _, tab := range v.Tabs.Items {
		badges[tab.Value] = tab.Badge
	}
	if badges["all"] != "" || badges["reading"] != "99+" || badges["travel"] != "5" || badges["quiet"] != "" {
		t.Fatalf("tab badges = %v", badges)
	}
	if last := v.Tabs.Items[len(v.Tabs.Items)-1]; last.Value != newValue || last.Title != "+ 新建空间" {
		t.Fatalf("last tab should be + 新建空间, got %#v", last)
	}
	spaces := v.Sections[0]
	reading := spaces.Items[0]
	if reading.Title != "阅读" || reading.Subtitle != "每月读两本书。" || reading.Value != "150 个对话" || reading.Action.ID != "pick" || reading.Action.Values["space"] != "reading" {
		t.Fatalf("reading row = %#v", reading)
	}
	if spaces.Items[2].Value != "0 个对话" {
		t.Fatalf("quiet row = %#v", spaces.Items[2])
	}
	recent := v.Sections[1]
	if recent.Title != "最近对话 · 全部空间" || len(recent.Items) != 3 {
		t.Fatalf("recent = %#v", recent)
	}
	want := []struct{ sid, space, slug, when string }{
		{"r-new", "阅读", "reading", "2 分钟前"}, {"t-mid", "旅行", "travel", "3 小时前"}, {"r-old", "阅读", "reading", "2 天前"},
	}
	for i, w := range want {
		row := recent.Items[i]
		if row.Action.Values["sid"] != w.sid || row.Badge == nil || row.Badge.Text != w.space || row.Action.Values["slug"] != w.slug || row.Value != w.when || row.Action.Values["mid"] != "computer-example" {
			t.Fatalf("recent row %d = %#v, want %v", i, row, w)
		}
	}
	for _, call := range f.called() {
		if strings.Contains(call, "/conversations?") && !strings.Contains(call, "limit=20") {
			t.Fatalf("conversations asked without the limit: %s", call)
		}
	}
}

// A space's page leads with compact quick-start and recent conversations, with SPACE.md folded to a single-line summary below.
func TestSelectedSpaceShowsOverviewAndExcerpt(t *testing.T) {
	fixClock(t)
	f := &fakeHub{
		tracks: []map[string]any{spaceTrack("reading", "阅读")},
		chats:  map[string][]map[string]any{"reading": {chat("r-1", "新书", testNow.Add(-26*time.Hour))}},
		md: map[string]string{"reading": "# 阅读\n<!-- persona:managed:facts:begin -->\n- 不该出现\n<!-- persona:managed:facts:end -->\n" +
			"<!-- 作者的备注 -->\n## 目标\n\n- 每月两本\n- 写笔记\n"},
	}
	v := render(t, f.start(t), "space", "reading")
	if v.Sections[0].Title != "新对话" || v.Sections[0].Items[0].Kind != wire.FieldKindText {
		t.Fatalf("quick start section = %#v", v.Sections[0])
	}
	recent := v.Sections[1]
	if recent.Title != "最近对话" || recent.Footer != "1 个对话 · 最近 昨天" || recent.Items[0].Badge.Text != "阅读" || recent.Items[0].Value != "昨天" {
		t.Fatalf("recent section = %#v", recent)
	}
	about := v.Sections[2]
	if about.Title != "空间说明" || len(about.Items) != 2 {
		t.Fatalf("about section = %#v", about)
	}
	mdRow := about.Items[0]
	if mdRow.Type != wire.ComponentRow || mdRow.Subtitle != "目标" || mdRow.Value != "查看 / 编辑" || mdRow.Action.ID != "edit" {
		t.Fatalf("folded SPACE.md row = %#v", mdRow)
	}
	if about.Items[1].Action.ID != "archive" {
		t.Fatalf("archive button = %#v", about.Items[1])
	}
}

func TestSpaceExcerpt(t *testing.T) {
	long := strings.Repeat("长", excerptRunes+50)
	for _, tc := range []struct{ in, title, want string }{
		{"# 阅读\n正文", "阅读", "正文"},
		{"# 别的\n正文", "阅读", "# 别的\n正文"},
		{"<!-- persona:managed:a:begin -->\nx\n<!-- persona:managed:a:end -->", "阅读", ""},
		{"前\n<!-- 注释\n跨行 -->\n后", "", "前\n\n后"},
		{"前\n<!-- 没有结束", "", "前"},
		{"1\n2\n3\n4\n5\n6\n7\n8\n9\n10", "", "1\n2\n3\n4\n5\n6\n7\n8"},
		{long, "", strings.Repeat("长", excerptRunes)},
	} {
		if got := spaceExcerpt(tc.in, tc.title); got != tc.want {
			t.Errorf("spaceExcerpt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// SPACE.md lives on the assistant's computer: when it cannot be read the page still draws, saying so.
func TestSpaceMDErrorDoesNotFailThePage(t *testing.T) {
	f := &fakeHub{tracks: []map[string]any{spaceTrack("reading", "阅读")}, mdFail: map[string]bool{"reading": true}}
	a := f.start(t)
	v := render(t, a, "space", "reading")
	if v.Sections[2].Items[0].Subtitle != "SPACE.md 暂时读不到（助手所在的电脑可能离线）" {
		t.Fatalf("no notice for an unreadable SPACE.md: %#v", v.Sections[2])
	}
	// the page of every space falls back to the description
	render(t, a, "home", "")
	empty := render(t, (&fakeHub{tracks: []map[string]any{spaceTrack("reading", "阅读")}, md: map[string]string{"reading": "# 阅读\n"}}).start(t), "space", "reading")
	if empty.Sections[2].Items[0].Subtitle != "SPACE.md 还没有内容" {
		t.Fatalf("empty SPACE.md = %#v", empty.Sections[2].Items)
	}
}

// A hub without the scoped route refuses it (403 without a scope, or 404 from an older assistant manager): the
// extension asks the older assistant listing instead.
func TestConversationsFallBackToTheOldRoute(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) { testFallBack(t, status) })
	}
}

func testFallBack(t *testing.T, status int) {
	fixClock(t)
	f := &fakeHub{old: status, tracks: []map[string]any{spaceTrack("reading", "阅读")}, chats: map[string][]map[string]any{
		"reading": {chat("r-old", "旧", testNow.Add(-3*time.Hour)), chat("r-new", "新", testNow.Add(-time.Minute))},
	}}
	a := f.start(t)
	v := render(t, a, "space", "reading")
	rows := v.Sections[1].Items
	if len(rows) != 2 || rows[0].Action.Values["sid"] != "r-new" || v.Tabs.Items[1].Badge != "2" {
		t.Fatalf("fallback rows = %#v, tabs %#v", rows, v.Tabs)
	}
	var scoped, old bool
	for _, call := range f.called() {
		scoped = scoped || strings.HasPrefix(call, "GET /api/v1/assistant/tracks/reading/conversations")
		old = old || call == "GET /api/v1/assistant?space=reading"
	}
	if !scoped || !old {
		t.Fatalf("calls = %v", f.called())
	}
	machine, total, list, err := a.client.SpaceConversations(context.Background(), "reading", 1)
	if err != nil || machine != "computer-example" || total != 2 || len(list) != 1 || list[0].SessionID != "r-new" {
		t.Fatalf("SpaceConversations = %q %d %#v %v", machine, total, list, err)
	}
}

// Only a refusal that names no scope, or a 404 or 405, means the route is missing; a refused scope is a real error.
func TestMissingRoute(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&HTTPError{Status: http.StatusForbidden}, true},
		{&HTTPError{Status: http.StatusNotFound}, true},
		{&HTTPError{Status: http.StatusMethodNotAllowed}, true},
		{&HTTPError{Status: http.StatusForbidden, Scope: "spaces"}, false},
		{&HTTPError{Status: http.StatusInternalServerError}, false},
		{fmt.Errorf("network: %w", context.DeadlineExceeded), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := missingRoute(c.err); got != c.want {
			t.Errorf("missingRoute(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// An archived space chosen in the page stays in the switcher (its value must be one of the items) and can be restored.
func TestArchivedSelectedSpaceStaysInTheSwitcher(t *testing.T) {
	archived := spaceTrack("old-trip", "旧旅行")
	archived["archived"] = true
	f := &fakeHub{tracks: []map[string]any{spaceTrack("reading", "阅读"), archived}}
	a := f.start(t)
	v := render(t, a, "space", "old-trip")
	if v.Tabs == nil || v.Tabs.Value != "old-trip" || len(v.Tabs.Items) != 4 {
		t.Fatalf("tabs = %#v", v.Tabs)
	}
	for _, s := range v.Sections {
		if s.Title == "新对话" {
			t.Fatal("an archived space offers a new conversation")
		}
	}
	if b := v.Sections[len(v.Sections)-1].Items; b[len(b)-1].Action.ID != "unarchive" {
		t.Fatalf("about = %#v", b)
	}
	all := render(t, a, "home", "")
	if len(all.Tabs.Items) != 3 || len(all.Sections[0].Items) != 1 {
		t.Fatalf("archived space listed on the page of every space: %#v", all)
	}
}

// A space that is gone, or a track that is not a space, draws every space with a note.
func TestUnknownSpaceShowsAll(t *testing.T) {
	builtin := spaceTrack("health", "健康")
	builtin["builtin"] = true
	a := (&fakeHub{tracks: []map[string]any{spaceTrack("reading", "阅读"), builtin}}).start(t)
	for _, state := range []string{"gone", "health"} {
		v := render(t, a, "space", state)
		if v.State != allValue || v.Tabs.Value != allValue || v.Sections[0].Items[0].Text != "这个空间已不存在，下面是全部空间。" {
			t.Fatalf("%s: %#v", state, v)
		}
	}
	none := render(t, (&fakeHub{}).start(t), "home", "gone")
	if none.Tabs != nil || none.Filter != nil || none.Sections[0].Items[len(none.Sections[0].Items)-1].Type != wire.ComponentEmpty {
		t.Fatalf("no spaces = %#v", none)
	}
}

// Starting a conversation answers with that space chosen on the page it started from.
func TestStartShowsTheSpaceOnBothPages(t *testing.T) {
	a := (&fakeHub{tracks: spacesNamed(2)}).start(t)
	for _, contribution := range []string{"home", "space"} {
		res, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Contribution: contribution, State: "all", Action: "start", Values: map[string]string{"slug": "space-1", "prompt": "你好"}}})
		if err != nil || res.View == nil || res.View.State != "space-1" || res.Toast != "对话已开始" {
			t.Fatalf("%s start = %#v, %v", contribution, res, err)
		}
	}
}

// When the hub lists launchers, the selected space offers launcher, model and effort fields and forwards them on start;
// older hubs that refuse /tracks/launchers degrade cleanly without failing the page.
func TestStartWithLauncherModelAndEffort(t *testing.T) {
	f := &fakeHub{
		tracks: []map[string]any{spaceTrack("reading", "阅读")},
		launchers: &TrackLaunchers{
			Default: TrackLauncherDefault{Launcher: "claude", Model: "claude-sonnet-4-6"},
			Launchers: []TrackLauncher{
				{Name: "claude", DisplayName: "Claude Code", DefaultModel: "claude-sonnet-4-6"},
				{Name: "codex", DisplayName: "Codex", DefaultModel: "gpt-5.4"},
			},
		},
	}
	a := f.start(t)
	v := render(t, a, "space", "reading")
	startItems := v.Sections[0].Items
	if len(startItems) != 5 || startItems[1].Name != "launcher" || startItems[2].Name != "model" || startItems[3].Name != "effort" {
		t.Fatalf("expected launcher/model/effort fields, got %#v", startItems)
	}
	if len(startItems[1].Choices) != 3 || startItems[1].Choices[0].Value != "" || startItems[1].Choices[2].Value != "codex" {
		t.Fatalf("launcher choices = %#v", startItems[1].Choices)
	}
	res, err := a.action(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{
		Contribution: "space",
		State:        "reading",
		Action:       "start",
		Values: map[string]string{
			"slug":     "reading",
			"prompt":   "推荐两本书",
			"launcher": "codex",
			"model":    "gpt-5.4",
			"effort":   "high",
		},
	}})
	if err != nil || res.View == nil || res.Toast != "对话已开始" {
		t.Fatalf("start with model = %#v, %v", res, err)
	}
	if f.startBody["launcher"] != "codex" || f.startBody["model"] != "gpt-5.4" || f.startBody["effort"] != "high" || f.startBody["space"] != "reading" {
		t.Fatalf("forwarded start body = %#v", f.startBody)
	}

	// Older hub without /tracks/launchers still renders cleanly with prompt + start button.
	oldApp := (&fakeHub{tracks: []map[string]any{spaceTrack("reading", "阅读")}, old: http.StatusForbidden}).start(t)
	oldView := render(t, oldApp, "space", "reading")
	if len(oldView.Sections[0].Items) != 2 {
		t.Fatalf("old hub start items = %#v", oldView.Sections[0].Items)
	}
}

func TestRelativeTime(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, loc)
	for _, tc := range []struct {
		ago  time.Duration
		want string
	}{
		{0, "刚刚"}, {-time.Minute, "刚刚"}, {5 * time.Minute, "5 分钟前"}, {3 * time.Hour, "3 小时前"},
		{11 * time.Hour, "昨天"}, {30 * time.Hour, "昨天"}, {3 * 24 * time.Hour, "3 天前"}, {10 * 24 * time.Hour, "11-10"},
	} {
		if got := relativeTime(now.Add(-tc.ago), now, loc); got != tc.want {
			t.Errorf("%v ago = %q, want %q", tc.ago, got, tc.want)
		}
	}
	if relativeTime(time.Time{}, now, loc) != "" {
		t.Error("no time should write nothing")
	}
	for n, want := range map[int]string{0: "", 1: "1", 99: "99", 100: "99+"} {
		if got := countBadge(n); got != want {
			t.Errorf("countBadge(%d) = %q, want %q", n, got, want)
		}
	}
}
