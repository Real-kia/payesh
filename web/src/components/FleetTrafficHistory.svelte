<script lang="ts">
  import { ApiError, apiClient, type FleetTrafficHistory } from '../api';
  import { PREVIEW_MODE } from '../lib/env';
  import { formatBytes, sumNetworkRate } from '../lib/format';
  import { formatNetworkRate } from '../network';
  import type { PreviewServer } from '../preview/fixtures';

  let { servers, timezone, theme, onAuthExpired, onOpenStorage }: { servers: PreviewServer[]; timezone: string; theme: string; onAuthExpired: () => void; onOpenStorage?: () => void } = $props();

  const loadChart = () => import('../TrafficChart.svelte');
  const hour = 3600000;
  const ranges = [[24, '24 hours'], [168, '7 days'], [720, '30 days']] as const;
  let tag = $state('');
  let savedTags = $state<string[]>([]);
  const tagOptions = $derived.by(() => {
    const labels = new Map(savedTags.map((label) => [label, label]));
    for (const server of servers) {
      for (const label of server.tags ?? []) labels.set(label.toLowerCase(), label);
    }
    if (tag && !labels.has(tag)) labels.set(tag, tag);
    return [...labels].sort(([, a], [, b]) => a.localeCompare(b));
  });
  const taggedServers = $derived(servers.filter((server) => server.tags?.some((label) => label.toLowerCase() === tag)));
  let range = $state(24);
  let loaded = $state<{ tag: string; history: FleetTrafficHistory } | null>(null);
  const history = $derived(loaded?.tag === tag ? loaded.history : null);
  let error = $state('');
  let retry = $state(0);

  $effect(() => {
    if (PREVIEW_MODE) return;
    const selectedTag = tag;
    const controller = new AbortController();
    error = '';
    async function load() {
      try {
        const result = await apiClient.getFleetTraffic(controller.signal, selectedTag);
        if (controller.signal.aborted) return;
        loaded = { tag: selectedTag, history: result };
        if (!selectedTag) savedTags = result.tags ?? [];
        error = '';
      } catch (cause) {
        if (controller.signal.aborted) return;
        if (cause instanceof ApiError && cause.authExpired) onAuthExpired();
        error = cause instanceof ApiError ? cause.message : 'Fleet traffic history could not be loaded.';
      }
    }
    void retry;
    void load();
    const timer = window.setInterval(() => void load(), 60000);
    return () => { controller.abort(); window.clearInterval(timer); };
  });

  const hasSavedTraffic = $derived(history?.hours.some((entry) => entry.download_bytes !== null || entry.upload_bytes !== null) ?? false);

  const canPickRange = $derived(!!loaded && (loaded.history.enabled || hasSavedTraffic || (!!tag && savedTags.includes(tag))));

  const view = $derived.by(() => {
    const end = Date.parse(history?.end ?? '');
    const points = new Map(history?.hours.map((entry) => [Date.parse(entry.start), entry]));
    const timestamps: number[] = [];
    const networkRx: (number | null)[] = [];
    const networkTx: (number | null)[] = [];
    let download = 0n;
    let upload = 0n;
    let recorded = 0;
    let complete = 0;
    if (Number.isFinite(end)) {
      for (let start = end - range * hour; start < end; start += hour) {
        const entry = points.get(start);
        const rx = entry?.download_bytes ?? null;
        const tx = entry?.upload_bytes ?? null;
        timestamps.push(start / 1000);
        networkRx.push(rx === null ? null : Number(rx));
        networkTx.push(tx === null ? null : Number(tx));
        if (rx !== null) download += BigInt(rx);
        if (tx !== null) upload += BigInt(tx);
        if (rx !== null || tx !== null) recorded++;
        if (entry && rx !== null && tx !== null && entry.download_servers === entry.server_count && entry.upload_servers === entry.server_count) complete++;
      }
    }
    return { data: { timestamps, networkRx, networkTx }, recorded, complete,
      download: networkRx.some((value) => value !== null) ? download : null,
      upload: networkTx.some((value) => value !== null) ? upload : null };
  });
</script>

<article class="panel fleet-traffic">
  <div class="panel-heading">
    <div><h2>Fleet traffic history</h2><span class="muted">Recorded download and upload per hour · {timezone}</span></div>
    <div class="filters">
      {#if !PREVIEW_MODE && tagOptions.length}
        <label>Server tag
          <select bind:value={tag}>
            <option value="">All servers</option>
            {#each tagOptions as [key, label] (key)}
              <option value={key}>{label}</option>
            {/each}
          </select>
        </label>
      {/if}
      {#if canPickRange}
        <div class="segmented" role="group" aria-label="Fleet traffic range">
          {#each ranges as [hours, label]}
            <button type="button" aria-pressed={range === hours} onclick={() => (range = hours)}>{label}</button>
          {/each}
        </div>
      {/if}
    </div>
  </div>
  {#if tag}
    <dl class="totals">
      <div><dt>Current download rate</dt><dd class="tabular">{formatNetworkRate(sumNetworkRate(taggedServers, 'download'))}</dd></div>
      <div><dt>Current upload rate</dt><dd class="tabular">{formatNetworkRate(sumNetworkRate(taggedServers, 'upload'))}</dd></div>
    </dl>
  {/if}
  {#if PREVIEW_MODE}
    <p class="muted">Fleet traffic history is available on the hub.</p>
  {:else if error}
    <p role="alert">{error} <button class="button ghost small" type="button" onclick={() => retry++}>Retry</button></p>
  {:else if !history}
    <p class="muted" role="status">Loading fleet traffic history…</p>
  {:else}
    {#if !history.enabled}
      <div class="recording-status">
        <span class="muted">Traffic recording is disabled.</span>
        {#if onOpenStorage}
          <button class="button ghost small" type="button" onclick={onOpenStorage}>Open storage settings</button>
        {/if}
      </div>
    {/if}
    {#if view.recorded}
      <dl class="totals">
        <div><dt>Download</dt><dd class="tabular">{view.download === null ? 'Unavailable' : formatBytes(view.download)}</dd></div>
        <div><dt>Upload</dt><dd class="tabular">{view.upload === null ? 'Unavailable' : formatBytes(view.upload)}</dd></div>
        <div><dt>Total recorded</dt><dd class="tabular">{formatBytes((view.download ?? 0n) + (view.upload ?? 0n))}</dd></div>
      </dl>
      {#key `${range}-${timezone}-${theme}`}
        {#await loadChart() then { default: TrafficChart }}
          <TrafficChart data={view.data} {timezone} hourlyBytes />
        {:catch}
          <p role="alert">Could not load the chart. Reload the page to try again.</p>
        {/await}
      {/key}
      <p class="muted">{view.recorded} of {range} hours have data. {view.complete} of those hours include download and upload from every {tag ? 'server with this tag' : 'server'} at the time of recording.</p>
    {:else if tag && (history.enabled || canPickRange)}
      <p class="muted">No traffic recorded for this tag in this range.</p>
    {:else if history.enabled}
      <p class="muted">No traffic data is available for this range.</p>
    {:else if hasSavedTraffic}
      <p class="muted">No saved traffic in this range.</p>
    {/if}
  {/if}
</article>

<style>
  .fleet-traffic { margin-bottom: 24px; min-width: 0; }
  .panel-heading { flex-wrap: wrap; gap: 12px; }
  .filters { display: flex; align-items: center; flex-wrap: wrap; gap: 8px 12px; }
  .filters label { display: flex; align-items: center; gap: 8px; }
  .recording-status { display: flex; align-items: center; flex-wrap: wrap; gap: 8px 12px; margin: 10px 0 16px; }
  .recording-status:last-child { margin-bottom: 0; }
  .totals { display: flex; flex-wrap: wrap; gap: 12px 32px; margin: 0 0 16px; }
  dt { color: var(--muted); font-size: 12px; }
  dd { margin: 3px 0 0; font-size: 17px; font-weight: 600; }
  p { margin: 10px 0 0; }
</style>
