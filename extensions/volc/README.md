# Volcengine speech (火山语音)

[简体中文](README.zh-CN.md) · [Extension development](../../docs/development.md) · [Installation](../../docs/installation.md)

Meeting transcription on the hub and spoken meeting answers with **your own Volcengine (Doubao Speech) account**. This is the official package of the hub's former built-in `volc` extension: the same id, contributions, secret and settings, as a program built with the official Go SDK instead of `personad volc`. It needs a hub at build `202611272113` or later (`engines.persona`), which hands an existing built-in `volc` over to this package in place.

| Extension point | Contribution | Behavior |
| --- | --- | --- |
| `transcription` | `volc` | Live and replay transcription jobs over the hub's transcription WebSocket (`GET /transcription/volc/ws`), with interim text; `GET /transcription/volc/info` says whether it is ready. |
| `speech` | `volc` | Reads a meeting answer aloud: `POST /speech/volc/speak` streams 24 kHz PCM as Volcengine synthesizes it; `GET /speech/volc/info` says whether it is ready. The phone uses its own voice on any error. |
| `ui` (`extension_page`) | `status` | "连接与测试": each service's last outcome, Test Connection, and where to get a key. |

Only a full device or the manager itself may call the two points; the program also answers `settings.check`.

## Setup

1. In the Volcengine Doubao Speech console, turn on the recognition model (Doubao streaming ASR 2.0 by default) and, to read answers aloud, the synthesis model 2.0; then copy an API Key.
2. Install the extension and paste the key into **API Key**. The key is a required secret: until it is saved the hub shows the extension as needing setup and does not start it.
3. Pick this extension as the transcription service in the recording settings. The package has no "use for meeting transcription" switch of its own.

Settings (the form is in Chinese only):

| Setting | Default | Meaning |
| --- | --- | --- |
| `read_answers` | on | Read answers aloud in the voice below when a meeting is set to "participant". |
| `voice` | `zh_female_vv_uranus_bigtts` | One of six synthesis 2.0 voices. |
| `advanced.asr_model` | `volc.seedasr.sauc.duration` | Recognition model: 2.0 or 1.0, billed by duration or by concurrency. |
| `advanced.voice_id` | empty | Any voice ID from the console's voice library, in place of `voice`. |
| `advanced.app_id` | empty | Only for the old console: its App ID, with its Access Token in the API Key field. |

These keys and their choices are a subset of the built-in's, so rolling the package back to the built-in keeps the saved settings (`TestConfigWithinBuiltinSchema`).

## Connection test and a rejected key

The connection test recognizes 0.6 seconds of silence and, while answers are read aloud, synthesizes two characters. Both are real, billed calls (a few characters). It runs:

- when settings are saved while the extension runs (`settings.check`), refusing a key or model the service rejects;
- from Test Connection on the status page, at most once every 10 seconds;
- **once at start** when no outcome is kept for the current settings. The hub does not check the first key it is given, because an extension that still needs setup is not running. Outcomes are kept per settings fingerprint (a hash, never the key) in the extension's data directory as `status.json`, so the same settings are not tested again on the next start.

When Volcengine refuses the key (at start, in a test or in real use), each point reflects only its own service's refusal with `setup: "key_rejected"`: `GET /transcription/volc/info` when recognition refuses it (meetings then do not pick it as ready), `GET /speech/volc/info` when synthesis refuses it (the phone then uses its own voice). A refusal by one service does not hold back the other point. The status page says why. The program keeps running, so saving another key goes through `settings.check` and takes effect at once. A model that is not turned on, a used-up quota or a network failure is shown on the page but is not `key_rejected`.

## Differences from the built-in

- The settings form is Chinese only; the status page and errors follow the client's language (Chinese, English or Japanese).
- The first key is tested after the extension starts, not when it is saved, and the extension list still shows it as running.
- No meeting-transcription switch in the form; choose it in the recording settings.
- A built-in row that was enabled without a key becomes "needs setup" after the handoff and does not run until a key is saved.
- The program runs in the hub's clean environment and reaches only `openspeech.bytedance.com`.

## Network and data

The program connects only to `openspeech.bytedance.com` (recognition over WebSocket, synthesis over HTTPS). Meeting audio and answer text pass through to Volcengine and are not stored; the only file it writes is `status.json`. The key comes from the hub's encrypted secrets and is never logged or written.

## Development

```sh
go -C extensions/volc test -race ./...
scripts/package.sh volc
```

The tests run against `internal/volctest`, a local stand-in of the Volcengine services, and need no account. `PERSONA_VOLC_SERVICE=http://host:port` points the program at such a stand-in; the hub never sets it.
