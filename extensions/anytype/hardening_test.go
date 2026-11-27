package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestEmptySearchReturnsSpacesWithoutStatusPoison(t *testing.T) {
	f := newFakeAnytype(t)
	a := f.app()

	res, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "search",
		Values:    map[string]string{"q": "   "},
	}})
	if err != nil || res.View == nil {
		t.Fatalf("empty search should return spaces view, got err=%v res=%+v", err, res)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("empty search view failed CheckView: %v", err)
	}
	if st := a.status(); st["state"] != "ok" {
		t.Fatalf("empty search must not poison status: %+v", st)
	}
}

func TestCreateViewBackLongBodyDraftIDAndDetailSpaceValidation(t *testing.T) {
	f := newFakeAnytype(t)
	a := f.app()

	longBody := strings.Repeat("x", 2500)
	reviewRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "review-create",
		State:     ext.EncodeState(pageState{Mode: "create", SpaceID: "space-1", SpaceName: "Personal", DraftID: "draft-1"}),
		Values:    map[string]string{"type_key": "page", "name": "Long note", "body": longBody},
	}})
	if err != nil || reviewRes.View == nil {
		t.Fatalf("review-create failed: %v %+v", err, reviewRes)
	}
	if err := wire.CheckView(reviewRes.View); err != nil {
		t.Fatalf("review-create CheckView failed: %v", err)
	}

	backRes, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "create",
		State:     reviewRes.View.State,
	}})
	if err != nil || backRes.View == nil {
		t.Fatalf("back to create failed: %v %+v", err, backRes)
	}
	if err := wire.CheckView(backRes.View); err != nil {
		t.Fatalf("createView Back with 2500-char body failed CheckView: %v", err)
	}

	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "confirm-create",
		State:     ext.EncodeState(pageState{Mode: "review-create", SpaceID: "space-1", TypeKey: "page", Name: "T", Body: "B", DraftID: ""}),
	}}); err == nil {
		t.Fatal("confirm-create with empty DraftID unexpectedly succeeded")
	}

	if _, err := a.action(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{
		Locale:    "en",
		UIVersion: 6,
		Action:    "detail",
		Values:    map[string]string{"id": "obj-1", "space": "bad/space"},
	}}); err == nil {
		t.Fatal("detail action accepted invalid space path segment")
	}
}

func TestPaginatedListsDedupLongIDAndEmptyPageStop(t *testing.T) {
	var emptyCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		switch r.URL.Path {
		case "/v1/spaces":
			if offset == 0 {
				_ = json.NewEncoder(w).Encode(pageOf([]space{
					{ID: "space-1", Name: "Alpha"},
					{ID: strings.Repeat("z", 2100), Name: "TooLongID"},
				}, true))
			} else {
				_ = json.NewEncoder(w).Encode(pageOf([]space{
					{ID: "space-1", Name: "Alpha"},
					{ID: "space-2", Name: "Beta"},
				}, false))
			}
		case "/v1/spaces/space-1/objects":
			if offset == 0 {
				_ = json.NewEncoder(w).Encode(pageOf([]knowledgeObject{
					{ID: "obj-1", SpaceID: "space-1", Name: "First"},
					{ID: "obj-bad", SpaceID: "space-1", Name: ""},
				}, true))
			} else {
				_ = json.NewEncoder(w).Encode(pageOf([]knowledgeObject{
					{ID: "obj-1", SpaceID: "space-1", Name: "First"},
					{ID: "obj-2", SpaceID: "space-1", Name: "Second"},
				}, false))
			}
		case "/v1/spaces/empty-space/objects":
			emptyCalls.Add(1)
			_ = json.NewEncoder(w).Encode(pageOf([]knowledgeObject{}, true))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	a := &app{client: newClient(srv.URL, testToken), receipts: newWriteStore("")}

	sv, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Offset: 50})
	if err != nil {
		t.Fatalf("spaces page failed: %v", err)
	}
	if err := wire.CheckView(sv); err != nil {
		t.Fatalf("spaces page with duplicate + oversized ID failed CheckView: %v", err)
	}
	if got := len(sv.Sections[0].Items); got != 2 {
		t.Fatalf("expected 2 valid deduplicated spaces, got %d", got)
	}

	ov, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "objects", SpaceID: "space-1", SpaceName: "Alpha", Offset: 50})
	if err != nil {
		t.Fatalf("objects page failed: %v", err)
	}
	if err := wire.CheckView(ov); err != nil {
		t.Fatalf("objects page with duplicates failed CheckView: %v", err)
	}
	if got := len(ov.Sections[0].Items); got != 2 {
		t.Fatalf("expected 2 deduplicated objects, got %d", got)
	}
	if want := fmt.Sprintf(textsFor("en").objects, 2); ov.Sections[0].Title != want {
		t.Fatalf("objects title = %q, want %q", ov.Sections[0].Title, want)
	}

	// Empty page with HasMore=true and huge offset must terminate after 1 request.
	ev, err := a.page(t.Context(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{Mode: "objects", SpaceID: "empty-space", SpaceName: "Empty", Offset: 5000})
	if err != nil || wire.CheckView(ev) != nil {
		t.Fatalf("empty-space page failed: %v", err)
	}
	if got := emptyCalls.Load(); got != 1 {
		t.Fatalf("empty page loop sent %d HTTP requests, want 1", got)
	}
	if ev.Sections[0].More != nil {
		t.Fatal("empty batch must not expose More")
	}
}

func TestWriteReceiptKeyCanonicalAndActionScoped(t *testing.T) {
	f := newFakeAnytype(t)
	a := f.app()

	raw1 := json.RawMessage(`{"space_id":"space-1","type_key":"page","name":"Spec","body":"Body"}`)
	raw2 := json.RawMessage(`{ "body": "Body", "name": "Spec", "type_key": "page", "space_id": "space-1" }`)

	out1, err := a.doAction(t.Context(), "req-canon-1", "anytype_create_object", raw1)
	if err != nil {
		t.Fatalf("first doAction failed: %v", err)
	}
	out2, err := a.doAction(t.Context(), "req-canon-1", "anytype_create_object", raw2)
	if err != nil {
		t.Fatalf("replayed doAction with reordered JSON keys failed: %v", err)
	}
	if out1["text"] != out2["text"] {
		t.Fatalf("replayed text mismatch: %v vs %v", out1, out2)
	}
	if len(f.creates) != 1 {
		t.Fatalf("expected 1 upstream create, got %d", len(f.creates))
	}
}

func TestConcurrentInitAndReadsNoRace(t *testing.T) {
	f := newFakeAnytype(t)
	a := f.app()

	initCfg, _ := json.Marshal(map[string]string{"instance_url": f.srv.URL})
	initReq := &ext.Init{
		InitializeParams: wire.InitializeParams{
			Config:  initCfg,
			Secrets: map[string]string{"api_key": testToken},
		},
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for range 10 {
			_, _ = a.init(context.Background(), initReq, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for range 10 {
			_, _ = a.render(context.Background(), &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
		}
	}()
	go func() {
		defer wg.Done()
		for range 10 {
			_, _ = a.callTool(context.Background(), "anytype_spaces", json.RawMessage(`{}`))
			_ = a.status()
		}
	}()
	wg.Wait()
}
