# Known limitations

This source tree is an implementation checkpoint, not an accepted v1 release.

- Release bundles are reproducible and checksum-validated locally. The
  repository provides an offline owner-invoked signer and explicit-anchor
  verifier, but no production signing key, trust-anchor registry, official
  release URL, or published artifact is stored or promised here.
- The command-line installer can preflight and apply verified artifacts, and
  durable SSH-install job binding is implemented; live SSH installation on a
  disposable target remains acceptance work.
- Update verification, staging, activation rollback, SQLite backup, the
  independent watchdog, filtered/encrypted history transfer, durable hub-first
  scheduling, and a local signed-release executor exist as composable pieces.
  The executor requires caller-owned backup/health adapters and rejects remote
  targets; authenticated remote execution, production schema-migration
  registry entries, and live role-cutover hook wiring are not complete. The
  generic version/backup gates and durable one-controller cutover state machine
  are implemented and unit-tested, but not yet accepted on a multi-host
  deployment.
- Browser read and owner mutation/onboarding API wiring exists. Real two-host
  TLS enrollment and monitoring ingestion passed between Ubuntu 22 amd64 and
  Debian 12 arm64. Public browser TLS issuance, managed renewal, HTTPS webhook
  delivery, and Telegram delivery passed on Ubuntu 22. Certificate renewal for
  internal node identities, remote action delivery, SSH-driven installation,
  and signed clean-host rollout remain incomplete. Keep the server on loopback
  or use the managed HTTPS helper; its no-domain mode provides an encrypted SSH
  tunnel instead of exposing credentials over plain HTTP.
- Optional Port Traffic, CPU Controls, and Bandwidth Controls require Linux
  kernel facilities, privileges, and ownership-safe setup. Unsupported or
  foreign nftables/qdisc/cgroup state is refused. A macOS test or synthetic
  fixture does not prove kernel enforcement.
- The acceptance tooling now measures bounded isolated-path throughput and
  durable-ledger sample overshoot. Collector-driven kernel quota overshoot,
  whole-process resource overhead, and 24-hour retention remain unmeasured.
  `scripts/resource-benchmark.sh` now provides bounded, process-tree-scoped
  JSON/TSV CPU, RSS, and (on Linux) block-I/O evidence, but representative
  30-minute and 24-hour workloads still need to be run. The disposable Ubuntu
  22 network profile passes
  veth/namespace, bridge, routed nft masquerade, TUN-device, and Podman
  runtime checks; a disposable Podman bridge container also passed outbound
  connectivity. This does not prove Docker-specific, overlay, or
  provider-specific tunnel behavior. Offload controls remain unsupported on
  the provider's disposable veth, and basic Ubuntu 22 CPU enforcement and
  selected reboot/service checks are covered by the disposable runner.
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
- `91.108.151.131.realkia.com` received a valid Let's Encrypt certificate with
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
