<script lang="ts">
  import Icon from '../../Icon.svelte';
  import Sparkline from '../../Sparkline.svelte';
  import CircleChart from '../../CircleChart.svelte';
  import ChartPreview from '../../ChartPreview.svelte';
  import { metricValue, resourceLevel, sampleAge } from '../../lib/format';
  import type { PreviewChartData, PreviewChartRange, PreviewServer } from '../../preview/fixtures';

  let { server, chartData, range = $bindable(), timezone, theme }: {
    server: PreviewServer;
    chartData: PreviewChartData | null;
    range: PreviewChartRange;
    timezone: string;
    theme: string;
  } = $props();

  const cards = [
    { key: 'cpu', label: 'CPU', icon: 'cpu', tone: 'teal' },
    { key: 'memory', label: 'Memory', icon: 'memory', tone: 'purple' }
  ] as const;

  function history(metric: 'cpu' | 'memory' | 'disk'): Array<number | null> {
    return server.metricHistory?.ranges[range]?.[metric] ?? [];
  }

  function seriesRange(values: Array<number | null>): string {
    const numeric = values.filter((value): value is number => value !== null && Number.isFinite(value));
    if (numeric.length === 0) return 'unavailable';
    return `${Math.min(...numeric).toFixed(2)}–${Math.max(...numeric).toFixed(2)}%`;
  }

  function average(values: Array<number | null>): string {
    const numeric = values.filter((value): value is number => value !== null && Number.isFinite(value));
    return numeric.length ? `${(numeric.reduce((sum, value) => sum + value, 0) / numeric.length).toFixed(1)}%` : '—';
  }

  const disk = $derived(server.metrics.disk);
</script>

<div class="metric-grid stagger">
  {#each cards as card, index (card.key)}
    {@const values = history(card.key)}
    {@const value = server.metrics[card.key]}
    <article class={`metric-card ${resourceLevel(value)}`} class:purple={card.tone === 'purple'} style:--i={index}>
      <div class="card-top">
        <span class="metric-title"><Icon name={card.icon} size={14} />{card.label}</span>
        <strong class="metric-big tabular">{metricValue(value)}</strong>
      </div>
      {#if values.length > 1}
        <div class="sparkline"><Sparkline {values} tone={card.tone} /></div>
      {:else}
        <div class="sparkline-unavailable">{values.some((point) => point !== null) ? 'Collecting history' : 'Awaiting samples'}</div>
      {/if}
      <div class="metric-footer"><span>{sampleAge(server)}</span><span>avg {average(values)} · {range}</span></div>
    </article>
  {/each}
  <article class={`metric-card disk ${resourceLevel(disk)}`} style:--i={2}>
    <div class="card-top">
      <span class="metric-title"><Icon name="hard-drive" size={14} />Disk</span>
      <strong class="metric-big tabular">{metricValue(disk)}</strong>
    </div>
    <div class="disk-ring"><CircleChart value={disk} size={62} strokeWidth={9} label="Used" color="var(--blue)" /></div>
    <div class="metric-footer"><span>{sampleAge(server)}</span><span>{disk !== null ? `${(100 - disk).toFixed(0)}% free` : `Window: ${range}`}</span></div>
  </article>
</div>

<article class="panel chart-panel">
  <div class="panel-heading">
    <div><h2>CPU and memory utilization</h2><span class="muted">Scale 0–100% · {timezone}</span></div>
    <div class="segmented" role="group" aria-label="Chart time range">
      {#each [['15m', '15 min'], ['1h', '1 hour'], ['24h', '24 hours']] as [id, label] (id)}
        <button type="button" aria-pressed={range === id} onclick={() => (range = id as PreviewChartRange)}>{label}</button>
      {/each}
    </div>
  </div>
  {#if chartData && chartData.coverage !== 'unavailable'}
    <div class="legend">
      <span><i class="legend-dot accent"></i> CPU</span>
      <span><i class="legend-dot purple"></i> Memory</span>
      {#if chartData.coverage === 'gap'}<span class="gap-note"><Icon name="alert-circle" size={12} /> Some samples are missing in this range</span>{/if}
    </div>
    {#key `${range}-${theme}-${server.id}-${timezone}`}
      <ChartPreview data={chartData} {range} {timezone} label="CPU and memory history over the selected time range" />
    {/key}
    <div class="highlights">
      <span>CPU range <strong class="tabular">{seriesRange(chartData.cpu)}</strong></span>
      <span>Memory range <strong class="tabular">{seriesRange(chartData.memory)}</strong></span>
    </div>
  {:else}
    <div class="unavailable-panel"><strong>Resource history unavailable</strong><span>No telemetry samples have been recorded yet for this range.</span></div>
  {/if}
</article>

<article class="panel disk-panel">
  <div class="panel-heading"><div><h2>Disk storage</h2><span class="muted">Root filesystem</span></div></div>
  <div class="disk-layout">
    <CircleChart value={disk} size={128} strokeWidth={10} label="Used" color="var(--blue)" sublabel={disk !== null ? `${(100 - disk).toFixed(1)}% free` : ''} />
    <div class="disk-stats">
      <div class="stat"><span>Used space</span><strong class="tabular">{disk !== null ? `${disk.toFixed(1)}%` : '—'}</strong></div>
      <div class="stat"><span>Available space</span><strong class="tabular">{disk !== null ? `${(100 - disk).toFixed(1)}%` : '—'}</strong></div>
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
    box-shadow: var(--shadow);
  }
  .metric-card.purple { --metric: var(--purple); }
  .metric-card.disk { --metric: var(--blue); }
  .metric-card.warning { --metric: var(--warning); }
  .metric-card.critical { --metric: var(--danger); }
  .card-top { display: flex; align-items: baseline; justify-content: space-between; gap: 10px; }
  .metric-title { display: inline-flex; align-items: center; gap: 6px; color: var(--muted); font-size: 12.5px; font-weight: 600; }
  .metric-big { color: var(--metric); font-size: 24px; font-weight: 700; letter-spacing: -0.03em; }
  .sparkline, .sparkline-unavailable, .disk-ring { height: 50px; margin: 12px 0 10px; }
  .sparkline-unavailable { display: grid; place-items: center; border-radius: var(--radius-sm); background: var(--surface-muted); color: var(--muted); font-size: 12px; }
  .disk-ring { display: grid; place-items: center; height: auto; flex: 1; margin: 4px 0; }
  .metric-footer { display: flex; justify-content: space-between; gap: 8px; margin-top: auto; color: var(--muted); font-size: 12px; }

  .chart-panel { margin-bottom: var(--gap); }
  .gap-note { color: var(--warning); }
  .highlights { display: flex; flex-wrap: wrap; gap: 8px 22px; margin-top: 14px; padding-top: 12px; border-top: 1px solid var(--line-light); color: var(--muted); font-size: 12.5px; }
  .highlights strong { color: var(--ink); }

  .disk-layout { display: flex; flex-wrap: wrap; align-items: center; gap: 28px; padding: 4px; }
  .disk-stats { display: grid; flex: 1; grid-template-columns: repeat(3, minmax(110px, 1fr)); gap: 12px; }
  .stat { display: grid; gap: 4px; padding: 12px 14px; border: 1px solid var(--line-light); border-radius: var(--radius-md); background: var(--surface-muted); }
  .stat span { color: var(--muted); font-size: 12px; }
  .stat strong { font-size: 16px; font-weight: 650; }
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
