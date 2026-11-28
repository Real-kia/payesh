import assert from 'node:assert/strict';
import { test } from 'node:test';
import { recentFleetBandwidth } from '../src/lib/fleet-bandwidth.ts';

function server(timestamps, networkRx, networkTx, connectionState = 'connected') {
  return { connectionState, metricHistory: { ranges: { '15m': { timestamps, networkRx, networkTx } } } };
}

test('aggregates asynchronous samples and includes the nearest point at the saved history handoff', () => {
  const fleet = [
    server([95, 110, 125], [10, 20, 30], [5, 10, 15]),
    server([90, 105, 120], [1, 2, 3], [2, 4, 6]),
    server([110], [1000], [1000], 'disconnected')
  ];
  assert.deepEqual(recentFleetBandwidth(fleet, 105, 80), {
    timestamps: [105, 110, 120, 125],
    networkRx: [12, 22, 23, 33],
    networkTx: [9, 14, 16, 21]
  });
});

test('leaves gaps for stale, unavailable and interrupted samples without dropping a direction that is complete', () => {
  const fleet = [
    server([100, 115, 230, 245], [10, null, 30, 40], [5, 10, 15, 20]),
    server([100, 245], [1, 2], [2, 4])
  ];
  assert.deepEqual(recentFleetBandwidth(fleet, 99, 100), {
    timestamps: [100, 115, 205, 230, 245],
    networkRx: [11, null, null, null, 42],
    networkTx: [7, 12, null, null, 24]
  });
  assert.deepEqual(recentFleetBandwidth(fleet, 245, 100).timestamps, []);
  assert.deepEqual(recentFleetBandwidth(fleet, 100, 240).timestamps, [245]);
  assert.deepEqual(recentFleetBandwidth([...fleet, { connectionState: 'connected' }], 230, 240).networkRx, [null]);
});
