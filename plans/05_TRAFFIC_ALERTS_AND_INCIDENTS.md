# 05 — Traffic allowances, alerts and incidents

Status: Checkpoint A implemented in the working tree, with internal
Critical/Major review and fix cycles complete (see `docs/handoffs/05.md`);
provider-billing comparison, Linux traffic instrumentation, external outage
observation, real notification endpoints, and resource soaks remain
outstanding. Planning baseline: 2026-09-08.
Risk/assignment guidance: Medium–hard; time/accounting review.
Master milestone: M4.

Read [shared context](00_SHARED_CONTEXT.md) and [the package guide](README.md) before acting. This document is a future build assignment template, not permission to start coding in the documentation-only session.

## Prerequisites and ownership

**Requires:** 03 history/log/snapshot interfaces; 04 health/fleet identity for final integration.

**Owns:** Billing periods, traffic forecasting, alert state, notification delivery and incident orchestration.

Inspect actual repository conventions before selecting source paths. The lead owns shared contracts, schema ordering, dependency locks and integration unless explicitly delegated. A dependency may be replaced by an isolated test fixture while developing, but not for final integration acceptance.

## Work to implement

1. Implement selected interface/direction allowances and persistent period identity; coordinate continuity/checkpoints with 03.
2. Use calendar/timezone boundaries and preview schedule/allowance changes. Restart/update/reconnect never grants a fresh allowance.
3. Implement timestamp/coverage-based pending/firing/recovered states, hysteresis, grouping, maintenance, deduplication and bounded retry/reminders.
4. Implement Telegram/webhook delivery with protected secrets, bounded requests and explicit owner-configured destinations.
5. Use 03 bounded snapshots/log adapters for incident context and send contracts to 02. Later process observations do not prove an earlier culprit.
6. Expose period/usage semantics to 09. Crossing a monitoring threshold does not authorize a control policy.

## Acceptance gates

- Test days 29–31, DST/leap year, timezone/schedule/allowance changes, reboot, duplicates, late samples and gaps.
- Forecast requires ≥24h usable data and adequate coverage with an estimate label.
- Test brief/sustained load, sparse sampling, oscillation, maintenance/recovery, offline grouping and notification outage.
- Real-data novice flow links unhealthy metrics and retained evidence without query syntax; explain standalone self-outage limits.

## Out of scope

Kernel port instrumentation, automatic blocking, AI root-cause claims, probes or central log archive.

Reference extracts below sometimes mention neighboring packages. They explain integration requirements; they do not expand this worker's editable scope. Read-only inspection of neighboring interfaces is expected.

## Handoff and stop condition

Write `docs/handoffs/05.md` using the shared-context handoff checklist: baseline/files, implemented behavior, changed/consumed interfaces, actual test commands/results, measured or unmeasured costs, limitations, and next-package inputs. Request lead acceptance. Do not mark the full milestone/product complete or continue into another package on your own. If a prerequisite is missing, report exactly which contract, implementation or test environment is required.

## Detailed reference requirements

These are relevant verbatim extracts from [the master plan](../PLAN.md) at the planning baseline. Together with shared context, they are part of this assignment's requirements, not optional background. The lead synchronizes affected copies when the master changes.

### Traffic allowances

- Per server: selected interface(s), outbound-only or combined traffic, allowance in bytes, billing reset day, billing timezone, and warning percentages. Default warnings: 80%, 90%, and 100%.
- Use UTC unless the owner selects a billing timezone. If a reset day is 29–31 and absent that month, use that month's last day. Compute successive periods explicitly; do not use “30 days” as a month.
- Store the period identity with usage. An ordinary reboot or software update must never reset the allowance.
- Changing allowance size affects the active period immediately; changing billing schedule takes effect at the next existing boundary unless the owner explicitly starts a new period. Preview this behavior.
- Forecast only after sufficient observations, using an explicitly labelled recent-rate estimate. Default: at least 24 hours of usable data, with up to 7 days for the recent daily average. Hide misleading predictions when coverage is poor.
- Traffic is a host-side estimate, not the provider's authoritative bill. Incoming traffic may have been billed before Linux drops it.

### Alerts

- Starter rules: CPU above 90% for 5 minutes, available RAM below 10% for 5 minutes, disk/inodes above 90%, traffic thresholds, and unreachable nodes. Make rules editable and clearly show enabled defaults.
- Evaluate using sample timestamps and coverage, not a fixed count of samples. If sampling is slower than a rule's duration, warn about reduced detection precision; do not claim continuous evidence from one point.
- Use pending/firing/recovered state, hysteresis, grouping, deduplication, and bounded retries. Default sustained-alert reminders: at most once per hour; recovery is a separate message.
- Maintenance pauses suppress notifications, not collection. Group symptoms during a server outage to avoid dozens of secondary alerts.
- Keep control-policy events distinct from monitoring alerts. Record who changed a limit and its result.
- A standalone server cannot report its own complete power/network outage without another observer. The UI/docs must explain this; do not simulate external coverage.

### Collection and retention defaults

| Setting | Default | Required behavior |
| --- | --- | --- |
| Standard sample interval | 15 seconds | Adjustable 5 seconds–1 hour. |
| Economy interval | 60 seconds | Same measurements, less frequent collection. |
| Full-resolution history | 24 hours | Only retain samples actually collected. |
| Minute summaries | Up to age 7 days | Aggregate available samples; do not manufacture minute samples from hourly collection. |
| Hour summaries | Up to age 90 days | Preserve useful summary statistics. |
| Monthly traffic totals | 13 months | Independent from graph downsampling. |
| Payesh events/snapshots | 7 days, 100 MiB | Earliest limit wins; configurable. |
| Total managed data budget | Standalone 512 MiB; hub 2 GiB | Includes live database, WAL, indexes, snapshots, and managed operational logs. |
| Node offline spool | 32 MiB | Stop growth at the cap and report dropped coverage. |

Retention is a maximum age, not a promise that every configured age fits the disk budget. The UI shows effective oldest available data and any shortened retention. Budget settings must not silently delete configuration, active policies, identity keys, or current billing-period totals.

Implement time-weighted gauge summaries with sample counts/coverage, min/max, and average. Sum valid counter deltas for traffic and I/O; do not average byte totals. Align buckets using UTC timestamps. Display gaps; a zero value and a missing sample must look different.

Use batched SQLite transactions, bounded query results, and appropriate time/server indexes. Avoid an immediate fsync per field. A scheduled low-priority worker downsamples and removes expired data in small batches. Cap WAL growth and reclaim reusable/free space without a full blocking VACUUM on every cycle.

Start eviction before the hard storage ceiling. Remove expired/old detailed data first, then oldest snapshots and eligible history. Reserve operational headroom. If protected data plus database overhead cannot fit, suspend new history writes, keep live monitoring/control responsive where possible, and raise a storage error. Do not spin endlessly on “disk full.”

Upgrade staging and backups require separately reported temporary headroom; never silently consume an unbounded second copy under the ordinary data budget.

### Logs are events; metrics are samples

The sample interval controls numeric measurements. Application log events must not be arbitrarily sampled once an hour and presented as a complete log. Existing log retention remains the application's/journal's responsibility.

- Core log access reads existing sources only on demand, with source/time/severity filters, text search, live tail, and readable multiline entries.
- Default request bounds: 200 entries/page and 1 MiB per response. Bound line size and total scanned bytes/time. Truncate with an explicit notice and cancellation, not an unbounded process.
- Use cursors appropriate to journal entries or file identity/offset. Handle rotation, truncation, missing files, permissions, partial lines, invalid UTF-8, and a source disappearing during a query.
- Never interpret log content as HTML, shell commands, or instructions. Redact configured sensitive patterns and obvious credential fields from stored snapshots and diagnostic exports; do not claim redaction is perfect.
- Do not allow arbitrary root-readable paths from an API request. Use discovered or explicitly configured, validated sources; prevent traversal and unsafe symlink changes.
- No continuous central application-log archive in v1. Existing log history is available only while the node/source can be reached and retains it. Make that distinction visible.
- An incident snapshot stores up to 200 relevant entries, at most 256 KiB of log text, plus bounded nearby metric context. It is not a complete forensic archive.
- Label on-demand process information with its collection time. A process seen after an incident is not proof that it consumed resources during the earlier spike.
- Payesh's own structured operational logs rotate locally: default five files of at most 5 MiB each, within the managed storage budget. Routine samples should not produce verbose log lines.
