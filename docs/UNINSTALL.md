# Uninstall and detachment

The `payesh-install --uninstall` command removes only the selected role's
Payesh artifacts and service definitions. It stops/disables only the exact
Payesh units for that role, and preserves `/var/lib/payesh`, `/etc/payesh`,
and `/var/log/payesh` by default. Before removing retained history, export or
back up data you need; deleting it is a separate, explicit decision.

Preview the target first, then use an explicit role:

```sh
sudo /usr/bin/payesh-install --role node --uninstall
sudo /usr/bin/payesh-install --role standalone --uninstall --remove-data
```

`--remove-data` is intentionally required to delete the database, identity,
configuration, and logs. The command refuses symlinks and special files and
does not use wildcard paths. The repository acceptance gate exercises both
the data-preserving and explicit-data-removal paths in an isolated fixture:
`scripts/install-acceptance.sh`.

For a systemd host, stop and disable only the Payesh units that were enabled
for the installed role, then remove their exact unit files and reload systemd:

```sh
sudo systemctl disable --now payesh-agent.service payesh-server.service \
  payesh-updater-watchdog.service
sudo systemctl daemon-reload
```

Remove optional module units only when they were installed, and stop their
independent watchdogs first. On OpenRC, use the corresponding
`rc-service payesh-* stop` and `rc-update del payesh-*` operations. Verify the
service list before running any command; do not use a wildcard.

The direct installer owns `/etc/payesh`, `/var/lib/payesh`,
`/var/log/payesh`, `/usr/bin/payesh*`, `/usr/share/payesh/web-assets`, and
Payesh service definitions. Preserve `/var/lib/payesh` if history or recovery
state may be needed. Remove it only after an explicit, verified backup and a
deliberate data-deletion decision. Do not remove package-manager dependencies,
firewall rules, qdiscs, nftables tables, cgroups, or configuration belonging
to another application.

Optional controls require module-specific teardown. If cleanup failed, keep
the module executable and its checkpoint/journal and retry recovery; deleting
those files can strand kernel state.
