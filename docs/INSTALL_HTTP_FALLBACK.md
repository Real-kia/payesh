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
