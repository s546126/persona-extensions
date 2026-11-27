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

const testToken = "synthetic-kimai-token"

type fakeKimai struct {
	srv       *httptest.Server
	mu        sync.Mutex
	paths     []string
	creates   []map[string]any
	stops     int
	failWrite bool
}

func sampleTimesheet() timesheet {
	item := timesheet{ID: 42, Begin: "2026-11-10T10:00:00+08:00", Duration: 1800, Description: "Review source context."}
	item.Project = &struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}{ID: 7, Name: "Reading"}
	item.Activity = &struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}{ID: 3, Name: "Research"}
	return item
}

// isHTML5Time is whether v is a date and time as Kimai takes them in a write: local, without an offset.
func isHTML5Time(v any) bool {
	text, _ := v.(string)
	_, err := time.Parse("2006-01-02T15:04:05", text)
	return err == nil
}

func newFakeKimai(t *testing.T) *fakeKimai {
	f := &fakeKimai{}
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
		case "/api/timesheets/recent", "/api/timesheets/active", "/api/timesheets":
			if r.Method == http.MethodPost {
				if f.failWrite {
					http.Error(w, "synthetic interrupted write", http.StatusServiceUnavailable)
					return
				}
				var in map[string]any
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in["project"] != float64(7) || in["activity"] != float64(3) || !isHTML5Time(in["begin"]) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				f.mu.Lock()
				f.creates = append(f.creates, in)
				f.mu.Unlock()
				item := sampleTimesheet()
				item.ID = 43
				_ = json.NewEncoder(w).Encode(item)
				return
			}
			if r.URL.Query().Get("user") != "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode([]timesheet{sampleTimesheet()})
		case "/api/users/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "username": "reader", "timezone": "Asia/Shanghai"})
		case "/api/timesheets/42/stop":
			if r.Method != http.MethodPatch || f.failWrite {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			f.mu.Lock()
			f.stops++
			f.mu.Unlock()
			item := sampleTimesheet()
			end := "2026-11-10T11:00:00+08:00"
			item.End = &end
			_ = json.NewEncoder(w).Encode(item)
		case "/api/projects":
			if r.URL.Query().Get("visible") != "1" || r.URL.Query().Get("ignoreDates") != "1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode([]namedResource{{ID: 7, Name: "Reading", Visible: true}})
		case "/api/activities":
			if r.URL.Query().Get("project") != "7" || r.URL.Query().Get("visible") != "1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode([]namedResource{{ID: 3, Name: "Research", Visible: true}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeKimai) app() *app {
	return &app{client: newClient(f.srv.URL, testToken), receipts: newWriteStore("")}
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeKimai(t).app())))
}

func TestManifestAndURL(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "kimai" || len(m.Secrets) != 1 || len(m.Contributes.Connectors[0].ConfirmAlways) != 2 || !slices.Equal(m.Permissions.Network, []string{}) {
		t.Fatalf("%+v", m)
	}
	for _, raw := range []string{"https://kimai.example.com", "http://127.0.0.1:8001/", "https://[::1]:8001"} {
		if _, err := instanceURL(raw); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"http://kimai.example.com", "https://me@example.com", "not-a-url"} {
		if _, err := instanceURL(raw); err == nil {
			t.Fatal("accepted " + raw)
		}
	}
}

func TestCurrentUserTimesheetsAndLookups(t *testing.T) {
	f := newFakeKimai(t)
	a := f.app()
	for _, view := range []string{"recent", "active", "timesheets"} {
		text, err := a.callTool(t.Context(), "kimai_timesheets", json.RawMessage(`{"view":"`+view+`"}`))
		if err != nil || !strings.Contains(text, "Timesheet #42") {
			t.Fatalf("%s %v %s", view, err, text)
		}
	}
	text, err := a.callTool(t.Context(), "kimai_projects", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(text, "Project #7") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "kimai_activities", json.RawMessage(`{"project_id":7}`))
	if err != nil || !strings.Contains(text, "Activity #3") {
		t.Fatalf("%v %s", err, text)
	}
	f.mu.Lock()
	paths := strings.Join(f.paths, "\n")
	f.mu.Unlock()
	if strings.Contains(paths, "user=") || !strings.Contains(paths, "project=7") || !strings.Contains(paths, "full=true") {
		t.Fatal(paths)
	}
}

func TestCreateStopReplayAndValidation(t *testing.T) {
	f := newFakeKimai(t)
	a := f.app()
	args := []byte(`{"project_id":7,"activity_id":3,"begin":"2026-11-10T10:00:00+08:00","end":"2026-11-10T11:00:00+08:00","description":"Review context"}`)
	for range 2 {
		if _, err := a.doAction(t.Context(), "create-1", "kimai_create_timesheet", args); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := a.doAction(t.Context(), "stop-1", "kimai_stop_timesheet", []byte(`{"timesheet_id":42}`)); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.creates) != 1 || f.stops != 1 {
		t.Fatal("receipt replay repeated writes")
	}
	if _, err := describeAction("kimai_create_timesheet", args); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"project_id":7,"activity_id":3,"begin":"2026-11-10 10:00"}`, `{"project_id":7,"activity_id":3,"begin":"2026-11-10T11:00:00Z","end":"2026-11-10T10:00:00Z"}`} {
		if _, err := describeAction("kimai_create_timesheet", []byte(bad)); err == nil {
			t.Fatal("accepted invalid time entry " + bad)
		}
	}
}

func TestPersistentReplayAndUnknownOutcome(t *testing.T) {
	f := newFakeKimai(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	args := []byte(`{"project_id":7,"activity_id":3,"begin":"2026-11-10T10:00:00Z"}`)
	if _, err := a.doAction(t.Context(), "persist-1", "kimai_create_timesheet", args); err != nil {
		t.Fatal(err)
	}
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "persist-1", "kimai_create_timesheet", args); err != nil {
		t.Fatal(err)
	}
	if len(f.creates) != 1 {
		t.Fatal("restart repeated time entry")
	}
	key, _ := json.Marshal(createArgs{ProjectID: 7, ActivityID: 3, Begin: "2026-11-10T10:00:00Z"})
	raw, _ := json.Marshal(map[string]writeReceipt{"persist-1": {Key: "kimai_create_timesheet:" + string(key), Status: "pending", StartedAt: time.Now()}})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.receipts, _ = openWriteStore(filepath.Dir(store.path))
	if _, err := a.doAction(t.Context(), "persist-1", "kimai_create_timesheet", args); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatal("unknown time entry was retried")
	}
}

func TestLocalizedViewsAndStopBinding(t *testing.T) {
	f := newFakeKimai(t)
	a := f.app()
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, ui := range []int{1, 3, 6} {
			for _, mode := range []string{"", "active", "projects", "activities", "create", "review-create"} {
				r := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: ui}}
				st := pageState{Mode: mode, ProjectID: 7, ActivityID: 3, DraftID: "draft-1", Begin: "2026-11-10T10:00:00Z"}
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
	r := &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}}
	v, err := a.page(t.Context(), r, pageState{Mode: "active"})
	if err != nil {
		t.Fatal(err)
	}
	r.Action, r.State, r.Values = "stop", v.State, map[string]string{"id": "42"}
	result, err := a.action(t.Context(), r)
	if err != nil || !result.Pop || f.stops != 1 {
		t.Fatalf("stop did not bind the selected timer: %v %+v", err, result)
	}
	if newApp().status()["state"] != "not_configured" {
		t.Fatal("unconfigured extension was ready")
	}
}

func TestFailedWriteIsUnknown(t *testing.T) {
	f := newFakeKimai(t)
	f.failWrite = true
	if _, err := f.app().doAction(t.Context(), "failed-1", "kimai_stop_timesheet", []byte(`{"timesheet_id":42}`)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatal("failed stop was safe to retry")
	}
}
