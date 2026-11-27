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

const testToken = "synthetic-vikunja-token"

type fakeVikunja struct {
	srv       *httptest.Server
	mu        sync.Mutex
	paths     []string
	creates   []map[string]any
	completes []map[string]any
	failWrite bool
}

func sampleTask() task {
	return task{ID: 42, Identifier: "READ-42", Title: "Review reading queue", Description: "Keep the source context beside the next step.", DueDate: "2026-11-30T12:00:00Z", ProjectID: 7, Assignees: []user{{ID: 1, Username: "reader"}}, Comments: []comment{{ID: 1, Comment: "Check the source before completion.", Author: user{}}}}
}

func newFakeVikunja(t *testing.T) *fakeVikunja {
	f := &fakeVikunja{}
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
		case "/api/v1/user":
			_ = json.NewEncoder(w).Encode(user{ID: 1, Username: "reader", Name: "Reader"})
		case "/api/v1/tasks":
			if r.URL.Query().Get("s") != "" && r.URL.Query().Get("filter") != "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.URL.Query().Get("s") == "" && !strings.Contains(r.URL.Query().Get("filter"), "assignees = 'reader'") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("X-Pagination-Total", "51")
			w.Header().Set("X-Pagination-Total-Pages", "2")
			items := []task{}
			if r.URL.Query().Get("page") == "1" {
				items = append(items, sampleTask())
			}
			_ = json.NewEncoder(w).Encode(items)
		case "/api/v1/tasks/42":
			if r.Method == http.MethodPost {
				if f.failWrite {
					http.Error(w, "synthetic interrupted update", http.StatusServiceUnavailable)
					return
				}
				var in map[string]any
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in) != 1 || in["done"] != true {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				f.mu.Lock()
				f.completes = append(f.completes, in)
				f.mu.Unlock()
				item := sampleTask()
				item.Done = true
				_ = json.NewEncoder(w).Encode(item)
				return
			}
			if r.URL.Query().Get("expand") != "comments" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(sampleTask())
		case "/api/v1/projects":
			_ = json.NewEncoder(w).Encode([]project{{ID: 7, Title: "Reading", Identifier: "READ"}})
		case "/api/v1/projects/7/tasks":
			if r.Method != http.MethodPut || f.failWrite {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var in map[string]any
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in["title"] == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.creates = append(f.creates, in)
			f.mu.Unlock()
			item := sampleTask()
			item.ID, item.Title = 43, in["title"].(string)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(item)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVikunja) app() *app {
	return &app{client: newClient(f.srv.URL, testToken), receipts: newWriteStore("")}
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeVikunja(t).app())))
}

func TestManifestAndURL(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "vikunja" || len(m.Secrets) != 1 || len(m.Contributes.Connectors[0].ConfirmAlways) != 2 || !slices.Equal(m.Permissions.Network, []string{}) {
		t.Fatalf("%+v", m)
	}
	for _, raw := range []string{"https://vikunja.example.com", "http://127.0.0.1:3456/", "https://[::1]:3456"} {
		if _, err := instanceURL(raw); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"http://vikunja.example.com", "https://me@example.com", "not-a-url"} {
		if _, err := instanceURL(raw); err == nil {
			t.Fatal("accepted " + raw)
		}
	}
	if newClient("https://vikunja.example.com", "x").base != "https://vikunja.example.com/api/v1" {
		t.Fatal("missing API base path")
	}
}

func TestAssignedTasksSearchAndComments(t *testing.T) {
	f := newFakeVikunja(t)
	a := f.app()
	text, err := a.callTool(t.Context(), "vikunja_tasks", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(text, "Task #42") || !strings.Contains(text, "Next page: 2") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "vikunja_tasks", json.RawMessage(`{"query":"reading"}`))
	if err != nil || !strings.Contains(text, "Review reading") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "vikunja_task", json.RawMessage(`{"task_id":42}`))
	if err != nil || !strings.Contains(text, "Vikunja user") || !strings.Contains(text, "source context") {
		t.Fatalf("%v %s", err, text)
	}
	f.mu.Lock()
	paths := strings.Join(f.paths, "\n")
	f.mu.Unlock()
	for _, want := range []string{"done+%3D+false", "assignees+%3D+%27reader%27", "s=reading", "expand=comments", "per_page=50"} {
		if !strings.Contains(paths, want) {
			t.Fatalf("missing %q in %s", want, paths)
		}
	}
}

func TestCreateCompleteReplayAndPayload(t *testing.T) {
	f := newFakeVikunja(t)
	a := f.app()
	args := []byte(`{"project_id":7,"title":"Keep reading context","description":"Review source.","due_date":"2026-11-30T12:00:00Z"}`)
	for range 2 {
		if _, err := a.doAction(t.Context(), "create-1", "vikunja_create_task", args); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := a.doAction(t.Context(), "complete-1", "vikunja_complete_task", []byte(`{"task_id":42}`)); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.creates) != 1 || len(f.completes) != 1 || f.completes[0]["done"] != true {
		t.Fatal("replay repeated writes or completion changed other fields")
	}
	if _, err := describeAction("vikunja_create_task", args); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"project_id":7,"title":"Bad","due_date":"tomorrow"}`, `{"project_id":-1,"title":"Bad"}`} {
		if _, err := describeAction("vikunja_create_task", []byte(bad)); err == nil {
			t.Fatal("accepted invalid create " + bad)
		}
	}
	if _, err := a.doAction(t.Context(), "complete-1", "vikunja_complete_task", []byte(`{"task_id":43}`)); err == nil {
		t.Fatal("request ID accepted a different completion")
	}
}

func TestPersistentReplayAndUnknownOutcome(t *testing.T) {
	f := newFakeVikunja(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	args := []byte(`{"project_id":7,"title":"Persistent task"}`)
	if _, err := a.doAction(t.Context(), "persist-1", "vikunja_create_task", args); err != nil {
		t.Fatal(err)
	}
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "persist-1", "vikunja_create_task", args); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 {
		t.Fatal("restart repeated creation")
	}
	keyData, _ := json.Marshal(createArgs{ProjectID: 7, Title: "Persistent task"})
	raw, _ := json.Marshal(map[string]writeReceipt{"persist-1": {Key: "vikunja_create_task:" + string(keyData), Status: "pending", StartedAt: time.Now()}})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.receipts, _ = openWriteStore(filepath.Dir(store.path))
	if _, err := a.doAction(t.Context(), "persist-1", "vikunja_create_task", args); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatal("unknown create was retried")
	}
}

func TestLocalizedViewsAndUTF8Draft(t *testing.T) {
	f := newFakeVikunja(t)
	a := f.app()
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, ui := range []int{1, 3, 6} {
			for _, mode := range []string{"", "detail", "projects", "create", "review-create"} {
				r := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: ui}}
				st := pageState{Mode: mode, TaskID: 42, ProjectID: 7, DraftID: "draft-1", Title: "Keep context", Description: "Keep the source."}
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
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6, Values: map[string]string{"title": "整理阅读", "description": strings.Repeat("上下文", 400)}}}
	r.Action = "review-create"
	r.State = ext.EncodeState(pageState{Mode: "create", ProjectID: 7, DraftID: "draft-1"})
	result, err := a.action(t.Context(), r)
	if err != nil || wire.CheckView(result.View) != nil || len(f.creates) != 0 {
		t.Fatalf("UTF-8 draft: %v", err)
	}
	r.Action, r.State = "confirm-create", result.View.State
	result, err = a.action(t.Context(), r)
	if err != nil || !result.Pop || len(f.creates) != 1 {
		t.Fatalf("confirm: %v %+v", err, result)
	}
	r.Action = "confirm-create"
	r.State = ext.EncodeState(pageState{Mode: "create", ProjectID: 7, DraftID: "different", Title: "Unreviewed"})
	if _, err := a.action(t.Context(), r); err == nil || len(f.creates) != 1 {
		t.Fatal("created unreviewed task")
	}
	if newApp().status()["state"] != "not_configured" {
		t.Fatal("unconfigured extension was ready")
	}
}

func TestFailedWriteIsUnknown(t *testing.T) {
	f := newFakeVikunja(t)
	f.failWrite = true
	if _, err := f.app().doAction(t.Context(), "failed-1", "vikunja_complete_task", []byte(`{"task_id":42}`)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatal("failed completion was safe to retry")
	}
}
