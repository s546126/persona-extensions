# Feishu Sidebar

[Documentation](../README.md) · [Source](../../extensions/feishu-sidebar/)

Open Persona in Feishu's desktop navigation. The extension builds a Feishu AppLink from the configured hub address. Clicking it in Feishu opens Persona in a tab in the left navigation. It needs no Feishu app id, makes no request to Feishu, and keeps no credentials.

## How it works

Feishu's general link (no app id, no web app):

```text
https://applink.feishu.cn/client/web_url/open?mode=appCenter&url=<percent-encoded target URL>
```

`mode=appCenter` opens the web page as a tab in the desktop client's left navigation. Right-click the tab and choose **Pin** to keep it there.

The extension percent-encodes the target URL the way JavaScript's `encodeURIComponent` does (Chinese, `?`, `#`, `&` and `=` all encoded), so the query and hash of the target path are carried whole.

## Configuration

| Setting | Purpose |
| --- | --- |
| `hub_url` | **Required.** The address Feishu's built-in browser can reach the hub at, such as `https://persona.example.com`. The page and `status` stay `not_configured` until it is set. |
| `path` | The same-site path to open by default, such as `/` or `/j/<conversation-id>`; starts with `/`. Default `/`. |

A trailing `/` on the address is normalized away. Only same-site relative paths are accepted (they start with `/`, are not a full or scheme-relative URL, and have no `..` segment); a path that fails these is refused rather than escaped into a different site.

## Contributions

| Contribution | Behavior |
| --- | --- |
| `ui` `feishu-sidebar` (`nav_page`) | The page shows the built applink (an "open in Feishu" link plus the raw link to copy), the target it opens, and the notes below. With no address it shows what to fill in instead. |
| `connector` `feishu` (`status`, `tools`) | `feishu_sidebar_link` builds the applink for a path (default: the setting). `status` is `not_configured` when the address is missing. |

## Notes shown to the user

- Use Feishu desktop 7.5 or newer to open the link in the left navigation.
- The first open asks for a login inside Feishu's built-in browser: the web sign-in is separate from the app's.
- The extension never calls Feishu and keeps no credentials; the link is built locally.

## Package and install

Use the [public market](../installation.md), or package locally from the repository root:

```sh
scripts/package.sh feishu-sidebar
```

Run the tests:

```sh
go -C extensions/feishu-sidebar test ./...
```

The [manifest](../../extensions/feishu-sidebar/persona-extension.json) declares its contributions and platforms.
