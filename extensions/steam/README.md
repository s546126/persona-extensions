# Steam

Steam 0.3.0 researches Store games and reads player status, two weeks of playtime, the searchable game library, achievements, official game updates and wishlist prices. Open **Personal › Steam** to review your backlog, annotate games locally and open a playtime recap. The Overview card opens the same page. Friends Radar, wishlist alerts and gaming profile memory are optional.

New installations use Steam Mobile QR by default. In **Settings › Extensions › Steam**, select **Sign In**, scan with Steam Guard in the Steam Mobile App, and approve the login on the phone. The Hub encrypts the refresh token in its connector secret store. The extension keeps the access token, Steam account data and recap history in memory for the current session. Signing out clears them and preserves your explicit local annotations.

To use the read-only Web API key fallback, select **Web API key** and enter a SteamID64 or profile vanity name and a key. Existing installations with both values use this mode automatically. Steam's privacy settings govern what the key can read. Bounded account caches and recap snapshots can persist on this device. The optional Store country defaults to `US`. [Setup, permissions and sign-out](../../docs/extensions/steam.md)

## Achievements and game updates

- Ask `steam_achievement_hunter` for one game's AppID to find the most commonly unlocked achievements you still need and see your recent unlocks. If Steam supplies only counts or the public unlock rates, the tool shows the progress Steam shares and global rankings. Locked hidden achievements display their hidden status with their names and descriptions withheld.
- Open **Personal › Steam › Recent official updates** to browse the latest headlines for your played games. Ask `steam_game_news` for a specific AppID or omit it to check up to four games by default. The tool uses recent playtime first and falls back to lifetime playtime if the recent list is empty or unavailable. [Tool arguments and limits](../../docs/extensions/steam.md#assistant-tools)

## Backlog and tonight's picks

Open a game from the library to select and save a local status. The **Backlog & local progress** section shows library counts, and annotated games display their status on their rows. Use **Clear local annotation** in game details to remove the status, note and tags.

- `steam_backlog_summary` counts games by lifetime minutes, recent activity and local status. Pass `bucket` or `status` to list matching games in pages of up to 50. Counts can overlap.
- `steam_backlog_mark` sets `status` for one `appid`, with an optional `note` and `tags`. Omitted fields retain their values. Use `clear: true` with the AppID to clear the annotation.
- `steam_tonight_pick` ranks games from your library by `available_minutes` and `mood` and gives a reason for each pick. Filter by local `tag` or title `query`. It returns five candidates by default, up to 20.

Both sign-in modes save explicit annotations by SteamID64 in `PERSONA_EXTENSION_DATA_DIR/steam-store-v1/backlog.json`. Signing out keeps those annotations. Steam account access stays read-only. [Tool arguments and storage details](../../docs/extensions/steam.md#backlog-and-tonights-picks)

## Playtime recap

Open **Personal › Steam › Playtime recap** or call `steam_playtime_recap`. Choose `month` (default), `quarter`, `year`, or `all` observed history, and set `top_n` from 1 to 20 (default 5). The recap shows the actual interval between library observations, added playtime by game, new library entries, and changes in local backlog progress. The first visit shows current library totals, the last two weeks, and the distribution of local statuses. Open the recap on another UTC day to accumulate changes automatically. The recap page loads on demand. [Recap periods and calculations](../../docs/extensions/steam.md#playtime-recap)

## Gaming profile memory

Enable **Export gaming profile to assistant memory** to let Persona use a compact profile built from data already available locally. The four selectable dimensions are game and genre preferences, active games, backlog progress, and milestones. Genre data comes from previous Store game details reads, and achievement milestones use progress already read. The profile contains game titles and aggregate progress. Account identifiers, sign-in data, friends, personal notes and tags stay out of the export. Turning off export or clearing all dimensions returns zero entries, makes zero Steam requests, and uses zero extra context tokens. [Profile limits and Hub routing](../../docs/extensions/steam.md#gaming-profile-memory)

## Game research

Use `steam_store_tags` to find localized Store tags, `steam_store_search` to filter by gameplay, price, sale, language and platform, and `steam_game_details` to compare a shortlist. Search marks games in your library or wishlist when Steam shares those datasets. Details include current regional prices, Deck status, player modes, and overall and 30-day review percentages and counts. [Research arguments and data sources](../../docs/extensions/steam.md#game-research)

The **Steam game discovery** skill is an optional contribution. Enable it under **Settings › Extensions › Steam › Contributions**. Turning off **Steam game research tools and skill** hides the three tools and withdraws the skill from the assistant's next refresh.

## Friends Radar

**Friends Radar** is off by default. Enable it in **Settings › Extensions › Steam** to show **Friends in game** on the Steam page and expose `steam_friends_radar` and `steam_coop_overlap`. While it is off, the extension sends zero friend requests and adds zero friend tool definitions to assistant context.

Web API key mode uses the account's publicly visible friends list and presence. Mobile QR uses the signed-in account's access token and reports any friend endpoints Steam restricts for that session. Shared library results use the friends and game details Steam makes visible to the current sign-in method. Friend lists and presence stay in memory for a 90-second freshness window. QR sign-out clears that cache, and shared library reads use request-local data. [Friends Radar inputs, paging and privacy](../../docs/extensions/steam.md#friends-radar)

## Tools

The connector advertises these tools to coding Agents. The Hub's `memory_sources` routing uses profile entries for assistant conversation recall and withholds the extension's connector reads and data-directory discovery skill in that conversation. Open **Playtime recap** in the Steam page to see the same report from the UI. [Persona memory source routing](https://github.com/nwverse/persona/blob/main/docs/extensions/extensions.md#memory-sources)

| Tool | What it does |
| --- | --- |
| `steam_profile` | Reads visible player presence, current game and library count. |
| `steam_recent_games` | Reads playtime from the last two weeks. |
| `steam_library` | Lists, searches and sorts owned games. |
| `steam_achievements` | Reads unlocked and total achievements for one game. |
| `steam_achievement_hunter` | Ranks locked achievements and lists dated unlocks. |
| `steam_game_news` | Reads recent official game updates, with optional plain-text excerpts. |
| `steam_wishlist` | Lists wishlist titles and regional prices. |
| `steam_wishlist_deals` | Checks deals, target prices, local lows and giveaways on demand. |
| `steam_backlog_summary` | Counts and filters games by progress and activity. |
| `steam_backlog_mark` | Saves or clears a local status, note and tags. |
| `steam_tonight_pick` | Ranks games for a time budget and mood. |
| `steam_playtime_recap` | Compares sampled playtime and local progress by period. |
| `steam_store_tags` | Finds localized Store tags when research is enabled. |
| `steam_store_search` | Finds Store games by tags, budget and platform when research is enabled. |
| `steam_game_details` | Compares genres, features, prices, Deck status and reviews when research is enabled. |
| `steam_friends_radar` | Shows visible friend activity when Friends Radar is enabled. |
| `steam_coop_overlap` | Finds shared games when Friends Radar is enabled. |

## Token settings

The table shows which features can add assistant context or trigger Inbox triage. Turning off a feature brings its extra token use to zero. On-demand recap and local snapshot storage make no model calls. A requested tool response consumes tokens when used in a conversation. [Issue #1282](https://github.com/nwverse/persona-extensions/issues/1282)

| Setting | Default | Token impact |
| --- | --- | --- |
| `friends_radar_enabled` | Off | Off keeps friend data and both friend tool definitions out of context and skips all friend requests. |
| `inbox_events_enabled` | Off | Starts wishlist scans for alerts about prices, releases, demos and limited-time giveaways. Turning it off stops scans, Inbox notifications and their token use. |
| `inbox_max_events_per_day` | 5 (1–50) | Caps new alerts per Steam account and UTC day. |
| `patch_notes_summary_enabled` | Off | Adds plain-text excerpts of up to 700 characters per post for summaries. Off returns headline metadata and uses zero excerpt tokens. |
| `sale_roundup_enabled` | Off | Requires Inbox events. Off uses zero roundup tokens. Individual alerts follow the Inbox settings. |
| `memory_export_enabled` | Off | Off returns an empty profile with zero additional context tokens or Steam requests. |
| `memory_export_dimensions` | All four | Choose `playstyle`, `active_games`, `backlog_progress` and `milestones`. An empty list stops export. |
| `memory_max_items` | 20 (1–100) | Caps exported profile entries. Their text shares a 2,048-byte budget. |
| `discovery_tools_enabled` | On | Off hides the research tools, rejects calls and withdraws the skill with zero research context tokens. |
| `discovery_max_results` | 15 (1–50) | Caps tags and search candidates per call. Details batches also have a ceiling of 10 games. |
| Steam game discovery contribution | Off | The optional skill has its own Contributions switch and follows the research gate and Hub delivery rules. |

Choose the triggers in `wishlist_event_triggers`. The discount threshold defaults to 50%, and `wishlist_target_prices` accepts optional rules such as `123:USD:999`. A local-low alert needs an earlier price in the same currency. Steam checks at startup or QR sign-in and every six hours while alerts are enabled. Each price is announced once per sale window. Releases and demos use their own versions. QR sign-out clears the in-memory observations and Inbox queue. Web API key mode stores these snapshots atomically under `steam-store-v1`. [Wishlist alerts and deduplication](../../docs/extensions/steam.md#wishlist-inbox-alerts)

Use `steam_wishlist_deals` for an on-demand list of deals, target prices met, local lows and giveaways, including when Inbox alerts are off.

## Development

From the repository root, run the focused race tests and build a package.

```sh
go -C extensions/steam test -race -count=1 ./...
go -C extensions/steam vet ./...
./scripts/package.sh steam
./scripts/ci.sh
```

The package supports Linux and macOS on amd64 and arm64. The tests use synthetic data and local HTTP test servers.
