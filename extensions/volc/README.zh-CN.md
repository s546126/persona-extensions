# 火山语音

[English](README.md) · [扩展开发](../../docs/development.md) · [安装](../../docs/installation.md)

用**你自己的火山引擎（豆包语音）账号**在 Hub 上做会议转录，并把会议里的回答念出来。这是 Hub 原内置扩展 `volc` 的官方扩展包：id、贡献、密钥和设置都相同，只是改为用官方 Go SDK 构建的程序运行，不再是 `personad volc`。需要构建号 `202611272113` 或更新的 Hub（`engines.persona`），这样的 Hub 会把已有的内置 `volc` 原地交接给这个包。

| 扩展点 | 贡献 | 行为 |
| --- | --- | --- |
| `transcription` | `volc` | 通过 Hub 的转录 WebSocket（`GET /transcription/volc/ws`）做实时和重放转录，带中间结果；`GET /transcription/volc/info` 说明是否就绪。 |
| `speech` | `volc` | 朗读会议回答：`POST /speech/volc/speak` 在火山合成的同时流式返回 24 kHz PCM；`GET /speech/volc/info` 说明是否就绪。出任何错时手机改用自带的声音。 |
| `ui`（`extension_page`） | `status` | 「连接与测试」：每个服务最近一次的结果、测试连接按钮，以及去哪里拿 Key。 |

这两个扩展点只接受完整设备或管理进程本身的调用；程序也应答 `settings.check`。

## 设置

1. 在豆包语音控制台开通识别模型（默认是豆包流式语音识别模型 2.0）；要朗读回答，再开通豆包语音合成模型 2.0；然后复制一把 API Key。
2. 安装扩展，把 Key 粘贴到 **API Key**。这是必填密钥：保存之前 Hub 把扩展显示为需要设置，不会启动它。
3. 在录音设置里把转录服务选成这个扩展。扩展包自己没有「用于会议转录」开关。

设置项（表单只有中文）：

| 设置 | 默认 | 含义 |
| --- | --- | --- |
| `read_answers` | 开 | 会议设成「参与者」时，用下面的音色念回答。 |
| `voice` | `zh_female_vv_uranus_bigtts` | 六个语音合成 2.0 音色之一。 |
| `advanced.asr_model` | `volc.seedasr.sauc.duration` | 识别模型：2.0 或 1.0，按时长或按并发计费。 |
| `advanced.voice_id` | 空 | 控制台音色库里的任意音色 ID，代替 `voice`。 |
| `advanced.app_id` | 空 | 只用于旧版控制台：填它的 App ID，API Key 栏填 Access Token。 |

这些键和选项都是内置扩展的子集，所以把扩展包回滚到内置时，已保存的设置仍然有效（`TestConfigWithinBuiltinSchema`）。

## 连接测试与被拒绝的 Key

连接测试会识别 0.6 秒静音；开着朗读时，再合成两个字。两者都是真实的计费调用（几个字的量）。它在这些时候运行：

- 扩展运行中保存设置时（`settings.check`），服务拒绝的 Key 或模型会被拦下；
- 在状态页点「测试连接」，每 10 秒最多一次；
- 当前设置还没有保存过任何结果时，**启动时测一次**。Hub 不会校验它拿到的第一把 Key，因为还需要设置的扩展没有在运行。结果按设置指纹（一个哈希，从不含 Key）保存在扩展数据目录的 `status.json` 里，所以同样的设置下次启动不会再测。

火山拒绝这把 Key 时（启动时、测试时或真实使用中），各点只反映自己服务的拒绝，返回 `setup: "key_rejected"`：识别拒绝时是 `GET /transcription/volc/info`（会议不会把它当作就绪），合成拒绝时是 `GET /speech/volc/info`（手机改用自带的声音）。一个服务的拒绝不会影响另一个点。状态页会说明原因。程序照常运行，所以改存另一把 Key 时会走 `settings.check` 并立即生效。模型没开通、额度用完或网络不通会显示在状态页上，但不算 `key_rejected`。

## 与内置扩展的差异

- 设置表单只有中文；状态页和错误信息跟随客户端语言（中文、英文或日文）。
- 第一把 Key 在扩展启动后才测，而不是在保存时；扩展列表里仍显示为运行中。
- 表单里没有会议转录开关，要在录音设置里选。
- 已启用但没有 Key 的内置行在交接后变为「需要设置」，保存 Key 之前不会运行。
- 程序运行在 Hub 的干净环境里，只能访问 `openspeech.bytedance.com`。

## 网络与数据

程序只连接 `openspeech.bytedance.com`（识别用 WebSocket，合成用 HTTPS）。会议音频和回答文字直接传给火山引擎，不做保存；它写的唯一文件是 `status.json`。Key 来自 Hub 的加密密钥，从不写进日志或文件。

## 开发

```sh
go -C extensions/volc test -race ./...
scripts/package.sh volc
```

测试跑在 `internal/volctest` 上，它是火山语音服务的本地替身，不需要账号。`PERSONA_VOLC_SERVICE=http://host:port` 让程序改连这样的替身；Hub 从不设置它。
