# Payesh quickstart

## Install in one line

On your Linux server (x86_64 or arm64), run:

```sh
curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh | sudo sh
```

That's it. The installer:

1. checks that the server is supported,
2. downloads the latest Payesh release from GitHub and verifies every file
   against the release `SHA256SUMS`,
3. installs the services, creates your login, and starts Payesh.

When it finishes, it prints how to open the dashboard.

> Want to see if your server is supported first, without changing anything?
> Add `-s -- --check` at the end:
> `curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh | sudo sh -s -- --check`

## Open the dashboard

Get your login (generated during install):

```sh
sudo cat /etc/payesh/owner-credentials
```

Payesh listens only on the server itself (`127.0.0.1:8787`) so it's never
exposed by accident. From your own computer, open an SSH tunnel:

```sh
ssh -N -L 8787:127.0.0.1:8787 root@YOUR_SERVER_IP
```

Then browse to **http://127.0.0.1:8787** and log in.

## Access from anywhere (optional, needs a domain)

Point a domain at your server, then run:

```sh
curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/scripts/configure-public-access.sh \
  | sudo sh -s -- --domain payesh.example.com --apply
```

It installs Caddy, gets a free HTTPS certificate, and renews it
automatically. It never touches anything already using ports 80 or 443. Run
it without `--apply` to preview what it would do.

## Installer options

Pass options after `sh -s --`:

```sh
curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh | sudo sh -s -- --role node
```

| Option | What it does |
| --- | --- |
| `--role standalone` | Dashboard + monitoring on this server (default) |
| `--role node` | Monitoring agent only, reporting to another Payesh |
| `--role hub` | Dashboard for a fleet of nodes |
| `--role cli-only` | Only the `payesh` command-line tool |
| `--version 0.1.0` | Install a specific release instead of the latest |
| `--listen 127.0.0.1:9000` | Use a different dashboard address |
| `--check` | Only check the server; install nothing |

## Uninstall

```sh
sudo payesh-install --role standalone --uninstall
```

Your data in `/var/lib/payesh` is kept. Add `--remove-data` to delete it too.
See [UNINSTALL.md](UNINSTALL.md) for details.

---

## Temporary: installing while the repository is private

> **Remove this section when the repository goes public.**

While `Real-kia/payesh` is private, `raw.githubusercontent.com` and release
downloads return 404 without credentials. The installer can download through
the GitHub API instead when `GITHUB_TOKEN` is set.

1. Create a [fine-grained personal access token](https://github.com/settings/personal-access-tokens/new)
   limited to the `Real-kia/payesh` repository with **Contents: Read-only**
   and a short expiry.
2. On the server (curl is required in this mode):

```sh
export GITHUB_TOKEN=github_pat_xxx
curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" \
  -H "Accept: application/vnd.github.raw" \
  https://api.github.com/repos/Real-kia/payesh/contents/install.sh \
  | sudo --preserve-env=GITHUB_TOKEN sh
```

Installer options work the same way, e.g. `... | sudo --preserve-env=GITHUB_TOKEN sh -s -- --check`.
To test an unmerged branch's installer, add `?ref=BRANCH` to the
`contents/install.sh` URL.

Afterwards, run `unset GITHUB_TOKEN` and revoke the token once testing is
done. It's only used to download; the installed system never stores it.

### Publishing a release for the installer

The installer downloads from GitHub Releases. Pushing a version tag builds and
publishes one automatically (`.github/workflows/release.yml`):

```sh
git tag v0.1.0 && git push origin v0.1.0
```

These release bundles are unsigned (`signing_key_id` is
`unavailable-local`). The installer verifies them against the release's
`SHA256SUMS` over HTTPS, which catches corrupted or incomplete downloads
but doesn't independently prove who published them. Signed releases are
tracked in [contracts/RELEASE_FORMAT.md](contracts/RELEASE_FORMAT.md).

---

## For developers: build and run from source

Payesh targets Linux amd64 and arm64.

```sh
make build
sudo ./dist/payesh-install --role standalone   # preflight only, changes nothing
```

Run a local monitoring preview. In one terminal:

```sh
./dist/payesh-server -db ./payesh.db -listen 127.0.0.1:8787
```

In another, register the local server and feed samples to the same database:

```sh
./dist/payesh register-server -ensure -db ./payesh.db -identity-file ./server-id
./dist/payesh-agent -interval 15s -identity-file ./server-id \
  | ./dist/payesh ingest -db ./payesh.db -identity-file ./server-id -follow
```

The health endpoint is `http://127.0.0.1:8787/healthz`. Don't expose this
development listener publicly. Stop with Ctrl-C.

### Build a local release bundle

```sh
(cd web && npm ci --ignore-scripts --no-audit --no-fund)
make web-check
SOURCE_DATE_EPOCH=1768089600 make release-package
make release-validate RELEASE_VERSION=0.1.0
```

This writes every archive plus `manifest.json` and `SHA256SUMS` under
`dist/releases/0.1.0/`. An authorized release owner can sign it with
`make release-sign`; see [CONTRIBUTING.md](CONTRIBUTING.md).

### Notification acceptance

Webhook and Telegram credentials stay in the environment only:

```sh
PAYESH_ALERT_WEBHOOK_URL=https://hooks.example.com/payesh \
PAYESH_ALERT_WEBHOOK_SECRET='replace-me' \
go run ./scripts/notification-acceptance
```

For Telegram, set `PAYESH_ALERT_TELEGRAM_TOKEN` and
`PAYESH_ALERT_TELEGRAM_CHAT_ID`. Set `PAYESH_NOTIFICATION_RETRY=1` to exercise
the bounded retry policy; `PAYESH_WEBHOOK_FAIL_FIRST=2` simulates an outage
(at most four failures). Rotate any test token shared through chat or logs.

### Installation acceptance

```sh
scripts/install-acceptance.sh
```

This runs first install, upgrade, failed-verification recovery, and uninstall
against a private filesystem fixture without touching the real host. For a
live disposable host, `TestLiveSSHInstall` is opt-in via
`PAYESH_LIVE_SSH_INSTALL=1` plus target, user, key, a confirmed host-key
fingerprint, and a directory of verified artifacts (`PAYESH_SSH_BIND_ADDRESS`
selects the source address on multi-interface machines).
