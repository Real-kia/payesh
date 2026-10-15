import type { RelayInfo } from '../api';

export type RelayOption = { id: string; name: string; address: string; countryCode?: string; ready: boolean; nodes: number };

type ServerLike = { id: string; name: string; address?: string; role: string; connectionState: string; countryCode?: string };

/**
 * Relay choices: nodes that already relay come first (nothing to set up),
 * then other connected nodes with a usable address.
 */
export function relayOptions(servers: ServerLike[], relays: RelayInfo[], exclude?: string | null): RelayOption[] {
  const ready = new Map(relays.map((relay) => [relay.server_id, relay]));
  return servers
    .filter((server) => server.id !== exclude && server.role === 'node' && server.connectionState === 'connected' && !!server.address && !/\s/.test(server.address))
    .map((server) => ({ id: server.id, name: server.name, address: server.address ?? '', countryCode: server.countryCode, ready: ready.has(server.id), nodes: ready.get(server.id)?.nodes ?? 0 }))
    .sort((a, b) => Number(b.ready) - Number(a.ready) || b.nodes - a.nodes || a.name.localeCompare(b.name));
}

const LAST_RELAY_KEY = 'payesh.lastRelay';

export function rememberRelay(id: string): void {
  try { localStorage.setItem(LAST_RELAY_KEY, id); } catch { /* storage unavailable */ }
}

/** The relay to preselect: the last one used if still offered, else the first ready one. */
export function preferredRelay(options: RelayOption[]): string {
  let last = '';
  try { last = localStorage.getItem(LAST_RELAY_KEY) ?? ''; } catch { /* storage unavailable */ }
  if (last && options.some((option) => option.id === last)) return last;
  return options.find((option) => option.ready)?.id ?? '';
}
