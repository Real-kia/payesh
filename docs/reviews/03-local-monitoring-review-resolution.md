# Package 03 review loop

## Pass 1 findings and fixes

- SQLite retention used `DELETE ... LIMIT`, which the selected SQLite build did
  not accept. All bounded deletes now select rowids in a limited subquery.
- Cursor pages encoded the extra look-ahead row, skipping the first item on the
  next request. Server, metric, traffic, log, and rollup cursors now encode the
  last returned item; metric cursors also include collector epoch ties.
- A metric query kept a look-ahead `Rows` open while querying coverage gaps,
  deadlocking the single-connection store. Rows are explicitly closed before
  the second query.
- Log scans could read an arbitrarily large unterminated line before applying
  the byte cap. The bounded reader now stops at the remaining byte budget.
- Continuation lines are redacted and advance their cursor; relative paths,
  symlinks, non-regular files, invalid UTF-8, and JSON-style credential fields
  are handled explicitly.

## Pass 2 completeness checks

- Added disk-I/O collection and overflow handling, explicit CPU iowait/steal
  availability, virtual-interface billing exclusion, durable traffic periods,
  persisted rollups, retention age/byte-budget tests, and a bounded live-tail
  route that reads only registered sources. Added a fixed-argument, bounded
  journald snapshot adapter and bounded metric-field/source pagination limits.
- Added systemd/OpenRC definitions and fixed CI to assert the matrix-declared
  init implementation instead of only checking `/sbin/init`.
- Split the collector into SQLite-free `internal/collector`; the agent dependency
  graph is now verified independently from the server/CLI persistence package.
- Normalized empty page arrays instead of emitting JSON `null`, added bounded
  response serialization, explicit retention coverage gaps, and server/source
  identifier validation.
- Added tests for large decimal counters, reset/gap uncertainty, pagination,
  log bounds/redaction, path/symlink rejection, API auth, live tail, storage
  pruning, and race detection.

## Pass 3 contract and boundary checks

- Minute/hour API and CLI queries now read the bounded persisted rollup table;
  raw pages are not silently reinterpreted as incomplete rollups. The OpenAPI
  rollup schema now requires coverage and agrees with the Go validation bounds.
- Per-interface byte counters now declare `bytes` units while packet/error/drop
  counters remain `count`; disk-I/O aggregate overflow makes the entire metric
  family explicitly unavailable instead of dropping one device silently.
- Log retention measures UTF-8 text bytes (`CAST(text AS BLOB)`), and direct
  ingestion validates the same bounded server identifier used by the durable
  schema.

## Result after pass 3

The local package tests, race tests, vet/lint/build matrix, OpenAPI contract
check, frontend check/build, persisted-rollup API test, and CLI SQLite smoke
path pass. Remaining items in the handoff are environment-dependent acceptance
work or explicitly owned by later packages, not unresolved package-03 code-review
findings.

## Pass 4 restart, rollup, cursor and live-tail checks

- The agent and CLI now generate a fresh random collector epoch (with a
  uniqueness-preserving fallback) whenever no explicit override is supplied.
  Service definitions omit the
  override, so every process start gets a new deduplication namespace while
  deterministic fixture tests can still pass an explicit epoch.
- Ingestion now materializes the affected UTC minute/hour buckets and their
  neighbors into `metric_rollups`. Neighbor recomputation handles late points
  that change a gauge's ending duration or a counter interval at a bucket
  boundary. Rollup intervals are assigned to the bucket containing the ending
  sample, preserving every valid delta exactly once; resets, missing sequences,
  and explicit gaps remain uncertain.
- File-tail cursor identities use stable device/inode values rather than mtime,
  so ordinary appends resume at the stored offset while replacement/truncation
  still resets safely. The server write deadline now exceeds the five-minute
  live-tail bound.
- Billing totals scan all valid interfaces while the per-interface response
  remains capped at sixteen entries, preventing a late physical interface from
  disappearing behind virtual devices.

The new tests cover fresh epochs, persisted cross-bucket rollups, counter
boundary totals, append-safe log cursors, and billing beyond the display cap.

## Pass 5 ingestion, pressure, reset, and tail-integrity checks

- Late-sample materialization now also recomputes the bucket containing the
  later endpoint sample, so an interval is not retained once under its old
  predecessor and again under the late sample.
- Raw sample/gap commits now remain successful acknowledgements even when
  derived rollup writes encounter storage pressure or a transient SQLite
  failure. A durable rebuild queue records affected buckets, and
  rollup reads retry that queue after headroom returns.
- Existing and current traffic-period rows use protected write capacity while
  ordinary historical writes remain subject to the managed storage ceiling.
- Unterminated file lines remain pending without advancing a resumable cursor;
  the completed line is emitted only after its newline arrives. CPU utilization
  now rejects a decrease in any cumulative component, and the explicit
  timestamp uncertainty value `none` remains certain.
- Agent stdout is no longer routed into systemd journald or OpenRC rotating
  logs; routine sample streams stay separate from operational diagnostics.
  Live-tail authorization performs an exact source lookup rather than a
  first-page-only search.

The new regression tests cover late endpoint recomputation, durable ACK and
queued rollup recovery under pressure, protected billing updates, partial
lines, partial CPU resets, explicit `none` uncertainty, and live-tail sources
beyond the first 200.

## Final result after pass 5

All local Package 03 review findings are resolved. Go tests, race tests,
vet/lint, native and Linux cross-builds, OpenAPI checks, and frontend checks
pass. Real Linux init/journald, rotation-under-load, and physical disk-full
acceptance remain environment-dependent as described in the handoff.

## Pass 6 integrity, identity, and boundary checks

- Integer-valued memory and root-disk capacity counters remain exact in raw
  samples but are classified as gauges for minute/hour aggregation, producing
  min/max/time-weighted means instead of counter deltas.
- Agent installations now load or atomically create a restrictive persistent
  server identity. An omitted `-server-id` no longer shares a literal ID across
  nodes or restarts; the service templates point at `/var/lib/payesh/server-id`.
- Server listing decodes a SQL `NULL` heartbeat as an absent heartbeat. Billing
  totals exclude logical interfaces using Linux device metadata plus conservative
  bridge/bond/VLAN/tunnel naming rules, while the per-interface display cap stays
  bounded.
- Retention deletes the exact ordered row IDs used to derive retention gaps.
  Gap rollup recovery queues known affected buckets after boundary lookup
  failures, and raw metric pages expose an independent `gaps_cursor`/
  `gaps_next_cursor` path.
- Metric JSON columns are decoded independently, and log text is redacted again
  at the persistence boundary. CLI receipt timestamps are captured after stdin
  decoding/store setup; missing load and dependent memory fields are explicitly
  marked unavailable.
- File cursors carry a bounded content fingerprint in addition to device/inode
  and offset, allowing same-inode copytruncate/rewrite detection without
  treating ordinary appends as rotation.

The new regression tests cover capacity rollups, persistent IDs, logical billing
interfaces, NULL heartbeats, independent gap pagination, independent JSON
decoding, persistence-boundary redaction, exact retention deletion, and
same-inode log rewrites.

## Final result after pass 6

The latest Package 03 review findings are resolved locally. Go tests, race tests,
vet/lint/build checks, OpenAPI validation, and frontend checks pass. Real Linux
device metadata, init/journald, rotation-under-load, and physical disk-full
acceptance remain environment-dependent as described in the handoff.

## Final result

The pass-4, pass-5, and pass-6 fixes are covered by the repository test, race, vet,
lint, native and cross-build checks. No unresolved local code-review findings
remain; Linux distribution, init-system, journald, rotation-under-load, and
disk-pressure acceptance still require the disposable environments listed in
the handoff.

## Pass 7 service, retention, rollup, and query integration checks

- The installed agent service now registers its persisted identity and pipes
  the newline-delimited agent stream into `payesh ingest --follow`, so routine
  Linux samples reach the local SQLite API instead of being discarded. The
  ingest command enforces the per-sample envelope bound while supporting a
  continuous stream.
- The local server runs bounded retention work at startup and every five
  minutes, with cancellation and a finite prune timeout. This keeps the
  configured age and log budgets active without requiring a manual CLI call.
- Gauge rollups split intervals at adjacent UTC bucket edges and preserve
  uncertainty across sequence/epoch discontinuities. Rollup queries now return
  a retryable pending error instead of silently serving stale derived data.
- Raw coverage includes metrics represented only in the validity map and lowers
  confidence when explicit coverage gaps are present. Historical log queries
  now support bounded severity and text-search filters.
- Collector configuration accepts explicit authoritative billing interfaces,
  while the conservative discovery heuristic remains the default. A bounded
  `capture-log` CLI path registers and persists file snapshots for the local log
  API.

The new regression tests cover adjacent-bucket gauge weighting, validity-only
coverage, authoritative interface selection, and filtered log queries. The
service pipeline was smoke-tested by collecting a sample, ingesting it into a
temporary SQLite database, and querying it back.

## Pass 8 service-state, storage, and bounded-query checks

- Service bootstrap now uses an insert-only `register-server -ensure` path, so
  restarts preserve labels, revisions, capabilities, and freshness state. A
  successful local ingest touches the server heartbeat without changing
  configuration fields, and registration defaults architecture from the
  running binary instead of assuming AMD64.
- The systemd pipeline uses `bash` `pipefail`, and OpenRC enables its shell
  pipe-failure mode, so an agent or ingest failure cannot be hidden by a
  successful final pipeline command.
- Managed storage begins bounded history eviction before the hard ceiling,
  checkpoints WAL, and uses incremental vacuuming without deleting protected
  server or traffic-period state. Derived rollup writes reserve headroom and
  leave newly acknowledged raw samples intact when materialization is paused.
- Raw coverage-gap queries are scoped through persisted temporal boundaries in
  the requested time range. Historical file/journal-log queries refresh the
  registered source on demand, and persisted log pages stop at the one-MiB
  response budget with a cursor.
- The OpenAPI metric contract documents retryable `503` rollup responses and
  the log route documents source-unavailable responses.

Verification after this pass: `go test ./...` passes; race/vet, lint, build,
service, and contract checks remain required before lead acceptance.

## Pass 9 bounded history, freshness, and journal checks

- Retention gap generation now sorts the exact deleted sequences and emits
  separate contiguous runs, preventing an unrelated retained sequence from
  being reported as missing.
- One-sided source/gap observation bounds remain NULL. Range filters treat an
  unknown side as potentially overlapping, avoiding fabricated timing and
  hidden coverage uncertainty.
- Follow-mode ingestion sends a separate 15-second heartbeat cadence. The
  server derives fresh/stale/disconnected states from heartbeat age on API
  reads and the retention worker; metric sampling frequency no longer controls
  liveness.
- Managed storage accounting includes configured operational-log directories,
  in addition to the SQLite database, WAL, SHM, and journal files.
- Historical file queries resume from the source cursor and safely restart on
  rotation. Registered journald units support bounded live tails using
  `--after-cursor`; metric pages are byte-bounded and return continuation
  cursors before reaching the one-MiB response limit.

Regression coverage was added for non-contiguous retention deletion. Full
verification after this pass: `go test ./...`, `go test -race ./...`, `go vet
./...`, `make lint`, `make build`, `make build-matrix`, `make contract-check`,
`make service-check`, and `make web-check` all pass. Linux journald/init,
rotation-under-load, and physical disk-full tests remain environment-dependent.
