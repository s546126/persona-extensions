// Command volc is the Volcengine speech extension: meeting transcription on the hub and spoken meeting answers with
// the owner's Volcengine (Doubao Speech) key. It is the official package of the hub's former built-in `volc`
// extension, with the same id, contributions, secret and settings (less the built-in's meetings switch).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"

	"github.com/nwverse/persona-extensions/extensions/volc/internal/volc"
	"github.com/nwverse/persona-extensions/pluginkit"
)

//go:embed persona-extension.json
var authorManifest []byte

// manifest is the runtime manifest: without the source-only tags, replaces and icon.
var manifest = pluginkit.PackagedManifest(authorManifest)

var version = func() string {
	var m struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(manifest, &m)
	return m.Version
}()

func main() {
	if err := volc.Run(context.Background(), version, manifest); err != nil {
		fmt.Fprintln(os.Stderr, "volc:", err)
		os.Exit(2)
	}
}
