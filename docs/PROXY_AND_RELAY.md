# Installing servers through a proxy or relay node

Some servers cannot be reached by the hub directly — for example, a provider
or national filter blocks inbound SSH from abroad — even though the server
itself can reach the internet, or at least other servers in its data centre.
The **Add a server** form has a *Connection route* with three choices.

| Route | Use when |
|---|---|
| **Direct** | The hub can open the server's SSH port. This is the default. |
| **Proxy** | You run an HTTP CONNECT or SOCKS5 proxy that the hub can reach and that can reach the server. |
| **Relay node** | Another node already connected to this hub can reach the server (usually one in the same data centre). |

## What happens

With a proxy or relay, the hub's `ssh`, `scp`/`rsync` and host-key check all
go through it, and installation files are **uploaded over that SSH
connection**, using the hub's build for the server's CPU architecture. The
server does not need to download anything.

Choosing a **relay node** needs no setup on that node. The hub:

1. Turns on a small authenticated CONNECT relay in that node's agent
   (TCP port `9798`; the hub must be able to reach it).
2. Issues a credential that only opens the target's SSH port and expires after
   two hours, and removes it when the installation ends.
3. Issues a second credential, limited to the hub's node transport address,
   for the new node — kept only if that node actually needs it (see below).

Only password hashes are stored on the hub and sent to the relay. The relay
refuses any destination a credential does not name, so it is not an open
proxy. Deleting a server deletes its relay credential; deleting the relay node
stops the relay.

## Installing a separate hub from the dashboard

In **Add a server → Install over SSH**, choose **Install as → Separate hub**,
then choose a direct connection, proxy, or relay node. The originating hub
uploads the installer, server, agent, CLI, privilege helper, and web assets for
the target architecture through the selected SSH route. The target needs no
GitHub access.

This creates an independent dashboard and owner account. Completion checks the
new hub's services instead of waiting for it to enroll as a node. The dashboard
opens on port `8787`; owner credentials are stored on the new server in
`/etc/payesh/owner-credentials`. The temporary deployment record is removed from
the originating fleet after success. Existing connected nodes cannot be
replaced by a separate hub installation through this form.

## Bootstrapping a hub from a terminal

The shell installer can use an HTTP(S) or SOCKS proxy:

```sh
sudo sh install.sh --role hub --proxy-url socks5h://127.0.0.1:1080
```

It can also open a temporary SOCKS tunnel through an SSH server:

```sh
sudo sh install.sh --role hub --relay-ssh operator@relay.example.test --relay-ssh-port 22
```

Use the SSH identity or agent available to the account running the installer.
The relay must permit TCP forwarding and reach GitHub. Existing SSH host-key
verification remains enabled. The tunnel listens only on loopback, carries
installation downloads with remote DNS resolution, and closes when the
installer exits, including after a failed download. Normal release verification
options still apply; the transport does not change trust policy.

If the target cannot download `install.sh` itself, copy it from a trusted
machine or retrieve it through the SSH relay first:

```sh
ssh operator@relay.example.test 'curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh' > install.sh
```

`--proxy-url` and `--relay-ssh` are mutually exclusive. A terminal SSH tunnel uses
that server's SSH forwarding, rather than a managed Payesh CONNECT credential.
The tunnel is temporary; it does not configure the new hub's future outbound
network route.

## Node download size and slow connections

Node bundles contain the agent, CLI/update worker, privilege helper, and
one-time installer. They exclude the dashboard server and web assets. Release
binaries omit debug symbols to reduce transfer and installed size. The exact
size depends on release and architecture. Hub bundle downloads have a fifteen
minute transfer window so a working slow link is not cut off after two minutes;
checksums are still verified before extraction and execution.

## Joining with one command

When you would rather not give the hub SSH access, **Add a server → Run one
command on the server** has a *Download from* choice:

- **GitHub** — the classic installer from the latest release.
- **This hub** — the server downloads the hub's own build from the hub. Use it
  when GitHub is blocked but the hub is reachable.
- **Relay node** — for servers that cannot reach the hub (or GitHub) directly.
  The hub prepares the relay, and the command fetches the script and files
  through it: `curl -fsSL -x 'http://node-…@RELAY:9798' https://HUB/api/v1/join-scripts/ID | sudo sh`.

The script checks every checksum, installs the node, and writes the hub's
address, certificate and (if needed) the relay into the node's configuration;
the agent then enrolls itself with the single-use token. With *Directly when
possible* the script tests a direct connection first. The script expires with
the enrollment token.

## Reusing a relay

A node becomes a relay the first time it is chosen and stays one. Relays that
are already set up are listed first as *Ready relays* (with how many servers
use them), and the last relay you picked is preselected, so later servers only
need that one choice. `GET /api/v1/relays` lists them.

## Cleanup

Everything created for joins and relays is removed automatically:

- Join scripts expire with their enrollment token; download bundles after
  30 minutes.
- Installation credentials expire after two hours.
- A node's relay access is removed when the node has not connected through
  the relay for the number of days set in **Settings → Storage → Remove
  unused relay access after** (default 30, 0 keeps it). The hub refreshes
  "last used" while the node is connected through the relay.

## How the new node reaches the hub

*After installation, the node reaches the hub* controls the node's own
connection:

- **Directly when possible** (default): during installation the node first
  tries the hub directly. If that works it stays direct and does not depend on
  the proxy or relay; otherwise it is configured to use it.
- **Always through the proxy/relay**: for servers with no international access.
- **Always directly**.

A proxied node keeps its proxy in `/etc/payesh/payesh.env` as
`PAYESH_TRANSPORT_PROXY` (`http://` or `socks5://`, credentials allowed). The
agent's TLS connection to the hub runs end to end through the tunnel, so the
proxy cannot read or alter it.

## API

`POST /api/v1/installations` (and `POST /api/v1/servers/{id}/install`) accept
`proxy_url`, `relay_server_id` and `node_route` (`auto`, `proxy`, `direct`).
`proxy_url` and `relay_server_id` are mutually exclusive. Like SSH passwords,
`proxy_url` is held in memory for the job only; the job's idempotency check
uses just the proxy's address.

## Requirements and limits

- Proxied installation uploads binaries for the target's architecture from the
  hub's matrix directory (`/usr/share/payesh/matrix`). A hub without a build
  for that architecture fails with a clear error instead of uploading the
  wrong one.
- A relay node needs an agent version with the `relay` capability.
- If a proxied node's hub transport port is later changed, update the relay
  credential by reinstalling through the relay (a node credential allows only
  the transport address it was issued for).
