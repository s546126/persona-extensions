package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestHardeningReInitDoesNotPanic(t *testing.T) {
	f := newFakeChatGPT(t)
	h := start(t, f, "zh-Hans")
	in := &ext.Init{}
	in.InitializeParams.Extension = &wire.ExtensionInfo{ID: "chatgpt-apps", Version: manifestVersion(), DataDir: h.data}
	in.InitializeParams.Hub.Locale = "en"
	if _, err := h.app.init(context.Background(), in, h.app.hub); err != nil {
		t.Fatalf("second init failed: %v", err)
	}
}

func TestHardeningEmptyMarketLongSearchLongKeysAndPluginBounds(t *testing.T) {
	f := newFakeChatGPT(t)
	h := start(t, f, "zh-Hans")
	h.signIn()

	// 1. Empty marketplace (no plugins at all) must still produce a CheckView-valid view (no empty section).
	emptySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/backend-api/ps/plugins/installed":
			writeJSON(w, 200, map[string]any{"plugins": []any{}, "pagination": map[string]any{}})
		case r.URL.Path == "/backend-api/ps/plugins/list":
			writeJSON(w, 200, map[string]any{"plugins": []any{}, "pagination": map[string]any{}})
		case r.URL.Path == "/backend-api/ps/mcp":
			writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": []any{}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(emptySrv.Close)
	h.app.ep.API = emptySrv.URL + "/backend-api"
	h.app.clearCaches()
	v := h.page(uiState{}, 3, "")
	if len(v.Sections) == 0 || len(v.Sections[0].Items) == 0 {
		t.Fatalf("empty market produced empty view: %s", dump(v))
	}

	// 2. Huge search query (>4096 chars) must be clipped so Search.Value and State pass CheckView.
	longQ := strings.Repeat("q", 5000)
	res := h.tap(uiState{}, "search", map[string]string{"q": longQ})
	if res.View == nil {
		t.Fatal("nil view for long search")
	}
	if len(res.View.State) > wire.StateMost {
		t.Fatalf("state length %d > %d", len(res.View.State), wire.StateMost)
	}

	// 3. Long/duplicate plugin IDs, blank titles, invalid ShareURL, and >300 skills+apps must pass CheckView.
	longID := "plugin_" + strings.Repeat("x", 200)
	skills := make([]any, 0, 220)
	for i := range 220 {
		name := fmt.Sprintf("skill-%d", i)
		if i == 0 {
			name = "   "
		}
		skills = append(skills, map[string]any{"name": name, "description": "desc"})
	}
	appIDs := make([]string, 0, 150)
	batchApps := make([]any, 0, 150)
	for i := range 150 {
		id := fmt.Sprintf("connector_%s_%03d", strings.Repeat("a", 130), i)
		appIDs = append(appIDs, id)
		batchApps = append(batchApps, map[string]any{"id": id, "name": "", "supported_auth_types": []string{"OAUTH"}})
	}
	bigPlugin := map[string]any{
		"id": longID, "name": "", "share_url": "javascript:alert(1)",
		"release": map[string]any{
			"display_name": "", "description": "desc", "version": "1.0.0",
			"app_ids": appIDs, "skills": skills,
			"interface": map[string]any{"developer_name": "Dev"},
		},
	}
	bigSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/backend-api/ps/plugins/installed":
			writeJSON(w, 200, map[string]any{"plugins": []any{bigPlugin, bigPlugin}, "pagination": map[string]any{}})
		case r.URL.Path == "/backend-api/ps/plugins/list":
			writeJSON(w, 200, map[string]any{"plugins": []any{bigPlugin, bigPlugin}, "pagination": map[string]any{}})
		case strings.HasPrefix(r.URL.Path, "/backend-api/ps/plugins/"):
			writeJSON(w, 200, bigPlugin)
		case r.URL.Path == "/backend-api/ps/apps/batch":
			writeJSON(w, 200, map[string]any{"apps": batchApps})
		case r.URL.Path == "/backend-api/ps/mcp":
			writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": []any{}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(bigSrv.Close)
	h.app.ep.API = bigSrv.URL + "/backend-api"
	h.app.clearCaches()

	// Marketplace with duplicate & >128-byte IDs and blank titles.
	_ = h.page(uiState{}, 3, "")
	// Plugin detail page with >300 apps+skills, >128-byte app IDs, blank skill name, and javascript: ShareURL.
	pv := h.page(uiState{Page: pagePlugin, ID: longID}, 3, "")
	for _, it := range items(pv) {
		if it.Type == wire.ComponentLink && !strings.HasPrefix(it.URL, "https://") {
			t.Fatalf("non-https connect URL leaked: %q", it.URL)
		}
	}
}

func TestHardeningPaginationCyclesStopAndDedup(t *testing.T) {
	f := newFakeChatGPT(t)
	h := start(t, f, "en")
	h.signIn()

	listCalls := 0
	mcpCalls := 0
	cycleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/backend-api/ps/plugins/installed":
			listCalls++
			tok := r.URL.Query().Get("pageToken")
			next := "tok-a"
			if tok == "tok-a" {
				next = "tok-b"
			} else if tok == "tok-b" {
				next = "tok-a"
			}
			writeJSON(w, 200, map[string]any{
				"plugins":    []any{map[string]any{"id": "plugin_1", "name": "p1"}},
				"pagination": map[string]any{"next_page_token": next},
			})
		case r.URL.Path == "/backend-api/ps/mcp":
			mcpCalls++
			var req struct {
				ID     any            `json:"id"`
				Params map[string]any `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			cur, _ := req.Params["cursor"].(string)
			next := "c1"
			if cur == "c1" {
				next = "c2"
			} else if cur == "c2" {
				next = "c1"
			}
			writeJSON(w, 200, map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"tools":      []any{map[string]any{"name": "app.read_it"}},
					"nextCursor": next,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cycleSrv.Close)
	h.app.ep.API = cycleSrv.URL + "/backend-api"
	h.app.clearCaches()

	ps, err := h.app.installed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if listCalls > 3 || len(ps) != 1 {
		t.Fatalf("installed cycle did not stop/dedup: calls=%d len=%d", listCalls, len(ps))
	}

	tools, err := h.app.listTools(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if mcpCalls > 3 || len(tools) != 1 {
		t.Fatalf("listTools cycle did not stop/dedup: calls=%d len=%d", mcpCalls, len(tools))
	}
}

func TestHardeningShortExactRuneBoundAndPartialDirIgnored(t *testing.T) {
	if got := short("一二三四五", 3); utf8.RuneCountInString(got) > 3 {
		t.Fatalf("short(5 runes, 3) = %q (%d runes > 3)", got, utf8.RuneCountInString(got))
	}
	if got := short("hello", 0); got != "" {
		t.Fatalf("short(hello, 0) = %q", got)
	}
	if got := short("hello", -2); got != "" {
		t.Fatalf("short(hello, -2) = %q", got)
	}

	// pluginSkill must ignore stray .partial directories when picking the latest installed version.
	f := newFakeChatGPT(t)
	h := start(t, f, "en")
	h.signIn()
	goodDir := filepath.Join(h.data, "plugins", "example-notes", "1.2.0", "skills", "notes")
	if err := os.MkdirAll(goodDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goodDir, "SKILL.md"), []byte("good skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Lexicographically greater than "1.2.0"!
	partialDir := filepath.Join(h.data, "plugins", "example-notes", "9.9.9.partial")
	if err := os.MkdirAll(partialDir, 0o755); err != nil {
		t.Fatal(err)
	}
	text, err := h.tool("chatgpt_plugin_skill", map[string]any{"plugin": "example-notes", "skill": "notes"})
	if err != nil || !strings.Contains(text, "good skill") {
		t.Fatalf("pluginSkill picked .partial dir: %q %v", text, err)
	}

	// extract must reject tar entry named "." with TypeReg.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: ".", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	if err := extract(bytes.NewReader(buf.Bytes()), filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("extract accepted regular file named '.'")
	}
}
