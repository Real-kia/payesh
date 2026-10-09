<script lang="ts">
  import Icon from '../Icon.svelte';
  import CircleChart from '../CircleChart.svelte';
  import StatusPill from './StatusPill.svelte';
  import { formatNetworkRate } from '../network';
  import { formatHeartbeatInTz } from '../timezone';
  import { clampPercent, currentNetworkRate, displayAddress, metricValue, resourceLevel, roleLabel, stateLabel } from '../lib/format';
  import type { PreviewServer } from '../preview/fixtures';

  let { servers, timezone, onSelect, nameHeading = 'Server & address' }: {
    servers: PreviewServer[];
    timezone: string;
    onSelect: (server: PreviewServer) => void;
    nameHeading?: string;
  } = $props();

  type SortKey = 'status' | 'name' | 'cpu' | 'memory' | 'disk';
  let sortKey = $state<SortKey | null>(null);
  let descending = $state(false);

  // Problems first for status; metrics high to low; unknown values always last.
  const urgency: Record<string, number> = { failed: 0, unreachable: 1, stale: 2, installing: 3, pending: 4, unsupported: 5, disabled: 6, healthy: 7 };
  const sorted = $derived.by(() => {
    if (!sortKey) return servers;
    const key = sortKey;
    const value = (server: PreviewServer): number | string | null => key === 'name' ? server.name.toLowerCase() : key === 'status' ? urgency[server.displayState] ?? 9 : server.connectionState === 'connected' ? server.metrics[key] : null;
    return [...servers].sort((a, b) => {
      const left = value(a), right = value(b);
      if (left === null || right === null) return left === right ? 0 : left === null ? 1 : -1;
      const order = left < right ? -1 : left > right ? 1 : 0;
      return descending ? -order : order;
    });
  });

  function sortBy(key: SortKey) {
    if (sortKey === key) {
      if (descending === (key === 'cpu' || key === 'memory' || key === 'disk')) descending = !descending;
      else { sortKey = null; descending = false; }
    } else {
      sortKey = key;
      descending = key === 'cpu' || key === 'memory' || key === 'disk';
    }
  }

  function detail(server: PreviewServer): string {
    return server.lastHeartbeat ? formatHeartbeatInTz(server.lastHeartbeat, timezone) : (server.freshnessReason || server.connectionState);
  }
</script>

<div class="table-container">
  <div class="table-header" role="group" aria-label="Sort servers">
    {#each [['status', 'Status', ''], ['name', nameHeading, ''], ['cpu', 'CPU', ''], ['memory', 'Memory', ''], ['disk', 'Disk', 'col-disk']] as [key, label, extra] (key)}
      <button type="button" class={`sort ${extra}`} class:sorted={sortKey === key} aria-pressed={sortKey === key} aria-label={`Sort by ${label.toLowerCase()}${sortKey === key ? (descending ? ', descending' : ', ascending') : ''}`} onclick={() => sortBy(key as SortKey)}>
        {label}<span class="arrow" aria-hidden="true">{sortKey === key ? (descending ? '↓' : '↑') : '↕'}</span>
      </button>
    {/each}
    <span class="col-rxtx">Rx / Tx</span>
    <span></span>
  </div>
  <div class="server-list stagger">
    {#each sorted as server, index (server.id)}
      {@const online = server.connectionState === 'connected'}
      <button class="server-row" type="button" style:--i={Math.min(index, 12)} onclick={() => onSelect(server)} aria-label={`${server.name}, ${stateLabel(server.displayState)}. Open details`}>
        <div class="col-status"><StatusPill state={server.displayState} label={stateLabel(server.displayState)} /></div>
        <div class="col-name">
          <strong>{server.name}<span class="role-chip">{roleLabel(server.role)}</span></strong>
          <small class="mono">{displayAddress(server)} · {detail(server)}</small>
        </div>
        <div class="col-metric">
          {#if online}
            <strong class="tabular">{metricValue(server.metrics.cpu)}</strong>
            <div class={`microbar ${resourceLevel(server.metrics.cpu)}`}><span style:width={`${clampPercent(server.metrics.cpu)}%`}></span></div>
          {:else}<span class="faint">—</span>{/if}
        </div>
        <div class="col-metric">
          {#if online}
            <strong class="tabular">{metricValue(server.metrics.memory)}</strong>
            <div class={`microbar memory ${resourceLevel(server.metrics.memory)}`}><span style:width={`${clampPercent(server.metrics.memory)}%`}></span></div>
          {:else}<span class="faint">—</span>{/if}
        </div>
        <div class="col-metric col-disk">
          {#if online}
            <div class="disk-cell">
              <CircleChart value={server.metrics.disk} size={26} strokeWidth={11} showValue={false} color="var(--blue)" />
              <strong class="tabular">{metricValue(server.metrics.disk)}</strong>
            </div>
          {:else}<span class="faint">—</span>{/if}
        </div>
        <div class="col-rxtx">
          {#if online}
            <span class="rates tabular">
              <span class="down">↓ {formatNetworkRate(currentNetworkRate(server, 'download'))}</span>
              <span class="up">↑ {formatNetworkRate(currentNetworkRate(server, 'upload'))}</span>
            </span>
          {:else}<span class="faint">—</span>{/if}
        </div>
        <div class="col-action" aria-hidden="true"><Icon name="chevron-right" size={16} /></div>
      </button>
    {/each}
  </div>
</div>

<style>
  .table-container {
    overflow: hidden;
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
    box-shadow: var(--shadow);
  }
  .table-header, .server-row {
    display: grid;
    grid-template-columns: 128px minmax(180px, 2fr) 92px 92px 104px minmax(150px, 1.2fr) 24px;
    gap: 16px;
    align-items: center;
    padding: 0 20px;
  }
  .table-header {
    min-height: 40px;
    border-bottom: 1px solid var(--line);
    background: var(--surface-muted);
    color: var(--muted);
    font-size: 11.5px;
    font-weight: 600;
    letter-spacing: 0.03em;
    text-transform: uppercase;
  }
  .sort { display: inline-flex; align-items: center; gap: 4px; padding: 0; border: 0; background: none; color: inherit; font: inherit; letter-spacing: inherit; text-align: left; text-transform: inherit; cursor: pointer; }
  .sort:hover, .sort.sorted { color: var(--ink); }
  .arrow { opacity: 0; font-size: 11px; transition: opacity var(--transition-fast); }
  .sort:hover .arrow, .sort:focus-visible .arrow { opacity: 0.5; }
  .sort.sorted .arrow { opacity: 1; color: var(--accent); }
  .server-list { display: flex; flex-direction: column; }
  .server-row {
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
  .server-row::before {
    content: '';
    position: absolute;
    left: 0;
    top: 8px;
    bottom: 8px;
    width: 3px;
    border-radius: 0 3px 3px 0;
    background: var(--accent);
    transform: scaleY(0);
    transition: transform var(--dur) var(--ease-out);
  }
  .server-row:last-child { border-bottom: 0; }
  .server-row:hover, .server-row:focus-visible { background: var(--surface-hover); }
  .server-row:hover::before, .server-row:focus-visible::before { transform: scaleY(1); }
  .server-row:focus-visible { outline-offset: -2px; }

  .col-name { min-width: 0; }
  .col-name strong { display: flex; align-items: center; gap: 8px; font-size: 14px; font-weight: 600; }
  .col-name small { display: block; margin-top: 3px; overflow: hidden; color: var(--muted); font-size: 11.5px; text-overflow: ellipsis; white-space: nowrap; }
  .role-chip {
    padding: 1px 6px;
    border: 1px solid var(--line);
    border-radius: var(--radius-xs);
    color: var(--muted);
    font-size: 10.5px;
    font-weight: 600;
    letter-spacing: 0.02em;
  }

  .col-metric { display: flex; flex-direction: column; gap: 5px; min-width: 0; }
  .col-metric strong { font-size: 13.5px; font-weight: 600; }
  .microbar { height: 4px; overflow: hidden; border-radius: var(--radius-pill); background: var(--surface-muted); }
  .microbar span { display: block; height: 100%; border-radius: inherit; background: var(--accent); transition: width var(--dur-slow) var(--ease-out); }
  .microbar.memory span { background: var(--purple); }
  .microbar.warning span { background: var(--warning); }
  .microbar.critical span { background: var(--danger); }
  .disk-cell { display: inline-flex; align-items: center; gap: 8px; }
  .disk-cell strong { font-size: 13.5px; font-weight: 600; }

  .rates { display: flex; flex-wrap: wrap; gap: 2px 10px; font-size: 12px; font-weight: 500; }
  .down { color: var(--accent); }
  .up { color: var(--blue); }

  .col-action { display: flex; justify-content: flex-end; color: var(--muted); transition: transform var(--dur) var(--ease-out), color var(--transition-fast); }
  .server-row:hover .col-action { color: var(--accent); transform: translateX(3px); }

  @media (max-width: 1100px) {
    .table-header, .server-row { grid-template-columns: 120px minmax(150px, 1.5fr) 84px 84px 100px 20px; }
    .col-rxtx { display: none; }
  }
  @media (max-width: 760px) {
    .table-header { display: none; }
    .server-row {
      grid-template-columns: minmax(0, 1fr) auto auto;
      grid-template-areas: 'name name action' 'status cpu action';
      gap: 8px 12px;
      padding: 14px 16px;
    }
    .col-name { grid-area: name; }
    .col-status { grid-area: status; }
    .col-action { grid-area: action; }
    .col-metric:not(.col-disk):nth-child(3) { grid-area: cpu; flex-direction: row; align-items: center; }
    .col-metric:nth-child(3) .microbar { width: 54px; }
    .col-metric:nth-child(4), .col-disk { display: none; }
  }
</style>
