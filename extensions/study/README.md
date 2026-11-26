# Study

Study (学习) turns what you already keep into flashcards and schedules their review with [FSRS](https://github.com/open-spaced-repetition/go-fsrs). Cards come from Persona knowledge-base topics, Notion pages, an Obsidian vault, NotebookLM notebooks or text handed over in a conversation. The **学习** page reviews the day's cards, the **今天要复习** widget shows how many are due, and a daily reminder says when it is time. The assistant can quiz you with **考考我**.

- [Extension guide and settings](../../docs/extensions/study.md)
- [Installation](../../docs/installation.md)
- [Development and tests](../../docs/development.md)
- [Manifest](persona-extension.json)

The package contains the `study` program, started with the hub, and a node program that reads an Obsidian vault and runs the NotebookLM command line on the computer you choose. Cards and the review log stay in the extension data directory on the hub.
