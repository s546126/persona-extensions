# Code navigation

[Documentation](../README.md) · [Source](../../extensions/code-nav/)

Code navigation adds a **代码** (Code) page to the main navigation. It reads repositories on the Hub machine or your paired computers and lets you navigate them like a JetBrains IDE: guided in-page setup, per-machine language server probing and configuration, Git diff inspection with click-to-jump into source code, a file tree and breadcrumb, a file's outline, symbol search and Find in Files, go to definition, go to implementation, find usages, quick info, UI version 13 in-code navigation (`CodeComponent.navigate`), and back/forward history. Everything is strictly read-only: no repository file is ever created or modified.

## Guided setup and configuration

On first install (when no computer is enabled yet) or whenever you open the **配置向导** (Setup) tab, the page walks you through configuration directly in the UI:

1. **Choose target computers**: enable **本机（Hub）** (Hub local) with a single toggle to browse repositories on the Hub machine itself, or select any computer authorized in extension settings. Paired computers discovered through `machines.read` are listed with their online status.
2. **Manage code folders**: add or remove code root folders (`~/code` by default) per computer, or one-tap detected folders (`~/code`, `~/workspace`, `~/projects`, etc.). Each folder is probed live for readability, sandbox status, and available repository count.
3. **Verify and browse**: live summary counters show enabled computers, folders, repositories, and ready language servers before switching straight into the **仓库** (Repositories) tab.

In-page per-machine overrides (Hub toggle, code folders, per-language enable/disable and custom server paths, and idle eviction timeout) are persisted safely in the extension's data directory (`code-nav-settings.json`, `0600`) and synchronized automatically to the target node.

### Language server probing and configuration

The **语言服务** (Languages) tab lets you switch between **本机（Hub）** and any configured remote computer to inspect and manage each language server:

| Language | Server | Install |
| --- | --- | --- |
| Go | `gopls` | `go install golang.org/x/tools/gopls@latest` |
| TypeScript, JavaScript | `typescript-language-server --stdio` | `npm install -g typescript-language-server typescript` |
| Python | `pyright-langserver --stdio`, else `pylsp` | `npm install -g pyright`, or `pip install python-lsp-server` |
| Rust | `rust-analyzer` | `rustup component add rust-analyzer` |
| Swift | `sourcekit-lsp` | comes with Xcode or a Swift toolchain |
| C, C++ | `clangd --background-index=false` | your system's package manager |

For every language on the selected machine, the UI shows its live status (`已就绪`, `未安装`, `已禁用`, `路径无效`), resolved executable path, detected version, running server count, and install hint. You can re-probe with one tap, toggle a language on or off, set or reset a custom executable path (validated immediately on save), configure the idle eviction timeout (`1–1440` minutes), or stop running servers immediately.

Without a server for a language, its files can still be opened, browsed, diffed, and searched with Find in Files.

### Extension settings

| Setting | Meaning |
| --- | --- |
| Computers | Remote Persona nodes where `code-nav node` runs and reads code. |
| Code folders | Up to 32 default folders; `~` is that computer's home (can also be managed per computer in the Setup tab). |
| Go / TypeScript and JavaScript / Python / Rust / Swift server path | Optional default executable paths (can also be overridden per computer in the Languages tab). |
| Idle minutes | Default timeout before an idle language server is stopped, 10 minutes by default (`1–1440`). |

## Git diff and jump to source

Inside any Git repository, tap **变更 Diff** (Diff) in the top bar to inspect read-only changes across three scopes:

- **未暂存** (Unstaged): working tree modifications plus untracked non-ignored files.
- **已暂存** (Staged): index changes (`git diff --cached`).
- **对比主干** (vs Main): changes relative to the repository's default branch (`origin/HEAD`, `main`, or `master`).

Tap a changed file to inspect its syntax-highlighted unified diff (`DiffComponent`) along with clickable hunk and modified-line jump targets, or jump directly from the file list menu. Tapping any modified hunk/line or **打开完整源码** (Open full source) opens the full source file centered and highlighted on that exact line, immediately unlocking the LSP outline, line names, Hover, Go to Definition / Implementation / References, UI 13 `CodeComponent.navigate`, and Back/Forward history.

Diff reads use read-only Git commands (`GIT_OPTIONAL_LOCKS=0`, `GIT_TERMINAL_PROMPT=0`), filter out secret paths and escaping symlinks, and truncate oversized diffs safely within view budgets.

## The page

| Part | What it does |
| --- | --- |
| Repositories | Repositories discovered in configured folders, with ready language summary. |
| Setup guide | Visual onboarding for Hub/node selection, folder management, and live probing. |
| Languages | Per-machine LSP status, version probing, enable/disable switches, custom paths, and idle policy. |
| Git diff | Unstaged, staged, and branch diffs with one-tap jump from any modified line to full source. |
| Breadcrumb | `仓库 / repository / folders / file` (or `变更 Diff / file`); folders are tappable. |
| File tree and folders | Tracked and untracked non-ignored files (up to 20,000), filtering out secret paths. |
| Outline | The open file's functions, types, and fields with jump, usages, implementations, and quick info. |
| Code | Centered around the target line with highlight, lazy loading, Go to line, and UI 13 `navigate`. |
| Search | Workspace symbol search via LSP, or Find in Files (**全文**), with automatic fallback. |
| Back and forward | Tracks up to 30 navigation steps each way across files, definitions, and diff jumps. |

### UI version 12 and 13

Everything works on Persona apps of UI version 12 (and degrades gracefully on older versions): outline rows, marked-line names, diff jump targets, and search results are tappable rows. On UI version 13, `CodeComponent.navigate` is populated directly so ⌘/Ctrl-click, ⌘/Ctrl+B, ⌥F7, and ⌘[ trigger definitions, references, and back navigation inside the code viewer. The `nav` action always operates on the file currently open in page state, never trusting a client-supplied path.

## What the program reads

The program reads only files inside the configured repositories and (when Go definitions lead there) `GOROOT` and `GOMODCACHE`. All paths are symlink-resolved before sandbox checks. It never lists, diffs, or reads `.git` internals, `.env*` secrets (except `.env.example`, `.env.sample`, `.env.template`), credential files (`.npmrc`, `.pypirc`, `.netrc`, `.git-credentials`, `.pgpass`, `credentials.toml`, etc.), private keys/certificates (`*.pem`, `*.key`, `*.p12`, etc.), sensitive home directories (`.ssh`, `.gnupg`, `.aws`, `.kube`, `.docker`, `.config/gh`, `.persona`, etc.), or iCloud Drive. Language servers run with read-only flags/settings and never write to repositories.

## Tests

```sh
go -C extensions/code-nav test -race -count=1 ./...
```

- `TestThePageOnARepository` and `TestGitDiffJumpToSourceWithRealGopls` drive the full workflow (including Git diff -> click modified line -> source highlight -> UI 13 `CodeComponent.navigate` -> Back/Forward) against a real `gopls` (`CODE_NAV_REQUIRE_GOPLS=1` fails if `gopls` is missing).
- `ux_v2_test.go` verifies persistent settings storage, visual onboarding on Hub local, per-language LSP probing/toggling/custom paths/idle policy, and Git diff read-only/sandbox/truncation guarantees.
- `viewcheck_test.go` validates every page, tab, diff state, empty/error state, and edge case against `wire.CheckView`.
