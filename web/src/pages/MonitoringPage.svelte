<script lang="ts">
  import FleetSummary from '../components/FleetSummary.svelte';
  import MonitoringCard from '../components/MonitoringCard.svelte';
  import ServerToolbar from '../components/ServerToolbar.svelte';
  import LiveIndicator from '../components/LiveIndicator.svelte';
  import { filterServers, loadFilter, saveFilter } from '../lib/serverFilter';
  import type { PreviewServer } from '../preview/fixtures';

  let { servers, timezone, download, upload, onSelect, refreshing = false, updatedAt = null }: {
    servers: PreviewServer[];
    timezone: string;
    download: number | null;
    upload: number | null;
    onSelect: (server: PreviewServer) => void;
    refreshing?: boolean;
    updatedAt?: number | null;
  } = $props();

  let filter = $state(loadFilter('monitoring'));
  $effect(() => saveFilter('monitoring', filter));
  const visible = $derived(filterServers(servers, filter));
</script>

<section class="page" aria-labelledby="monitoring-title">
  <div class="page-heading">
    <div>
      <h1 id="monitoring-title" tabindex="-1">Server monitoring</h1>
    </div>
    <LiveIndicator {refreshing} {updatedAt} />
  </div>

  <FleetSummary {servers} {download} {upload} detailed />

  {#if servers.length === 0}
    <div class="state-panel"><h2>No servers to monitor</h2><p>Add a server to see its health here.</p></div>
  {:else}
    <ServerToolbar {servers} bind:state={filter} shown={visible.length} />
    {#if visible.length === 0}
      <div class="unavailable-panel"><strong>No servers match</strong></div>
    {:else}
      <div class="monitoring-grid">
        {#each visible as server (server.id)}
          <div class="cell"><MonitoringCard {server} {timezone} {onSelect} /></div>
        {/each}
      </div>
    {/if}
  {/if}
</section>

<style>
  .page-heading { align-items: center; }
  .monitoring-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 280px), 1fr)); gap: 12px; }
  .cell { display: grid; min-width: 0; }
</style>
