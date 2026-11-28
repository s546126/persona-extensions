package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/nwverse/persona-extensions/pluginkit"
	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

//go:embed persona-extension.json
var authorManifest []byte

var manifest = pluginkit.PackagedManifest(authorManifest)

// connectorID is the connector's id: its tools are named chatgpt_*.
const connectorID = "chatgpt"

// endpoints are where it talks to: chatgpt.com's web backend and OpenAI's sign-in. Tests point them at a fake.
type endpoints struct {
	API  string // https://chatgpt.com/backend-api
	Auth string // https://auth.openai.com
}

func liveEndpoints() endpoints {
	return endpoints{API: "https://chatgpt.com/backend-api", Auth: "https://auth.openai.com"}
}

// App is the program's state. Tokens live only here and in the hub's connector secrets.
type App struct {
	ep   endpoints
	http *http.Client
	now  func() time.Time
	// loadTimeout is how long a call waits for the tokens to come from the hub after a start.
	loadTimeout time.Duration
	// answerMost is the most of one ChatGPT answer read (maxAnswer; tests make it small).
	answerMost int64
	// Optional test hooks (set before Start; nil in production).
	beforeCompleteLogin func()
	afterPoll           func()

	mu      sync.Mutex
	ctx     context.Context // the program's, from Init
	hub     *ext.Hub
	text    texts
	dataDir string
	loaded  chan struct{} // closed once the stored tokens were asked for
	tok     *tokens       // nil: not signed in
	expired bool          // the last sign-in stopped working (refresh refused)
	login   *deviceLogin  // the sign-in in progress, if any
	authGen uint64        // monotonic counter for local auth mutations (guards late loadTokens)

	refreshMu sync.Mutex // one refresh at a time: a refresh token works once

	tools   []mcpTool // the connected apps' tools, as ChatGPT listed them last
	toolsAt time.Time

	plugins map[string]pluginDetail // plugin details seen in reads, by id and by name, for the cards
	ledger  *ledger

	// The plugin pages' reads (ui.go): marketplace listings by search, the installed plugins, package hosts logged.
	listMu   sync.Mutex // one listing read at a time
	listings map[string]*listing
	inst     []pluginDetail
	instAt   time.Time
	hosts    map[string]bool
}

func newApp(ep endpoints) *App {
	return &App{
		ep:          ep,
		http:        &http.Client{Timeout: 110 * time.Second}, // HTTPS_PROXY from the environment: the hub's egress
		now:         time.Now,
		loadTimeout: 8 * time.Second,
		answerMost:  maxAnswer,
		text:        zhHans,
		loaded:      make(chan struct{}),
		plugins:     map[string]pluginDetail{},
		ledger:      newLedger(""),
		listings:    map[string]*listing{},
		hosts:       map[string]bool{},
	}
}

func (a *App) program() ext.Program {
	return ext.Program{
		ID:       "chatgpt-apps",
		Version:  manifestVersion(),
		Manifest: manifest,
		Init:     a.init,
		Namespaces: map[string]ext.Handler{
			"auth": ext.AuthHandler(a),
			ext.ConnectorNamespace: ext.Methods{
				"status":           ext.Method(a.status),
				"tools.list":       ext.Method(func(context.Context, wire.ConnectorParams) (wire.ConnectorTools, error) { return readTools(), nil }),
				"tools.call":       ext.Method(whole(a.callRead)),
				"actions.list":     ext.Method(func(context.Context, wire.ConnectorParams) (wire.ConnectorTools, error) { return writeTools(), nil }),
				"actions.describe": ext.Method(a.describe),
				"actions.do":       ext.Method(whole(a.do)),
			}.For(connectorID),
		},
		UI: map[string]ext.Page{uiPlugins: a.pluginsPage()},
	}
}

func manifestVersion() string {
	var m struct{ Version string }
	_ = json.Unmarshal(manifest, &m)
	return m.Version
}

func (a *App) init(ctx context.Context, in *ext.Init, hub *ext.Hub) (map[string]any, error) {
	loaded := make(chan struct{})
	a.mu.Lock()
	a.ctx, a.hub, a.text = ctx, hub, textsFor(in.Hub.Locale)
	a.dataDir = in.Info().DataDir
	if a.dataDir == "" {
		a.dataDir = ext.Env().Data
	}
	a.ledger = newLedger(a.dataDir)
	a.loaded = loaded
	a.mu.Unlock()
	// The stored sign-in comes from the hub (connector.secrets.load), asked once initialize is answered.
	go a.loadTokensWith(ctx, hub, loaded)
	// The connector as the manifest has it, so the hub does not log that the program does not say it has one.
	var m struct {
		Contributes struct {
			Connectors json.RawMessage `json:"connectors"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil || len(m.Contributes.Connectors) == 0 {
		return nil, nil
	}
	return map[string]any{"connectors": m.Contributes.Connectors}, nil
}

func (a *App) getLedger() *ledger {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ledger
}

func (a *App) loadTokens(ctx context.Context) {
	a.mu.Lock()
	hub, loaded := a.hub, a.loaded
	a.mu.Unlock()
	a.loadTokensWith(ctx, hub, loaded)
}

func (a *App) loadTokensWith(ctx context.Context, hub *ext.Hub, loaded chan struct{}) {
	defer close(loaded)
	if hub == nil {
		return
	}
	data, err := hub.LoadSecret(ctx, connectorID)
	if err != nil {
		log.Printf("could not read the stored sign-in: %v", err)
		return
	}
	t, err := decodeSecret(data)
	if err != nil {
		log.Printf("the stored sign-in does not decode; signed out")
		return
	}
	a.mu.Lock()
	if a.authGen == 0 {
		a.tok = t
	}
	a.mu.Unlock()
}

// waitLoaded waits (a little) for the stored sign-in, so a call right after a start does not read as signed out.
func (a *App) waitLoaded(ctx context.Context) {
	a.mu.Lock()
	loaded := a.loaded
	a.mu.Unlock()
	timer := time.NewTimer(a.loadTimeout)
	defer timer.Stop()
	select {
	case <-loaded:
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (a *App) status(ctx context.Context, _ wire.ConnectorParams) (wire.ConnectorStatus, error) {
	a.waitLoaded(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.tok != nil:
		return wire.ConnectorStatus{State: wire.ConnectorStateOk, Account: a.tok.account(), Via: "ChatGPT"}, nil
	case a.expired:
		return wire.ConnectorStatus{State: wire.ConnectorStateNotSignedIn, Error: a.text.expiredError, Guide: []string{a.text.signInGuide}}, nil
	}
	return wire.ConnectorStatus{State: wire.ConnectorStateNotSignedIn, Error: a.text.notSignedIn, Guide: []string{a.text.signInGuide}}, nil
}

// changed tells the hub the sign-in changed by itself: it asks auth.status again (and connector.status on its round).
func (a *App) changed() {
	a.mu.Lock()
	hub := a.hub
	a.mu.Unlock()
	if hub != nil {
		if err := hub.AuthChanged(); err != nil {
			log.Printf("auth.changed: %v", err)
		}
	}
}

func init() {
	log.SetFlags(0)
	log.SetOutput(os.Stderr) // stdout carries the protocol
}
