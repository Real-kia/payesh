<script lang="ts">
  import { followLink } from '../lib/navigation';
  import { serverLabel } from '../lib/flags';
  import Icon from '../Icon.svelte';
  import StatusPill from './StatusPill.svelte';
  import Pager from './Pager.svelte';
  import { formatHeartbeatInTz } from '../timezone';
  import { allAddresses, displayAddress, nodeVersionOf, roleLabel, stateLabel } from '../lib/format';
  import type { PreviewServer } from '../preview/fixtures';

  let { servers, timezone, onSelect, nameHeading = 'Server & address', selectedIds = [], onSelectionChange, latestNode = '' }: {
    servers: PreviewServer[];
    timezone: string;
    onSelect: (server: PreviewServer) => void;
    nameHeading?: string;
    latestNode?: string;
    selectedIds?: string[];
    onSelectionChange?: (ids: string[]) => void;
  } = $props();

  // Only nodes can be updated or deleted, so the hub row has no checkbox.
  const selectable = $derived(!!onSelectionChange);
  const selectableIds = $derived(servers.filter((server) => server.role === 'node').map((server) => server.id));
  const allSelected = $derived(selectableIds.length > 0 && selectableIds.every((id) => selectedIds.includes(id)));
  const someSelected = $derived(selectedIds.some((id) => selectableIds.includes(id)));

  let selectionOrder = $state<string[]>([]);
  function rememberOrder() {
    if (!selectedIds.length) selectionOrder = sorted.map((server) => server.id);
  }
  function toggle(id: string) {
    rememberOrder();
    onSelectionChange?.(selectedIds.includes(id) ? selectedIds.filter((entry) => entry !== id) : [...selectedIds, id]);
  }
  function toggleAll() {
    rememberOrder();
    onSelectionChange?.(allSelected ? selectedIds.filter((id) => !selectableIds.includes(id)) : [...new Set([...selectedIds, ...selectableIds])]);
  }

  type SortKey = 'status' | 'name' | 'platform' | 'version' | 'contact';
  let sortKey = $state<SortKey | null>(null);
  let descending = $state(false);
  const urgency: Record<string, number> = { failed: 0, unreachable: 1, stale: 2, installing: 3, pending: 4, unsupported: 5, disabled: 6, healthy: 7 };
  const sorted = $derived.by(() => {
    const key = sortKey;
    const value = (server: PreviewServer): number | string | null => key === 'name' ? server.name.toLowerCase() : key === 'status' ? urgency[server.displayState] ?? 9 : key === 'platform' ? server.platform : key === 'version' ? nodeVersionOf(server) : server.lastHeartbeat;
    const ordered = !key ? servers : [...servers].sort((a, b) => {
      const left = value(a), right = value(b);
      if (left === null || right === null) return left === right ? 0 : left === null ? 1 : -1;
      const order = left < right ? -1 : left > right ? 1 : 0;
      return descending ? -order : order;
    });
    if (!selectedIds.length || !selectionOrder.length) return ordered;
    const ranks = new Map(selectionOrder.map((id, index) => [id, index]));
    return [...ordered].sort((a, b) => (ranks.get(a.id) ?? Infinity) - (ranks.get(b.id) ?? Infinity));
  });

  function isOlder(installed: string, latest: string): boolean {
    const parse = (value: string) => value.split(/[-+]/)[0].split('.').map((part) => Number.parseInt(part, 10) || 0);
    if (!/^\d+\.\d+\.\d+/.test(installed) || !/^\d+\.\d+\.\d+/.test(latest)) return false;
    const a = parse(installed), b = parse(latest);
    for (let i = 0; i < Math.max(a.length, b.length); i++) if ((a[i] ?? 0) !== (b[i] ?? 0)) return (a[i] ?? 0) < (b[i] ?? 0);
    return false;
  }

  const PAGE_SIZE = 25;
  let page = $state(0);
  const paged = $derived(sorted.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE));

  function sortBy(key: SortKey) {
    page = 0;
    selectionOrder = [];
    if (sortKey === key) descending = !descending;
    else { sortKey = key; descending = key === 'contact'; }
    if (selectedIds.length) selectionOrder = sorted.map((server) => server.id);
  }

  function detail(server: PreviewServer): string {
    return server.lastHeartbeat ? formatHeartbeatInTz(server.lastHeartbeat, timezone) : (server.freshnessReason || server.connectionState);
  }
</script>

<div class="table-container" class:selectable>
  <div class="table-header" role="group" aria-label="Sort servers">
    {#if selectable && selectableIds.length}
      <input class="row-check" type="checkbox" aria-label="Select all servers" checked={allSelected} indeterminate={someSelected && !allSelected} onchange={toggleAll} />
    {/if}
    {#each [['status', 'Status', ''], ['name', nameHeading, ''], ['platform', 'Platform', 'col-platform'], ['version', 'Agent / release', 'col-version'], ['contact', 'Last contact', 'col-contact']] as [key, label, extra] (key)}
      <button type="button" class={`sort ${extra}`} class:sorted={sortKey === key} aria-pressed={sortKey === key} aria-label={`Sort by ${label.toLowerCase()}${sortKey === key ? (descending ? ', descending' : ', ascending') : ''}`} onclick={() => sortBy(key as SortKey)}>
        {label}<span class="arrow" aria-hidden="true">{sortKey === key ? (descending ? '↓' : '↑') : '↕'}</span>
      </button>
    {/each}
    <span></span>
  </div>
  <div class="server-list">
    {#each paged as server, index (server.id)}
      <div class="row-wrap" class:checked={selectedIds.includes(server.id)}>
      {#if selectable && server.role === 'node'}
        <input class="row-check" type="checkbox" aria-label={`Select ${serverLabel(server)}`} checked={selectedIds.includes(server.id)} onchange={() => toggle(server.id)} />
      {/if}
      <a href={`/servers/${encodeURIComponent(server.id)}`} class="server-row" onclick={(event) => followLink(event, () => onSelect(server))} aria-label={`${serverLabel(server)}, ${stateLabel(server.displayState)}. Open details`}>
        <div class="col-status">{#if server.duplicateOf}<span class="duplicate-pill" title={`Same machine as ${server.duplicateOfName ?? 'another server'}`}>Duplicate</span>{:else}<StatusPill state={server.displayState} label={stateLabel(server.displayState)} />{/if}</div>
        <div class="col-name">
          <strong>{serverLabel(server)}<span class="role-chip">{roleLabel(server.role)}</span>{#if latestNode && server.role === 'node' && server.connectionState === 'connected' && isOlder(nodeVersionOf(server), latestNode)}<span class="role-chip update-chip" title={`Node v${nodeVersionOf(server)}; v${latestNode} is available`}>Update</span>{/if}</strong>
          <small class="mono">{displayAddress(server)}{#if allAddresses(server).length > 1}<span class="more-addresses" title={allAddresses(server).join(', ')}> +{allAddresses(server).length - 1}</span>{/if}</small>
        </div>
        <div class="col-platform"><span>{server.platform || '—'}</span><small class="mono">{server.architecture || '—'}</small></div>
        <div class="col-version mono tabular" title={server.role === 'node' ? `Node agent v${nodeVersionOf(server)}, installed from release v${server.version}` : `v${server.version}`}>{server.version && server.version !== '—' ? `v${server.role === 'node' ? nodeVersionOf(server) : server.version.replace(/^v/, '')}` : '—'}</div>
        <div class="col-contact">{detail(server)}</div>
        <div class="col-action" aria-hidden="true"><Icon name="chevron-right" size={16} /></div>
      </a>
      </div>
    {/each}
  </div>
</div>
<Pager total={sorted.length} bind:page pageSize={PAGE_SIZE} label="servers" />

<style>
  .table-container {
    overflow: hidden;
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
  }
  .table-header, .server-row {
    display: grid;
    grid-template-columns: 116px minmax(180px, 2fr) minmax(110px, 1fr) 110px minmax(130px, 1fr) 16px;
    gap: 16px;
    align-items: center;
    padding: 0 20px;
  }
  .table-header {
    position: relative;
    min-height: 36px;
    border-bottom: 1px solid var(--line);
    color: var(--muted);
    font-size: 12px;
    font-weight: 500;
  }
  .sort { display: inline-flex; align-items: center; gap: 4px; padding: 0; border: 0; background: none; color: inherit; font: inherit; letter-spacing: inherit; text-align: left; text-transform: inherit; cursor: pointer; }
  .sort:hover, .sort.sorted { color: var(--ink); }
  .arrow { opacity: 0; font-size: 12px; transition: opacity var(--transition-fast); }
  .sort:hover .arrow, .sort:focus-visible .arrow { opacity: 0.5; }
  .sort.sorted .arrow { opacity: 1; }
  .server-list { display: flex; flex-direction: column; }
  .row-wrap { position: relative; border-bottom: 1px solid var(--line-light); }
  .row-wrap:last-child { border-bottom: 0; }
  .row-wrap.checked { background: var(--surface-muted); }
  .row-check { position: absolute; z-index: 1; left: 18px; top: 50%; width: 16px; height: 16px; margin: -8px 0 0; accent-color: var(--accent); cursor: pointer; }
  .selectable .table-header, .selectable .server-row { padding-left: 52px; }
  .server-row {
    text-decoration: none;
    position: relative;
    width: 100%;
    padding-top: var(--row-pad-y);
    padding-bottom: var(--row-pad-y);
    border: 0;
    border-bottom: 1px solid var(--line-light);
    background: transparent;
    color: var(--ink);
    text-align: left;
    transition: background-color var(--transition-fast);
  }
  .server-row:last-child { border-bottom: 0; }
  .server-row:hover, .server-row:focus-visible { background: var(--surface-hover); }
  .server-row:focus-visible { outline-offset: -2px; }

  .col-name { min-width: 0; }
  .col-name strong { display: flex; align-items: center; gap: 8px; font-size: 13.5px; font-weight: 600; }
  .col-name small { display: block; margin-top: 3px; overflow: hidden; color: var(--muted); font-size: 12px; text-overflow: ellipsis; white-space: nowrap; }
  .role-chip {
    padding: 1px 6px;
    border: 1px solid var(--line);
    border-radius: var(--radius-xs);
    color: var(--muted);
    font-size: 11.5px;
    font-weight: 500;
  }

  .col-platform, .col-contact, .col-version { min-width: 0; color: var(--ink-secondary); font-size: 12px; }
  .col-platform small { display: block; margin-top: 3px; color: var(--muted); }
  .col-contact { color: var(--muted); }

  .col-action { display: flex; justify-content: flex-end; color: var(--muted); opacity: 0.5; transition: opacity var(--transition-fast); }
  .server-row:hover .col-action { opacity: 1; }

  @media (max-width: 1100px) {
    .table-header, .server-row { grid-template-columns: 100px minmax(150px, 2fr) minmax(100px, 1fr) 100px 16px; gap: 12px; }
    .col-contact { display: none; }
  }
  @media (max-width: 760px) {
    .table-header { display: none; }
    .server-row { grid-template-columns: minmax(0, 1fr) auto; grid-template-areas: 'name action' 'status version' 'platform platform'; gap: 8px 12px; padding: 14px 16px; }
    .col-name { grid-area: name; }
    .col-name strong { flex-wrap: wrap; }
    .col-status { grid-area: status; }
    .col-action { grid-area: action; }
    .col-version { grid-area: version; }
    .col-platform { grid-area: platform; display: flex; gap: 8px; align-items: baseline; }
    .col-platform small { margin: 0; }
  }
  .duplicate-pill { display: inline-flex; align-items: center; padding: 2px 8px; border-radius: var(--radius-sm); border: 1px solid var(--line-strong, var(--line)); color: var(--muted); font-size: 12px; font-weight: 550; }
  .update-chip { border-color: var(--accent); color: var(--accent); }
  .more-addresses { color: var(--muted); font-weight: 600; }
</style>
