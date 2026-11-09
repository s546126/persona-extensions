# Spaces

A multi-space ontology workbench in the spirit of Muse (Glint) spaces and an EAV (entity-attribute-value) ontology. Every space has four views (动态 activity, 点子 ideas, 目标 goals, 资源库 library) and its own object types with typed fields, statuses and relations; the library's tables, cards and forms are generated from that schema. It ships three template spaces (LifeOS household, deals, cloud hosts), a blank one, an overview card, a widget, two shortcuts, an assistant skill and a local MCP server (`spaces mcp`).

- [Guide](../../docs/extensions/spaces.md)
- [Manifest](persona-extension.json)
- [Assistant skill](skills/spaces/SKILL.md)
- [Development and tests](../../docs/development.md)

```sh
go -C extensions/spaces test ./...
go -C extensions/spaces run . preview --addr 127.0.0.1:8741   # browser preview with the example spaces
go -C extensions/spaces run . mcp --data /tmp/spaces-data      # MCP server on stdio
```

All data is local, in the extension's data directory (`spaces_state.json`). No network, no secrets; the examples are synthetic.
