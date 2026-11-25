<script lang="ts">
  import { ApiError, apiClient, type FleetTrafficHistory } from '../api';
  import { PREVIEW_MODE } from '../lib/env';
  import { formatBytes } from '../lib/format';

  let { timezone, theme, onAuthExpired }: { timezone: string; theme: string; onAuthExpired: () => void } = $props();

  const loadChart = () => import('../TrafficChart.svelte');
  const hour = 3600000;
  const ranges = [[24, '24 hours'], [168, '7 days'], [720, '30 days']] as const;
  let range = $state(24);
  let history = $state<FleetTrafficHistory | null>(null);
  let error = $state('');
  let retry = $state(0);

  $effect(() => {
    if (PREVIEW_MODE) return;
    const controller = new AbortController();
    async function load(attempt: number) {
      if (attempt) error = '';
      try {
        const result = await apiClient.getFleetTraffic(controller.signal);
        if (controller.signal.aborted) return;
        history = result;
        error = '';
      } catch (cause) {
        if (controller.signal.aborted) return;
        if (cause instanceof ApiError && cause.authExpired) onAuthExpired();
        error = cause instanceof ApiError ? cause.message : 'Fleet traffic history could not be loaded.';
      }
    }
    void load(retry);
    const timer = window.setInterval(() => void load(0), 60000);
    return () => { controller.abort(); window.clearInterval(timer); };
  });

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
        if (entry && entry.download_servers === entry.server_count && entry.upload_servers === entry.server_count) complete++;
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
    <div class="segmented" role="group" aria-label="Fleet traffic range">
      {#each ranges as [hours, label]}
        <button type="button" aria-pressed={range === hours} onclick={() => (range = hours)}>{label}</button>
      {/each}
    </div>
  </div>
  {#if PREVIEW_MODE}
    <p class="muted">Fleet traffic history is available on the hub.</p>
  {:else if error}
    <p role="alert">{error} <button class="button ghost small" type="button" onclick={() => retry++}>Retry</button></p>
  {:else if !history}
    <p class="muted" role="status">Loading fleet traffic history…</p>
  {:else}
    {#if !history.enabled}
      <p class="muted">Saving is off. The hub owner can enable it in Settings → Storage.</p>
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
      <p class="muted">{view.complete} of {range} hours have data for both directions from every server listed when the hub saved the hour. The totals include partial hours.</p>
    {:else if history.enabled}
      <p class="muted">No traffic recorded in this range yet. The hub starts saving with the first full hour after you enable it.</p>
    {:else}
      <p class="muted">No saved traffic in this range.</p>
    {/if}
  {/if}
</article>

<style>
  .fleet-traffic { margin-bottom: 24px; min-width: 0; }
  .panel-heading { flex-wrap: wrap; gap: 12px; }
  .totals { display: flex; flex-wrap: wrap; gap: 12px 32px; margin: 0 0 16px; }
  dt { color: var(--muted); font-size: 12px; }
  dd { margin: 3px 0 0; font-size: 17px; font-weight: 600; }
  p { margin: 10px 0 0; }
</style>
