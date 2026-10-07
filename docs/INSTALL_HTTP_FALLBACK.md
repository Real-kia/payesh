# Hub-hosted installation files

Dashboard SSH installation tries the matching GitHub release first. If a
binary download fails or its checksum is wrong, the node downloads a verified
bundle from the hub's existing HTTP(S) listener. No extra port is opened.

The bundle URL uses a random, job-scoped token. It expires after 30 minutes
and is revoked when the job finishes or fails. Bundles contain installation
files only; SSH credentials and node identities are never included. Checksums
are delivered through the verified SSH connection and checked before extraction.

Hub and standalone updates cache the core Linux AMD64 and ARM64 binaries in
`/usr/share/payesh/matrix`. A node's architecture is detected over SSH, and ELF
headers are checked before serving its bundle. An unavailable architecture is
reported before execution rather than substituting the hub's own binary.

Remote staging probes `/var/tmp`, `/tmp`, and the SSH user's home directory for
an executable location. This avoids running an installer from a `noexec` mount.

The hub URL normally comes from the authenticated dashboard request. Set
`PAYESH_PUBLIC_URL=https://panel.example.com:8787` in the hub environment when
its externally reachable address differs from the address seen by a reverse
proxy. Update the hub using the normal installation command, then retry the
node installation.

## Node telemetry and completion

With HTTPS enabled, new nodes use `wss://<dashboard-domain>:9797/node/v1` by default. This separate TLS-only listener serves node enrollment, metrics, and commands; it does not serve browser sessions or dashboard APIs. Node identities are validated against the hub enrollment authority. The dashboard retains its own port, normally 8787. Allow both ports through the hub firewall. An explicitly configured node listener and certificate continue to work.

The default node listener shares the automatically renewed domain certificate with dashboard HTTPS. The hub installs its system certificate-authority bundle on the node to validate that certificate, including after renewal. Without HTTPS or a configured TLS node listener, node installation is rejected before remote changes.

Before installing or starting node services, the staged agent checks the hub
transport endpoint from the target node. The check has a ten-second limit and
verifies TCP reachability, TLS trust, and the authenticated WebSocket upgrade.
It runs in both the GitHub download and hub fallback paths, without collecting
metrics or opening a controller session. An unavailable port or failed TLS
check stops installation with a node transport port error; check the hub
listener, firewall, DNS, forwarding rules, and certificate trust before retrying.

Installation completion requires a newly received, stored metric sample within 90 seconds. Enrollment does not fabricate a heartbeat. On success the dashboard clears the installation panel and opens the server metrics view. A single sample is shown as collecting history until a trend can be drawn.

Nodes installed by older versions without a transport URL must be reinstalled from the updated hub to receive transport configuration and an authenticated identity. Updating the hub alone does not reconfigure existing nodes.

## Changing ports

Settings → SSL / TLS has separate dashboard and node transport port controls.
Changing the dashboard port leaves the node transport endpoint unchanged.
Before changing the node port, allow the new TCP port through the hub firewall
and any forwarding rules. The hub binds the new port before publishing it.

Agents supporting `transport-migration` verify the new TLS/WebSocket endpoint
with their existing identity and trust anchor, save it atomically, and
reconnect. Until the new authenticated connection is accepted, they retain
the previous address as a fallback. Failed probes keep the working connection
and report a retryable migration failure to the dashboard.

Previous node ports remain available across hub restarts so offline nodes can
return and receive the new endpoint. The settings page lists migrated, pending,
failed, and update-required nodes. Update older agents to enable automatic
migration. Existing installations temporarily retain node access through old
dashboard ports for compatibility; fresh installations use only the separate
node listener. Once every enrolled, non-revoked node has connected to the
current port, select **Retire previous node endpoints** to close previous node
listeners and disable legacy node access through dashboard ports. Retirement
is rejected while any node is pending.

The agent stores migrated endpoint state alongside its identity in
`node-identity.json.transport.json`. An explicit change to its originally
configured transport URL overrides this saved endpoint. Migration changes
only the port on the same trusted hub host; moving to another hub or changing
TLS trust requires enrollment configuration.
