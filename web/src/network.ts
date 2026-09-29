import type { MetricSample } from './api';

// Counter differences use BigInt so hosts with large lifetime totals retain
// precise rates. Restarts, lost samples, and invalid counters break the series.
export function networkRates(samples: MetricSample[], name: string): Array<number | null> {
  return samples.map((sample, index) => {
    const previous = samples[index - 1];
    if (!previous || sample.collector_epoch !== previous.collector_epoch || sample.server_id !== previous.server_id) return null;
    if ((sample.validity?.[name] && sample.validity[name] !== 'valid') || (previous.validity?.[name] && previous.validity[name] !== 'valid')) return null;
    const currentCounter = sample.counters?.[name];
    const previousCounter = previous.counters?.[name];
    if (!currentCounter || !previousCounter || !/^\d+$/.test(currentCounter) || !/^\d+$/.test(previousCounter)) return null;
    try {
      if (BigInt(sample.sequence) !== BigInt(previous.sequence) + 1n) return null;
      const current = BigInt(currentCounter);
      const before = BigInt(previousCounter);
      const seconds = (Date.parse(sample.observed_at) - Date.parse(previous.observed_at)) / 1000;
      if (current < before || !Number.isFinite(seconds) || seconds <= 0) return null;
      const rate = Number(current - before) / seconds;
      return Number.isFinite(rate) ? rate : null;
    } catch { return null; }
  });
}

export function formatNetworkRate(bytesPerSecond: number | null | undefined): string {
  return bytesPerSecond == null || !Number.isFinite(bytesPerSecond) ? '—' : `${(bytesPerSecond * 8 / 1_000_000).toFixed(2)} Mbit/s`;
}

export function networkRollupRate(rollup: Record<string, unknown> | undefined): number | null {
  if (!rollup || rollup.coverage !== 'complete' || typeof rollup.counter_delta !== 'string' || !/^\d+$/.test(rollup.counter_delta)) return null;
  const seconds = rollup.observed_seconds;
  if (typeof seconds !== 'number' || !Number.isFinite(seconds) || seconds <= 0) return null;
  const rate = Number(BigInt(rollup.counter_delta)) / seconds;
  return Number.isFinite(rate) ? rate : null;
}
