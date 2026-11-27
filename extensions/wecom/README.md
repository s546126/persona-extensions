# WeCom

[简体中文](README.zh-CN.md) · [Extension development](../../docs/development.md) · [Installation](../../docs/installation.md)

Chat with Persona from WeCom (企业微信). The extension drives a **smart robot (智能机器人) of your own WeCom organization** in API mode and contributes both a connector and a channel with id `wecom`. It runs separately from `personad` through the official Go SDK for process extensions. It uses the robot's long connection: the hub opens an outbound WebSocket to `wss://openws.work.weixin.qq.com`, so it needs no public address or callback URL.

| Extension point | Contribution | Behavior |
| --- | --- | --- |
| `connectors` | `wecom` | Sign-in (the robot's BotID and long-connection Secret, then a pairing code) and status. Nothing goes into the Inbox or the knowledge base. |
| `channel` | `wecom` | One-to-one chat with the robot: text, mixed text and pictures, pictures, files, voice (WeCom's transcript) and quotes come in; Markdown answers, notes, questions and pushes go out. |

## What a robot can and cannot see

A smart robot receives only the messages people send to it: one-to-one chats with it, and messages that @-mention it in a group. It cannot read your own chats, so this extension is an entry point for talking to Persona (a channel), not a data source that syncs your conversations. Group messages are ignored, as for every Persona channel.

## Setting up the robot

1. In the WeCom admin console, create a **smart robot** (智能机器人) and choose **API mode**.
2. Under the API settings, choose **long connection** (使用长连接) as the connection method. Copy the **BotID**, and get and copy the **Secret** (the long-connection Secret, not the URL-callback Token or EncodingAESKey).
3. In Persona, open **Settings → Channels → WeCom** (or **Settings → Connectors → WeCom**) and choose **Bind**. Enter the BotID and Secret. The extension checks them by subscribing once, then shows a six-digit pairing code.
4. Send that code to the robot in a one-to-one chat within 10 minutes. Whoever sends it becomes the owner.

Only the owner's messages reach Persona. Before pairing, the robot tells others how to pair. After pairing, anyone else gets one line saying the robot only works for the person who paired it (at most once every 10 minutes per person). Five wrong codes void the pairing. Entering credentials again always requires pairing again.

**One connection per robot.** WeCom keeps only one long connection per robot: a new subscriber closes the old one. If another program (or another hub) connects with the same robot, this extension shows that in its status and waits 30 minutes before taking the robot back, so that the two do not keep closing each other. Binding again takes it back at once.

## Messages

- **In**: text, mixed messages (text and pictures), up to 4 pictures of 3 MB each, files up to 50 MB (WeCom allows 20 MB), at most 40 MB of pictures and files in one message (the hub's own total, which also keeps the base64 `channel.receive` frame under the extension protocol's 64 MiB line), voice as WeCom's transcript, and the text of a quoted message. Media come as a URL valid for five minutes and a per-message AES key; the extension downloads them over HTTPS and decrypts them (AES-256-CBC). A file message carries no name: it is taken from the download's `Content-Disposition`. Videos, and anything too large or not downloadable, are named for the assistant instead. Conversation routing, `/new`, `/stop`, questions and turn handling belong to the hub's channel pipeline ([Persona's channel documentation](https://github.com/nwverse/persona/blob/main/docs/extensions/channels.md)).
- **Delivery**: a message counts as handled only once the hub took it. When the hub cannot take it (restarting, say), the owner hears so once, and a copy WeCom delivers again is tried again rather than dropped as a duplicate.
- **Out**: Markdown, sent to the owner with `aibot_send_msg`, in pieces of at most 4,000 characters (WeCom takes 20,480 bytes). WeCom allows it only after the owner has written to the robot, at most 30 messages a minute per chat, and keeps no idempotency key, so the extension sends each hub `ref` once and never sends a piece again by itself. A refusal (an errcode) or a request that never left lets the hub try the `ref` again. When the request went out but WeCom's acknowledgement did not come back, or part of a long message was already delivered, the outcome is reported as uncertain and the ref is never sent again, so the owner never gets a duplicate.
- **Questions and approvals** are answered in text (`asks: text`). Streaming replies, cards and buttons are not used yet.

## Privacy and network

The BotID, Secret, the owner (the id WeCom gives the robot for them, and the pairing time) and the ids of recently handled messages are kept as the connector's encrypted secret in the hub's database. They are never stored in the extension directory, logs or status. Signing out deletes all of it and closes the connection. Message contents and secrets are not logged.

The network grants are `*.qq.com` (the long connection), `*.myqcloud.com` (Tencent Cloud object storage, where WeCom keeps message pictures and files) and `*.qpic.cn` (another Tencent media host). Media are fetched over HTTPS only from those hosts, and a redirect is followed only when it stays on HTTPS and those hosts. Signed media addresses are kept out of logs.

## Development

```sh
go -C extensions/wecom test -race ./...
scripts/package.sh wecom
```

The tests run against a synthetic fake of the long connection (`internal/wecom/api/apitest`). They cover subscribing and refused credentials, heartbeats, ordered messages, sending and refusals, being taken over, message parsing (mixed, voice, quotes), media download and decryption (padding, wrong keys, size limits, unknown hosts, file names from the download), the handshake's header spelling, the credentials and pairing steps, owner-only forwarding, deduplication, groups, sign-out, the official SDK's connector and channel conformance, manifest/runtime consistency and the three locale catalogs.

Local tests are not production acceptance. Before relying on the extension, verify with a real smart robot that the long connection comes up, that pairing works, and that one test conversation makes a full round trip, including a picture and a file.
