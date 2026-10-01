# Storage, sampling, and notifications

Open **Settings → Storage & sampling**. Only the owner can save changes;
other authenticated users can view the current usage and settings.

The default maximum database size is **1 GB** (1,000,000,000 bytes), including
SQLite journal/WAL files. The limit can be set between 0.128 and 64 GB. It is
persisted in the database and survives updates and service restarts. Separate
CLI ingestion and dashboard processes read the same settings.

Cleanup starts near **90%** of the limit and works toward **85%**, leaving
room for new data. It removes oldest eligible raw measurements first, then
old derived history and captured logs when raw history has already been
removed. Selection follows observation time, including late-arriving history.
Server identities, authentication state, and traffic billing periods are
preserved. Cleanup uses bounded transactions, checkpoints, and incremental
vacuum rather than a blocking full database rebuild.

Cleanup runs before ingestion and in the periodic maintenance worker. A large
reduction of the limit can require multiple passes. If the database is still
above the hard limit, new history writes pause and remote nodes retain
unacknowledged batches in their bounded spool. Protected state and filesystem
conditions can prevent immediate reclamation; the limit is not a guarantee
that existing files shrink instantly after saving a smaller value.

## Sampling

Normal collection defaults to **15 seconds**. Set a longer interval to reduce
new sample volume. Intervals are bounded to 5–3600 seconds. Existing raw
history normally expires after 24 hours, with minute summaries retained for
7 days and hourly summaries for 90 days; pressure cleanup may shorten these
windows.

Automatic sampling reduction is enabled by default. When database usage
reaches 90%, new collection switches to the storage-saving interval, which
defaults to **60 seconds** and must be at least the normal interval. It returns
to the normal interval below 75% to avoid frequent switching. Disabling the
automatic reduction does not disable history cleanup.

The local master agent reads the policy through a read-only database handle.
Updated nodes receive it over the authenticated TLS connection in sample
acknowledgements. Older nodes do not receive unsupported protocol fields and
keep their current interval until updated. Node heartbeats continue every
30 seconds independently of metric collection. Sampling changes apply on a
subsequent collection cycle; they do not drop already collected samples or
create sequence gaps. A node keeps its last received cadence while offline.

## Notifications

Storage notifications are enabled by default. **Settings → Notifications**
and the top-bar unread count show database warnings at 80%, storage-saving
mode, old-history cleanup, hard-limit write pauses, and recovery. The latest
100 events are retained. Repeated event kinds are deduplicated within
10-minute windows to avoid filling the inbox during sustained pressure.

These are shared workspace notifications. Users with edit access can mark
all read; only the owner can enable or disable future storage notifications.
They are in-app notifications. Existing webhook/Telegram alert configuration
continues to handle monitoring alerts separately.

The authenticated API exposes `GET/PUT /api/v1/settings/storage` and
`GET/POST /api/v1/notifications`. Settings writes require owner access, CSRF,
and the current revision. Notification mutations require edit access and
CSRF. Settings reads include current database bytes and effective cadence.
