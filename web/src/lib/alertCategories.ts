import type { AlertRule, AlertState } from '../api';

export type AlertCategory = 'availability' | 'cpu' | 'memory' | 'disk' | 'network' | 'other';

export const alertCategories: { id: AlertCategory; label: string; icon: string }[] = [
  { id: 'availability', label: 'Availability', icon: 'radio' },
  { id: 'cpu', label: 'CPU', icon: 'cpu' },
  { id: 'memory', label: 'Memory', icon: 'memory' },
  { id: 'disk', label: 'Disk', icon: 'hard-drive' },
  { id: 'network', label: 'Network', icon: 'globe' },
  { id: 'other', label: 'Other', icon: 'activity' }
];

const known = new Set<string>(alertCategories.map((category) => category.id));

/** Classifies a metric name or rule identifier by the signal it watches. */
export function categoryForMetric(metric: string): AlertCategory {
  const value = metric.toLowerCase();
  if (/unreachable|heartbeat|availability|offline/.test(value)) return 'availability';
  if (/cpu|load/.test(value)) return 'cpu';
  if (/memory|swap|mem\b/.test(value)) return 'memory';
  if (/disk|inode|filesystem/.test(value)) return 'disk';
  if (/traffic|network|bandwidth|net\b/.test(value)) return 'network';
  return 'other';
}

/** Prefers the rule's group key, then its metric, then the identifier text. */
export function categoryForRule(rule: Pick<AlertRule, 'expression' | 'group_key' | 'id'>): AlertCategory {
  if (rule.group_key && known.has(rule.group_key)) return rule.group_key as AlertCategory;
  const metric = rule.expression.trim().split(/\s*(?:>=|<=|==|!=|>|<)/)[0];
  const fromMetric = categoryForMetric(metric);
  return fromMetric === 'other' ? categoryForMetric(rule.id) : fromMetric;
}

export function categoryForAlert(alert: Pick<AlertState, 'rule_id'>, rules: AlertRule[]): AlertCategory {
  const rule = rules.find((item) => item.id === alert.rule_id);
  return rule ? categoryForRule(rule) : categoryForMetric(alert.rule_id);
}
