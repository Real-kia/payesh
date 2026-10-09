<script lang="ts">
  import { onDestroy } from 'svelte';
  import Icon from '../../Icon.svelte';
  import TrafficChart from '../../TrafficChart.svelte';
  import StatusPill from '../../components/StatusPill.svelte';
  import { ApiError, apiClient, type TrafficUsage } from '../../api';
  import { formatNetworkRate } from '../../network';
  import { formatDateTimeInTz } from '../../timezone';
  import { currentNetworkRate, formatBytes, percentage } from '../../lib/format';
  import { PREVIEW_MODE } from '../../lib/env';
  import type { PreviewChartData, PreviewChartRange, PreviewServer } from '../../preview/fixtures';

  let { server, chartData, range = $bindable(), timezone, theme, onAuthExpired }: {
    server: PreviewServer;
    chartData: PreviewChartData | null;
    range: PreviewChartRange;
    timezone: string;
    theme: string;
    onAuthExpired: () => void;
  } = $props();

  // Traffic used in a date range. Inputs are completed UTC hours.
  const defaultEnd = new Date(Math.floor(Date.now() / 3600000) * 3600000);
  let trafficFrom = $state(new Date(defaultEnd.getTime() - 86400000).toISOString().slice(0, 16));
  let trafficTo = $state(defaultEnd.toISOString().slice(0, 16));
  let usage = $state<TrafficUsage | null>(null);
  let usageBusy = $state(false);
  let usageError = $state('');
  let controller: AbortController | null = null;

  const hasRates = $derived(!!chartData && (chartData.networkRx?.some((v) => v !== null) || chartData.networkTx?.some((v) => v !== null)));
  const used = $derived(percentage(server.traffic.countedBytes, server.traffic.allowanceBytes));

  function preset(hours: number) {
    const end = new Date(Math.floor(Date.now() / 3600000) * 3600000);
    trafficTo = end.toISOString().slice(0, 16);
    trafficFrom = new Date(end.getTime() - hours * 3600000).toISOString().slice(0, 16);
    void loadUsage();
  }

  async function loadUsage(): Promise<void> {
    usage = null;
    usageError = '';
    const from = new Date(trafficFrom + 'Z');
    const to = new Date(trafficTo + 'Z');
    if (!Number.isFinite(from.getTime()) || !Number.isFinite(to.getTime()) || to <= from || to.getTime() - from.getTime() > 90 * 86400000 || from.getTime() % 3600000 || to.getTime() % 3600000 || to.getTime() > Math.floor(Date.now() / 3600000) * 3600000) {
      usageError = 'Choose completed UTC hours, from before to, spanning at most 90 days.';
      return;
    }
    if (PREVIEW_MODE) { usageError = 'Traffic history is read from the hub and is unavailable in the fixture preview.'; return; }
    const id = server.id;
    const current = new AbortController();
    controller?.abort();
    controller = current;
    usageBusy = true;
    const timeout = window.setTimeout(() => current.abort(), 15000);
    try {
      const result = await apiClient.queryTrafficUsage(id, { from: from.toISOString(), to: to.toISOString(), signal: current.signal });
      if (controller === current && !current.signal.aborted && server.id === id) usage = result;
    } catch (error) {
      if (controller === current && server.id === id) usageError = error instanceof ApiError ? error.message : 'Traffic history could not be loaded. Please retry.';
      if (error instanceof ApiError && error.authExpired) onAuthExpired();
    } finally {
      window.clearTimeout(timeout);
      if (controller === current) usageBusy = false;
    }
  }

  onDestroy(() => controller?.abort());
</script>

<div class="rates stagger">
  <article class="rate-card down" style:--i={0}>
    <span class="rate-icon"><Icon name="download" size={16} /></span>
    <div><span class="muted">Download now</span><strong class="tabular">{formatNetworkRate(currentNetworkRate(server, 'download'))}</strong></div>
  </article>
  <article class="rate-card up" style:--i={1}>
    <span class="rate-icon"><Icon name="upload" size={16} /></span>
    <div><span class="muted">Upload now</span><strong class="tabular">{formatNetworkRate(currentNetworkRate(server, 'upload'))}</strong></div>
  </article>
</div>

<article class="panel">
  <div class="panel-heading">
    <div><h2>Download & upload bandwidth</h2><span class="muted">Mbit/s · {timezone}</span></div>
    <div class="segmented" role="group" aria-label="Traffic chart time range">
      {#each [['15m', '15 min'], ['1h', '1 hour'], ['24h', '24 hours']] as [id, label] (id)}
        <button type="button" aria-pressed={range === id} onclick={() => (range = id as PreviewChartRange)}>{label}</button>
      {/each}
    </div>
  </div>
  {#if chartData && hasRates}
    <div class="legend"><span><i class="legend-dot accent"></i> Download</span><span><i class="legend-dot blue"></i> Upload</span></div>
    {#key `${range}-${theme}-${server.id}-${timezone}-traffic`}
      <TrafficChart data={chartData} {timezone} />
    {/key}
  {:else}
    <div class="unavailable-panel"><strong>Waiting for network samples</strong><span>At least two consecutive network counter samples are needed to calculate bandwidth rate.</span></div>
  {/if}
</article>

<article class="panel">
  <div class="panel-heading"><div><h2>Traffic used in a date range</h2><span class="muted">Completed hours, UTC</span></div></div>
  <div class="presets" role="group" aria-label="Quick ranges">
    {#each [[24, 'Last 24 hours'], [24 * 7, 'Last 7 days'], [24 * 30, 'Last 30 days']] as [hours, label] (hours)}
      <button class="button ghost small" type="button" disabled={usageBusy} onclick={() => preset(Number(hours))}>{label}</button>
    {/each}
  </div>
  <form class="range-form" onsubmit={(event) => { event.preventDefault(); void loadUsage(); }}>
    <label>From<input type="datetime-local" step="3600" required bind:value={trafficFrom} disabled={usageBusy} /></label>
    <label>To<input type="datetime-local" step="3600" required bind:value={trafficTo} disabled={usageBusy} /></label>
    <button class="button primary" type="submit" disabled={usageBusy}>{#if usageBusy}<span class="version-spinner" aria-hidden="true"></span>Calculating…{:else}Calculate usage{/if}</button>
  </form>
  {#if usageError}<p class="error-text" role="alert">{usageError}</p>{/if}
  {#if usage}
    <p class="muted period">{formatDateTimeInTz(usage.from, timezone)} → {formatDateTimeInTz(usage.to, timezone)}</p>
    <div class="usage" aria-live="polite">
      <div><span class="muted">Download</span><strong class="tabular down-text">{usage.download_bytes === null ? 'Unavailable' : formatBytes(usage.download_bytes)}</strong><small class="muted">{usage.download_hours}/{usage.requested_hours} hours recorded</small></div>
      <div><span class="muted">Upload</span><strong class="tabular up-text">{usage.upload_bytes === null ? 'Unavailable' : formatBytes(usage.upload_bytes)}</strong><small class="muted">{usage.upload_hours}/{usage.requested_hours} hours recorded</small></div>
      <div><span class="muted">Total recorded</span><strong class="tabular">{usage.total_bytes === null ? 'Unavailable' : formatBytes(usage.total_bytes)}</strong></div>
    </div>
    {#if usage.download_hours < usage.requested_hours || usage.upload_hours < usage.requested_hours}
      <p class="warning-text" role="status">History is incomplete. These are available totals; missing hours and uncertain counters are excluded.</p>
    {/if}
  {/if}
</article>

{#if server.traffic.from}
  <article class="panel">
    <div class="panel-heading">
      <div><h2 class="capitalize">{server.traffic.scope} billing window</h2><span class="muted mono">{server.traffic.from.slice(0, 10)} → {server.traffic.to.slice(0, 10)}</span></div>
      <StatusPill state={server.traffic.continuity === 'complete' ? 'healthy' : 'stale'} label={server.traffic.continuity} />
    </div>
    <div class="billing">
      <div><strong class="tabular big">{formatBytes(server.traffic.countedBytes)}</strong><span class="muted">of {formatBytes(server.traffic.allowanceBytes)} allowance</span></div>
      <span class="share tabular" class:warn={used >= 75} class:crit={used >= 90}>{used}%</span>
    </div>
    <div class="progress" class:warn={used >= 75} class:crit={used >= 90}><span style:width={`${used}%`}></span></div>
    <div class="details">
      <div><span>Direction</span><strong class="capitalize">{server.traffic.direction}</strong></div>
      <div><span>Timezone</span><strong>{server.traffic.timezone}</strong></div>
      <div><span>Continuity</span><strong class="capitalize">{server.traffic.continuity}</strong></div>
    </div>
  </article>
{:else}
  <div class="unavailable-panel"><strong>Traffic allowance not configured</strong><span>Bandwidth monitoring is active. Configure an allowance to enable quota tracking.</span></div>
{/if}

<style>
  .rates { display: grid; grid-template-columns: 1fr 1fr; gap: var(--gap); margin-bottom: var(--gap); }
  .rate-card { --tone: var(--accent); --tone-bg: var(--accent-bg); display: flex; align-items: center; gap: 14px; padding: var(--card-pad) 20px; border: 1px solid var(--line); border-radius: var(--radius-lg); background: radial-gradient(120% 120% at 100% 0%, var(--tone-bg), transparent 55%), var(--surface); box-shadow: var(--shadow); }
  .rate-card.up { --tone: var(--blue); --tone-bg: var(--blue-bg); }
  .rate-icon { display: grid; place-items: center; width: 38px; height: 38px; border-radius: var(--radius-md); background: var(--tone-bg); color: var(--tone); }
  .rate-card > div { display: grid; gap: 2px; font-size: 12.5px; }
  .rate-card strong { color: var(--tone); font-size: 22px; font-weight: 700; letter-spacing: -0.02em; }

  .presets { display: flex; flex-wrap: wrap; gap: 8px; margin-bottom: 14px; }
  .range-form { display: flex; flex-wrap: wrap; align-items: flex-end; gap: 12px; margin-bottom: 8px; }
  .range-form label { display: grid; flex: 1 1 200px; gap: 7px; min-width: 0; font-size: 13px; font-weight: 550; }
  .range-form input { width: 100%; }
  .period { margin: 16px 0 10px; font-size: 12.5px; }
  .usage { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 10px; }
  .usage > div { display: grid; gap: 4px; padding: 14px; border: 1px solid var(--line-light); border-radius: var(--radius-md); background: var(--surface-muted); font-size: 12.5px; animation: pop-in var(--dur) var(--ease-out); }
  .usage strong { font-size: 20px; letter-spacing: -0.02em; }
  .usage small { font-size: 11.5px; }
  .down-text { color: var(--accent); }
  .up-text { color: var(--blue); }

  .billing { display: flex; align-items: flex-end; justify-content: space-between; gap: 12px; margin-bottom: 12px; }
  .billing > div { display: grid; gap: 2px; }
  .big { font-size: 28px; font-weight: 700; letter-spacing: -0.035em; }
  .share { color: var(--accent); font-weight: 700; }
  .share.warn { color: var(--warning); }
  .share.crit { color: var(--danger); }
  .progress.warn span { background: var(--warning); }
  .progress.crit span { background: var(--danger); }
  .details { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 12px; margin-top: 18px; }
  .details span { display: block; color: var(--muted); font-size: 12px; }
  .details strong { display: block; margin-top: 3px; font-size: 13px; }

  @media (max-width: 640px) {
    .rates, .usage, .details { grid-template-columns: 1fr; }
    .panel-heading { flex-wrap: wrap; }
  }
</style>
