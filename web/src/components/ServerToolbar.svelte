<script lang="ts">
  import Icon from '../Icon.svelte';
  import { countryFlag } from '../lib/flags';
  import type { ServerFilterState } from '../lib/serverFilter';
  import type { PreviewServer } from '../preview/fixtures';

  let { servers, state = $bindable(), shown }: { servers: PreviewServer[]; state: ServerFilterState; shown: number } = $props();

  const tags = $derived([...new Set(servers.flatMap((server) => server.tags ?? []))].sort((a, b) => a.localeCompare(b)));
  const countries = $derived([...new Set(servers.map((server) => (server.countryCode ?? '').toUpperCase()).filter(Boolean))].sort());
  const filtered = $derived(state.status !== 'all' || !!state.tag || !!state.country || !!state.query.trim());
</script>

<div class="toolbar">
  <label class="search"><Icon name="search" size={14} /><input type="search" placeholder="Search name, IP or tag" aria-label="Search servers" bind:value={state.query} /></label>
  <select aria-label="Status" bind:value={state.status}>
    <option value="all">All statuses</option>
    <option value="attention">Needs attention</option>
    <option value="healthy">Healthy</option>
    <option value="offline">Offline</option>
  </select>
  {#if tags.length}
    <select aria-label="Tag" bind:value={state.tag}>
      <option value="">All tags</option>
      {#each tags as tag (tag)}<option value={tag}>{tag}</option>{/each}
    </select>
  {/if}
  {#if countries.length > 1}
    <select aria-label="Location" bind:value={state.country}>
      <option value="">All locations</option>
      {#each countries as country (country)}<option value={country}>{countryFlag(country)} {country}</option>{/each}
    </select>
  {/if}
  <select aria-label="Sort" bind:value={state.sort}>
    <option value="urgency">Most urgent</option>
    <option value="name">Name</option>
    <option value="cpu">CPU</option>
    <option value="memory">Memory</option>
    <option value="disk">Disk</option>
    <option value="download">Download</option>
    <option value="upload">Upload</option>
  </select>
  {#if filtered}
    <span class="count">{shown}/{servers.length}</span>
    <button class="button ghost small" type="button" onclick={() => (state = { ...state, query: '', status: 'all', tag: '', country: '' })}>Clear</button>
  {/if}
</div>

<style>
  .toolbar { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; margin: 0 0 14px; }
  .search { display: flex; flex: 1 1 220px; align-items: center; gap: 7px; min-width: 0; padding: 0 10px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface); color: var(--muted); }
  .search input { flex: 1; min-width: 0; height: 34px; padding: 0; border: 0; background: none; color: var(--ink); font: inherit; font-size: 13px; outline: none; }
  select { height: 36px; max-width: 170px; font-size: 13px; }
  .count { color: var(--muted); font-size: 12px; font-variant-numeric: tabular-nums; }
  @media (max-width: 520px) { select { flex: 1 1 calc(50% - 4px); max-width: none; } }
</style>
