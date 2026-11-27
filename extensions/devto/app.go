package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
)

const connectorID = "devto"
const pageUI = "articles"

type app struct {
	mu      sync.RWMutex
	client  *client
	cfg     config
	lastOK  time.Time
	lastErr error
}

func newApp() *app { return &app{client: newClient(apiBase)} }

func (a *app) init(_ context.Context, in *ext.Init, _ *ext.Hub) (map[string]any, error) {
	var cfg config
	if err := in.Config(&cfg); err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	if cfg.MinReactions < 0 {
		cfg.MinReactions = 0
	}
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	return nil, nil
}

func (a *app) config() config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

func (a *app) record(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		a.lastOK, a.lastErr = time.Now(), nil
	} else if !errors.Is(err, context.Canceled) && !errors.Is(err, errNotFound) && !strings.Contains(err.Error(), "invalid") {
		a.lastErr = err
	}
}

func (a *app) status() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := map[string]any{"state": "ok", "account": "DEV Community 公开内容，不用 token"}
	if a.lastErr != nil {
		out["state"] = "error"
		out["error"] = clipText(a.lastErr.Error(), 200)
	}
	if !a.lastOK.IsZero() {
		out["last_sync"] = a.lastOK
	}
	return out
}

func (a *app) connector(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var in struct {
		Contribution string          `json:"contribution"`
		Name         string          `json:"name"`
		Args         json.RawMessage `json:"args"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, &ext.Error{Code: "invalid", Message: "invalid connector parameters"}
		}
	}
	if in.Contribution != "" && in.Contribution != connectorID {
		return nil, ext.NoContribution(in.Contribution)
	}
	switch method {
	case "status":
		return a.status(), nil
	case "tools.list":
		return map[string]any{"tools": readTools}, nil
	case "tools.call":
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		text, err := a.callTool(ctx, in.Name, in.Args)
		if err != nil {
			return nil, err
		}
		return map[string]any{"text": text}, nil
	}
	return nil, ext.UnknownMethod("connector." + method)
}

func main() {
	log.SetFlags(0)
	ext.Main(program(newApp()))
}

var about = func() (out struct{ ID, Version string }) {
	if err := json.Unmarshal(manifest, &out); err != nil {
		panic(err)
	}
	return out
}()

func program(a *app) ext.Program {
	return ext.Program{ID: about.ID, Version: about.Version, Manifest: manifest, Init: a.init,
		UI:         map[string]ext.Page{pageUI: {Render: a.render, Action: a.action}},
		Namespaces: map[string]ext.Handler{"connector": a.connector}}
}
