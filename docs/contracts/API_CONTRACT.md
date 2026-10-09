# REST API contract v1

`api/openapi.yaml` is the canonical API surface under `/api/v1`. Setup and
login are the only unauthenticated operations. Every server, metric, traffic,
log, alert/incident, module, policy, release/update, settings/backup,
enrollment, and job operation requires either the secure `payesh_session`
cookie or an API token, and returns the shared `APIError` shape on failure.

API tokens (`Authorization: Bearer pyt_...`) are created from a signed-in
session through `/api-tokens`, act as their account capped at the token's
`read` or `edit` permission, expire after at most 365 days, and are stored only
as SHA-256 digests. Tokens, `/api-tokens`, and `/mcp` are served only over
HTTPS (TLS terminated by the server, or a trusted proxy reporting
`proto=https`); plain HTTP gets `403 https_required`. Token requests are exempt
from CSRF. Tokens cannot manage
tokens or accounts. Password resets and account deletion revoke the account's
tokens. `POST /mcp` serves the API to Model Context Protocol clients and
accepts only API tokens; each tool call is dispatched as an ordinary REST
request with the caller's token. See [API and MCP access](../API_AND_MCP.md).

Standalone and hub installers generate a unique owner username and password,
store the one-time operator-readable record under `/etc/payesh`, and initialize
the persisted browser account on first service start. Login requests must send
both credentials; legacy `admin` records remain readable for upgrades.

Browser mutations require both the secure session cookie and the
`X-CSRF-Token` header. API clients must not infer CSRF exemption from a JSON
content type; the server validates the token and origin policy.

The owner can manage up to 100 delegated `admin` or `member` accounts through
`/accounts`. Each has `read` or `edit` permission. Read-only sessions may query
the API and revoke their own session using CSRF-protected `DELETE /session`;
they cannot change product data. Only the owner may list, create, change, or
delete accounts. Password resets revoke that account's sessions. `GET /account/me`
returns the current role and permission without credential material.

The packaged server's default browser mode binds to the loopback HTTP listener
and uses a non-Secure cookie so a local browser can complete setup. A
non-loopback deployment must set `--secure-browser-cookies` and place the
listener behind HTTPS; public browser sessions must remain Secure. When a
reverse proxy fronts the listener, configure its exact IP/CIDR through
`--trusted-proxy-cidrs` (or `PAYESH_TRUSTED_PROXY_CIDRS`). Forwarded client
addresses are used for login throttling only from those trusted peers;
unconfigured or untrusted peers are keyed by their direct address.

Mutating operations either create a durable `Job` or require a bounded
`idempotency_key` (header or request field) plus an expected revision where
state can conflict. Metric and log queries cap pages at 200 items, accept an
opaque cursor, and limit the requested time range to 31 days. Raw history must
use pagination; callers should select minute/hour rollups for longer periods.
All collection endpoints use a page object capped at 200 items with an
optional `next_cursor`; the root contract caps requests and responses at 1 MiB.

Raw metric coverage gaps use a separate `gaps_cursor`/`gaps_next_cursor`, so
gap pagination does not repeat or depend on the raw sample page.

Metric responses carry either raw samples or minute/hour `Rollup` records with
sample count, observed duration, extrema, weighted mean, and counter delta.
Traffic periods include timezone, allowance, direction, counted bytes, and
continuity. Logs support bounded history and a capped NDJSON live tail with
source/cursor metadata. Alert history and maintenance windows are first-class
resources. Update preflight reports per-machine eligibility before a selected
rollout, while backup export/import and role-transition jobs retain
recoverable history.

Module install/enable/disable/remove (`POST /servers/{serverId}/modules/{moduleId}/...`)
and CPU control preview/apply/revert (`POST /servers/{serverId}/cpu-policies/{targetKind}/{targetName}/...`)
are synchronous, bounded `idempotency_key` + `expected_revision` operations
that return the resulting `ModuleInstallation`/`Policy` record directly, the
same convention as traffic configuration below — not the generic async
`Job`-wrapped, `policyId`-addressed contract this document originally
sketched.

Package-05 traffic configuration is `POST /servers/{serverId}/traffic` with a
bounded `idempotency_key` and `expected_revision`. It accepts selected interface
names, inbound/outbound/combined direction, a byte allowance, reset day, billing
timezone, warning percentages, and an optional `start_new_period` flag. The
response reports the durable period and preview: allowance-size changes apply
to the active period, while reset-day/timezone changes wait for that period's
existing boundary unless a new period is explicitly requested. Traffic totals
remain host-side estimates; `gap` and `uncertain` continuity are visible rather
than treated as provider-authoritative usage. An explicit period restart closes
the active predecessor atomically, so durable period bounds do not overlap.

`GET /servers/{serverId}/traffic/forecast` accepts a required `scope` and
direction plus an optional RFC3339 `as_of` instant. It returns a bounded,
explicitly labelled recent-rate estimate only when at least 24 hours of usable
counter coverage is available; otherwise it returns `available: false` with a
reason. Explicit future `as_of` values are rejected. The raw sample read is
finite: the handler checks a 24-hour window first and may fall back to the
seven-day window for sparse collectors; high-cadence reads retain a bounded
newest tail and include a bounded predecessor interval for normal 5s/10s edge
cadence. A target period with `gap` or
`uncertain` continuity is not forecastable, and historical active-period totals
require a contiguous replay from the period boundary.

Alert rules are editable threshold expressions (`PATCH`/`PUT /alerts/rules/{ruleId}`)
evaluated against observation timestamps and coverage. Each rule exposes a
durable `effective_at` policy boundary; queued observations before that instant
cannot seed state or emit notifications after a disable/re-enable or material
policy edit. Durable states are `pending`, `firing`, and `recovered`;
sparse sampling records a precision warning instead of claiming continuous
evidence. Maintenance windows suppress notifications but not collection. Alert
history, grouped incident snapshots, and bounded Telegram or HTTPS-webhook
delivery are separate from control-policy events. Configured delivery advances
its durable cadence only after a successful worker send; admission or terminal
delivery failures remain retryable. Rule creation/reconciliation is bounded at
20,000 total and 4,096 per server, counting disabled rows. An alert never
authorizes throttling or blocking.

This document defines resource names and safety constraints. Browser
authentication and its SQLite persistence are implemented, as is the
SQLite-backed enrollment-authority repository. Durable job reads and
revision-checked/idempotent cancellation are implemented; job producers are
not. Several declared endpoints and production integrations remain package
work—notably enrollment production, WebSocket transport, installers, updates,
backups and role transitions—and
must not be replaced with successful dummy responses.

## Storage settings

Authenticated `GET /api/v1/settings/storage` returns settings, physical live
SQLite file usage as `database_bytes`, retained schema recovery usage as
`recovery_snapshot_bytes`, and `effective_sample_seconds`. The recovery field
is always present and defaults to zero. The configured storage limit and
pressure state account for the sum of both byte counts. Owner-only `PUT`
requires CSRF and the current settings revision. These endpoints and their
`StorageStatus` and `StorageSettings` schemas are specified in OpenAPI.

## Node transport settings

The implementation exposes `/api/v1/settings/node-transport` separately from
dashboard HTTPS settings. `GET` returns current port/URL, previous ports, legacy
dashboard compatibility, per-node migration state, and pending count. `PATCH`
accepts `{ "port": 9797 }`, reserves the listener, and starts verified migration.
`DELETE` retires previous node endpoints only when every enrolled, non-revoked
node has connected on the current port. Reads require a session; mutations also
require CSRF and edit permission. See [Settings](../SETTINGS.md) for migration
and firewall behavior. The OpenAPI document includes this extension and its
response schemas. Migration status returns at most 200 node details per page
with an opaque cursor, and enforces the 1 MiB response limit. The `total`,
`migrated` and `pending` counts include every non-revoked enrolled node, so
retirement remains blocked by pending nodes outside the displayed page.
Passing contract validation does not establish completeness of all API routes.
