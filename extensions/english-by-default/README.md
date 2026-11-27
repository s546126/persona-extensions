# English by Default (Claude Code)

> **Category: Public extension** — General-purpose extension using only public Persona extension APIs; prompts and their pairs stay in the extension's data directory on each chosen computer.

Practice writing to coding agents in English. On the computers you choose, this extension adds a Claude Code `UserPromptSubmit` hook that catches prompts you wrote in another language (Chinese, Japanese, Korean, other scripts, and Spanish, French, German and other Latin-script languages; quoted text, code, links and paths do not count) and, by the mode you pick, holds the prompt back until you rewrite it in English and a local model agrees the meaning matches (block), lets it through with an English translation for you and the model (warn), or only records it (log). Typing `giveup` after a block shows the English and sends the original. Slash commands, prompts with an urgent word, the extension's own model calls and Persona's own assistant conversations go through untouched; in sessions Persona runs, the English is put at the top of the reply so the app shows it. The pairs are listed on the extension's page.

- [Extension guide: modes, detection, what is stored, limits](../../docs/extensions/english-by-default.md)
- [Installation](../../docs/installation.md)
- [Development and tests](../../docs/development.md)
- [Manifest](persona-extension.json)

The gate, its prompts and messages are a Go port of [s546126/english-by-default](https://github.com/s546126/english-by-default) (MIT), by the same author.
