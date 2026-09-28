# Payesh

Payesh is an open-source Linux VPS monitoring prototype for one or multiple
servers. The current source includes a Go agent, SQLite-backed server/API,
CLI, installer preflight, update/recovery primitives, and separately built
Port Traffic, CPU Controls, and Bandwidth Controls modules.

## Install

On a Linux server (x86_64 or arm64, systemd or OpenRC):

```sh
curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh | sudo sh
```

The installer downloads the latest release, verifies it, starts Payesh, and
prints your login. Open **http://YOUR_SERVER_IP:8787**. That connection is not
encrypted until you add a domain:

```sh
sudo payesh domain panel.example.com
```

Payesh then gets a free Let's Encrypt certificate and serves HTTPS on the same
port, without using port 443 or touching nginx. See the
[quickstart](docs/QUICKSTART.md) for details, installer options, and
(temporarily) installing while the repository is private.

Current releases are previews, not v1. Release archives are checksummed but
intentionally unsigned until an authorized owner performs the external
Ed25519 signing step.

## Documentation

- [Quickstart](docs/QUICKSTART.md) — one-line install, dashboard access, and building from source
- [Acceptance status](docs/ACCEPTANCE_STATUS.md) — current verified checks and remaining gates
- [Update and recovery](docs/UPDATE_AND_RECOVERY.md)
- [Uninstall and detachment](docs/UNINSTALL.md)
- [Known limitations](docs/KNOWN_LIMITATIONS.md)
- [Provider billing comparison](docs/BILLING_COMPARISON.md)
- [Contributing and release preparation](docs/CONTRIBUTING.md)
- [Linux support matrix](docs/support/LINUX_MATRIX.md)
- [Release format](docs/contracts/RELEASE_FORMAT.md)

The dashboard and optional kernel controls remain subject to the acceptance
gates and limitations recorded in the implementation status and package
handoffs.
