// Code navigation (docs/extensions/code-nav.md): read the code on your computers in Persona and jump around it the
// way an IDE does: go to a definition, find references, a file's outline, symbols by name. The computer's program
// asks a language server (gopls for Go) and only ever reads.
//
// One program, two roles:
//
//	code-nav hub     the hub's: the 代码 page (hub.go)
//	code-nav node    each computer's: repositories, files and the language servers (node.go, lsp.go)
package main

import (
	_ "embed"
	"encoding/json"
	"os"

	"github.com/nwverse/persona/sdk/go/ext"
)

//go:embed persona-extension.json
var authorManifest []byte

var manifest = packagedManifest(authorManifest)

// Author metadata belongs in the signed market entry, not the runtime manifest.
func packagedManifest(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		panic("invalid embedded extension manifest")
	}
	delete(fields, "tags")
	delete(fields, "replaces")
	delete(fields, "icon")
	out, err := json.Marshal(fields)
	if err != nil {
		panic("cannot encode embedded extension manifest")
	}
	return out
}

func main() {
	var m struct{ ID, Version string }
	_ = json.Unmarshal(manifest, &m)
	role := ""
	if len(os.Args) > 1 {
		role = os.Args[1]
	}
	switch role {
	case "node":
		n := &node{}
		ext.Main(n.program(m.ID, m.Version))
		n.cleanup()
	default:
		ext.Main((&hubApp{}).program(m.ID, m.Version))
	}
}
