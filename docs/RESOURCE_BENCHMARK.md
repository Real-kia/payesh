# Resource benchmark procedure

`resource-benchmark` records bounded, process-tree-scoped resource evidence for
the agent, server, and optional modules. It does not install services, alter
kernel state, generate traffic, or write anywhere except the requested report.
It is a measurement harness; it does not claim that a workload is
representative by itself.

## Usage

Run a command for at most 30 seconds and write JSON:

```sh
scripts/resource-benchmark.sh \
  -label payesh-server -duration 30s -interval 1s \
  -out dist/bench/payesh-server.json -- \
  dist/payesh-server -data /tmp/payesh-benchmark.db
```

Observe an already-running process without terminating it:

```sh
scripts/resource-benchmark.sh -pid 1234 -duration 60s -interval 5s \
  -format tsv -out dist/bench/server.tsv
```

The command mode starts its process in a private process group. On timeout it
sends that group `SIGTERM`, waits two seconds, and then leaves any process that
does not exit visible in the report. The PID mode never terminates the target.
Durations are limited to 24 hours and intervals to 10ms–1 hour.

## Measurements and interpretation

On Linux, `/proc` is sampled for the root and all descendants visible to the
invoking user. CPU is cumulative user plus system time, RSS is resident memory,
and block I/O uses `read_bytes`/`write_bytes`. On macOS and other Unix systems,
`ps` supplies CPU/RSS samples; block I/O is explicitly marked unavailable.
`measurement_mode`, `process_scope`, `io_supported`, `sampler_errors`, and
`notes` must be retained with every result. A report with sampler errors or
unavailable I/O is not a complete overhead result.

Use a warm-up, then repeat the same workload with each optional module disabled
and enabled. For release evidence, record host/kernel/architecture, build
digest, workload, duration, interval, module configuration, and whether the
run was idle, steady-state, or under load. A 30-minute steady-state run and a
separate 24-hour retention/ingestion soak are required release evidence; short local runs only validate the harness.

The JSON schema is `payesh.resource_benchmark.v1`. TSV has one row per run and
is suitable for importing into a spreadsheet. Reports contain no environment
secrets, but command arguments may contain paths, so store them with the same
care as normal operational logs.
