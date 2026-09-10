# Package 01 review resolution

Updated: 2026-09-09

The first foundation review identified eight contract gaps. Their resolutions
are recorded here for the next reviewer:

1. Protected API operations now require the `payesh_session` cookie and expose
   `401` responses; setup/login remain unauthenticated.
2. OpenAPI now reserves paths for enrollment/server management, traffic, logs,
   alerts/incidents, modules, policies, releases/updates, settings/backups,
   and durable jobs.
3. All Go 64-bit wire fields use JSON string tags; OpenAPI uses decimal-string
   patterns. Fixtures and tests cover values above JavaScript's safe range.
4. Source-clock-ahead samples are preserved with both timestamps and an
   explicit uncertainty code instead of being rejected.
5. Helper actions now distinguish local `target` from remote
   `target_server_id` in the shared type and documentation.
6. Metric/log queries cap ranges at 31 days, pages at 200 items, and use opaque
   cursors.
7. Job cancellation requires both an idempotency key and expected revision.
8. CI invokes `make lint` in addition to tests, vet, builds, and contract checks.

The implementation remains a contract scaffold. Authentication, cryptographic
verification, persistence, migrations, helper authorization, and feature
behavior still require focused review before implementation.

## Second review pass

- Zero expected revisions are now serialized explicitly as `"0"`; counters
  are parsed as bounded uint64 values and metric `values` must be non-nil.
- OpenAPI now represents rollups, traffic allowances, live logs, source/cursor
  metadata, alert history/maintenance windows, complete module lifecycle,
  policy safety flow, update preflight/selection, backup export/import, role
  transitions, complete release compatibility metadata, and required job
  expiry.
- CI includes explicit oldest/current representative images for Debian, Fedora,
  Rocky, AlmaLinux, and Alpine in addition to Ubuntu architecture builds.
- A minimal Svelte/Vite shell now runs `svelte-check` and `vite build`; the
  OpenAPI check parses YAML and resolves references. Browser mutations require
  CSRF headers, list resources use bounded page/cursor contracts, release fields
  match the signed manifest, and node/helper payloads have shared typed records.

## Third review pass

1. Node input is now `NodeMetricSample`, which deliberately has no
   `received_at`. Only `WithReceivedAt` at hub ingestion creates the persisted
   or API-visible `MetricSample`; it stamps source-clock-ahead uncertainty.
2. The release format specifies RFC 8785 JCS UTF-8 canonicalization, RFC 8032
   Ed25519, and exactly how the detached 64-byte signature is base64url encoded.
3. `ModuleInvocationRequest` now carries and validates
   `protocol: payesh.module.v1`; hello includes a bounded typed installed-module
   ID/version inventory.
4. Both build loops are fail-fast. Go tests cover node receipt stamping, module
   protocol/version validation, hello module inventory, and invalid coverage
   gaps. Batch validation now checks every gap's epoch, order, and reason.
5. The OpenAPI uint64 pattern is tested against its maximum and high-range
   values, and rejects overflow/leading zeroes. API Server records expose
   connection/freshness state and reason; label updates are CSRF-protected,
   revision-checked, and use an idempotency header.
6. Shared `AuditEvent` Go/API/data-model contracts define actor, target, result,
   revision, correlation, and redaction. Login now documents `429` with a typed
   retryable error and a bounded `Retry-After` header.
