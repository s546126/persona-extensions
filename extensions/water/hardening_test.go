package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestHardeningOlderActionOverflowNoPanic(t *testing.T) {
	now := instant("2026-11-16T10:00:00Z")
	a := testApp(t, defaults(), newHub("", "", nil), now)
	a.s.Records = []record{{ID: "d1", At: now, ML: 250}}

	rawState := fmt.Sprintf(`{"days":7,"offset":%d}`, math.MaxInt-5)
	res, err := a.action(context.Background(), request("older", rawState, nil))
	if err != nil {
		t.Fatalf("older action failed: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("older view failed wire.CheckView: %v", err)
	}
}

func TestHardeningInitNilHubAndReinitStopsPriorGoroutines(t *testing.T) {
	p := program()
	dir := t.TempDir()
	var in ext.Init
	raw := fmt.Sprintf(`{"extension":{"data_dir":%q},"hub":{"timezone":"UTC"}}`, dir)
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Init with nil hub must not panic on subsequent mutating actions that trigger widget invalidation.
	if _, err := p.Init(ctx, &in, nil); err != nil {
		t.Fatalf("first init with nil hub: %v", err)
	}
	res, err := p.UI["water"].Action(ctx, request("drink", "", nil))
	if err != nil {
		t.Fatalf("drink after nil-hub init: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("view invalid: %v", err)
	}

	// Repeated Init calls must cancel previous background goroutines instead of leaking 2 per call.
	before := runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		if _, err := p.Init(ctx, &in, nil); err != nil {
			t.Fatalf("re-init %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+6 {
		t.Fatalf("re-init leaked background goroutines: before=%d after=%d", before, after)
	}
}

func TestHardeningManifestVersionMatchesProgram(t *testing.T) {
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if got := program().Version; got != m.Version {
		t.Fatalf("program().Version = %q, manifest version = %q", got, m.Version)
	}
}

func TestHardeningLongRecordIDAndSequenceRecovery(t *testing.T) {
	now := instant("2026-11-16T10:00:00Z")
	dir := t.TempDir()

	// 1. Persisted record with > 2000-byte ID should be treated as corrupt on load (cannot pass CheckView).
	badPath := filepath.Join(dir, "bad.json")
	badState := state{
		Records: []record{{ID: strings.Repeat("x", 2100), At: now, ML: 250}},
	}
	raw, _ := json.Marshal(badState)
	if err := os.WriteFile(badPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(badPath, now, "UTC")
	if err != nil {
		t.Fatalf("load bad record id: %v", err)
	}
	if len(s.Records) != 0 {
		t.Fatalf("expected oversized record ID to be quarantined as corrupt, got %d records", len(s.Records))
	}

	// 2. Persisted records with sequence < max(dN) must advance Sequence so logDrink does not emit duplicate IDs.
	seqPath := filepath.Join(dir, "seq.json")
	seqState := state{
		Sequence: 0,
		Records:  []record{{ID: "d1", At: now, ML: 200}, {ID: "d5", At: now, ML: 300}},
	}
	raw, _ = json.Marshal(seqState)
	if err := os.WriteFile(seqPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = load(seqPath, now, "UTC")
	if err != nil {
		t.Fatalf("load seq state: %v", err)
	}
	logDrink(&s, now, 250)
	seen := map[string]bool{}
	for _, r := range s.Records {
		if seen[r.ID] {
			t.Fatalf("duplicate record ID %q after logDrink", r.ID)
		}
		seen[r.ID] = true
	}
}

func TestHardeningHubRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sent":true,"reason":"` + strings.Repeat("a", (1<<20)+100) + `"}`))
	}))
	defer srv.Close()

	h := newHub(srv.URL, "tok", []string{"notify"})
	_, err := h.notify(context.Background(), notice{Title: "t"})
	if err == nil {
		t.Fatal("expected oversized response error")
	}
	if !strings.Contains(err.Error(), "too large") && !strings.Contains(err.Error(), "过大") {
		t.Fatalf("expected explicit oversized error, got: %v", err)
	}
}

func TestHardeningSeenMapBounded(t *testing.T) {
	now := instant("2026-11-16T10:00:00Z")
	s := state{Seen: map[string]time.Time{}}
	for i := 0; i < 1500; i++ {
		s.Seen[fmt.Sprintf("r=%d:drink", now.Add(-time.Duration(i)*time.Second).UnixMilli())] = now.Add(-time.Duration(i) * time.Second)
	}
	s.prune(now)
	if len(s.Seen) > 512 {
		t.Fatalf("expected Seen map bounded <= 512 after prune, got %d", len(s.Seen))
	}
}
