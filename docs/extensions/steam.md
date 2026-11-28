# Steam

[Documentation](../README.md) · [Source](../../extensions/steam/README.md)

Steam 0.3.0 researches games in the Store and reads player status, playtime, library, achievements, official game updates and wishlist prices. Open **Personal › Steam** to search and sort the library, review the backlog, or open **Playtime recap**. Game details show achievement progress, local annotations and a link to the Steam Store. **Recent official updates** loads up to five dated Steam announcements when you open it. The Overview card opens the main Steam page. Friends Radar, wishlist alerts and gaming profile memory are optional.

## Sign in with Steam Mobile QR

1. Install Steam on Persona Hub build `202611130452` or later for game research skill delivery. Follow the local installation guide to install a locally built archive. [Install a local package](../installation.md#install-a-local-package) · [Persona v0.4.0](https://github.com/nwverse/persona/releases/tag/v0.4.0)
2. Open **Settings › Extensions › Steam** and select **Sign In**. New installations use QR by default. If you already have a Steam player ID and Web API key, select **Steam Mobile QR** as the sign-in method and save before signing in.
3. Open Steam Guard in the Steam Mobile App, scan the QR code and approve the sign-in on your phone. The extension reads the SteamID of the account you approved. A pending scan expires after five minutes. Start again if the code expires or Steam ends the request. [Steam Guard Mobile Authenticator](https://help.steampowered.com/faqs/view/7EFD-3CAE-64D3-1C31)

QR sign-in can read the signed-in account's private library and wishlist when Steam shares them. Each dataset can be unavailable independently. Presence shows as unknown when Steam omits it. An ambiguous wishlist reply shows as unavailable, and an explicitly empty wishlist shows as empty. Remote account data and observations stay in the signed-in session's memory. The Hub encrypts the refresh token. Signing out clears the session and preserves explicit local annotations.

## Use a Web API key

In **Settings › Extensions › Steam**, choose **Web API key** as the sign-in method and save a **Steam player ID** and **Steam Web API key**. The ID is a 17-digit individual SteamID64 or the vanity name after `/id/` in a Steam profile URL. The Hub encrypts the key. Installations that already have both settings continue in this mode when **Sign-in method** is unset. Clear the saved key in the extension settings to disconnect that configuration. [Steam Web API key settings](https://steamcommunity.com/dev/apikey) · [Steamworks ISteamUser reference](https://partner.steamgames.com/doc/webapi/ISteamUser)

Web API key mode uses read-only calls for publicly visible data. Set the profile and **Game details** to **Public** for the activity, library and achievements you want Persona to see. [Steam profile privacy](https://help.steampowered.com/en/faqs/view/588C-C67D-0251-C276)

In either mode, set **Store country or region** to a two-letter ISO 3166-1 alpha-2 code for Store prices, currency and research budgets. The default is `US`. [ISO 3166 country codes](https://www.iso.org/iso-3166-country-codes.html)

## Assistant tools

The extension advertises these tools through its Steam connector, including to coding Agents. With `memory_sources` routing, Persona uses the curated gaming profile for recall and withholds the extension's connector reads and data-directory skill from ordinary assistant conversations. The `memory_export_enabled` setting controls what the profile shares. The navigation page provides the playtime recap and other account views. [Persona memory source routing](https://github.com/nwverse/persona/blob/main/docs/extensions/extensions.md#memory-sources)

| Tool | Result |
| --- | --- |
| `steam_profile` | Player name, presence and current game, with the owned game count when Steam shares it. |
| `steam_recent_games` | Games played in the last two weeks, lifetime minutes per game and total minutes played in the last two weeks. |
| `steam_library` | Owned games filtered by an optional `query` for title or AppID. Set `sort` to `playtime` (default, highest first) or `name`. |
| `steam_achievements` | Achievement progress for a single numeric `appid`, including unlocked and total counts. |
| `steam_wishlist` | Wishlist titles with current Store prices and discount percentages for the configured country. |
| `steam_achievement_hunter` | Locked achievements sorted by global unlock rate and a timeline of dated personal unlocks for one numeric `appid`. |
| `steam_game_news` | Latest game updates in descending date order for one optional numeric `appid` or a selection of the account's played games. |
| `steam_wishlist_deals` | Current wishlist discounts, target prices met, locally observed lows and limited-time giveaways, with Steam Store links. |
| `steam_backlog_summary` | Library counts by playtime, activity and local status, with optional paged titles. |
| `steam_backlog_mark` | Saves or clears one game's local annotation and returns the current annotation. |
| `steam_tonight_pick` | Ranked owned games with a short reason for each pick. |
| `steam_playtime_recap` | Period playtime, top games, new library entries and local progress changes. |
| `steam_store_tags` | Localized Store tags and IDs when game research is enabled. |
| `steam_store_search` | Store games filtered by tags, price, language or platform when game research is enabled. |
| `steam_game_details` | Store details, Deck compatibility, prices and review summaries when game research is enabled. |
| `steam_friends_radar` | Friends' visible online and in-game activity when Friends Radar is enabled. |
| `steam_coop_overlap` | Shared games for up to five selected friends when Friends Radar is enabled. |

The recent games, library, wishlist and backlog list results accept an `offset` from 0 through 20,000 and a `limit` from 1 through 50, with 20 items by default. Continue with `next_offset` using the same filters and sort. Playtime is returned in minutes. An unavailable dataset reports a reason. A partial or cached result carries a warning when a request fails.

`steam_achievement_hunter` applies the same `offset` and `limit` to each list and provides `next_offset` when another page is available. The personal timeline uses Unix timestamps in seconds and starts with the most recent unlock. Hidden achievements carry `hidden: true`. Their names, API identifiers and descriptions appear only after a dated unlock. Missing achievement metadata yields `hidden: null` and hides those fields. When Steam withholds individual progress, use `global_easiest` and `global_rarest` as reference lists. Personal progress counts reflect what Steam supplies, with `unlocked: null` if it withholds the unlocked count.

`steam_game_news` accepts a `limit` from 1 through 20, with 10 updates by default. Omitting `appid` checks up to `games_limit` games (default 4, range 1–5) ranked by minutes played over the past two weeks. If the recent list is empty or unavailable, Steam ranks played library games by lifetime minutes. The default `official_only: true` uses Steam's official announcements feed and selects updates, patches and DLC using tags and headlines from up to 40 recent posts per game. This can miss posts without recognized tags or headlines. Set `official_only: false` to include other announcements and third-party media. Publication dates are Unix timestamps in seconds. [Steamworks news API](https://partner.steamgames.com/doc/webapi/ISteamNews)

`steam_wishlist_deals` accepts a `limit` from 1 through 50, with 20 by default. It checks the configured region on demand and sorts giveaways first, followed by local lows and larger discounts. A local-low flag means the current quote improved on an earlier local price or matches a low previously set that way. The tool works while Inbox alerts are off and publishes no events.

## Backlog and tonight's picks

**Backlog & local progress** shows the counts for the visible library. Annotated games have status badges. Open a game, choose a status and select **Save status locally**. Existing notes and tags stay with the annotation. In game details, **Clear local annotation** asks you to confirm removal of the status, note and tags.

`steam_backlog_summary` returns counts by default. Pass a `bucket`, `status`, `limit` or `offset` to get a page of titles sorted by name. The `status` filter matches your exact local annotation, including `unplayed`. If you supply both filters, they apply together. Counts cover the analyzed games in the visible library and can overlap.

| Bucket | Included games |
| --- | --- |
| `never_played` | Zero lifetime minutes, excluding games marked `completed` or `dropped`. |
| `tried_under_2h` | One to 119 lifetime minutes. |
| `playing` | Marked `playing` or played within the last two weeks. |
| `long_term` | At least 1,200 lifetime minutes. |
| `queued`, `paused`, `completed`, `dropped` | Games with the corresponding local status. |

`steam_backlog_mark` takes a positive numeric `appid` and one of six statuses, `unplayed`, `playing`, `queued`, `paused`, `completed` or `dropped`. Omit `note` or `tags` to keep its current value. Send an empty string or array to clear that field. Send only `appid` and `clear: true` to remove the whole annotation. Select **Refresh** on the Steam page to see edits made through the assistant.

`steam_tonight_pick` accepts `available_minutes` from 1 to 1,440 (default 60) and a `mood` of `quick_session`, `continue_playing`, `clear_backlog`, `hidden_gem` or `any` (default). Quick picks favor games with recorded playtime within your budget. Continuing favors manually marked or recently active games. Clearing the backlog favors queued and unopened titles. Hidden gems favor games you tried briefly and haven't played in the last two weeks. The ranking uses aggregate playtime as a heuristic for time fit.

Filter tonight's candidates by an exact local `tag` or a title substring in `query`. Both ignore case and surrounding whitespace. The result defaults to five candidates, with a `limit` of 1 to 20. Set `include_completed: true` to consider completed games. Dropped games stay excluded.

## Playtime recap

Open **Personal › Steam › Playtime recap** to switch between **Monthly**, **Quarterly**, **Yearly** and **All observed history**. Only the recap page and `steam_playtime_recap` load the detailed history. The tool accepts `period` (`month`, `quarter`, `year`, or `all`, default `month`) and `top_n` (1–20, default 5).

Calendar periods begin at midnight UTC. The recap compares the first retained observation in the selected period with the current complete library observation. `all` starts at the earliest retained observation. The tool includes the actual `from` and `to` timestamps, so an observation that starts late in the period shows the shorter coverage. Its playtime and ranking use positive per-game minute differences for games in the current library. A newly visible AppID counts as a new library entry. Removals and downward corrections never make playtime negative.

With local annotations available at both ends, the recap reports net counts and games that moved into `completed`, `playing`, `backlog` or `dropped`. The backlog includes games marked unplayed, queued or paused and unmarked games with zero lifetime minutes. The backlog consumption rate is the share of games in that starting group that gained playtime or moved to playing or completed. The rate is null when the starting group is empty. The recap compares observed states at the two endpoints.

Until there is a detailed baseline from an earlier UTC day, you see the current library, two weeks of recent playtime and current local annotation counts. Reopen the recap on another day to track deltas automatically. Older aggregate playtime files remain readable. The detailed observations begin with a complete recap read. The first complete observation each UTC day is eligible to save. The store retains each sampled month's first observation and the latest day, within a 400-day window. Web API key mode writes atomically to `steam-store-v1/playtime-recap.json`. QR mode saves no detail to disk and clears the history on sign-out.

## Gaming profile memory

In **Settings › Extensions › Steam**, enable **Export gaming profile to assistant memory** and select the dimensions Persona may read. The `steam-gaming-profile` memory source is read-only and makes zero outbound calls. It uses already observed games from the configured Web API key account's bounded cache or the active QR session. It can use explicit backlog statuses saved locally for that account. Genres and achievement milestones require data already collected by other Steam reads in the running extension.

| Dimension | Entries |
| --- | --- |
| `playstyle` | Up to three most-played titles, with common genres when Store game details are already available. These short summaries are eligible for standing context. |
| `active_games` | Games marked playing and games played in the last two weeks, with completed and dropped games left out. |
| `backlog_progress` | Aggregate counts for games waiting or paused, playing, completed and dropped. |
| `milestones` | Representative titles with at least 20 hours of play, at least 90% of achievements unlocked when progress is known, or a local completion mark. |

All dimensions are selected by default when export is enabled. The default limit is 20 entries (1–100), with at most 2,048 UTF-8 bytes of entry text across the whole export. Turning export off or clearing the dimensions returns an empty profile, with zero Steam requests and zero extra context tokens. The profile leaves out Steam account IDs, credentials, emails, friend data, notes and tags. On QR sign-out the extension immediately clears its account cache and returns an empty profile. Persona applies its own rules for filtering and refreshing the entries it reads. [Persona memory source protocol and cache](https://github.com/nwverse/persona/blob/main/docs/extensions/extensions.md#memory-sources)

## Game research

| Tool | Arguments and result |
| --- | --- |
| `steam_store_tags` | Find Store tag IDs by name or ID with `query`. Set `language` for localized labels and use `limit` for a shorter list. |
| `steam_store_search` | Combine `query` and up to eight `tags` given as IDs or exact localized or English names. Set `max_price` in the configured currency, `discounted_only`, or `platforms` as needed. All listed platforms are required. `steam_deck` includes Verified and Playable games. Set `language` to require the listed game language and localize results. Sort by `relevance`, `reviews`, `price_asc`, or `release_date`. Use `exclude_owned` to remove titles confirmed in the current library. |
| `steam_game_details` | Pass one `appid` or an `appids` array, with optional `language`. Returns a short Store description, tags, genres, categories, release information, OS and Deck compatibility, controller and play modes, regional prices and discounts, and compact review summaries. |

The Store's display language defaults to the Hub locale where supported. Prices use the configured Store country. A budget of `19.99` means 19.99 in that currency, and `0` finds free offers. Use the formatted price in the answer. Numeric `final` values are in Steam's hundredths of the currency unit. Price may be absent for an unreleased or unpriced title. [Steam language codes](https://partner.steamgames.com/doc/store/localization/languages) · [Steam Deck compatibility](https://partner.steamgames.com/doc/steamhardware/compat)

Search uses a bounded first page from the Steam storefront and verifies its returned prices against the exact budget. A result marked `truncated` is a shortlist from a larger set. When the library or wishlist is unavailable, the corresponding `owned` or `in_wishlist` flag is null. With `exclude_owned` enabled the tool filters confirmed owned titles and preserves candidates whose status is unknown. [Steam storefront search](https://store.steampowered.com/search/?category1=998&deck_compatibility=3%2C2)

The tag directory has an eight-hour fresh cache and built-in common labels in English and Simplified Chinese for outages. Search has a three-second Store request budget and a 512 KiB response cap. Search results are fresh for five minutes. A temporary failure can reuse the same filters' results up to 30 minutes old with a warning and an `as_of` timestamp. An uncached failure reports that search is unavailable. [Steam tag directory](https://store.steampowered.com/tagdata/populartags/english)

Details use Valve's public app metadata and review counts. The review fields contain only all-language Steam-purchase counts and percentages for the overall and rolling 30-day windows, with the Store's sentiment label when available. Percentages are absent for zero reviews. A failed data source can leave partial results with a warning. [Legacy Steam review route](https://partner.steamgames.com/doc/store/getreviews) · [Current Valve reviews API](https://partner.steamgames.com/doc/webapi/IUserReviewsService)

**Steam game discovery** is an optional skill under **Settings › Extensions › Steam › Contributions**. It helps the assistant translate natural language into tags, a search and a comparison of a few games. It starts switched off. Turning off `discovery_tools_enabled` hides the tools, returns a disabled error for direct calls, and withdraws the skill on the Hub's next skill refresh. [Persona skill delivery](https://github.com/nwverse/persona/blob/main/docs/extensions/extensions.md#assistant-content-from-the-data-directory-5770)

## Friends Radar

**Friends Radar** (`friends_radar_enabled`) is off by default. Enable it in **Settings › Extensions › Steam** to read friends' online status and public game libraries. **Personal › Steam** shows up to five friends with visible in-game activity. Only the navigation page and the two opt-in tools request friend data. The disabled setting hides the page section and both tool definitions, so the feature adds zero requests and zero tool definition tokens to assistant context.

| Tool | Inputs and results |
| --- | --- |
| `steam_friends_radar` | `online_only` defaults to `true` and includes friends who are playing, online, away or busy. Set `in_game_only` to `true` to see only friends whose current game is visible. Results rank playing friends first, followed by online friends, away or busy friends, then offline friends and unknown states. `in_game_count` counts visible playing friends across the scanned list. `in_game_now` groups the returned page by game. |
| `steam_coop_overlap` | Provide one `friend_steam_id` (a 17-digit SteamID64), `friend_steam_ids` (1–5 distinct SteamID64 strings), or `friend_query` (the full nickname, case insensitive). IDs must appear in the account's visible friends list. If multiple visible friends share a nickname, use a SteamID64. Games must be shared by you and every selected friend whose library Steam makes visible. The tool sorts by total lifetime minutes across those accounts and reports whether everyone included has played each title. |

Both tools accept `offset` and `limit`, with a default of 20 and a maximum of 50 results. Use `next_offset` with the same filters and friend selection. Friends Radar scans up to 2,000 friends and calls Steam for presence in batches of at most 100.

The overlap tool shows each selected friend's `library_visibility`. A private or unavailable library is omitted from the intersection, with a warning and a `partial` result when other libraries are visible. The tool needs at least one visible friend library and your own library to compute an intersection. Each library read uses up to 20,000 titles.

With `coop_only` enabled by default, the tool keeps Steam Store multiplayer and co-op categories such as **Online Co-op** and **Shared/Split Screen**. Check `multiplayer_categories` for the specific mode. Games with missing or inconclusive categories are included with `coop_support: unknown`. Setting `coop_only` to `false` also includes games that Steam categorizes as single-player only. `shared_count` is the intersection size before filtering. `next_offset` advances through that sorted intersection, so a page may have fewer games than requested or be empty and still have a next page. Each call checks up to 150 candidates in up to three anonymous Store batches.

Steam controls whether the selected sign-in method can see the account's **Friends List**, friends' presence and their **Game details**. Web API key mode uses public data. Mobile QR tries the friend endpoints with the session's access token and explains when Steam restricts access. A visible friends list is required to verify SteamIDs in either mode. A verified SteamID can be used for overlap if QR presence is restricted and Steam still shares the selected libraries. [Steam profile privacy](https://help.steampowered.com/en/faqs/view/588C-C67D-0251-C276) · [Steamworks friend list and player summaries](https://partner.steamgames.com/doc/webapi/ISteamUser#GetFriendList) · [Steamworks owned games](https://partner.steamgames.com/doc/webapi/IPlayerService#GetOwnedGames)

The extension caches friends lists and presence in memory for a 90-second freshness window. QR sign-out clears the session's friend cache. The overlap tool keeps libraries in memory for the current request only. The extension writes zero friend data to its local cache or snapshot files.

## Wishlist Inbox alerts

Set **Steam Inbox events** in **Settings › Extensions › Steam** to opt in. Steam scans once at startup or QR sign-in and every six hours while signed in. It reads only the configured account's visible wishlist and regional Store data. The disabled setting runs zero background scans, produces zero events or Inbox notifications and uses zero extra LLM tokens. The page and assistant tools remain available for on-demand reads.

| Setting | Default | Effect |
| --- | --- | --- |
| `inbox_events_enabled` | Off | Turns on automatic wishlist alerts. The extension assigns the action tier, and Persona may apply its Inbox triage settings. |
| `inbox_max_events_per_day` | 5 (1–50) | Hard cap on new events per Steam account and UTC day, shared across regions. |
| `wishlist_event_triggers` | All five | Choose `discount`, `local_low`, `release`, `demo` and `giveaway`. Target price rules and roundups have their own settings. |
| `wishlist_discount_threshold` | 50% (1–100) | Matches paid discounts at or above this percentage. The giveaway trigger handles 100%-off offers. |
| `wishlist_target_prices` | Empty | Up to 20 comma-separated `AppID:CURRENCY:amount` rules. `123:USD:999` means $9.99 for AppID 123. Use the integer Steam price units reported by `steam_wishlist` and the matching currency. |
| `sale_roundup_enabled` | Off | When at least three wishlist games have paid discounts, replaces their individual price alerts with one roundup. The top five rank new local lows first and larger discounts second. Giveaways, releases and demos have separate alerts. |

One price alert may carry several reasons. A local low requires an earlier observation in the same region and currency, and qualifies only when the price falls below it. Release alerts require an observed transition from upcoming to released. Demo alerts use the demo AppID supplied by the Store. Giveaways require a free-to-keep offer or a 100% discount with a zero final price. Steam distinguishes these offers from permanent free-to-play games and free weekends. [Steamworks Free to Keep](https://partner.steamgames.com/doc/marketing/discounts/freetokeep) · [Steamworks Demos](https://partner.steamgames.com/doc/store/application/demos)

In a sale window, each game and price is announced once. Steam's discount end date can distinguish successive windows. When Steam omits that date, a checked regular price ends the observed window. A roundup stays quiet during the same run of overlapping sales. Demo AppIDs and Steam release timestamps serve as version keys. The UTC daily cap gives giveaways and roundups priority. Qualifying offers can be picked up by a later scan when the cap resets, while one-time price or release transitions can be dropped when the cap is full.

The extension hands recent results to Persona through the memory-only `connector.inbox.collect` call. It sends `connector.changed` with scope `inbox` after new events are recorded. Rows keep stable identifiers for up to 24 hours after detection or until a known offer deadline, whichever comes first. QR sign-out clears the extension's queue and cancels its scan. Web API key mode can recover recent rows and the daily cap from atomically persisted cursors after a restart. [Persona Connector Inbox protocol](https://github.com/nwverse/persona/blob/main/docs/protocol/connector-protocol.md)

## Token settings

Under **Settings › Extensions › Steam**, these settings control what can enter assistant context or trigger triage. Turning off a feature brings its extra token use to zero. Recaps and local snapshots make no model calls. On-demand tool results use tokens when included in a conversation. [Issue #1282](https://github.com/nwverse/persona-extensions/issues/1282)

| Setting | Default | Effect |
| --- | --- | --- |
| `friends_radar_enabled` | Off | Off omits friend data and the friend tool definitions from context and stops friend requests. |
| `inbox_events_enabled` | Off | Off stops background wishlist scans, Inbox events and their token use. |
| `inbox_max_events_per_day` | 5 (1–50) | Caps events per UTC day and account. The effective cap is zero while Inbox events are off. |
| `patch_notes_summary_enabled` | Off | Allows `steam_game_news` to share up to 700 characters per update for summaries. Off uses zero excerpt tokens and returns `summary_enabled: false` with headline metadata. |
| `sale_roundup_enabled` | Off | Requires Inbox events. Off uses zero roundup tokens. Individual alerts follow the Inbox settings. |
| `memory_export_enabled` | Off | Off provides zero entries, makes zero Steam requests and adds zero tokens to context. |
| `memory_export_dimensions` | All four | Choose `playstyle`, `active_games`, `backlog_progress` and `milestones`. An empty selection stops export. |
| `memory_max_items` | 20 (1–100) | Caps profile items within a 2,048-byte text budget. |
| `discovery_tools_enabled` | On | Off hides and disables the research tools and withdraws the skill, with zero research context tokens. |
| `discovery_max_results` | 15 (1–50) | Caps tags and search candidates per call. Details batches use the lower of this setting and 10. |
| Steam game discovery contribution | Off | The optional skill's own switch follows the research gate and Hub delivery rules. |

Token settings use the documented default when a numeric limit is missing or outside its range. The Steam page shows news headlines, dates and links. The backlog tools compute counts and rankings locally on demand and bound their output. Steam collects local observations during normal reads.

## Refresh and storage

Profile metadata, vanity resolution and the owned library are cached for eight hours. The account's presence, recent games and wishlist prices use ten-minute freshness windows. Select **Refresh** to request new Steam data and reload local annotations. The optional friends section uses its own freshness window. Game details fetch achievement progress when opened.

The global unlock rates, achievement schema and news are cached in memory by AppID, with freshness windows of one hour, eight hours and ten minutes respectively. The news page and tool share cached headline results when summary mode is off.

In QR mode, the extension keeps its data cache in memory for the active signed-in account. Web API key mode keeps one bounded record per dataset in memory and in `steam-cache-v1` under the extension data directory (`PERSONA_EXTENSION_DATA_DIR`, or the data directory sent by the Hub). A temporary request failure can reuse a labeled snapshot up to 24 hours old. A definitive denial for a dataset discards its cached record. Automatic retries for a failing dataset wait 30 seconds, and Refresh can retry immediately.

The navigation and tools browse up to 20,000 library or recent entries and price up to 1,000 wishlist items in batches of 50. The wishlist keeps its full item count alongside the number of Store responses received. The card shows a lower bound when some prices remain unchecked and a dash when none could be checked. Titles without a Steam Store price in the chosen region show a price availability message.

### Local snapshots

QR mode keeps Steam observations and Inbox state in memory for the signed-in session and clears them at sign-out. Web API key mode can persist observations and event cursors in separate files under `steam-store-v1`. Price and playtime snapshots reuse data already read from Steam. The extension records new Inbox events before publishing them, and on-demand reads remain available if a snapshot write fails.

Both modes use `PERSONA_EXTENSION_DATA_DIR/steam-store-v1/backlog.json` for explicit personal annotations. The file stores each status, note and set of tags under its SteamID64 and AppID. Signing out preserves the annotations, and they are available again when you use the same Steam account in either mode. A local edit for a vanity name first resolves its current SteamID64. The stores use the Hub's data directory when the environment variable is absent, and fall back to in-memory storage when no directory is available. Versioned files have a 16 MiB limit and are replaced atomically.

| Store | Contents and limits |
| --- | --- |
| Price history | Fresh, checked wishlist prices are scoped to the Steam account, Store country and AppID. Up to 2,000 series retain 32 sampled UTC days each, the latest quote and the first observation of the local low with its discount. A change in Store currency starts a new series. |
| Playtime | A complete library read records lifetime minutes, the two-week minutes supplied by the library, and counts of owned and unplayed games. The aggregate store keeps the latest snapshot on each UTC day for up to 400 days through the most recent observation, across up to 16 accounts. |
| Playtime recap | On-demand observations use per-AppID minutes and status for visible library games. The store keeps the first observation of each month and the latest observation overall. Retention is up to 400 days and up to 16 accounts, within the snapshot file budget. |
| Backlog | Up to 2,000 local annotations, each with a status, a note of up to 240 characters and up to eight tags of 32 characters each. Annotation edits apply to local storage only. |
| Event cursors | The store records the account, region, kind, deduplication key and sale window or version, along with the last observed sale and release states. Up to 5,000 records are retained. Cursors expire after 90 days without a matching observation. |

## Network and credentials

QR sign-in uses Valve's first-party `IAuthenticationService` over HTTPS. The extension begins a `MobileApp` session with `BeginAuthSessionViaQR`, follows Valve's poll interval with `PollAuthSessionStatus`, and renews access with `GenerateAccessTokenForApp`. The extension hands the `s.team` challenge URL to the Hub for local QR rendering. The extension's only network permissions are `api.steampowered.com` and `store.steampowered.com`. [Valve authentication message definitions](https://github.com/SteamDatabase/Protobufs/blob/master/webui/service_authentication.proto) · [Persona extension sign-in protocol](https://github.com/nwverse/persona/blob/main/docs/extensions/extensions.md#sign-in-auth)

The QR grant is a Steam mobile-client session with account privileges determined by Valve. The extension uses it for the read operations below, with friend reads available after opt-in. It sends the access token only in HTTPS requests to `api.steampowered.com`. Backlog analysis and tonight's picks reuse the owned library and recent playtime reads. Web API key mode uses `X-WebAPI-Key` for the authenticated legacy calls at that host and reads the public wishlist anonymously. Store names, prices and categories are fetched anonymously in both modes. The client refuses redirects and reports fixed diagnostic messages. [Steam Web API overview](https://partner.steamgames.com/doc/webapi_overview)

| Read | Steam Mobile QR | Web API key |
| --- | --- | --- |
| Player name, presence and current game | `IPlayerService/GetPlayerLinkDetails` | `ISteamUser/GetPlayerSummaries` |
| Recent playtime | `IPlayerService/GetRecentlyPlayedGames` | `IPlayerService/GetRecentlyPlayedGames` |
| Owned games and lifetime playtime | `IPlayerService/GetOwnedGames` | `IPlayerService/GetOwnedGames` |
| Achievement progress | `IPlayerService/GetAchievementsProgress` | `ISteamUserStats/GetPlayerAchievements` |
| Individual achievement states and unlock times | `IPlayerService/GetUserAchievements` when Steam shares them | `ISteamUserStats/GetPlayerAchievements` |
| Game achievement schema | Public `IPlayerService/GetGameAchievements` | `ISteamUserStats/GetSchemaForGame` with public `GetGameAchievements` fallback |
| Global achievement percentages | Public `ISteamUserStats/GetGlobalAchievementPercentagesForApp` | Same public endpoint |
| Game news | Public `ISteamNews/GetNewsForApp` | Same public endpoint |
| Wishlist | `IWishlistService/GetWishlist` | Public `IWishlistService/GetWishlist` |
| Friends list and presence (opt-in) | `ISteamUser/GetFriendList` and batched `IPlayerService/GetPlayerLinkDetails`, subject to QR permission | `ISteamUser/GetFriendList` and batched `ISteamUser/GetPlayerSummaries` |
| Shared game libraries (opt-in) | `IPlayerService/GetOwnedGames` for each selected account, subject to QR permission | `IPlayerService/GetOwnedGames` for each selected account |

Wishlist names and multiplayer categories use `IStoreBrowseService/GetItems` with the selected Store country and language. The alert scan and the deals tool also request release, demo and purchase metadata in those batches. Prices come from `store.steampowered.com/api/appdetails` for the selected country. [Player message definitions](https://github.com/SteamDatabase/Protobufs/blob/master/webui/service_player.proto) · [Store category message definitions](https://github.com/SteamDatabase/Protobufs/blob/master/webui/common.proto) · [Steamworks IPlayerService](https://partner.steamgames.com/doc/webapi/IPlayerService)

Game research uses anonymous HTTPS GET requests for Store tags, filtered search, app details, tag and Deck metadata, and reviews. Overall reviews try the legacy `/appreviews` route and fall back to `IUserReviewsService/GetAppReviews` when needed. The recent review call uses the current API's explicit date range. Only the ownership and wishlist checks use the account's existing read paths. Public search responses are cached separately from account data, and the wishlist check reads IDs without pulling names or prices.

The Hub encrypts the versioned QR sign-in record through `connector.secrets.*`. It holds the refresh token, SteamID, account name and timestamps. The access token stays in the extension process. The extension makes a rotated refresh token usable after the Hub has saved it.

## Sign out or revoke a QR session

**Sign Out** in Persona cancels the extension's pending sign-in and reads, clears the session's cache and observations, and removes the Hub's saved QR sign-in. Personal backlog annotations remain on the device. For server-side revocation, open Steam's **Authorized Devices** and remove the corresponding Persona session. When Steam confirms that the refresh token is invalid or revoked, Persona clears the session and prompts you to sign in again. [Steam Authorized Devices](https://store.steampowered.com/account/authorizeddevices) · [Steam Support account security](https://help.steampowered.com/en/faqs/view/6639-EB3C-EC79-FF60)

Switching to Web API key mode uses the ID and key from settings. Switching back to QR restores any saved QR sign-in. Use **Sign Out** while QR is selected if you want to clear that record before switching.
