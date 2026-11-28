package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/nwverse/persona-extensions/extensions/x/internal/serve"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

func TestConcurrentBuildAndPageCallsNoRace(t *testing.T) {
	dir := t.TempDir()
	a := &app{
		findGrok: func(string) string { return "" },
	}
	init := serve.Init{
		DataDir: dir,
		Config:  json.RawMessage(`{"source":"x_api"}`),
	}
	if _, err := a.build(init, nil); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 15 {
				_, _ = a.build(init, nil)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 15 {
				_, _ = a.render(ctx, &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6}})
				_, _ = a.action(ctx, &ext.UIRequest{UIParams: wire.UIParams{Locale: "en", UIVersion: 6, Action: "window:24h"}})
			}
		}()
	}
	wg.Wait()
}
