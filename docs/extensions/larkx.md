# Feishu through LarkAgentX (larkx)

[Documentation](../README.md) · [Source](../../extensions/larkx/)

This extension (id `larkx`) reaches Feishu as the user, next to the hub's own `lark-cli` integration (nwverse/persona#1487). It runs [LarkAgentX](https://github.com/cv-cat/LarkAgentX), a Python library for the Feishu web client, as a connector program: the user signs in with a QR code, every message arrives over the web client's long connection, the assistant can send as the user after a confirmation on the phone, and a trigger prefix (`/agent` by default, configurable via `agent_prefix`) or a self-mention in any chat hands a request to the assistant. It is an ordinary connector and channel program (Persona's `docs/connector-protocol.md` and `docs/extensions/channels.md`), so the hub needs no change.

This page describes version 0.4.1. When the manifest shown in the install preview disagrees with this page, the manifest wins.

| Extension point | Contribution | Behavior |
| --- | --- | --- |
| `connectors` | `larkx` (飞书（LarkAgentX）) | Capabilities `status`, `tools`, `actions`, `events`, `inbox`, `archive`, `login`, `threads`, `fallback`. Inbox kinds `dm`, `mention`, `group`. `larkx_*` actions are `confirm_always`. |
| `assistant` | `larkx` | Instructions in `assistant/instructions.md`: when to use `larkx_*` and when `lark-cli`. |
| `channel` | `larkx` (飞书 /agent) | Kind `im`, on the `larkx` connector, with `threads` and `push`: the user's own messages starting with the configured `agent_prefix` (`/agent` by default) or mentioning themself reach the assistant, and the answers go back under them. Each group topic has a conversation of its own; direct chats use the channel's target. Pushes go only to the user's chat with themself. Plain text, at most 4,000 characters. |

## Before you use it

- **Unofficial protocol.** LarkAgentX reverse-engineers the Feishu web client's undocumented protocol. It can stop working whenever Feishu changes it, it may not fit Feishu's terms of use, and the account may be flagged by Feishu's risk controls. Whether to use it is the account owner's decision; for a company account, check the company's rules first.
- **The sign-in is a full web session.** Whoever holds it can read and write Feishu as the user. It is stored through `connector.secrets.*`, encrypted with the hub's key, and never written to a file or the log. To revoke it, sign out in the connector settings or remove the device under Feishu's signed-in devices. Signing out cancels startup restoration and its retries; an in-flight backend request finishes before sign-out returns, and cannot reactivate the old account. Starting a new QR sign-in also cancels restoration of the saved session.
- **It runs as the hub's system user.** LarkAgentX's code can read and write the files that user can; where the hub enforces it, its network is limited to the manifest's `permissions.network`. LarkAgentX is pinned to one commit and checked file by file, so an upstream change reaches the hub only through a new release of this extension.
- **Messages are stored in plain text** in the extension's data directory (`larkx.db`, mode 0600) and deleted when the extension is uninstalled. Messages older than 30 days are deleted once the knowledge base has archived them.

## LarkAgentX and its license

LarkAgentX has no license. Neither this repository nor the extension's package carries its code: the package holds only this extension's own program. On first start the program downloads LarkAgentX from GitHub (`codeload.github.com`) at the commit pinned in `src/larkx_persona/upstream.json`, keeps only the files listed there, and checks each one's SHA-256; a missing or different file fails the start and nothing is kept. The copy lives in the extension's data directory (`larkagentx-<commit>/`); a new pinned commit replaces it.

Its dependencies are installed the same way, with `pip install --require-hashes` from PyPI into the data directory (`deps-py3X-<hash>/`). `requirements.txt` pins every package, the transitive ones too, with the SHA-256 of each of its files on PyPI. Directories left by earlier Python versions or requirement files are removed.

## Two ways to reach Feishu

| | `lark-cli` (hub integration) | LarkAgentX (`larkx`) |
| --- | --- | --- |
| Interface | The official open platform | The reverse-engineered web protocol |
| Identity | An app, with the user's authorization | The user |
| Setup | Install and authorize `lark-cli` on the hub's machine | Install the extension, scan |
| Every message, live | No (IM events go to bots only) | Yes (long connection) |
| Sends as | A bot | The user, after a phone confirmation |
| Inbox and knowledge base | No | Yes |
| `/agent` in any chat | No | Yes |
| Docs, calendar, minutes | Yes | No (messages only) |

They can be installed together and know nothing of each other. `larkx` does not take over the hub's `lark` connector.

## Install and sign in

The machine the hub runs on needs Python 3.9 or newer with pip. Install the extension from this market (see [installation](../installation.md)), then:

1. On first start the program downloads LarkAgentX and installs its dependencies, which takes a minute or two. Meanwhile the connector's status reads 正在准备运行环境 ("preparing the runtime"). If it fails, the status says why and asks for Python 3.9+, pip, and access to PyPI (`pypi.org`, `files.pythonhosted.org`) and GitHub (`codeload.github.com`).
2. Open 飞书（LarkAgentX） in the connector settings and scan the QR code with the Feishu app. A code expires after about a minute and is replaced.
3. Optionally set 站点 (site: Feishu or Lark; changing it needs a new scan) and 分拣规则 (your own triage rules, used instead of the hub's global ones).

## What it does

| Capability | How |
| --- | --- |
| Sign-in | `login.step` with `kind: "qr"`; the code comes from LarkAgentX's `QrLogin` and is replaced when it expires. |
| Receiving | LarkAgentX's long connection, reconnecting with a 1–60 s backoff. After a reconnect it catches up on the 30 chats that moved last, up to 50 messages each. |
| Inbox | One item per known group topic. Direct chats and group messages without a known topic keep their chat-level thread. A reply from the user settles only that thread. Direct messages and @-mentions go to model triage. For other group messages, the reminder state determines whether to triage, omit, or fold the item. |
| Thread page | `thread.page` serves what the program has kept and reads older messages from the server by position. Topic pages show locally retained messages from that topic. Pictures come through `thread.media`; files show their names only. |
| Knowledge base | `archive.changes`; the cursor is a local row number, so nothing is delivered twice. The identity hint is `lark`. |
| Assistant tools | `larkx_status`, `larkx_chats`, `larkx_messages`, `larkx_members`, `larkx_search`. |
| Sending | `larkx_send` (to a chat; `reply_to` answers in a message's topic) and `larkx_send_dm` (a direct message to a person). |
| `/agent` and self-mention | The user's own live message that starts with `/agent` (or the trigger prefix set in `agent_prefix`: must start with `/`, not `//`, be 2–32 characters with no whitespace, control characters or colons; blank or invalid values keep `/agent`), or contains a rich-text @ tag for that same signed-in account, goes to the hub with `channel.receive`, with the chat it was written in and the messages just before it. The self-mention is removed from the request; with no other text, the existing empty-request context behavior applies. Only a real @ tag counts, not an `@name` typed as plain text. The setting `agent_mention` is enabled by default; turning it off leaves the trigger prefix available. In a group, the request carries its topic as `thread` (see below; controlled by `agent_group_topics`, enabled by default). The answer comes back through `channel.send` and is posted as the user under the request, ending with an AI-answer mark. Only a reply to one of the user's own requests, or a push (below), is sent (anything else is `cannot_send`), each once. Fetched history, old messages, other people's messages and the answers themselves never trigger it. For POST rich text, the title line does not block the trigger prefix, child elements keep their order, and up to four inline images (3 MB each) are forwarded as channel images. |

**Inbox group triage.** Add the group IDs returned by `larkx_chats` to `inbox_triage_chats` to see ordinary messages from those groups sorted by your Inbox rules, including in muted groups. Leave `triage_rules` blank to inherit the global rules. Ordinary group messages fall back to Noise when the triage model is unavailable. For a topic item, Inbox replies go to the topic root. Existing chat-level items still reply to the chat.

**Topics in groups.** The channel declares `threads`, and a request in a group carries `thread`, `<chat>:<topic root>`: the topic it was written in, or, for a request outside a topic, the request itself, since its answer opens a topic there. In a topic group every topic counts the same way. Alongside `thread`, `channel.receive` also emits additive `chat_id`, `chat_kind` (`direct`, `group`, or `topic`), and `topic_id` (set for topic/thread messages) for hubs that support structured group/topic routing; older hubs ignore those fields and continue using `thread`. Under legacy routing (when `integrations_v2` is off), the hub gives each thread a conversation of its own, so a request outside a topic starts a new assistant conversation, and a later request in that topic continues it. When **Settings › Labs › `integrations_v2`** is enabled on the hub, the channel's `thread_mode` controls routing instead: `channel_default` (the default) sends all requests—including group topics—to the channel's target regardless of `agent_group_topics`; `per_group_and_topic` merges non-topic group requests into one conversation per group, and a follow-up written in the topic opened by the answer starts a separate topic conversation (unlike legacy `thread`, which kept both in the same conversation); `per_topic` isolates only topic messages. `/new` in a topic starts that topic over. Requests in direct chats carry no thread and go to the channel's target (the main chat by default). The setting `agent_group_topics` (enabled by default) turns group/topic routing off (suppressing `thread`, `topic_id`, and group/topic `chat_kind`), so group requests go to the channel target under both legacy and `integrations_v2` routing. A hub without per-thread conversations (nwverse/persona#11939) ignores `thread`, and every request goes to the target.

**Pushes.** The channel declares `push`, so the hub can send what comes after an answer: a background task's report, a main-chat turn started elsewhere, a reminder. It sends them only after the user turns on the channel's "Send notifications here too" (推送也发到这里) or "Sync main chat" (同步主对话) switch in the app; both are off by default. A push has no `reply_to` and may carry anything the assistant knows, so it goes only to the user's own chat with themself (the direct chat opened with their own id), as the user, ending with the AI-answer mark. Before the first push of a sign-in, the program checks that this chat is a direct chat whose member count is exactly 1 and whose owner is the user. The check fails closed: a missing or unreadable count or owner means no push. It never falls back to another chat: when the chat cannot be found or fails the check, the push is `cannot_send`. A send with `reply_to` still has to answer one of the user's own requests (#533).

**Command hooks.** Optional, and off until both **/命令 Hook** (`command_hooks`) and **Hook 地址** (`command_hook_url`) are set. See [Command hooks](#command-hooks): the user's own live message that starts with one of the listed commands goes, as a signed JSON POST, to the user's own address instead of the assistant.

**Writes.** `confirm_always: ["larkx_*"]`: even an `allow` in `connector_gate` is held for the phone confirmation, so the assistant cannot send as the user without them. The confirmation names the recipient, quotes the message a reply answers, and shows the full text. A direct chat seen only through the user's own messages says it does not know the other person yet. A reply the user sends from the app goes through `inbox.reply`, where pressing send is the confirmation.

Each send runs at most once per `request_id` (actions) or `client_id` (replies); an action without a `request_id` is refused. When the result is unknown (the request failed, or the answer has no message id), the program returns an error and never retries, because the message may have gone out.

**Nothing is marked read.** LarkAgentX's own `lark listen` marks handled messages read; this extension does not, so unread counts in Feishu stay as they are.

## Command hooks

Deterministic commands of your own, such as `/todo` or `/note`, in any Feishu chat: the program posts each one to a web address you run, without the assistant and without a model call. It is an alternative to running a second unofficial Feishu client next to this one.

| Setting | Default | Meaning |
| --- | --- | --- |
| **/命令 Hook** (`command_hooks`) | empty (off) | Up to 20 commands: a slash, then a letter or digit, then letters, digits, `_` or `-`. Matched like `/agent`: case aside, followed by a space, a colon, a line break or nothing, so `/todos` is not `/todo`. `/agent` stays the assistant's and is refused. |
| **Hook 地址** (`command_hook_url`) | none | Where commands go: `https://` to any host, or `http://` to this machine's loopback (`127.0.0.1`, `localhost`, `[::1]`). No user name, password or fragment. Redirects are not followed. |
| **Hook 附带上文** (`command_hook_context`) | 0 | How many messages from before the command, in the same chat, go with it (0–20). They may include other people's messages. |
| **Hook 附带引用** (`command_hook_quote`) | on | When the command replies to a message the program has kept, that message goes with it. |
| **Hook 签名密钥** (secret `command_hook_secret`) | none | Kept in the hub's encrypted secret store. When set, every request is signed (below). |

**Which messages.** The same guards as `/agent`: only your own text or rich-text message, received live within 10 minutes of being sent. Someone else's message never fires a hook, even when it mentions you or starts with the command; neither do fetched history (catch-up after a reconnect, older pages), messages ending with the AI-answer mark, or anything else. Each message id fires at most once, across reconnects and restarts. A message that matches a hook is not an `/agent` request too, even with a self-mention in it, and the assistant never answers it. With hooks off, or when their settings cannot work, commands are ordinary messages; the connector's status then says why.

**The request.** An asynchronous `POST` with `Content-Type: application/json`; the long connection never waits for it.

```json
{
  "delivery_id": "<32 hex characters, the same for every try of one message>",
  "command": "/todo",
  "text": "buy milk",
  "raw_text": "/todo buy milk",
  "message": {"id": "<message id>", "chat": "<chat id>", "chat_kind": "group", "chat_name": "<chat name>",
              "root": "<topic root id or empty>", "parent": "<replied message id or empty>", "at": "2026-01-02T03:04:05Z"},
  "quote": {"id": "<message id>", "sender_name": "<name>", "mine": false, "text": "…", "at": "…"},
  "context": [{"id": "<message id>", "sender_name": "<name>", "mine": false, "text": "…", "at": "…"}]
}
```

`text` is what follows the command; `raw_text` is the message from the command on. `quote` and `context` are there only when the settings ask for them and there is something to send. For rich text, a command on the line under the title counts, as for `/agent`.

Headers: `X-Larkx-Timestamp` (Unix seconds of this try), `X-Larkx-Delivery` (the `delivery_id`), and, with a secret, `X-Larkx-Signature: sha256=<hex>`, the HMAC-SHA256 keyed with the secret over the timestamp, a dot and the raw body. A receiver checks it and the timestamp's age, and drops a `delivery_id` it has handled:

```python
import hashlib, hmac, time

def verified(secret: bytes, headers, body: bytes, most_seconds=300):
    timestamp = headers["X-Larkx-Timestamp"]
    want = "sha256=" + hmac.new(secret, timestamp.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, headers.get("X-Larkx-Signature", "")) and abs(time.time() - int(timestamp)) <= most_seconds
```

**Answers and retries.** A `2xx` answer is a delivery. A failed connection, a timeout (10 s per try), `429` or `5xx` is tried again after 2, 5 and 15 seconds, at most four tries in all; any other answer, a redirect included, ends it. The log says only the command and the outcome (`command hook /todo delivered (HTTP 200)`), never the text, the address or the secret. Nothing is posted back to Feishu: a receiver that wants to answer does it its own way.

**Network and sandbox.** The manifest asks for the address you set (`config:command_hook_url`, HTTPS) and for `host.persona.internal`. In the sandbox, an `http://` loopback address is reached as `host.persona.internal` with the same port, which the hub's egress proxy connects to this machine's loopback; an `https://` address keeps its host, so the certificate still matches.

## Not there yet

LarkAgentX's `client.api()` reaches over 2,200 gateway commands, so most of this is wiring:

- Listing every chat: the program knows the chats it has seen a message in since sign-in. Older ones appear with their next message, or through `larkx_search`.
- The other person of a direct chat seen only through the user's own messages (the chat with oneself among them): it is named once they write.
- The app's Feishu chat page (`/api/v1/lark/…`): it would have to take over the `lark` connector's `api`.
- Docs, Moments, pins, reactions, Feishu's own encryption, read state, sending pictures and files.

## Structure

| File | Contents |
| --- | --- |
| `bin/larkx` | The launcher: `python3` with `src/` on the path. Starting needs nothing installed. |
| `src/larkx_persona/wire.py` | One JSON message a line; each request runs on its own thread, so a handler can call back into the hub. |
| `src/larkx_persona/main.py` | Handshake and namespaces. stdout is the protocol only; a library's `print` goes to the log. |
| `src/larkx_persona/connector.py` | Every connector method; knows nothing of LarkAgentX. |
| `src/larkx_persona/hooks.py` | Command hooks: the settings, matching a command, the address, signing, and the POST with its retries. |
| `src/larkx_persona/store.py` | SQLite: `status` and `inbox.collect` answer without the network; send deduplication. |
| `src/larkx_persona/real.py` | LarkAgentX: download and check it, install the dependencies, keep the sign-in in the hub, hold the long connection. Its log carries message bodies, so only where an error happened is kept. |
| `src/larkx_persona/upstream.json` | LarkAgentX's repository, pinned commit, archive URL and each file's SHA-256. |
| `src/larkx_persona/fixture.py` | A fake backend with synthetic data, used only when a test sets `LARKX_PERSONA_BACKEND=fixture` (an installed extension never gets that variable). |

## Tests and verification

```sh
(cd extensions/larkx && python3 -I -B -m unittest discover -s tests)
```

`scripts/ci.sh` runs these (step `python`). `tests/test_connector.py` covers what the conformance test does not call: `actions.do`, `inbox.reply`, failed and uncertain sends, sign-in and expiry, pruning. `tests/test_agent.py` covers `/agent`: what is handed over, what is not, the topic a group request carries, and `channel.send` once per ref. `tests/test_hooks.py` covers command hooks: which messages fire one and which never do, once per message, the payload, the signature, retries and refusals, the address rules and the sandbox name, against a recorder and a local HTTP server. `tests/test_upstream.py` checks the download and its file-by-file check against a synthetic archive, and that `upstream.json` and `requirements.txt` pin everything. `tests/test_real.py` decodes a synthetic push with the real LarkAgentX and checks the sign-in never reaches a file; it needs the directories the program downloads on first start (see the top of the file) and skips without them.

The conformance tests live in Persona, whose `internal/` packages only it can import: the real Python program on the fake backend passes `plugin/conformance.Check` and `CheckProgram`, and the channel passes `channel/conformance.Check`.

```sh
scripts/package.sh larkx   # the packaged manifest: the source one carries the market-only "icon"
LARKX_PERSONA_BACKEND=fixture PERSONA_TEST_CONNECTOR_PROGRAM="$PWD/dist/larkx/bin/larkx" \
  go -C <persona>/server test ./internal/connector/conformance -run ConnectorProgramLive -v
```

A real account can only be checked by hand: scan, receive a message, send one to yourself, `/agent` in a chat, sign out.
