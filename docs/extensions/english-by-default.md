# English by Default (Claude Code and Codex)

[Documentation](../README.md) · [Source](../../extensions/english-by-default/)

A habit tool for people who want to write to their coding agents in English. On the computers you choose, the extension adds a `UserPromptSubmit` hook to Claude Code and, where it is installed, to Codex CLI ([Codex](#codex)). When a prompt is written in another language, the hook handles it by the mode you set, and keeps the original with its English as a pair for later review.

## Modes

| Mode | What happens to a prompt in another language | Model calls |
| --- | --- | --- |
| Block (`block`) | Held back. Rewrite it in English: the hook asks the model whether the rewrite would make the agent do the same thing and lets it through at the threshold score (70 by default); otherwise it is held back again with a one-sentence hint. Typing a give-up word (`giveup`, `give up`, `放弃`, `算了`) shows the English and sends the original as context instead | One per rewrite, one per give-up |
| Warn (`warn`, default) | Goes through; the English translation is shown and given to the model as context (the original stays the request) | One per prompt |
| Log only (`log`) | Goes through silently; only the original is saved | None |

A blocked prompt waits 30 minutes for its rewrite, per Claude Code or Codex session. Prompts that start with `/` or `!` (slash commands, shell escapes) go through unless a rewrite is awaited, and so do prompts containing one of the urgent words (`紧急`, `urgent`, `hotfix`, `incident` and others; editable; a Latin word must stand alone), which are saved for later.

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

## Codex

The node program also adds the hook to each Codex home in `codex_dirs` (`~/.codex` by default, passed over on a computer without it; an empty list turns Codex off), in its `hooks.json`, with the same care as `settings.json`: the group is added at the end of the `UserPromptSubmit` list beside other tools' hooks, a copy is saved as `hooks.json.english-by-default.bak`, putting it in again changes nothing, and it is taken out on the same occasions. `config.toml` is never written.

Checked with codex-cli 0.171.0-alpha.13.2 in a scratch `CODEX_HOME` (`codex exec`):

- Codex reads Claude Code's hook layout from `<CODEX_HOME>/hooks.json`; the `hooks` feature is stable and on by default in this version (older versions need it switched on in `[features]`; the extension does not do that).
- The hook gets the same input as from Claude Code (`hook_event_name`, `session_id`, `prompt`, `transcript_path`, `cwd`, plus `model`, `permission_mode` and `turn_id`) and Codex takes the same answer: `{"decision":"block","reason":…}` holds the prompt back (the model is not called; `codex exec` prints `hook: UserPromptSubmit Blocked`), and `hookSpecificOutput.additionalContext` and `systemMessage` let it through with them. Codex's output schema allows no other field; the tests hold the hook to it.
- **Codex runs a hook only after you have reviewed it.** Until then it skips the hook without a word, so the prompt goes through. Type `/hooks` in Codex and approve the entry; a changed entry needs approving again, and since each version of the extension runs from its own path, so does each upgrade. Codex keeps the approval in `config.toml` (`[hooks.state."<hooks.json>:user_prompt_submit:<group>:<handler>"]`); the extension never writes it, because approving its own hook would skip the review Codex asks of you. The page reads whether the entry's slot has an approval on record and says **Review in Codex** when it has none. The record is keyed by the group's place in the list, so another tool's group after ours is moved when ours is taken out and Codex asks to review it again.
- A Codex transcript (its rollout) starts with Codex's own `<environment_context>` message; a user turn is a user message that is not such a block. This is how a session's first prompt is told apart in sessions Persona runs.

Not checked: the interactive TUI (how the block reason and the system message look there, and the `/hooks` approval screen), Codex sessions that Persona's node starts and how Persona's apps show them, and a Codex-only computer. The default model command is `claude`; on a computer without it, set `llm_command` to a command that reads a prompt on standard input and prints the answer (until then block mode cannot judge rewrites and lets them through, and warn mode has no translation).

## Limits

- Codex: see [Codex](#codex) for what has and has not been checked.
- A prompt that mixes languages evenly is a judgment call; the thresholds lean towards letting it through.
- Review (quiz, spaced repetition, Feynman) is not part of this version; the pairs keep a review date for it.
