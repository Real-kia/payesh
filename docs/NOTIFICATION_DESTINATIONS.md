# Alert notification destinations

Payesh can send alert notifications (firing, recovered, and reminder) to any
number of destinations. Manage them under **Settings → Notifications → Alert
destinations**, or through the REST API and MCP tools described below. Only the
owner can create, change, test, or delete destinations; every signed-in account
can see the list.

Destinations are provider-neutral: a **webhook** with a choice of payload
format (a native JSON payload, a Slack/Discord/ntfy preset, a generic text body,
or your own template) or a **Telegram** bot.

Alerts still appear on the Alerts page whether or not a destination exists.

## Destinations

| Option | Meaning |
| --- | --- |
| Name | Label shown in the list. |
| URL | The receiving endpoint. **HTTPS is required.** Stored encrypted and never returned by the API; the list shows only the scheme and host. |
| Signing secret | Optional HMAC key. When set, each request carries `X-Payesh-Signature` (below). Write-only. |
| Custom headers | Up to 10 static headers, for example `Authorization: Bearer YOUR_TOKEN`. Values are write-only. |
| Format | `payesh`, `slack`, `discord`, `ntfy`, `text`, or `template`. |
| Timeout | 1–30 seconds per attempt (default 10). |
| Attempts | 1–5 (default 3), with exponential backoff starting at the configured delay (default 1 s, capped at 30 s). |
| Max per minute | 1–120 (default 30). See [Flood protection](#flood-protection). |
| Routing | Which notifications this destination receives. See [Routing](#routing). |
| Enabled | Disabled destinations are kept but receive nothing. |

Telegram destinations use the bot token as their secret, a chat ID, and the API
base URL (default `https://api.telegram.org`). They always send a text message
and take no custom headers.

At most 20 owner-managed destinations can exist.

### Managed by environment

Destinations configured with `PAYESH_ALERT_WEBHOOK_URL` /
`PAYESH_ALERT_WEBHOOK_SECRET` and `PAYESH_ALERT_TELEGRAM_TOKEN` /
`PAYESH_ALERT_TELEGRAM_CHAT_ID` (and optionally `PAYESH_ALERT_TELEGRAM_API_URL`)
keep working exactly as before. They are listed as **Managed by environment**,
can be tested, but cannot be edited, disabled, or deleted from the dashboard;
change the environment and restart the service instead. Because the operator
supplies them, they may point at private-network receivers. They receive all
alerts and use the default timeout, attempts, and rate limit.

## Payload formats

All formats are sent as `POST`. `Content-Type` is `application/json` except for
`ntfy` and plain-text templates (`text/plain; charset=utf-8`). Every request also
carries `X-Payesh-Event` (`firing`, `recovered`, `reminder`, `test`, or
`digest`) and `X-Payesh-Delivery` (a unique event ID, stable across retries), so
a receiver can de-duplicate.

### `payesh` — native JSON, schema `payesh.alert.v1`

```json
{
  "schema": "payesh.alert.v1",
  "kind": "firing",
  "alert_id": "alert-1",
  "state": "firing",
  "occurred_at": "2026-01-02T03:05:05Z",
  "reason": "sustained_condition",
  "event_id": "evt-1",
  "rule": { "id": "rule-cpu", "name": "High CPU", "expression": "cpu.utilization > 90", "threshold": 90 },
  "server": { "id": "srv-1", "name": "web-1", "tags": ["web", "prod"] },
  "value": 95.2,
  "since": "2026-01-02T03:04:05Z",
  "incident": { "id": "inc-1", "url": "https://payesh.example.com/alerts?incident=inc-1" }
}
```

Version 1 only ever **adds** fields; existing fields keep their meaning, and
`alert_id`, `state`, `occurred_at`, and `reason` are the original four. Treat
unknown fields as ignorable. `rule`, `server`, `value`, `since`, and `incident`
are omitted when unknown. `incident.url` is present only when the
`PAYESH_PUBLIC_URL` environment variable is set.

`kind` is the notification type; `state` is the alert state (`firing` or
`recovered`). A *digest* (see below) has `"kind": "digest"`, `count`, `dropped`,
and an `items` array of the grouped payloads.

### Signature

With a signing secret, `X-Payesh-Signature` is the lowercase hex
HMAC-SHA256 of the **exact request body bytes**, keyed with the secret. Verify
it in constant time against the raw body before parsing.

### Presets

| Format | Body |
| --- | --- |
| `slack` | `{"text": "[FIRING] High CPU on web-1 (cpu.utilization > 90), value 95.2\nhttps://…"}` — for a Slack incoming webhook. |
| `discord` | `{"content": "…", "username": "Payesh"}` — for a Discord webhook (truncated to 2000 characters). |
| `ntfy` | The text as the body, with `Title`, `Priority` (4 for firing, 3 for recovered), and `Tags` headers. Use your ntfy topic URL. |
| `text` | `{"text": "…"}` — a minimal body for generic receivers. |

### Custom templates

Choose `template` and write the body as a Go
[`text/template`](https://pkg.go.dev/text/template). Use **Preview with a
sample alert** to check the output before saving. The result is sent as JSON if
it is valid JSON, otherwise as plain text.

Available fields (all strings unless noted):

`.Title`, `.Summary` (one line), `.Kind`, `.State`, `.Reason`, `.Rule`,
`.RuleID`, `.Expression`, `.Metric`, `.Operator`, `.Threshold`, `.Value`,
`.Server`, `.ServerID`, `.Tags` (list), `.Since`, `.OccurredAt`, `.AlertID`,
`.EventID`, `.IncidentID`, `.IncidentURL`, `.Digest` (bool), `.Count` (number),
`.Dropped` (number), and `.Items` (list of the same fields, for digests).

Functions: `json` (encode a value as a JSON literal — use it for every string
in a JSON template), `lower`, `upper`, `join`, `truncate N text`, plus the
standard template control actions and comparison functions.

```text
{"text": {{json .Summary}}, "server": {{json .Server}}, "tags": {{json .Tags}}}
```

Templates are sandboxed: they can read only the fields above, cannot define or
call other templates, cannot range over a number, are limited to 4096 bytes, and
their output is limited to 16 KiB. A template that does not parse or render is
rejected when you save it.

## Routing

By default a destination receives every notification. Add filters to narrow it;
a notification must satisfy **all** filters that are set, and each filter list
is "any of":

- **Notification type**: `firing`, `recovered`, `reminder`.
- **Server tags**: the server has any listed tag (case-insensitive), for
  example only `flow`.
- **Rules**: the alert rule's ID or name matches.
- **Servers**: the server ID matches.

Alert rules do not have a severity level today, so routing by severity is not
offered; route by rule or tag instead.

## Safety

- **Secrets are never returned or logged.** The URL (which often contains a
  token), signing secret, and header values are stored encrypted with
  AES-256-GCM. The key is generated on first start in
  `notification.key` next to the database (mode `0600`). The API returns only
  the scheme and host, whether a secret exists, and header *names*. Delivery
  errors are short classifications such as `timeout` or `HTTP 500`; they never
  contain the URL or credentials. If the key file is lost or replaced, the
  stored secrets cannot be read and the destination shows a warning; edit it and
  enter the URL and secrets again. **Back up `notification.key` with the
  database** if you want restored destinations to keep working.
- **SSRF protection.** A destination cannot reach loopback, private (RFC 1918 and
  ULA), link-local, carrier-grade NAT, or unspecified addresses unless
  *Allow private-network addresses* is ticked for that destination. The check
  runs at connect time, on the resolved address, so a DNS name that later points
  at an internal address is refused. Cloud instance-metadata addresses
  (for example `169.254.169.254`) are **always** refused. Redirects are never
  followed, and no proxy is used.
- **Plain HTTP** needs two explicit opt-ins — *Allow private-network addresses*
  and *Allow plain http://* — and then only connects to private addresses, never
  to a public one. Use it for a self-hosted receiver on your own network.
- **Header limits.** `Host`, `Content-Length`, `Content-Type`,
  `Transfer-Encoding`, `X-Payesh-*`, and similar framing headers cannot be set.

## Flood protection

Each destination has a *max per minute* budget (default 30, with a burst of up
to 10). Notifications over budget are held for a few seconds and then sent as
**one grouped digest message** listing up to 20 of them (and a count of the
rest), so one outage on a large fleet cannot flood a chat channel. At most 200
notifications are held per destination; beyond that the overflow is retried by
the alert engine on its normal reminder schedule. Test sends are not
rate-limited by this budget but are limited to one every few seconds.

## Delivery status and testing

Each destination shows its last delivery: when, whether it succeeded, the HTTP
status, and a short error. **Send test** makes one real attempt (no retries)
with a sample notification and records the result as the last delivery. Retries
apply to network errors, `408`, `429`, and `5xx` responses; other `4xx`
responses are final.

## REST API

All routes are under `/api/v1/alerts/destinations`; see `api/openapi.yaml` for
the full schemas.

| Method and path | Purpose |
| --- | --- |
| `GET /alerts/destinations` | List destinations, including environment-managed ones. |
| `POST /alerts/destinations` | Create (owner). |
| `GET /alerts/destinations/{id}` | One destination. |
| `PATCH /alerts/destinations/{id}` | Partial update, enable or disable (owner). Omitted fields, including the URL and secrets, keep their value; a header sent as `null` keeps its stored value. |
| `DELETE /alerts/destinations/{id}` | Delete (owner). |
| `POST /alerts/destinations/{id}/test` | Send a test notification (owner). |
| `POST /alerts/destinations/preview` | Render a format or template for a sample alert. |

Example (placeholders only):

```sh
curl -X POST https://payesh.example.com/api/v1/alerts/destinations \
  -H "Authorization: Bearer YOUR_API_TOKEN" -H "Content-Type: application/json" \
  -d '{
    "name": "Ops chat",
    "kind": "webhook",
    "url": "https://hooks.example.com/services/YOUR_WEBHOOK_PATH",
    "format": "slack",
    "filter": { "kinds": ["firing", "recovered"], "tags": ["prod"] }
  }'
```

MCP clients can call `list_notification_destinations` and
`test_notification_destination`; creating or editing destinations goes through
`api_request` with an owner's edit token.

## Example receivers

All values below are placeholders.

### Verify the signature (Python)

```python
import hashlib, hmac, json
from http.server import BaseHTTPRequestHandler, HTTPServer

SECRET = b"YOUR_SIGNING_SECRET"

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        expected = hmac.new(SECRET, body, hashlib.sha256).hexdigest()
        if not hmac.compare_digest(expected, self.headers.get("X-Payesh-Signature", "")):
            self.send_response(401); self.end_headers(); return
        alert = json.loads(body)
        print(alert["kind"], alert.get("rule", {}).get("name"), alert.get("server", {}).get("name"))
        self.send_response(204); self.end_headers()

HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
```

Put this behind your own TLS (HTTPS is required), or for a receiver on a
private network tick *Allow private-network addresses* and *Allow plain
http://*.

### Slack

Create an incoming webhook in your workspace and use format `slack` with the
webhook URL `https://hooks.slack.com/services/YOUR_WEBHOOK_PATH`.

### Discord

In the channel settings create a webhook and use format `discord` with the URL
`https://discord.com/api/webhooks/YOUR_WEBHOOK_ID/YOUR_WEBHOOK_TOKEN`.

### ntfy

Use format `ntfy` with your topic URL, for example
`https://ntfy.example.com/YOUR_TOPIC`. For a protected topic add the custom
header `Authorization: Bearer YOUR_NTFY_TOKEN`.

### Generic text-only receiver

Use format `text`; the body is `{"text": "[FIRING] High CPU on web-1 …"}`.

### Custom template for a ticketing system

```text
{
  "title": {{json .Title}},
  "severity": {{if eq .Kind "recovered"}}"info"{{else}}"high"{{end}},
  "host": {{json .Server}},
  "labels": {{json .Tags}},
  "details": {{json .Summary}},
  "link": {{json .IncidentURL}}
}
```
