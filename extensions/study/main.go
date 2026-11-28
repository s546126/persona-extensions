// Command study turns notes into flashcards and schedules their review with FSRS.
//
// The program runs on the hub (`study`) and, when a computer is chosen in the settings, on that computer
// (`study node`), where it reads the Obsidian vault and runs the notebooklm command.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"os"
	"sync/atomic"
	_ "time/tzdata"

	"github.com/nwverse/persona-extensions/pluginkit"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

//go:embed persona-extension.json
var authorManifest []byte

var manifest = pluginkit.PackagedManifest(authorManifest)

var about = func() (out struct{ ID, Version string }) {
	if err := json.Unmarshal(manifest, &out); err != nil {
		panic(err)
	}
	return out
}()

func main() {
	log.SetFlags(0)
	if len(os.Args) > 1 && os.Args[1] == "node" {
		ext.Main(nodeProgram())
		return
	}
	ext.Main(program())
}

func program() ext.Program {
	var m struct {
		Contributes map[string]any `json:"contributes"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		panic(err)
	}
	var current atomic.Pointer[app]
	get := func() (*app, error) {
		a := current.Load()
		if a == nil {
			return nil, errors.New("学习扩展还没准备好")
		}
		return a, nil
	}
	page := func(f func(*app, context.Context, *ext.UIRequest) (*wire.View, error)) func(context.Context, *ext.UIRequest) (*wire.View, error) {
		return func(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
			a, err := get()
			if err != nil {
				return nil, err
			}
			return f(a, ctx, r)
		}
	}
	return ext.Program{ID: about.ID, Version: about.Version, Manifest: manifest,
		Init: func(ctx context.Context, in *ext.Init, hub *ext.Hub) (map[string]any, error) {
			c := defaults()
			if err := in.Config(&c); err != nil {
				return nil, err
			}
			if in.Info().DataDir == "" {
				return nil, errors.New("没有可用的数据目录")
			}
			a, err := openApp(in.Info().DataDir, c, in.Secrets["notion_secret"], hub, in.Hub.Timezone)
			if err != nil {
				return nil, err
			}
			a.notify = hubNotifier(in.Hub.URL, in.Hub.Token, in.Hub.Scopes)
			current.Store(a)
			a.mu.Lock()
			a.changed()
			a.mu.Unlock()
			go a.run(ctx)
			return m.Contributes, nil
		},
		UI: map[string]ext.Page{
			pageID: {Render: page((*app).render), Action: func(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
				a, err := get()
				if err != nil {
					return nil, err
				}
				return a.action(ctx, r)
			}},
			widgetID: {Render: page((*app).renderWidget)},
		},
		Namespaces: map[string]ext.Handler{
			"connector": func(ctx context.Context, method string, params json.RawMessage) (any, error) {
				a, err := get()
				if err != nil {
					return nil, err
				}
				return a.connector(ctx, method, params)
			},
			// The node program's events (node.event) need no answer here.
			"node": func(context.Context, string, json.RawMessage) (any, error) { return map[string]any{}, nil },
		},
	}
}
