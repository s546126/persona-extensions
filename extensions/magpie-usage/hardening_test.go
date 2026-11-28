package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestAccountSecretCreatesMissingDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "magpie-data")
	sec1, err := accountSecret(dir)
	if err != nil {
		t.Fatalf("accountSecret on missing dir failed: %v", err)
	}
	if len(sec1) != 32 {
		t.Fatalf("got secret len %d, want 32", len(sec1))
	}
	sec2, err := accountSecret(dir)
	if err != nil {
		t.Fatalf("second accountSecret failed: %v", err)
	}
	if string(sec1) != string(sec2) {
		t.Fatal("accountSecret did not persist stable secret")
	}
}

func TestFetchQuotasRejectsLocalAndUnspecifiedBypassForRemote(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	blockedRemote := []string{
		"https://localhost.:3425",
		"https://foo.localhost:3425",
		"https://host.persona.internal.:3425",
		"https://0.0.0.0:3425",
		"https://[::]:3425",
	}
	for _, addr := range blockedRemote {
		if _, _, err := fetchQuotas(context.Background(), srv.Client(), addr, true, "secret-key"); err == nil {
			t.Errorf("remote fetchQuotas(%q) succeeded, want error", addr)
		}
	}
	if _, _, err := fetchQuotas(context.Background(), srv.Client(), "https://magpie.example.com", true, "bad\r\nX-Inject: 1"); err == nil {
		t.Error("remote fetchQuotas accepted CRLF in quota key, want error")
	}
	if hits.Load() != 0 {
		t.Fatalf("unexpected HTTP requests sent: %d", hits.Load())
	}
}

func TestProgramVersionMatchesManifestAndConcurrentInitQuery(t *testing.T) {
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	prog := program()
	if prog.Version != m.Version {
		t.Fatalf("program().Version = %q, manifest version = %q", prog.Version, m.Version)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	dataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("PERSONA_EXTENSION_DATA", dataDir)
	makeInit := func(localAddr string) *ext.Init {
		cfgRaw, _ := json.Marshal(map[string]any{"local_address": localAddr})
		return &ext.Init{
			InitializeParams: wire.InitializeParams{
				Config: cfgRaw,
			},
		}
	}
	if _, err := prog.Init(context.Background(), makeInit(srv.URL), nil); err != nil {
		t.Fatalf("initial Init failed: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = prog.Init(context.Background(), makeInit(srv.URL), nil)
		}()
		go func() {
			defer wg.Done()
			_, _ = prog.Namespaces["usage"](context.Background(), "query", json.RawMessage(`{"contribution":"magpie","lang":"en"}`))
		}()
	}
	wg.Wait()
}

func TestInitEmptyLocalAddressFallsBackToDefault(t *testing.T) {
	prog := program()
	dataDir := t.TempDir()
	t.Setenv("PERSONA_EXTENSION_DATA", dataDir)
	in := &ext.Init{
		InitializeParams: wire.InitializeParams{
			Config: json.RawMessage(`{"local_address":"   "}`),
		},
	}
	if _, err := prog.Init(context.Background(), in, nil); err != nil {
		t.Fatalf("Init with blank local_address failed: %v", err)
	}
	// Calling query against defaultMagpieURL fails with unreachable (or succeeds if local Magpie is running),
	// never with "invalid Magpie address".
	_, err := prog.Namespaces["usage"](context.Background(), "query", json.RawMessage(`{"contribution":"magpie"}`))
	if err != nil && strings.Contains(err.Error(), "invalid Magpie address") {
		t.Fatalf("blank local_address did not fall back to defaultMagpieURL: %v", err)
	}
}

func TestParseTimeLocalizedWhitespaceErrorAndPeriodOverflow(t *testing.T) {
	if got := parseTime("  2026-11-27T10:00:00Z \n"); got == nil {
		t.Fatal("parseTime failed on RFC3339 timestamp with surrounding whitespace")
	}
	if got := localized("zh-CN", "en", "zh", "ja"); got != "zh" {
		t.Fatalf("localized(zh-CN) = %q, want zh", got)
	}
	if got := localized("zh-Hans-CN", "en", "zh", "ja"); got != "zh" {
		t.Fatalf("localized(zh-Hans-CN) = %q, want zh", got)
	}
	if got := localized("ja-JP", "en", "zh", "ja"); got != "ja" {
		t.Fatalf("localized(ja-JP) = %q, want ja", got)
	}

	used := 42.0
	if _, ok := parseMagpieWindow(magpieWindow{Period: math.MaxInt64, Used: &used}); ok {
		t.Fatal("parseMagpieWindow accepted math.MaxInt64 period")
	}

	secret, err := accountSecret(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := `{"data":[{"provider":"claude","kind":"subscription","user":"alice","readAt":" 2026-11-27T10:00:00Z ","error":"   ","readError":"\t","windows":[{"name":"5h","period":18000,"used":25}]}]}`
	accs, err := mapQuotas(body, secret, defaultMagpieURL, "zh-CN", mapOptions{})
	if err != nil {
		t.Fatalf("mapQuotas failed: %v", err)
	}
	if len(accs) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accs))
	}
	if len(accs[0].Windows) != 1 {
		t.Fatalf("expected 1 parsed window with padded readAt, got %d", len(accs[0].Windows))
	}
	if accs[0].Notice != "" {
		t.Fatalf("expected empty Notice for whitespace-only errors, got %q", accs[0].Notice)
	}
	_ = os.RemoveAll("")
}
