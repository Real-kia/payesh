import { test } from 'node:test';
import assert from 'node:assert/strict';
import { filterServers, defaultFilter, reconcileServerFilter } from '../src/lib/serverFilter.ts';
import { allAddresses } from '../src/lib/format.ts';

const server = (id, extra) => ({ id, name: id, role: 'node', displayState: 'healthy', connectionState: 'connected', metrics: { cpu: 10, memory: 10, swap: null, disk: 10 }, ...extra });
const fleet = [
  server('alpha', { address: '203.0.113.1', addresses: ['203.0.113.1', '198.51.100.20'], tags: ['edge'], countryCode: 'DE', metrics: { cpu: 80, memory: 5, swap: null, disk: 1 } }),
  server('beta', { address: '203.0.113.2', tags: ['db'], countryCode: 'IR' }),
  server('gamma', { displayState: 'unreachable', connectionState: 'disconnected' })
];
const names = (state) => filterServers(fleet, { ...defaultFilter, ...state }).map((s) => s.name);

test('search matches name, every address and tags', () => {
  assert.deepEqual(names({ query: '198.51.100.20' }), ['alpha']);
  assert.deepEqual(names({ query: 'db' }), ['beta']);
  assert.deepEqual(names({ query: 'GAM' }), ['gamma']);
});

test('filters and sorts', () => {
  assert.deepEqual(names({ tag: 'EDGE' }), ['alpha']);
  assert.deepEqual(names({ country: 'IR' }), ['beta']);
  assert.deepEqual(names({ status: 'offline' }), ['gamma']);
  assert.deepEqual(names({ sort: 'cpu' }), ['alpha', 'beta', 'gamma']);
  assert.deepEqual(names({}), ['gamma', 'alpha', 'beta']);
});

test('addresses are merged without duplicates', () => {
  assert.deepEqual(allAddresses(fleet[0]), ['203.0.113.1', '198.51.100.20']);
  assert.deepEqual(allAddresses(fleet[2]), []);
});

test('clears saved filters whose options disappeared and keeps other choices', () => {
  const state = { ...defaultFilter, tag: 'retired', country: 'GB', status: 'healthy', query: 'alpha', sort: 'name' };
  assert.deepEqual(reconcileServerFilter(state, ['edge', 'db'], ['DE', 'IR']), { ...state, tag: '', country: '' });
});

test('keeps a tag filter when only the option casing changed', () => {
  const state = { ...defaultFilter, tag: 'EDGE', country: 'DE' };
  const current = reconcileServerFilter(state, ['edge', 'db'], ['DE', 'IR']);
  assert.deepEqual(current, { ...state, tag: 'edge' });
  assert.equal(reconcileServerFilter(current, ['edge', 'db'], ['DE', 'IR']), current);
});
