import type { DisplayState, PreviewServer } from '../preview/fixtures';
// Explicit extension so node's unit tests can load this module directly.
import { allAddresses, currentNetworkRate } from './format.ts';

export type StatusFilter = 'all' | 'attention' | 'healthy' | 'offline';
export type SortKey = 'urgency' | 'name' | 'cpu' | 'memory' | 'disk' | 'download' | 'upload';
export type ServerFilterState = { query: string; status: StatusFilter; tag: string; country: string; sort: SortKey };

export const defaultFilter: ServerFilterState = { query: '', status: 'all', tag: '', country: '', sort: 'urgency' };

const urgency: Record<DisplayState, number> = { failed: 0, unreachable: 1, stale: 2, installing: 3, pending: 4, unsupported: 5, disabled: 6, healthy: 7 };

const offline = (server: PreviewServer) => server.connectionState !== 'connected';

/** Search (name, every address, tags), filters and sort, applied in the browser. */
export function filterServers(servers: PreviewServer[], state: ServerFilterState): PreviewServer[] {
  const query = state.query.trim().toLowerCase();
  const metric = (server: PreviewServer): number => {
    if (state.sort === 'download' || state.sort === 'upload') return currentNetworkRate(server, state.sort) ?? -1;
    if (state.sort === 'cpu' || state.sort === 'memory' || state.sort === 'disk') return server.metrics[state.sort] ?? -1;
    return 0;
  };
  return servers
    .filter((server) => state.status === 'all'
      || (state.status === 'healthy' && server.displayState === 'healthy')
      || (state.status === 'attention' && server.displayState !== 'healthy')
      || (state.status === 'offline' && offline(server)))
    .filter((server) => !state.tag || (server.tags ?? []).some((tag) => tag.toLowerCase() === state.tag.toLowerCase()))
    .filter((server) => !state.country || (server.countryCode ?? '').toUpperCase() === state.country)
    .filter((server) => !query || `${server.name} ${allAddresses(server).join(' ')} ${(server.tags ?? []).join(' ')}`.toLowerCase().includes(query))
    .sort((a, b) => {
      if (state.sort === 'name') return a.name.localeCompare(b.name);
      if (state.sort === 'urgency') return urgency[a.displayState] - urgency[b.displayState] || a.name.localeCompare(b.name);
      return metric(b) - metric(a) || a.name.localeCompare(b.name);
    });
}

export function loadFilter(key: string): ServerFilterState {
  try {
    const saved = JSON.parse(localStorage.getItem(`payesh.filter.${key}`) ?? 'null');
    if (saved && typeof saved === 'object') return { ...defaultFilter, ...saved, query: '' };
  } catch { /* storage unavailable */ }
  return { ...defaultFilter };
}

export function saveFilter(key: string, state: ServerFilterState): void {
  try { localStorage.setItem(`payesh.filter.${key}`, JSON.stringify({ ...state, query: '' })); } catch { /* storage unavailable */ }
}
