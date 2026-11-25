# English by Default (Claude Code)

[Documentation](../README.md) · [Source](../../extensions/english-by-default/)

A habit tool for people who want to write to their coding agents in English. On the computers you choose, the extension adds a Claude Code `UserPromptSubmit` hook. When a prompt is written in another language, the hook handles it by the mode you set, and keeps the original with its English as a pair for later review.

## Modes

| Mode | What happens to a prompt in another language | Model calls |
| --- | --- | --- |
| Block (`block`) | Held back. Rewrite it in English: the hook asks the model whether the rewrite says the same thing and lets it through at the threshold score (70 by default); otherwise it is held back again with a hint. Typing a give-up word (`giveup`, `give up`, `放弃`, `算了`) shows the English and sends the original as context instead | One per rewrite, one per give-up |
| Warn (`warn`, default) | Goes through; the English translation is shown and given to the model as context | One per prompt |
| Log only (`log`) | Goes through silently; only the original is saved | None |

A blocked prompt waits 30 minutes for its rewrite, per Claude Code session. Prompts that start with `/` or `!` (slash commands, shell escapes) go through unless a rewrite is awaited, and so do prompts containing one of the urgent words (`紧急`, `urgent`, `hotfix`, `incident` and others; editable), which are saved for later.

## Detection

A prompt counts as written in another language when, after fenced and inline code is removed, it has at least two characters of a non-Latin script and they make up at least 30% of its letters, a Han, kana or Hangul character weighing as three Latin letters. An English prompt that names a Chinese file or UI string stays English. Languages written in the Latin alphabet (Spanish, German, French and the like) are not detected: telling them apart needs word lists, and a wrong block costs more than a missed one.

## The model

Judging and translating run the configured command on that computer (`claude -p --model haiku` by default). The prompt is passed on standard input, so it does not appear in the process list; the command runs in the temporary directory, so no project's `CLAUDE.md` or transcript is touched, and with `EBD_INTERNAL=1`, so its own prompt is not gated. When the command fails or times out (90 seconds), the prompt goes through: the hook fails open.

## What is stored, and where

In the node program's data directory on each computer: `queue.jsonl` (the pairs, newest 1000; each prompt cut at 4000 characters), `pending.json` (blocked prompts awaiting a rewrite), `options.json` (the settings the hook reads) and a heartbeat. Nothing leaves the computer except the newest 30 pairs and the counts, which the extension's page asks for through the hub. Prompts can contain anything you typed, so treat the data directory like your shell history.

## Installing and removing the hook

The node program adds one `UserPromptSubmit` group with the hook alone in it to each configured Claude config directory's `settings.json` (`~/.claude` by default; add the directories you use with `CLAUDE_CONFIG_DIR`). Other keys and hooks keep their order and values, the file's indent is kept, a copy is saved as `settings.json.english-by-default.bak` before each change, and a file that is not a JSON object is left alone. The entry is removed when the extension is switched off for that computer, disabled, upgraded or uninstalled. If the program is killed before it can do that, the heartbeat goes stale within five minutes and the hook does nothing.

## Limits

- Claude Code only. Codex has a `UserPromptSubmit` hook of the same shape; its input fields and session ids have not been verified, so it is not installed yet.
- Persona's own assistant conversations (`PERSONA_ASSISTANT=1`) are not gated.
- When a prompt sent from a Persona client is held back, Claude Code shows the reason in the terminal. Whether the client shows it as well depends on the Persona version and has not been verified.
- Review (quiz, spaced repetition, Feynman) is not part of this version; the pairs keep a review date for it.
