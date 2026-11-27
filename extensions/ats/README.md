# ATS

An agent task queue for Persona. The assistant queues a task for a to-do (asking you first, every time); the queue picks the moment by priority, difficulty and remaining quota, runs the task as a Persona agent session in a git worktree of its own on a computer you chose, runs the task's acceptance command there, and writes the outcome with its evidence back to the to-do.

The extension is self-contained: one binary, no external scheduler, no account. It uses only the hub's public extension interfaces.

- [Manifest](persona-extension.json)
- [Skill](skills/ats-todo/SKILL.md)
- [Assistant gate](assistant/gate.json)
- [Installation](../../docs/installation.md)

## Set up

On the home page before setup (see below), or in the extension's settings:

1. **Computers** (`machines`): the computers tasks run on. The extension's program is installed on each. With none selected, tasks queue and do not run.
2. **Allowed repository directories** (`repos`): tasks run only in git repositories under these directories (absolute paths or `~/…`). Empty, no task can be queued.
3. Optional settings include `write_permission_mode` (`acceptEdits` by default), tasks at a time per computer, timeouts, retry count, the quota reserve line and a model per Claude tier.
4. Read-only session permissions (`readonly_permission_mode`) default to `plan`. If a local gateway stalls on approval for read-only Bash commands, select `bypassPermissions` when 「Bypass 自动允许」 is enabled and read-only tasks run in isolated worktrees.

## How a task runs

One binary has two roles. `ats hub` is the hub program: the queue (one JSON file in the extension's data directory, written by this program only), its dispatcher, the home page and the HTTP API. `ats node` is the program on each selected computer: it makes and removes worktrees and runs acceptance commands.

| Step | What happens |
| --- | --- |
| queued | The scheduler admits tasks by priority (`p0`–`p3`), difficulty (model tier `fast`, `standard`, `strong`), waiting time, free slots on the online computers and quota. Below the reserve line only `p0` and `p1` go; with the quota used up nothing goes until it resets. |
| preparing | The computer's program resolves the ref to a commit and checks it out into a new worktree in its data directory (a detached checkout for a read-only task, a branch `ats/<task>-<attempt>` for one that may write). |
| working | The dispatcher starts a Persona agent session in the worktree, attached to the to-do, and watches it. A session that waits for you (a question, an approval) or ends its turn saying it is blocked shows as **needs you** at the top of the home page, with what it asks for (the command it wants to run, its question, or what it said it is blocked on); answer in the session and the task goes on. |
| accepting | The computer's program runs the acceptance command in the worktree. Exit code 0 is a pass. A task with no acceptance command ends as "ran, not verified". |
| finished | Changes the agent of a writing task left uncommitted are committed by ATS on the task's branch `ats/<task>-<attempt>`, without running hooks or signing (nobody is there to answer them), and the worktree is removed (kept when that commit cannot be made); the branch, the commit and the session stay as evidence. The outcome is written to the to-do as a decision, once (`ref: ats:<task>:<state>[:<verdict>]`). |

A failed attempt is retried with a growing delay, up to `max_retries` times, as a new attempt with a new worktree, when another attempt can end differently: the acceptance command failed or the agent broke off (`task_failed`), a timeout (`timeout`), the computer or its environment (`infra_error`), a rate limit (`rate_limited`), a session that went away (`lost`). A failure that would repeat is not retried and the task ends at once: the model or the configuration is not available (`config_error`), the agent is signed out (`auth_expired`), the repository or the ref is wrong (`rejected`).

A retry does not start blind. Its prompt carries how the previous attempt ended (the reason and the end of the acceptance command's output), and a writing task's new branch starts from the previous attempt's commit, so the agent goes on from the work that is there. Every agent is asked to end its reply with a `Summary:` part; that part (or the beginning of the reply when there is none) is kept with the attempt and goes into the to-do decision.

Each attempt carries a fence token, so a late report of an old attempt never overwrites a newer one. The dispatcher keeps all of its state in the queue file, so a restart picks every attempt up where it was.

## The home page

**ATS 自动做待办** in the main navigation. All of its text is plain Chinese; scheduler terms (priorities `p0`–`p3`, tiers, admission modes, error classes, ids) appear only in a task page's technical details.

- At the top, one line on what ATS does and how to hand it work (ask the assistant in a to-do).
- Until a computer and an allowed folder are set, the page is three steps done on the page itself. Step 1 lists the online computers ATS can work on (not awaiting the owner's confirmation; macOS, Linux, or a system the hub has not reported yet), each with a **用这台** button; a single one is picked already, and the page says so. Step 2 takes a folder in a field or from the suggestions `~/code` and `~/src`, with one line on why. Step 3 names the computer and the folder picked and opens this extension's settings (`persona://settings/extensions/ats`) to save them there. A step already saved is marked done. The host lets no extension write its own settings, and the program on the computer reads the folder allowlist from those settings only, so the picks stay a draft of the page until they are saved. Saving the settings restarts the program; once it is up it asks the apps to draw both pages again, so the usual home page follows the save without waiting for the next refresh. The suggestions carry their folder in the button's action id, so they also work in apps that send no action values.
- Once set up: one line on how things stand (taking work, low on quota so only urgent and important tasks run, out of quota until a time, or no computer online), then **needs you** (what each task waits for and where to answer), **being done and queued**, **recently finished**, the counts in words, and the chosen computers (online or not, how busy) with the allowed folders. The remaining quota is shown only when the hub has a gateway account to read it from.
- Button errors are told in Chinese with what to do next; an error it does not know keeps its original text after the sentence.

A card on the Overview page, titled **ATS 自动做待办 · 概况** (the hub's settings page lists each page and card by its title, so the card's differs from the home page's), shows the same in a glance and opens the home page when tapped. Before setup it says which of the two steps is missing and that the page it opens sets them, and how many earlier tasks have not ended. Once set up it shows the counts (**needs you**, **being done**, **queued**, and **not done** when any failed) and one next step: the first task that waits for you and what it asks, else no computer online, else low or used-up quota with the time it comes back (only when quota was measured), else that nothing needs you.

Tapping a row opens the task's page and never changes the task. The page leads with where the task stands and what to do next, then what the agent said it did, how the result was checked (the command, passed or not, the end of its output), where the changes are (branch and commit, never pushed), what was asked, the facts, every attempt, and last the technical details (ids, model tier, start commit, exit code, the raw error). **Cancel** (asks first) and **retry** are on the page and in a row's menu and under its swipe.

The words in the to-do decision and in the API are unchanged.

## What is written to the to-do

One decision per final outcome:

| Field | Content |
| --- | --- |
| question | `ATS 任务「<title>」的结果` |
| choice | Passed acceptance, ran with no acceptance command, failed with the reason in plain words and the number of attempts, canceled, or expired |
| reason | The agent's summary of what it did; the acceptance command and its exit code (only when it ran, otherwise what stopped the task); the branch and commit; the task's id |

The decision's `ref` makes the write idempotent: the same outcome is never written twice.

## Safeguards

- **The user confirms every task.** The assistant reaches the extension through a gated command; `POST /tasks` asks every time, reads pass, everything else is refused. The confirmation says that an agent will run unattended on your computer and shows the command; the skill has the assistant say first what it is about to queue (title, repository, read-only or writing, acceptance command). The credential stays out of the assistant's sight.
- **Read-only by default.** Read-only tasks use a detached checkout and receive strict read-only instructions in either permission mode. A task can request `permission_profile: "default"` to work on its own writable branch, which the confirmation shows. Writing tasks never push.
- **Repository allowlist.** Checked when the task is queued, and again with real paths (symlinks resolved) by the program on the computer, whose answer counts.
- **Bounded.** At most `max_queued` queued tasks (`too_many_queued`), 5 unfinished tasks per to-do (`too_many_for_todo`), one task at a time per computer unless set otherwise, a timeout for the agent and for the acceptance command (killed with its process group).
- **A task always belongs to a to-do.** `todo_id` is required and the caller cannot set the `persona_todo` label itself.

## HTTP API

Served by the hub at `/api/v1/extensions/ats/…`.

| Request | Answer |
| --- | --- |
| `GET /tasks?todo_id=&state=&priority=&limit=&after=` | Tasks, newest first, and the scheduler's state (admission mode, remaining quota, counts). A task is listed in short: id, title, state, what it waits for and when, with fields at their default left out (`priority` `p2`, `permission_profile` `readonly`, `agent_type` `claude`, a first attempt, the verdict of a task that has not succeeded, the `todo_id` asked for); `view=full` lists every field. With more tasks than `limit` (20 by default) the answer has `next`, which goes in `after` for the next page |
| `GET /tasks/{id}` | The task and its attempts. A prompt over 240 characters is cut to its beginning, with `prompt_bytes` the length of the whole, and an attempt shows when it ran, where and how it ended; `?view=full` has the whole prompt and every field of the attempts |
| `GET /tasks/{id}/evidence` | The conclusion first: `verified`, state, verdict, then the acceptance command, the reference of the decision written to the to-do, and the last report (exit code, the agent's summary, artifacts, the last 600 characters of the output with `message_bytes` the length of the whole). `?attempt=N` has the whole report of that attempt |
| `POST /tasks` | Queues a task. `todo_id`, `prompt`, and `repo` are required. Optional fields are `ref`, `title`, `acceptance`, `agent_type` (`claude` by default, also `codex` and `agy`), `model`, `launcher`, `permission_profile`, `priority`, `difficulty`, `machine`, `qos`, `pool`, `tags`, `labels`, and `idempotency_key`. |

- `model` overrides the model for the selected engine. Model IDs are at most 128 bytes after trimming and cannot contain whitespace or control characters. When the field is omitted, Claude uses its configured tier model if present, and other engines use their own defaults.
- `launcher` accepts a launcher name or a `machine=launcher` map separated by commas, semicolons, or newlines, such as `default-launcher,studio=local-launcher`. The first bare name is the map's fallback. When the task specifies no launcher for its machine, Claude uses the extension's launcher setting and other engines use their engine name. The specification is limited to 512 bytes after trimming, with line feeds allowed as map separators.
- The task API shows the requested overrides while the task is queued. Once a session starts, it shows the resolved session values when available. Replaying an idempotency key returns the original task.

## Permissions

| Permission | Used for |
| --- | --- |
| `todos.write` | Attaching a task's session to its to-do and writing the outcome decision |
| `agents.read`, `agents.control` | Starting, watching and interrupting the task's agent session |
| `machines.read` | Which of the selected computers are online |
| `extensions.call:ats` | The assistant's gated command calling this extension's API |
| `usage_gateways` | Reading the usage windows of the hub's gateway accounts for quota-aware admission |
| `node.permissions.files` | Worktrees and acceptance commands on the selected computers |

## Development

```sh
go -C extensions/ats test ./...
scripts/package.sh ats
```

Version 0.4.0 replaces the stdio MCP server and the external worker interface of 0.3.0, which could queue tasks but had nothing to run them. Version 0.5.0 stops retrying failures that would repeat, lets a retry go on from the previous attempt, adds the task page and rewords the queue page, the to-do decision and the confirmation. Version 0.6.0 lets a new task choose an agent engine such as `codex`, keeps `claude` as the default for omitted values and older queue records, and carries the choice through attempts and retries. Version 0.7.0 rewrites the home page, the task page and the settings in plain Chinese: a setup checklist before anything else, one status line, priorities and outcomes in words, technical details last. Version 0.7.1 adds the Overview card. Version 0.7.2 makes the assistant skill use the page, setting and button names the pages show since 0.7.0. Version 0.7.3 titles the Overview card 「ATS 自动做待办 · 概况」, so its switch in the settings no longer looks like a duplicate of the home page's. Version 0.7.4 sets ATS up on the home page: it lists the online computers to pick from, takes the folder with suggestions, opens the settings to save, and cuts the intro to one line. Version 0.7.5 cuts what the assistant reads to follow a task by about a third: a shorter skill and tool description; `GET /tasks` lists tasks in short and in pages; `GET /tasks/{id}` answers with the beginning of a long prompt; `GET /tasks/{id}/evidence` puts the conclusion first and keeps the end of the output; `view=full` and `attempt=N` have the rest. A retry is now told how the earlier acceptance command exited (the line was cut off after a long output). `go -C extensions/ats test -run TestTokenCost -v .` prints what each of these costs in bytes and estimated tokens and holds each to a budget.

## Version 0.7.6

This release improves scheduling across computers and records more evidence from each attempt. It adds per-task model and launcher overrides, uses each non-Claude engine's own default model, and adds the opt-in `readonly_permission_mode` setting for unattended inspections.

The full task view (`?view=full`) and per-attempt report (`?attempt=N`) carry the stored agent reply and worktree commit SHAs. The compact views include `dirty` when ATS inspected the worktree.
