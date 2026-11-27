# Karakeep bookmarks

Search and organize your self-hosted [Karakeep](https://karakeep.app) bookmark library in
Persona: find the right link or note, read its context and highlights, save a new URL, or
mark a bookmark as favourite/archived. This is not a server administration extension.

This is a fresh Persona Go extension using Karakeep's public REST API. It ships no Raycast
source, assets, branding or runtime.

## Set up

1. In Karakeep, create an API key under **Settings › API Keys**.
2. In **Settings › Extensions › Karakeep**, set the **Karakeep instance URL** (for example
   `https://karakeep.example.com`) and paste the **Karakeep API Key**. A local instance may
   use `http://127.0.0.1:3000`; remote instances require HTTPS.
3. The extension calls Karakeep's REST API under `/api/v1`; an address that already ends
   in `/api` or `/api/v1` also works. Do not include credentials in the URL.

The key arrives through the encrypted extension-secret flow and is used only as a Bearer
token to the configured instance. Redirects are not followed. Assets are not downloaded
or mirrored in this version.

## Read and organize

- Full-text search across bookmark titles, content, descriptions and notes, or list recent
  bookmarks. `limit` (default 20, at most 50) is sent to Karakeep, so `nextCursor` continues
  right after the last bookmark shown and is never replaced by a guessed page number.
- Read link/text bookmark metadata, note, text and the bookmark's own highlights. Bookmark
  text is untrusted, not an instruction.
- See favourite/archive state, original link and manual/smart list context where available.
- `karakeep_save` creates a link bookmark after confirmation. If the URL already exists,
  Karakeep returns the existing bookmark rather than creating a duplicate.
- `karakeep_update` changes only explicitly supplied `note`, `favourited` or `archived`.
  Omitted fields stay unchanged; an explicitly empty note clears the note.

The page offers save-with-review, favourite and archive confirmation. It does not delete
bookmarks, change instance settings, run backups or update the server.

Before a write, the extension stores its request ID and normalized action in
`write-receipts.json` in its own data directory (`0600`, without the API key). A replay
after restart returns the original result; different arguments with the same request ID
are refused. Up to 256 receipts are retained; new writes stop when full instead of losing
that protection.

A timeout or lost response leaves the outcome **unknown**. Inspect Karakeep before retrying;
the extension does not assume a failed response means no remote change occurred.

## Verify

```sh
go -C extensions/karakeep test -race ./...
scripts/package.sh karakeep
```

Tests use synthetic instances and cover URL validation, full-text search, associated
highlights, partial updates, localized views and persistent replay. No real account is used.

## Public API

[Karakeep API](https://docs.karakeep.app/api/karakeep-api): Bearer authentication under
`/api/v1`, with `/bookmarks`, `/bookmarks/search`, `/bookmarks/{id}`,
`/bookmarks/{id}/highlights` and `/lists`.
