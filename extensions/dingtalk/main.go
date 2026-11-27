// DingTalk runs independently of personad and speaks the official extension protocol.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/nwverse/persona-extensions/extensions/dingtalk/internal/dingtalk"
	"github.com/nwverse/persona-extensions/pluginkit"
	"github.com/nwverse/persona/sdk/go/ext"
)

//go:embed persona-extension.json
var authorManifest []byte

var manifest = pluginkit.PackagedManifest(authorManifest)

var version = func() string {
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		panic(err)
	}
	return m.Version
}()

func program() ext.Program {
	p := dingtalk.Program(version)
	p.Manifest = manifest
	return p
}

func main() {
	if len(os.Args) > 1 && os.Args[1] != "manifest" {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println(version)
			return
		default:
			fmt.Fprintln(os.Stderr, "usage: dingtalk [version|manifest]: started by the Persona hub over stdin / stdout")
			os.Exit(2)
		}
	}
	log.SetFlags(0)
	ext.Main(program())
}
