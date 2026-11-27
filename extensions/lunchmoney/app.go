package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
)

const connectorID = "lunchmoney"
const pageUI = "finance"

type app struct {
	mu      sync.RWMutex
	client  *client
	lastOK  time.Time
	lastErr error
}

func newApp() *app { return &app{client: newClient("")} }

func (a *app) init(_ context.Context, in *ext.Init, _ *ext.Hub) (map[string]any, error) {
	if strings.TrimSpace(in.Secrets["api_token"]) == "" {
		return nil, nil
	}
	a.mu.Lock()
	a.client = newClient(in.Secrets["api_token"])
	a.mu.Unlock()
	return nil, nil
}

func (a *app) currentClient() *client {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.client
}

func (a *app) record(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		a.lastOK, a.lastErr = time.Now(), nil
	} else {
		a.lastErr = err
	}
}

func (a *app) status() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	state := "ok"
	if a.client.token == "" {
		state = "not_configured"
	}
	out := map[string]any{"state": state, "account": "Lunch Money 个人账务"}
	if a.lastErr != nil {
		out["error"] = clipText(a.lastErr.Error(), 200)
		switch {
		case errors.Is(a.lastErr, errToken):
			out["state"] = "not_signed_in"
		case errors.Is(a.lastErr, errRateLimited):
			out["state"] = "rate_limited"
		case state == "ok":
			out["state"] = "error"
		}
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
	case "actions.list":
		return map[string]any{"tools": []tool{}}, nil
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
