import { test } from 'node:test';
import assert from 'node:assert/strict';
import { categoryForAlert, categoryForMetric, categoryForRule } from '../src/lib/alertCategories.ts';

const rule = (id, expression, group_key) => ({ id, name: id, expression, group_key, enabled: true, duration_seconds: 60, reminder_seconds: 300, idempotency_key: id, created_at: '2026-10-01T00:00:00Z' });

test('metrics map to the signal they watch', () => {
  assert.equal(categoryForMetric('cpu.utilization'), 'cpu');
  assert.equal(categoryForMetric('memory.swap_used_percent'), 'memory');
  assert.equal(categoryForMetric('disk.root.inodes_used_percent'), 'disk');
  assert.equal(categoryForMetric('traffic.egress'), 'network');
  assert.equal(categoryForMetric('node.unreachable'), 'availability');
  assert.equal(categoryForMetric('heartbeat.age_seconds'), 'availability');
  assert.equal(categoryForMetric('custom.thing'), 'other');
});

test('a known group key wins over the expression', () => {
  assert.equal(categoryForRule(rule('r1', 'custom.metric > 1', 'disk')), 'disk');
  assert.equal(categoryForRule(rule('r2', 'memory.used_percent > 90', 'unknown-group')), 'memory');
  assert.equal(categoryForRule(rule('cpu-high-abc', 'custom.metric > 1')), 'cpu');
});

test('alerts use their rule, falling back to the rule id text', () => {
  const rules = [rule('r-disk', 'disk.root.used_percent > 90')];
  assert.equal(categoryForAlert({ rule_id: 'r-disk' }, rules), 'disk');
  assert.equal(categoryForAlert({ rule_id: 'host-heartbeat-stale' }, rules), 'availability');
  assert.equal(categoryForAlert({ rule_id: 'mystery' }, rules), 'other');
});
