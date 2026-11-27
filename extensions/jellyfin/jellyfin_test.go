package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"
)

const testToken = "synthetic-jellyfin-token"
const userID = "123e4567e89b42d3a456426614174000"
const itemID = "223e4567e89b42d3a456426614174000"

type fakeJellyfin struct {
	srv       *httptest.Server
	mu        sync.Mutex
	paths     []string
	favorites []string
	failWrite bool
}

func sampleItem() mediaItem {
	item := mediaItem{ID: itemID, Name: "The reading session", Type: "Episode", Overview: "Keep source context beside the next step.", ProductionYear: 2026, RunTimeTicks: 18000000000, SeriesName: "Reading Journal", SeasonName: "Season 1", IndexNumber: 3, ParentIndexNumber: 1, MediaType: "Video"}
	item.UserData.PlaybackPositionTicks = 6000000000
	item.UserData.PlayedPercentage = 33.3
	return item
}

func newFakeJellyfin(t *testing.T) *fakeJellyfin {
	f := &fakeJellyfin{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != `MediaBrowser Token="`+testToken+`"` || r.Header.Get("X-Emby-Token") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.RequestURI())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/Users/Me":
			_ = json.NewEncoder(w).Encode(user{ID: userID, Name: "Reader"})
		case "/UserItems/Resume", "/Items":
			if r.URL.Query().Get("userId") != userID || r.URL.Query().Get("limit") != "30" || r.URL.Query().Get("enableUserData") != "true" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.URL.Path == "/Items" && r.URL.Query().Get("searchTerm") == "" && r.URL.Query().Get("filters") != "IsFavorite" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(itemPage{Items: []mediaItem{sampleItem()}, TotalRecordCount: 31, StartIndex: 0})
		case "/Users/" + userID + "/Items/" + itemID:
			_ = json.NewEncoder(w).Encode(sampleItem())
		case "/UserFavoriteItems/" + itemID:
			if r.URL.Query().Get("userId") != userID || f.failWrite {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if r.Method != http.MethodPost && r.Method != http.MethodDelete {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			f.mu.Lock()
			f.favorites = append(f.favorites, r.Method)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(sampleItem().UserData)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeJellyfin) app() *app {
	return &app{client: newClient(f.srv.URL, testToken), receipts: newWriteStore("")}
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeJellyfin(t).app())))
}

func TestManifestAndURL(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "jellyfin" || len(m.Secrets) != 1 || len(m.Contributes.Connectors[0].ConfirmAlways) != 1 || !slices.Equal(m.Permissions.Network, []string{}) {
		t.Fatalf("%+v", m)
	}
	for _, raw := range []string{"https://jellyfin.example.com", "http://127.0.0.1:8096/", "https://[::1]:8096"} {
		if _, err := instanceURL(raw); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"http://jellyfin.example.com", "https://me@example.com", "not-a-url"} {
		if _, err := instanceURL(raw); err == nil {
			t.Fatal("accepted " + raw)
		}
	}
}

func TestResumeSearchDetailsAndPagination(t *testing.T) {
	f := newFakeJellyfin(t)
	a := f.app()
	for _, view := range []string{"resume", "favorites", "search"} {
		text, err := a.callTool(t.Context(), "jellyfin_items", json.RawMessage(`{"view":"`+view+`","query":"reading"}`))
		if err != nil || !strings.Contains(text, itemID) || !strings.Contains(text, "Next start index: 1") {
			t.Fatalf("%s %v %s", view, err, text)
		}
	}
	text, err := a.callTool(t.Context(), "jellyfin_item", json.RawMessage(`{"item_id":"`+itemID+`"}`))
	if err != nil || !strings.Contains(text, "Reading Journal") || !strings.Contains(text, "source context") {
		t.Fatalf("%v %s", err, text)
	}
	f.mu.Lock()
	paths := strings.Join(f.paths, "\n")
	f.mu.Unlock()
	for _, want := range []string{"userId=" + userID, "limit=30", "searchTerm=reading", "filters=IsFavorite", "enableUserData=true"} {
		if !strings.Contains(paths, want) {
			t.Fatalf("missing %q in %s", want, paths)
		}
	}
}

func TestFavoriteReplayAndPayload(t *testing.T) {
	f := newFakeJellyfin(t)
	a := f.app()
	args := []byte(`{"item_id":"` + itemID + `","favorite":true}`)
	for range 2 {
		if _, err := a.doAction(t.Context(), "favorite-1", "jellyfin_set_favorite", args); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.favorites) != 1 || f.favorites[0] != http.MethodPost {
		t.Fatal("favorite replay repeated or used the wrong method")
	}
	if _, err := describeAction("jellyfin_set_favorite", args); err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "favorite-1", "jellyfin_set_favorite", []byte(`{"item_id":"`+itemID+`","favorite":false}`)); err == nil {
		t.Fatal("request ID accepted a different favorite state")
	}
}

func TestPersistentReplayAndUnknownOutcome(t *testing.T) {
	f := newFakeJellyfin(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	args := []byte(`{"item_id":"` + itemID + `","favorite":true}`)
	if _, err := a.doAction(t.Context(), "persist-1", "jellyfin_set_favorite", args); err != nil {
		t.Fatal(err)
	}
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "persist-1", "jellyfin_set_favorite", args); err != nil {
		t.Fatal(err)
	}
	if len(f.favorites) != 1 {
		t.Fatal("restart repeated favorite update")
	}
	raw, _ := json.Marshal(map[string]writeReceipt{"persist-1": {Key: "jellyfin_set_favorite:" + string(args), Status: "pending", StartedAt: time.Now()}})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.receipts, _ = openWriteStore(filepath.Dir(store.path))
	if _, err := a.doAction(t.Context(), "persist-1", "jellyfin_set_favorite", args); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatal("unknown favorite update was retried")
	}
}

func TestLocalizedViewsAndBounds(t *testing.T) {
	f := newFakeJellyfin(t)
	a := f.app()
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, ui := range []int{1, 3, 6} {
			for _, mode := range []string{"", "detail"} {
				r := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: ui}}
				v, err := a.page(t.Context(), r, pageState{Mode: mode, ItemID: itemID, DraftID: "draft-1"})
				if err != nil {
					t.Fatalf("%s %d %s: %v", locale, ui, mode, err)
				}
				if err := wire.CheckView(v); err != nil {
					t.Fatalf("%s %d %s: %v", locale, ui, mode, err)
				}
			}
		}
	}
	item := sampleItem()
	item.Overview = strings.Repeat("media context\n", 10000)
	v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{ItemID: itemID}, item)
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	if newApp().status()["state"] != "not_configured" {
		t.Fatal("unconfigured extension was ready")
	}
}

func TestFailedFavoriteIsUnknown(t *testing.T) {
	f := newFakeJellyfin(t)
	f.failWrite = true
	if _, err := f.app().doAction(t.Context(), "failed-1", "jellyfin_set_favorite", []byte(`{"item_id":"`+itemID+`","favorite":true}`)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatal("failed favorite update was safe to retry")
	}
}
