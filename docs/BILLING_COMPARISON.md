# Provider billing comparison

Provider totals and Payesh totals must cover the same server, interfaces,
direction, start instant, end instant, timezone, and provider reset boundary.
Do not compare a provider's combined total with Payesh outbound-only usage, or
decimal gigabytes with binary gibibytes without converting both to bytes.

Export the exact Payesh `counted_bytes` decimal string from the matching
traffic period and the provider's authoritative byte total, then run:

```sh
go run ./scripts/billing-compare \
  -payesh-bytes 90071992547409930 \
  -provider-bytes 90071992547409931 \
  -tolerance-percent 5
```

The command emits `payesh.billing-comparison.v1` JSON and exits non-zero when
the absolute difference exceeds the configured tolerance. It uses arbitrary-
precision integers and rational arithmetic; values above JavaScript's safe
integer range are not rounded. A zero provider total matches only a zero
Payesh total.

Record the provider, account/instance identifier, period bounds, direction,
unit conversion, Payesh configuration revision, command, and JSON result in
release evidence. The comparator cannot prove that two differently scoped
inputs are comparable, so the operator must verify those fields first.
