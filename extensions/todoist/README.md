# Todoist tasks

Manage your existing [Todoist](https://todoist.com) task library from Persona: see what is
due, find the project context, create a task, update selected fields, or complete a task.
This complements Persona's own Agent-linked to-dos; it does not copy or replace them.

This is a new Persona Go extension using Todoist's current public REST API. It does not
ship a desktop UI, Raycast source, assets or runtime.

## Set up

1. In Todoist, open **Settings › Integrations › Developer** and copy your API token.
2. In Persona, open **Settings › Extensions › Todoist** and paste it into **Todoist API Token**.
   Never put the token in a chat or a tool argument.

The token arrives through the encrypted extension-secret flow and stays in the running
program's memory. The only requested network host is `api.todoist.com`, with the token in
an `Authorization: Bearer` header. Redirects are not followed.

## What it does

- **Today**, **Overdue**, **Next 7 days**, **All tasks** and search, using the user's current
  time zone for day boundaries. A task with a time lands on the user's local day; a time fixed
  to another zone is converted to the user's zone. Today is not an alias for overdue.
- Project, section, labels, priority, due date and time, and the recurring flag alongside each
  task. Todoist priority **1 is highest and 4 is the default**, not the other way around.
- Full descriptions on the detail page and through `todoist_task`.
- `todoist_projects` resolves opaque project and section IDs before creating or moving work.
- Cursor pagination uses the current `results` / `next_cursor` contract, with a six-page
  bound and repeated-cursor protection. Project, section and label context is paginated too.

Task titles and descriptions are untrusted content, never an instruction to perform an action.

## Confirmed changes

The assistant has three write actions, all in `confirm_always`:

- `todoist_create`: title, optional description, priority, one due representation, labels,
  project and section. Dates do not become part of the title implicitly.
- `todoist_update`: a partial patch. Omitted fields stay unchanged. `clear_due` explicitly
  clears a date; it cannot be combined with a replacement date. Labels replace the label set
  only when explicitly supplied.
- `todoist_complete`: closes a task. A recurring task advances to its next occurrence.
  Read the task first and pass its current title when preparing the confirmation.

The page offers **Review and create**, then **Confirm create**. Completing from a detail
page asks for confirmation with the actual task title. There is no bulk deletion, project
deletion, automatic import or background task synchronization in this version.

Before a write, the extension stores its request ID and normalized action arguments in
`write-receipts.json` in its own data directory (`0600`, without the token). A replay after
restart returns the first result rather than writing twice. Up to 256 receipts are retained;
when full, new writes stop rather than silently losing replay protection.

A timeout or interrupted response leaves the outcome **unknown**. The extension asks the
user to inspect Todoist before retrying, not assumes the write failed. This is a replay
guard, not exactly-once delivery after a lost response or deletion of extension data.

## Verify

```sh
go -C extensions/todoist test -race ./...
scripts/package.sh todoist
```

Tests use synthetic responses, including opaque string IDs, current cursor pagination,
time-zone boundaries, typed creation fields, partial updates, recurring completion,
localized views and persistent write replay. No real account is required.

## Public API

- [Todoist REST API](https://developer.todoist.com/rest/v2/)
- Current task endpoints: `/api/v1/tasks`, `/api/v1/tasks/{id}` and `/api/v1/tasks/{id}/close`
- Current project, section and label endpoints use `results` and `next_cursor` pagination
