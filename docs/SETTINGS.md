# Settings

Settings has six sections: General, SSL / TLS, Versions & updates, Storage &
sampling, Notifications, and User management.
User management is visible to the owner, who can search, add, edit, and remove
users and set their role and read/edit access. Sign out is in the top bar.

General holds per-browser preferences: the theme (light, dark, or following the
operating system), an accent colour, reduced animations for slow machines or
remote desktops, compact density for large fleets, and the time zone used for
charts, logs, and alerts. They are stored in the browser, apply before the
dashboard draws its first frame, and do not change the hub.

Versions & updates shows the installed version, latest stable release, and the
50 most recent GitHub releases, shown five per page, with dates and links to release notes. Drafts,
prereleases, and tags that are not application versions are excluded. Private
repositories use the hub's GITHUB_TOKEN; an unavailable release history does not
hide the installed version.

SSL / TLS manages the domain, certificate, dashboard port, and a separate node
transport port. The dashboard defaults to **8787**; TLS node transport defaults
to **9797**. Nodes initiate an outbound connection to the hub. No inbound Payesh
listener is required on the node.

Changing either port reserves its listening socket before persisting the change.
Occupied ports and invalid values are rejected without changing the current
address. Changing the dashboard port opens the new browser address and does not
change the separate node transport endpoint. Dashboard port history is stored
in `ports.json` in the TLS state directory; old dashboard listeners remain
available and are restored on restart.

Changing the node port starts a verified migration. Agents advertising
`transport-migration` test the new TLS/WebSocket endpoint, save it durably, and
reconnect. Failed migrations retain the working endpoint. Offline nodes receive
the proposal when they return; older agents must be upgraded. New installations
use the current node endpoint and test reachability and TLS trust from the node
before applying services.

The node card shows progress and migration errors. Temporary settings-fetch
failures offer a retry and recover automatically while SSL / TLS is open. A hub
without this endpoint shows an upgrade message rather than an empty error. Previous node listeners remain
available until every enrolled, non-revoked node has connected on the current
port and an operator selects **Retire previous node endpoints**. Upgraded hubs
may also retain node access through dashboard listeners for legacy agents;
retirement disables that compatibility access. It does not close old dashboard
listeners. Node listener history and observations are stored in
`node-ports.json`; both port histories are capped at 16 entries.

Enable managed HTTPS or configure a trusted node TLS certificate before adding
nodes. Removing the managed domain also affects nodes using that certificate.
Existing connections may remain open temporarily, but reconnects fail until
managed HTTPS is restored or independent node TLS is configured.
Migration changes the port on the same hostname; moving the hub to another
hostname requires separate configuration. Open the new port in the firewall
and update any forwarding rules before migration. Retire old firewall rules only
after the endpoint retirement succeeds.

Mutations retain session, CSRF, and read/edit permission checks. The hub does not
change firewall rules when selecting a port.

Opening Settings directly loads the TLS settings after authentication. The SSL / TLS
panel reports browser HTTPS separately from the managed certificate state; loading
and failed settings requests are never shown as an unencrypted connection.

The update panel checks releases automatically and offers an owner-only Update
button when the update service is installed. An installed version newer than the
latest public release is labeled **Beta · ahead of latest release**, and is never
automatically downgraded. Older installations need one command-line update to
install the browser update service. Browser updates require production release
trust configured by the operator. Requests expire after 30 minutes and remain
durable through activation; the worker checks installed version and service
health and uses the installation transaction for rollback and crash recovery.

## Storage and notifications

The owner can configure the database limit (default 1 GB), normal sampling
(default 15 seconds), automatic storage-saving sampling (default 60 seconds),
and in-app notifications. See [storage and notifications](STORAGE_AND_NOTIFICATIONS.md)
for cleanup thresholds, node compatibility, and retention behavior.
