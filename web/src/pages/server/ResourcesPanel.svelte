<script lang="ts">
  import Icon from '../../Icon.svelte';
  import Sparkline from '../../Sparkline.svelte';
  import ChartPreview from '../../ChartPreview.svelte';
  import { clampPercent, formatBytes, metricValue, resourceLevel, sampleAge } from '../../lib/format';
  import type { PreviewChartData, PreviewChartRange, PreviewServer } from '../../preview/fixtures';

  let { server, chartData, range = $bindable(), timezone, theme }: {
    server: PreviewServer;
    chartData: PreviewChartData | null;
    range: PreviewChartRange;
    timezone: string;
    theme: string;
  } = $props();

  const cards = [
    { key: 'cpu', label: 'CPU' },
    { key: 'memory', label: 'Memory' },
    { key: 'swap', label: 'Swap' }
  ] as const;

  function history(metric: 'cpu' | 'memory' | 'swap' | 'disk'): Array<number | null> {
    return server.metricHistory?.ranges[range]?.[metric] ?? [];
  }

  function seriesRange(values: Array<number | null>): string {
    const numeric = values.filter((value): value is number => value !== null && Number.isFinite(value));
    if (numeric.length === 0) return 'unavailable';
    return `${Math.min(...numeric).toFixed(1)}–${Math.max(...numeric).toFixed(1)}%`;
  }

  function average(values: Array<number | null>): string {
    const numeric = values.filter((value): value is number => value !== null && Number.isFinite(value));
    return numeric.length ? `${(numeric.reduce((sum, value) => sum + value, 0) / numeric.length).toFixed(1)}%` : '—';
  }

  function hasSamples(values: Array<number | null>): boolean {
    return values.some((value) => value !== null && Number.isFinite(value));
  }

  const disk = $derived(server.metrics.disk);
  const hasSwap = $derived(
    server.metrics.swap !== null || hasSamples(history('swap')) || (chartData ? hasSamples(chartData.swap) : false)
  );
  const visibleCards = $derived(cards.filter((card) => card.key !== 'swap' || hasSwap));
</script>

<div class="metric-grid">
  {#each visibleCards as card, index (card.key)}
    {@const values = history(card.key)}
    {@const value = server.metrics[card.key]}
    <article class={`metric-card ${resourceLevel(value)}`}>
      <div class="card-top">
        <span class="metric-title">
          {card.label}
          {#if card.key === 'cpu' && server.capacity?.cpuCores}
            <span class="metric-sub">{server.capacity.cpuCores} {server.capacity.cpuCores === 1 ? 'core' : 'cores'}</span>
          {:else if card.key === 'memory' && server.capacity?.memoryTotalBytes}
            <span class="metric-sub">{formatBytes(server.capacity.memoryUsedBytes ?? '')} / {formatBytes(server.capacity.memoryTotalBytes)}</span>
          {:else if card.key === 'swap' && server.capacity?.swapTotalBytes}
            <span class="metric-sub">{formatBytes(server.capacity.swapUsedBytes ?? '')} / {formatBytes(server.capacity.swapTotalBytes)}</span>
          {/if}
        </span>
        <strong class="metric-big tabular">{metricValue(value)}</strong>
      </div>
      {#if values.length > 1}
        <div class="sparkline"><Sparkline {values} /></div>
      {:else}
        <div class="sparkline-unavailable">{values.some((point) => point !== null) ? 'Collecting history' : 'Awaiting samples'}</div>
      {/if}
      <div class="metric-footer">
        <span>{sampleAge(server)}</span>
        <span>
          {#if card.key === 'cpu' && server.capacity?.cpuCores}
            {server.capacity.cpuCores} {server.capacity.cpuCores === 1 ? 'core' : 'cores'} ·
          {:else if card.key === 'memory' && server.capacity?.memoryTotalBytes}
            {formatBytes(server.capacity.memoryUsedBytes ?? '')} of {formatBytes(server.capacity.memoryTotalBytes)} ·
          {:else if card.key === 'swap' && server.capacity?.swapTotalBytes}
            {formatBytes(server.capacity.swapUsedBytes ?? '')} of {formatBytes(server.capacity.swapTotalBytes)} ·
          {/if}
          avg {average(values)} · {range}
        </span>
      </div>
    </article>
  {/each}
  <article class={`metric-card disk ${resourceLevel(disk)}`}>
    <div class="card-top">
      <span class="metric-title">
        Disk
        {#if server.capacity?.diskTotalBytes}
          <span class="metric-sub">{formatBytes(server.capacity.diskUsedBytes ?? '')} / {formatBytes(server.capacity.diskTotalBytes)}</span>
        {/if}
      </span>
      <strong class="metric-big tabular">{metricValue(disk)}</strong>
    </div>
    <div class="disk-bar"><div class="meter"><span style:width={`${clampPercent(disk)}%`}></span></div></div>
    <div class="metric-footer">
      <span>{sampleAge(server)}</span>
      <span>{server.capacity?.diskTotalBytes ? `${formatBytes(server.capacity.diskAvailableBytes ?? '')} free of ${formatBytes(server.capacity.diskTotalBytes)}` : (disk !== null ? `${(100 - disk).toFixed(0)}% free` : `Window: ${range}`)}</span>
    </div>
  </article>
</div>

<article class="panel chart-panel">
  <div class="panel-heading">
      <div><h2>CPU utilization</h2><span class="muted">{#if server.capacity?.cpuCores}{server.capacity.cpuCores} {server.capacity.cpuCores === 1 ? 'core' : 'cores'} · {/if}Scale 0–100% · {timezone}</span></div>
    <div class="segmented" role="group" aria-label="Chart time range">
      {#each [['15m', '15 min'], ['1h', '1 hour'], ['24h', '24 hours']] as [id, label] (id)}
        <button type="button" aria-pressed={range === id} onclick={() => (range = id as PreviewChartRange)}>{label}</button>
      {/each}
    </div>
  </div>
  {#if chartData && chartData.coverage !== 'unavailable'}
    <div class="legend">
      <span><i class="legend-dot accent"></i> CPU</span>
      {#if chartData.coverage === 'gap'}<span class="gap-note"><Icon name="alert-circle" size={12} /> Some samples are missing in this range</span>{/if}
    </div>
    {#key `${range}-${theme}-${server.id}-${timezone}`}
      <ChartPreview data={chartData} {range} {timezone} metric="cpu" label="CPU history over the selected time range" />
    {/key}
    <div class="highlights">
      {#if server.capacity?.cpuCores}<span>Cores <strong class="tabular">{server.capacity.cpuCores}</strong></span>{/if}
      <span>CPU range <strong class="tabular">{seriesRange(chartData.cpu)}</strong></span>
    </div>
  {:else}
    <div class="unavailable-panel"><strong>Resource history unavailable</strong><span>No telemetry samples have been recorded yet for this range.</span></div>
  {/if}
</article>

<article class="panel chart-panel">
  <div class="panel-heading"><div><h2>Memory utilization</h2><span class="muted">{#if server.capacity?.memoryTotalBytes}{formatBytes(server.capacity.memoryUsedBytes ?? '')} used of {formatBytes(server.capacity.memoryTotalBytes)} · {/if}Scale 0–100% · {timezone}</span></div></div>
  {#if chartData && chartData.coverage !== 'unavailable'}
    <ChartPreview data={chartData} {range} {timezone} metric="memory" label="Memory history over the selected time range" />
    <div class="highlights">
      {#if server.capacity?.memoryTotalBytes}
        <span>Total <strong class="tabular">{formatBytes(server.capacity.memoryTotalBytes)}</strong></span>
        <span>Used <strong class="tabular">{formatBytes(server.capacity.memoryUsedBytes ?? '')}</strong></span>
        {#if server.capacity.memoryAvailableBytes}<span>Available <strong class="tabular">{formatBytes(server.capacity.memoryAvailableBytes)}</strong></span>{/if}
      {/if}
      <span>Memory range <strong class="tabular">{seriesRange(chartData.memory)}</strong></span>
    </div>
  {:else}<div class="unavailable-panel"><strong>Memory history unavailable</strong><span>No telemetry samples have been recorded yet for this range.</span></div>{/if}
</article>

{#if hasSwap}
<article class="panel chart-panel">
  <div class="panel-heading"><div><h2>Swap utilization</h2><span class="muted">{#if server.capacity?.swapTotalBytes}{formatBytes(server.capacity.swapUsedBytes ?? '')} used of {formatBytes(server.capacity.swapTotalBytes)} · {/if}Scale 0–100% · {timezone}</span></div></div>
  {#if chartData && chartData.coverage !== 'unavailable' && hasSamples(chartData.swap)}
    <ChartPreview data={chartData} {range} {timezone} metric="swap" label="Swap history over the selected time range" />
    <div class="highlights">
      {#if server.capacity?.swapTotalBytes}
        <span>Total <strong class="tabular">{formatBytes(server.capacity.swapTotalBytes)}</strong></span>
        <span>Used <strong class="tabular">{formatBytes(server.capacity.swapUsedBytes ?? '')}</strong></span>
      {/if}
      <span>Swap range <strong class="tabular">{seriesRange(chartData.swap)}</strong></span>
    </div>
  {:else}<div class="unavailable-panel"><strong>Swap history unavailable</strong><span>This node may not expose swap telemetry.</span></div>{/if}
</article>
{/if}

<article class="panel disk-panel">
  <div class="panel-heading"><div><h2>Disk storage</h2><span class="muted">Root filesystem{#if server.capacity?.diskTotalBytes} · {formatBytes(server.capacity.diskTotalBytes)} total{/if}</span></div></div>
  <div class="disk-layout">
    <div class="disk-usage">
      <div class="disk-usage-top"><strong class="tabular">{metricValue(disk)}</strong><span class="muted">used</span></div>
      <div class={`meter large ${resourceLevel(disk)}`} role="meter" aria-label="Disk usage" aria-valuemin="0" aria-valuemax="100" aria-valuenow={disk ?? undefined}><span style:width={`${clampPercent(disk)}%`}></span></div>
    </div>
    <div class="disk-stats">
      {#if server.capacity?.diskTotalBytes}
        <div class="stat"><span>Total space</span><strong class="tabular">{formatBytes(server.capacity.diskTotalBytes)}</strong></div>
      {/if}
      <div class="stat"><span>Used space</span><strong class="tabular">{server.capacity?.diskUsedBytes ? `${formatBytes(server.capacity.diskUsedBytes)} (${disk !== null ? `${disk.toFixed(1)}%` : ''})` : (disk !== null ? `${disk.toFixed(1)}%` : '—')}</strong></div>
      <div class="stat"><span>Available space</span><strong class="tabular">{server.capacity?.diskAvailableBytes ? `${formatBytes(server.capacity.diskAvailableBytes)} (${disk !== null ? `${(100 - disk).toFixed(1)}%` : ''})` : (disk !== null ? `${(100 - disk).toFixed(1)}%` : '—')}</strong></div>
      <div class={`stat health ${resourceLevel(disk)}`}><span>Health</span><strong>{disk === null ? '—' : disk >= 90 ? 'Critical' : disk >= 75 ? 'Warning' : 'Healthy'}</strong></div>
    </div>
  </div>
</article>

<style>
  .metric-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: var(--gap); margin-bottom: var(--gap); }
  .metric-card {
    --metric: var(--accent);
    display: flex;
    flex-direction: column;
    min-width: 0;
    padding: var(--card-pad);
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
  }
  .metric-card.warning { --metric: var(--warning); }
  .metric-card.critical { --metric: var(--danger); }
  .card-top { display: flex; align-items: baseline; justify-content: space-between; gap: 10px; }
  .metric-title { display: inline-flex; align-items: baseline; gap: 8px; color: var(--muted); font-size: 12.5px; font-weight: 500; flex-wrap: wrap; }
  .metric-sub { color: var(--muted); font-size: 12px; font-weight: 400; opacity: 0.85; }
  .metric-big { color: var(--ink); font-size: 22px; font-weight: 600; letter-spacing: -0.02em; }
  .metric-card.warning .metric-big, .metric-card.critical .metric-big { color: var(--metric); }
  .sparkline, .sparkline-unavailable { height: 44px; margin: 12px 0 10px; }
  .sparkline-unavailable { display: grid; place-items: center; border-radius: var(--radius-sm); background: var(--surface-muted); color: var(--muted); font-size: 12px; }
  .disk-bar { display: flex; flex: 1; align-items: center; margin: 12px 0 10px; }
  .meter { width: 100%; height: 4px; overflow: hidden; border-radius: 2px; background: var(--surface-muted); }
  .meter span { display: block; height: 100%; background: var(--metric, var(--accent)); }
  .meter.large { height: 8px; }
  .meter.warning { --metric: var(--warning); }
  .meter.critical { --metric: var(--danger); }
  .metric-footer { display: flex; justify-content: space-between; gap: 8px; margin-top: auto; color: var(--muted); font-size: 12px; }

  .chart-panel { margin-bottom: var(--gap); }
  .gap-note { color: var(--warning); }
  .highlights { display: flex; flex-wrap: wrap; gap: 8px 22px; margin-top: 14px; padding-top: 12px; border-top: 1px solid var(--line-light); color: var(--muted); font-size: 12.5px; }
  .highlights strong { color: var(--ink); }

  .disk-layout { display: grid; gap: 18px; }
  .disk-usage { display: grid; gap: 8px; }
  .disk-usage-top { display: flex; align-items: baseline; gap: 6px; }
  .disk-usage-top strong { font-size: 22px; font-weight: 600; letter-spacing: -0.02em; }
  .disk-stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(140px, 1fr)); gap: 0; border-top: 1px solid var(--line-light); }
  .stat { display: grid; gap: 2px; padding: 12px 0 0; }
  .stat span { color: var(--muted); font-size: 12px; }
  .stat strong { font-size: 14px; font-weight: 600; }
  .health.normal strong { color: var(--success); }
  .health.warning strong { color: var(--warning); }
  .health.critical strong { color: var(--danger); }

  @media (max-width: 860px) {
    .metric-grid { grid-template-columns: 1fr 1fr; }
    .metric-card.disk { grid-column: 1 / -1; }
  }
  @media (max-width: 560px) {
    .metric-grid, .disk-stats { grid-template-columns: 1fr; }
    .panel-heading { flex-wrap: wrap; }
  }
</style>
