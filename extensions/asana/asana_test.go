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

const testToken = "synthetic-asana-token"

type fakeAsana struct {
	srv       *httptest.Server
	mu        sync.Mutex
	paths     []string
	creates   []map[string]any
	completes []map[string]any
}

func sampleTask() task {
	item := task{GID: "task-1", Name: "Review the reading notes", Notes: "Keep source context beside the next action.", DueOn: "2026-11-30", PermalinkURL: "https://app.asana.com/0/project/task-1"}
	item.Projects = []resource{{GID: "project-1", Name: "Reading"}}
	item.Assignee = &resource{GID: "user-1", Name: "Example User"}
	return item
}

func newFakeAsana(t *testing.T) *fakeAsana {
	f := &fakeAsana{}
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
		case "/workspaces":
			page := page[workspace]{Data: []workspace{{GID: "workspace-1", Name: "Personal", IsOrganization: false}}}
			if r.URL.Query().Get("offset") == "next-workspace" {
				page.Data = nil
			} else {
				page.NextPage = &struct {
					Offset string `json:"offset"`
					URI    string `json:"uri"`
				}{Offset: "next-workspace"}
			}
			_ = json.NewEncoder(w).Encode(page)
		case "/projects":
			if r.URL.Query().Get("workspace") != "workspace-1" || r.URL.Query().Get("archived") != "false" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(page[resource]{Data: []resource{{GID: "project-1", Name: "Reading"}}})
		case "/projects/project-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": project{GID: "project-1", Name: "Reading", Workspace: &resource{GID: "workspace-1", Name: "Personal"}}})
		case "/projects/project-other":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": project{GID: "project-other", Name: "Other", Workspace: &resource{GID: "workspace-other"}}})
		case "/tasks":
			if r.Method == http.MethodPost {
				var in struct {
					Data map[string]any `json:"data"`
				}
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if in.Data["workspace"] != "workspace-1" || in.Data["assignee"] != "me" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				f.mu.Lock()
				f.creates = append(f.creates, in.Data)
				f.mu.Unlock()
				item := sampleTask()
				item.GID, _ = in.Data["gid"].(string)
				item.Name, _ = in.Data["name"].(string)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": item})
				return
			}
			if r.URL.Query().Get("assignee") != "me" || r.URL.Query().Get("workspace") != "workspace-1" || r.URL.Query().Get("completed_since") != "now" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			page := page[task]{Data: []task{sampleTask()}}
			if r.URL.Query().Get("offset") == "next-task" {
				page.Data = nil
			} else {
				page.NextPage = &struct {
					Offset string `json:"offset"`
					URI    string `json:"uri"`
				}{Offset: "next-task"}
			}
			_ = json.NewEncoder(w).Encode(page)
		case "/tasks/task-1":
			if r.Method == http.MethodPut {
				var in struct {
					Data map[string]any `json:"data"`
				}
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				f.mu.Lock()
				f.completes = append(f.completes, in.Data)
				f.mu.Unlock()
				item := sampleTask()
				item.Completed = true
				_ = json.NewEncoder(w).Encode(map[string]any{"data": item})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": sampleTask()})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAsana) app() *app {
	return &app{client: newClient(f.srv.URL, testToken), receipts: newWriteStore("")}
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeAsana(t).app())))
}

func TestManifest(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "asana" || len(m.Secrets) != 1 || m.Secrets[0].Name != "access_token" {
		t.Fatalf("%+v", m)
	}
	if len(m.Contributes.Connectors[0].ConfirmAlways) != 2 {
		t.Fatal("create and complete must be confirmed")
	}
	if !slices.Equal(m.Permissions.Network, []string{"app.asana.com"}) {
		t.Fatal(m.Permissions.Network)
	}
}

func TestWorkspacesTasksAndPagination(t *testing.T) {
	f := newFakeAsana(t)
	a := f.app()
	text, err := a.callTool(t.Context(), "asana_workspaces", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(text, "Next offset: next-workspace") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "asana_projects", json.RawMessage(`{"workspace_gid":"workspace-1"}`))
	if err != nil || !strings.Contains(text, "project-1") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "asana_tasks", json.RawMessage(`{"workspace_gid":"workspace-1","offset":"next-task"}`))
	if err != nil || !strings.Contains(text, "No incomplete assigned tasks") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "asana_task", json.RawMessage(`{"task_gid":"task-1"}`))
	if err != nil || !strings.Contains(text, "Keep source context") {
		t.Fatalf("%v %s", err, text)
	}
	f.mu.Lock()
	paths := strings.Join(f.paths, "\n")
	f.mu.Unlock()
	for _, want := range []string{"assignee=me", "completed_since=now", "offset=next-task", "opt_fields=", "archived=false"} {
		if !strings.Contains(paths, want) {
			t.Fatalf("missing %q in %s", want, paths)
		}
	}
}

func TestCreateAndCompleteReplay(t *testing.T) {
	f := newFakeAsana(t)
	a := f.app()
	create := []byte(`{"workspace_gid":"workspace-1","project_gid":"project-1","name":"Review reading notes","notes":"Preserve context.","due_on":"2026-11-30"}`)
	for range 2 {
		if _, err := a.doAction(t.Context(), "create-1", "asana_create_task", create); err != nil {
			t.Fatal(err)
		}
	}
	complete := []byte(`{"task_gid":"task-1"}`)
	for range 2 {
		if _, err := a.doAction(t.Context(), "complete-1", "asana_complete_task", complete); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.creates) != 1 || len(f.completes) != 1 {
		t.Fatalf("%v %v", f.creates, f.completes)
	}
	if len(f.completes[0]) != 1 || f.completes[0]["completed"] != true {
		t.Fatal("completion changed other fields")
	}
	if _, err := a.doAction(t.Context(), "complete-1", "asana_complete_task", []byte(`{"task_gid":"other-task"}`)); err == nil {
		t.Fatal("receipt accepted another task")
	}
	if _, err := a.doAction(t.Context(), "create-other-project", "asana_create_task", []byte(`{"workspace_gid":"workspace-1","project_gid":"project-other","name":"Wrong workspace"}`)); err == nil || len(f.creates) != 1 {
		t.Fatal("cross-workspace project was accepted")
	}
	if _, err := describeAction("asana_create_task", create); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"workspace_gid":"workspace-1","name":"bad","due_on":"2026-02-30"}`, `{"workspace_gid":"workspace-1","name":"bad","project_gid":"project/other"}`} {
		if _, err := describeAction("asana_create_task", []byte(raw)); err == nil {
			t.Fatal("accepted invalid create " + raw)
		}
	}
}

func TestPersistentReplayAndUnknownOutcome(t *testing.T) {
	f := newFakeAsana(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	args := []byte(`{"task_gid":"task-1"}`)
	if _, err := a.doAction(t.Context(), "persist-1", "asana_complete_task", args); err != nil {
		t.Fatal(err)
	}
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "persist-1", "asana_complete_task", args); err != nil {
		t.Fatal(err)
	}
	if len(f.completes) != 1 {
		t.Fatal("restart repeated completion")
	}
	key, _ := json.Marshal(completeArgs{TaskGID: "task-1"})
	raw, _ := json.Marshal(map[string]writeReceipt{"persist-1": {Key: "asana_complete_task:" + string(key), Status: "pending", StartedAt: time.Now()}})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.receipts, _ = openWriteStore(filepath.Dir(store.path))
	if _, err := a.doAction(t.Context(), "persist-1", "asana_complete_task", args); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatal("unknown completion was retried")
	}
	if len(f.completes) != 1 {
		t.Fatal("unknown completion caused another write")
	}
}

func TestLocalizedViewsAndActions(t *testing.T) {
	f := newFakeAsana(t)
	a := f.app()
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, ui := range []int{1, 3, 6} {
			for _, mode := range []string{"", "tasks", "detail", "create", "review-create"} {
				r := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: ui}}
				st := pageState{Mode: mode, WorkspaceGID: "workspace-1", WorkspaceName: "Personal", TaskGID: "task-1", DraftID: "draft-1", Name: "Review reading notes", Notes: "Keep source context.", DueOn: "2026-11-30"}
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
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6, Values: map[string]string{"name": "Review notes", "notes": "Preserve context.", "project_gid": "project-1", "due_on": "2026-11-30"}}}
	st := pageState{Mode: "create", WorkspaceGID: "workspace-1", DraftID: "draft-1"}
	result, err := a.action(t.Context(), withAction(r, "review-create", st))
	if err != nil || result.View == nil || wire.CheckView(result.View) != nil {
		t.Fatalf("review: %v %+v", err, result)
	}
	result, err = a.action(t.Context(), withAction(r, "confirm-create", pageState{Mode: "review-create", WorkspaceGID: "workspace-1", DraftID: "draft-1", Name: "Review notes", Notes: "Preserve context.", ProjectGID: "project-1", DueOn: "2026-11-30"}))
	if err != nil || !result.Pop || len(f.creates) != 1 {
		t.Fatalf("confirm: %v %+v %v", err, result, f.creates)
	}
}

func withAction(r *ext.UIRequest, action string, st pageState) *ext.UIRequest {
	r.Action = action
	r.State = ext.EncodeState(st)
	return r
}

func TestBoundsAndNoConfig(t *testing.T) {
	item := sampleTask()
	item.Notes = strings.Repeat("context ", 3000)
	v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{WorkspaceGID: "workspace-1", TaskGID: "task-1", DraftID: "draft-1"}, item)
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://app.asana.com/0/task", "https://example.com/task"} {
		item.PermalinkURL = raw
		v = detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{TaskGID: "task-1"}, item)
		if err := wire.CheckView(v); err != nil {
			t.Fatal(err)
		}
	}
	a := newApp()
	if a.status()["state"] != "not_configured" {
		t.Fatal(a.status())
	}
	v, err := a.render(t.Context(), &ext.UIRequest{})
	if err != nil || !strings.Contains(v.Sections[0].Items[0].Title, "personal access token") {
		t.Fatalf("%v %+v", err, v)
	}
}

func TestNetworkFailureIsUnknown(t *testing.T) {
	f := newFakeAsana(t)
	f.srv.Close()
	a := f.app()
	if _, err := a.doAction(t.Context(), "network-1", "asana_complete_task", []byte(`{"task_gid":"task-1"}`)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatal(err)
	}
	if len(f.completes) != 0 {
		t.Fatal("failed write was counted as successful")
	}
	if _, err := fmt.Fprint(os.Stderr, ""); err != nil {
		t.Fatal(err)
	}
}
