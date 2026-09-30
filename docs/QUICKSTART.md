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

When it finishes, it prints the dashboard address and your login.

### Install a monitoring node in one line

Run this on the server that will become a node:

```sh
curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh | sudo sh -s -- --role node
```

This installs the agent. To send data to a hub, enroll the node through the
hub's **Add Server** flow; a node without enrollment collects locally only.

### Check for updates

In **Settings → Updates**, select **Check GitHub** to compare the installed
version with the latest release. From the server shell:

```sh
payesh update check
sudo payesh update
```

`payesh update` keeps the installed role and downloads verified release archives.

### Convert a hub to a node

First move every managed node to another hub and remove its old record and
enrollment from this hub. Enroll this server with the destination hub, then
save its node identity JSON and the destination hub CA certificate locally.
The conversion refuses to run if the old hub still has node records or active
certificates.

```sh
sudo payesh role convert node --transport-url wss://NEW_HUB:8787/node/v1 \
  --node-identity-file /root/new-node-identity.json \
  --hub-ca-file /root/new-hub-ca.pem --check
sudo payesh role convert node --transport-url wss://NEW_HUB:8787/node/v1 \
  --node-identity-file /root/new-node-identity.json \
  --hub-ca-file /root/new-hub-ca.pem
```

The conversion stops the old dashboard service, preserves its database for
recovery, installs the node role, and starts the agent with the destination
hub credentials.

> Want to see if your server is supported first, without changing anything?
> Add `-s -- --check` at the end:
> `curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh | sudo sh -s -- --check`

## Open the dashboard

Browse to **http://YOUR_SERVER_IP:8787** and log in with the username and
password the installer printed. You can show them again with:

```sh
sudo cat /etc/payesh/owner-credentials
```

> ⚠️ **No SSL yet.** Until you add a domain, the dashboard uses plain HTTP, so
> your password and data are not encrypted on the way. The installer and the
> dashboard both warn about this. Adding a domain takes one command (below).

If a firewall is active, allow the port, for example `sudo ufw allow 8787/tcp`.
The installer tells you when it detects `ufw` or `firewalld`.

## Turn on HTTPS (free, automatic)

1. Create a DNS **A record** for a domain or subdomain (for example
   `panel.example.com`) pointing to your server's IP.
2. Run:

```sh
sudo payesh domain panel.example.com
```

Payesh gets a free Let's Encrypt certificate, switches the dashboard to
**https://panel.example.com:8787**, and renews the certificate automatically.
Plain `http://` visits are redirected to HTTPS.

What it does and doesn't touch:

- HTTPS runs on the **same port as the dashboard (8787)**. Port 443 is not used.
- nginx, Apache, Caddy and other web servers are **left alone**.
- Let's Encrypt must check the domain on port 80. If port 80 is free, Payesh
  uses it for a few seconds while the certificate is issued or renewed. If
  something else (such as nginx) already uses port 80, Payesh verifies
  through DNS instead, which needs a Cloudflare API token (next section).

You can also set the domain:

- **during install:** `curl -fsSL …/install.sh | sudo sh -s -- --domain panel.example.com`
- **from the dashboard:** Settings → Domain & HTTPS

Other commands:

```sh
sudo payesh domain                 # show the domain and certificate expiry
sudo payesh domain --remove        # go back to plain HTTP
```

### When port 80 is taken (nginx etc.): Cloudflare DNS

If your domain uses Cloudflare DNS, Payesh can prove ownership with a DNS
record instead of port 80:

1. In Cloudflare, go to **My Profile → API Tokens → Create Token**, use the
   **Edit zone DNS** template, and limit it to your domain's zone.
2. Run:

```sh
sudo PAYESH_CLOUDFLARE_API_TOKEN=your-token payesh domain panel.example.com
```

Or paste the token into the Cloudflare field under Settings → Domain & HTTPS.
The token is stored owner-only in `/var/lib/payesh/tls` so renewals keep
working, and it is never shown again.

## Installer options

Pass options after `sh -s --`:

```sh
curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh | sudo sh -s -- --domain panel.example.com
```

| Option | What it does |
| --- | --- |
| `--domain panel.example.com` | Turn on HTTPS right after installing |
| `--email you@example.com` | Optional Let's Encrypt expiry notices |
| `--role standalone` | Dashboard + monitoring on this server (default) |
| `--role node` | Monitoring agent only, reporting to another Payesh |
| `--role hub` | Dashboard for a fleet of nodes |
| `--role cli-only` | Only the `payesh` command-line tool |
| `--version 0.2.0` | Install a specific release instead of the latest |
| `--listen 0.0.0.0:9000` | Use a different dashboard port (default `0.0.0.0:8787`) |
| `--listen 127.0.0.1:8787` | Keep the dashboard private to the server (use an SSH tunnel) |
| `--check` | Only check the server; install nothing |

## Uninstall

```sh
sudo payesh-install --role standalone --uninstall
# Complete removal, including stored data and the installer itself:
sudo payesh-install --role standalone --uninstall --remove-data --remove-installer
```

The default keeps data in `/var/lib/payesh`. The one-line `install.sh` wrapper
also accepts `--uninstall`; add `--remove-data --remove-installer` for complete
removal. See [UNINSTALL.md](UNINSTALL.md) for details.

---

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
