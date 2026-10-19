import { test } from 'node:test';
import assert from 'node:assert/strict';
import { getSavedTimezone, saveTimezone } from '../src/timezone.ts';

function setWindow(t, value) {
  const previous = Object.getOwnPropertyDescriptor(globalThis, 'window');
  Object.defineProperty(globalThis, 'window', { configurable: true, value });
  t.after(() => { if (previous) Object.defineProperty(globalThis, 'window', previous); else delete globalThis.window; });
}

test('timezone is saved and restored from browser storage', (t) => {
  const values = new Map();
  setWindow(t, { localStorage: { getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value) } });
  assert.equal(saveTimezone('Asia/Tehran'), true);
  assert.equal(getSavedTimezone(), 'Asia/Tehran');
});

test('unavailable storage keeps the chosen timezone for the tab and reports persistence failure', (t) => {
  setWindow(t, { get localStorage() { throw new Error('Storage blocked'); } });
  assert.equal(saveTimezone('Europe/Berlin'), false);
  assert.equal(getSavedTimezone(), 'Europe/Berlin');
});

test('failed writes do not restore an older stored timezone', (t) => {
  setWindow(t, { localStorage: { getItem: () => 'UTC', setItem: () => { throw new Error('Quota exceeded'); } } });
  assert.equal(saveTimezone('Asia/Tehran'), false);
  assert.equal(getSavedTimezone(), 'Asia/Tehran');
});
