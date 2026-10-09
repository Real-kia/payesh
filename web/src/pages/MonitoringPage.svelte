<script lang="ts">
  import FleetSummary from '../components/FleetSummary.svelte';
  import MonitoringCard from '../components/MonitoringCard.svelte';
  import type { DisplayState, PreviewServer } from '../preview/fixtures';

  let { servers, timezone, download, upload, onSelect }: {
    servers: PreviewServer[];
    timezone: string;
    download: number | null;
    upload: number | null;
    onSelect: (server: PreviewServer) => void;
  } = $props();

  type Filter = 'all' | 'attention' | 'healthy';
  let filter = $state<Filter>('all');
  let query = $state('');

  // Most urgent first, so problems are at the top of a large fleet.
  const urgency: Record<DisplayState, number> = { failed: 0, unreachable: 1, stale: 2, installing: 3, pending: 4, unsupported: 5, disabled: 6, healthy: 7 };
  const visible = $derived(
    servers
      .filter((server) => filter === 'all' || (filter === 'healthy' ? server.displayState === 'healthy' : server.displayState !== 'healthy'))
      .filter((server) => !query.trim() || `${server.name} ${server.address ?? ''} ${server.platform}`.toLowerCase().includes(query.trim().toLowerCase()))
      .sort((a, b) => urgency[a.displayState] - urgency[b.displayState] || a.name.localeCompare(b.name))
  );
  const attentionCount = $derived(servers.filter((server) => server.displayState !== 'healthy').length);
</script>

<section class="page" aria-labelledby="monitoring-title">
  <div class="page-heading">
    <div>
      <h1 id="monitoring-title" tabindex="-1">Server monitoring</h1>
      <p class="lede">Live resources per server, most urgent first.</p>
    </div>
  </div>

  <FleetSummary {servers} {download} {upload} detailed />

  {#if servers.length === 0}
    <div class="state-panel"><h2>No servers to monitor</h2><p>Add a server to see its health here.</p></div>
  {:else}
    <div class="toolbar">
      <div class="segmented" role="group" aria-label="Filter servers">
        <button type="button" aria-pressed={filter === 'all'} onclick={() => (filter = 'all')}>All <span class="n">{servers.length}</span></button>
        <button type="button" aria-pressed={filter === 'attention'} onclick={() => (filter = 'attention')}>Needs attention <span class="n">{attentionCount}</span></button>
        <button type="button" aria-pressed={filter === 'healthy'} onclick={() => (filter = 'healthy')}>Healthy <span class="n">{servers.length - attentionCount}</span></button>
      </div>
      <input class="search" type="search" placeholder="Filter by name, address or OS" aria-label="Filter servers by name" bind:value={query} />
    </div>
    {#if visible.length === 0}
      <div class="unavailable-panel"><strong>No servers match</strong><span>Change the filter or search to see more servers.</span></div>
    {:else}
      <div class="monitoring-grid stagger">
        {#each visible as server, index (server.id)}
          <div class="cell" style:--i={Math.min(index, 10)}><MonitoringCard {server} {timezone} {onSelect} /></div>
        {/each}
      </div>
    {/if}
  {/if}
</section>

<style>
  .toolbar { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 16px; }
  .n { padding: 0 6px; border-radius: var(--radius-pill); background: var(--surface-muted); color: var(--muted); font-size: 11px; font-variant-numeric: tabular-nums; }
  .segmented button[aria-pressed='true'] .n { background: var(--accent-bg); color: var(--accent); }
  .search { width: min(280px, 100%); }
  .monitoring-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 430px), 1fr)); gap: var(--gap); }
  .cell { display: grid; min-width: 0; }
  @media (max-width: 640px) { .segmented { width: 100%; overflow-x: auto; } .search { width: 100%; } }
</style>
