// Command steam provides read-only Steam data to Persona.
package main

import (
	"context"
	_ "embed"
	"log"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/nwverse/persona-extensions/pluginkit"
	"github.com/nwverse/persona/sdk/go/ext"
)

const (
	extensionID = "steam"
	version     = "0.3.0"
	summaryID   = "steam-summary"
)

//go:embed persona-extension.json
var authorManifest []byte

var manifest = pluginkit.PackagedManifest(authorManifest)

type app struct {
	*steamData
	publicData *steamPublicData
	qr         *qrAuth
	backlog    *BacklogStore
	discovery  *steamDiscovery
	locale     string
}

func main() {
	ext.Main(program(nil, time.Now))
}

func program(client *http.Client, now func() time.Time) ext.Program {
	return (&app{}).program(client, now)
}

func (a *app) program(client *http.Client, now func() time.Time) ext.Program {
	if client == nil {
		client = steamHTTP(nil, httpTimeout)
	} else {
		client = steamHTTP(client.Transport, client.Timeout)
	}
	return ext.Program{
		ID: extensionID, Version: version, Manifest: manifest,
		Init: func(ctx context.Context, in *ext.Init, hub *ext.Hub) (map[string]any, error) {
			var cfg config
			if in.Config(&cfg) != nil {
				return nil, invalid("Invalid Steam configuration.")
			}
			if in.Active != nil && !slices.Contains(in.Active["connectors"], extensionID) {
				cfg.InboxEventsEnabled = false
			}
			dir := os.Getenv("PERSONA_EXTENSION_DATA_DIR")
			if dir == "" {
				dir = in.Info().DataDir
			}
			if dir == "" {
				dir = os.Getenv("PERSONA_EXTENSION_DATA")
			}
			a.steamData = newSteamData(cfg, in.Secrets[keyName], dir, client, now)
			a.publicData = &steamPublicData{client: steamClient{http: client, key: a.client.key}, now: now}
			a.backlog = NewBacklogStore(dir)
			a.locale = in.Hub.Locale
			a.discovery = nil
			if cfg.AllowsDiscoveryTools() {
				a.discovery = newSteamDiscovery(client, now)
			}
			if err := syncDiscoverySkill(dir, cfg.AllowsDiscoveryTools() && slices.Contains(in.Active["skills"], discoverySkillID)); err != nil {
				log.Print("steam: could not update the game discovery skill")
			}
			if a.config.AuthMode == modeQR {
				a.qr = newQRAuth(ctx, hub, client, now, a.config, a.locale)
			} else {
				a.steamData.startWishlistInbox(ctx, hub, textFor(a.locale))
			}
			return nil, nil
		},
		Namespaces: map[string]ext.Handler{"connector": a.connector, "auth": ext.AuthHandler(a), "memory": a.memoryEntries},
		UI: map[string]ext.Page{
			extensionID: {Render: a.render, Action: a.action},
			summaryID:   {Render: a.card},
		},
	}
}
