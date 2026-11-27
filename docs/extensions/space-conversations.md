# Space Conversations

A space keeps one part of your life together: its own Persona conversations, a SPACE.md with long-lived instructions, and an information boundary. This is the optional product surface for Persona's domain Spaces, not the schema-driven records and boards of the separate `spaces` extension.

## What it does

- One page (the `空间对话` navigation entry, and the `空间` page with a space as its state) with a compact switcher on top: 全部, each active space, and `+ 新建空间`, as tabs with each space's conversation count (99+ above 99) while there are at most six spaces, as a drop-down with more. Apps before UI version 4 get the drop-down, apps before UI version 3 a row per space. An archived space opened from elsewhere stays in the switcher while it is shown; a space that no longer exists shows 全部 with a note.
- 全部 is an overview: each space with the first line of its SPACE.md (or its description) and how many conversations it has, then the newest conversations of all spaces merged (at most 30), each labeled with its space. Up to 20 spaces are read, four at a time; a space that cannot be read is still listed, without its count, rather than failing the page.
- A chosen space leads with a compact single-line field to start a new conversation (with launcher, optional model ID, and effort selectors when the hub provides `GET /api/v1/assistant/tracks/launchers`; older hubs degrade cleanly to default start) and its recent conversations (newest first, with the count and last active time in the section footer) on the first screen. Below the conversations, SPACE.md is folded into a single-line summary row with a view/edit action (and a note when the assistant's computer cannot be reached), alongside the archive or restore button.
- Creates a space with a stable slug and optional information categories.
- Reads and edits the human-owned part of SPACE.md with the core's revision and managed-block checks.
- Archives and restores spaces without deleting their conversations, files or SPACE.md.

## Identity and data

The extension ID is `space-conversations`; its display name is `空间对话`. The existing `spaces` extension and its own records are unchanged.

Track records remain in the assistant's Postgres tables and SPACE.md remains in the track's existing directory on the assistant's computer. The extension does not copy these into a private JSON database. Disabling or uninstalling the extension hides the product surface; it does not delete those records.

The declarative `track_kinds` contribution supplies the `space` kind. Core retains execution, directory isolation, information policy, conversation filtering and managed SPACE.md behavior.

## Permissions

- `spaces`: manage domain-space metadata and SPACE.md, including creating and archiving a space.
- `assistant.ask`: list a space's conversations and start a new one.

Conversations are read from the hub's space-scoped route (`GET /api/v1/assistant/tracks/{slug}/conversations`), which answers an extension only for spaces its policy shows and leaves out the conversations it hides. A hub without that route refuses it (403 without a scope, or 404/405 when the assistant's manager is older than the hub), and the extension then asks the older assistant listing for the space; counts are then those of the listing.

This scope is explicit and is not inherited from the legacy every-scope token. Sensitive tracks, including Health, remain subject to their existing extension-denial policy.

## Upgrade behavior

On a hub that already has domain-space records, the core migration installs this extension once from the signed official marketplace and starts it. A hub with no such records does not receive it automatically. An already installed extension, including one the owner disabled, always wins over migration. Existing conversations and files are not migrated or deleted.

## Development

```sh
go -C extensions/space-conversations test ./...
scripts/package.sh space-conversations
```

The UI uses only declarative SDK views. No JavaScript or executable code runs in the clients. The conversation handoff is a narrow app-owned route containing a computer, space and session, not a general internal-link permission.
