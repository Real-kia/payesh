# Server cutover

`payesh cutover` moves one server's history and its ingest authority from a
source to a destination. It is an operator tool for a supervised, one-off move.
It is not a high-availability mechanism and it is not part of normal updates.

Status: implemented and tested locally, against local stores, over a loopback
mutual-TLS peer, and with the real binary on Linux amd64 and arm64 inside a
sandbox with no outside network. It has also been run once between two hosts over
the public internet with a throwaway test server; see
[Known limitations](KNOWN_LIMITATIONS.md).

## What moves

For the one named server: its retained samples, coverage gaps, retired-epoch
markers, traffic periods, control policies and rollups, using the replay-safe
migration artifact. Browser accounts, fleet identity state, owner credentials and
action payloads are never transferred. Artifacts larger than 64 MiB are refused.

## How authority stays single-owner

1. The source freezes the server. From that commit on, the source refuses ingest,
   new jobs, action leases and results, module, policy and configuration changes,
   enrollment claims, certificate renewals, enrollment tokens and node identities
   for it. A write that loses the race fails; it is never acknowledged.
2. The freeze records a digest of the server's exact stored state, computed in the
   same transaction.
3. The destination imports the artifact, recomputes the digest from what it
   actually holds, and activates authority only if it matches. A missing or extra
   tail refuses activation and leaves no authority behind.
4. The source then relinquishes. The destination's generation is the source's final
   generation plus one, so authority only moves forward.

A failure before the destination activates resumes the source with a recorded,
generation-advancing abort. The cutover identifier is then burned: retry with a
new identifier. Every phase is idempotent and every exported artifact is written
atomically before use, so a crash resumes from the interrupted phase once its
lease expires. A failure after activation stops in `recovery-required` for an
operator.

## Trust model

The destination serves TLS 1.3 and requires a client certificate. A client is
authorized only by a **grant**: one client-certificate fingerprint, one server, one
cutover, expiring within 24 hours. The peer never reads the server or cutover from
the request, so a client cannot widen its own access. The client pins the
destination's certificate fingerprint and refuses any other. A new grant replaces
the client's previous one; a grant file that names one client twice is rejected,
and a corrupt or group/world-readable grant file denies everything.

## Procedure

On the **source**, create a client identity and give the printed fingerprint to
the destination operator:

    payesh cutover identity --dir /var/lib/payesh/cutover-client

On the **destination**, serve the peer (it prints a JSON line with the address and
the certificate pin to give to the source operator), then grant that fingerprint
one cutover:

    payesh cutover serve --db /var/lib/payesh/payesh.db --listen 0.0.0.0:9798 \
      --generate-tls --tls-cert /var/lib/payesh/cutover-cert.pem \
      --tls-key /var/lib/payesh/cutover-key.pem \
      --grants-file /var/lib/payesh/cutover-grants.json \
      --spool-dir /var/lib/payesh/cutover-spool

    payesh cutover grant --grants-file /var/lib/payesh/cutover-grants.json \
      --server-id SERVER_ID --cutover-id CUTOVER_ID --client-fingerprint FINGERPRINT

The serving process re-reads the grant file on every request, so a grant added
while it runs takes effect immediately. Stop it with SIGTERM when finished.

On the **source**, run the cutover with a new identifier:

    payesh cutover run --db /var/lib/payesh/payesh.db --server-id SERVER_ID \
      --cutover-id CUTOVER_ID --source-id OWNER_A --destination-id OWNER_B \
      --source-role hub --destination-role node \
      --peer-url https://DESTINATION:9798 --peer-pin PIN \
      --client-cert /var/lib/payesh/cutover-client/client-cert.pem \
      --client-key /var/lib/payesh/cutover-client/client-key.pem \
      --state-dir /var/lib/payesh/cutover-state

    payesh cutover status --journal /var/lib/payesh/cutover-state/cutover-journal.db \
      --cutover-id CUTOVER_ID

`run` keeps a verified SQLite backup of the source in the state directory and
never deletes source data.

Open the destination's port only to the source, using a host or network firewall
rule (a systemd `IPAddressAllow` list did not reliably block other hosts in testing),
only for the duration of the cutover, and remove the grant file entry and stop the server afterwards.

To run the peer under a service manager instead of a terminal, use
`deploy/systemd/payesh-cutover-peer.service` or `deploy/openrc/payesh-cutover-peer`.
Both read `PAYESH_CUTOVER_DB` and `PAYESH_CUTOVER_LISTEN` from
`/etc/payesh/cutover-peer.env`, keep the certificate, key, grant file and spool under
`/var/lib/payesh/cutover`, refuse to start without a grant file, and are meant to be
started for one cutover and stopped afterwards (they are not enabled at boot). The
certificate is created once and reused, so the pin stays the same across restarts.
Run `payesh cutover grant` as the service user (`payesh`), so the grant file and its
lock file stay owned by the account the service runs as.
