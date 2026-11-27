# English by Default (Claude Code)

[Documentation](../README.md) · [Source](../../extensions/english-by-default/)

A habit tool for people who want to write to their coding agents in English. On the computers you choose, the extension adds a Claude Code `UserPromptSubmit` hook. When a prompt is written in another language, the hook handles it by the mode you set, and keeps the original with its English as a pair for later review.

## Modes

| Mode | What happens to a prompt in another language | Model calls |
| --- | --- | --- |
| Block (`block`) | Held back. Rewrite it in English: the hook asks the model whether the rewrite would make the agent do the same thing and lets it through at the threshold score (70 by default); otherwise it is held back again with a one-sentence hint. Typing a give-up word (`giveup`, `give up`, `放弃`, `算了`) shows the English and sends the original as context instead | One per rewrite, one per give-up |
| Warn (`warn`, default) | Goes through; the English translation is shown and given to the model as context (the original stays the request) | One per prompt |
| Log only (`log`) | Goes through silently; only the original is saved | None |

A blocked prompt waits 30 minutes for its rewrite, per Claude Code session. Prompts that start with `/` or `!` (slash commands, shell escapes) go through unless a rewrite is awaited, and so do prompts containing one of the urgent words (`紧急`, `urgent`, `hotfix`, `incident` and others; editable; a Latin word must stand alone), which are saved for later.

## Detection

The question is which language you wrote in, not which characters the prompt contains: a Chinese sentence often carries English terms, links and paths (`把 handleRequest 的 timeout 改成 30s`), and an English one often quotes Chinese (`Change the label "登录" to …`). So the detector:

1. Sets aside what you quoted rather than wrote: fenced and inline code, code lines and comments pasted without a fence, text in quotes (`"…"`, `“…”`, `「…」`, `«…»` and others), XML-style tag blocks (Persona's selected text), block quotes, log messages (`error: …`, `fatal: …`), links, paths and e-mail addresses, and everything after an English line that ends in a colon (`Translate this into English:` and a paragraph).
2. Weighs what is left: text in a script that is not Latin, counted as words (a Han character 0.6, a kana or Hangul syllable 0.4, a word of an alphabet 1), against Latin words, with identifiers and acronyms (`handleRequest`, `de_id`, `PR`) counting half.
3. Looks at the grammar. With no English function words (`the`, `to`, `why`, `please` …), any text in another script means you did not write in English (`开 PR`, `跑 scripts/ci.sh`, `好`); with them, the other script must make up 40% of the words (`please 帮我看看 this function`).
4. Tells Latin-script languages (Spanish, French, German, Portuguese, Italian, Dutch, Vietnamese, Turkish, Polish, Indonesian) by their function words: at least two, and more than twice the English ones; a short reply of up to three words needs one (`Sí, continúa`, `Ja, mach weiter`). A loanword or a name (`café`, `São Paulo`, `La Liga`) does not count.

The labeled corpus in `testdata/` measures it (`corpus_test.go`), and the test fails if any case regresses:

| Corpus | Version 0.1.0 detector | This detector |
| --- | --- | --- |
| `detect.jsonl`, 133 prompts the rules were tuned on | 68.4% (13 English prompts blocked, 29 missed) | 100% |
| `holdout.jsonl`, 71 prompts written afterwards | 76.1% (7 blocked, 10 missed) | 97.2% when first run, 100% after two fixes |

The corpora cover English, English quoting other languages, Chinese with English terms, links and paths, short replies, Japanese, Korean, Cyrillic and other scripts, and Latin-script languages. They are written for this tool, so real prompts will find cases they miss.

## The model

Judging and translating run the configured command on that computer (`claude -p --model haiku --no-session-persistence` by default). The prompt is passed on standard input, so it does not appear in the process list; the command runs in the temporary directory, so no project's `CLAUDE.md` is touched; `--no-session-persistence` keeps each call from leaving a session transcript behind; and `EBD_INTERNAL=1` keeps its own prompt from being gated. The texts are put in tags and the model is told they are data, so a rewrite cannot talk the judge into a pass and a question is translated rather than answered.

The judge lists what the original asks for (the action, its target, every constraint, number, name and negation) and checks each in the rewrite; tone, politeness and word order do not count. `judge_live_test.go` measures it with the real model (`EBD_LIVE_JUDGE=2 go test -run TestJudgeLive -v`) on 36 pairs, half of them rewrites that drop, change, reverse or add a requirement or try to instruct the judge, plus an 18-pair holdout: version 0.1.0's judge got 106 of 108 answers right with haiku (it failed `继续` → `Continue.` and a faithful rewrite that kept the request but not its hedging), this one 108 of 108, and 54 of 54 in a third round with the default command, every correct rewrite scoring 90 or more and every wrong one 55 or less. When the command fails or times out (90 seconds), the prompt goes through: the hook fails open.

## In sessions Persona runs

The hook works in Claude Code sessions that Persona's node starts on the chosen computers (Persona puts `PERSONA_HOME` in front of their command, which is how the hook tells them apart), with two differences, because Persona's apps show the conversation from its transcript:

- **The English goes at the top of the reply.** Persona's apps do not show a hook's messages or context, so in warn mode, and when a first prompt is let through (below), the model is asked to begin its reply with `English: …`. Switch it off with `echo_in_reply`.
- **A session's first prompt is not held back.** Persona types a first prompt again when it does not reach the transcript, so blocking it would send it three times and report the session as stuck; it is handled as in warn mode. Later prompts are held back as anywhere else, and Claude Code writes the reason to the transcript as a notice, which Persona's apps show.

Checked with Claude Code 2.1: a blocked prompt is recorded as a system line of subtype `informational` with the reason and no user line, which Persona's transcript reader turns into a notice; in warn mode the hook's message and context are recorded as attachments, which it drops, and the reply starts with the `English:` line. Not checked on a phone: how the notice looks there, and whether Persona's input box is left with the blocked text after a block in a terminal session.

Persona's own assistant conversations (`PERSONA_ASSISTANT=1`) are not gated.

## What is stored, and where

In the node program's data directory on each computer: `queue.jsonl` (the pairs, newest 1000; each prompt cut at 4000 characters), `pending.json` (blocked prompts awaiting a rewrite), `options.json` (the settings the hook reads) and a heartbeat. Nothing leaves the computer except the newest 30 pairs and the counts, which the extension's page asks for through the hub. Prompts can contain anything you typed, so treat the data directory like your shell history.

## Installing and removing the hook

The node program adds one `UserPromptSubmit` group with the hook alone in it to each configured Claude config directory's `settings.json` (`~/.claude` by default; add the directories you use with `CLAUDE_CONFIG_DIR`). Other keys and hooks keep their order and values, the file's indent is kept, a copy is saved as `settings.json.english-by-default.bak` before each change, and a file that is not a JSON object is left alone. The entry is removed when the extension is switched off for that computer, disabled, upgraded or uninstalled. If the program is killed before it can do that, the heartbeat goes stale within five minutes and the hook does nothing.

## Limits

- Claude Code only. Codex has a `UserPromptSubmit` hook of the same shape; its input fields and session ids have not been verified, so it is not installed yet.
- A prompt that mixes languages evenly is a judgment call; the thresholds lean towards letting it through.
- Review (quiz, spaced repetition, Feynman) is not part of this version; the pairs keep a review date for it.
