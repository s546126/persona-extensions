# Token usage

[Documentation](../README.md)

`token-usage` summarizes how many tokens each computer runs and keeps 400 days of it. The hub keeps about a week; the extension pulls that and stores the rest itself.

The navigation page has three tabs. 总览 shows today's, the last 7 days' and the last 30 days' totals, each against the period before, a daily chart, and the top models, computers (电脑) and launch methods (启动方式: the program that opened the conversation on the computer, with sessions opened straight from a terminal shown as 终端直接打开). 明细 is the same data filterable by computer, model and launch method, one row per day. Its 「显示 CSV 文本」 button shows those rows as CSV text in a code block on the page, one line per day, computer, model and launch method under Chinese headers, for the user to copy into a spreadsheet or save as a `.csv` file. The extension protocol cannot hand the apps a file: an action answers only with a view, a toast, a link, a page or lines. A CSV over 190,000 bytes is cut at a whole row, and the page says how many rows it shows. 预算 sets a daily or weekly limit for one model or for all of them. The overview card (「Token 用量 · 概况」: the hub's settings page lists each page and card by its title, so the card's differs from the page's) shows today's total against the 7-day daily average and the busiest model.

When a pull fails, 总览 and 明细 say so above the data already stored, in plain words with what to do. If nothing could be read, they give the cause. If only some computers could not be read, they name them, out of how many there are. A computer the hub no longer knows (404) is described as probably removed, not as an outdated hub. The technical detail goes only to the extension log.

## Data

The extension reads only endpoints the hub already has: `GET /api/v1/machines` and `GET /api/v1/machines/{id}/usage` (`machines.read`), and, when the hub offers it, the `usage.gateway_accounts` extension method (`usage_gateways`) for the gateway account quotas. It asks for nothing else and adds nothing to the hub.

A background loop starts with the extension, pulls the last 7 days once and the last 24 hours every 10 minutes after that. Each bucket is one minute of one model and launcher on one machine and replaces the stored copy of that minute, so a pull never double-counts.

## Storage

The history is one JSON file in the extension's data directory, locked with `flock` while it is written and replaced atomically. It is summed per day, machine, model and launcher and pruned to 400 days. The budgets and the record of which alerts were already sent are a second JSON file. A file that is not valid for its format is left untouched.

## Budgets and alerts

Crossing 80% and 100% of a budget each sends one notification through `POST /api/v1/notify` (`notify`), and neither is sent again the same day. A model whose current hour exceeds five times its average for that hour over the previous seven days sends one notification that day as well.

## Assistant

The `token-usage mcp` command speaks the Model Context Protocol on stdio. `usage_summary(range, group_by)`, `usage_top(range, dim, n)` and `budget_status()` only read; `budget_set` and `budget_delete` change a budget. The read-only three are in the manifest's `allow` list, so they run without asking. The bundled skill explains to the assistant when each tool applies. The command is found on `PATH` or in `~/bin`, unless `PERSONA_TOKEN_USAGE_BIN` names it.
