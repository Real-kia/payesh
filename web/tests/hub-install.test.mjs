import { test } from 'node:test';
import assert from 'node:assert/strict';
import { hubDashboardURL } from '../src/lib/hubInstall.ts';

test('new hub links use the dashboard port for hostnames and IPv6', () => {
  assert.equal(hubDashboardURL('hub.example.test'), 'http://hub.example.test:8787');
  assert.equal(hubDashboardURL('192.0.2.10'), 'http://192.0.2.10:8787');
  assert.equal(hubDashboardURL('2001:db8::1'), 'http://[2001:db8::1]:8787');
});

test('new hub links reject credentials and URL paths', () => {
  for (const host of ['operator@hub.example.test', 'hub.example.test/path', 'hub.example.test?query', 'hub.example.test#fragment']) assert.throws(() => hubDashboardURL(host));
});
