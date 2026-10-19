import { test } from 'node:test';
import assert from 'node:assert/strict';
import { deleteServersConcurrently } from '../src/lib/bulkDelete.ts';

test('bulk deletion overlaps requests, bounds concurrency, and reports each success immediately', async () => {
  let active = 0, maximum = 0, settled = false;
  const removed = [];
  const result = await deleteServersConcurrently([1,2,3,4,5,6,7,8,9], async () => {
    maximum = Math.max(maximum, ++active);
    await new Promise(resolve => setTimeout(resolve, 0));
    active--;
  }, { onDeleted: target => { assert.equal(settled, false); removed.push(target); } });
  settled = true;
  assert.equal(maximum, 4);
  assert.equal(result.errors.length, 0);
  assert.deepEqual(result.deleted.sort(), [1,2,3,4,5,6,7,8,9]);
  assert.deepEqual(removed.sort(), result.deleted);
});

test('bulk deletion retains failed targets and finishes the other deletions', async () => {
  const failure = new Error('revision conflict');
  const result = await deleteServersConcurrently([1,2,3,4,5], async target => { if (target === 2) throw failure; });
  assert.deepEqual(result.deleted.sort(), [1,3,4,5]);
  assert.deepEqual(result.errors, [failure]);
});

test('an expired session stops new work while retaining results of in-flight requests', async () => {
  const expired = new Error('session expired');
  const attempted = [];
  const result = await deleteServersConcurrently([1,2,3,4,5,6,7,8], async target => {
    attempted.push(target);
    if (target === 1) throw expired;
    await new Promise(resolve => setTimeout(resolve, 0));
  }, { isAuthFailure: error => error === expired });
  assert.deepEqual(attempted, [1,2,3,4]);
  assert.deepEqual(result.deleted.sort(), [2,3,4]);
  assert.deepEqual(result.errors, [expired]);
});
