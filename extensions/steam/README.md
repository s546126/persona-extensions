# Steam

Steam is a standalone, read-only Persona extension for one configured player's Steam activity. The Personal sidebar page shows the player, two weeks of playtime, the searchable game library, achievements and the public wishlist. The Overview card shows presence, recent playtime and the number of wishlist titles on sale.

Set a SteamID64 or profile vanity name and a Steam Web API key in **Settings › Extensions › Steam**. The optional Store country defaults to `US`. See the [Steam guide](../../docs/extensions/steam.md) for setup, data visibility, assistant tools and cache behavior.

## Token settings

These runtime gates and budgets apply to planned Steam features. Each disabled feature uses zero extra LLM tokens. Local price and playtime observations use the existing Steam reads and are stored under `PERSONA_EXTENSION_DATA_DIR/steam-store-v1`. [Issue #1282](https://github.com/nwverse/persona-extensions/issues/1282)

| Setting | Default | Token impact |
| --- | --- | --- |
| `inbox_events_enabled` | Off | Allows Steam Inbox events that can trigger AI triage. |
| `inbox_max_events_per_day` | 5 (1–50) | Caps daily emissions that can trigger triage. |
| `patch_notes_summary_enabled` | Off | Allows a model to summarize major updates for frequently played games. |
| `sale_roundup_enabled` | Off | Allows sale roundups that can trigger Inbox triage. Also requires Steam Inbox events. |
| `memory_export_enabled` | Off | Allows the gaming profile to use assistant context via `memory_sources`. |
| `memory_max_items` | 10 (1–30) | Caps profile items added to context. |
| `discovery_tools_enabled` | On | Allows high-context game research tools and their skill. |
| `discovery_max_results` | 15 (1–50) | Caps candidates returned per research request. |

## Development

From the repository root, run the focused race tests and build a package.

```sh
go -C extensions/steam test -race ./...
scripts/package.sh steam
```

The package supports Linux and macOS on amd64 and arm64. The tests use synthetic data and local HTTP test servers.
