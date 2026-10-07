# Payesh

Payesh is an open-source Linux VPS monitoring prototype for one or multiple
servers. The current source includes a Go agent, SQLite-backed server/API,
CLI, installer preflight, update/recovery primitives, and separately built
Port Traffic, CPU Controls, and Bandwidth Controls modules.

## Install

On a Linux server (x86_64 or arm64, systemd or OpenRC):

```sh
curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh | sudo sh -s -- --release-mode preview
```

This explicitly installs an unsigned preview. The installer downloads the latest release, checks its byte integrity, starts Payesh, and
prints your login. Open **http://YOUR_SERVER_IP:8787**. That connection is not
encrypted until you add a domain:

```sh
sudo payesh domain panel.example.com
```

Payesh then gets a free Let's Encrypt certificate and serves HTTPS on the same
port, without using port 443 or touching nginx. See the
[quickstart](docs/QUICKSTART.md) for details and installer options.

Monitoring nodes use a separate TLS port, **9797** by default. Allow it through
the hub firewall before adding nodes. Dashboard and node ports can be changed
independently in Settings → SSL / TLS; capable agents migrate automatically
when the node port changes.

Current releases are previews, not v1. Release archives are checksummed but
intentionally unsigned until an authorized owner performs the external
Ed25519 signing step.

Production installation defaults to refusing unsigned releases and requires an
independently provisioned Ed25519 public anchor and key ID. The release owner
must publish signed checksum metadata including the bootstrap script; see
[production release trust](docs/contracts/RELEASE_FORMAT.md#production-bootstrap-and-updates).

## Documentation

- [Quickstart](docs/QUICKSTART.md) — one-line install, dashboard access, and building from source
- [Update and recovery](docs/UPDATE_AND_RECOVERY.md)
- [Uninstall and detachment](docs/UNINSTALL.md)
- [Known limitations](docs/KNOWN_LIMITATIONS.md)
- [Release checklist](docs/RELEASE_CHECKLIST.md)
- [Provider billing comparison](docs/BILLING_COMPARISON.md)
- [Traffic usage by date range](docs/TRAFFIC_USAGE.md)
- [Storage, sampling, and notifications](docs/STORAGE_AND_NOTIFICATIONS.md)
- [Contributing and release preparation](docs/CONTRIBUTING.md)
- [Linux support matrix](docs/support/LINUX_MATRIX.md)
- [Release format](docs/contracts/RELEASE_FORMAT.md)

The dashboard and optional kernel controls remain subject to the acceptance
gates and limitations recorded in the implementation status.
