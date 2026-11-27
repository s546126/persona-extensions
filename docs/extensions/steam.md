# Steam

[Documentation](../README.md) · [Source](../../extensions/steam/README.md)

Steam v0.1 reads a configured Steam player's profile, playtime, library, achievements and public wishlist. Open **Personal › Steam** to search the library, sort it by lifetime playtime or name, and open a game to see achievement progress and its Steam Store link. The Overview card opens the same page.

## Configuration

1. Install Steam on your Persona Hub. For a locally built archive, follow [Install a local package](../installation.md#install-a-local-package).
2. In **Settings › Extensions › Steam**, enter a 17-digit individual SteamID64 or the vanity name after `/id/` in the Steam profile URL. The extension resolves vanity names through the Steam Web API. [Steamworks ISteamUser reference](https://partner.steamgames.com/doc/webapi/ISteamUser)
3. Enter your key in the **Steam Web API key** secret field. The Hub encrypts it. [Steam Web API key settings](https://steamcommunity.com/dev/apikey)
4. Set **Store country or region** to a two-letter ISO 3166-1 alpha-2 code for the desired wishlist prices and currency. It defaults to `US`. [ISO 3166 country codes](https://www.iso.org/iso-3166-country-codes.html)

Set Steam profile visibility and **Game details** to **Public** for the activity, library and achievements you want Persona to see. Restricted data shows a visibility message, while available sections remain usable. Steam can return an indistinguishable response for a hidden or empty wishlist. The extension presents this as unavailable unless Steam explicitly returns an empty list. [Steam profile privacy](https://help.steampowered.com/en/faqs/view/588C-C67D-0251-C276)

## Assistant tools

| Tool | Result |
| --- | --- |
| `steam_profile` | Player name, presence and current game, with the owned game count when Steam shares it. |
| `steam_recent_games` | Games played in the last two weeks, lifetime minutes per game and total minutes played in the last two weeks. |
| `steam_library` | Owned games filtered by an optional `query` for title or AppID. Set `sort` to `playtime` (default, highest first) or `name`. |
| `steam_achievements` | Achievement progress for a single numeric `appid`, including unlocked and total counts. |
| `steam_wishlist` | Wishlist titles with current Store prices and discount percentages for the configured country. |

The three list tools accept an `offset` and a `limit` from 1 through 50, with 20 items by default. Continue with `next_offset` using the same search and sort. Playtime is returned in minutes. An unavailable dataset reports a reason. A partial or cached result carries a warning when a request fails.

## Token settings

Under **Settings › Extensions › Steam**, these settings define the defaults and limits for the planned Inbox, AI summaries, memory export and high-context discovery features. A disabled feature uses zero extra LLM tokens. Its gate applies before emitting events, generating summaries, exporting context or running discovery, and its effective item budget is zero where applicable. [Issue #1282](https://github.com/nwverse/persona-extensions/issues/1282)

| Setting | Default | Effect |
| --- | --- | --- |
| `inbox_events_enabled` | Off | Allows Steam to send wishlist price drops, releases, sale roundups and major updates to the Inbox, where triage can use tokens. Turning this off stops all Steam Inbox emissions. |
| `inbox_max_events_per_day` | 5 (1–50) | Caps daily Steam events so they can trigger triage only up to this count. |
| `patch_notes_summary_enabled` | Off | Allows an AI summary of a major update announcement for a frequently played game. Turning this off skips the summary prompt. Inbox delivery also requires the Inbox switch. |
| `sale_roundup_enabled` | Off | Allows wishlist deal roundups during major sales. This also requires `inbox_events_enabled`. Turning either off skips roundup work and its Inbox delivery. |
| `memory_export_enabled` | Off | Allows `memory_sources` to share a personal gaming profile. Turning this off keeps the profile out of assistant context. |
| `memory_max_items` | 10 (1–30) | Caps the number of gaming preference items in the exported profile. |
| `discovery_tools_enabled` | On | Allows high-context game research tools and their skill. Turning this off disables those features before they can add research context. |
| `discovery_max_results` | 15 (1–50) | Caps the candidates that a game search or research request can add to context. |

The runtime uses the documented default when a numeric limit is missing or outside its range. Steam currently provides the five read-only tools above and collects local observations during normal reads.

## Refresh and storage

Profile metadata, vanity resolution and the owned library are cached for eight hours. Presence, recent games and wishlist prices use ten-minute freshness windows. Select **Refresh** to request a new snapshot for all four sections. Game details fetch achievement progress when opened.

The extension keeps one record per dataset in memory and in a `steam-cache-v1` subdirectory of the extension data directory (`PERSONA_EXTENSION_DATA_DIR`, or the data directory sent by the Hub). A temporary request failure can reuse a labeled snapshot up to 24 hours old. A definitive denial for a dataset discards its cached record. Automatic retries for a failing dataset wait 30 seconds, and Refresh can retry immediately.

The navigation and tools browse up to 20,000 library or recent entries and price up to 1,000 wishlist items in batches of 50. The wishlist keeps its full item count alongside the number of Store responses received. The card shows a lower bound when some prices remain unchecked and a dash when none could be checked. Titles without a Steam Store price in the chosen region show a price availability message.

### Local snapshots

Steam keeps local snapshots in four versioned files under `steam-store-v1` inside the extension data directory. Each file has a 16 MiB limit and is replaced atomically. With no data directory the stores keep their state in memory. Snapshot writes happen during existing reads and carry zero additional network or LLM requests. A failed local write leaves the Steam read result available.

| Store | Contents and limits |
| --- | --- |
| Price history | Fresh, checked wishlist prices are scoped to the Steam account, Store country and AppID. Up to 2,000 series retain 32 sampled UTC days each, the latest quote and the first observation of the local low with its discount. A change in Store currency starts a new series. |
| Playtime | A complete library read records lifetime minutes, the two-week minutes supplied by the library, and counts of owned and unplayed games. The latest snapshot on each UTC day is kept for up to 400 days through the most recent observation, across up to 16 accounts. |
| Backlog | Local per-account and per-game annotations support `unplayed`, `playing`, `queued`, `paused`, `completed` and `dropped`. The store holds up to 2,000 entries, with a 240-character note and up to eight tags of 32 characters each. Edits apply to the local annotation store only. |
| Event cursors | Records of successful deliveries are scoped to the account, event kind, deduplication key and sale window or version. Up to 5,000 cursors expire after 90 days. |

## Network and credentials

Account reads go to `api.steampowered.com`. The Web API key is sent in a header over HTTPS for authenticated player calls to that host. The public wishlist and storefront metadata calls use AppIDs or the configured SteamID64. Store price requests go to `store.steampowered.com` with AppIDs and the selected country code. The client refuses redirects and converts upstream failures to fixed messages. Cached records contain parsed response fields and account or region identifiers. [Steam Web API overview](https://partner.steamgames.com/doc/webapi_overview)

The extension uses IPlayerService for games and ISteamUserStats for achievement progress. Wishlist availability comes from Valve's public Wishlist service, with regional pricing from Steam Store AppDetails. [IPlayerService](https://partner.steamgames.com/doc/webapi/IPlayerService), [ISteamUserStats](https://partner.steamgames.com/doc/webapi/ISteamUserStats)
