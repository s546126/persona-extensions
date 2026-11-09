# Spaces

[Documentation](../README.md) · [Source](../../extensions/spaces/)

Spaces is a multi-space ontology workbench. It combines two ideas:

- **Muse (Glint) spaces**: a sidebar of isolated spaces (薅羊毛空间, LifeOS 家庭空间, 云电脑, + 创建空间), each opened on four tabs: 动态 (activity), 点子 (ideas), 目标 (goals) and 资源库 (library).
- **An EAV (entity-attribute-value) ontology**: each space holds object types (a key, a label, an icon, typed fields, a primary field and a status workflow), entities of those types with typed properties, and typed relations between entities. The library's stat grid, table columns, entity cards and forms are generated from the schema, so a type registered from the UI or by the assistant gets its views straight away.

All data is local, in the extension's data directory as `spaces_state.json`. It makes no network requests and needs no secrets. Every example record is synthetic; host addresses are the placeholder `<host-address>` and links point at `example.com`.

## Contributions

| Contribution | Behavior |
| --- | --- |
| `ui` `spaces` (`nav_page`) | The space switcher (the view's filter, with + 创建空间 and 管理空间…), the four tabs, entity detail and edit pages, the spaces page and the object type form. |
| `ui` `spaces-card` (`overview_card`) | The active space's open ideas, active goals and the progress of its top goals. |
| `ui` `spaces-widget` (`widget`) | Goal progress for small, medium, large, accessory_rectangular and accessory_inline. |
| `shortcuts` `spaces-capture` | Records an idea in the active space (status 待探索) and logs it in the activity stream. |
| `shortcuts` `spaces-progress` | Matches a goal of the active space by name (exact, then contains) and sets its current value; reaching the target marks it achieved. |
| `skills` `spaces` | Tells the assistant how to read before writing and how values are validated. |
| `mcp_servers` `spaces` | `spaces mcp` on the assistant's computer. Only the three read-only tools are in `allow`; the write tools follow the conversation's permission policy. |

## The ontology

| Concept | Fields |
| --- | --- |
| `Space` | `id`, `name`, `icon`, `description`, `template`, `archived`, its `types`, `entities` and `relations` |
| `ObjectType` | `key`, `label`, `icon` (a view NavIcon), `description`, `primary_field`, `statuses`, `fields` |
| `FieldDef` | `key`, `label`, `kind` (`text`, `number`, `boolean`, `date`, `enum`, `url`, `relation`), `required`, `unit`, `options` (enum), `target_type` and `relation_type` (relation) |
| `Entity` | `id`, `space_id`, `type`, `title`, `status`, `tags`, `properties`, `created_at`, `updated_at`, `archived` |
| `Relation` | `id`, `space_id`, `from_id`, `relation_type` (`spawns`, `belongs_to`, `located_in`, `supports`, …), `to_id`, `created_at` |

Four types are built into every space and cannot lose their fields or statuses (they can gain fields):

| Type | Statuses | Fields |
| --- | --- | --- |
| `feed` 动态 (alias `dynamic`, `activity`) | none | `event_type`, `summary`, `actor`, `target_id`, `target_type` |
| `idea` 点子 | `inbox` 待探索 → `exploring` 孵化中 → `incubated` 已转化 → `archived` 已归档 | `category`, `priority`, `summary`, `incubated_target_id` |
| `goal` 目标 | `planned` 规划中 → `active` 进行中 → `achieved` 已达成 → `archived` 已归档 | `target_value`, `current_value`, `unit`, `deadline`, `priority`, `milestone_summary` |
| `resource` 资源 | `saved`, `using`, `archived` | `category`, `url`, `summary`, `rating` |

Every create, update, status change, incubation, relation, archive, delete and schema change writes a `feed` entry in its space, with the actor (你, 助手, 快捷指令 or 模板). The stream keeps the newest 500 entries of a space.

### Schema and value rules

- **Schemas are scoped to their space.** An object type and its fields live inside a space; the same key can mean different things in two spaces, and relations never cross spaces.
- **Values are converted by the declared kind, never by their look.** A `text` field keeps `"007"`; a `number` accepts a number or a numeric string; `boolean`, `date` (`YYYY-MM-DD`, RFC 3339 is shortened), `enum` (one of the options), `url` (http or https) and `relation` (an entity of this space, of `target_type` when set) are checked. An unknown property is refused with the list of fields, and a missing required field is refused.

A `relation` field also keeps its edge in the graph (named by `relation_type`, else the field key), and the edge follows the field when it changes or is cleared. Deleting an entity removes its edges and clears relation fields that pointed at it.

## Templates

| Template | Space | Object types |
| --- | --- | --- |
| `deals` | 薅羊毛空间 | `deal` 好价线报 (platform, merchant_name, merchant → `belongs_to`, original_price, deal_price, saved_amount, valid_until, url), `merchant` 商家/渠道, `coupon` 优惠券 |
| `lifeos` | LifeOS 家庭空间 | `inventory_item` 家庭库存 (category, location → `located_in`, quantity, unit, expiry_date, restock_needed), `location` 收纳位置, `meal_plan` 餐食计划, `chore` 家务事项 |
| `cloud` | 云电脑 | `cloud_host` 云主机节点 (provider, region, spec, os, address, monthly_cost, expires_at), `runbook` 运维手册 (applies_to → `supports`) |
| `blank` | 自定义本体空间 | only the four built-in types |

On first start the extension seeds 薅羊毛空间 (active), LifeOS 家庭空间 and 云电脑 with synthetic examples, ideas, goals and relations, so the views and the preview are populated without setup.

## The tabs

- **动态**: a stat grid (总动态, 今日新增, 点子数, 目标与资源数), a field to post a note, and the stream newest first with an icon and badge per event and the actor; a row opens the entity it is about.
- **点子**: a capture form (title, summary, priority, tags) and the ideas grouped by 待探索, 孵化中 and 已转化, with 开始探索, 孵化为目标, 转化为资源 and 归档. 孵化为目标 creates a linked goal (target 100 %, the idea's priority and summary), adds `idea → spawns → goal`, marks the idea `incubated` and logs the incubation; 转化为资源 does the same with a resource.
- **目标**: a stat grid (进行中目标, 已达成, 平均达成率, 关联关系数), a progress bar per goal (`current / target unit · %`), 推进进度 +10% (a planned goal becomes active; reaching the target marks it achieved) and 标记达成, and a form for a new goal.
- **资源库**: the schema browser. A row per object type (全部 or one type), a stat grid (实体总数, counts per status, 关系边, 字段数), a table whose columns come from the type's fields, entity cards with key properties, tags and edge counts, a form generated from the fields to add an entity, and buttons to register or change an object type. Fields for a type are written `key:标签:kind[:extra]`, separated by `;` or new lines, with `!` for required: `fee:月费:number:元!; plan:套餐:enum:月付|年付; shop:店铺:relation:merchant:belongs_to`.

An entity's page shows its properties, its outgoing and incoming edges (each opens the other end), status buttons for its workflow, edit, archive and delete, and a form to add a relation.

Older apps get the same pages: before UI version 4 the tabs become a segmented control (buttons before version 2); before version 3 the space filter and the search become a field and a button, load more becomes a button, and markdown, tables and empty states become text and rows. The SDK draws pushed pages in place for them.

## Persistence and the MCP server

`spaces_state.json` is written atomically (a temporary file, then rename) with a generation counter. Before each render or action the store compares the file's modification time and size with what it last read or wrote and reads it again when they differ, so a write by `spaces mcp` shows in the app on its next render. A failed change leaves the state as it was; an unreadable file is moved to `spaces_state.json.corrupt` and the examples are seeded again.

`spaces mcp` speaks MCP over stdio (JSON-RPC 2.0, one message a line: `initialize`, `notifications/initialized`, `ping`, `tools/list`, `tools/call`). `spaces --help` prints `mcp`, which the manifest's `requires` checks. It finds its data in `--data`, else `PERSONA_SPACES_DATA`, else the data directory the hub's extension last wrote to `<user config dir>/persona-spaces/data-dir` on the same computer (best effort: a sandboxed extension may not be allowed to write it), else `<user config dir>/persona-spaces/data`. Set `PERSONA_SPACES_BIN` to the program's path when `spaces` is not on `PATH` or in `~/bin`.

| Tool | Access | What it does |
| --- | --- | --- |
| `space_list` | read, allowed | All spaces, the active one, each space's types and entity, feed and relation counts. |
| `space_query` | read, allowed | Entities of a space with their edges, by `type` (`feed`/`dynamic`, `idea`, `goal`, `resource` or a custom type), `keyword`, `status`, `filters` (exact property matches), `include_archived`, `limit` (30, at most 200). |
| `space_schema_get` | read, allowed | The object types with their field constraints and statuses, relation types, status labels and the view definitions. |
| `space_create` | write, asks | Creates a space from a template or with a name, icon and description; `action` `switch`, `archive` or `unarchive`. |
| `space_schema_upsert` | write, asks | Defines or evolves an object type. |
| `space_entity_upsert` | write, asks | Creates or updates an entity, with `relations` and, for ideas, `incubate_to_goal`. |
| `space_entity_delete` | write, asks | Deletes an entity and its edges, or archives it with `archive: true`. |

## Browser preview

```sh
go -C extensions/spaces run . preview --addr 127.0.0.1:8741 --data /tmp/spaces-preview
```

The preview uses the same store and ontology as the extension and draws a space the way Muse does: the space list with + 创建空间 on the left, the tabs 主要聊天 / 动态 / 点子 / 目标 / 资源库 on top, KPI cards, schema badges for the selected type, relation edges on each card, and forms to switch spaces, post, add and incubate ideas, move goals, register object types and add entities. It follows the system's light or dark appearance. `/api/state` and `/api/schema?space=<id>` return the state as JSON. Without `--data` it starts on a fresh temporary directory with the example spaces.

## Tests

```sh
go -C extensions/spaces test -race ./...
```

The tests cover the protocol conformance check and the manifest, templates and space isolation of types, entities and relations, switching and archiving, typed validation of every field kind, relation fields and the graph, the activity stream, reloading across processes, every tab, the detail, edit, spaces and type pages, the card and the widget across UI versions 1 to 5, idea → goal incubation, the shortcuts, all seven MCP tools over stdio and the preview server.
