# API and AI agent (MCP) access

Everything the dashboard does is available through the REST API at `/api/v1`
and to AI agents through the [Model Context Protocol](https://modelcontextprotocol.io)
at `/api/v1/mcp`. Both use **API tokens**.

> **HTTPS only.** Tokens and MCP are refused over plain HTTP (`403 https_required`).
> Set up a domain under **Settings → SSL / TLS** first. Behind a reverse proxy,
> add the proxy to `--trusted-proxy-cidrs` and have it send `X-Forwarded-Proto: https`.

## 1. Create a token

**Settings → API & AI access → Create API key.** Give it a name, an expiry
(up to 1 year), and an access level per category: **None**, **Read only**, or
**Read and write**. Copy the `pyt_…` secret; it is shown once.

The page then shows the setup command for your agent, already filled in with
your hub's address and the new token. For existing keys, the **Connect an AI
agent** section builds the same command; paste the key to fill it in.

## 2. Connect your agent

Replace `payesh.example.com` with your hub's domain and `YOUR_TOKEN` with the key.

**Claude Code**

```sh
claude mcp add --transport http payesh https://payesh.example.com/api/v1/mcp --header "Authorization: Bearer YOUR_TOKEN"
```

**Codex**

```sh
mkdir -p ~/.codex && printf '\n[mcp_servers.payesh]\nurl = "https://payesh.example.com/api/v1/mcp"\nhttp_headers = { Authorization = "Bearer YOUR_TOKEN" }\n' >> ~/.codex/config.toml
```

To keep the token out of the config file, use
`codex mcp add payesh --url https://payesh.example.com/api/v1/mcp --bearer-token-env-var PAYESH_TOKEN`
and export `PAYESH_TOKEN` in your shell instead.

**Gemini CLI**

```sh
gemini mcp add --transport http payesh https://payesh.example.com/api/v1/mcp --header "Authorization: Bearer YOUR_TOKEN"
```

**VS Code**

```sh
code --add-mcp '{"name":"payesh","type":"http","url":"https://payesh.example.com/api/v1/mcp","headers":{"Authorization":"Bearer YOUR_TOKEN"}}'
```

**Claude Desktop, Cursor, and other JSON-configured clients**

```json
{
  "mcpServers": {
    "payesh": {
      "type": "http",
      "url": "https://payesh.example.com/api/v1/mcp",
      "headers": { "Authorization": "Bearer YOUR_TOKEN" }
    }
  }
}
```

The endpoint uses the Streamable HTTP transport, keeps no session state, and
accepts only API tokens (not the browser session).

### Tools

| Tool | Returns |
| --- | --- |
| `whoami` | The account and permission the token acts as |
| `list_servers`, `get_server` | Servers and their connection state |
| `get_metrics`, `list_processes` | CPU, memory, disk, load, network; running processes |
| `get_traffic_periods`, `get_traffic_forecast` | Traffic usage and end-of-period estimate |
| `list_log_sources`, `query_logs` | Log sources and log search |
| `list_active_alerts`, `get_alert_history`, `get_incident`, `list_alert_rules`, `list_maintenance_windows` | Alerting state and history |
| `list_packages`, `list_server_packages` | Package catalog and installations |
| `get_job`, `get_update_status` | Job progress and update state |
| `api_request` | Any other REST call (the only tool that can make changes) |

The OpenAPI document is exposed as the `payesh://openapi.yaml` resource. Every
tool call runs as a normal REST request with the token, so an agent can never
do more than the token allows. **Give agents read-only tokens unless they need
to make changes.**

## REST API

```sh
curl -H "Authorization: Bearer YOUR_TOKEN" https://payesh.example.com/api/v1/servers
```

- Full contract: [`api/openapi.yaml`](../api/openapi.yaml); notes in the [API contract](contracts/API_CONTRACT.md).
- Lists return up to 200 items; pass `next_cursor` back as `cursor`.
- Time ranges are RFC3339, at most 31 days.
- Errors: `{"code": "...", "message": "...", "retryable": false}`.
- Changes to versioned resources need `expected_revision` and an `idempotency_key`.

## Token rules

- A token acts as its account, capped by the account's current permission.
  Read-only accounts can create read tokens only.
- Categories: monitoring, logs, traffic, alerts, packages, operations,
  settings, updates. New tokens start at **None** everywhere.
- Tokens cannot manage tokens or accounts, so a leaked token cannot renew itself.
- Up to 20 active tokens per account. Resetting an account's password or
  deleting it revokes its tokens.
- Rotating issues a new secret and invalidates the old one immediately.
  Extending the expiry revives an expired token (expired tokens stay listed for 30 days).
- Only a token's own account can edit or rotate it. The owner can view
  activity for and revoke any account's tokens.
- **Activity** shows the last 100 requests per token (time, method, path, IP,
  status), never bodies, query strings, or secrets.

Token management is also available over the API from a signed-in HTTPS
session: `GET/POST /api/v1/api-tokens`, `PATCH/DELETE /api/v1/api-tokens/{id}`,
`POST /api/v1/api-tokens/{id}/rotate`, and `GET /api/v1/api-tokens/{id}/activity`.
See [`api/openapi.yaml`](../api/openapi.yaml) for fields.

## Adding an MCP tool

Add an entry to the `tools` table in `internal/mcp/tools.go` naming a REST
route and its parameters. `{path}` placeholders are filled from arguments; the
rest become query parameters. The REST handler enforces permissions, so no
auth code is needed. `go test ./internal/mcp` checks the entry is documented.
