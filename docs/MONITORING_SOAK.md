# Monitoring ingestion and retention soak

`monitoring-soak` is a bounded operations/acceptance benchmark for the local
SQLite monitoring store. It generates valid metric/counter samples, ingests
them through the same durable `monitoring.Store.IngestSamples` path used by
the CLI, runs bounded retention passes, and reports throughput, ingestion
latency, prune behavior, database/WAL size, storage pressure, and teardown.

It is deliberately separate from the production server and never writes
`/var/lib/payesh` or `/var/log/payesh`. With no `-db`, it creates a private
temporary directory and removes it after the report. `-keep` retains that
directory for inspection. A caller-provided `-db` is never removed.

## Short CI run

```sh
GOCACHE=/private/tmp/payesh-go-cache go test ./scripts/monitoring-soak
make monitoring-soak-ci
```

The command writes one JSON report to stdout. Progress is written to stderr,
so the report can be captured directly:

```sh
make monitoring-soak-ci > soak.json
```

The benchmark acknowledges each accepted post-process sample. This keeps the
derived-work queue bounded and means the run measures raw durable ingestion
and retention, not an intentionally saturated queue.

## Longer run

Use an explicit duration and retention policy. The tool bounds a run to seven
days and caps generated batch size at the wire limit of 200 samples:

```sh
go run ./scripts/monitoring-soak \
  -duration=24h \
  -interval=15s \
  -prune-interval=5m \
  -retention-age=24h \
  -max-bytes=$((512 * 1024 * 1024)) \
  -quiet > soak-24h.json
```

Use `-db /path/to/soak.db -keep` when the database and WAL need to remain for
post-run inspection. Use a disposable filesystem with enough free space; a
managed storage ceiling does not override SQLite's own inability to allocate
pages. `storage_pressure=true` in the report means the configured ceiling was
reached and should be investigated, not silently treated as a pass.

## Report fields

The report format is `payesh.monitoring-soak.v1`. `attempted_samples`,
`inserted_samples`, `ingest_samples_per_second`, and the p50/p95/p99/max
latencies describe the durable ingest calls. `prune_runs`,
`prune_deleted_samples`, and `retention_observed` show whether age-based
removal actually happened. `database_bytes_*` and `wal_bytes_*` are sampled
while the store is open; `storage_bytes_*` also includes configured managed
paths. WAL is checkpointed during final close, so the pre-close peak is the
useful bound for growth analysis. `clean_teardown=true` means a generated
temporary directory was removed; it is false when `-keep` intentionally
retains state.

## Evidence policy

This tool produces measurements; it does not label them as production
capacity, 24-hour evidence, or a pass/fail release gate. Record the exact
command, OS/kernel, Go version, storage device, duration, configuration, and
JSON report when accepting a run. A short CI run proves repeatability and
cleanup only. A 24-hour retention/soak claim requires an actually completed
24-hour run and review of the resulting report.
