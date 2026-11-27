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

const testToken = "synthetic-immich-key"
const assetID = "123e4567-e89b-42d3-a456-426614174000"
const albumID = "223e4567-e89b-42d3-a456-426614174000"

type fakeImmich struct {
	srv       *httptest.Server
	mu        sync.Mutex
	searches  []map[string]any
	favorites []map[string]any
	failWrite bool
}

func sampleAsset() asset {
	item := asset{ID: assetID, OriginalFileName: "reading-trip.jpg", Type: "IMAGE", FileCreatedAt: "2026-11-10T10:00:00Z", Visibility: "timeline"}
	item.Exif.City, item.Exif.Country, item.Exif.Make, item.Exif.Model, item.Exif.Description = "Example City", "Example Country", "Example Camera", "Model 1", "A reading trip with source notes."
	item.People = []struct {
		Name string `json:"name"`
	}{{Name: "Reader"}}
	return item
}

func newFakeImmich(t *testing.T) *fakeImmich {
	f := &fakeImmich{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != testToken || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/search/metadata", "/api/search/smart":
			var in map[string]any
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.searches = append(f.searches, map[string]any{"path": r.URL.Path, "body": in})
			f.mu.Unlock()
			if r.URL.Path == "/api/search/smart" && in["query"] == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.URL.Path == "/api/search/metadata" {
				if in["page"] != nil || in["query"] != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				filter, ok := in["filter"].(map[string]any)
				if !ok {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if _, ok := filter["isFavorite"]; !ok {
					if _, ok := filter["originalFileName"]; !ok {
						if _, ok := filter["albumIds"]; !ok {
							w.WriteHeader(http.StatusBadRequest)
							return
						}
					}
				}
			}
			cursor := "synthetic-next-cursor"
			var out searchResponse
			out.Assets.Items = []asset{sampleAsset()}
			out.Assets.Count = 1
			out.Assets.NextCursor = &cursor
			_ = json.NewEncoder(w).Encode(out)
		case "/api/assets/" + assetID:
			if r.Method == http.MethodPut {
				if f.failWrite {
					http.Error(w, "synthetic interrupted update", http.StatusServiceUnavailable)
					return
				}
				var in map[string]any
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in) != 1 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if _, ok := in["isFavorite"].(bool); !ok {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				f.mu.Lock()
				f.favorites = append(f.favorites, in)
				f.mu.Unlock()
			}
			_ = json.NewEncoder(w).Encode(sampleAsset())
		case "/api/albums":
			_ = json.NewEncoder(w).Encode([]album{{ID: albumID, AlbumName: "Reading trips", AssetCount: 12}})
		case "/api/albums/" + albumID:
			_ = json.NewEncoder(w).Encode(album{ID: albumID, AlbumName: "Reading trips", AssetCount: 1})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeImmich) app() *app {
	return &app{client: newClient(f.srv.URL, testToken), receipts: newWriteStore("")}
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeImmich(t).app())))
}

func TestManifestAndURL(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "immich" || len(m.Secrets) != 1 || len(m.Contributes.Connectors[0].ConfirmAlways) != 1 || !slices.Equal(m.Permissions.Network, []string{}) {
		t.Fatalf("%+v", m)
	}
	for _, raw := range []string{"https://immich.example.com", "http://127.0.0.1:2283/", "https://[::1]:2283"} {
		if _, err := instanceURL(raw); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"http://immich.example.com", "https://me@example.com", "not-a-url"} {
		if _, err := instanceURL(raw); err == nil {
			t.Fatal("accepted " + raw)
		}
	}
}

func TestSearchSmartSearchAssetAndAlbums(t *testing.T) {
	f := newFakeImmich(t)
	a := f.app()
	text, err := a.callTool(t.Context(), "immich_search", json.RawMessage(`{"query":"reading","cursor":"synthetic-cursor"}`))
	if err != nil || !strings.Contains(text, assetID) || !strings.Contains(text, "Next cursor") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "immich_search", json.RawMessage(`{"favorites_only":true}`))
	if err != nil || !strings.Contains(text, "reading-trip.jpg") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "immich_smart_search", json.RawMessage(`{"query":"reading by a window"}`))
	if err != nil || !strings.Contains(text, "Example Camera") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "immich_asset", json.RawMessage(`{"asset_id":"`+assetID+`"}`))
	if err != nil || !strings.Contains(text, "Reader") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "immich_album", json.RawMessage(`{"album_id":"`+albumID+`"}`))
	if err != nil || !strings.Contains(text, "Reading trips") || !strings.Contains(text, "reading-trip.jpg") {
		t.Fatalf("%v %s", err, text)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, search := range f.searches {
		body := search["body"].(map[string]any)
		if search["path"] == "/api/search/metadata" && body["cursor"] != nil && body["cursor"] != "synthetic-cursor" {
			t.Fatal("wrong cursor")
		}
	}
}

func TestFavoriteReplayAndPayload(t *testing.T) {
	f := newFakeImmich(t)
	a := f.app()
	args := []byte(`{"asset_id":"` + assetID + `","favorite":true}`)
	for range 2 {
		if _, err := a.doAction(t.Context(), "favorite-1", "immich_set_favorite", args); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.favorites) != 1 || f.favorites[0]["isFavorite"] != true {
		t.Fatal("favorite replay repeated or changed extra fields")
	}
	if _, err := describeAction("immich_set_favorite", args); err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "favorite-1", "immich_set_favorite", []byte(`{"asset_id":"`+assetID+`","favorite":false}`)); err == nil {
		t.Fatal("request ID accepted a different favorite state")
	}
}

func TestPersistentReplayAndUnknownOutcome(t *testing.T) {
	f := newFakeImmich(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	args := []byte(`{"asset_id":"` + assetID + `","favorite":true}`)
	if _, err := a.doAction(t.Context(), "persist-1", "immich_set_favorite", args); err != nil {
		t.Fatal(err)
	}
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "persist-1", "immich_set_favorite", args); err != nil {
		t.Fatal(err)
	}
	if len(f.favorites) != 1 {
		t.Fatal("restart repeated favorite update")
	}
	raw, _ := json.Marshal(map[string]writeReceipt{"persist-1": {Key: "immich_set_favorite:" + string(args), Status: "pending", StartedAt: time.Now()}})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.receipts, _ = openWriteStore(filepath.Dir(store.path))
	if _, err := a.doAction(t.Context(), "persist-1", "immich_set_favorite", args); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatal("unknown update was retried")
	}
}

func TestLocalizedViewsAndBounds(t *testing.T) {
	f := newFakeImmich(t)
	a := f.app()
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, ui := range []int{1, 3, 6} {
			for _, mode := range []string{"", "detail"} {
				r := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: ui}}
				v, err := a.page(t.Context(), r, pageState{Mode: mode, AssetID: assetID, DraftID: "draft-1"})
				if err != nil {
					t.Fatalf("%s %d %s: %v", locale, ui, mode, err)
				}
				if err := wire.CheckView(v); err != nil {
					t.Fatalf("%s %d %s: %v", locale, ui, mode, err)
				}
			}
		}
	}
	item := sampleAsset()
	item.Exif.Description = strings.Repeat("photo context\n", 10000)
	v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{AssetID: assetID}, item)
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	if newApp().status()["state"] != "not_configured" {
		t.Fatal("unconfigured extension was ready")
	}
}

func TestFailedFavoriteIsUnknown(t *testing.T) {
	f := newFakeImmich(t)
	f.failWrite = true
	if _, err := f.app().doAction(t.Context(), "failed-1", "immich_set_favorite", []byte(`{"asset_id":"`+assetID+`","favorite":true}`)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatal("failed favorite update was safe to retry")
	}
}
