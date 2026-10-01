# Node protocol v1

This is the authenticated node↔hub envelope used by `payesh-agent`. The node
opens one persistent TLS WebSocket; messages are bidirectional after it opens.
Browser/API sessions never share this channel.

## Connection and identity

- TLS uses the installed enrollment trust anchor. Each node has one certificate
  and stable random `server_id`; hostname and IP are never identity keys.
- A new random `collector_epoch` is generated on every agent start. `sequence`
  increases within an epoch and starts at zero.
- The hub permits one active controller per node. Duplicate, revoked, expired,
  or replayed identities fail closed and require authenticated re-enrollment.
- Heartbeats are independent of metric cadence: send every 30 seconds and mark
  stale after 90 seconds. Reconnect uses jittered exponential backoff capped at
  60 seconds.

## Envelope

```json
{"protocol":"payesh.node.v1","message":"hello","request_id":"…","sent_at":"2026-09-08T00:00:00Z","body":{}}
```

The envelope has a bounded body and maximum frame size. Unknown message types
are rejected with a typed error. `request_id` is required for jobs and allows
idempotent acknowledgement.

Shared Go payloads are `Hello`, `Heartbeat`, `SampleBatch`, `Acknowledgement`,
and `Cancellation`; `SampleBatch` is capped at 200 samples/gaps and the
envelope body at 1 MiB. These types are the required payloads for the matching
`message` values and are not arbitrary command JSON.

`hello` includes software version, protocol range, architecture/platform,
capabilities, a bounded typed installed-module ID/version inventory, and
configuration revision. The hub rejects a range that does not include its
supported `v1` protocol before establishing the node channel. `sample` contains
the `NodeMetricSample` fields plus integer counters. It deliberately excludes
`received_at`: the hub assigns that
field only when it durably accepts a sample, producing the `MetricSample` record
returned by the API. Source observation and hub receipt timestamps are stored
separately. If the source clock is ahead of the hub, the sample is preserved and marked with
`timestamp_uncertainty: source-clock-ahead`; timestamp ordering is not a
validity gate.

## Delivery

Samples are acknowledged only after durable hub acceptance. The hub deduplicates
`server_id + collector_epoch + sequence`; retransmission after lost ACK is safe.
`through_sequence` advances only through the highest contiguous durable coverage
sequence in the collector epoch, starting at sequence zero. A coverage interval
is either a durable sample or an explicit node-reported loss gap (for example a
bounded spool eviction); an unreported hole is never covered. If neither a
sample nor an explicit gap reaches sequence zero, the hub reports a retryable
sequence-gap result instead of claiming a cumulative acknowledgement. An
out-of-order sample is retained, but a missing sequence is never implied to
have been acknowledged.
The node spool is batched, oldest-first, capped at 32 MiB, and reports an
explicit coverage gap when eviction occurs. Heartbeats, ACKs, cancellations,
and safety messages take priority over logs/downloads. Queues and concurrent
streams are bounded.

Jobs carry target identity, expiry, idempotency key, and expected configuration
revision. Expired or conflicting jobs fail explicitly on reconnect; old commands
are not silently applied.

## Sampling policy extension

Nodes advertising the `sampling-policy` capability can receive optional
`sampling_interval_seconds` (5–3600) in authenticated sample acknowledgements.
It controls future collection without changing sequence numbers or dropping
already collected samples. Heartbeats remain independent. The hub omits this
field for older nodes, whose strict decoders reject unknown fields. Policy can
be included on a retryable rejected batch so storage pressure also slows
future collection while the existing batch remains in the spool.
