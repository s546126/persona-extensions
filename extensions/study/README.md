# Study

Study (学习) turns what you already keep into flashcards and schedules their review with [FSRS](https://github.com/open-spaced-repetition/go-fsrs). Cards come from Persona knowledge-base topics, Notion pages, an Obsidian vault, a Logseq graph, NotebookLM notebooks or text handed over in a conversation (for example a document or a vocabulary list the assistant read from a source you connected). Each card belongs to a knowledge point with a mastery score; the **学习** page reviews the day's cards, lists weak knowledge points, runs tutor practice with four exercise kinds, Socratic hints, automatic grading with a diagnosis of each mistake, and reinforcement cards, and, since 0.3.0, walks a knowledge point or a text in a guided lesson (an intuition, an analogy, key points and an unrecorded quick check per section) and draws a knowledge map of every knowledge point by mastery level. Optional learner-profile settings (`learner_level`, `learner_interests`, `explain_style`) adapt the depth, examples and tone of what the model writes, never its content. The **待复习** widget shows how many are due, and a daily reminder says when it is time. The assistant can quiz you with **考考我**, tutor you with **深度辅导** and **Tutor 练习**, and walk your weakest knowledge point with **精读薄弱点**, using the `study_map` and `study_guide` tools.

- [Extension guide and settings](../../docs/extensions/study.md)
- [Installation](../../docs/installation.md)
- [Development and tests](../../docs/development.md)
- [Manifest](persona-extension.json)

The package contains the `study` program, started with the hub, and a node program that reads an Obsidian vault and a Logseq graph and runs the NotebookLM command line on the computer you choose. Cards, the review log, practice answers, exercises and guided lessons stay in the extension data directory on the hub.
