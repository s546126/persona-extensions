// Command steam provides read-only Steam data to Persona.
package main

import (
	"context"
	_ "embed"
	"net/http"
	"os"
	"time"

	"github.com/nwverse/persona-extensions/pluginkit"
	"github.com/nwverse/persona/sdk/go/ext"
)

const (
	extensionID = "steam"
	version     = "0.1.0"
	summaryID   = "steam-summary"
)

//go:embed persona-extension.json
var authorManifest []byte

var manifest = pluginkit.PackagedManifest(authorManifest)

type app struct {
	*steamData
	locale string
}

func main() {
	ext.Main(program(steamHTTP(nil, httpTimeout), time.Now))
}

func program(client *http.Client, now func() time.Time) ext.Program {
	a := &app{}
	return ext.Program{
		ID: extensionID, Version: version, Manifest: manifest,
		Init: func(_ context.Context, in *ext.Init, _ *ext.Hub) (map[string]any, error) {
			var cfg config
			if in.Config(&cfg) != nil {
				return nil, invalid("Invalid Steam configuration.")
			}
			dir := os.Getenv("PERSONA_EXTENSION_DATA_DIR")
			if dir == "" {
				dir = in.Info().DataDir
			}
			if dir == "" {
				dir = os.Getenv("PERSONA_EXTENSION_DATA")
			}
			a.steamData = newSteamData(cfg, in.Secrets[keyName], dir, client, now)
			a.locale = in.Hub.Locale
			return nil, nil
		},
		Namespaces: map[string]ext.Handler{"connector": a.connector},
		UI: map[string]ext.Page{
			extensionID: {Render: a.render, Action: a.action},
			summaryID:   {Render: a.card},
		},
	}
}
