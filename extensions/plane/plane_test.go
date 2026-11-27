package main

import (
	"encoding/json"
	"fmt"
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

const testToken = "synthetic-plane-api-key"

type fakePlane struct {
	srv     *httptest.Server
	mu      sync.Mutex
	paths   []string
	creates int
	updates []map[string]any
}

func sampleItem() workItem {
	item := workItem{ID: "item-1", Name: "Keep reading context", DescriptionStripped: "Preserve the source beside the notes.", SequenceID: 42, Priority: "high", Project: "project-1", ProjectIdentifier: "READ", TargetDate: "2026-11-30", UpdatedAt: "2026-11-10T10:00:00Z"}
	item.State = entityRef{ID: "state-backlog", Name: "Backlog", Group: "backlog"}
	return item
}

func sampleStates() []workflowState {
	return []workflowState{{ID: "state-backlog", Name: "Backlog", Group: "backlog"}, {ID: "state-progress", Name: "In Progress", Group: "started"}, {ID: "state-done", Name: "Done", Group: "completed"}}
}

func newFakePlane(t *testing.T) *fakePlane {
	f := &fakePlane{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/workspaces/personal/projects/":
			page := page[project]{Results: []project{{ID: "project-1", Name: "Reading", Identifier: "READ"}}, NextPageResults: true, NextCursor: "100:1:0"}
			if r.URL.Query().Get("cursor") == "100:1:0" {
				page.Results = nil
				page.NextPageResults = false
			}
			_ = json.NewEncoder(w).Encode(page)
		case "/workspaces/personal/projects/project-1/work-items/":
			if r.Method == http.MethodPost {
				var in map[string]any
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if _, ok := in["description_stripped"]; ok {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				f.mu.Lock()
				f.creates++
				f.mu.Unlock()
				item := sampleItem()
				item.ID = "created-item"
				item.Name, _ = in["name"].(string)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(item)
				return
			}
			page := page[workItem]{Results: []workItem{sampleItem()}, NextPageResults: true, NextCursor: "50:1:0"}
			if r.URL.Query().Get("cursor") == "50:1:0" {
				page.Results = nil
				page.NextPageResults = false
			}
			_ = json.NewEncoder(w).Encode(page)
		case "/workspaces/personal/projects/project-1/work-items/item-1/":
			if r.Method == http.MethodPatch {
				var in map[string]any
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				f.mu.Lock()
				f.updates = append(f.updates, in)
				f.mu.Unlock()
				item := sampleItem()
				if priority, _ := in["priority"].(string); priority != "" {
					item.Priority = priority
				}
				if state, _ := in["state"].(string); state != "" {
					item.State.ID = state
				}
				_ = json.NewEncoder(w).Encode(item)
				return
			}
			_ = json.NewEncoder(w).Encode(sampleItem())
		case "/workspaces/personal/projects/project-1/states/":
			_ = json.NewEncoder(w).Encode(page[workflowState]{Results: sampleStates()})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePlane) app() *app {
	return &app{client: newClient(f.srv.URL, testToken), workspace: "personal", receipts: newWriteStore("")}
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakePlane(t).app())))
}

func TestManifest(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "plane" || len(m.Secrets) != 1 || m.Secrets[0].Name != "api_key" {
		t.Fatalf("%+v", m)
	}
	if len(m.Contributes.Connectors[0].ConfirmAlways) != 2 {
		t.Fatal("create and update must be confirmed")
	}
	if !slices.Equal(m.Permissions.Network, []string{}) {
		t.Fatal(m.Permissions.Network)
	}
}

func TestURLValidationAndWorkItemLink(t *testing.T) {
	for raw, want := range map[string]string{"https://api.plane.so": "https://api.plane.so/api/v1", "https://plane.example.com/api/v1": "https://plane.example.com/api/v1", "http://127.0.0.1:8080": "http://127.0.0.1:8080/api/v1"} {
		got, err := instanceURL(raw)
		if err != nil || got != want {
			t.Fatalf("%q: %v %q", raw, err, got)
		}
	}
	for _, raw := range []string{"http://plane.example.com", "https://me@example.com", "not-a-url"} {
		if _, err := instanceURL(raw); err == nil {
			t.Fatal("accepted " + raw)
		}
	}
	c := newClient("https://api.plane.so/api/v1", testToken)
	if got := c.itemURL(workItem{WebURL: "https://plane.example.com/personal/projects/item-1"}); got != "https://plane.example.com/personal/projects/item-1" {
		t.Fatal(got)
	}
	if got := c.itemURL(workItem{WebURL: "http://plane.example.com/item-1"}); got != "" {
		t.Fatal("accepted an insecure untrusted link")
	}
}

func TestProjectsWorkItemsAndCursor(t *testing.T) {
	f := newFakePlane(t)
	a := f.app()
	text, err := a.callTool(t.Context(), "plane_projects", json.RawMessage(`{"cursor":"100:1:0"}`))
	if err != nil || !strings.Contains(text, "No projects") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "plane_work_items", json.RawMessage(`{"project_id":"project-1"}`))
	if err != nil || !strings.Contains(text, "Next cursor: 50:1:0") || !strings.Contains(text, "item-1") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "plane_work_item", json.RawMessage(`{"project_id":"project-1","work_item_id":"item-1"}`))
	if err != nil || !strings.Contains(text, "Preserve the source") {
		t.Fatalf("%v %s", err, text)
	}
	f.mu.Lock()
	paths := strings.Join(f.paths, "\n")
	f.mu.Unlock()
	for _, want := range []string{"cursor=100%3A1%3A0", "per_page=50", "expand=state"} {
		if !strings.Contains(paths, want) {
			t.Fatalf("missing %q in %s", want, paths)
		}
	}
	for _, raw := range []string{`{"project_id":"project-1","cursor":"bad"}`, `{"project_id":"project-1","cursor":"cursor-not-from-plane"}`} {
		if _, err := a.callTool(t.Context(), "plane_work_items", json.RawMessage(raw)); err == nil {
			t.Fatal("accepted an unverified cursor " + raw)
		}
	}
}

func TestCreateAndPartialUpdateReplay(t *testing.T) {
	f := newFakePlane(t)
	a := f.app()
	create := []byte(`{"project_id":"project-1","name":"Keep reading context","description":"Preserve source context.","priority":"high","target_date":"2026-11-30","state_id":"state-progress"}`)
	for range 2 {
		if _, err := a.doAction(t.Context(), "create-1", "plane_create_work_item", create); err != nil {
			t.Fatal(err)
		}
	}
	update := []byte(`{"project_id":"project-1","work_item_id":"item-1","state_id":"state-progress"}`)
	for range 2 {
		if _, err := a.doAction(t.Context(), "update-1", "plane_update_work_item", update); err != nil {
			t.Fatal(err)
		}
	}
	if f.creates != 1 || len(f.updates) != 1 {
		t.Fatalf("%d %v", f.creates, f.updates)
	}
	if len(f.updates[0]) != 1 || f.updates[0]["state"] != "state-progress" {
		t.Fatalf("partial update cleared other fields: %v", f.updates[0])
	}
	if _, err := a.doAction(t.Context(), "update-1", "plane_update_work_item", []byte(`{"project_id":"project-1","work_item_id":"item-1","priority":"low"}`)); err == nil {
		t.Fatal("receipt accepted a different update")
	}
	before := len(f.updates)
	if _, err := a.doAction(t.Context(), "update-bad-state", "plane_update_work_item", []byte(`{"project_id":"project-1","work_item_id":"item-1","state_id":"state-other"}`)); err == nil {
		t.Fatal("accepted another project's state")
	}
	if len(f.updates) != before {
		t.Fatal("invalid state reached the remote API")
	}
	for _, raw := range []string{`{"project_id":"project-1","work_item_id":"item-1"}`, `{"project_id":"project-1","name":"new","priority":"high","target_date":"2026-02-30"}`} {
		if _, err := describeAction("plane_update_work_item", []byte(raw)); err == nil {
			if _, err := describeAction("plane_create_work_item", []byte(raw)); err == nil {
				t.Fatal("accepted invalid action " + raw)
			}
		}
	}
}

func TestPersistentReplayAndUnknownOutcome(t *testing.T) {
	f := newFakePlane(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	args := []byte(`{"project_id":"project-1","work_item_id":"item-1","priority":"low"}`)
	if _, err := a.doAction(t.Context(), "persist-1", "plane_update_work_item", args); err != nil {
		t.Fatal(err)
	}
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "persist-1", "plane_update_work_item", args); err != nil {
		t.Fatal(err)
	}
	if len(f.updates) != 1 {
		t.Fatal("restart repeated update")
	}
	key, _ := json.Marshal(updateArgs{ProjectID: "project-1", WorkItemID: "item-1", Priority: "low"})
	raw, _ := json.Marshal(map[string]writeReceipt{"persist-1": {Key: "plane_update_work_item:" + string(key), Status: "pending", StartedAt: time.Now()}})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.receipts, _ = openWriteStore(filepath.Dir(store.path))
	if _, err := a.doAction(t.Context(), "persist-1", "plane_update_work_item", args); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatal("unknown update was retried")
	}
	if len(f.updates) != 1 {
		t.Fatal("unknown update caused another write")
	}
}

func TestLocalizedPagesAndCommentlessForms(t *testing.T) {
	f := newFakePlane(t)
	a := f.app()
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, ui := range []int{1, 3, 6} {
			for _, mode := range []string{"", "items", "detail", "create", "review-create", "edit", "review-edit"} {
				r := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: ui}}
				st := pageState{Mode: mode, ProjectID: "project-1", ProjectName: "Reading", Identifier: "READ", WorkItemID: "item-1", DraftID: "draft-1", Name: "Preserve context", Description: "Keep the source.", Priority: "high", StateID: "state-progress"}
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
}

func TestPageActionsPreserveDraftAndPartialEdit(t *testing.T) {
	f := newFakePlane(t)
	a := f.app()
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6, Values: map[string]string{"priority": "low", "state_id": "state-progress"}}}
	st := pageState{Mode: "edit", ProjectID: "project-1", WorkItemID: "item-1", Priority: "high", StateID: "state-backlog", DraftID: "draft-1"}
	result, err := a.action(t.Context(), withAction(r, "review-edit", st))
	if err != nil || result.View == nil || wire.CheckView(result.View) != nil {
		t.Fatalf("review: %v %+v", err, result)
	}
	result, err = a.action(t.Context(), withAction(r, "confirm-edit", pageState{Mode: "review-edit", ProjectID: "project-1", WorkItemID: "item-1", Priority: "low", StateID: "state-progress", DraftID: "draft-1"}))
	if err != nil || !result.Pop || len(f.updates) != 1 {
		t.Fatalf("confirm: %v %+v %v", err, result, f.updates)
	}
	if _, ok := f.updates[0]["name"]; ok || f.updates[0]["priority"] != "low" || f.updates[0]["state"] != "state-progress" {
		t.Fatal(f.updates[0])
	}
}

func withAction(r *ext.UIRequest, action string, st pageState) *ext.UIRequest {
	r.Action = action
	r.State = ext.EncodeState(st)
	return r
}

func TestBoundsAndNoConfig(t *testing.T) {
	item := sampleItem()
	item.DescriptionStripped = strings.Repeat("context ", 3000)
	v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{ProjectID: "project-1"}, item, "")
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	a := newApp()
	if a.status()["state"] != "not_configured" {
		t.Fatal(a.status())
	}
	v, err := a.render(t.Context(), &ext.UIRequest{})
	if err != nil || !strings.Contains(v.Sections[0].Items[0].Title, "workspace slug") {
		t.Fatalf("%v %+v", err, v)
	}
	for _, value := range []string{"", " ", "2026-02-30", "2026-13-01"} {
		if err := validDate(value); value != "" && err == nil {
			t.Fatal("accepted " + value)
		}
	}
}

func TestAPIFailureDoesNotWrite(t *testing.T) {
	f := newFakePlane(t)
	f.srv.Close()
	a := f.app()
	if _, err := a.doAction(t.Context(), "network-1", "plane_update_work_item", []byte(`{"project_id":"project-1","work_item_id":"item-1","priority":"low"}`)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatal(err)
	}
	if len(f.updates) != 0 {
		t.Fatal("network failure was written as success")
	}
	if _, err := fmt.Fprint(os.Stderr, ""); err != nil {
		t.Fatal(err)
	}
}
