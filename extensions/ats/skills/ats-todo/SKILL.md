---
name: ats-todo
description: 查待办背后 ATS 任务的进度、卡点和验收证据，或替用户派 ATS 任务。用于「做到哪了」「真做完了吗」「为什么卡住」「ATS 在跑什么」「交给 ATS 做」。
---

# ATS 待办任务

ATS 在所选电脑的独立 git 工作目录运行 Agent 并验收，结论写回待办。一个待办可有多个任务。

命令 `ats METHOD PATH [JSON | @文件]`：GET 直接执行，`POST /tasks` 每次先问用户。

- `ats GET '/tasks?todo_id=<待办 id>'`：列任务，可加 `state=`（逗号分隔，取值见第 1 条）、`priority=p0..p3`、`limit=`（默认 20）；有 `next` 就加 `after=<next>` 翻页。列表省略默认值（p2、只读、`claude`、首次尝试），仓库、验收命令见单个任务
- `ats GET /tasks/<id>`：单个任务（长 `prompt` 只给开头，全文加 `?view=full`）；`ats GET /tasks/<id>/evidence`：证据
- `ats POST /tasks '<JSON>'`：派任务。也可传 `@/绝对路径` 原样发送文件

1. 进度按 `state`：`queued` 排队（有 `not_before` 是失败后等重试）；`running`，`phase` 为 `preparing` 准备、`working` 在做、`accepting` 跑验收；`needs_input` 要用户去该 Agent 会话回答或批准（等什么见 `waiting`）；`succeeded`、`failed`、`canceled`。列表的 `scheduler`：队列档位和剩余额度。
2. 做完没只看 `/evidence`：`verified` 为 true（`succeeded` 且 `verdict` 为 `pass`）才算通过验收；`succeeded` 但 `verdict` 为 `unverified` 只能说「跑完了、没验收」。`last_report` 含验收退出码、Agent 总结 `summary`、分支和提交、输出末尾 `message`；第 N 次的完整报告加 `?attempt=N`。
3. 失败或卡住：说第几次尝试、`failure_reason`、报告原话、一句下一步建议。自动重试：`task_failed`（没过验收或 Agent 出错）、`timeout`、`infra_error`（电脑或环境）、`rate_limited`（额度或限流）、`lost`（会话丢失）；不重试、要用户先改设置或任务：`config_error`（模型或配置不可用）、`auth_expired`（Agent 登录失效）、`rejected`（仓库或 ref 不对）。重试会接着上一次尝试的提交做，并带上上次的失败原因和验收输出。别自己重派；「再试一次」和「取消」由用户在「ATS 自动做待办」页点开任务后点。
4. 报错直说，不编进度：`too_many_queued`、`too_many_for_todo`（每个待办最多 5 个没结束的任务）、`repos_not_configured`/`repo_not_allowed`（请用户在「设置 › 扩展 › ATS 自动做待办」的「允许它动哪些文件夹」加上）、`not_found`。
5. 仅按用户明确要求派，一次一个。确认卡上只有这条命令的原文，执行前先告诉用户标题、仓库、只读或可写、验收命令。
   - 必填 `todo_id`（结论写回的待办）、`prompt`（完整说明：Agent 看不到对话，写清目标、范围、怎样算完成）、`repo`（那台电脑上的绝对或 `~/` 路径，须在允许目录内）
   - `acceptance`：验收 shell 命令，在工作目录跑，退出码 0 为过；尽量给（没有就只能说「跑完了」）
   - `permission_profile`：默认 `readonly` 只调查、不改；改代码才传 `default`，并向用户说明可写。改动提交到那台电脑的 `ats/<任务>-<次数>` 分支（Agent 没提交的由 ATS 提交），不 push
   - 只读会话默认 `plan`，由用户在扩展设置选择 `readonly_permission_mode`
   - 可选 `ref`（默认 `HEAD`，分支/标签/提交）、`agent_type`（默认 `claude`，用户指定才传，如 `codex` 或 `agy`）、`model`（覆盖模型）、`launcher`（名称或 `default,studio=local`）、`priority`（`p0` 紧急、`p1` 高、`p2` 默认、`p3` 后台）、`difficulty`（`easy`/`medium` 默认/`hard`/`extreme`，影响调度和 Claude 模型档）、`machine`（电脑名）、`title`（页上标题）、`idempotency_key`（同 key 只建一个任务）

例：`ats POST /tasks '{"todo_id":"todo_123","title":"修复登录页空指针","prompt":"……","repo":"~/code/app","acceptance":"go test ./...","permission_profile":"default"}'`
