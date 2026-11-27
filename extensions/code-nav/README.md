# Code navigation

Read the code on your computers in Persona and move around it the way a JetBrains IDE does: a file tree and a breadcrumb, a file's outline, symbol search and Find in Files, go to definition and implementation, find usages, quick info, and back and forward. The computer's program starts a language server per repository and language as needed (`gopls`, `typescript-language-server`, `pyright-langserver` or `pylsp`, `rust-analyzer`, `sourcekit-lsp`) and only reads; without one, the code can still be read and searched.

- [Guide](../../docs/extensions/code-nav.md)
- [Manifest](persona-extension.json)
- [Development and tests](../../docs/development.md)
