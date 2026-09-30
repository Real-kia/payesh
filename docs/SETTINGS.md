# Settings

Settings has three sections: SSL / TLS, Versions & updates, and User management.
User management is visible to the owner, who can search, add, edit, and remove
users and set their role and read/edit access. Sign out remains in the profile menu.

Versions & updates shows the installed version, latest stable release, and the
20 most recent GitHub releases with dates and links to release notes. Drafts,
prereleases, and tags that are not application versions are excluded. Private
repositories use the hub's GITHUB_TOKEN; an unavailable release history does not
hide the installed version.

SSL / TLS manages the domain, certificate, and dashboard port. Changing a port
reserves its listening socket before persisting the change; occupied ports and
invalid values are rejected without changing the current address. The browser
opens the new address after the change. Existing endpoints remain available to
previously enrolled nodes, and the saved endpoints are restored on restart.
New node enrollments use the selected port. Port history is capped at 16 addresses.
The configuration is stored in ports.json in the TLS state directory, separately
from domain and certificate state, so certificate removal and renewal preserve it.

Mutations retain session, CSRF, and read/edit permission checks. The hub does not
change firewall rules when selecting a port.
