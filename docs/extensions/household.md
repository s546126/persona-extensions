# Household

The household extension (`household`, 家庭管家) keeps a home's stock, shopping list, chores and meal plan, and tells you when something runs low, expires or falls due. It is built on the ontology kernel of [spaces](spaces.md): every record is a kernel entity in one space with six types, and the household rules are a thin layer above it.

| Extension point | What it adds |
| --- | --- |
| `contributes.ui` `nav_page` `household` | The 家庭管家 page: 总览, 库存, 采购, 家务 and 餐食 tabs, with a badge counting what needs attention |
| `contributes.ui` `overview_card` `household-card` | The 家庭管家 · 概况 card on the overview, with the counts and the most urgent entries |
| `contributes.mcp_servers` `household` | Fifteen tools for the assistant (`household mcp`), six of them read-only and allowed |
| `contributes.skills` `household` | Tells the assistant when and how to use the tools |
| `permissions.hub` `notify` | The daily reminder as a push notification |
| `permissions.hub` `todos.write` | Reminders as to-dos (optional, chosen in the settings) |

## What it keeps

One space, `household`, with these types. Names, stages and options are stored values and stay in Chinese.

| Type | Key | Stages | Properties |
| --- | --- | --- | --- |
| 位置 | `place` | none | `note` text; `items` rollup: count of 物品 stored here |
| 物品 | `item` | none | `category` enum (食材, 调味, 日用, 清洁, 药品, 母婴, 其他); `place` relation to 位置 (backlink 存放物品); `qty` number; `unit` text; `min` number, the low-stock threshold; `expires` date; `daily` number, the average use per day; `counted` date, when `qty` was last true; `lead` number, days of warning; `refill` number, how much to buy; `note` text |
| 采购 | `buy` | 待买 (todo) → 已买 (done); 不买 (dropped) | `item` relation to 物品 (backlink 采购记录); `amount` number; `unit` text; `category` enum, as on 物品; `reason` enum (低库存, 已用完, 已过期, 用量预警, 手动); `note` text |
| 家务 | `chore` | none | `every` number, days between two rounds; `last` date; `next` date; `who` text; `place` relation to 位置 (backlink 家务); `minutes` number; `note` text |
| 菜谱 | `recipe` | none | `needs` relation to many 物品 (backlink 可做的菜); `minutes` number; `url` url; `note` text |
| 餐食 | `meal` | 计划 (todo) → 已做 (done); 取消 (dropped) | `date` date; `slot` enum (早餐, 午餐, 晚餐, 加餐); `recipe` relation to 菜谱 (backlink 排期); `note` text |

The purchase amount is called `amount`, not `qty`, on purpose: deriving a purchase from an item copies the properties both types declare (`unit`, `category`), and the stock quantity must not become the amount to buy.

## Rules

All rules are computed from the records when they are read; nothing derived is stored. "Today" is the date in the hub's time zone.

**Estimated quantity.** An item with `daily` set is assumed to be used up at that rate since `counted` (or, without it, since the record last changed): `est = max(0, qty − daily × days)`. Without `daily`, `est = qty`. An item without `qty` is not tracked by quantity and only its expiry date counts.

**Findings.** Each item gets at most one finding, the most severe that applies:

| Finding | When | Purchase reason |
| --- | --- | --- |
| `expired` 已过期 | `expires` is before today | 已过期 |
| `out` 已用完 | `qty` is set and `est` is 0 | 已用完 |
| `low` 低库存 | `min` is set and `est ≤ min` | 低库存 |
| `runout` 用量预警 | `daily > 0` and `est / daily ≤ lead` | 用量预警 |
| `expiring` 即将过期 | `expires` is within `lead` days from today | none: use it first |

`lead` defaults to 7 days for 母婴 items and 3 days for everything else. `runout` is what warns about baby supplies before they are gone: give a pack of diapers its count, the daily use and the day you counted, and it is flagged a week before the estimate reaches zero.

**Shopping list.** 一键补货 creates one 待买 purchase for every item with an `expired`, `out`, `low` or `runout` finding that has no 待买 purchase yet, in one transaction. Each is derived from its item (the kernel's derive: same title, `unit` and `category` copied, origin recorded; the item's note is not carried over) with `item` pointing back, `amount` = the item's `refill` (1 when unset) and the reason from the table. Running it again creates nothing new.

**Overview numbers.** The 总览 tab opens with one line on what the extension is for, then four numbers, then the imports waiting (see "Import") and 今天的建议. 需要处理 is what the rules find: the items with an `expired`, `out`, `low` or `runout` finding plus the chores due, and its detail shows the two parts (补货 n · 家务 n); items that are only `expiring` are not in it and have their own number, 即将过期. 待买 is the open purchases on the shopping list, however they got there (by hand, 一键补货 or the assistant). The two overlap only when an item that needs buying already has a 待买 purchase; a line under the numbers tells them apart.

**Buying.** Marking a purchase 已买 through the page or the `household_purchase` tool stocks the item: `amount` is added to its estimated quantity and `counted` becomes today. When the item's `expires` date is already past, the old batch is taken as thrown away: the quantity becomes `amount` alone and the date is cleared (enter the new one when you put the batch away). An item without `qty` stays untracked; only a past date is cleared. 不买 only closes the purchase.

**Chores.** A chore is due when `next` is today or earlier. 完成 sets `last` to today and `next` to today + `every` (rounded up to whole days, at least one); a chore without `every` is a one-off and has its `next` cleared. A new chore without `next` is due today.

**Meals.** Suggestions rank recipes by the ingredients at hand. An ingredient counts as at hand when the item is not expired and is either untracked or has `est > 0`. Recipes whose ingredients are all at hand come first, then by how many of their ingredients are `expiring` (use those first), then by the share at hand. Each suggestion lists what is missing. Planning a meal creates a 计划 entry for a date and slot; marking it 已做 does not change the stock, because recipes do not record amounts. Adjust items with 用掉 on the stock tab or with the `household_use` tool.

## Today's advice

`household_advise` and the 「今天的建议」 block on 总览 come from one pure function, `rules.Advise`: the same records and day always give the same advice, and it reuses the functions behind the rest of the page, so the two cannot disagree. In rank order:

1. **先吃**: the items with an `expiring` finding, soonest first (the 即将过期 number).
2. **可以做**: the first three meal ideas, exactly those of 今天吃什么 and `household_meal_suggest` with `limit` 3, each saying which soon-expiring ingredients it uses and what is missing.
3. **家务**: the chores due, most overdue first (the 家务到期 number).
4. **该买**: the items with an `expired`, `out`, `low` or `runout` finding (the 补货 part of 需要处理), those not yet on the shopping list first. The reason gives the numbers from the records: days past the date, the estimate against the threshold, or the days left at the daily use.
5. **待买** (kind `shopping`): last and least urgent, the open purchases on the shopping list that no 该买 tip names (whose item needs no buying, or that have no item), in the order of the list. With the 该买 tips marked `on_list` they cover every open purchase, so the advice is empty only when 待买 is 0 as well.

Each tip carries its reason and the ids (`item_id`, `recipe_id`, `chore_id`, `buy_id`) the existing tools act on. The tool also returns the same `summary` as `household_status`. The records have no "opened" state, so 先吃 goes by the expiry date alone. The page block has at most one line per kind, and each line opens the tab where the user acts (库存, 餐食, 采购, 家务); the 待买 line reads 「采购清单上还有 n 样没买：…等」 (「另外还有」 when 该买 items are on the list too). It says 「今天没有要特别注意的」 only when there is no tip at all.

Below the advice, 需要补货, 先用掉 and 到期家务 appear only when they have rows. The empty ones are folded into one quiet line under the advice (「库存里没有缺的，没有快过期的，没有到期的家务。」); with no advice at all they are all empty and the advice line says it. 今天吃什么 always shows: its empty line is the next step (add recipes on 餐食).

## Reminders

The extension starts with the hub (`activation_events: ["on_startup"]`) and checks the rules every ten minutes and whenever the records change.

| Extension point | Used for |
| --- | --- |
| Badge on the navigation entry (`ui.badge`) | The number of findings that need a purchase plus the chores due; `danger` when something is expired or used up, otherwise `caution`; cleared at zero |
| `POST /api/v1/notify` (scope `notify`) | One notification per day, at or after the configured time, when anything needs attention: the title has the count, the body lists expired, used-up, low, soon-used-up and soon-expiring items and the chores due. It carries the collapse id `household-<date>` and opens the 家庭管家 page |
| `POST` / `PATCH /api/v1/todos` (scope `todos.write`) | When the settings choose to-dos: one to-do per item that needs a purchase and per chore due, with today as its due date. An item keeps its one to-do while its finding gets worse. When the item is stocked or the chore done, the to-do is marked done; so are the open ones when to-dos are switched off. At most 20 to-dos are created in one round |

Which to-dos and notifications were already sent is recorded in `remind.json` in the extension's data directory, so a restart repeats nothing. A hub that is unreachable or an assistant that is offline (`503`) is retried in the next round; one failing call does not hold back the others. A to-do whose creation has an unknown outcome is not created again, because the to-do API has no idempotency key and the hub may have made it: a `408`, another `5xx` (such as `502` `assistant_in_doubt`), a connection lost after the request was sent, or a reply without a to-do id. The log says so once, with the to-do's title; that alert is recorded in `remind.json` and gets no new to-do while it lasts. Without the id, its to-do is not marked done automatically, so check and close it in the to-do list. Once the alert ends, the record is dropped, and the next time the item runs low or the chore falls due it gets a to-do again. Other `4xx` replies, `503` and a hub that refuses the connection are retried in the next round.

Settings (`configuration`):

| Key | Default | Meaning |
| --- | --- | --- |
| `remind` | `notify` | `notify`, `todos`, `both` or `off` |
| `remind_time` | `08:30` | The time of day (hub time zone) from which the daily reminder is sent |

## Assistant tools

`household mcp` serves these tools over stdio. The read-only ones are listed in the manifest's `allow`.

| Tool | Read-only | What it does |
| --- | --- | --- |
| `household_status` | yes | Findings, purchases still open, chores due, and the meals planned for today and tomorrow |
| `household_list` | yes | The records of one type (`place`, `item`, `buy`, `chore`, `recipe`, `meal`), optionally only those whose title contains a text; items come with their estimate and finding |
| `household_meal_suggest` | yes | Recipes ranked by the ingredients at hand, with what is missing and what to use first |
| `household_save` | no | Creates or changes one record of any type; the kernel checks the values against the type |
| `household_use` | no | Takes an amount off an item's estimated quantity and sets `counted` to today |
| `household_restock` | no | Creates the purchases described under "Shopping list" |
| `household_purchase` | no | Marks purchases 已买 (stocking the items) or 不买 |
| `household_chore_done` | no | Completes a chore and schedules the next round |
| `household_remove` | no | Deletes a record; relations pointing to it are removed |
| `household_import_preview` | yes (stores a draft, never changes a record) | Plans the lines the assistant read off a receipt, a photo, an order screenshot or a text against the records and stores the plan as a draft with a `revision`; answers per line what will happen, what was inferred and what needs the user's answer, the totals and a summary to read back. With `draft_id` (and `edits`) it plans a stored draft again as its next revision; a draft the user pasted on the page is re-planned only on the page |
| `household_import_list` | yes | The drafts waiting (including pasted lists from the page) and the last import, with whether it can be undone |
| `household_import_apply` | no | Writes exactly the `revision` of a draft the user agreed to (only `draft_id` and `revision`: corrections, answers and skips go through the preview, which makes a new revision), in one transaction; refuses a draft pasted on the page, another revision, open questions, and drafts whose outcome the records have changed |
| `household_import_undo` | no | Undoes the last import while nothing it touched has changed since |
| `household_import_discard` | no | Drops a draft; no record changes |
| `household_advise` | yes | Today's advice from the records, ranked, each with its reason and record ids (see "Today's advice") |

Meals are planned with `household_save` (type `meal`, with `date`, `slot` and `recipe`).

The tools that change records (`household_save` and the others marked "no", including `household_import_apply`, `household_import_undo` and `household_import_discard`) are left out of `allow`, so the hub asks the user before each call. `readOnlyHint` is true exactly for the allowed ones; `destructiveHint` marks the tools that overwrite or delete records (save, use, purchase, chore done, remove, import apply, import undo).

## Import

The user sends the Persona assistant a photo of a receipt, the fridge or a shelf, an order screenshot or a text and says, for example, 「把这张小票记到家庭管家」. The assistant's own model reads it; the extension adds no model and no key. It gets lines (`title`, and when readable `qty`, `unit`, `category`, `place`, `expires`, `bought_on`, `price`, `target` `stock` or `shopping`) and plans them in `internal/imports`:

- **Names.** Titles are trimmed, full-width letters and digits made half-width, and spaces closed up. A line matches a recorded item when the names are then equal (exact). Names that only come near (one contains the other, or they agree once sizes such as 250ml×12, brackets and words such as 纯 or 进口 are left out; 「纯牛奶」 and 「牛奶」) are never merged on their own: the line becomes a question with the candidates and 当新的记.
- **Units.** Aliases are resolved (kg → 千克, ml → 毫升). Weights and volumes convert (斤 → 千克, the preview says so). A unit that does not convert (盒 against 个) is a question; a missing unit takes the item's, marked inferred. An item counted by quantity without a quantity on the line is a question too; the extension never assumes one.
- **Duplicates.** Lines of the same name and target in one import are added up when their units are the same or convert (斤 into 千克, the earliest date kept). A line of the same thing that cannot be added (a unit that does not convert, or a quantity on only one of them) becomes a question: two steps for one item would double it or, when counting, overwrite each other.
- **Stock lines.** A new item, or more of a recorded item (`a → b` on the estimate, the same rules as buying: a past date means the old batch is gone, and the preview says so). When the item, or an entry of the same name without an item, is on the shopping list, it is marked 已买. Photos of the fridge or a shelf count instead of adding (`mode` `count`): the quantity becomes what was seen, which can be lower than what is recorded; such lines are marked 会变少 (`lowers`) with 「会从约 a 改成 b」, counted in the totals and in the confirmation, and the tool's description and the skill tell the assistant to say so and ask when a photo may be incomplete. Values a recorded item lacks (unit, category, place) are added. A given expiry date replaces the stored one, shown as `old → new`, except in add mode when the stored date is still ahead and earlier than the new one (reminders stay on the older batch); in count mode a different date always replaces the stored one.
- **Shopping lines.** Already on the list (an open purchase of that name, or for an item of that name), a question when only a near name is there, else a new 待买 purchase linked to the one item of that name.
- **Inferred values.** A small built-in table of common groceries and household goods suggests a category and a recorded place whose name has a fitting word (冰箱, 厨房柜, 卫生间 …). A name matches a word of the table only when it is the whole name or its end (the head of a Chinese product name: 土鸡蛋, 进口香蕉), the longest word wins (洋葱 over 葱), and a short exclusion list covers names that end in a word but are something else (棉花糖, 果冻 …); so 鱼罐头, 贝果, 葱油饼, 糖果 and 盐焗鸡 get no suggestion: none is better than a wrong one. Every such value is listed in `inferred` and marked （推测）. The usual shelf life of fresh goods (鲜牛奶 7 days, 鸡蛋 30 …) is only said (`usual_days`, 「一般能放 7 天左右」) and never stored as the expiry date: a guessed date would raise 已过期 and make a later purchase or import throw the batch away, so the safer choice is to store only dates the user or the package gave. A place the line names but the household lacks is created.

The plan is stored as a draft in `imports.json` beside the records (`imports.lock` guards it; the file has a `version`, and a broken file or one of a newer version is refused, never overwritten, and the page says so). A draft lives 24 hours; at most five are kept. A new draft pushes the oldest out, but an assistant's draft never pushes out a draft the user pasted on the page; it is refused instead. Each time a draft is planned again (an answer, an edit, 重新算一遍) its `revision` goes up, and a yes is for one revision: the page's buttons carry the revision they were drawn for and an answer names its choice, and the tool's apply requires the `revision` it previewed, so a plan changed in between is refused with 「预览已经变了，请先看一遍新的再确认」. Applying re-plans the draft against the records as they are and refuses when a line the user did not edit in that call would now promise anything else (its action, records, quantity and result, unit, dates, place, category, whether the old batch is gone or the count goes down), when a question is left, or when nothing would be written. It then runs every step through `internal/ops` (`Save`, `Purchase`, the new `AddStock`) on a copy of the base: one failure writes nothing. It records each record it touched as it was before and right after, in two phases: the import is recorded as pending (the draft stays) inside the records' update and confirmed once the records are saved; a failed save takes it back. An import or undo left pending by an interrupted program is settled at the start of the next write of the records, under their lock and before anything else changes (page actions, every assistant write tool, the program's start), and, without writing anything, whenever they are read. At that moment the records are exactly the state before or after the interrupted save, so the decision is exact: written only when every record the import touched equals the state it left, not written only when every one equals the state before (a created record absent), never by the mere existence of an id (ids are sequential). Anything else is not guessed: the entry is marked unknown, its draft cannot be applied again (the page says so and offers 放弃), and no undo is offered for it, while the previous import's undo is not hidden by an import that was never written. An undo whose outcome cannot be decided is not called done: the page says so, offers no undo and has 「知道了」 to hide the notice without touching records. While `imports.json` cannot be read, writes of the records are refused with 「导入记录读不出来，先处理好再改」 (a pending import must stay decidable); reading goes on. Two new things of near names in one import (酸奶 and 老酸奶) are asked about like a recorded item (「和第 n 行是同一样」 or 当新的记). When one import both stocks a thing (牛奶 2盒) and lists it to buy (牛奶 to the shopping list), the list entry stays open and is not marked bought: the user bought some and still needs more, and the preview says so. Lines that turn out to be the same recorded item (by name or by the user's answer, such as 「小番茄」 answered 就是「番茄」) are merged after matching, so the preview shows the final quantity of each record. Undo works the same way and restores the last import (removes what it created, puts back what it changed, purchases back to 待买) only while every touched record is still exactly as the import left it and no other record points at one it created; otherwise it names what is in the way and changes nothing. The last ten imports are remembered; only the newest can be undone.

**On the page.** When drafts exist, the 总览 tab shows 「待确认的导入」 right under the numbers (the newest three, at most eight lines each): source, time and note, each line with what will happen and a badge (要你定, 会变少, 有推测, 不改, 没读懂), the totals, 「确认写入」 (its confirmation counts the changes by kind, including 「n 件按看到的记数量，其中 m 件会比现在记的少」) and 「放弃」. A line with a question opens a page with its answers. A draft with more lines than the overview shows offers 「看全部 n 行再确认」 instead of 确认写入: its full list has every line, every question and the buttons, so nothing is confirmed unseen. A draft the records have overtaken offers 「重新算一遍」. After an import, 「刚才的导入」 shows for a day with 「撤销这次导入」, or why it can no longer be undone. The overview card has a line 「有 n 份导入等你确认」.

**粘贴清单.** A page-only entry that needs no model: one thing per line (「牛奶 2 盒」「鸡蛋 12 个 2026-12-05」「两瓶酱油」), where it goes and whether it adds or counts. The parser reads a name, a quantity in Latin or Chinese digits (两, 十二, 半) with a known unit, or a bare Latin number after a space or ×, a date (2026-12-05, 2026/12/5, 2026年12月5日, or 12月5日 in this year, said in the note) as the expiry date, and a price (¥18.5); bullets and numbering are dropped. A line that is only a number or a date, has no letters, reads like a sentence, is too long or has an impossible date is shown as 没读懂 with the reason, never guessed. The result is the same draft as the tool's, marked as the user's. At most 60 lines are read; the rest are one 没读懂 line saying how many.

**记新东西.** The last section of 总览 (and of the empty overview) has the three ways to add things, each a row with one sentence and its button under it: 记一样 with 「添加物品」 (the item form), 自己粘贴一份清单 with 「粘贴清单」 (read by the page itself, no assistant involved, the same preview and 确认写入), and 让助手帮我记, which says what to send the assistant and has no button.

**「让助手帮我记」.** Its row says what to do in the assistant. An extension page cannot open or prefill an assistant conversation: `wire.Action.reply` and `open` (and `Push.page`) are for views shown inside an assistant conversation only, and `ActionResult.open_url` opens https links. The hub scope `assistant.ask` (`POST /api/v1/assistant/conversations`, used by homeassistant and space-conversations) starts a conversation with a prompt the extension writes, but it cannot carry the user's photo, the page cannot open that conversation on every hub, and it would add a permission to accept on upgrade, so this version does not use it.

## Data and location

Records are kept in the extension's own data directory, in `spaces.json`, the kernel's file format, written by the same store as spaces (lock file, temporary file and rename). `remind.json` and `imports.json` (import drafts and the last imports, for undo) sit beside it.

`household mcp`, started by the assistant, finds the directory in this order: `--data`; the `PERSONA_HOUSEHOLD_DATA` environment variable; the directory the running extension recorded in `persona-household/data-dir` under the user's configuration directory, when that directory exists; `persona-household/data` under the user's configuration directory.

The running extension also records the hub's time zone beside the records, so `household mcp` counts days the same way as the page; without that record it uses the zone of the machine.

The assistant's command is `household`, looked up on `PATH` and then in `~/bin`; `PERSONA_HOUSEHOLD_BIN` names the binary directly, and `off` leaves the server out. See "Making the assistant's `spaces` command findable" in [spaces.md](spaces.md): the steps are the same with the other name.

## Design notes

**An extension of its own, on the shared kernel.** A template pack inside spaces can describe the six types but not the rules: the kernel's derived properties are aggregates over relations and cannot express "estimated quantity at or below the threshold", decay by daily use, an idempotent restock, or a reminder that runs in the background. Those need code, and they do not belong in the general kernel. So household is a separate extension that uses the kernel as a library and adds a domain layer.

**How the kernel is shared.** Go does not allow `extensions/household` to import `extensions/spaces/internal/...`. The kernel and its file store therefore move, unchanged, into the shared module that the repository already has for code used by more than one extension:

- `extensions/spaces/internal/kernel` → `pluginkit/spaces/kernel`
- `extensions/spaces/internal/store` → `pluginkit/spaces/store`

Both extensions reach them through their existing kind of `replace` directive for `pluginkit`. The move changes import paths only; spaces keeps its behaviour, its tests and its version. The kernel stays free of dependencies (its purity test moves with it). The alternatives were rejected: a copy of the kernel in household would drift; a public facade inside the spaces module would make one extension a library of another and tie their releases together; a new top-level module would need a change to the layout rules for no gain.

**Its own data file, not the spaces file.** An extension installed on the hub may run in a sandbox that can write only its own data directory, so household cannot open the file of spaces. It keeps a kernel base of its own with the single space `household`. The format is the same, so a later hub-provided store (the kernel's `Storage` interface) or an import into spaces can bring the two together without converting anything. Until then the household records do not show up on the 空间 page.

**The schema belongs to the extension.** On every start, and before every change, the extension makes sure the space and its six types exist and match this document (missing types are created first without their relations, then every type that differs is written in full, the same two-pass order templates use). Reading never writes: on a new data directory the read-only tools answer with nothing. There is no tool that changes the types; a file edited by hand so that a type differs is brought back to this schema, and values of properties the schema does not have are dropped (a record that loses a value counts as changed then, so an item without `counted` restarts its estimate). The one exception: when a record holds a value this schema does not accept (an option added by hand, say), that type is left as it was found, so the extension keeps working.

### Packages

| Package | Role |
| --- | --- |
| `internal/model` | The schema (space id, type and property keys, stage and option names), `Ensure`, fictional sample records, and `Load`, which reads a space into typed records |
| `internal/rules` | Pure functions over the typed records: estimate, findings, restock suggestions, chores due, meal ideas, summary, today's advice |
| `internal/ops` | The transactions: save, use, restock, purchase, add stock, restore, chore done, remove, load samples. Each works on a `*kernel.Base` inside one `Update` |
| `internal/remind` | The hub client (`notify`, to-dos), the daily plan and its `remind.json` state |
| `internal/ui` | The page and the overview card |
| `internal/imports` | The import: matching and merging, the built-in table, the paste parser, drafts in `imports.json`, apply and undo |
| `internal/mcp` | The stdio MCP server and the fifteen tools |
| `internal/datadir` | Where the data directory is, and recording it for `household mcp` |

## Limits

- One household per hub.
- The kernel's limits apply: 5,000 records in the space, 24 properties per type.
- Quantities are estimates when `daily` is set; count again and save to correct them.
- Recipes list ingredients without amounts, so cooking a meal does not reduce stock.
- Changing a purchase's stage with `household_save` does not stock the item; use `household_purchase`.
- Reminders need the hub's `notify` or `todos.write` permission; without them only the badge and the page show what is due.

## Development

```sh
go -C pluginkit test -race ./spaces/...
go -C extensions/household test -race ./...
scripts/package.sh household
```

All sample records in the tests and behind 载入示例 are made up.
