package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestNaNQuantityCreateActionValidationAndReviewDraft(t *testing.T) {
	a := newFakeHomebox(t).app()

	if err := (createArgs{EntityTypeID: typeID, LocationID: locationID, Name: "Item", Quantity: math.NaN()}).validate(); err == nil {
		t.Fatal("createArgs.validate accepted NaN quantity")
	}
	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, Action: "review-create",
		State:  ext.EncodeState(pageState{Mode: "create", EntityTypeID: typeID, LocationID: locationID, DraftID: "d-nan"}),
		Values: map[string]string{"name": "Item", "quantity": "NaN"},
	}}); err == nil {
		t.Fatal("review-create accepted NaN quantity")
	}

	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale: "en", UIVersion: 6, Action: "create",
		Values: map[string]string{"entity_type_id": "not-a-uuid", "location_id": "also-bad"},
	}}); err == nil {
		t.Fatal("create action accepted invalid entity_type_id/location_id")
	}

	if v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{
		Mode: "review-create", DraftID: "d-empty",
	}); err == nil {
		if cerr := wire.CheckView(v); cerr != nil {
			t.Fatalf("review-create with empty draft returned invalid View: %v", cerr)
		}
		t.Fatal("review-create accepted invalid empty draft")
	}
}

func TestCreateChoicesBoundsDedupPaginatedEntitiesAndDetailOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/entities":
			if r.URL.Query().Get("page") == "1" {
				_ = json.NewEncoder(w).Encode(entityPage{Items: []entity{sampleEntity()}, Page: 1, PageSize: 1, Total: 2})
			} else {
				e2 := sampleEntity()
				e2.ID, e2.Name = "523e4567-e89b-42d3-a456-426614174000", "Second item"
				_ = json.NewEncoder(w).Encode(entityPage{Items: []entity{sampleEntity(), e2}, Page: 2, PageSize: 1, Total: 2})
			}
		case "/api/v1/entity-types":
			_ = json.NewEncoder(w).Encode([]entityType{{ID: typeID, Name: strings.Repeat("T", 2100), IsLocation: false}})
		case "/api/v1/entities/tree":
			_ = json.NewEncoder(w).Encode([]treeItem{
				{ID: locationID, Name: "Study", Type: "location"},
				{ID: locationID, Name: "Study dup", Type: "location"},
				{ID: "723e4567-e89b-42d3-a456-426614174000", Name: "", Type: "location"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}
	v, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Page: 2})
	if err != nil {
		t.Fatalf("page error: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("page with duplicate entity/location, blank location, and long type name failed CheckView: %v", err)
	}
	if len(v.Sections[0].Items) != 2 {
		t.Fatalf("got %d entity rows, want 2 deduped rows", len(v.Sections[0].Items))
	}
	if len(v.Sections[1].Items) != 1 {
		t.Fatalf("got %d create choices, want 1 valid deduped choice", len(v.Sections[1].Items))
	}

	// detailView metadata order must be deterministic across renders.
	it := sampleEntity()
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	first := detailView(r, pageState{Mode: "detail", EntityID: entityID}, it)
	var baseline []string
	for _, c := range first.Sections[0].Items {
		baseline = append(baseline, c.Text)
	}
	for range 30 {
		cur := detailView(r, pageState{Mode: "detail", EntityID: entityID}, it)
		for i, c := range cur.Sections[0].Items {
			if c.Text != baseline[i] {
				t.Fatalf("detailView metadata order is nondeterministic: got %q at %d, want %q", c.Text, i, baseline[i])
			}
		}
	}
}

func TestCanonicalActionScopedReceiptKey(t *testing.T) {
	f := newFakeHomebox(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	if _, err := a.doAction(t.Context(), "canon-1", "homebox_create_item", []byte(`{"entity_type_id":"`+typeID+`","location_id":"`+locationID+`","name":"Spare reader","quantity":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "canon-1", "homebox_create_item", []byte(`{ "quantity" : 1 , "name" : "Spare reader" , "location_id" : "`+locationID+`" , "entity_type_id" : "`+typeID+`" }`)); err != nil {
		t.Fatalf("replayed canonical JSON rejected: %v", err)
	}
	if len(f.creates) != 1 {
		t.Fatalf("creates=%d, want 1", len(f.creates))
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]writeReceipt
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(saved["canon-1"].Key, "homebox_create_item:") {
		t.Fatalf("receipt key %q is not action-scoped", saved["canon-1"].Key)
	}
}
