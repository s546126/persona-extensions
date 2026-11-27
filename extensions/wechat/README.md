# WeChat

> **Category: Public extension** — General-purpose extension using only public Persona extension APIs; credentials and user data stay strictly in installed settings and local extension data.

[Extension development](../../docs/development.md) · [Installation](../../docs/installation.md)

A personal WeChat bot using Tencent's public iLink protocol. It contributes both connector and channel id `wechat`. It runs independently of `personad` through the official Go process-extension SDK; no hub runtime or TypeScript source-profile change is required for its long-polling channel.

## Install and bind

Install **WeChat** from Persona's public extension marketplace, then open **Settings → Connectors → WeChat** and scan the QR code in WeChat. Confirm on the phone; if WeChat asks for a number, enter that number in Persona. QR expiry, scanned confirmation, redirects and verification-code steps retain the former connector's behavior.

Only the person who bound the bot can send messages into Persona. Other senders receive a rate-limited explanation and never enter the hub conversation. Text, pictures (up to 16 MiB each, so photos sent as the original come through; the hub scales them down for the model), documents, videos and voice messages come in; videos are delivered as files. Voice messages use WeChat's transcript when present; when WeChat omits the transcript, the extension downloads and decrypts the voice audio (up to 8 MiB per voice message), wraps raw PCM in a WAV header when `encode_type` is PCM, preserves detected or declared audio formats such as SILK, WAV, AMR, MP3, OGG and M4A without external dependencies, and delivers the audio as a file attachment, falling back to an unsupported voice notice when the media reference is missing, invalid, oversize or fails to download or decrypt. Pictures and files share a 40 MiB per-message budget; excess attachments are described as unsupported. Plain-text replies are chunked at 3,500 characters, and the pictures of an answer (sent by a hub with `images_out`) arrive as WeChat pictures after its text: each is encrypted with a fresh key, uploaded to WeChat's CDN through iLink's `getuploadurl`, and sent as an `image_item`; the channel supports questions, push messages and typing indicators. A picture that cannot be sent (not decodable, over 8 MiB, or refused by WeChat) is skipped, the pictures after it still go, and a last text tells the owner how many were not sent. A failure that may pass stops the send instead, and so does a send still going 100 seconds after the hub's call (waiting for an earlier send of the same ref included), before the hub's 2-minute call timeout: the hub's retry of the same ref continues from that piece or picture and does not upload a picture the CDN already took. If the ref comes again with other text or pictures, or with earlier content after other content, the whole message is sent again from the start under new client ids.

When a quoted message contains only `svr_id`, the extension restores its text or media summary from `quote-index.json` in the extension data directory; `partial_text` selections are resolved against the cached text. The index holds up to 2,048 entries and 4 MiB of indexed data: sent and received text is retained for 30 days, while media kind, filename and size are retained for 7 days. Startup and message activity remove expired and oldest entries; the file is replaced atomically with mode `0600`. An absent, unreadable or corrupt index leaves the main message and the existing quote fallback unchanged.

Conversation routing, `/new` and turn execution belong to the hub, not this extension. See [Persona's channel documentation](https://github.com/nwverse/persona/blob/main/docs/extensions/channels.md).

## Upgrading from the built-in

Do not sign out, delete connector data, or copy credentials to another polling process. The connector id remains `wechat`, and the extension loads the same hub-encrypted `connector.secrets` blob:

- `creds.token`, `creds.bot_id`, `creds.user_id`, `creds.base_url`;
- `old`, `cursor`, `context` (the last owner's iLink context token), `seen`.

The existing token is reused without a QR login. Polling resumes from the retained cursor; replies use the retained owner/context token. State never becomes a plaintext credential file in the extension directory.

A hub with the built-in must support registry handoff before installing this same-id extension. The old process must fully stop before the new process starts: two pollers with one token can steal messages. Keep the built-in available until a verified migration and the operator's final removal approval. During this window, uninstalling the extension restores the built-in through the registry's handoff mechanism; do not use logout as rollback.

## Privacy and network

The network grants are `*.weixin.qq.com` and `*.wechat.com`, matching Tencent's iLink and media-CDN allowlist for mainland Weixin and international WeChat accounts. API and redirect origins are restricted to HTTPS Tencent hosts. Tokens and message contents are not logged. Polling, outbound retries, message deduplication and stable send refs retain the built-in implementation.

The quote index contains message text and media metadata in plaintext on local disk, but no tokens, CDN URLs, decryption keys or downloaded media. Entries are scoped to the bound account; logging out or changing accounts clears the index.

The implementation was moved from Persona's MIT-licensed Go plugin. Its iLink wire behavior follows Tencent's MIT-licensed `openclaw-weixin` v2.4.6; the Go client does not embed Tencent's plugin runtime. [License notices](assistant/THIRD_PARTY_NOTICES.md) ship in the package.

## Development

```sh
go -C extensions/wechat test -race ./...
scripts/package.sh wechat
```

The tests cover QR binding and verification steps, owner-only forwarding, media, transcribed and untranscribed voice fallback (audio download/decryption, PCM-to-WAV wrapping, format/size boundaries and failure degradation), typing, revoked tokens, reply retries and duplicate refs, quote restoration, retention, eviction, corruption and concurrent access, official SDK connector/channel conformance, manifest/runtime consistency, all three locale catalogs, and loading a literal synthetic legacy credential blob without requesting a QR.

Local tests are not production acceptance: inspect the installed hub's QR/status UI and verify single-poller ownership, credential reuse and one test-conversation round trip separately. Never test against the user's primary conversation.
