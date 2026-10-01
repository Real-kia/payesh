# Traffic usage by date range

Open **Servers → select a server → Traffic → Traffic used in a date range**.
Choose whole-hour start and end times in **UTC**, then select **Calculate
usage**. The end time is excluded. Reports cover up to 90 days of retained
hourly history and show download, upload, and their combined recorded total.

The report reads the host's billing counters, which exclude virtual interfaces
from automatic discovery. It does not add per-interface counters to that
baseline, so bridged/container traffic is not counted twice. Explicit
`payesh-agent -billing-interfaces=wan0` selections override discovery.

Traffic intervals are attributed to the hour containing their ending sample.
Consequently traffic crossing a boundary may appear in the next hour, and
start/end hours may be partial. These are recorded host totals, not a provider
invoice or an estimate of unobserved traffic.

Unavailable history remains **Unavailable**. Reset, missing, and uncertain
counter buckets are excluded; the report shows how many selected hours have
usable totals for each direction. Incomplete totals are visibly marked.
It cannot recover history that was never collected or has expired.

The authenticated read-only endpoint is
`GET /api/v1/servers/{server_id}/traffic/usage?from=...&to=...` with RFC3339
timestamps. It queries only two existing hourly counter series, streams rows,
and returns decimal byte strings to preserve large totals. It creates no
additional collector, background job, or database tables.

Routine telemetry samples every 15 seconds. Dashboard refreshes also use
15 seconds, stop while the tab is hidden, and avoid overlapping requests.
Virtual network interfaces are omitted from routine per-interface telemetry
unless explicitly selected, limiting container-host metric churn. Existing
historical rows expire under the normal retention policy; updates do not
delete that history immediately.
