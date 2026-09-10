# Implementation status

Updated: 2026-09-10

This file records implementation evidence, not planning intent. The repository
started as planning documents only. Package 01 / milestone M0 is **ready for review**
and is not accepted. Package 02 / milestone M1 has a **fixture-only preview**;
it is not a working dashboard or an accepted milestone. Package 03 / milestone
M2 now has a **local-monitoring Checkpoint A**; Linux/distribution acceptance is
still pending. Package 04 remains at Checkpoint A awaiting its acceptance gates.
Package 05 Checkpoint A has completed its internal Critical/Major review and fix
cycles, but still awaits the package acceptance evidence listed below. Package 06
now has a **module-framework Checkpoint A** (catalog, trust verification, safe
archive staging, lifecycle, durable store, HTTP routes); it is not accepted and
has no real `payesh-privd` wiring or provisioned production signing key yet.

## Completed in this session

- Recorded a Go 1.26 module and four separate build targets: `payesh-agent`,
  `payesh-server`, `payesh`, and `payesh-privd`.
- Added a dependency-free shared contracts package with protocol/version,
  sample, error, and durable-job request types.
- Added the initial REST/OpenAPI contract under `/api/v1`.
- Added node, helper, module, release-trust, and data-model contracts.
- Added a Makefile with format/vet/test checks and Linux amd64/arm64 build
  matrix.
- Added typed durable-job/action records, decimal-string 64-bit wire fields,
  validation helpers, and fixtures for large counters, gaps, expired jobs, and
  partial-fleet results.
- Added a proposed ownership ADR and a committed npm lockfile with CI `npm ci`
  verification.
- Incorporated review fixes: protected-route session authentication and 401
  responses, all planned API resource families, bounded query pagination/range,
  idempotent job cancellation, explicit source-clock skew semantics, aligned
  helper targets, and CI lint enforcement.
- Recorded the comment-by-comment resolution in
  `docs/reviews/01-foundation-review-resolution.md`.
- Second review fixes add bounded uint64 validation, required metric values,
  explicit rollups/traffic allowances/live logs/alert history/maintenance,
  complete module and policy safety flows, update preflight and machine
  selection, backup/role-transition contracts, full release metadata, required
  job expiry, and the declared distribution-image CI matrix.
- Recorded the exact CI representatives and test scope in
  `docs/support/LINUX_MATRIX.md`.
- Added a minimal Vite/Svelte shell with `svelte-check`, a real frontend build
  target, and YAML-parsing OpenAPI/reference validation in CI.
- Added CSRF requirements for browser mutations, page/cursor bounds for list
  resources, strict release-field alignment, typed node/helper payloads, and
  explicit request/response byte limits.
- Added the minimal Svelte/Vite shell, `svelte-check`, real `web-check` build
  target, parsed OpenAPI reference checking, and distro smoke execution of the
  Linux agent artifact.
- Completed package 02 Checkpoint A with responsive onboarding, fleet overview,
  and server metrics/traffic/logs previews. Fixtures are loaded only through a
  development/explicit-preview adapter; production builds render an API-needed
  state instead of fabricated data. The preview covers all eight server states,
  capability-gated uPlot range selection, local navigation/theme persistence,
  validated access and optional enrollment steps, bounded log metadata, and
  explicit unavailable states. The second review pass also fixed chart data
  boundaries, retry behavior, contrast, readable time axes, and long-name
  wrapping. Design decisions and the handoff are recorded under
  `design-system/payesh/MASTER.md` and `docs/handoffs/02.md`.
- Third foundation review fixes separate node-observed samples from hub-stamped
  receipt records; specify JCS/Ed25519 detached release signatures; validate
  module protocol/hello inventories and coverage gaps; make build loops
  fail-fast; correct high-range uint64 validation; and add server
  label/freshness, audit-event, and login-throttle API contracts.
- Implemented package-03 Checkpoint A: Linux proc/syscall collectors for CPU,
  memory/swap, load/uptime, disk capacity/inodes/I/O, and network counters;
  hub-stamped SQLite ingestion with deduplication, coverage gaps, rollups,
  traffic periods, persisted rollup queries, cursor queries, retention/storage-
  pressure checks, bounded file/journal logs and live tails, protected local API
  reads, CLI commands, and systemd/OpenRC service definitions. Omitted agent
  epoch flags generate a fresh epoch per process start; ingestion materializes
  affected minute/hour rollups and preserves cross-bucket counter intervals.
  Late endpoint buckets are recomputed; raw ingestion acknowledgements remain
  successful when derived writes are pressured, with a durable rebuild queue
  for recovery. Current billing periods remain writable under history pressure,
  all CPU counter components are checked for resets, partial log lines retain
  their cursor, and routine agent samples are excluded from service logs.
  The collector is SQLite-free so the agent build graph remains independent.
  Capacity counters are classified as gauges for rollups, agent server IDs are
  persisted per installation, logical interfaces are excluded from billing,
  retention deletes the exact rows selected for gap creation, and coverage-gap
  pagination is independent from raw sample pagination. Store-level log
  redaction, NULL-heartbeat decoding, post-decode receipt stamping, explicit
  missing-field validity, and same-inode rewrite fingerprints are covered by
  regression tests.
  The installed agent service now feeds a bounded streaming ingest process,
  the server schedules bounded retention/prune work, adjacent-bucket gauge
  weighting is preserved, rollup failures are surfaced as retryable, raw
  coverage includes validity-only metrics and gaps, authoritative billing
  interfaces are configurable, and filtered/persisted log capture is exposed
  through the CLI.
  Service registration is insert-only during bootstrap, ingestion refreshes
  heartbeat/freshness, architecture defaults to the running binary, storage
  pressure evicts eligible history before the ceiling with WAL/incremental
  vacuum recovery, and on-demand file-log queries enforce the one-MiB page
  bound. Coverage gaps are scoped to requested raw sample ranges.
  Retention now emits only contiguous deleted-sequence gaps and preserves
  unknown timing bounds. Freshness derives from heartbeat age, with follow-mode
  ingestion sending an independent 15-second heartbeat. Managed storage counts
  configured operational-log directories; historical file cursors resume at
  source offsets, journald units support bounded live tails, and raw metric
  pages enforce a byte-aware continuation bound.
  The review resolution is recorded in
  `docs/reviews/03-local-monitoring-review-resolution.md` and the handoff in
  `docs/handoffs/03.md`.
- Package-04 Checkpoint A adds process-local browser owner setup/login/logout
  with PBKDF2 password records, transport-aware session cookies and bounded,
  expiring CSRF/login throttling (with trusted reverse-proxy client keys);
  CA-owned single-use enrollment tokens with one certificate per server and
  node certificates with revocation that closes live channels; a
  one-active-connection hub ingestion boundary with compatible hello
  validation, sequence-zero-anchored contiguous durable acknowledgements and
  bounded typed jobs; and side-effect-free role/platform/init preflight checks
  against the pinned release matrix that require systemd or OpenRC for service
  roles. This is not the
  package-04 acceptance gate: persistence, WebSocket framing, offline spool,
  renewal/recovery, SSH/direct installers and Linux acceptance remain
  outstanding. See `docs/handoffs/04.md`.

- Package-05 Checkpoint A adds calendar/timezone traffic allowances with
  durable period identity and restart-safe schedule transitions, selected
  interface/direction counter deltas, 24-hour-first bounded as-of recent-rate
  forecasts with a seven-day fallback,
  compare-and-swap configuration revisions, editable
  timestamp/coverage-based alert rules, hysteresis and sparse-sampling
  precision warnings, maintenance/outage notification suppression with durable
  reminder cadence, grouped bounded incident snapshots, HTTPS webhook/Telegram
  delivery with bounded retries, authenticated browser routes for
  traffic/rules/history/maintenance/incidents, and durable post-process replay
  plus exactly-once traffic charging. Forecasts fail closed when a target
  period's continuity is `gap` or `uncertain`; historical allowance/interface
  snapshots are preserved during delayed processing and queued work pins
  period history until it drains. Derived processing is sequence-frontier
  aware, gap-only batches wake deferred work, and concurrent drains/state
  transitions are serialized. Complete counter intervals are split across
  calendar/DST boundaries and explicit owner-started periods transactionally;
  deferred identities are skipped so ready streams make progress. Alert rules
  carry durable effective-at boundaries; legacy generic traffic starters are
  disabled and terminalized; a bounded retirement authority rejects unseen
  epochs at saturation while preserving exact replay markers. This is not the
  package-05 acceptance gate:
  provider billing comparison, Linux traffic instrumentation, external outage
  observation, real notification endpoints, full snapshot/log context, and
  24-hour resource soaks remain unrun. See `docs/handoffs/05.md`.
  Fresh collector epochs mark sequence-zero usage uncertain when prior epoch
  metadata exists; explicit period restarts close active predecessors
  atomically. Forecast reads retain a bounded newest tail and accept ordinary
  5s/10s cadence edges. Notification cadence advances only after successful
  delivery, and alert-rule creation/reconciliation is bounded at 20,000 total
  and 4,096 per-server rows, including disabled rules.

- Package-06 Checkpoint A adds the signed optional-module framework: an
  RFC-8785-subset JCS canonicalizer and Ed25519 detached-signature verifier
  (`internal/trust`), the curated three-module catalog with per-server
  eligibility checks, safe tar.gz archive staging (checksum/size verified
  before extraction; absolute paths, traversal, and every symlink/hardlink
  entry rejected; atomic activation preserving the previous release),
  an explicit lifecycle state machine that keeps install separate from
  activation and preserves the previous working state on a failed update,
  and a durable per-server `module_installations`/`module_lifecycle_requests`
  store with the same compare-and-swap-revision and idempotency-key
  conventions as the package-05 traffic endpoint. Browser routes expose the
  catalog, per-server status, and install/enable/disable/remove. This is not
  the package-06 acceptance gate: `payesh-privd` still has no real
  `module.invoke` actions, no production release-signing key is provisioned,
  audit-event persistence does not exist for any package yet, and only
  synthetic in-process archive fixtures were exercised (no real Linux host,
  no real official module release artifact). See `docs/handoffs/06.md`.
  This session also fixed two pre-existing build-breaking regressions in
  `internal/monitoring/store.go` (a `*time.Time` dereference bug and a stale
  `RollupPage{}` return from an earlier edit) that left `go build ./...`
  failing before any package-06 work began.

## Verification

Run from the repository root:

```text
GOCACHE=/private/tmp/payesh-go-cache go test ./...
GOCACHE=/private/tmp/payesh-go-cache go test -race ./...
GOCACHE=/private/tmp/payesh-go-cache go vet ./...
GOCACHE=/private/tmp/payesh-go-cache make lint
GOCACHE=/private/tmp/payesh-go-cache make build
GOCACHE=/private/tmp/payesh-go-cache make build-matrix
make contract-check
make web-check
```

Results on the development host: all Go tests passed, `make lint` passed,
the amd64/arm64 Linux matrix built, the OpenAPI contract check passed, and the
frontend pin/lockfile check passed. Race tests and cross-compiled Linux
monitoring test binaries also passed. CI configuration includes
protected-route, lint, service-definition, and distro/init checks. The
default Go cache location is restricted in this sandbox, so `GOCACHE` must be
set to a writable temporary directory here.

These checks cover compilation, monitoring arithmetic/storage behavior, static
contracts, and the frontend shell build. The collector/storage tests use
fixtures and an in-memory SQLite database on macOS; real Linux kernel,
journald, systemd/OpenRC, and disk-pressure acceptance still require the
disposable environments described in `docs/handoffs/03.md`.

The package-02 Checkpoint A Svelte design system and preview views now build,
but real data/authentication/enrollment waits for packages 03 and 04. Browser
visual screenshots and manual assistive-technology checks are still unrun in
this environment. This is not production completion.

## Next authorized work

Review and accept the package-03 handoff in a disposable Linux matrix, then
continue package-04 with persisted auth/enrollment state, WebSocket framing,
bounded offline spool and installer/SSH flows. Connect the M1 previews to
authenticated API records after those seams are stable. Run the committed CI
workflow and focused security/schema review before accepting M0/M2. For
package 06, the next inputs are real `payesh-privd module.invoke` actions for
enable/disable/health-check, a provisioned production release-signing key,
durable audit-event persistence, and Linux acceptance tests against real
signed module release artifacts rather than only synthetic in-process
fixtures — see `docs/handoffs/06.md`. Do not describe this checkpoint as a
complete monitoring product.
