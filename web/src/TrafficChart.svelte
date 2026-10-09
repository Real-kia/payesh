<script lang="ts">
  import { onMount } from 'svelte';
  import { formatNetworkRate } from './network';
  import uPlot from 'uplot';
  import 'uplot/dist/uPlot.min.css';
  import { chartColors, gradientFill } from './lib/chart';
  import type { PreviewChartData } from './preview/fixtures';

  let { data, timezone = 'UTC' }: { data: PreviewChartData; timezone?: string } = $props();
  let host: HTMLDivElement;
  let plot: uPlot | undefined;
  const rate = formatNetworkRate;

  $effect(() => {
    const updated: uPlot.AlignedData = [data.timestamps, data.networkRx ?? data.timestamps.map(() => null), data.networkTx ?? data.timestamps.map(() => null)];
    plot?.setData(updated);
  });

  onMount(() => {
    if (!data.networkRx || !data.networkTx || data.timestamps.length < 2) return;
    const { muted, line, surface, read } = chartColors(host);
    const teal = read('--accent', '#2f6bd8');
    const blue = read('--blue', '#1d8cb0');
    const value = (_u: uPlot, raw: number | null) => raw == null || !Number.isFinite(raw) ? '—' : rate(raw);
    plot = new uPlot({
      width: Math.max(240, host.clientWidth),
      height: 240,
      scales: { x: { time: true } },
      axes: [
        {
          stroke: muted,
          space: 70,
          grid: { stroke: line, width: 1 },
          ticks: { show: false },
          font: '11px system-ui, -apple-system, Segoe UI, sans-serif',
          values: (_u, values) => values.map((v) => new Date(Number(v) * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', timeZone: timezone }))
        },
        {
          stroke: muted,
          size: 65,
          grid: { stroke: line, width: 1 },
          ticks: { show: false },
          font: '11px system-ui, -apple-system, Segoe UI, sans-serif',
          values: (_u, values) => values.map((v) => (Number(v) * 8 / 1_000_000).toLocaleString([], { maximumFractionDigits: 2 }))
        }
      ],
      cursor: {
        focus: { prox: 30 },
        points: {
          size: 7,
          width: 2,
          fill: surface
        }
      },
      series: [
        {},
        {
          label: 'Download',
          stroke: teal,
          width: 2,
          fill: gradientFill(teal),
          points: { show: false },
          value
        },
        {
          label: 'Upload',
          stroke: blue,
          width: 2,
          fill: gradientFill(blue),
          points: { show: false },
          value
        }
      ]
    }, [data.timestamps, data.networkRx, data.networkTx] as uPlot.AlignedData, host);
    const resize = new ResizeObserver(([entry]) => plot?.setSize({ width: Math.max(240, entry.contentRect.width), height: 240 }));
    resize.observe(host);
    return () => { resize.disconnect(); plot?.destroy(); };
  });
</script>

<div bind:this={host} class="chart-host" role="img" aria-label="Network download and upload rate history"></div>

<style>
  .chart-host { min-height: 240px; width: 100%; }
  .chart-host :global(.uplot) { background: transparent; font-family: inherit; }
  .chart-host :global(.u-axis) { color: var(--muted); }
  .chart-host :global(.u-cursor-x), .chart-host :global(.u-cursor-y) { border-color: var(--muted); opacity: 0.45; }
  .chart-host :global(.u-legend) { margin-top: 6px; color: var(--muted); font-size: 12px; }
  .chart-host :global(.u-legend .u-marker) { width: 9px; height: 9px; border-radius: 50%; }
  .chart-host :global(.u-legend th) { font-weight: 500; }
  .chart-host :global(.u-legend .u-value) { color: var(--ink); font-variant-numeric: tabular-nums; }
</style>
