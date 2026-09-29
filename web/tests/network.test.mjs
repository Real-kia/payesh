import { test } from 'node:test';
import assert from 'node:assert/strict';
import { networkRates, networkRollupRate, formatNetworkRate } from '../src/network.ts';

const sample = (counter, overrides = {}) => ({ server_id: 'server-local-0001', collector_epoch: 'epoch-1', sequence: '1', observed_at: '2026-09-30T00:00:00Z', received_at: '2026-09-30T00:00:00Z', values: {}, counters: { 'net.billing.rx_bytes': counter }, validity: { 'net.billing.rx_bytes': 'valid' }, ...overrides });
const rate = (second) => networkRates([sample('9007199254740993'), sample('9007199254742493', { sequence: '2', observed_at: '2026-09-30T00:00:15Z', ...second })], 'net.billing.rx_bytes');

test('uses precise deltas above JavaScript safe integer range', () => assert.deepEqual(rate(), [null, 100]));
test('shows gaps for restarts, loss, invalid or missing counters, and clock reversal', () => {
  for (const override of [{ collector_epoch: 'epoch-2' }, { sequence: '3' }, { counters: {} }, { counters: { 'net.billing.rx_bytes': '' } }, { counters: { 'net.billing.rx_bytes': '1' } }, { validity: { 'net.billing.rx_bytes': 'unavailable' } }, { observed_at: '2026-09-29T00:00:00Z' }]) assert.deepEqual(rate(override), [null, null]);
});
test('shows zero traffic as zero and formats bytes per second as bits per second', () => {
  assert.deepEqual(rate({ counters: { 'net.billing.rx_bytes': '9007199254740993' } }), [null, 0]);
  assert.equal(formatNetworkRate(1250000), '10.00 Mbit/s');
  assert.equal(formatNetworkRate(null), '—');
});
test('uses observed duration for historical rates and hides incomplete buckets', () => {
  assert.equal(networkRollupRate({ coverage: 'complete', counter_delta: '6000', observed_seconds: 30 }), 200);
  assert.equal(networkRollupRate({ coverage: 'gap', counter_delta: '6000', observed_seconds: 30 }), null);
  assert.equal(networkRollupRate({ coverage: 'complete', counter_delta: '6000', observed_seconds: 0 }), null);
});
