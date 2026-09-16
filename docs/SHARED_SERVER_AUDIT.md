# Shared-server audit procedure

`scripts/shared-server-audit.sh` is a read-only inventory for a host that
already carries unrelated workloads. It reads OS/init, kernel, listening
sockets, interfaces/routes, process/resource snapshots, cgroup mode, installed
tooling, and `/proc/net/dev` counters. It never installs packages, binds a
socket, changes routes/qdiscs/firewalls/cgroups, starts or stops services,
kills processes, reboots, or generates traffic.

The optional `-unused-port` mode scans the requested range using `ss` and
prints a currently unbound port. This is only a race-prone recommendation; it
does not reserve the port. The helper fails closed when `ss` is unavailable so
it cannot accidentally choose an occupied port from an incomplete inventory.
It is not used by acceptance listeners: those bind `127.0.0.1:0` and consume
the kernel-selected address after the bind, providing bind-before-publish
reservation semantics.

Example:

```sh
scripts/shared-server-audit.sh -sample 5
scripts/shared-server-audit.sh -start 18000 -end 18999 -unused-port
```

## 2026-09-12 shared-host evidence

A read-only audit was run against the supplied shared host. The result was
recorded as capability evidence only; no Payesh artifact was installed and no
host state was changed.

- Debian 12 (Bookworm), arm64, Linux 6.1, systemd PID 1.
- Unified cgroup v2 is mounted and Docker is present with multiple existing
  containers/bridge interfaces.
- Existing services include SSH, HTTP/HTTPS reverse proxying, database and
  proxy workloads, application runtimes, and container-published ports.
- The host had active established TCP sessions and non-zero interface counters
  during the passive snapshot.
- `ss`, `ip`, `nft`, `tc`, `systemctl`, `journalctl`, Docker, and common
  inspection tools were available; `ethtool` and Podman were absent.
  Availability does not imply permission to mutate state.

Because this is a live shared host, it is not evidence for Payesh firewall,
qdisc, cgroup enforcement, service installation, reboot, disk-pressure,
container-network mutation, or generated-throughput acceptance. Those tests
remain assigned to the disposable Ubuntu host or dedicated VMs. The audit is
useful for future planning: port selection must avoid the observed listeners,
and any real deployment must use an operator-selected port range plus an
explicit bind-time error path.
