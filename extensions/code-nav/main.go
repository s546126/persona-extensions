// Code navigation (docs/extensions/code-nav.md): read the code on your computers in Persona and jump around it the
// way a JetBrains IDE does: go to a definition or an implementation, find usages, quick info, a file's outline,
// symbols by name, Find in Files, back and forward. The computer's program asks the language server of each file's
// language (gopls, typescript-language-server, pyright or pylsp, rust-analyzer, sourcekit-lsp) and only ever reads.
//
// One program, two roles:
//
//	code-nav hub     the hub's: the 代码 page (hub.go)
//	code-nav node    each computer's: repositories, files, Find in Files and the language servers (node.go,
//	                 search.go, lsp.go)
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
