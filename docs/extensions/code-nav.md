# Code navigation

[Documentation](../README.md) · [Source](../../extensions/code-nav/)

Code navigation adds a **代码** (Code) page to the main navigation. It reads the repositories on your computers and lets you move around them the way a JetBrains IDE does: a file tree and a breadcrumb, a file's outline, symbol search and Find in Files, go to definition, go to implementation, find usages, quick info (a symbol's type, signature and documentation), and back and forward through where you have been. The computer's program asks a language server for the language of each file and only ever reads; it changes no file.

## Setting it up

1. In the extension's settings, choose the computers whose code you want to read. The program runs there, as part of the computer's Persona node.
2. Add the folders your code is in (`~/code` by default). A folder that is itself a repository is listed as one; otherwise the repositories up to two levels below it are. A repository is a Git work tree, or a folder with a project file of a language below at its top (`go.mod`, `package.json`, `tsconfig.json`, `pyproject.toml`, `setup.py`, `Cargo.toml`, `Package.swift` and the like).
3. Install the language servers for the languages you read. Each is found on the login shell's `PATH` and the usual install folders (`~/go/bin`, `~/.local/bin`, `~/.cargo/bin`, Homebrew), or at the path set in the settings.

| Language | Server | Install |
| --- | --- | --- |
| Go | `gopls` | `go install golang.org/x/tools/gopls@latest` |
| TypeScript, JavaScript | `typescript-language-server --stdio` | `npm install -g typescript-language-server typescript` |
| Python | `pyright-langserver --stdio`, else `pylsp` | `npm install -g pyright`, or `pip install python-lsp-server` |
| Rust | `rust-analyzer` | `rustup component add rust-analyzer` |
| Swift | `sourcekit-lsp` | comes with Xcode or a Swift toolchain |
| C, C++ | `clangd` | your system's package manager |

Without a server for a language, its files can still be opened, browsed and searched with Find in Files; the page names the server to install where a jump would be. Go is tested end to end against a real `gopls`; the other languages are driven through the same client and tested with a stand-in server.

### Settings

| Setting | Meaning |
| --- | --- |
| Computers | Where the program runs and reads code. |
| Code folders | Up to 32 folders; `~` is that computer's home. |
| Go / TypeScript and JavaScript / Python / Rust / Swift server path | Optional; empty searches the `PATH`. A path from the settings is started with the arguments its program needs (`--stdio` for `typescript-language-server` and `pyright-langserver`, none for `pylsp`, `gopls`, `rust-analyzer` and `sourcekit-lsp`). The old `gopls` setting keeps its meaning. |
| Idle minutes | A language server nobody has asked anything for this long is stopped, 10 minutes by default (1 to 1440). It starts again on the next request. |

## The page

| Part | What it does |
| --- | --- |
| Repositories | The repositories found in the configured folders, and the languages the computer can jump in. |
| Breadcrumb | 仓库 / repository / folders / file. Every folder in it opens a list of that folder; a path deeper than the breadcrumb holds keeps its first folder and its last ones. |
| File tree and folders | The repository's files (Git's tracked and untracked-but-not-ignored files, at most 20,000), opened folder by folder in the side pane, and as rows of folders and files in the main pane (the repository's top shows its README below them). Files that hold secrets are not listed. |
| Outline | The open file's functions, types and fields. Tapping one goes to its line; its menu finds usages, goes to implementations or shows quick info. |
| Code | The file from 200 lines above the line you went to, about 190,000 characters at a time, with that line highlighted; the rest loads as you scroll, and a button shows the lines above. **跳到行** (Go to line) takes a line, or `line:column`. |
| Names on the marked line | Every name on the highlighted line, the one at the column first: tapping one goes to its definition; its menu goes to implementations, finds usages or shows quick info. |
| Quick info | What the language server says about a name (its type or signature and documentation) above the code, with **跳到定义** (Go to definition). |
| Definitions, implementations, usages | One definition or implementation opens directly; several are listed, as usages are, each with its line. |
| Search | The search box searches symbols of the whole repository by name (the language server's workspace search) or, switched to **全文** (Text), Find in Files. Without a server for the repository, symbol search answers with Find in Files. |
| Back and forward | Every jump remembers where you were: **← 后退** returns there and **前进 →** goes forward again (the last 30 places each way). A new jump after going back drops the way forward, as in an IDE. |

### UI version 12 and 13

Everything above works on a Persona app of UI version 12: the outline, the names of the marked line, the search results and the lists are rows whose tap and menu do the jumps. An app of UI version 13 can also report which name in the code was chosen (⌘-click or ⌘B on a Mac, Ctrl-click or Ctrl+B elsewhere; Alt+F7 for usages; ⌘[ to go back) through the code component's `navigate` action. The page sets it as soon as the copied SDK has the field and the app says it is of version 13, with no change to the extension; until then the field is not drawn and the page is the version 12 one. The program answers the `nav` action (values `line`, `column`, `word` and `intent`: `definition`, `implementation`, `references`, `hover`, `back` or `forward`) either way, always for the file the page has open, never for a path the values name.

A definition in a standard library or a Go module dependency opens read-only, outside the repository.

## Find in Files

Find in Files scans the repository's listed files in the computer's program, in plain Go, without a shell or an external search tool. It matches the text literally, ignoring case unless the text has a capital letter (smart case), and lists at most 200 places, 20 per file, with the part of a long line around each. It skips binary files (a NUL in the first 8,000 bytes), files over 1 MB, every folder named `.git`, `node_modules`, `dist`, `build`, `out`, `target`, `vendor`, `.next`, `__pycache__`, `.venv` or another usual dependency or build folder, files that hold secrets, and any file that a link leads out of the repository. It stops after 8 seconds and says how many files it read.

## What the program reads

The program reads only files inside the configured repositories, and, when a definition leads there, inside Go's own sources and the module cache. Paths are resolved with links followed before that check, so a link (to a file or a folder) cannot lead out. It never reads or lists `.git` internals, `.env` files (except `.env.example`, `.env.sample` and `.env.template`), private keys and certificates (`*.pem`, `*.key`, `*.p12`, `*.pfx`, `*.p8`, keystores, SSH keys), the home folder's credential directories and files (`.ssh`, `.gnupg`, `.aws`, `.kube`, `.docker`, `.config/gh`, `.netrc`, `.git-credentials` and others, Persona's own `.persona` included), or iCloud Drive. A file is read up to 4 MB; binary files are not shown. Nothing is written: no file of a repository is created or changed, and the language servers are told about files only with their text as read (never a save or an edit).

Language servers start on the first request for a repository and language and stop after the idle time in the settings. They run in a process group of their own with the login shell's `PATH` and a short list of variables (`HOME`, `GOPATH`, `GOFLAGS` and the like), not the node's whole environment.

## Tests

```sh
go -C extensions/code-nav test -race -count=1 ./...
```

`TestThePageOnARepository` builds a small Go module and drives the page against a real `gopls`: opening a file, the outline, the names of a line, quick info, go to definition, find usages, go to implementation, back and forward, symbol search, Find in Files and the version 13 `nav` action. Without `gopls` its language-server part is skipped; with `CODE_NAV_REQUIRE_GOPLS=1` a missing `gopls` fails it instead. The test binary also stands in for a language server, so the other languages, server paths from the settings, lazy start and idle stop are tested without their servers installed. `viewcheck_test.go` holds every page, state and edge (empty states, long lines and names, deep paths, missing servers, every action on odd states) to `wire.CheckView`.
