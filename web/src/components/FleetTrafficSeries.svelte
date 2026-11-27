<script lang="ts">
  import { ApiError, apiClient, type FleetTrafficHistory, type FleetTrafficHour } from '../api';
  import { PREVIEW_MODE } from '../lib/env';
  import { formatBytes, sumNetworkRate } from '../lib/format';
  import { formatNetworkRate } from '../network';
  import type { PreviewServer } from '../preview/fixtures';

  let { servers, timezone, theme, onAuthExpired, onOpenStorage, tag, label, visible = true,
    onHistory, mode, range }: {
    servers: PreviewServer[];
    timezone: string;
    theme: string;
    onAuthExpired: () => void;
    onOpenStorage?: () => void;
    tag: string;
    label?: string;
    visible?: boolean;
    onHistory?: (history: FleetTrafficHistory) => void;
    mode: 'Bandwidth' | 'Traffic';
    range: number;
  } = $props();

  const loadChart = () => import('../TrafficChart.svelte');
  const hour = 3600000;
  const scopedServers = $derived(tag ? servers.filter((server) => server.tags?.some((label) => label.toLowerCase() === tag)) : servers);
  let history = $state<FleetTrafficHistory | null>(null);
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
        history = result;
        onHistory?.(result);
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

  const hasSavedTraffic = $derived(!!history && (!!history.five_minutes?.length || history.hours.some((entry) => entry.download_bytes !== null || entry.upload_bytes !== null)));

  const fiveMinute = $derived(mode === 'Bandwidth' && range === 24 && !!history?.five_minute_end && !!history.five_minutes?.length);
  const interval = $derived(fiveMinute ? 5 * 60 * 1000 : hour);
  function bucketCoverage(timestamp: number): string | undefined {
    return view.coverage.get(timestamp);
  }

  const view = $derived.by(() => {
    const end = Date.parse((fiveMinute ? history?.five_minute_end : history?.end) ?? '');
    const windowStart = end - range * hour;
    const hourlyPoints = new Map(history?.hours.map((entry) => [Date.parse(entry.start), entry]));
    const finePoints = new Map(history?.five_minutes?.map((entry) => [Date.parse(entry.start), entry]));
    const buckets: { start: number; duration: number; entry?: FleetTrafficHour }[] = [];
    if (Number.isFinite(end)) {
      // Keep complete hourly buckets until the first fine-resolution record.
      // An hour that overlaps any five-minute buckets would double-count bytes.
      const fineStart = fiveMinute
        ? Math.max(windowStart, Math.min(...finePoints.keys()))
        : end;
      for (let start = Math.ceil(windowStart / hour) * hour; start + hour <= fineStart; start += hour) {
        buckets.push({ start, duration: hour, entry: hourlyPoints.get(start) });
      }
      if (fiveMinute) {
        for (let start = fineStart; start < end; start += interval) {
          buckets.push({ start, duration: interval, entry: finePoints.get(start) });
        }
      }
    }
    const coverage = new Map<number, string>();
    const timestamps: number[] = [];
    const networkRx: (number | null)[] = [];
    const networkTx: (number | null)[] = [];
    const days = new Map<number, { download: bigint | null; upload: bigint | null }>();
    const calendar = new Intl.DateTimeFormat('en', { timeZone: timezone, year: 'numeric', month: 'numeric', day: 'numeric' });
    let download = 0n;
    let upload = 0n;
    let recorded = 0;
    let complete = 0;
    let recordedHours = 0;
    let recordedFiveMinutes = 0;
    let latest: FleetTrafficHour | undefined;
    for (const { start, duration, entry } of buckets) {
      const rx = entry?.download_bytes ?? null;
      const tx = entry?.upload_bytes ?? null;
      if (mode === 'Bandwidth' || range === 24) {
        const divisor = mode === 'Bandwidth' ? duration / 1000 : 1;
        const timestamp = (start + (fiveMinute ? duration : 0)) / 1000;
        timestamps.push(timestamp);
        if (entry && (rx !== null || tx !== null)) {
          const resolution = fiveMinute ? (duration === hour ? 'Hourly average · ' : '5-minute average · ') : '';
          coverage.set(timestamp, `${resolution}Servers recorded · In ${entry.download_servers}/${entry.server_count} · Out ${entry.upload_servers}/${entry.server_count}`);
        }
        networkRx.push(rx === null ? null : Number(rx) / divisor);
        networkTx.push(tx === null ? null : Number(tx) / divisor);
      } else {
        const parts = calendar.formatToParts(start);
        const year = Number(parts.find((part) => part.type === 'year')?.value);
        const month = Number(parts.find((part) => part.type === 'month')?.value);
        const day = Number(parts.find((part) => part.type === 'day')?.value);
        // Encode local calendar dates at UTC midnight for evenly spaced day labels.
        const date = Date.UTC(year, month - 1, day) / 1000;
        const totals = days.get(date) ?? { download: null, upload: null };
        if (rx !== null) totals.download = (totals.download ?? 0n) + BigInt(rx);
        if (tx !== null) totals.upload = (totals.upload ?? 0n) + BigInt(tx);
        days.set(date, totals);
      }
      if (rx !== null) download += BigInt(rx);
      if (tx !== null) upload += BigInt(tx);
      if (rx !== null || tx !== null) {
        latest = entry;
        recorded++;
        if (duration === hour) recordedHours++;
        else recordedFiveMinutes++;
      }
      if (entry && rx !== null && tx !== null && entry.download_servers === entry.server_count && entry.upload_servers === entry.server_count) complete++;
    }
    for (const [date, totals] of days) {
      timestamps.push(date);
      networkRx.push(totals.download === null ? null : Number(totals.download));
      networkTx.push(totals.upload === null ? null : Number(totals.upload));
    }
    return { data: { timestamps, networkRx, networkTx }, coverage, recorded, complete, recordedHours, recordedFiveMinutes, latest,
      download: networkRx.some((value) => value !== null) ? download : null,
      upload: networkTx.some((value) => value !== null) ? upload : null };
  });
</script>

{#if visible}
  <section class:tagged={!!label} aria-label={label}>
    {#if label}<h3>{label}</h3>{/if}
    {#if scopedServers.length}
      <dl class="totals">
        <div><dt>Live download rate</dt><dd class="tabular">{formatNetworkRate(sumNetworkRate(scopedServers, 'download'))}</dd></div>
        <div><dt>Live upload rate</dt><dd class="tabular">{formatNetworkRate(sumNetworkRate(scopedServers, 'upload'))}</dd></div>
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
        {#if mode === 'Bandwidth'}
          <p class="muted resolution">{fiveMinute ? (view.recordedHours ? 'Hourly and 5-minute averages · Times mark the end of each interval' : '5-minute averages · Times mark the end of each interval') : range === 24 ? 'Hourly history · Five-minute records appear as they are collected' : 'Hourly averages'}</p>
          {#if view.latest && (view.latest.download_servers < view.latest.server_count || view.latest.upload_servers < view.latest.server_count)}
            <p class="muted resolution">Latest recorded interval has partial coverage. Download includes {view.latest.download_servers}/{view.latest.server_count} servers and upload includes {view.latest.upload_servers}/{view.latest.server_count}. Missing servers are excluded from the chart rates.</p>
          {/if}
        {/if}
        <dl class="totals">
          <div><dt>Recorded download</dt><dd class="tabular">{view.download === null ? 'Unavailable' : formatBytes(view.download)}</dd></div>
          <div><dt>Recorded upload</dt><dd class="tabular">{view.upload === null ? 'Unavailable' : formatBytes(view.upload)}</dd></div>
          <div><dt>Total recorded</dt><dd class="tabular">{formatBytes((view.download ?? 0n) + (view.upload ?? 0n))}</dd></div>
        </dl>
        {#key `${mode}-${range}-${timezone}-${theme}-${interval}`}
          {#await loadChart() then { default: TrafficChart }}
            <TrafficChart data={view.data} {timezone} fleetMode={mode === 'Traffic' ? 'traffic' : 'bandwidth'} daily={range !== 24}
              bucketSeconds={interval / 1000} coverage={mode === 'Bandwidth' || range === 24 ? bucketCoverage : undefined} />
          {:catch}
            <p role="alert">Could not load the chart. Reload the page to try again.</p>
          {/await}
        {/key}
        <p class="muted">{#if fiveMinute && view.recordedHours}{view.recordedHours} hourly and {view.recordedFiveMinutes} five-minute intervals have recorded data.{:else}{view.recorded} of {range * hour / interval} {fiveMinute ? 'five-minute intervals' : 'hours'} have recorded data.{/if} {view.complete} of those include download and upload from every {tag ? 'server with this tag' : 'server'} at the time of recording.</p>
      {:else if tag && (history.enabled || hasSavedTraffic)}
        <p class="muted">No traffic recorded for this tag in this range.</p>
      {:else if history.enabled}
        <p class="muted">No traffic data is available for this range.</p>
      {:else if hasSavedTraffic}
        <p class="muted">No saved traffic in this range.</p>
      {/if}
    {/if}
  </section>
{/if}

<style>
  section { min-width: 0; }
  .tagged { border-top: 1px solid var(--line); padding-top: 16px; margin-top: 16px; }
  h3 { margin: 0 0 12px; font-size: 14px; }
  .recording-status { display: flex; align-items: center; flex-wrap: wrap; gap: 8px 12px; margin: 10px 0 16px; }
  .recording-status:last-child { margin-bottom: 0; }
  .totals { display: flex; flex-wrap: wrap; gap: 12px 32px; margin: 0 0 16px; }
  dt { color: var(--muted); font-size: 12px; }
  dd { margin: 3px 0 0; font-size: 17px; font-weight: 600; }
  p { margin: 10px 0 0; }
  .resolution { margin-bottom: 10px; }
</style>
