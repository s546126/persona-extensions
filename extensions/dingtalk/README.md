# DingTalk

[简体中文](README.zh-CN.md) · [Extension development](../../docs/development.md) · [Installation](../../docs/installation.md)

Chat with Persona from DingTalk. The extension drives the robot of **your own DingTalk internal app** and contributes both a connector and a channel with id `dingtalk`. It runs separately from `personad` through the official Go SDK for process extensions. It connects to DingTalk over Stream mode: the hub opens an outbound WebSocket, so it needs no public address or callback URL.

| Extension point | Contribution | Behavior |
| --- | --- | --- |
| `connectors` | `dingtalk` | Sign-in (the app's Client ID and Client Secret, then a pairing code) and status. Nothing goes into the Inbox or the knowledge base. |
| `channel` | `dingtalk` | One-to-one chat with the robot: text, rich text, pictures, files and voice (DingTalk's own transcript) come in; Markdown answers, notes, questions and pushes go out. |

## What a robot can and cannot see

A DingTalk robot receives only the messages people send to it: one-to-one chats with it, and messages that @-mention it in a group. DingTalk's public APIs do not let it read your own chats, so this extension is an entry point for talking to Persona (a channel). It is not a data source that syncs your conversations. Group messages are ignored, as for every Persona channel.

## Setting up the app

1. In the [DingTalk developer console](https://open-dev.dingtalk.com/), create an **internal app** (企业内部应用) for your organization.
2. Add the **Robot** capability. For receiving messages, choose **Stream mode**.
3. Publish the app, so that you can find the robot in DingTalk and open a one-to-one chat with it.
4. In Persona, open **Settings → Channels → DingTalk** (or **Settings → Connectors → DingTalk**) and choose **Bind**. Enter the app's **Client ID** (AppKey) and **Client Secret** (AppSecret). The extension checks them by fetching an access token, then shows a six-digit pairing code.
5. Send that code to the robot in a one-to-one chat within 10 minutes. Whoever sends it becomes the owner.

Only the owner's messages reach Persona. Before pairing, the robot tells others how to pair. After pairing, anyone else gets one line saying the robot only works for the person who paired it (at most once every 10 minutes per person). Five wrong codes void the pairing. Entering credentials again, even the same ones, always requires pairing again.

## Messages

- **In**: text and rich text; up to 4 pictures, 3 MB each; files up to 50 MB, and at most 40 MB of pictures and files in one message (the hub's own total, which also keeps the base64 `channel.receive` frame under the extension protocol's 64 MiB line); voice as DingTalk's transcript. Videos, and anything too large or not downloadable, are named for the assistant instead. Conversation routing, `/new`, `/stop`, questions and turn handling belong to the hub's channel pipeline ([Persona's channel documentation](https://github.com/nwverse/persona/blob/main/docs/extensions/channels.md)).
- **Delivery**: a message is answered on the Stream, and counted as handled, only once the hub took it. When the hub cannot take it (restarting, say), the owner hears so once and the frame stays unanswered, so DingTalk delivers it again; a message still queued when the extension stops is delivered again the same way.
- **Out**: Markdown, sent to the owner with `robot/oToMessages/batchSend`, in pieces of at most 4,000 characters. The first line of a piece becomes the title DingTalk shows in the chat list and notifications. DingTalk keeps no idempotency key, so the extension sends each hub `ref` once; a hub retry of the same `ref` gets the earlier result. A piece is sent again only when DingTalk certainly did not take it (rate limit, flow control, no connection). When the request went out but no answer came back, or part of a long message was already delivered, the outcome is reported as uncertain and the ref is never sent again, so the owner never gets a duplicate.
- **Questions and approvals** are answered in text (`asks: text`). DingTalk has no typing indicator for robots, and progress cards and buttons are not used yet.

## Privacy and network

The Client ID, Client Secret, the owner (DingTalk staff id, display name, pairing time) and the ids of recently handled messages are kept as the connector's encrypted secret in the hub's database. They are never stored in the extension directory, logs or status. Signing out deletes all of it and closes the connection. Message contents and tokens are not logged.

The network grants are `*.dingtalk.com` (the open platform `api.dingtalk.com`, the Stream endpoint it hands out, such as `wss-open-connection.dingtalk.com`, and session webhooks on `oapi.dingtalk.com`) and `*.aliyuncs.com`: DingTalk keeps the pictures, files and voice clips of messages in Alibaba Cloud OSS buckets and gives their download addresses there. It gives those addresses as `http://`; the extension fetches them over HTTPS instead, and refuses download addresses on any other host. The open platform and webhooks must be HTTPS, and replies go only to DingTalk session webhooks.

## Development

```sh
go -C extensions/dingtalk test -race ./...
scripts/package.sh dingtalk
```

The tests run against a synthetic fake of the open platform (`internal/dingtalk/api/apitest`). They cover the access token, Stream mode (pings, callbacks in order, unanswered frames, moved connections), sending and DingTalk's receiver lists, downloads and size limits, webhook restrictions, the credentials and pairing steps (wrong, expired and voided codes, pairing again), owner-only forwarding, deduplication across restarts, media, sign-out, the official SDK's connector and channel conformance, manifest/runtime consistency and the three locale catalogs.

Local tests are not production acceptance. Before relying on the extension, verify with a real internal app on an installed hub that the Stream connection comes up, that pairing works, and that one test conversation makes a full round trip, including a picture and a file. Never test against your primary conversation.
