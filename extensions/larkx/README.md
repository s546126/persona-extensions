# Feishu through LarkAgentX (larkx)

Feishu as yourself, through [LarkAgentX](https://github.com/cv-cat/LarkAgentX): sign in to the Feishu web client with a QR code, receive every message over its long connection into the inbox and the knowledge base, and send as yourself after you confirm the full text on the phone. Write `/agent <what to do>` in any chat to hand it to the assistant, whose answer appears under it, as you, marked as an AI answer; the setting **/agent 触发前缀** (`agent_prefix`) changes that trigger prefix (must start with `/`, not `//`, be 2–32 characters with no whitespace, control characters or colons; blank or invalid values keep `/agent`), and **/agent 回复末尾** (`agent_reply_suffix`) changes that closing line, and a blank one or one shorter than 4 characters keeps the default, so an answer is always marked. A Python program; Persona needs no change.

Read this before installing:

- **Unofficial protocol.** LarkAgentX reverse-engineers the Feishu web client. It can break whenever Feishu changes, it may not fit Feishu's terms of use, and the account can be flagged. Whether to use it is the account owner's call; for a company account, check the company's rules first.
- **Not in the package.** LarkAgentX has no license, so neither this repository nor the package carries its code. On first start the program downloads it from GitHub at the commit in [`src/larkx_persona/upstream.json`](src/larkx_persona/upstream.json) and checks every file's SHA-256, then installs its dependencies from the hash-locked [`requirements.txt`](requirements.txt).
- **The sign-in is a full web session.** It stays in the hub's encrypted secret store, never in a file or the log.

- [Extension guide](../../docs/extensions/larkx.md)
- [Installation](../../docs/installation.md)
- [Development and tests](../../docs/development.md#add-a-python-extension)
- [Manifest](persona-extension.json)

## Send real mentions

`larkx_send` accepts `mentions: [{id, name}]` using the exact person or bot and numeric id returned by `larkx_members` or `larkx_search`; write `@name` literally in `text` for each target. It builds a rich-text AT element and the notification user-id list, so the target gets a real mention rather than plain text. Without `mentions`, an @name stays ordinary text and the original send path is unchanged. Assistant replies resolve exact names from the chat's member/bot roster (via `PullChatChattersRequest`) as well as recent senders and historical mentions; unknown or ambiguous names are never guessed.

## Invoke the assistant by mentioning yourself

Write your configured trigger prefix (default `/agent`, set via `agent_prefix`), such as `/agent <what to do>`, or mention your own signed-in Feishu account with a real rich-text @ tag in your own message. The @ tag is removed from the request. A mention without any other text uses the same context-based request as an empty trigger-prefix message.

Only your own fresh live messages qualify: another person mentioning you does not invoke the assistant, and a reply ending with the configured AI-answer marker is ignored. Typing an `@name` as plain text is not a mention. Set **@自己唤起助手** (`agent_mention`, enabled by default) to `false` to allow only the configured trigger prefix (`agent_prefix`, `/agent` by default). Answers to either kind of request appear under the original message with the configured AI-answer marker.

## Topics in groups

In a group, a request outside a topic starts a new assistant conversation, and its answer opens a topic under it. A later request in that topic continues the same conversation; sending `/new` with the configured trigger prefix (for example `/agent /new`) there starts the topic over. A topic group works the same way, one conversation per topic. Requests in direct chats go to the channel's target, the main chat by default. Set **群话题各开对话** (`agent_group_topics`, enabled by default) to `false` to send group requests there too. This needs a hub with per-thread conversations for extension channels; an older hub sends every request to the target.

## Command hooks

List your own commands, such as `/todo` and `/note`, in **/命令 Hook** (`command_hooks`) and set **Hook 地址** (`command_hook_url`). Your own fresh message that starts with one of them then goes to that address as a JSON POST instead of to the assistant, once per message, without a model call: an `https://` address anywhere, or `http://` on the hub's machine (reached as `host.persona.internal` in the sandbox). The same guards as `/agent` apply: other people's messages, fetched history and AI answers never fire a hook. Set **Hook 签名密钥** (`command_hook_secret`) to sign every request with `X-Larkx-Signature: sha256=<HMAC-SHA256>`; **Hook 附带上文** and **Hook 附带引用** add the messages before the command and the one it replies to. The payload, the signature and the retries are in the [extension guide](../../docs/extensions/larkx.md#command-hooks).

## Pushes

Turn on the channel's "Send notifications here too" (推送也发到这里) or "Sync main chat" (同步主对话) switch in the app to get what comes after an answer, such as a background task's report. Pushes go only to your own Feishu chat with yourself, never to a group or anybody else's chat, and end with the AI-answer marker.
