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

const testToken = "synthetic-homebox-token"
const entityID = "123e4567-e89b-42d3-a456-426614174000"
const locationID = "223e4567-e89b-42d3-a456-426614174000"
const typeID = "323e4567-e89b-42d3-a456-426614174000"

type fakeHomebox struct {
	srv       *httptest.Server
	mu        sync.Mutex
	paths     []string
	creates   []map[string]any
	failWrite bool
}

func sampleEntity() entity {
	item := entity{ID: entityID, AssetID: "000042", Name: "Reading tablet", Description: "Personal reading device.", Quantity: 1, SerialNumber: "SYNTHETIC-SERIAL", ModelNumber: "READER-1", Manufacturer: "Example Devices", Notes: "Keep the warranty with this record.", WarrantyDetails: "Two-year coverage", WarrantyExpires: "2028-11-10"}
	item.Location = &entity{ID: locationID, Name: "Study"}
	item.Tags = []tag{{ID: "423e4567-e89b-42d3-a456-426614174000", Name: "Electronics"}}
	return item
}

func newFakeHomebox(t *testing.T) *fakeHomebox {
	f := &fakeHomebox{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/entities":
			switch r.Method {
			case http.MethodGet:
				items := []entity{}
				if r.URL.Query().Get("page") == "1" {
					items = append(items, sampleEntity())
				}
				_ = json.NewEncoder(w).Encode(entityPage{Items: items, Page: 1, PageSize: 30, Total: 31})
			case http.MethodPost:
				if f.failWrite {
					http.Error(w, "synthetic interrupted write", http.StatusServiceUnavailable)
					return
				}
				var in map[string]any
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in["name"] == "" || in["parentId"] != locationID || in["locationId"] != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				f.mu.Lock()
				f.creates = append(f.creates, in)
				f.mu.Unlock()
				item := sampleEntity()
				item.ID, item.Name = "523e4567-e89b-42d3-a456-426614174000", in["name"].(string)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(item)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		case "/api/v1/entities/" + entityID:
			_ = json.NewEncoder(w).Encode(sampleEntity())
		case "/api/v1/entity-types":
			_ = json.NewEncoder(w).Encode([]entityType{{ID: typeID, Name: "Item", IsLocation: false}, {ID: "623e4567-e89b-42d3-a456-426614174000", Name: "Location", IsLocation: true}})
		case "/api/v1/entities/tree":
			if r.URL.Query().Get("withItems") != "false" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode([]treeItem{{ID: locationID, Name: "Study", Type: "location", Children: []treeItem{{ID: "723e4567-e89b-42d3-a456-426614174000", Name: "Desk drawer", Type: "location"}}}})
		case "/api/v1/tags":
			_ = json.NewEncoder(w).Encode([]tag{{ID: "423e4567-e89b-42d3-a456-426614174000", Name: "Electronics"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHomebox) app() *app {
	return &app{client: newClient(f.srv.URL, testToken), receipts: newWriteStore("")}
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeHomebox(t).app())))
}

func TestManifestAndURL(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "homebox" || len(m.Secrets) != 1 || len(m.Contributes.Connectors[0].ConfirmAlways) != 1 || !slices.Equal(m.Permissions.Network, []string{}) {
		t.Fatalf("%+v", m)
	}
	for _, raw := range []string{"https://homebox.example.com", "http://127.0.0.1:7745/", "https://[::1]:7745"} {
		if _, err := instanceURL(raw); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"http://homebox.example.com", "https://me@example.com", "not-a-url"} {
		if _, err := instanceURL(raw); err == nil {
			t.Fatal("accepted " + raw)
		}
	}
}

func TestInventorySearchDetailsAndLookups(t *testing.T) {
	f := newFakeHomebox(t)
	a := f.app()
	text, err := a.callTool(t.Context(), "homebox_search", json.RawMessage(`{"query":"tablet"}`))
	if err != nil || !strings.Contains(text, entityID) || !strings.Contains(text, "Next page: 2") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "homebox_item", json.RawMessage(`{"entity_id":"`+entityID+`"}`))
	if err != nil || !strings.Contains(text, "SYNTHETIC-SERIAL") || !strings.Contains(text, "Two-year coverage") {
		t.Fatalf("%v %s", err, text)
	}
	for _, name := range []string{"homebox_types", "homebox_locations", "homebox_tags"} {
		text, err = a.callTool(t.Context(), name, json.RawMessage(`{}`))
		if err != nil || !strings.Contains(text, "http") && text == "" {
			t.Fatalf("%s %v %s", name, err, text)
		}
	}
	f.mu.Lock()
	paths := strings.Join(f.paths, "\n")
	f.mu.Unlock()
	for _, want := range []string{"q=tablet", "pageSize=30", "orderBy=updatedAt", "withItems=false"} {
		if !strings.Contains(paths, want) {
			t.Fatalf("missing %q in %s", want, paths)
		}
	}
}

func TestCreateReplayAndLocationSemantics(t *testing.T) {
	f := newFakeHomebox(t)
	a := f.app()
	args := []byte(`{"entity_type_id":"` + typeID + `","location_id":"` + locationID + `","name":"Spare reader","description":"Backup device","quantity":1,"tag_ids":["423e4567-e89b-42d3-a456-426614174000"]}`)
	for range 2 {
		if _, err := a.doAction(t.Context(), "create-1", "homebox_create_item", args); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.creates) != 1 || f.creates[0]["parentId"] != locationID || f.creates[0]["locationId"] != nil {
		t.Fatal("item used a location override instead of its parent location")
	}
	if _, err := describeAction("homebox_create_item", args); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"entity_type_id":"bad","location_id":"` + locationID + `","name":"Bad","quantity":1}`, `{"entity_type_id":"` + typeID + `","location_id":"` + locationID + `","name":"Bad","quantity":0}`} {
		if _, err := describeAction("homebox_create_item", []byte(bad)); err == nil {
			t.Fatal("accepted invalid item " + bad)
		}
	}
}

func TestPersistentReplayAndUnknownOutcome(t *testing.T) {
	f := newFakeHomebox(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	args := []byte(`{"entity_type_id":"` + typeID + `","location_id":"` + locationID + `","name":"Spare reader","quantity":1}`)
	if _, err := a.doAction(t.Context(), "persist-1", "homebox_create_item", args); err != nil {
		t.Fatal(err)
	}
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "persist-1", "homebox_create_item", args); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 {
		t.Fatal("restart repeated creation")
	}
	key, _ := json.Marshal(createArgs{EntityTypeID: typeID, LocationID: locationID, Name: "Spare reader", Quantity: 1})
	raw, _ := json.Marshal(map[string]writeReceipt{"persist-1": {Key: "homebox_create_item:" + string(key), Status: "pending", StartedAt: time.Now()}})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.receipts, _ = openWriteStore(filepath.Dir(store.path))
	if _, err := a.doAction(t.Context(), "persist-1", "homebox_create_item", args); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatal("unknown creation was retried")
	}
}

func TestLocalizedViewsAndBounds(t *testing.T) {
	f := newFakeHomebox(t)
	a := f.app()
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, ui := range []int{1, 3, 6} {
			for _, mode := range []string{"", "detail", "create", "review-create"} {
				r := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: ui}}
				st := pageState{Mode: mode, EntityID: entityID, EntityTypeID: typeID, LocationID: locationID, DraftID: "draft-1", Name: "Spare reader", Description: "Backup device", Quantity: 1}
				v, err := a.page(t.Context(), r, st)
				if err != nil {
					t.Fatalf("%s %d %s: %v", locale, ui, mode, err)
				}
				if err := wire.CheckView(v); err != nil {
					t.Fatalf("%s %d %s: %v", locale, ui, mode, err)
				}
			}
		}
	}
	item := sampleEntity()
	item.Notes = strings.Repeat("inventory context\n", 10000)
	v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{EntityID: entityID}, item)
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	if newApp().status()["state"] != "not_configured" {
		t.Fatal("unconfigured extension was ready")
	}
}

func TestFailedWriteIsUnknown(t *testing.T) {
	f := newFakeHomebox(t)
	f.failWrite = true
	args := []byte(`{"entity_type_id":"` + typeID + `","location_id":"` + locationID + `","name":"Spare reader","quantity":1}`)
	if _, err := f.app().doAction(t.Context(), "failed-1", "homebox_create_item", args); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatal("failed creation was safe to retry")
	}
}
