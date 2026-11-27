package main

import (
	"encoding/json"
	"encoding/xml"
	"io"
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

const testUsername = "reader"
const testPassword = "synthetic-nextcloud-app-password"

type fakeNextcloud struct {
	srv       *httptest.Server
	mu        sync.Mutex
	paths     []string
	searches  []string
	uploads   []string
	failWrite bool
}

func sampleEntry() fileEntry {
	return fileEntry{Href: "/remote.php/dav/files/reader/Notes/reading.md", DisplayName: "reading.md", ContentType: "text/markdown", ContentLength: 45, LastModified: "Tue, 10 Nov 2026 10:00:00 GMT", FileID: 42}
}

func newFakeNextcloud(t *testing.T) *fakeNextcloud {
	f := &fakeNextcloud{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != testUsername || password != testPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.RequestURI())
		f.mu.Unlock()
		switch {
		case r.Method == "SEARCH" && r.URL.Path == "/remote.php/dav/":
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.searches = append(f.searches, string(body))
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			data, _ := xml.Marshal(multistatus{Responses: []fileEntry{sampleEntry()}})
			_, _ = w.Write(data)
		case r.Method == "PROPFIND" && r.URL.Path == "/remote.php/dav/files/reader/Notes/reading.md":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			data, _ := xml.Marshal(multistatus{Responses: []fileEntry{sampleEntry()}})
			_, _ = w.Write(data)
		case r.Method == http.MethodGet && r.URL.Path == "/remote.php/dav/files/reader/Notes/reading.md":
			w.Header().Set("Content-Type", "text/markdown")
			_, _ = w.Write([]byte("# Reading\n\nKeep source context."))
		case r.Method == http.MethodPut && r.URL.Path == "/remote.php/dav/files/reader/Notes/new-note.md":
			if r.Header.Get("If-None-Match") != "*" || f.failWrite {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.uploads = append(f.uploads, string(body))
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNextcloud) app() *app {
	return &app{client: newClient(f.srv.URL, testUsername, testPassword), receipts: newWriteStore("")}
}

func TestConformance(t *testing.T) {
	exttest.Check(t, manifest, exttest.Serve(t, program(newFakeNextcloud(t).app())))
}

func TestManifestAndURL(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", manifest); err != nil {
		t.Fatal(err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "nextcloud" || len(m.Secrets) != 2 || len(m.Contributes.Connectors[0].ConfirmAlways) != 1 || !slices.Equal(m.Permissions.Network, []string{}) {
		t.Fatalf("%+v", m)
	}
	for _, raw := range []string{"https://cloud.example.com", "http://127.0.0.1:8080/"} {
		if _, err := instanceURL(raw); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"http://cloud.example.com", "https://me@example.com", "not-a-url"} {
		if _, err := instanceURL(raw); err == nil {
			t.Fatal("accepted " + raw)
		}
	}
	for _, path := range []string{"../secret", "/etc/passwd", "Notes/../secret", "Notes\\secret", "Notes/%2e%2e/secret"} {
		if validRemotePath(path) {
			t.Fatal("accepted unsafe remote path " + path)
		}
	}
}

func TestSearchReadAndXMLEscaping(t *testing.T) {
	f := newFakeNextcloud(t)
	a := f.app()
	text, err := a.callTool(t.Context(), "nextcloud_search", json.RawMessage(`{"query":"reading <draft> & notes"}`))
	if err != nil || !strings.Contains(text, "reading.md") {
		t.Fatalf("%v %s", err, text)
	}
	text, err = a.callTool(t.Context(), "nextcloud_read_file", json.RawMessage(`{"path":"Notes/reading.md"}`))
	if err != nil || !strings.Contains(text, "Keep source context") {
		t.Fatalf("%v %s", err, text)
	}
	if len(f.searches) != 1 || !strings.Contains(f.searches[0], "reading &lt;draft&gt; &amp; notes") {
		t.Fatal("search query was not XML escaped")
	}
	if !strings.Contains(f.searches[0], "<d:href>/files/reader</d:href>") || !strings.Contains(f.searches[0], "<d:nresults>50</d:nresults>") {
		t.Fatal("search scope or limit is missing")
	}
}

func TestCreateNoteReplayAndNoOverwrite(t *testing.T) {
	f := newFakeNextcloud(t)
	a := f.app()
	args := []byte(`{"path":"Notes/new-note.md","text":"# New note\n\nKeep context."}`)
	for range 2 {
		if _, err := a.doAction(t.Context(), "create-1", "nextcloud_create_note", args); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.uploads) != 1 {
		t.Fatal("receipt replay repeated upload")
	}
	if _, err := describeAction("nextcloud_create_note", args); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"path":"../secret.md","text":"bad"}`, `{"path":"Notes/existing.txt","text":"bad"}`, `{"path":"Notes/new.md","text":""}`} {
		if _, err := describeAction("nextcloud_create_note", []byte(bad)); err == nil {
			t.Fatal("accepted invalid note " + bad)
		}
	}
}

func TestPersistentReplayAndUnknownOutcome(t *testing.T) {
	f := newFakeNextcloud(t)
	a := f.app()
	store, err := openWriteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.receipts = store
	args := []byte(`{"path":"Notes/new-note.md","text":"Persistent note"}`)
	if _, err := a.doAction(t.Context(), "persist-1", "nextcloud_create_note", args); err != nil {
		t.Fatal(err)
	}
	a.receipts, err = openWriteStore(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.doAction(t.Context(), "persist-1", "nextcloud_create_note", args); err != nil {
		t.Fatal(err)
	}
	if len(f.uploads) != 1 {
		t.Fatal("restart repeated note upload")
	}
	keyData, _ := json.Marshal(createArgs{Path: "Notes/new-note.md", Text: "Persistent note"})
	raw, _ := json.Marshal(map[string]writeReceipt{"persist-1": {Key: "nextcloud_create_note:" + string(keyData), Status: "pending", StartedAt: time.Now()}})
	if err := os.WriteFile(store.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.receipts, _ = openWriteStore(filepath.Dir(store.path))
	if _, err := a.doAction(t.Context(), "persist-1", "nextcloud_create_note", args); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatal("unknown note upload was retried")
	}
}

func TestLocalizedViewsAndBounds(t *testing.T) {
	f := newFakeNextcloud(t)
	a := f.app()
	for _, locale := range []string{"en", "ja", "zh-Hans"} {
		for _, ui := range []int{1, 3, 6} {
			for _, mode := range []string{"", "detail", "create", "review-create"} {
				r := &ext.UIRequest{UIParams: wire.UIParams{Locale: locale, UIVersion: ui}}
				st := pageState{Mode: mode, Query: "reading", Path: "Notes/reading.md", DraftID: "draft-1", Text: "Keep context"}
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
	if newApp().status()["state"] != "not_configured" {
		t.Fatal("unconfigured extension was ready")
	}
}

func TestFailedUploadIsUnknown(t *testing.T) {
	f := newFakeNextcloud(t)
	f.failWrite = true
	if _, err := f.app().doAction(t.Context(), "failed-1", "nextcloud_create_note", []byte(`{"path":"Notes/new-note.md","text":"New note"}`)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatal("failed upload was safe to retry")
	}
}
