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

const testToken = "opapi-synthetic-token"

type fakeOpenProject struct {
	srv       *httptest.Server
	mu        sync.Mutex
	paths     []string
	creates   []map[string]any
	failWrite bool
}

func sampleWorkPackage() workPackage {
	item := workPackage{ID: 42, Subject: "Preserve reading context", StartDate: "2026-11-10", DueDate: "2026-11-30", CreatedAt: "2026-11-10T10:00:00Z"}
	item.Description.Raw = "Keep source context beside the next step."
	item.Links.Project = link{Href: "/api/v3/projects/7", Title: "Reading"}
	item.Links.Type = link{Href: "/api/v3/types/1", Title: "Task"}
	item.Links.Status = link{Href: "/api/v3/statuses/1", Title: "New"}
	item.Links.Assignee = link{Href: "/api/v3/users/42", Title: "Reader"}
	return item
}

func newFakeOpenProject(t *testing.T) *fakeOpenProject {
	f := &fakeOpenProject{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/hal+json")
		switch r.URL.Path {
		case "/api/v3/users/me":
			_ = json.NewEncoder(w).Encode(currentUser{ID: 42, Name: "Reader", Login: "reader", Links: struct {
				Self link `json:"self"`
			}{Self: link{Href: "/api/v3/users/42"}}})
		case "/api/v3/work_packages":
			if r.Method == http.MethodPost {
				f.mu.Lock()
				fail := f.failWrite
				f.mu.Unlock()
				if fail {
					http.Error(w, "synthetic interrupted write", http.StatusServiceUnavailable)
					return
				}
				var in map[string]any
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in["subject"] == "" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				f.mu.Lock()
				f.creates = append(f.creates, in)
				f.mu.Unlock()
				item := sampleWorkPackage()
				item.ID, item.Subject = 43, in["subject"].(string)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(item)
				return
			}
			if !strings.Contains(r.URL.Query().Get("filters"), "assigned_to") || !strings.Contains(r.URL.Query().Get("filters"), "42") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			items := []workPackage{sampleWorkPackage()}
			if r.URL.Query().Get("offset") != "1" {
				items = nil
			}
			var out collection[workPackage]
			out.Embedded.Elements = items
			out.Total, out.Count, out.PageSize, out.Offset = 1, len(items), 50, 1
			out.Links.NextByOffset = link{Href: "/api/v3/work_packages?offset=2"}
			_ = json.NewEncoder(w).Encode(out)
		case "/api/v3/work_packages/42":
			_ = json.NewEncoder(w).Encode(sampleWorkPackage())
		case "/api/v3/projects":
			items := []project{{ID: 7, Name: "Reading", Identifier: "reading", Active: true, Links: struct {
				Self link `json:"self"`
			}{Self: link{Href: "/api/v3/projects/7"}}}}
			var out collection[project]
			out.Embedded.Elements = items
			out.Total, out.Count = 1, 1
			_ = json.NewEncoder(w).Encode(out)
		case "/api/v3/types":
			items := []workType{{ID: 1, Name: "Task", IsDefault: true, IsMilestone: false, Links: struct {
				Self link `json:"self"`
			}{Self: link{Href: "/api/v3/types/1"}}}}
			var out collection[workType]
			out.Embedded.Elements = items
			out.Total, out.Count = 1, 1
			_ = json.NewEncoder(w).Encode(out)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOpenProject) app() *app {
	return &app{client: newClient(f.srv.URL, testToken), receipts: newWriteStore("")}
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeOpenProject(t).app())))
}

func TestManifest(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "openproject" || len(m.Secrets) != 1 || m.Secrets[0].Name != "api_token" {
		t.Fatalf("%+v", m)
	}
	if len(m.Contributes.Connectors[0].ConfirmAlways) != 1 {
		t.Fatal("create must be confirmed")
	}
	if !slices.Equal(m.Permissions.Network, []string{}) {
		t.Fatal(m.Permissions.Network)
	}
}

func TestInstanceURLValidation(t *testing.T) {
	for raw, want := range map[string]string{"https://openproject.example.com": "https://openproject.example.com", "http://127.0.0.1:8080/": "http://127.0.0.1:8080"} {
		got, err := instanceURL(raw)
		if err != nil || got != want {
			t.Fatalf("%q: %v %q", raw, err, got)
		}
	}
	for _, raw := range []string{"http://openproject.example.com", "https://me@example.com", "not-a-url"} {
		if _, err := instanceURL(raw); err == nil {
			t.Fatal("accepted " + raw)
		}
	}
}

func TestAssignedWorkPackagesProjectsTypesAndPagination(t *testing.T) {
	f := newFakeOpenProject(t)
	a := f.app()
	text, err := a.callTool(t.Context(), "openproject_work_packages", json.RawMessage(`{"offset":2}`))
	if err != nil || !strings.Contains(text, "No assigned work packages") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "openproject_work_package", json.RawMessage(`{"work_package_id":42}`))
	if err != nil || !strings.Contains(text, "Keep source context") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "openproject_projects", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(text, "/api/v3/projects/7") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "openproject_types", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(text, "/api/v3/types/1") {
		t.Fatalf("%v %s", err, text)
	}
	f.mu.Lock()
	paths := strings.Join(f.paths, "\n")
	f.mu.Unlock()
	for _, want := range []string{"assigned_to", "offset=2", "pageSize=50", "sortBy="} {
		if !strings.Contains(paths, want) {
			t.Fatalf("missing %q in %s", want, paths)
		}
	}
}

func TestCreateReplayAndPayload(t *testing.T) {
	f := newFakeOpenProject(t)
	a := f.app()
	args := []byte(`{"project_href":"/api/v3/projects/7","type_href":"/api/v3/types/1","subject":"Keep reading context","description":"Preserve source context.","start_date":"2026-11-10","due_date":"2026-11-30"}`)
	for range 2 {
		if _, err := a.doAction(t.Context(), "create-1", "openproject_create_work_package", args); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.creates) != 1 {
		t.Fatal(f.creates)
	}
	links, ok := f.creates[0]["_links"].(map[string]any)
	if !ok || links["project"].(map[string]any)["href"] != "/api/v3/projects/7" || links["type"].(map[string]any)["href"] != "/api/v3/types/1" {
		t.Fatal(f.creates)
	}
	if _, err := a.doAction(t.Context(), "create-1", "openproject_create_work_package", []byte(`{"project_href":"/api/v3/projects/7","type_href":"/api/v3/types/1","subject":"Different"}`)); err == nil {
		t.Fatal("receipt accepted a different issue")
	}
	if _, err := describeAction("openproject_create_work_package", args); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"project_href":"/api/v3/projects/7","type_href":"/api/v3/types/1","subject":"bad","start_date":"2026-02-30"}`, `{"project_href":"https://example.com/project","type_href":"/api/v3/types/1","subject":"bad"}`, `{"project_href":"/api/v3/projects/7/../../users/me","type_href":"/api/v3/types/1","subject":"bad"}`, `{"project_href":"/api/v3/projects/7","type_href":"/api/v3/types/1?full=true","subject":"bad"}`} {
		if _, err := describeAction("openproject_create_work_package", []byte(bad)); err == nil {
			t.Fatal("accepted invalid create " + bad)
		}
	}
}

func TestPersistentReplayAndUnknownOutcome(t *testing.T) {
	f := newFakeOpenProject(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	args := []byte(`{"project_href":"/api/v3/projects/7","type_href":"/api/v3/types/1","subject":"Keep reading context","description":"Preserve source context."}`)
	if _, err := a.doAction(t.Context(), "persist-1", "openproject_create_work_package", args); err != nil {
		t.Fatal(err)
	}
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "persist-1", "openproject_create_work_package", args); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 {
		t.Fatal("restart repeated creation")
	}
	key, _ := json.Marshal(createArgs{ProjectHref: "/api/v3/projects/7", TypeHref: "/api/v3/types/1", Subject: "Keep reading context", Description: "Preserve source context."})
	raw, _ := json.Marshal(map[string]writeReceipt{"persist-1": {Key: "openproject_create_work_package:" + string(key), Status: "pending", StartedAt: time.Now()}})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.receipts, _ = openWriteStore(filepath.Dir(store.path))
	if _, err := a.doAction(t.Context(), "persist-1", "openproject_create_work_package", args); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatal("unknown create was retried")
	}
	if len(f.creates) != 1 {
		t.Fatal("unknown create caused another write")
	}
}

func TestLocalizedViewsAndCreateFlow(t *testing.T) {
	f := newFakeOpenProject(t)
	a := f.app()
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, ui := range []int{1, 3, 6} {
			for _, mode := range []string{"", "detail", "projects", "create", "review-create"} {
				r := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: ui}}
				st := pageState{Mode: mode, WorkID: 42, ProjectHref: "/api/v3/projects/7", ProjectName: "Reading", TypeHref: "/api/v3/types/1", TypeName: "Task", DraftID: "draft-1", Subject: "Keep context", Description: "Preserve the source."}
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
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "zh-Hans", UIVersion: 6, Values: map[string]string{"subject": "Keep context", "description": "保留来源上下文。"}}}
	result, err := a.action(t.Context(), withAction(r, "review-create", pageState{Mode: "create", ProjectHref: "/api/v3/projects/7", TypeHref: "/api/v3/types/1", DraftID: "draft-1"}))
	if err != nil || result.View == nil || wire.CheckView(result.View) != nil {
		t.Fatalf("review: %v %+v", err, result)
	}
	result, err = a.action(t.Context(), withAction(r, "confirm-create", pageState{Mode: "review-create", ProjectHref: "/api/v3/projects/7", TypeHref: "/api/v3/types/1", DraftID: "draft-1", Subject: "Keep context", Description: "保留来源上下文。"}))
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
	item := sampleWorkPackage()
	item.Description.Raw = strings.Repeat("context\n", 2000)
	v := detailView(&ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}, pageState{WorkID: 42}, item)
	if err := wire.CheckView(v); err != nil {
		t.Fatal(err)
	}
	a := newApp()
	if a.status()["state"] != "not_configured" {
		t.Fatal(a.status())
	}
	v, err := a.render(t.Context(), &ext.UIRequest{})
	if err != nil || !strings.Contains(v.Sections[0].Items[0].Title, "instance URL") {
		t.Fatalf("%v %+v", err, v)
	}
}

func TestPaginationAndStrictCreateLinks(t *testing.T) {
	f := newFakeOpenProject(t)
	a := f.app()
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	result, err := a.action(t.Context(), withAction(r, "more", pageState{Offset: 1}))
	if err != nil {
		t.Fatal(err)
	}
	var st pageState
	if err := json.Unmarshal([]byte(result.View.State), &st); err != nil || st.Offset != 2 {
		t.Fatalf("next page state did not preserve the server offset: %q %v", result.View.State, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(strings.Join(f.paths, "\n"), "offset=2") {
		t.Fatal("more action requested the first page again")
	}
}

func TestFailedFinalWriteIsUnknown(t *testing.T) {
	f := newFakeOpenProject(t)
	f.failWrite = true
	a := f.app()
	if _, err := a.doAction(t.Context(), "network-1", "openproject_create_work_package", []byte(`{"project_href":"/api/v3/projects/7","type_href":"/api/v3/types/1","subject":"Keep context"}`)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatal(err)
	}
	if len(f.creates) != 0 {
		t.Fatal("failed write was counted as successful")
	}
	if _, err := fmt.Fprint(os.Stderr, ""); err != nil {
		t.Fatal(err)
	}
}
