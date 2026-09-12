# Payesh

Payesh is an open-source Linux VPS monitoring prototype for one or multiple
servers. The current source includes a Go agent, SQLite-backed server/API,
CLI, installer preflight, update/recovery primitives, and separately built
Port Traffic, CPU Controls, and Bandwidth Controls modules.

This repository is not a published v1 release. Local release archives and
checksums are reproducible, but they are intentionally unsigned until an
authorized owner performs the external Ed25519 signing step.

- [Quickstart](docs/QUICKSTART.md) — build, preflight, and run a local preview
- [Update and recovery](docs/UPDATE_AND_RECOVERY.md)
- [Uninstall and detachment](docs/UNINSTALL.md)
- [Known limitations](docs/KNOWN_LIMITATIONS.md)
- [Contributing and release preparation](docs/CONTRIBUTING.md)
- [Linux support matrix](docs/support/LINUX_MATRIX.md)
- [Release format](docs/contracts/RELEASE_FORMAT.md)

The dashboard and optional kernel controls remain subject to the acceptance
gates and limitations recorded in the implementation status and package
handoffs.
