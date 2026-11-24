import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import ts from 'typescript';
const source = readFileSync(new URL('../src/App.svelte', import.meta.url), 'utf8');
const body = source.slice(source.indexOf('  function mergeMetricCharts('), source.indexOf('  let liveRefreshBusy'));
const js = ts.transpileModule(body, { compilerOptions: { target: ts.ScriptTarget.ES2022 } }).outputText;
const merge = new Function(`${js}; return mergeMetricCharts;`)();
const chart = (timestamps, values) => ({ timestamps, cpu: values, memory: values, swap: values, disk: values, networkRx: values, networkTx: values, coverage: 'complete' });
test('overlapping refreshes keep one sorted point per timestamp', () => {
  const merged = merge(chart([10, 20, 30], [1, 2, 3]), chart([20, 30, 40], [22, 33, 4]), 900);
  assert.deepEqual(merged.timestamps, [10, 20, 30, 40]);
  assert.deepEqual(merged.cpu, [1, 22, 33, 4]);
});
test('empty refresh retains existing history and all arrays stay aligned', () => {
  const previous = chart([10, 20], [1, 2]);
  const merged = merge(previous, chart([], []), 900);
  assert.deepEqual(merged, previous);
});
test('history drops samples outside the selected window', () => {
  assert.deepEqual(merge(chart([10, 800], [1, 2]), chart([1000], [3]), 900).timestamps, [800, 1000]);
});

test('a complete full-window refresh clears a CPU warning, while a partial refresh preserves it', () => {
  const previous = { ...chart([10, 20], [1, 2]), cpuGap: true };
  const next = { ...chart([20, 30], [2, 3]), cpuGap: false };
  assert.equal(merge(previous, next, 900).cpuGap, false);
  assert.equal(merge(previous, next, 900, true).cpuGap, true);
});

const chartBody = source.slice(source.indexOf('  const cpuMetricNames'), source.indexOf('  function applyMetric('));
const chartJS = ts.transpileModule(chartBody, { compilerOptions: { target: ts.ScriptTarget.ES2022 } }).outputText;
const metricChart = new Function('networkRates', `${chartJS}; return metricChart;`)(() => []);

test('only missing CPU samples or stream gaps warn on a raw CPU chart', () => {
  const sample = { observed_at: '2026-01-01T00:00:00Z', values: { 'cpu.utilization': 2 } };
  const query = { samples: [sample], rollups: [], coverage: { 'cpu.utilization': 1, 'net.billing.rx_bytes': 0 } };
  assert.equal(metricChart(query).cpuGap, false);
  assert.equal(metricChart({ ...query, gaps: [{}] }).cpuGap, true);
  const invalid = metricChart({ ...query, samples: [{ ...sample, validity: { 'cpu.utilization': 'invalid' } }] });
  assert.equal(invalid.cpuGap, true);
  assert.deepEqual(invalid.cpu, [null]);
});
