import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import ts from 'typescript';
const source = readFileSync(new URL('../src/api.ts', import.meta.url), 'utf8');
const method = source.slice(source.indexOf('  async queryMetrics('), source.indexOf('\n  queryTraffic('));
const js = ts.transpileModule(`class Client { ${method} }`, { compilerOptions: { target: ts.ScriptTarget.ES2022 } }).outputText;
const Client = new Function('queryString', `${js}; return Client;`)((query) => '?' + new URLSearchParams(Object.entries(query).filter(([, value]) => value != null)));
test('history follows cursors so counters on later metric pages remain visible', async () => {
  const client = new Client(), requests = [], signal = new AbortController().signal;
  client.request = async (path, options) => {
    requests.push(path);
    assert.equal(options.signal, signal);
    return requests.length === 1 ? { samples: [], rollups: [{ metric: 'cpu' }], coverage: {}, next_cursor: 'next', truncated: true } : { samples: [], rollups: [{ metric: 'net.billing.rx_bytes' }], coverage: {}, truncated: false };
  };
  const result = await client.queryMetrics('node', { from: 'from', to: 'to', resolution: 'hour', signal });
  assert.equal(result.rollups.length, 2);
  assert.match(requests[1], /cursor=next/);
  assert.equal(result.truncated, false);
});
test('repeated cursors cannot loop forever', async () => {
  const client = new Client();
  client.request = async () => ({ samples: [], rollups: [], coverage: {}, next_cursor: 'same' });
  await assert.rejects(client.queryMetrics('node', { from: 'from', to: 'to' }), /did not advance/);
});
