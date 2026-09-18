# Payesh acceptance status

Updated: 2026-09-18

This is the canonical current status. Package handoffs are historical evidence
and may describe an earlier point in the work; when they disagree with this
file, this file is authoritative.

## Repository-side verification completed today

The following checks completed in the current workspace:

- `make lint` — passed, including formatting, service-definition checks, shell
  syntax checks, and `go vet ./...`.
- `make contract-check` — passed.
- `make web-check` — passed with zero Svelte diagnostics and a production Vite
  build.
- `make install-acceptance` — passed disposable install, upgrade, failed-
  verification recovery, resume, uninstall, and symlink/traversal checks.
- `make monitoring-soak-ci` — passed 80/80 samples with zero ingest, prune, or
  storage errors; retention was observed and teardown was clean.
- `make build` and `make build-matrix` — passed for the core and optional
  Linux `amd64`/`arm64` artifacts.
- Local unsigned release generation and validation — passed for release `0.1.0`.
- Shell syntax and `git diff --check` — passed.

`go test ./...` and `go test -race ./...` both passed on 2026-09-18 after the
network-enabled development environment became available. This also cleared
the earlier sandbox-only loopback bind failure in
`TestInstallUpgradeAllowsItsExistingListenAddress`.

## Feature inventory and acceptance results

| Feature | Current implementation and evidence |
| --- | --- |
| Core binaries and CLI | Agent, server, CLI, installer, privileged helper, updater watchdog, and three module binaries build for Linux amd64/arm64. |
| Local monitoring | Proc/sys collectors, epochs, SQLite ingest/query, retention, rollups, logs, and authenticated API passed local checks and the disposable Ubuntu core profile. |
| Authentication and browser sessions | Owner setup, login/session/CSRF/throttling, enrollment authority, and durable jobs are implemented and covered by local tests. |
| Fleet transport | TLS WebSocket bootstrap, reconnect, spool replay, renewal, and typed action delivery passed the opt-in two-agent integration test. |
| Dashboard | Production API wiring plus explicit preview/empty/error/retry states build successfully; visual and accessibility acceptance remains. |
| Traffic allowances and alerts | Calendar periods, counter accounting, forecasts, alerts, incidents, maintenance, notifications, and durable replay are implemented and locally tested; provider-scope acceptance remains. |
| Signed module framework | Catalog, Ed25519/JCS trust, archive safety, lifecycle/CAS state, module routes, and `payesh-privd` are implemented and locally tested. |
| Port Traffic | Guarded nftables planning/apply/snapshot/remove passed on the disposable Ubuntu host; broader NAT/bridge/container/tunnel accounting remains. |
| CPU Controls | Real 200m cgroup-v2 enforcement passed in a delegated systemd unit on the disposable Ubuntu host. |
| Bandwidth Controls | Real `tc` apply/verify/revert, rollback/watchdog paths, 4 Mbit/s known-volume throughput, quota overshoot, and module socket passed on the disposable Ubuntu host. |
| Installer/uninstaller | Live pinned-host-key SSH node install passed; data-preserving uninstall and explicit data removal both passed on the disposable host. |
| Updates/recovery/migrations | Local signed-release verification, staging, backup, rollback, watchdog, and scheduling are implemented and tested; remote rollout/role cutover remains. |
| Release operations | Reproducible unsigned bundle generation/validation and checksums pass; production signing/publication remains. |

## Remote evidence completed on 2026-09-18

- Disposable `172.239.107.13`: Ubuntu 22.04.5 amd64, systemd, kernel 5.15,
  cgroup v2, `tc`, `nft`; consolidated `PAYESH_ACCEPTANCE_PROFILE=all` passed
  with throughput and quota probes enabled. Temporary remote state was removed.
- The disposable host's network profile initially lacked Podman; Podman 3.4.4
  was installed specifically for the container-network test. The final network
  and consolidated profiles passed. No Payesh units, binaries, data paths, or
  processes remain after the installer/uninstaller test; Podman remains as an
  intentional disposable-host test dependency.
- Shared `89.58.29.206`: Debian 12 arm64. Only the non-invasive core profile
  ran. It passed collector/identity, SQLite ingest/query, authenticated API,
  and cleanup. Normalized running services, listeners, and Docker containers
  were identical before and after; the existing `payesh-agent.service` remains
  enabled and active.
- The acceptance runner was corrected so systemd `Delegate=yes` CPU tests are
  not incorrectly skipped from an SSH session's non-delegated root cgroup.
  The offload probe was moved before the disposable veth is torn down.
- The disposable node installer was rebooted and recovered over SSH; after
  reboot `systemd=running`, `payesh-agent.service=active`, and
  `payesh-agent.service=enabled`. The node was then uninstalled with both
  preservation-first and explicit data-removal paths, leaving no Payesh paths,
  units, processes, or temporary acceptance directories.

## Product stage

| Area | Current state |
| --- | --- |
| Foundation and contracts | Implemented; review/acceptance still required. |
| Dashboard | Production API wiring and explicit preview mode implemented; browser visual, mutation, and accessibility acceptance remain. |
| Monitoring | Local collector, SQLite history, API, logs, retention, and disposable Linux service/reboot checks passed; long-duration pressure/soak evidence remains. |
| Fleet | Auth, enrollment, durable jobs, TLS transport, and local installer boundaries implemented; external fleet acceptance remains. |
| Traffic and alerts | Checkpoint implemented; provider-scope and external acceptance evidence remains. |
| Optional modules | Port Traffic, CPU Controls, and Bandwidth Controls passed the disposable Ubuntu all-profile matrix; broader production topology and long-duration evidence remains. |
| Updates and recovery | Local signed-release plumbing, staging, backup, rollback, watchdog, and scheduling implemented; remote rollout and role-cutover acceptance remain. |
| Release readiness | Reproducible unsigned bundles and validation implemented; signed clean-host release acceptance remains. |

## Remaining work that requires external capability

These are the only remaining gates that cannot be closed from this repository
and sandbox alone:

1. **Additional Linux/operations acceptance.** The supplied disposable Ubuntu
   host closed the core, kernel, network, module, installer, reboot, and
   cleanup gates. A longer retention/resource soak, disk-pressure and log-
   rotation run, OpenRC coverage, remote fleet rollout/role cutover, and
   broader provider/container/tunnel matrices still need suitable dedicated
   Linux environments. The shared host must remain read-only for these tests.
2. **Production signing authority.** An owner-managed Ed25519 private key,
   key-management procedure, public trust anchor/key ID, and approved artifact
   location are needed. The private key must not be placed in this repository
   or sent through chat. With the public anchor and signed artifacts available,
   the release/module validation and clean-host upgrade tests can be run.
3. **External provider evidence.** A provider's authoritative byte total for
   the exact same server, interfaces, direction, period, reset boundary, and
   units is needed to close the billing comparison gate.
4. **External notification endpoint, if this gate is still required.** An
   authorized test webhook or Telegram bot/chat is needed for a fresh,
   reproducible notification/outage run. Historical documents disagree about
   whether this was already accepted, so it should be either re-run or
   explicitly accepted from recorded evidence.
5. **Browser/accessibility environment.** A browser automation or manual QA
   environment is needed for screenshots, responsive interaction, live mutation
   flows, keyboard navigation, reduced-motion behavior, and screen-reader
   checks.

The repository can now be used for local loopback monitoring and development
preview. It should not yet be called a production-ready v1 release.
