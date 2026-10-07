# Known limitations

An earlier isolated standalone systemd check exposed a fresh-database startup
race between agent registration and server initialization. Source fixes now
recheck initialization under SQLite's writer lock and retry bounded WAL setup
contention. Repeated concurrent-startup tests preserve committed samples and
incremental vacuum, and verify WAL cancellation. The rebuilt baseline subsequently
collected samples under real isolated systemd on both Linux architectures, with
verified binaries, service identities and clean teardown. A further review found
that interruption between fresh schema creation and vacuum setup can leave
incremental cleanup disabled on reopen. Fresh initialization now commits a
temporary recovery marker with the schema and removes it only after verifying
incremental vacuum. Actual SQLite cancellation, restart and concurrent-finalizer
regressions pass; native interruption verification for this further fix remains
required. Signed update and recovery lifecycle acceptance remain open.

This source tree is an implementation checkpoint, not an accepted v1 release.

- Release bundles are reproducible and checksum-validated locally. The
  repository provides an offline owner-invoked core signer and explicit-anchor
  verifier. The tag workflow publishes checksum-based preview archives;
  production installation/update now authenticates a signed checksum envelope
  and bootstrap script against an external Ed25519 public anchor and key ID.
  Missing trust inputs or invalid/missing signatures fail closed before
  downloaded release code executes. Unsigned preview installation/update
  requires explicit preview mode. Test-only signed fixtures are verified;
  actual owner anchor distribution, signed publication and accepted production
  clean-host updates remain open. Initial `curl | sh` script trust must also be
  established independently; self-verification inside an untrusted script
  cannot authenticate that first entrypoint.
  Advanced Process Monitoring separately uses a pinned signed package.
- The command-line installer can preflight and apply verified artifacts, and
  durable SSH-install job binding is implemented. A pinned-host-key live SSH
  node install passed on disposable Ubuntu, including remote SHA-256
  re-verification, systemd activation, preservation-first uninstall, explicit
  data removal, and idempotent repeated uninstall. The transport supports an
  optional source bind address and prefers atomic rsync transfer with guarded
  SCP fallback for slow or broken provider SFTP implementations.
- Update verification, staging, activation rollback, SQLite backup, the
  independent watchdog, filtered/encrypted history transfer, durable hub-first
  scheduling, and a local signed-release executor exist as composable pieces.
  The local transactional executor requires caller-owned backup/health adapters.
  A separate authenticated fleet executor is now wired at server startup and
  delivers typed, release-pinned intent to node root workers. Durable requests
  and correlated results survive restarts; success requires fresh installed
  version/service health and a matching authenticated node hello. Fleet updates
  require production trust and refuse preview mode. Root workers now integrate
  installed-layout rollback of managed binaries, configuration, services,
  installation state and consistent SQLite snapshots, with durable interruption
  recovery. Results and role/init metadata are root-protected independently of
  the service-writable inbox/state; existing hosts missing protected scope fail
  closed until it is provisioned through the verified installer. Snapshot
  preflight and bounded copies enforce a 2 GiB snapshot ceiling, an 8 GiB
  retained archive budget and database/restore headroom before services stop.
  Terminal archives require operator cleanup; exhausted budgets refuse updates,
  and active or unresolved snapshots are never automatically removed.
  Rollback retains the failed generation's database/WAL/SHM, default spool and
  node replay identity files before restoring the earlier snapshot. If capture
  fails or retained hashes are invalid, restoration stops. Candidate-interval
  samples require operator reconciliation from that archive; they are not
  automatically merged into the restored database.
  Once restoration finishes, a durable restart phase prevents recovery retries
  from overwriting samples accepted by partially restarted services. Restored
  files and directory changes are synced before that boundary is recorded.
  Parent cancellation prevents queued node activation immediately, but cannot
  undo activation already started by a node worker. These source safeguards are
  regression-tested. Native disposable OpenRC workers on both architectures
  passed interruption at activating/restarting boundaries and real low-space
  refusal before services stopped, using disposable signing trust. Owner-signed
  multi-host rollout, other interruption phases, systemd recovery, activation-time
  disk exhaustion and physical storage faults remain open. Store schema
  migrations now use transactional registry steps and verified backups; signed
  candidate preflight and full storage acceptance are being completed. Live
  distributed role-cutover adapters are incomplete. The durable one-controller
  coordinator and a store-level orchestrator with crash-resumable phases are
  implemented and tested against local stores, against an authenticated mutual-TLS
  peer over loopback, with the real `payesh cutover` commands in a sandbox with
  no outside network, and once between two Linux hosts (arm64 source, amd64
  destination) over the public internet, moving a 3,000-sample test server with
  matching frontier digests and a relinquished source. That run used a throwaway
  server, not production data, and was followed by a run where the network was cut
  mid-cutover (it stopped before the freeze, left the source active, and resumed to
  completion) and by four runs where the source process was killed with SIGKILL at
  different moments and resumed after its five-minute lease (all completed with
  matching sample counts; the source ended relinquished and the destination active).
  Those crash runs used a loopback peer on one host. Other phases and power loss
  have not been exercised. Restricting the peer port by a systemd `IPAddressAllow` list was tried
  and did not stop an unlisted host from completing a TCP connection, so restrict the port with
  a host or network firewall rule instead; the peer itself relies on mutual TLS, a
  pinned certificate and a single grant. Transfers stay under the 64 MiB artifact limit, and `serve` is a
  foreground process by default. Optional systemd and OpenRC service definitions
  for it ship under `deploy/` (never enabled by default, bounded to 24 hours, with
  restart on failure); both were started, killed (and restarted automatically), restarted and stopped on
  real Linux (OpenRC on Alpine, systemd 252 on arm64 with paths moved to a
  throwaway directory) and kept the same certificate across restarts.
- Local hub/standalone-to-node conversion has destination preflight, protected
  snapshot size/headroom checks and durable rollback/recovery of managed files
  and prior active services. Database, logs and unrelated data are preserved. A failed or
  incomplete recovery blocks new conversion until resolved. These installer
  fixtures do not establish distributed role-cutover or live systemd/OpenRC
  conversion acceptance.
- Browser read and owner mutation/onboarding API wiring exists. Real two-host
  TLS enrollment and monitoring ingestion passed between Ubuntu 22 amd64 and
  Debian 12 arm64. Public browser TLS issuance, managed renewal, HTTPS webhook
  delivery, and Telegram delivery passed on Ubuntu 22. A live TLS WebSocket
  acceptance also passed internal identity renewal, atomic persistence,
  predecessor revocation, reconnect, and exactly-once durable no-op action
  delivery. A production-signed clean-host rollout remains incomplete. Installer deployments initially expose HTTP on port 8787; enable managed
  HTTPS before sending credentials across an untrusted network, or bind to
  loopback and use an encrypted SSH tunnel.
- Remote-node package installation/execution is not configured; Advanced
  Process Monitoring is scoped to the local standalone/hub server. Signed source-based installation on the local hub is now
  supported for Process Monitoring; other hub packages and remote targets remain
  unavailable.
- Store startup now uses the schema registry: invalid/newer metadata is refused
  before mutation, and schema versions commit with their data changes.
  Historical timestamp rewrites require verified recovery snapshots. Actual
  SQLite regression checks pass. Authenticated candidate schema preflight now
  precedes activation; the signed index binds manifest, installer and archive
  bytes. Recovery snapshots count toward storage pressure, and migration copying
  enforces a destination page limit. Fresh installed-host migration and signed
  interrupted-update acceptance remain open.
- The legacy SSH installer now checks trusted executable pins before downloaded
  or uploaded code runs. Directory-bearing hub/standalone roles use verified
  upload with the web tree pin passed to the installer. Regression checks pass;
  fresh live acceptance of the rebuilt installer/server remains open.
- The SSH node bootstrap observes its remote script for up to 90 seconds and
  returns an error if the script has not finished. The script was started with
  `nohup` and can still complete after that error, and the bootstrap has no
  host-wide lock of its own. After such a timeout, check the node's install log
  and wait for the script to finish before retrying so two installers do not
  run at once. A slow GitHub fetch, firewalled hub port or large artifact set
  are the usual causes.
- Schema 7 per-server ingest authority is store-level infrastructure. It fences
  ingest, job, module, control-policy and configuration writes and records
  cutover-bound transitions. Job state transitions and cancellations are
  deliberately left unfenced so a stale controller can still stop work, and the
  certificate and enrollment checks outside the store are point-in-time reads
  rather than part of the store transaction. Rollout scheduling is not fenced. A destination store verifies
  the transferred state before activating authority, but the authenticated peer
  transport that carries the handoff is not built, and no production command
  drives a cutover. The 6-to-7 upgrade and rollback passed native systemd
  and OpenRC fixtures with test trust for the final schema-7 build; endurance and
  production-trust runs have not been repeated for schema 7.
- Separate dashboard/node ports and verified migration are implemented and
  covered by local TLS tests and disposable Linux amd64/arm64 transport fixtures.
  Current matched two-host Linux migration and installation
  acceptance, including firewalls, offline nodes, older agents, and restarts,
  remains required. Endpoint and hub state are synced before migration
  acknowledgement or retirement. Persistent storage faults can also prevent
  durable restoration of prior state; physical power-loss acceptance remains
  open. Port changes do not configure firewalls or forwarding.
- Optional Port Traffic, CPU Controls, and Bandwidth Controls require Linux
  kernel facilities, privileges, and ownership-safe setup. Unsupported or
  foreign nftables/qdisc/cgroup state is refused. A macOS test or synthetic
  fixture does not prove kernel enforcement.
- The acceptance tooling now measures bounded isolated-path throughput and
  durable-ledger sample overshoot. Collector-driven kernel quota overshoot,
  representative deployed-fleet resource overhead with optional modules enabled,
  and completed 24-hour retention remain unaccepted.
  A precision-safe provider comparison command now exists, but acceptance still
  requires an authoritative provider total covering the exact same period,
  interfaces, direction, reset boundary, and byte units.
  `scripts/resource-benchmark.sh` now provides bounded, process-tree-scoped
  JSON/TSV CPU, RSS, and (on Linux) block-I/O evidence. A 30-minute baseline
  on the currently installed binaries completed 121 observations; a
  synthetic refreshed arm64 candidate baseline also completed 122 observations
  over 30 minutes with zero sampler errors and clean child shutdown. It inserted
  121 samples at the persisted 15-second sampling policy, with optional modules
  disabled and a 50% CPU quota/384 MiB memory limit. Representative fleet load
  and enabled-module overhead remain open; the separate bounded 24-hour store
  soak completed without ingest or prune errors and does not establish actual
  fleet endurance. The native isolated fleet runs completed more than 24 hours
  on both architectures, but failed the coverage-gap assertion despite zero
  query errors. They do not close the endurance gate; diagnosis and a passing
  rerun remain required. The disposable Ubuntu
  22 network profile passes
  veth/namespace, bridge, routed nft masquerade, TUN-device, and Podman
  runtime checks; a disposable Podman bridge container also passed outbound
  connectivity. This does not prove Docker-specific, overlay, or
  provider-specific tunnel behavior. Offload controls remain unsupported on
  the provider's disposable veth, and basic Ubuntu 22 CPU enforcement and
  selected reboot/service checks are covered by the disposable runner. Current
  isolated amd64 and arm64 runs also pass CPU quota, traffic shaping, bounded
  throughput, durable quota-overshoot, and exact UDP packet/byte accounting;
  optional-package lifecycle and provider integration remain unaccepted.
- Logs are bounded views of existing sources, not a centralized forensic
  archive. Coverage gaps, stale data, unsupported capabilities, and failed
  notifications must remain visible to operators.
- The uninstall subcommand now exists and is exact-path/data-preserving by
  default, but live service-stop and clean-host uninstall acceptance are still
  environment-dependent. There is no automatic OS upgrade mechanism,
  arbitrary remote shell, per-process bandwidth control, RAM control,
  container-specific integration, or MCP/AI service.

## Evidence completed on 2026-09-12 and 2026-09-13

- Full Go race suite, vet, OpenAPI contract validation, web type/build checks,
  shell syntax, and Linux amd64/arm64 build matrix passed.
- Disposable install/upgrade/failure-recovery/uninstall acceptance passed.
- Ubuntu 22 core acceptance passed collection, identity/epoch continuity,
  SQLite ingest/query, and authenticated populated-database API startup.
- Ubuntu 22 guarded nftables apply/snapshot/remove passed with cleanup.
- A real Debian 12 arm64 node consumed a single-use enrollment job over TLS and
  delivered sequence-zero monitoring data to the Ubuntu hub; both temporary
  deployments were removed afterward.
- A bounded 10-second retention smoke inserted 101/101 samples with no ingest,
  prune, or storage errors and clean teardown. It does not replace the 24-hour
  soak gate.
- A higher-volume Ubuntu soak ran for 121 seconds, accepted 4,280/4,280
  samples with no duplicate, ingest, prune, or storage errors, observed
  retention, deleted all eligible samples, and cleaned its temporary state.
  This remains shorter than the required 24-hour production endurance gate.
- A disposable test domain received a valid Let's Encrypt certificate with
  the correct DNS SAN. External checks passed HTTP-to-HTTPS redirect, hostname
  verification, TLS 1.3, loopback reverse proxying, and repeat configuration.
  The installed Caddy service owns automatic renewal. A provider NAT mismatch
  and absent distro Caddy package were handled by warning and the official
  upstream package repository respectively.
- A synthetic alert passed through the production notifier to an external
  HTTPS one-shot webhook with a verified HMAC signature, and a separate alert
  reached the supplied Telegram test chat. Same-host public-address webhook
  delivery was unavailable on this provider due to hairpin routing; the
  external delivery path passed.
- Public HTTPS outage/recovery acceptance returned two deliberate HTTP 503
  responses followed by success. Payesh verified the route, preserved valid
  HMAC signatures on every attempt, applied bounded retry backoff, and accepted
  the third delivery. The test listener shuts down gracefully so its final
  response cannot race connection teardown.
- The disposable Ubuntu transport gate passed two-agent bootstrap/ingestion,
  forced in-band node-certificate renewal, replacement identity persistence,
  old-certificate revocation, reconnect, and one side-effect-free typed action
  committed exactly once. Test-key signed HTTP release verification,
  unhealthy-activation rollback/retry, and watchdog recovery passed on the
  same Linux host.
- Ubuntu Linux incident-context acceptance passed a real file-log source and
  persisted both bounded metric evidence and timestamped log evidence in the
  firing incident snapshot.

- The 24-hour endurance runs on the schema-6 (amd64) and schema-7 (amd64 and arm64)
  candidates each failed at their final retention check with 0 query errors. The
  cause is a test-fixture bug, not a product bug: the fixture configured the
  billing allowance after the first few samples had arrived, so those samples were
  never billed and have no traffic-ledger rows. Traffic tombstones therefore begin at
  the first billed sequence, while the retention gap begins at the stream start, and
  the fixture's check demanded coverage from the gap start. A regression test
  reproduces exactly this shape. The corrected fixture needs a fresh 24-hour run;
  no endurance run has yet passed end to end. The corrected fixture was run on both
  architectures and deliberately stopped by the operator at about 16-17 hours
  (about 76,000 queries on arm64 and 85,000 on amd64, 0 query errors, memory steady,
  host services and containers unchanged, cleanup passed). Because retention pruning
  only starts after 24 hours, these partial runs do not exercise the retention
  check; that is covered only by the unit-level regression test.
