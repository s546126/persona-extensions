---
name: spaces
description: 用户的 Persona 空间（Muse 风格的多空间本体工作台，经本机 spaces MCP 服务器）：每个空间有动态、点子、目标、资源库四个视图和自己注册的对象类型（如薅羊毛空间的好价线报/商家/优惠券，LifeOS 家庭空间的库存/餐食/家务，云电脑的云主机/运维手册）。用户说「记个点子」「我的目标进度」「家里还有什么快过期」「最近有什么好价」「在某个空间里加一种类型」，或者提到空间、本体、对象类型时用。读用 mcp__spaces__space_list / space_query / space_schema_get；写入的工具会先请用户确认。
---

# 空间（mcp__spaces__* 工具）

空间是相互隔离的工作区。每个空间都有四个内置类型，再加上用户自己注册的对象类型：

- `feed`（动态，查询时也可以写 `dynamic`）：每次新建、更新、状态流转、孵化、删除都会自动记一条，不用手动写。
- `idea`（点子）：状态 `inbox` 待探索 → `exploring` 孵化中 → `incubated` 已转化 → `archived` 已归档。
- `goal`（目标）：状态 `planned` → `active` → `achieved` → `archived`；进度是 `current_value / target_value`，单位在 `unit`。
- `resource`（资源库通用条目）：`category`、`url`、`summary`、`rating`。

## 先读，再写

1. `space_list`：有哪些空间、哪个是当前空间、每个空间的对象类型和数量。不确定用户说的是哪个空间时先看这个。
2. `space_schema_get {space_id}`：一个空间的字段约束（kind、必填、单位、enum 选项、relation 的目标类型）和状态。写入前一定先看，属性必须符合字段。
3. `space_query {space_id?, type?, keyword?, status?, filters?}`：查实体和它们的关系边（`relations.out` / `relations.in`）。`filters` 是属性的精确匹配，例如 `{"restock_needed": true}`。

只读工具直接运行。下面四个写入工具会按对话的权限策略先问用户：

- `space_entity_upsert`：不给 `id` 是新建（要 `type` 和 `title`），给 `id` 是更新（属性合并，值为 `null` 删除该属性）。`relations: [{type, target_id, direction}]` 加关系；点子带 `incubate_to_goal: true` 会直接孵化出一个目标，并记下 `idea → spawns → goal`。
- `space_schema_upsert`：注册或演进对象类型。key 用 snake_case；字段 kind 只能是 text、number、boolean、date、enum、url、relation。内置类型只能加字段。
- `space_create`：从模板（`lifeos`、`deals`、`cloud`、`blank`）建空间，或者 `action: switch | archive | unarchive`。
- `space_entity_delete {id, archive?}`：删除（连同关系边）或只归档。删除前先和用户确认是哪一条。

## 注意

- 值按字段的 kind 校验：text 字段的 `"007"` 就是字符串 `"007"`，不会被当成数字；number 字段可以给数字或数字字符串；date 用 `YYYY-MM-DD`。
- 不认识的属性名会被拒绝，错误里会列出可用字段，照着改就行，不要换个名字硬塞。
- 不填 `space_id` 就是当前空间；用户在界面里切换空间后，当前空间也跟着变。
- 转述结果时用用户能看懂的名字（空间名、类型的中文标签、状态的中文），不要只报 id。
