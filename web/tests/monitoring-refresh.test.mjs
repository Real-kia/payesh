import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import ts from 'typescript';

const source = readFileSync(new URL('../src/App.svelte', import.meta.url), 'utf8');
const method = source.slice(source.indexOf('  async function readOverview('), source.indexOf('\n  let selectedRefreshController'));
const js = ts.transpileModule(method, { compilerOptions: { target: ts.ScriptTarget.ES2022 } }).outputText;
const bootMethod = source.slice(source.indexOf('  async function loadApiData('), source.indexOf('\n  function restoreState'));
const bootJS = ts.transpileModule(bootMethod, { compilerOptions: { target: ts.ScriptTarget.ES2022 } }).outputText;
function harness(apiClient, timeoutMs = 1000) {
  return new Function('apiClient', 'timeoutMs', `
    let liveRefreshBusy = false, PREVIEW_MODE = false, sessionState = 'authenticated', authExpired = false;
    let activePage = 'monitoring', partialWarning = '', servers = [{ id: 'node' }], applied = 0;
    let apiAbortController, previewState, apiError, activeInstall, updateStatus, selectedServerId, savedUiState;
    const loadAccounts = () => {};
    const checkLatestUpdate = async () => {};
    const loadNotifications = () => {};
    const restoreState = () => {};
    const document = { hidden: false };
    const window = { setTimeout: (fn) => setTimeout(fn, timeoutMs), clearTimeout };
    class ApiError extends Error {}
    const emptyApiServer = item => item;
    const applyOverview = (overview, onlyIds) => {
      applied++;
      servers = servers.map(server => !onlyIds || onlyIds.has(server.id)
        ? { ...server, ...overview.items.find(item => item.id === server.id) }
        : server);
    };
    ${js}
    ${bootJS}
    return { boot: loadApiData, refresh: refreshMonitoring, state: () => ({ liveRefreshBusy, partialWarning, applied, servers }) };
  `)(apiClient, timeoutMs);
}

function deferred() {
  let resolve;
  const promise = new Promise(r => { resolve = r; });
  return { promise, resolve };
}

test('metrics appear while the server status request is still pending', async () => {
  const list = deferred();
  const client = harness({ getOverview: async () => ({ items: [{ id: 'node', metrics: { cpu: 42 }, capacity: { cpuCores: 8 } }] }), listServers: () => list.promise });
  const refresh = client.refresh();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(client.state().applied, 1);
  assert.equal(client.state().liveRefreshBusy, true);
  list.resolve({ items: [{ id: 'node' }] });
  await refresh;
  assert.equal(client.state().servers[0].metrics.cpu, 42);
  assert.equal(client.state().servers[0].capacity.cpuCores, 8);
});

test('failed metrics show an error and a later successful retry clears it', async () => {
  let fail = true;
  const client = harness({
    getOverview: async () => { if (fail) throw new Error('Request failed (500)'); return { items: [] }; },
    listServers: async () => ({ items: [] })
  });
  await client.refresh();
  assert.match(client.state().partialWarning, /Live metrics.*500/);
  assert.equal(client.state().liveRefreshBusy, false);
  fail = false;
  await client.refresh(false);
  assert.equal(client.state().partialWarning, '');
  assert.equal(client.state().applied, 1);
});

test('initial metrics reads time out and leave the refresh available for retry', async () => {
  const client = harness({ getOverview: ({ signal }) => new Promise((resolve, reject) => {
    signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true });
  }) }, 1);
  await client.refresh(false);
  assert.match(client.state().partialWarning, /timed out/);
  assert.equal(client.state().liveRefreshBusy, false);
});

test('initial overview read starts while the server list is still in flight', async () => {
  const list = deferred();
  const metrics = deferred();
  let requested = false;
  const client = harness({
    getOverview: () => { requested = true; return metrics.promise; },
    listServers: () => list.promise
  });
  const loading = client.boot();
  assert.equal(requested, true);
  metrics.resolve({ items: [{ id: 'node', metrics: { cpu: 42 } }] });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(client.state().applied, 0);
  list.resolve({ items: [{ id: 'node' }] });
  await loading;
  assert.equal(client.state().servers[0].metrics.cpu, 42);
});

test('the same refresh supplies metrics for servers newly returned by the list', async () => {
  const list = deferred();
  const client = harness({
    getOverview: async () => ({ items: [{ id: 'new', metrics: { cpu: 55 } }] }),
    listServers: () => list.promise
  });
  const refresh = client.refresh();
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(client.state().servers, [{ id: 'node' }]);
  list.resolve({ items: [{ id: 'new' }] });
  await refresh;
  assert.equal(client.state().servers[0].metrics.cpu, 55);
});
