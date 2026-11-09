// Command spaces is the Persona Spaces extension: a multi-space ontology workbench (docs/extensions/spaces.md).
//
// Like Muse (Glint), each space has four views: 动态 (the activity stream), 点子 (ideas), 目标 (goals) and 资源库 (the
// library). Like an EAV (entity-attribute-value) ontology, each space holds its own ontology: object types with typed fields and a
// status workflow, entities of those types, and relations between them; the library's tables, cards and forms are
// drawn from the schema.
//
// The hub starts the program with no arguments and speaks the extension protocol. `spaces mcp` serves the same data
// to the assistant as an MCP server on stdio; `spaces preview` serves a browser preview.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/nwverse/persona/sdk/go/ext"
)

//go:embed persona-extension.json
var manifest []byte

var about = func() (m struct{ ID, Version string }) {
	_ = json.Unmarshal(manifest, &m)
	return m
}()

const usage = `spaces: Persona Spaces, a multi-space ontology workbench.

  spaces                     the extension (started by the Persona hub; protocol on stdin / stdout)
  spaces mcp [--data DIR]    the MCP server for the assistant on stdin / stdout (tools: space_list,
                             space_query, space_schema_get, space_create, space_schema_upsert,
                             space_entity_upsert, space_entity_delete)
  spaces preview [--addr 127.0.0.1:8741] [--data DIR]
                             a browser preview of the views
  spaces manifest            print persona-extension.json

The MCP server finds the data in --data, else PERSONA_SPACES_DATA, else the directory the hub's
extension last used on this computer.
`

func main() {
	log.SetFlags(0)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			fmt.Print(usage)
			return
		case "mcp":
			if err := runMCP(os.Args[2:]); err != nil {
				log.Fatal(err)
			}
			return
		case "preview":
			if err := runPreview(os.Args[2:]); err != nil {
				log.Fatal(err)
			}
			return
		}
	}
	ext.Main(program(newAppLogged(log.Printf)))
}

func newAppLogged(logf func(string, ...any)) *app {
	a := &app{store: openStore("", logf), logf: logf}
	return a
}

func program(a *app) ext.Program {
	return ext.Program{
		ID: about.ID, Version: about.Version, Manifest: manifest,
		Init: a.init,
		UI: map[string]ext.Page{
			pageUI:   {Render: a.render, Action: a.action},
			cardUI:   {Render: a.card},
			widgetUI: {Render: a.widget},
		},
		Namespaces: map[string]ext.Handler{"shortcut": a.shortcut},
	}
}

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	dir := fs.String("data", "", "the data directory (default: PERSONA_SPACES_DATA, else the hub extension's)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := resolveDataDir(*dir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	m := &mcpServer{store: openStore(data, func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) })}
	return m.serve(ctx, os.Stdin, os.Stdout)
}

func runPreview(args []string) error {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8741", "address to listen on")
	dir := fs.String("data", "", "the data directory (default: a fresh temporary one with the example spaces)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data := *dir
	if data == "" {
		var err error
		if data, err = os.MkdirTemp("", "spaces-preview-"); err != nil {
			return err
		}
	}
	a := &app{store: openStore(data, log.Printf), logf: log.Printf}
	srv := &http.Server{Addr: *addr, Handler: a.previewHandler()}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	fmt.Fprintf(os.Stderr, "spaces preview at http://%s (data %s)\n", *addr, data)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
