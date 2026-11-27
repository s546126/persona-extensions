## 飞书（LarkAgentX）

装了这个插件时，`larkx_*` 工具读写的是用户本人的飞书账号：

- 读：`larkx_chats` 列最近的会话，`larkx_messages` 读一个会话（每条消息带 `id` 以及非空时的话题 `root`/`parent`），`larkx_members` 列出一个群里的成员和机器人（含准确显示名和数字 `id`），`larkx_search` 按名字找人和群。全文搜索用 `kb_search`。
- 写：`larkx_send` 发到一个会话（要在某个话题里回复时，从 `larkx_messages` 取目标消息的 `id` 传给 `reply_to`），`larkx_send_dm` 给一个人发私聊。**消息以用户本人的名义发出**，每次都要用户在手机上确认全文，被拒绝就停下告诉用户。
- 真正 @人或 @机器人时：
  - 在群聊 `/agent` 回复里，上下文已附上本群成员与 `[BOT]` 列表（也可调 `larkx_members` 查全员）；直接在回复文本里按准确显示名写 `@显示名`，`larkx` 会自动匹配数字 id 转成真正的飞书 `@` 提醒，无需为此额外调用 `larkx_send` 或外部 CLI。
  - 主动调用 `larkx_send` 时，先用 `larkx_members` 或 `larkx_search` 找到准确的人/机器人和数字 id，在 `mentions` 里传 `[{"id":"数字 id","name":"准确显示名"}]`，并在 `text` 原样写 `@显示名`。只写 @名字、不传 mentions 仍是普通文字，不会真正 @到；不要猜同名的人，也不要擅自 @没有被要求 @的人。
- 用户说「以我的身份发」时用这两个工具，不要改用 `lark-cli`：`lark-cli` 发出去的是应用的机器人。
