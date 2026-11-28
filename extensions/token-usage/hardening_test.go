package main

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestHardeningInitNilHubAndReinitStopsPriorLoop(t *testing.T) {
	t.Setenv("PERSONA_HUB_URL", "http://127.0.0.1:1")
	t.Setenv("PERSONA_HUB_TOKEN", "test-tok")

	p := program()
	dir := t.TempDir()
	var in ext.Init
	raw := fmt.Sprintf(`{"extension":{"data_dir":%q},"hub":{"timezone":"UTC"}}`, dir)
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Init with nil hub must not panic when rendering page (which queries GatewayAccounts).
	if _, err := p.Init(ctx, &in, nil); err != nil {
		t.Fatalf("init with nil hub: %v", err)
	}
	v, err := p.UI["token-usage"].Render(ctx, &ext.UIRequest{})
	if err != nil {
		t.Fatalf("render after nil-hub init: %v", err)
	}
	if err := wire.CheckView(v); err != nil {
		t.Fatalf("view invalid: %v", err)
	}

	// Re-Init must cancel prior sync.Loop goroutine instead of leaking one per Init.
	before := runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		if _, err := p.Init(ctx, &in, nil); err != nil {
			t.Fatalf("re-init %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+4 {
		t.Fatalf("re-init leaked sync.Loop goroutines: before=%d after=%d", before, after)
	}
}
