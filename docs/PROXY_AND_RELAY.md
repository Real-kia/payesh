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
