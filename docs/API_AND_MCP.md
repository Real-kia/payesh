# API and AI agent (MCP) access

Payesh exposes everything the dashboard can do through a versioned REST API
under `/api/v1`, and serves the same API to AI agents through the
[Model Context Protocol](https://modelcontextprotocol.io) at `/api/v1/mcp`.
Both are authenticated with **API tokens**.

**HTTPS is required.** API tokens and MCP are not supported over plain HTTP:
the server refuses to create, list, or accept tokens on an HTTP connection and
answers `403` with the error code `https_required`. Set up a domain under
**Settings → SSL / TLS** (see [Settings](SETTINGS.md)) first. If a reverse
proxy terminates TLS in front of Payesh, list it in `--trusted-proxy-cidrs` and
have it send `X-Forwarded-Proto: https` (or `Forwarded: proto=https`). Payesh
ignores these headers from any other address. Browser sign-in over HTTP is
unchanged.

## Create an API token

1. Sign in to the dashboard and open **Settings → API & AI access**.
2. Name the token, choose **Read only** or **Can edit**, and pick a lifetime
   (up to 1 year). Optionally restrict it to selected servers or actions.
3. Copy the token (`pyt_…`). It is shown once; Payesh stores only its hash.

The page also shows a ready-to-paste Claude Code command and MCP client
configuration for the new token.

Tokens can also be created from a signed-in session with
`POST /api/v1/api-tokens` (`{"name": "...", "permission": "read", "expires_in_days": 90}`).

### What a token can do

- A token acts as the account that created it. Its permission is the lower of
  the token's own permission and the account's current permission, so
  downgrading an account also downgrades its tokens.
- Read-only accounts can create read tokens only.
- Tokens cannot manage tokens or accounts. A leaked token cannot renew itself
  or create a new login.
- Token requests send `Authorization: Bearer pyt_…` and need no CSRF header.
- Resetting an account's password or deleting the account revokes its tokens.
- Each account can hold 20 active tokens. Expired tokens stay visible with an
  expired status for 30 days, so they can be extended or their activity read,
  and do not count toward the limit. Tokens expiring within seven days show an
  expiring status.
- The owner can search the token list, filter by account, view activity,
  revoke any account's tokens, and see each account's active token count.
  Only a token's own account can edit or rotate it, so the owner never
  receives a secret that acts as another user.
- Empty `server_ids` and `actions` allow all servers and actions permitted by
  the token and account. `monitoring` allows monitoring reads, `read` allows
  all reads, and `write` allows mutations subject to edit permission. Selected
  servers restrict both direct REST calls and MCP tools.

### Manage existing tokens

All token-management endpoints require an HTTPS browser session. Mutations
also require `X-CSRF-Token`. Responses use `Cache-Control: no-store`.

- `GET /api/v1/api-tokens?username=alice&search=agent` filters by owner and
  searches names, usernames, and token hints. Other accounts see only their
  own tokens. The list includes creation time, expiry status, last use time,
  and the last client IP.
- `PATCH /api/v1/api-tokens/{id}` (own tokens only) accepts `name`, `permission`,
  `expires_in_days` (1–365, measured from now), `server_ids`, and `actions`.
  Omitted fields stay unchanged; empty scope arrays clear those restrictions.
  Extending expiry can renew an expired token.
- `POST /api/v1/api-tokens/{id}/rotate` (own tokens only) returns a new secret once and
  invalidates the old secret immediately. It preserves the token's settings
  and expiry unless `expires_in_days` is supplied.
- `GET /api/v1/api-tokens/{id}/activity` returns recent request activity: time, method, path, source IP, and HTTP status. It excludes request
  bodies, query strings, and bearer secrets.
- `DELETE /api/v1/api-tokens/{id}` revokes one token.
- `DELETE /api/v1/api-tokens?username=alice` revokes all tokens for one
  account and returns `{"revoked": N}`. The owner must specify an account;
  other accounts may omit `username` to revoke their own tokens.

Activity retains the latest 100 requests per token, including denied requests,
with the final HTTP status. Request paths are capped at 256 bytes; query
strings and request bodies are not recorded.

Activity and last-use details are kept in memory and saved at most every 30
seconds, and on shutdown, so token requests never wait on a database write. A
crash can lose up to the last 30 seconds of activity. Token creation, changes,
rotation, and revocation are always saved immediately. IP attribution trusts
forwarded headers only when the request comes through a configured trusted
proxy.

## REST API

```sh
curl -H "Authorization: Bearer $PAYESH_TOKEN" https://payesh.example.com/api/v1/servers
```

The full contract is [`api/openapi.yaml`](../api/openapi.yaml); behavior notes
are in the [API contract](contracts/API_CONTRACT.md). Common conventions:

- Collections return at most 200 items. Pass `next_cursor` back as `cursor`.
- Time ranges are RFC3339 and at most 31 days.
- Errors use one shape: `{"code": "...", "message": "...", "retryable": false}`.
- Mutations of versioned resources take an `expected_revision` (read the
  resource first) and an `idempotency_key`, so retries are safe.

## MCP for AI agents

The MCP endpoint uses the Streamable HTTP transport (served over HTTPS),
answers every request with plain JSON, and keeps no session state. It requires
an API token; the browser session cookie is not accepted there.

**Claude Code**

```sh
claude mcp add --transport http payesh https://payesh.example.com/api/v1/mcp \
  --header "Authorization: Bearer $PAYESH_TOKEN"
```

**Other MCP clients** (Claude Desktop, Cursor, and others that accept a JSON
configuration):

```json
{
  "mcpServers": {
    "payesh": {
      "type": "http",
      "url": "https://payesh.example.com/api/v1/mcp",
      "headers": { "Authorization": "Bearer pyt_..." }
    }
  }
}
```

### Tools

| Tool | What it returns |
| --- | --- |
| `whoami` | The account and permission the token acts as |
| `list_servers`, `get_server` | Servers, role, platform, connection and freshness state |
| `get_metrics` | CPU, memory, disk, load, and network samples or minute/hour rollups (default: last hour) |
| `list_processes` | Current processes with CPU, memory, I/O, and connections |
| `get_traffic_periods`, `get_traffic_forecast` | Traffic allowance usage and an end-of-period estimate |
| `list_log_sources`, `query_logs` | Collected log sources and log search (default: last hour) |
| `list_active_alerts`, `get_alert_history`, `get_incident`, `list_alert_rules`, `list_maintenance_windows` | Alerting state and history |
| `list_packages`, `list_server_packages` | Optional package catalog and per-server installations |
| `get_job`, `get_update_status` | Background job progress and update state |
| `api_request` | Any other REST call, such as changing alert rules or traffic allowances |

Every tool except `api_request` is read-only. The OpenAPI document is available
to the agent as the `payesh://openapi.yaml` resource, so it can find endpoints
that have no dedicated tool.

Tool calls are run as ordinary REST requests with the agent's token. The
agent cannot do anything through MCP that the token could not do through the
REST API. Give an agent a **read-only** token unless it needs to make changes.

### Adding a tool

Each tool is one entry in the `tools` table in `internal/mcp/tools.go`. An
entry names a REST route and its parameters. Path placeholders such as
`{server_id}` are filled from arguments, and every other argument becomes a
query parameter. No authorization code is needed, because the REST handler
applies the token's permissions. `go test ./internal/mcp` checks that every
entry is documented and that its path parameters are required arguments.
