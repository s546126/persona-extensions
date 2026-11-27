# Magpie Subscription Usage

This extension shows subscription allowance windows from Magpie in the Persona usage sidebar for any Magpie subscription provider that reports periodic windows (such as 5-hour, daily, weekly, monthly, model-scoped and pooled windows).

## Install and sync

1. Use a Persona hub and manager with [direct usage sources](https://github.com/nwverse/persona/pull/15509). The extension requires hub build `202611251338` or later.
2. Open **Settings → Extensions → Extension markets**, add the [official Persona extension market](../installation.md#register-the-public-market), and install **Magpie Subscription Usage**.
3. Open the subscription usage popover below the sidebar. With Magpie running on the same machine as the Persona manager at `http://127.0.0.1:3425`, subscription accounts with periodic allowance windows appear automatically during the next poll. If local Magpie uses a different port, change **Local Magpie address** in the extension's settings.

The local connection only reads Magpie's quota snapshot and sends no credential. Persona uses its normal sandbox proxy to reach the manager machine's local Magpie. To stop the reads, disable the contribution or the extension.

## Account names and provider filters

- **Account display names**: By default each row uses the account's Magpie alias when set, otherwise its subscription username, falling back to its Magpie card name. Underlying row IDs stay derived from an installation-local HMAC-SHA256 digest so pinned sidebar accounts remain stable across reorders and alias edits.
- **Anonymous mode**: Turn on **Hide account names (anonymous mode)** in the extension settings to replace account names with opaque per-account aliases (`Magpie …`).
- **Provider filters**: Leave **Only sync these providers** and **Exclude providers** empty to sync every Magpie `subscription` account that has valid periodic windows (subscriptions without periodic windows and prepaid balance cards are skipped). Add providers such as `codex` to **Exclude providers** to avoid duplicating local usage reads, or list specific providers in **Only sync these providers**.

## Use a remote Magpie

In the extension settings, save **Remote Magpie HTTPS address**, then enter a dedicated **Remote Magpie quota key**. Use a read-only key with permission to inspect the desired quotas. The extension contacts the saved HTTPS origin over its scoped network permission, sends the key only with the quota request, and refuses redirects. If you change origins, Persona clears the old key. Clear the remote address to return to the local source.

Only periodic window percentages, normalized plan labels, sampling and reset times, and configured account display names are sent to Persona. Missing reset clocks remain absent. A failed refresh retains the last sample with Persona's normal freshness indicators. Model keys, cost estimates and usage ledgers stay out of the sidebar.
