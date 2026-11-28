package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/spaces/internal/ui"
	"github.com/nwverse/persona-extensions/pluginkit/spaces/kernel"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

type changedStorage struct {
	changed atomic.Bool
}

func (s *changedStorage) View(func(*kernel.Base) error) error              { return nil }
func (s *changedStorage) Update(func(*kernel.Base, time.Time) error) error { return nil }
func (s *changedStorage) Changed() bool                                    { return s.changed.Swap(false) }

func TestHardeningInitNilHubNoPanicOnMutatingAction(t *testing.T) {
	p := program()
	var in ext.Init
	raw, _ := json.Marshal(map[string]any{"extension": map[string]any{"data_dir": t.TempDir()}})
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("unmarshal init: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Passing nil *ext.Hub must not leave a typed-nil Notifier that panics on Act.
	if _, err := p.Init(ctx, &in, nil); err != nil {
		t.Fatalf("Init: %v", err)
	}
	res, err := p.UI[ui.PageUI].Action(ctx, &ext.UIRequest{UIParams: wire.UIParams{
		UIVersion: 4,
		Action:    "create:blank",
		Values:    map[string]string{"name": "测试空间"},
	}})
	if err != nil {
		t.Fatalf("Action create:blank: %v", err)
	}
	if err := wire.CheckView(res.View); err != nil {
		t.Fatalf("CheckView: %v", err)
	}
}

func TestHardeningWatchNilHubNoPanic(t *testing.T) {
	st := &changedStorage{}
	st.changed.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	// Must not panic when hub is nil and st.Changed() returns true.
	watch(ctx, st, nil, 5*time.Millisecond)
}
