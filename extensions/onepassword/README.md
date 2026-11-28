# 1Password

Connect Persona to the 1Password vaults you choose, with a 1Password service account or with the 1Password CLI (`op`) on one of your computers. The assistant can then list those vaults, find items by title, tag or website, and see which fields an item has together with each field's `op://` secret reference. No secret value reaches the assistant: when it describes an item, 1Password hands the whole item to the extension's process (on the hub for a service account, on that computer for `op`), which keeps only names and types and drops the values at once, so no password, key, note or one-time code reaches the assistant, the knowledge base, the Inbox or a log. Nothing it does changes your vaults. To spare 1Password's request limits it keeps vault and item metadata (names, fields and references, never values) in memory for 5 minutes and in a bounded Hub-sealed metadata record across restarts. Older metadata remains a fallback while authorization has lapsed or the rate limit holds; values are never part of this record.

It is also Persona's source for `op://` references. Another extension's secret or a model provider's key can hold a reference instead of a pasted value (**Use a reference** in Persona's settings), and a value can be handed to one Agent run after you approve it on your phone. The extension resolves the reference when Persona needs the value and gives it to Persona's manager alone; Persona keeps no copy on disk. The connection page lists the recent uses.

The service account token is the extension's secret, encrypted on the hub and checked with 1Password before it is saved. Instead of a token you can pick one of your computers in the extension's settings: Persona then runs the extension's program there, which uses `op` as signed in through the 1Password app (Settings › Developer › Integrate with 1Password CLI), and the app asks you to approve it. Settings › Extensions › 1Password › 连接 shows whether 1Password answers and which vaults it can read.

References need a Persona hub with secret providers ([nwverse/persona#3962](https://github.com/nwverse/persona/issues/3962)); an older hub installs the extension and leaves that part unused.

- [Extension guide](../../docs/extensions/onepassword.md)
- [Installation](../../docs/installation.md)
- [Development and tests](../../docs/development.md)
- [Manifest](persona-extension.json)


Version 0.4.4 writes `OTP` fields with 1Password's native `TOTP_` field identity, both when creating a field and
when converting an existing one (the converted field gets a new ID; use the returned `op://` reference). A write
reports success only when the saved field reads back as type `OTP` with a live one-time code; a CLI downgrade to
`CONCEALED` now fails instead of passing silently.

Version 0.4.3 never prompts on everyday asks when `op` runs on a computer: an ask with an earlier answer (a value
Persona keeps, metadata kept here, a description) checks `op whoami` first and answers from what it had while the
1Password app's authorization has lapsed, so only something new needs the user to authorize `op` once. See
[authorized once](../../docs/extensions/onepassword.md#authorized-once-never-prompted-on-everyday-asks).

Version 0.4.1 adds native `OTP` field support (raw base32 or `otpauth://`), built-in `LOGIN` `username`/`password`
mapping without duplicate custom fields, and `edit` field deletion and in-place type conversion (including passkey-free
`LOGIN` items). See [phone-approved writes](../../docs/extensions/onepassword.md#phone-approved-writes) for local file
input, supported categories, restrictions and authorization expiry. The metadata query tools remain read-only.

Version 0.4.5 preserves previously authorized metadata across Hub restarts in the connector's encrypted secret
storage, including original acquisition times and the private name index. A different computer/token or **Check
again** clears it. Failed or corrupt records are never used as an authorized answer. No field values, CLI sessions
or service-account tokens are copied into this metadata record.
