<script lang="ts">
  import Icon from '../Icon.svelte';
  import StatusPill from '../components/StatusPill.svelte';
  import LoadingSkeleton from '../components/LoadingSkeleton.svelte';
  import { formatDateTimeInTz } from '../timezone';
  import type { AlertState } from '../api';
  import type { PreviewServer } from '../preview/fixtures';

  let { alerts, loadState, error, servers, timezone, onReload }: {
    alerts: AlertState[];
    loadState: 'ready' | 'loading' | 'empty' | 'error';
    error: string;
    servers: PreviewServer[];
    timezone: string;
    onReload: () => void;
  } = $props();

  let page = $state(0);
  let pageSize = $state(25);
  let stateFilter = $state<'all' | 'firing' | 'pending' | 'recovered'>('all');

  const filtered = $derived(alerts.filter((alert) => stateFilter === 'all' || (stateFilter === 'recovered' ? alert.state !== 'firing' && alert.state !== 'pending' : alert.state === stateFilter)));
  const pageCount = $derived(Math.max(1, Math.ceil(filtered.length / pageSize)));
  const paged = $derived(filtered.slice(page * pageSize, (page + 1) * pageSize));
  const counts = $derived({
    firing: alerts.filter((alert) => alert.state === 'firing').length,
    pending: alerts.filter((alert) => alert.state === 'pending').length
  });
  $effect(() => { if (page > pageCount - 1) page = pageCount - 1; });

  function serverName(id?: string): string {
    return servers.find((server) => server.id === id)?.name || id || 'Fleet-wide';
  }
  function ruleLabel(rule: string): string {
    return rule.replace(/[-_]+/g, ' ').replace(/^\w/, (c) => c.toUpperCase());
  }
  function tone(state: string): string {
    return state === 'firing' ? 'failed' : state === 'pending' ? 'stale' : 'healthy';
  }
</script>

<section class="page" aria-labelledby="alerts-title">
  <div class="page-heading">
    <div>
      <h1 id="alerts-title" tabindex="-1">Alerts</h1>
      <p class="lede">Rule states evaluated by the hub.</p>
    </div>
    <button class="button ghost" type="button" onclick={onReload} disabled={loadState === 'loading'}><Icon name="refresh" size={14} /><span>Reload</span></button>
  </div>

  {#if loadState === 'loading'}
    <LoadingSkeleton cards={0} rows={4} label="Loading alert states" />
  {:else if loadState === 'error'}
    <div class="state-panel error-state">
      <div class="state-icon"><Icon name="alert-triangle" size={24} /></div>
      <h2>Could not load alerts</h2>
      <p>{error}</p>
      <button class="button primary" type="button" onclick={onReload}>Retry</button>
    </div>
  {:else if loadState === 'empty'}
    <div class="state-panel">
      <div class="state-icon positive"><Icon name="check" size={24} /></div>
      <h2>All systems operating normally</h2>
      <p>No active incidents or firing alert rules across the fleet.</p>
    </div>
  {:else}
    <div class="toolbar">
      <div class="segmented" role="group" aria-label="Filter alerts by state">
        <button type="button" aria-pressed={stateFilter === 'all'} onclick={() => { stateFilter = 'all'; page = 0; }}>All <span class="n">{alerts.length}</span></button>
        <button type="button" aria-pressed={stateFilter === 'firing'} onclick={() => { stateFilter = 'firing'; page = 0; }}><i class="dot firing"></i>Firing <span class="n">{counts.firing}</span></button>
        <button type="button" aria-pressed={stateFilter === 'pending'} onclick={() => { stateFilter = 'pending'; page = 0; }}><i class="dot pending"></i>Pending <span class="n">{counts.pending}</span></button>
        <button type="button" aria-pressed={stateFilter === 'recovered'} onclick={() => { stateFilter = 'recovered'; page = 0; }}>Recovered <span class="n">{alerts.length - counts.firing - counts.pending}</span></button>
      </div>
      <label class="page-size">Per page
        <select bind:value={pageSize} onchange={() => (page = 0)}>
          <option value={10}>10</option><option value={25}>25</option><option value={50}>50</option><option value={100}>100</option>
        </select>
      </label>
    </div>

    {#if paged.length === 0}
      <div class="unavailable-panel"><strong>No alerts in this state</strong><span>Choose another filter.</span></div>
    {:else}
      <div class="alert-list stagger">
        {#each paged as alert, index (alert.id)}
          <article class={`alert-row ${alert.state}`} style:--i={Math.min(index, 10)}>
            <StatusPill state={tone(alert.state)} label={alert.state} />
            <div class="what">
              <strong>{ruleLabel(alert.rule_id)}</strong>
              <small><Icon name="server" size={11} /> {serverName(alert.server_id)}</small>
            </div>
            <span class="when"><Icon name="clock" size={12} /> {alert.last_observation ? formatDateTimeInTz(alert.last_observation, timezone) : 'Awaiting observation'}</span>
            {#if alert.last_value !== undefined}
              <span class="value"><strong class="tabular">{alert.last_value.toFixed(2)}</strong><small>last value</small></span>
            {:else}<span></span>{/if}
          </article>
        {/each}
      </div>
    {/if}

    <div class="pager">
      <span class="muted">Showing {filtered.length === 0 ? 0 : page * pageSize + 1}–{Math.min(filtered.length, (page + 1) * pageSize)} of {filtered.length}</span>
      {#if pageCount > 1}
        <div class="pager-buttons">
          <button class="button ghost small" type="button" disabled={page === 0} onclick={() => page--}>Previous</button>
          <span class="muted tabular">Page {page + 1} of {pageCount}</span>
          <button class="button ghost small" type="button" disabled={page >= pageCount - 1} onclick={() => page++}>Next</button>
        </div>
      {/if}
    </div>
  {/if}
</section>

<style>
  .toolbar { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 14px; }
  .n { padding: 0 6px; border-radius: var(--radius-pill); background: var(--surface-muted); color: var(--muted); font-size: 11px; font-variant-numeric: tabular-nums; }
  .dot { width: 6px; height: 6px; border-radius: 50%; }
  .dot.firing { background: var(--danger); }
  .dot.pending { background: var(--warning); }
  .page-size { display: inline-flex; align-items: center; gap: 8px; color: var(--muted); font-size: 12px; }
  .page-size select { padding-top: 5px; padding-bottom: 5px; }

  .alert-list { overflow: hidden; border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface); box-shadow: var(--shadow); }
  .alert-row {
    position: relative;
    display: grid;
    grid-template-columns: 110px minmax(0, 1.5fr) minmax(0, 1fr) 90px;
    gap: 16px;
    align-items: center;
    padding: var(--row-pad-y) 20px;
    border-bottom: 1px solid var(--line-light);
  }
  .alert-row:last-child { border-bottom: 0; }
  .alert-row.firing { background: linear-gradient(90deg, var(--danger-bg), transparent 40%); }
  .alert-row.firing::before { content: ''; position: absolute; left: 0; top: 0; bottom: 0; width: 3px; background: var(--danger); }
  .what { min-width: 0; }
  .what strong { display: block; overflow: hidden; font-size: 13.5px; text-overflow: ellipsis; white-space: nowrap; }
  .what small, .when { display: inline-flex; align-items: center; gap: 5px; color: var(--muted); font-size: 12px; }
  .value { display: grid; justify-items: end; }
  .value small { color: var(--muted); font-size: 11px; }

  .pager { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; margin-top: 14px; font-size: 12.5px; }
  .pager-buttons { display: flex; align-items: center; gap: 10px; }

  @media (max-width: 760px) {
    .alert-row { grid-template-columns: minmax(0, 1fr) auto; }
    .when { grid-column: 1 / -1; }
  }
</style>
