# Advanced Process Monitoring

Install **Advanced Process Monitoring** in Packages, then enable it. Select
**View processes**, or open the server's **Processes** tab.

The optional Linux executable is downloaded separately from the core. Supported
architectures are amd64 and arm64. GitHub, local path, and URL sources use the
same signed manifest and archive verification.

## Measurements

Snapshots update every three seconds. Search matches process name or PID; sort
by CPU, resident memory, disk read/write rates, socket count, or PID. The table
shows UID, process state, and thread count. CPU is measured relative to one
core: a multithreaded process can exceed 100%. Disk rates use actual Linux
`read_bytes` and `write_bytes`, rather than syscall byte counts. Memory is RSS (an approximate resident-memory measurement).
Network data counts socket file descriptors, including Unix sockets; it does
not claim per-process upload/download byte attribution. Aggregate network rates
remain built into the server dashboard.

CPU and I/O rates need two samples of the same process identity (PID plus start
time). Missing, denied, reset, or newly created counters are null, displayed as
`—`. Disappearing processes are removed. Sample failures and samples older than
15 seconds produce an unavailable response. Process names come from `comm`;
command lines and environment variables are not collected.

## Scope and lifecycle

The current package framework supports the configured local standalone/hub
server. Remote-node package execution is not implemented. The API rejects any
other target instead of returning the hub's processes for a node.

The server supervises this read-only module under its existing service account.
No additional capabilities or root service are requested. [Linux `/proc`](https://docs.kernel.org/filesystems/proc.html) access
restrictions can hide processes or prevent per-process I/O/FD inspection; those
counters remain unavailable. Full visibility depends on the host's proc policy.
Only enabled packages can serve process data, and all browser access requires a
Payesh session. The Unix socket is owner-only. Enabled packages resume after a
server restart, and unexpected exits restart after five seconds. Disable stops
the executable; remove is allowed only after disabling. Nothing kills or changes
monitored processes.

Scans are bounded to 32,768 directory entries, 65,536 file descriptors in total,
and 4,096 descriptors per process. The dashboard displays the first 200 sorted
matches; the API accepts a limit of at most 1,000. Partial FD scans show no count.

## Release signing

Package 0.1.0 is built and signed offline with an external Ed25519 key using
`scripts/process-package`. The core pins its public verification key. The
private key is never supplied to ordinary GitHub Actions or committed. Later
core releases copy the exact signed 0.1.0 package assets from v0.2.11, preserving
the signature/checksum pair. A new package version requires offline signing and
a reviewed catalog/key update. This pinned, immutable package does not use the
30-day manifest age limit applied to the older operator-provisioned modules;
future timestamps, version, platform, core compatibility, and signatures remain
validated.
