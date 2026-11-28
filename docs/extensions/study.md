# Study

[Documentation](../README.md) · [Extension source](../../extensions/study/README.md)

Study (学习) is a spaced-repetition companion and tutor for Persona. It makes question-and-answer cards from material you already have, schedules each review with FSRS, the scheduler used by Anki and Obsidian Spaced Repetition, scores how well you master each knowledge point, and runs guided practice on the weakest ones. Since 0.3.0 it also helps you understand material the first time: a guided lesson walks a knowledge point or a text section by section with a quick check in each, a knowledge map shows every knowledge point by mastery level, and an optional learner profile adapts the depth, examples and tone of everything the model writes. Open **学习** in Persona to review the day's cards, look over cards waiting to be kept, open the knowledge map or a guided lesson, see weak knowledge points, practise with the tutor, add cards from a source, or browse your collection.

## Where cards come from

| Source | How it is read | What becomes a card |
| --- | --- | --- |
| Knowledge-base topics | The hub's knowledge base (`kb.read`) | The model writes cards from the topic's summary and points; they wait as drafts. |
| Notion | The Notion API with your own integration secret, read-only | Toggle blocks become cards (the toggle is the question, its content the answer); pages without toggles go to the model. Cards wait as drafts. |
| Obsidian | The node program on the chosen computer reads the vault | Notes tagged `#flashcards` (configurable) are imported directly using Obsidian Spaced Repetition syntax: `Q::A`, `Q:::A` (both directions), multi-line cards separated by `?` or `??`, and `==cloze==` deletions. Other notes can be sent to the model; those cards wait as drafts. |
| Logseq | The node program on the chosen computer reads `pages/` and `journals/` of the graph | Blocks tagged `#card`, `#[[card]]`, `[[card]]` or `tags:: card` are imported directly: the block is the question and its indented child blocks, as an outline, the answer; `{{cloze …}}` makes one fill-in card per cloze. Other pages can be sent to the model; those cards wait as drafts. Cards are filed under the page name. |
| NotebookLM | The community [`notebooklm`](https://github.com/teng-lin/notebooklm-py) command line on the chosen computer | Flashcard and quiz artifacts you already generated in NotebookLM; they wait as drafts. |
| Conversation | The assistant's `study_make_cards` action | Text or cards handed over in a chat, including documents and vocabulary the assistant read from a source you connected (see [Feishu documents and Relingo vocabulary](#feishu-documents-and-relingo-vocabulary)); they wait as drafts. |
| Tutor | The `study_reinforce` action or **生成强化卡** on the page | Reinforcement cards that target a practice mistake or a weak knowledge point; they wait as drafts. |
| Guided lesson | **做成待确认卡片** at the end of a lesson | One card for each section: its quick check, or its key points when it has none; they wait as drafts. |

Drafts are kept or dropped on the page, one at a time or all at once. Importing the same note or artifact again does not duplicate cards: a card's identity is its source and question.

Model-made cards use the hub's model (`models`). The material is passed as untrusted text that the prompt tells the model never to follow as instructions, and at most `cards_per_source` cards are made from one source.

## Reviewing

The review page shows one card's question; **看答案** shows the answer and four buttons (忘了, 模糊, 记得, 很熟) labelled with the next interval each would set. New cards are introduced up to `new_per_day` a day, after the cards that are due. The home page shows the day's count, cards in learning, mature cards (an interval of 21 days or more), the streak and the measured retention over the last 30 days. A card can be paused, resumed, moved to another knowledge point, practised or deleted from its detail view.

An answer is drawn as Markdown when it fits the subset the apps draw (https links, no images); otherwise, for example with an Obsidian `![[embed]]`, it is drawn as plain text so the page is never refused.

## Knowledge points and mastery

Every card belongs to a knowledge point: the `topic` given when it was made (a knowledge-base topic's name, a Logseq page, the `topic` of `study_make_cards`), or else its source's title. Change it on the card's page.

Each card that was reviewed or practised gets a mastery score from 0 to 100:

| Part | Weight | From |
| --- | --- | --- |
| Retrievability now | 45% | The FSRS forgetting curve at this moment |
| Stability | 20% | FSRS stability, saturating at about a month |
| Ease | 15% | FSRS difficulty (1 easy to 10 hard) |
| Recent accuracy | 20% | Reviews not rated 忘了 and practice answers judged correct, in the last 30 days |

Each lapse takes off 6 points, at most 30. A card that was only practised, never reviewed, scores 70 times its recent practice accuracy (at most 不稳) until spaced reviews show the memory lasts. A practice answer that was also recorded as the card's review counts once. A knowledge point's score is the average of its studied cards. Scores below 50 are 薄弱 (weak), 50 to 74 不稳 (shaky), 75 and above 掌握 (solid); cards never studied are 未学 (new). **薄弱知识点** lists knowledge points weakest first, filtered by status, and a knowledge point's page lists its cards weakest first with their lapses, practice mistakes and the causes of recent mistakes.

## Tutor practice

**Tutor 练习** makes one exercise at a time from the weakest card (overall, of a knowledge point, or a card you pick), in one of four kinds:

| Kind | Asks |
| --- | --- |
| 概念辨析 (concept) | Explain the idea, or tell it apart from a close, easily confused one |
| 填空 (cloze) | Fill in the key term of a sentence |
| 选择 (choice) | Pick the right one of four options, the others being plausible mistakes |
| 场景变式 (scenario) | Apply the idea to a new, concrete situation |

The model writes the question, its answer and three Socratic hints that narrow down without giving the answer away. Choice and exact fill-in answers are graded by the extension itself; other answers are judged by the model on meaning, which also names the cause of a mistake (概念混淆, 记忆遗漏, 审题偏差, 推理错误, 迁移不足 or 粗心笔误) with a diagnosis and feedback. The model cannot overrule a wrong choice. Each exercise is judged once: answering it again returns the first judgement, so an answer written after seeing the reference answer does not count. The number of hints is the larger of what the caller reports and what the extension handed out. Tag-like text inside material and answers is neutralized before it reaches the model, so it cannot close or open the prompts' fences. When the exercise's card is due, the result is recorded as its review: wrong is 忘了, partly right or right after two or more hints is 模糊, otherwise 记得. Practice never changes the schedule of a card that is not due, and never rates 很熟.

After a wrong or partial answer (and only then), **生成强化卡** drafts cards aimed at that mistake; a knowledge point's page drafts cards for the whole point. They wait in 待确认 like other drafts and join FSRS once kept.

Without a model, the card's own question is asked with hints cut from its answer, open answers are judged roughly by wording (and say so), and reinforcement turns the missed exercise itself into a card. The extension keeps the last 100 exercises and 5,000 practice answers.

## Guided lessons

**精读学习** turns one knowledge point, or a text you paste, into a short lesson of up to six sections that build on each other (the model is asked for three to six). Its start page lists your knowledge points weakest first (the 20 weakest; the rest are reached from the knowledge map), a form for a text of up to 12,000 characters with an optional title, and the last eight lessons. **先精读这个知识点** on a knowledge point of the knowledge map opens the same lesson.

For a knowledge point the model gets its cards, weakest first (at most 12), with their mastery and up to eight recent practice mistakes, so the ideas behind those mistakes get a section of their own. For a text it gets the text. Either way the material is fenced as untrusted and the model is told to use only what it says.

Each page is one section: **第 K / N 节** with the section's title, **一句话直觉** (the idea said plainly), **打个比方** (an analogy and where it stops holding, when an honest one exists), **要点** (one to five points in the material's own terms), and **随堂小测**, one quick question. Type an answer and **提交**, or **查看参考答案**. The answer is compared on the device, without the model: it counts as right when, ignoring case, spaces and punctuation, it is the reference answer or one of the other accepted wordings, or a short answer that contains one. The reference answer is shown either way. The quick check only confirms you followed the section: it is not graded, not recorded, and does not touch the review schedule. **下一节** and **上一节** move between sections.

The last section offers **做成待确认卡片**, which drafts one card a section (its quick check as question and answer, or its key points when it has none); a question that already is a card or a draft is skipped, so pressing it twice adds nothing. Drafts wait in 待确认 like any other until you keep them. When the lesson is about a knowledge point that still has cards, **用深度辅导练这章** opens Tutor practice on it.

A lesson written by the model for the same knowledge point or the same text in the last 24 hours is opened again instead of written anew. Without a model, or when the model fails, a knowledge point's own cards become the lesson, one card a section, weakest first, up to six: the question is the section's title and its quick check, the answer its point and reference answer. The page says so, and such a lesson offers no drafts, since it was made from cards that already exist. A text cannot be made into a lesson without a model. The extension keeps the last 100 lessons.

## Knowledge map

**知识地图** shows mastery as a tree. At the top are the number of knowledge points at each level (薄弱, 不稳, 掌握, 未学), the total of knowledge points and cards, and a filter by level. Below, the knowledge points are grouped by level, weak first, each with its level as a coloured badge, its score, card count, weak cards and recent accuracy, and the cause its recent practice mistakes most often had (常错在…). The page lists at most 100 knowledge points and says how many more there are; the filter narrows it down. The scores are the ones described under [Knowledge points and mastery](#knowledge-points-and-mastery); the map keeps no numbers of its own.

Tapping a knowledge point drills into it: its mastery, number of cards and how many were studied, weak cards, recent accuracy, its cards by level, where they came from, and its mistakes by cause. Two buttons lead on: **先精读这个知识点** opens a guided lesson on it, and **针对弱点做 Tutor 练习** opens Tutor practice on it. Its cards follow, weakest first, with their score, lapses and practice mistakes (at most 40, filtered by level), each opening the card's page. **整张地图** goes back to the whole map.

Every review is appended to a log kept beside the cards, so schedules can be refitted or exported later. The log keeps the most recent 20,000 reviews.

## Widget and reminder

The **待复习** widget (small Home Screen widgets and Lock Screen accessories) shows how many cards are due and opens the page. The page's navigation badge shows the same count.

With `remind` on, the extension sends one notification a day at `remind_at`, in your device time zone, when cards are due. It needs the hub's `notify` permission; without it no reminder is sent and reviewing still works.

## Assistant

The quick prompts **考考我** (quiz the due cards one at a time), **深度辅导** (start from the weakest knowledge point and tutor with hints and diagnosis), **Tutor 练习** (a quick run of mixed exercise kinds) and **精读薄弱点** (look at the knowledge map, then walk the weakest knowledge point section by section with a quick check in each), with the `study-quiz` skill, drive these tools:

- `study_due`, `study_card`, `study_search`, `study_stats`: read-only. `study_due` lists questions without answers.
- `study_weak_points`: read-only. Knowledge points weakest first with mastery, weakest cards and recent mistake causes; filter by `status` (`weak`, `shaky`, `solid`, `new`) or `topic`.
- `study_map`: read-only. The knowledge map: every knowledge point grouped by level with its mastery, card count, recent accuracy and most frequent mistake; `status` keeps one level, `topic` drills into one knowledge point and lists its cards weakest first (`status` then filters the cards), `limit` (1 to 20, default 8) caps the list.
- `study_guide`: writes a guided lesson for a `topic` or a `text` (with an optional `title`) and returns its id and sections, including each quick check and its answer, for the assistant to present one section at a time. A model-written lesson for the same material in the last 24 hours is reused; without a model a topic's cards become the sections. It only stores the lesson in the extension's lesson cache: it schedules nothing and makes no cards.
- `study_practice`: makes one exercise (`kind`: `concept`, `cloze`, `choice`, `scenario`) for a `card`, the weakest card of a `topic`, or the weakest overall, and returns its question without the answer. It only stores the exercise in the extension's practice cache.
- `study_hint`: hint level 1, 2 or 3 of an exercise; it only notes on the exercise how many hints were handed out.
- `study_grade`: records your rating for a card.
- `study_check_answer`: judges an answer to an exercise, with the number of hints seen, and returns the verdict, score, cause, diagnosis, feedback and reference answer; records the review when the card is due.
- `study_reinforce`: drafts reinforcement cards for an `exercise` or a `topic`.
- `study_make_cards`: makes draft cards from text or from cards the assistant wrote, optionally under a `topic`; nothing is added to the review queue until you keep the drafts.

The last four are connector actions, so they go through the hub's confirmation rules. A repeated request (the same action, request id and arguments, compared as canonical JSON) returns the first result and is recorded once, also when the repeat arrives while the first is still running; one request id can answer neither for another action nor for other arguments. A failed action is not remembered, so a retry runs again.

## Feishu documents and Relingo vocabulary

Study has no Feishu or Relingo (Termwise) connection of its own and calls no service of theirs. Their material comes in through the assistant:

1. The assistant reads the document or the word list with a tool you already installed and authorized for that service (such as a Feishu extension or connector of your own), or you paste the text or export it (Markdown into the Obsidian vault or Logseq graph, CSV or text into the chat).
2. It calls `study_make_cards` with the material as `text`, or with cards it wrote: for vocabulary, one word per card with its context sentence on the front and the meaning, pronunciation and an example on the back. `topic` files them under the document's subject or a word list such as 英语生词.
3. The cards wait in 待确认 until you keep them.

The `study-quiz` skill spells this out for the assistant. Only what you point it to is read, and nothing about the source account is stored.

## Settings

Change these in **Settings › Extensions › 学习**:

| Setting | Default | Meaning |
| --- | --- | --- |
| `machines` | none | The one computer whose node reads the Obsidian vault and the Logseq graph and runs `notebooklm`. |
| `obsidian_vault` | empty | Absolute path of the vault on that computer (`~/` allowed). |
| `logseq_graph` | empty | Absolute path of the Logseq graph on that computer (`~/` allowed); only its `pages/` and `journals/` are read. |
| `flash_tag` | `flashcards` | Notes with this tag (or a sub-tag such as `#flashcards/biology`, inline or in front matter) are imported directly. |
| `notebooklm_command` | empty | Absolute path to the `notebooklm` program, if it is not on `PATH`, `~/.local/bin`, `/opt/homebrew/bin` or `/usr/local/bin`. |
| `new_per_day` | `20` | New cards introduced a day. |
| `retention` | `0.9` | FSRS target retention. |
| `cards_per_source` | `8` | Most cards the model makes from one source (up to 20). |
| `remind` | `true` | Send the daily reminder. |
| `remind_at` | `20:00` | Time of the daily reminder, HH:MM. |
| `learner_level` | empty | How deep to explain: `beginner` (intuition first, every term defined), `practical` (worked examples and when to use what) or `deep` (mechanisms, edge cases and trade-offs). Empty adds nothing. |
| `learner_interests` | empty | Fields to draw examples and analogies from, such as 分布式系统、篮球、做饭; at most 200 characters. |
| `explain_style` | empty | `balanced` (plain explanation with an example where it helps), `analogy` (an everyday comparison first, then the exact idea) or `rigorous` (precise definitions and conditions first, few metaphors). Empty adds nothing. |

The last three are the learner profile. When any of them is set, it goes as a `<learner_profile>` block in front of the material of every model call: cards, guided lessons, exercises, judging and reinforcement cards. Unknown values of `learner_level` and `explain_style` are ignored; `learner_interests` is cut to one line of at most 200 characters with control characters and the prompts' fence tags removed, so it cannot end its block or open another. The prompts treat the profile as data about you, never as instructions, and use it only to choose depth, examples and tone: it must not drop, change or weaken any concept, fact or constraint of the material, and an analogy must not bring in a conclusion the material does not state or replace a standard term. With all three empty nothing is added, and the prompts are the same as in 0.2.0.

The Notion integration secret is the optional `notion_secret`. Share the pages you want to study with the integration in Notion; the extension only reads.

## Obsidian

The vault path comes from the node's own settings, not from the hub's call, and the node reads only `.md` files inside it: no absolute paths, `..`, hidden folders such as `.obsidian`, or symlinks that leave the vault. An import scans up to 3,000 tagged notes and 5,000 cards; one note is read up to 512 KB. `<!--SR:...-->` scheduling comments written by the Obsidian plugin are ignored: Study keeps its own schedule.

## Logseq

The graph path comes from the node's own settings, and the node reads only `.md` files under the graph's `pages/` and `journals/` folders: never `logseq/` (settings and backups), `assets/`, hidden files or folders, `..`, absolute paths, or symlinks, including a symlinked `pages/` folder. The same limits as for Obsidian apply (3,000 pages, 5,000 cards, 512 KB a page). Page titles come from a `title::` property, else the file name, decoding namespaces (`a___b` or `a%2Fb` is `a/b`) and journals (`2026_11_27` is `2026-11-27`). Logseq's own `card-*` scheduling properties are ignored; Study keeps its own schedule. A `#card` inside a code block is not a card.

## NotebookLM

NotebookLM has no public API. Study runs the community command line [`notebooklm-py`](https://github.com/teng-lin/notebooklm-py) (MIT) on the chosen computer, which signs in to your Google account through a browser and calls NotebookLM's private web endpoints. Install it and sign in yourself on that computer:

```sh
pip install notebooklm-py
notebooklm login
```

Study only reads: it checks the sign-in (`auth check`), lists notebooks, lists completed flashcard and quiz artifacts, and downloads one as JSON. It never generates, changes or deletes anything in NotebookLM; generate flashcards or a quiz in NotebookLM first. Quiz questions become cards with the options on the front and the correct option on the back.

Because the endpoints are unofficial, Google can change or block them at any time, and using them may not be within Google's terms; the page says so before you import. If the sign-in has expired, the page shows the login command instead of notebooks.

## Data and network

Cards, drafts, the review log, practice answers, exercises and guided lessons are saved atomically to `study.json` in the extension data directory on the hub. Practice material, lesson material, answers, the learner profile and the model's judgements go only to the hub's model (`models`). The hub program reaches only `api.notion.com`. The node program reaches NotebookLM's Google hosts, through the `notebooklm` command, and reads files only in the configured vault and graph.
