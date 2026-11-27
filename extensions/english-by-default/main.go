// English by Default (docs/extensions/english-by-default.md): practice writing to coding Agents in English. On the
// computers you pick, this extension installs a UserPromptSubmit hook in Claude Code that catches prompts written in
// another language and, by the mode you pick, holds them back until they are rewritten in English (block), lets
// them through with an English translation (warn), or only records them (log). The pairs stay on that computer.
//
// One program, three roles:
//
//	english-by-default hub     the hub's: the extension's page (hub.go)
//	english-by-default node    each computer's: installs and removes the hook, keeps its heartbeat (node.go)
//	english-by-default hook …  what Claude Code runs on each prompt (hook.go)
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"os"
	"time"

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
	case "hook":
		fs := flag.NewFlagSet("hook", flag.ContinueOnError)
		var a hookArgs
		fs.StringVar(&a.Data, "data", "", "the node program's data directory")
		fs.String("persona-extension", "", "the entry's tag")
		fs.SetOutput(os.Stderr)
		if fs.Parse(os.Args[2:]) != nil {
			return // a hook that cannot read its flags stays silent: the prompt goes through
		}
		runHook(a, os.Stdin, os.Stdout, time.Now())
	case "node":
		n := &node{}
		ext.Main(n.program(m.ID, m.Version))
		n.cleanup()
	default:
		ext.Main((&hubApp{}).program(m.ID, m.Version))
	}
}
