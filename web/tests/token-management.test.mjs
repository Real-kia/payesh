import { test } from 'node:test';
import assert from 'node:assert/strict';
import { tokenStatus, filterTokens } from '../src/lib/tokenManagement.ts';

const now = Date.parse('2026-10-10T12:00:00Z');
const expiry = (delta) => ({ expires_at: new Date(now + delta).toISOString() });
test('expired tokens remain identifiable and expiry warning includes the seven-day boundary', () => {
  assert.equal(tokenStatus(expiry(-1), now), 'expired');
  assert.equal(tokenStatus(expiry(0), now), 'expired');
  assert.equal(tokenStatus(expiry(1), now), 'expiring');
  assert.equal(tokenStatus(expiry(7 * 86400000), now), 'expiring');
  assert.equal(tokenStatus(expiry(7 * 86400000 + 1), now), 'active');
});
test('owner filter and case-insensitive search compose without hiding expired tokens', () => {
  const tokens = [
    { id: '1', name: 'Claude', username: 'alice', hint: 'pyt_AbC', expires_at: '2020-01-01T00:00:00Z' },
    { id: '2', name: 'Deploy', username: 'bob', hint: 'pyt_Def', expires_at: '2099-01-01T00:00:00Z' }
  ];
  assert.deepEqual(filterTokens(tokens, '', ''), tokens);
  assert.deepEqual(filterTokens(tokens, 'alice', ' CLAUDE '), [tokens[0]]);
  assert.deepEqual(filterTokens(tokens, '', 'BOB'), [tokens[1]]);
  assert.deepEqual(filterTokens(tokens, '', 'ABC'), [tokens[0]]);
  assert.deepEqual(filterTokens(tokens, 'alice', 'deploy'), []);
});
