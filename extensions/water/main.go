// Command water keeps drink records and sends deterministic reminders through the hub.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata"

	"github.com/nwverse/persona-extensions/pluginkit"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

//go:embed persona-extension.json
var authorManifest []byte
var manifest = pluginkit.PackagedManifest(authorManifest)

type app struct {
	mu            sync.Mutex
	cfg           config
	s             state
	path          string
	hub           *hubClient
	now           func() time.Time
	invalidate    func(string) error
	widgetKey     string
	widgetPending string
	widgetWake    chan struct{}
}

func openApp(path string, c config, h *hubClient, now time.Time, hubZone ...string) (*app, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	s, err := load(path, now, hubZone...)
	if err != nil {
		return nil, err
	}
	if err = save(path, s); err != nil {
		return nil, err
	}
	a := &app{cfg: c, s: s, path: path, hub: h, now: time.Now}
	a.widgetKey = a.progressKey(now)
	a.widgetPending = a.widgetKey
	a.widgetWake = make(chan struct{}, 1)
	return a, nil
}

// update commits a private copy, so failed persistence never reports a successful action.
// Callers hold mu.
func (a *app) update(now time.Time, fn func(*state)) error {
	s := a.s.clone()
	fn(&s)
	s.prune(now)
	if err := save(a.path, s); err != nil {
		return err
	}
	a.s = s
	a.refreshWidget(now)
	return nil
}

func program() ext.Program {
	var m struct {
		Version     string         `json:"version"`
		Contributes map[string]any `json:"contributes"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		panic(err)
	}
	var (
		current  atomic.Pointer[app]
		initMu   sync.Mutex
		stopPrev context.CancelFunc
	)
	get := func() (*app, error) {
		a := current.Load()
		if a == nil {
			return nil, fmt.Errorf("喝水提醒还没准备好")
		}
		return a, nil
	}
	return ext.Program{ID: "water", Version: m.Version, Manifest: manifest,
		Init: func(ctx context.Context, in *ext.Init, hub *ext.Hub) (map[string]any, error) {
			c := defaults()
			if err := in.Config(&c); err != nil {
				return nil, err
			}
			if in.Info().DataDir == "" {
				return nil, fmt.Errorf("没有可用的记录目录")
			}
			a, err := openApp(filepath.Join(in.Info().DataDir, "water.json"), c, newHub(in.Hub.URL, in.Hub.Token, in.Hub.Scopes), time.Now(), in.Hub.Timezone)
			if err != nil {
				return nil, err
			}
			if hub != nil {
				a.invalidate = hub.Invalidate
			}
			runCtx, runCancel := context.WithCancel(ctx)
			initMu.Lock()
			if stopPrev != nil {
				stopPrev()
			}
			stopPrev = runCancel
			current.Store(a)
			initMu.Unlock()
			go a.sendWidgets(runCtx)
			go a.run(runCtx)
			return m.Contributes, nil
		}, Shortcuts: map[string]ext.Shortcut{"log_water": func(ctx context.Context, r *ext.ShortcutRequest) (*wire.ShortcutResult, error) {
			a, err := get()
			if err != nil {
				return nil, err
			}
			return a.shortcut(ctx, r)
		}}, UI: map[string]ext.Page{"water": {
			Render: func(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
				a, err := get()
				if err != nil {
					return nil, err
				}
				return a.render(ctx, r)
			},
			Action: func(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
				a, err := get()
				if err != nil {
					return nil, err
				}
				return a.action(ctx, r)
			},
		}, "water-today": {Render: func(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
			a, err := get()
			if err != nil {
				return nil, err
			}
			return a.renderWidget(ctx, r)
		}}}}
}

func (a *app) run(ctx context.Context) {
	t := time.NewTimer(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.pass(ctx); err != nil && ctx.Err() == nil {
				log.Printf("喝水提醒：%v", err)
			}
			t.Reset(30 * time.Second)
		}
	}
}

// pass runs one scheduling tick. The clock is read only after app.mu is taken, so a tick that waited
// behind a page action decides with the time it acts at, not the time it started waiting.
func (a *app) pass(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tick(ctx, a.now())
}

func main() { log.SetFlags(0); ext.Main(program()) }
