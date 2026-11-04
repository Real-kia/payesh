import { test } from 'node:test';
import assert from 'node:assert/strict';
import { followLink } from '../src/lib/navigation.ts';

test('ordinary link clicks navigate locally and preserve native keyboard activation', () => {
  let prevented = false, navigated = false;
  followLink({ button: 0, preventDefault() { prevented = true; } }, () => { navigated = true; });
  assert.ok(prevented && navigated);
});
test('modified clicks and middle clicks stay native and do not change the current page', () => {
  for (const modifier of [{ metaKey: true }, { ctrlKey: true }, { shiftKey: true }, { altKey: true }, { button: 1 }, { defaultPrevented: true }]) {
    followLink({ button: 0, ...modifier, preventDefault() { assert.fail('native navigation prevented'); } }, () => assert.fail('current page navigated'));
  }
});
