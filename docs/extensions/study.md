# Study

[Documentation](../README.md) · [Extension source](../../extensions/study/README.md)

Study (学习) is a spaced-repetition companion for Persona. It makes question-and-answer cards from material you already have and schedules each review with FSRS, the scheduler used by Anki and Obsidian Spaced Repetition. Open **学习** in Persona to review the day's cards, look over cards waiting to be kept, add cards from a source, or browse and edit your collection.

## Where cards come from

| Source | How it is read | What becomes a card |
| --- | --- | --- |
| Knowledge-base topics | The hub's knowledge base (`kb.read`) | The model writes cards from the topic's summary and points; they wait as drafts. |
| Notion | The Notion API with your own integration secret, read-only | Toggle blocks become cards (the toggle is the question, its content the answer); pages without toggles go to the model. Cards wait as drafts. |
| Obsidian | The node program on the chosen computer reads the vault | Notes tagged `#flashcards` (configurable) are imported directly using Obsidian Spaced Repetition syntax: `Q::A`, `Q:::A` (both directions), multi-line cards separated by `?` or `??`, and `==cloze==` deletions. Other notes can be sent to the model; those cards wait as drafts. |
| NotebookLM | The community [`notebooklm`](https://github.com/teng-lin/notebooklm-py) command line on the chosen computer | Flashcard and quiz artifacts you already generated in NotebookLM; they wait as drafts. |
| Conversation | The assistant's `study_make_cards` action | Text or cards handed over in a chat; they wait as drafts. |

Drafts are kept or dropped on the page, one at a time or all at once. Importing the same note or artifact again does not duplicate cards: a card's identity is its source and question.

Model-made cards use the hub's model (`models`). The material is passed as untrusted text that the prompt tells the model never to follow as instructions, and at most `cards_per_source` cards are made from one source.

## Reviewing

The review page shows one card's question; **看答案** shows the answer and four buttons (忘了, 模糊, 记得, 很熟) labelled with the next interval each would set. New cards are introduced up to `new_per_day` a day, after the cards that are due. The home page shows the day's count, cards in learning, mature cards (an interval of 21 days or more), the streak and the measured retention over the last 30 days. A card can be paused, resumed, edited or deleted from its detail view.

Every review is appended to a log kept beside the cards, so schedules can be refitted or exported later. The log keeps the most recent 20,000 reviews.

## Widget and reminder

The **今天要复习** widget (small Home Screen widgets and Lock Screen accessories) shows how many cards are due and opens the page. The page's navigation badge shows the same count.

With `remind` on, the extension sends one notification a day at `remind_at`, in your device time zone, when cards are due. It needs the hub's `notify` permission; without it no reminder is sent and reviewing still works.

## Assistant

The **考考我** quick prompt and the `study-quiz` skill let the assistant quiz you one card at a time. Its tools:

- `study_due`, `study_card`, `study_search`, `study_stats`: read-only. `study_due` lists questions without answers.
- `study_grade`: records your rating for a card. It is a connector action, so it goes through the hub's confirmation rules, and a repeated request id is recorded once.
- `study_make_cards`: makes draft cards from text or from cards the assistant wrote; nothing is added to the review queue until you keep the drafts.

## Settings

Change these in **Settings › Extensions › 学习**:

| Setting | Default | Meaning |
| --- | --- | --- |
| `machines` | none | The one computer whose node reads the Obsidian vault and runs `notebooklm`. |
| `obsidian_vault` | empty | Absolute path of the vault on that computer (`~/` allowed). |
| `flash_tag` | `flashcards` | Notes with this tag (or a sub-tag such as `#flashcards/biology`, inline or in front matter) are imported directly. |
| `notebooklm_command` | empty | Absolute path to the `notebooklm` program, if it is not on `PATH`, `~/.local/bin`, `/opt/homebrew/bin` or `/usr/local/bin`. |
| `new_per_day` | `20` | New cards introduced a day. |
| `retention` | `0.9` | FSRS target retention. |
| `cards_per_source` | `8` | Most cards the model makes from one source (up to 20). |
| `remind` | `true` | Send the daily reminder. |
| `remind_at` | `20:00` | Time of the daily reminder, HH:MM. |

The Notion integration secret is the optional `notion_secret`. Share the pages you want to study with the integration in Notion; the extension only reads.

## Obsidian

The vault path comes from the node's own settings, not from the hub's call, and the node reads only `.md` files inside it: no absolute paths, `..`, hidden folders such as `.obsidian`, or symlinks that leave the vault. An import scans up to 3,000 tagged notes and 5,000 cards; one note is read up to 512 KB. `<!--SR:...-->` scheduling comments written by the Obsidian plugin are ignored: Study keeps its own schedule.

## NotebookLM

NotebookLM has no public API. Study runs the community command line [`notebooklm-py`](https://github.com/teng-lin/notebooklm-py) (MIT) on the chosen computer, which signs in to your Google account through a browser and calls NotebookLM's private web endpoints. Install it and sign in yourself on that computer:

```sh
pip install notebooklm-py
notebooklm login
```

Study only reads: it checks the sign-in (`auth check`), lists notebooks, lists completed flashcard and quiz artifacts, and downloads one as JSON. It never generates, changes or deletes anything in NotebookLM; generate flashcards or a quiz in NotebookLM first. Quiz questions become cards with the options on the front and the correct option on the back.

Because the endpoints are unofficial, Google can change or block them at any time, and using them may not be within Google's terms; the page says so before you import. If the sign-in has expired, the page shows the login command instead of notebooks.

## Data and network

Cards, drafts and the review log are saved atomically to `study.json` in the extension data directory on the hub. The hub program reaches only `api.notion.com`. The node program reaches NotebookLM's Google hosts, through the `notebooklm` command, and reads files only in the configured vault.
