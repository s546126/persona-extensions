# Code navigation

[Documentation](../README.md) · [Source](../../extensions/code-nav/)

Code navigation adds a **代码** (Code) page to the main navigation. It reads the repositories on your computers and lets you move around them the way an IDE does: open files from a tree, see a file's outline, search symbols by name, go to a definition, find the usages of a function, and step back to where you were. The computer's program asks a language server, `gopls` for Go, and only ever reads; it changes no file.

## Setting it up

1. In the extension's settings, choose the computers whose code you want to read. The program runs there, as part of the computer's Persona node.
2. Add the folders your code is in (`~/code` by default). A folder that is itself a repository is listed as one; otherwise the Git repositories (or Go modules) up to two levels below it are.
3. For Go, install `gopls` on that computer: `go install golang.org/x/tools/gopls@latest`. The program finds it on the login shell's `PATH`, in `~/go/bin`, or at the path set in the settings. Without a language server the page still shows the files; the outline, search and jumps need one.

Other languages are found when their server is installed (`typescript-language-server`, `pyright-langserver` or `pylsp`, `rust-analyzer`, `clangd`), but only Go is tested so far.

## The page

| Part | What it does |
| --- | --- |
| Repositories | The repositories found in the configured folders, and the languages the computer can jump in. |
| File tree | The repository's files (Git's tracked and untracked-but-not-ignored files, at most 20,000), opened folder by folder. |
| Outline | The open file's functions, types and fields. Tapping one goes to its line; **查找引用** (Find usages) lists where it is used. |
| Code | The file from 200 lines above the line you went to, about 190,000 characters at a time, with that line highlighted; the rest loads as you scroll, and a button shows the lines above. |
| Symbol search | Symbols of the whole repository by name, through the language server's workspace search. |
| Back | Each jump remembers where you were; **← 后退** returns there (the last 30 places). |

Go to definition from inside the code (⌘B or ⌘-click on a Mac, Ctrl+B or Ctrl-click elsewhere; Alt+F7 for usages) needs a Persona app that can report which name in the code was chosen. Until then, the outline, the usages list and the symbol search do the same job. A definition in Go's standard library or a module dependency opens read-only, outside the repository.

## What the program reads

The program reads only files inside the configured repositories, and, when a definition leads there, inside Go's own sources and the module cache. Paths are resolved with links followed before that check. It never reads `.git` internals, `.env` files (except `.env.example` and the like), private keys and certificates (`*.pem`, `*.key`, `*.p12`, SSH keys), or the home folder's credential directories, Persona's own included. A file is read up to 4 MB; binary files are not shown.

Language servers start on the first request for a repository and stop after ten minutes without one. They run with the login shell's environment, so `GOPATH`, `GOFLAGS` and the like apply as in a terminal.

## Tests

```sh
go -C extensions/code-nav test ./...
```

The page test builds a small Go module and runs `gopls` against it when `gopls` is installed; without it, the language-server part is skipped.
