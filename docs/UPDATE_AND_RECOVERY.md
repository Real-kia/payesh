# Updates and recovery

The repository provides a local end-to-end release execution path for an
already-authorized installation. `internal/updater` obtains a manifest and
detached signature from the configured release source, verifies the signed
`payesh.release.v1` metadata, selects an exact OS/architecture artifact,
checks its declared size and SHA-256, stages safe tar.gz members, and
activates a release with a journal. A health-check failure restores the
previous complete release.

The independent watchdog only recovers an interrupted activation journal; it
does not download releases, execute shell commands, or decide whether a
release is trusted.

## Operator procedure for an authorized release

1. Keep a copy of the current database and configuration. Use the existing
   SQLite backup path (`BackupSQLite`) and verify the resulting snapshot before
   changing files.
2. Obtain the manifest and detached signature through the authorized channel.
   The signature must be Ed25519 over the RFC 8785 canonical manifest bytes;
   checksums beside an untrusted bootstrap do not authenticate a release.
3. Confirm the manifest's `release`, minimum core, key status, exact
   `linux/amd64` or `linux/arm64` artifact, size, digest, and safe archive
   layout before staging it in a versioned release directory.
4. Run the caller-supplied health check before committing activation. Keep the
   previous compatible release until health is confirmed.
5. If the process stops after preservation or activation, run the separately
   installed watchdog once (or let its service run):

   ```sh
   sudo /usr/bin/payesh-updater-watchdog --once \
     --journal /var/lib/payesh/update/journal.json
   ```

   A missing journal is normal. A malformed journal is a failure that must be
   investigated; do not delete it to make the warning disappear.

The durable scheduler can order an injected executor hub-first and one node at
a time, record offline/incompatible/failure results, serialize conflicting
jobs, and resume after restart. `HTTPReleaseSource` fetches bounded metadata
from `<base>/<release>/manifest.json` and `.sig`; `ReleaseExecutor` wires that
source to verification, download, staging, pre-activation backup, health
verification, atomic activation, and accepted-release persistence. It is
strictly local: a target ID mismatch is rejected and no remote path or shell
command is accepted. Fleet nodes still require an authenticated transport
executor, and schema-migration orchestration, role cutover, and full fleet
rollout remain unsupported. Do not treat a local unsigned bundle as a
production update.

## Updating from the dashboard

Installs of the `standalone` and `hub` roles include a root `payesh-update`
service (systemd or OpenRC). The owner can choose **Update** under
Settings → Versions & updates. The hub, which runs unprivileged, only writes a
validated release version to `update-request.json` in the data directory; the
worker re-validates it, refuses downgrades, and runs the same release installer
as `sudo payesh update`. Progress is reported through `update-status.json`, and
the page reloads once the new version is running. The worker reads
`GITHUB_TOKEN` from `/etc/payesh/payesh.env` for private repositories.

Servers installed before this feature need one `sudo payesh update` to add the
worker; until then the page shows the command instead of an Update button. After
login, every user sees a dismissible notice when GitHub has a newer release.
