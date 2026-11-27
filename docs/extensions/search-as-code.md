# Search as Code (AI 聊天记录搜索)

[Documentation](../README.md) · [Source](../../extensions/search-as-code/)

The extension is shown to users as **AI 聊天记录搜索**: it finds things in the user's earlier Claude Code and Codex chats (for example "how did I fix that 429 error last time?"). The page, the settings and the overview card speak plain Chinese; the pipeline syntax below is on the page's advanced tab (`高级：怎么搜的`) only.

Search as Code brings Perplexity's *Rethinking Search as Code Generation* paradigm and CASS-style coding-agent session search into Persona. Instead of issuing isolated single-turn keyword lookups, a multi-step investigation is compiled into and executed as a deterministic search program (`SearchProgram`) over local coding-agent transcripts (`~/.claude/projects/**/*.jsonl`, `~/.codex/sessions/**/*.jsonl`, and configured directories) and local code repositories.

Every run produces:

1. **The executable Search-as-Code program (`CodeListing`)**: a composable pipeline of source, filtering, correlation, aggregation, and ranking operators connected by `|>`.
2. **A transparent step execution trace (`StepTraces`)**: the input count, output count, execution time, and summary note for each operator in the pipeline.
3. **Role-attributed evidence cards (`Hits` and `Groups`)**: ranked sessions and files with `User`, `Assistant`, `Error`, and `File` line-numbered snippets, `Without tools` conversational noise reduction, and cross-session file/project correlations.

With no other computer picked, all indexing and execution happen on the hub's computer; the extension calls no outside service and requires no secrets. Restoring an archived session runs the `disklessly` CLI of the computer that holds it (see below). Other computers are reached only through Persona's own node connection (see below).

## Searching other computers (#519)

Coding-agent sessions are usually spread over several computers. Pick them in the `machines` setting (`要搜哪些电脑`, an array of `format: machine`, macOS and Linux). The hub then runs this extension's program on each of them (the manifest's `node` section, `search-as-code node`, `keep_running: lease`, file access only), and a search goes to all of them at once over `nodes.call`:

- **Fan-out.** The hub compiles the program once and sends it to every picked computer that is online (`sac.search`, 20 s each). Each computer runs the whole pipeline over its own transcripts and text indexes, including `exclude_tools()`, and answers with the ranked evidence only (snippets, never a transcript). The hub's own computer is searched as before, directly; when it is among the picked ones (the hub lists it as kind `local` with the hub's host name) it is searched only that way, once, shown under its name (`Studio（hub）`, which the page shows as `Studio（主电脑）`), and its id, name and key reach it as well as `hub` (unless a picked computer has that key). Every computer runs the program with the merged result's `limit` (100 when the program has none), and the hub cuts what each answers to it as well. The results are merged: each session once (a session both on the hub's computer and a picked one is shown under the picked one), ranked by score, cut to the program's `limit`, and grouped again.
- **Source computer.** Every hit carries `machine` (the hub's machine id; empty for the hub's own computer), `machine_name` and, for remote hits, an id `m:<machine>/session:<id>`. The page shows the computer on each hit and on the detail page; `machines` in the result says how each computer answered (`local`, `online`, `cached`, `unavailable`), shown as a table on the advanced tab (`高级：怎么搜的`), where the trace starts with a `fan_out` step and ends with a `merge` step, and each pipeline step's counts are summed over the computers. `scan_sessions(machine=…)`, `where(machine=…)` and the inline token `machine:<id, name or key>` (also `host:`) narrow a search to the computers named; `group_by(field="machine")` and `group:machine` aggregate by computer. The sources tab lists the picked computers, their state and sync time; a row searches that computer.
- **Lightweight index for offline computers.** Every 15 minutes (and from the sources tab on demand) the hub fetches the sessions changed on each online computer since the last sync (`sac.index`, pages of 60 sessions ordered by update time and id). The index is the plain-text conversational format the disklessly text index uses: metadata plus `user` / `assistant` turns only, no tool calls or outputs, each turn clipped to 280 characters, the first 20 and the latest 60 turns of a long session. The hub keeps each computer's newest 300 sessions in `<data>/machines/<hash>.json`. A computer that is offline or does not answer is searched in that copy; its hits are marked `cached` (`离线副本` on the page), and a computer with nothing synced yet is reported `unavailable`.
- **Restore on the computer that holds the session.** An archived (`已上云`) hit from another computer is restored there (`sac.restore`), from the page or the assistant's `POST /restore/{machine}/{entry}`. Only a computer picked at that moment is asked (an action drawn before it was removed from the setting is refused), and the computer restores only an entry of its own indexes.

Without any computer picked, nothing changes: a search is the single-computer one, with the same result shape.

## The assistant: approval before reading other computers and projects

Reading session history is a privacy boundary: transcripts on other computers or of unrelated projects may hold logs, credentials echoed in error output, or private work. So nothing reads them silently:

- The MCP server's `allow` holds only `sac_recipes_list`. `sac_execute` and `sac_session_search`, which read every project of the assistant's computer, follow the conversation's permission policy (asked on the phone by default; *Always allow in this session* remembers the tool).
- Other computers are searched through the assistant command `sac` (`contributes.assistant` `sessions`: the `sac-remote` skill and a command gate, `assistant/gate.json`), which calls this extension's API at the hub. The gate allows `GET /machines`, `GET /projects/{machine}` and `GET /recipes` (computers, project names and recipes), **confirms** `GET /search/{machine}/{project}/{scope}` and `POST /restore/{machine}/{entry}`, and denies everything else. The token is the extension's own (hub scopes `machines.read` and `extensions.call:search-as-code`); the gate's proxy adds it, so the assistant never sees it.
- The route names the target, and the program reads exactly that: one computer key (or `hub`, or `all`), one project key (or `all`) and one scope. A pipeline's `code`, inline `scope:` / `project:` tokens or a recipe cannot widen it: every `scan_sessions` step is pinned to the route's scope and project, and every other operator (`correlate`'s candidates, `where`, `exclude_tools`) sees only that project's sessions; machine filters in the code (`machine=`, `machine:`) are dropped, since the route's computers are the ones searched; `scan_files` (the hub's repositories) is dropped too. The approval card shows the target computers (offline ones marked as searched in their synced index), the project, the scope and the query: the extension answers `assistant.summary` with these names.
- *Always allow in this session* on the card remembers the route (the gate's `AlwaysKeys`, `METHOD /path` without the query), so the user approves a computer / project / scope once per conversation, and the next query on the same target does not ask again; another computer, project or scope asks again. The gate turns a path segment with a digit or longer than 24 characters into a wildcard, so machine and project keys contain neither: lowercase letters and `-` only, a digit `d` written as `z` plus the d-th letter (`Laptop 2` → `laptop-zc`, `z` itself → `zz`), with a five-letter hash for names with other characters or longer than 24, so a key stays one remembered segment. A project name the key does not spell out exactly (dots, underscores, spaces, capitals: `acme.web`, `acme_web`, `Acme Web`) gets a hash of the exact name, so two projects never share a key, and a key that two projects of a computer still answer to reads neither. Computer keys are unique among the computers: a name two of them share uses the id, and a key taken already gets a hash of the full id. `GET /machines` and `GET /projects/{machine}` list the keys.
- A restore is confirmed every time it is asked for.

## Sessions moved to the cloud (已上云)

When the disklessly CLI moves a transcript of this computer to the cloud and deletes the local file, it first writes a plain-text index of that session to `<stateDir>/sessions/text/<entryId>.jsonl`. The index is never uploaded. Its first line is a header (`"type":"disklessly-archive"`, `version` 1, `entryId`, `sessionId`, `tool`, original `path`, `cwd`, `title`, `firstPrompt`, `mtime`, `archivedAt`, ...); each following line is one user or assistant message (`{"type":"user"|"assistant","timestamp","text"}`) with tool calls, reasoning and injected blocks already dropped.

`scan_sessions` reads these indexes as a second session source:

- Directories: `DISKLESSLY_STATE_DIR/sessions/text`, `$XDG_DATA_HOME/disklessly/sessions/text` (default `~/.local/share`), `~/Library/Application Support/Disklessly/sessions/text` (or the legacy `256isenough` folder), each under both `$HOME` and the account's home from the user database (which the CLI uses on Linux when the two differ), plus the `cold_text_dirs` setting.
- Each index becomes a session with `user` / `assistant` turns only (no tool turns), the original path, entry id, archive time, harness (`claude` → `claude-code`) and project (from `cwd`).
- An index whose original path exists again is a restored session and is skipped; its live transcript is scanned instead, as `本地`, even outside the session directories. An index whose original path or session id matches a live transcript is skipped too, so nothing is counted twice.
- Cost is bounded by the scan limits below (1000 archived indexes, 4 MiB per index, 96 MiB in all), with each message clipped to 4000 characters.

Session hits carry `location` (`本地`, `已上云` or `未确认上云`); archived hits also carry `entry_id` and `original_path`. An index whose header still has an empty `segment` or `archivedAt` (the CLI writes it before the upload and fills these in once the segment is stored) is `未确认上云`: it stays searchable but is not offered for restore, because a cloud copy is not confirmed. The page shows an `已移到云端` badge (`可能没传完` for an unconfirmed index; the data keeps `已上云` / `未确认上云`) and a `恢复` row action (also on the hit's detail page) that asks `从云端恢复到原路径 <path>？恢复后可运行 claude --resume <sessionId>` (the resume hint only for Claude Code and Codex sessions). The restore runs in the background, because the hub waits at most 20 seconds for a page action: the page answers at once, shows the restore as `恢复中`, and lists the outcome in a `会话恢复` section when the CLI finishes. It runs `disklessly --json [--state-dir <dir>] sessions restore <entryId>` with a 5-minute timeout; on timeout the CLI's whole process group is killed. `--state-dir` is passed when the index's state directory is not the CLI's default. A per-entry lock in the extension's data directory keeps the page and the MCP server from restoring the same entry twice at once. The CLI is found through `DISKLESSLY_BIN`, `PATH`, `~/.disklessly/bin/disklessly`, `~/.local/bin/disklessly` or `/Applications/Disklessly.app/Contents/MacOS/disklessly`. Success shows the resume command (`claude --resume <id>` or `codex resume <id>`); exit status 4 shows `云端不可达，稍后再试`. Only entries present in this computer's indexes can be restored; the lookup opens `<entryId>.jsonl` directly, so it does not depend on the scan limits. The step trace, the result stats and the sources tab show how many archived sessions were scanned.

Scan limits: at most 1000 archived indexes per scan (newest first; restored indexes do not count), at most 4 MiB parsed per index (a longer one is cut, not dropped) and 96 MiB of index data in all; archived indexes left out are reported as over the limit (`太多没搜` on the sources tab). Restored transcripts outside the session directories are scanned as local, at most 20 of them and, when the 80-file live window is full, only those at least as new as it.

## Pipeline operators

| Operator | Arguments | Behavior |
| --- | --- | --- |
| `scan_sessions` | `query`, `scope` (`conversation`, `all`, `user`, `assistant`, `errors`), `project`, `harness`, `model`, `since`, `machine` | Scans recent Claude Code, Codex, and custom JSONL transcripts and the text indexes of sessions moved to the cloud (`已上云`), extracting turn-level snippets, error classifications, and touched file paths. |
| `scan_files` | `pattern`, `glob` | Scans configured local repository directories (`repo_dirs`) for matching lines and file paths. |
| `where` | `project`, `harness`, `model`, `role`, `since`, `has_error`, `machine` | Filters the current candidate set by metadata predicates or snippet role. |
| `exclude_tools` | none | CASS-style `Without tools` filter: strips noisy intermediate `tool_use` and `tool_result` JSON dumps, keeping conversational (`User` / `Assistant`) and `Error` turns. |
| `match_regex` | `pattern` | Filters hits and snippets against a case-insensitive regular expression. |
| `correlate` | `by` (`file`, `project`, `branch`) | Cross-session and cross-source join: links each hit to other sessions or repository files that touched the same file, project, or branch. |
| `dedup` | `by` (`session`, `file`, `title`) | Deduplicates the candidate set by session ID, primary file path, or normalized title. |
| `group_by` | `field` (`project`, `harness`, `model`, `role`, `error_kind`, `machine`) | Aggregates hits into summary buckets with total counts, error counts, and sample titles. |
| `rank` | `by` (`relevance_freshness`, `freshness`, `errors_first`) | Scores and sorts hits by term match density, conversational weight, correlation bonus, error count, and recency decay. |
| `limit` | `n` (1 to 100, default 25) | Truncates the ranked hit list to the top `n` results. |

### Natural-language to Search-as-Code compilation

When given a natural-language query (with optional inline tokens such as `project:persona`, `harness:claude-code`, `model:claude-opus-5-5`, `since:7d`, `machine:laptop`, `regex:panic|timeout`, `file:*.go`, `group:model`, `correlate:file`, `without:tools`), the compiler automatically synthesizes a multi-step pipeline:

```text
scan_sessions(query="quota cooldown", scope="conversation", project="persona")
|> exclude_tools()
|> correlate(by="file")
|> group_by(field="project")
|> rank(by="relevance_freshness")
|> limit(n=25)
```

## Built-in recipes

| ID | Name | Default scope | Pipeline |
| --- | --- | --- | --- |
| `error-triage` | 最近的报错 | `errors` | `scan_sessions(scope="errors") \|> exclude_tools() \|> correlate(by="file") \|> group_by(field="project") \|> rank(by="errors_first") \|> limit(n=25)` |
| `cross-session-context` | 以前是怎么定的 | `conversation` | `scan_sessions(scope="conversation") \|> exclude_tools() \|> correlate(by="project") \|> dedup(by="session") \|> group_by(field="project") \|> rank(by="relevance_freshness") \|> limit(n=25)` |
| `model-harness-audit` | 各个模型都做了什么 | `all` | `scan_sessions(scope="all") \|> correlate(by="file") \|> group_by(field="model") \|> rank(by="freshness") \|> limit(n=30)` |

Custom recipes can be created, edited, and deleted from the page or through the MCP server.

## The page

| Contribution | What it is (in plain words) | Behavior |
| --- | --- | --- |
| `ui` `search-as-code` (`nav_page`) | 页面「AI 聊天记录搜索」 | Search box, content filter `搜哪些内容` (`conversation` 只看对话, `all` 全部, `user` 只看我说的, `assistant` 只看助手说的, `errors` 只看报错), tabs (`结果` with an intro `这是做什么的` until the first search, `常用搜索` for recipes, `搜哪些地方` for sources and computers, `高级：怎么搜的` for the program, step trace, pipeline editor and history), hit detail view, and recipe editor. |
| `ui` `search-as-code-card` (`overview_card`) | 概览卡片「AI 聊天记录搜索 · 概况」 | Titled apart from the page because the hub's settings page lists contribution switches by title; the title is also the card's header on Overview. One line on what it is for (until the first search), the first saved searches, and the last stored run; or, when nothing could be searched, why (see below). |
| `skills` `search-as-code` | 教助手怎么在这台电脑的聊天记录里搜的说明 | Guides the assistant on composing `\|>` pipelines, using `conversation` scope to avoid tool noise, and saving reusable recipes. |
| `assistant` `sessions` | 让助手去其他电脑、其他项目搜聊天记录的 `sac` 命令（先问你） | The `sac-remote` skill and the `sac` command gate for searching other computers and projects (see above). |
| `mcp_servers` `search-as-code` | 助手在这台电脑上搜聊天记录用的工具 | `search-as-code mcp` over stdio. Only `sac_recipes_list` is in `allow`; the searches and the write tools follow the conversation's permission policy. |
| `node` | 装在选中电脑上的小程序 | `search-as-code node` on the computers picked in `machines`: `sac.search`, `sac.index`, `sac.projects`, `sac.restore`. |

The hub's settings page labels each contribution switch from the manifest: a `ui` item by its `title`, but a skill, an assistant contribution and an MCP server by their `id` only (`search-as-code`, `sessions`), because the manifest schema (`SkillItem`, `AssistantItem`, `MCPServerItem` in `sdk/go/wire/schema/manifest.schema.json`) has no display field for them and refuses an unknown one. Those ids cannot be renamed for display: the MCP server's id is the `mcp__search-as-code__*` prefix of its tools, and every id keys the switch's stored state (and `off_by_default`). The plain names above wait for a display field in the hub's schema.

### When nothing could be searched

When a search read no chat at all, the `结果` tab keeps the intro and replaces the counts, the quick searches and the empty list with one of four states, and the card shows the matching line instead of its saved searches:

- **No computer picked** (`还没设置好`), and nothing in this computer's chat folders: four short lines in the order a first-time user acts on them: what is missing, where to pick one (`设置 › 扩展 › AI 聊天记录搜索` → `要搜哪些电脑`), to press `再找一次` afterwards, and that this is not an error. One footer line adds that the picked computer must be on and connected, the background sync (every 15 minutes), and that chats in another folder on this computer go under `搜哪些地方` instead.
- **Computers picked, not searched there yet** (the shown run did not reach them): how many are picked, and that `再找一次` searches them.
- **None answered**: which computers were offline or did not answer with nothing synced, that this does not mean they have no chats, and what to check.
- **They answered and have no chats**: which ones answered (and which did not), and where extra chat folders are set (`其他聊天记录文件夹（一般不用填）`).

The card goes by the stored run and by whether this computer's chat folders hold anything (their own entries only), so drawing it still runs no search.

Switching a computer on in `要搜哪些电脑` asks for confirmation (`x-confirm`): this extension's program runs there with the user's own rights (not sandboxed) and reads the Claude Code and Codex chat folders, the disklessly text indexes and the extra chat folders; it is read-only and writes nothing there except when the user restores a chat moved to the cloud (then disklessly writes that file back); it makes no outside connection and sends to the hub, over Persona's own connection, only the snippets and project names a search finds and the compact synced copy described above.

## The MCP server

`search-as-code mcp` speaks MCP over stdio (JSON-RPC 2.0, one message per line: `initialize`, `ping`, `tools/list`, `tools/call`). `search-as-code --help` names `mcp`, which the manifest's `requires` checks. Set `PERSONA_SEARCH_AS_CODE_BIN` to the binary path when `search-as-code` is not on `PATH` or in `~/bin`.

| Tool | Access | What it does |
| --- | --- | --- |
| `sac_execute` | read, asks | Compiles and executes explicit `code`, a named `recipe`, or a natural-language `query` with optional `scope`, `project`, `harness`, `model`, `since`, and `limit`. Returns `program` (`code_listing`), `step_traces`, `groups`, and `hits`. |
| `sac_session_search` | read, asks | CASS-style multi-scope session search (`query`, `scope`, `project`, `harness`, `model`, `since`, `exclude_tools`, `limit`). |
| `sac_recipes_list` | read, allowed | Lists built-in and custom Search-as-Code recipes, available pipeline operators, scopes, and discovered session source statistics. |
| `sac_recipe_upsert` | write, asks | Creates or updates a reusable Search-as-Code recipe after validating its pipeline syntax. |
| `sac_recipe_delete` | write, asks | Deletes a custom recipe by `id`. |
| `sac_session_restore` | write, asks | Restores an archived (`已上云`) session by `entry_id` to its original path through the local `disklessly` CLI. Returns `path`, `session_id` and the `resume` command. |

## Storage

The extension and `search-as-code mcp` share `search_as_code_state.json` synchronized via an exclusive file lock on `.search_as_code.lock` and atomic rename. `search-as-code mcp` resolves its data directory from `--data`, else `PERSONA_SEARCH_AS_CODE_DATA`, else `<user config dir>/persona-search-as-code/data-dir` written by the hub extension, else `<user config dir>/persona-search-as-code/data`.

## Tests

```sh
go -C extensions/search-as-code test -race ./...
```

The test suite verifies protocol conformance and manifest schema validation, Claude Code and Codex JSONL session parsing, `Without tools` (`conversation`) noise filtering vs `all` / `user` / `assistant` / `errors` scopes, natural-language to Search-as-Code compilation, multi-step pipeline execution (`where`, `exclude_tools`, `match_regex`, `correlate`, `dedup`, `group_by`, `rank`, `limit`), local repository file scanning, UI rendering and actions across all tabs and detail views (`wire.CheckView`), all six MCP tools, and archived sessions: synthetic text indexes found by `scan_sessions` and marked `已上云`, restored and live duplicates not counted twice, and restore through a fake `disklessly` (success, exit 4). Cross-computer search runs each computer's node program in process through the JSON `nodes.call` carries: fan-out and merge with source computers, the synced index for an offline computer (no tool output in it), index pages that lose no session, the API's routes and their pinning against widening code and inline tokens, the gate's rules, the approval card's text, restore routed to the computer that holds the session, route keys that stay one remembered segment, that nothing changes with no computer picked, the four states of a search that read nothing on the page and the card, the confirmation for switching a computer on, and that the contribution ids stay as they are.
