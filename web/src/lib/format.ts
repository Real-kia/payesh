import type { DisplayState, PreviewServer } from '../preview/fixtures';
// Explicit extension so node's unit tests can load this module directly.
import { PREVIEW_MODE } from './env.ts';

/** Formats a decimal byte count kept as a string (uint64-safe) in SI units. */
export function formatBytes(value: string): string {
  try {
    const bytes = BigInt(value);
    const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
    let scaled = Number(bytes);
    let unit = 0;
    while (scaled >= 1000 && unit < units.length - 1) {
      scaled /= 1000;
      unit += 1;
    }
    return `${scaled >= 100 ? scaled.toFixed(0) : scaled.toFixed(1)} ${units[unit]}`;
  } catch {
    return '—';
  }
}

/** Integer percentage of counted/allowance, clamped to 0..100. */
export function percentage(counted: string, allowance: string): number {
  try {
    const result = (BigInt(counted) * 100n) / BigInt(allowance);
    return Math.max(0, Math.min(100, Number(result)));
  } catch {
    return 0;
  }
}

export function stateLabel(state: DisplayState): string {
  return state.replace('-', ' ');
}

export function metricValue(value: number | null): string {
  return value === null ? '—' : `${value.toFixed(2)}%`;
}

export function clampPercent(value: number | null | undefined): number {
  return Math.max(0, Math.min(100, value ?? 0));
}

/** Resource severity used for colouring gauges and bars. */
export function resourceLevel(value: number | null | undefined): 'normal' | 'warning' | 'critical' {
  if (value == null) return 'normal';
  return value >= 90 ? 'critical' : value >= 75 ? 'warning' : 'normal';
}

/** Latest bandwidth from the 15 minute history, or null when it is not current. */
export function currentNetworkRate(server: PreviewServer, direction: 'download' | 'upload'): number | null {
  if (server.freshnessState !== 'fresh') return null;
  const history = server.metricHistory?.ranges['15m'];
  if (!PREVIEW_MODE && (!history?.timestamps.length || Date.now() / 1000 - history.timestamps.at(-1)! > 90)) return null;
  const series = direction === 'download' ? history?.networkRx : history?.networkTx;
  return series?.at(-1) ?? null;
}

export function sumNetworkRate(fleet: PreviewServer[], direction: 'download' | 'upload'): number | null {
  const rates = fleet.filter((server) => server.connectionState === 'connected').map((server) => currentNetworkRate(server, direction));
  return rates.length && rates.every((rate) => rate !== null) ? rates.reduce<number>((sum, rate) => sum + (rate ?? 0), 0) : null;
}

export function sampleAge(server: PreviewServer): string {
  if (!server.latestMetricAt) return 'awaiting sample';
  const seconds = Math.max(0, Math.round((Date.now() - Date.parse(server.latestMetricAt)) / 1000));
  return seconds < 5 ? 'live now' : seconds < 60 ? `${seconds}s ago` : `${Math.floor(seconds / 60)}m ago`;
}

export function displayAddress(server: PreviewServer): string {
  if (server.address) return server.address;
  if (!PREVIEW_MODE && (server.role === 'standalone' || server.role === 'hub') && typeof window !== 'undefined') return window.location.hostname;
  return 'Address unavailable';
}

export function roleLabel(role: PreviewServer['role']): string {
  return role === 'node' ? 'Node' : role === 'cli-only' ? 'CLI' : 'Master';
}
