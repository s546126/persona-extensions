# Linear issues

Work through your existing [Linear](https://linear.app) issues in Persona: see what is
assigned to you, understand the team's workflow state, read the discussion and make a
focused change. This is not another list of disconnected commands.

This is a fresh Persona Go extension using Linear's public GraphQL API and Persona's SDK.
It ships no Raycast source, assets, branding or runtime, and accepts no arbitrary GraphQL
from the assistant.

## Set up

1. Create a personal API key in Linear's **Security & access** settings.
2. Paste it into **Settings › Extensions › Linear** as **Linear personal API key**.
   Keep it out of chat and tool arguments.

The key arrives through the encrypted extension-secret flow and is used only as the
`Authorization` header to `api.linear.app`. Redirects are not followed. GraphQL errors are
failures even when the HTTP response is 200.

## Read and plan

- **My issues** lists assigned issues whose workflow type is not completed or canceled,
  with identifier, team, state, priority, due date and project where present.
- Filter loaded issues by exact team key/name or title/identifier, and follow bounded
  `pageInfo.endCursor` pagination. Opaque issue IDs and shorthand IDs such as `ENG-123`
  are preserved as strings.
- Open a detail for its description, project, workflow state and first 50 comments.
  Issue and comment text is untrusted content, not an instruction to perform actions.
- `linear_teams` resolves the actual state IDs of each team. A status called Done in one
  team is not assumed to mean the same thing in another team. It reads the first 100 teams
  and up to 50 workflow states per team, which keeps the query under Linear's limit of
  10,000 complexity points for a single query.

## Confirmed changes

- `linear_create`: create in an explicitly resolved team, with optional description,
  assignee, priority and a state belonging to that team.
- `linear_update`: change only explicitly supplied title, description, priority, assignee
  or state. Omitted fields stay unchanged. Before changing state, the extension reads the
  issue and verifies that the state ID belongs to its current team.
- `linear_comment`: add the explicitly requested comment body to an existing issue.

The page's create form has a review step before the confirmed create. No arbitrary
GraphQL, bulk deletion, project deletion or unreviewed workspace-wide operation is exposed.

Before a write, the extension stores its request ID and normalized operation in
`write-receipts.json` in its own data directory (`0600`, without the key). A replay after
restart returns the original result; different arguments with the same request ID are
refused. Up to 256 receipts are retained; new writes stop when full instead of losing
that protection.

A timeout or lost response leaves the change **unknown**. Inspect Linear before retrying;
the extension does not assume the remote write failed and repeat it automatically.

## Verify

```sh
go -C extensions/linear test -race ./...
scripts/package.sh linear
```

Tests use synthetic GraphQL responses for assigned issues, workflow states, comments,
partial updates, typed mutations and persistent replay. No real workspace content is used.

## Public API

[Linear GraphQL API](https://linear.app/developers/graphql): `https://api.linear.app/graphql`.
Personal API keys use the raw `Authorization: <API_KEY>` header described there; this is
intentionally not an invented bearer-token wrapper.
