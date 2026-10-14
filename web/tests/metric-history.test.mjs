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
