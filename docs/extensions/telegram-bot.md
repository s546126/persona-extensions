# Telegram Bot

[Documentation](../README.md) · [Source](../../extensions/telegram-bot/)

Connect a Telegram bot you own (via `@BotFather` and the official Telegram Bot API) as a Persona chat channel. Unlike the [Telegram](telegram.md) MTProto connector (which reads your personal Telegram account into the Inbox and Knowledge Base), **Telegram Bot** (`telegrambot`) is a bidirectional **chat channel** for talking to Persona from Telegram, receiving pushes, and answering Agent questions.

| Extension point | Contribution | Behavior |
| --- | --- | --- |
| `connectors` | `telegrambot` | Capabilities `status` and `login`: verifies the BotFather token with `getMe`, issues a 6-digit pairing code, and stores encrypted binding state in the hub. |
| `channel` | `telegrambot` (`kind: "im"`, `bind: "fields"`) | Private DM channel to Persona's main chat (`images_in`, `images_out`, `files_in`, `format: "plain"`, `max_text: 4000`, `asks: "text"`, `push`, `typing`). |

## Setup and pairing

1. In Telegram, open `@BotFather`, run `/newbot`, and copy the HTTP API token (`<id>:<secret>`).
2. In Persona, open **Settings → Connectors → Telegram 机器人** (or **Settings → Channels**).
3. Enter the **Bot token** and, if your hub machine reaches `api.telegram.org` through a local HTTP(S) proxy (such as `http://127.0.0.1:7890`), enter **HTTP(S) proxy**. Submit.
4. Persona verifies the token (`getMe`) and shows a 6-digit pairing code with a `https://t.me/<bot_username>` link.
5. Open the bot in a private Telegram chat and send the 6-digit code. Once confirmed, Persona binds your Telegram user ID and chat ID and stops accepting pairing codes.

The pairing code expires after 10 minutes and locks after 5 wrong guesses. Signing out deletes the stored token and owner binding on the hub.

## Behavior and boundaries

- **Owner-only private chats**: Group and supergroup messages are ignored. Direct messages from any Telegram user ID other than the paired owner receive a single rate-limited notice (at most once per 10 minutes per sender) and never reach Persona.
- **Inbound**: Text, captions, quoted reply context, up to 4 images (`<= 5 MiB` each), and up to 4 files (`<= 10 MiB` each). Unsupported media types (voice, video, stickers, locations, contacts) are passed as placeholders.
- **Outbound**: Replies split cleanly at 4,000 runes and carry `reply_parameters` when replying to a specific message, followed by any generated/attached images (`sendPhoto`). Outbound sends are deduplicated by `ref`.
- **Network and proxy**: Manifest `permissions.network` allows only `api.telegram.org`. When no explicit proxy is entered, `http.ProxyFromEnvironment` applies (`HTTPS_PROXY`). If using a local loopback proxy on a non-official install, disable the hub sandbox (`personad extension sandbox telegram-bot off`).
- **Privacy**: The bot token is stored encrypted in the hub's connector secrets and scrubbed from all error messages. Message contents are never logged.

## Tests

```sh
go -C extensions/telegram-bot test -race -v ./...
```

Unit tests run against a local `httptest.NewServer` Bot API double covering token/proxy validation, token redaction, two-step pairing, wrong-code lockout, expiry, group ignoring, stranger rate-limiting, photo/document transfers, chunked replies, typing actions, logout, and SDK connector/channel conformance (`exttest.CheckConnectors`, `exttest.CheckChannels`).
