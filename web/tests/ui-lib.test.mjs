import { test } from 'node:test';
import assert from 'node:assert/strict';
import { resolveLogLevel, generatePreviewFleetLogs } from '../src/lib/logs.ts';
import { formatBytes, percentage, resourceLevel, metricValue, sumNetworkRate } from '../src/lib/format.ts';
import { withAlpha } from '../src/lib/chart.ts';

test('stored severities win and INFO text is checked for error and warning markers', () => {
  assert.equal(resolveLogLevel('err', 'fine'), 'ERROR');
  assert.equal(resolveLogLevel('Warning', ''), 'WARN');
  assert.equal(resolveLogLevel('DEBUG', 'panic: no'), 'DEBUG');
  assert.equal(resolveLogLevel('INFO', 'level=error msg="db"'), 'ERROR');
  assert.equal(resolveLogLevel(undefined, 'database is locked'), 'ERROR');
  assert.equal(resolveLogLevel('INFO', 'WARNING: disk almost full'), 'WARN');
  assert.equal(resolveLogLevel('', 'started'), 'INFO');
});

test('preview logs respect the server filter', () => {
  const servers = [{ id: 'a', name: 'A' }, { id: 'b', name: 'B' }];
  const all = generatePreviewFleetLogs(servers, 'all');
  const onlyB = generatePreviewFleetLogs(servers, 'b');
  assert.ok(all.length > onlyB.length && onlyB.length > 0);
  assert.ok(onlyB.every((entry) => entry.serverId === 'b'));
  assert.equal(new Set(all.map((entry) => entry.id)).size, all.length);
});

test('byte counters stay exact past the safe integer range', () => {
  assert.equal(formatBytes('0'), '0.0 B');
  assert.equal(formatBytes('1526000000000'), '1.5 TB');
  assert.equal(formatBytes('not-a-number'), '—');
  assert.equal(percentage('9007199254740993', '18014398509481986'), 50);
  assert.equal(percentage('5', '0'), 0);
  assert.equal(percentage('300', '100'), 100);
});

test('resource levels and metric text', () => {
  assert.equal(resourceLevel(null), 'normal');
  assert.equal(resourceLevel(74.9), 'normal');
  assert.equal(resourceLevel(75), 'warning');
  assert.equal(resourceLevel(90), 'critical');
  assert.equal(metricValue(null), '—');
  assert.equal(metricValue(12.345), '12.35%');
});

test('fleet bandwidth is unknown unless every connected server reports a current rate', () => {
  const server = (rate, state = 'connected') => ({ connectionState: state, freshnessState: 'fresh', metricHistory: { ranges: { '15m': { timestamps: [Date.now() / 1000], networkRx: [rate], networkTx: [rate] } } } });
  assert.equal(sumNetworkRate([server(10), server(5)], 'download'), 15);
  assert.equal(sumNetworkRate([server(10), server(null)], 'download'), null);
  assert.equal(sumNetworkRate([server(10), server(null, 'disconnected')], 'upload'), 10);
  assert.equal(sumNetworkRate([], 'upload'), null);
});

test('chart colours accept hex and rgb forms', () => {
  assert.equal(withAlpha('#2f6bd8', 0.5), 'rgba(47, 107, 216, 0.5)');
  assert.equal(withAlpha('#fff', 0.1), 'rgba(255, 255, 255, 0.1)');
  assert.equal(withAlpha('rgb(1, 2, 3)', 0.2), 'rgba(1, 2, 3, 0.2)');
  assert.equal(withAlpha('oklch(0.5 0.1 200)', 0.2), 'oklch(0.5 0.1 200)');
});
