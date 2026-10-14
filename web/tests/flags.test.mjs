import { test } from 'node:test';
import assert from 'node:assert/strict';
import { countryFlag, serverLabel } from '../src/lib/flags.ts';

test('country codes become flag emoji', () => {
  assert.equal(countryFlag('DE'), '🇩🇪');
  assert.equal(countryFlag('us'), '🇺🇸');
});

test('missing or malformed codes show no flag', () => {
  for (const value of [undefined, null, '', 'D', 'DEU', '1A', '🇩🇪', 42]) assert.equal(countryFlag(value), '');
});

test('server labels lead with the flag only when known', () => {
  assert.equal(serverLabel({ name: 'Frankfurt edge', countryCode: 'DE' }), '🇩🇪 Frankfurt edge');
  assert.equal(serverLabel({ name: 'Frankfurt edge' }), 'Frankfurt edge');
  assert.equal(serverLabel({ name: 'Frankfurt edge', countryCode: 'bad' }), 'Frankfurt edge');
});
