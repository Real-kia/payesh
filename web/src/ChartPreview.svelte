<script lang="ts">
  import { onMount } from 'svelte';
  import uPlot from 'uplot';
  import 'uplot/dist/uPlot.min.css';
  import { chartColors, gradientFill } from './lib/chart';
  import type { PreviewChartData, PreviewChartRange } from './preview/fixtures';

  let { label = 'Resource chart', range = '15m', data, timezone = 'UTC' }: { label?: string; range?: PreviewChartRange; data: PreviewChartData; timezone?: string } = $props();
  let host: HTMLDivElement;
  let plot: uPlot | undefined;

  function formatClock(value: number): string {
    const date = new Date(value * 1000);
    return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', timeZone: timezone });
  }

  $effect(() => {
    const updated: uPlot.AlignedData = [data.timestamps, data.cpu, data.memory];
    plot?.setData(updated);
  });

  onMount(() => {
    if (data.coverage === 'unavailable' || data.timestamps.length < 2) return;
    const { muted, line, surface, read } = chartColors(host);
    const teal = read('--accent', '#2f6bd8');
    const purple = read('--purple', '#7a55c7');
    const chartData: uPlot.AlignedData = [data.timestamps, data.cpu, data.memory];
    const value = (_u: uPlot, raw: number | null) => raw == null || !Number.isFinite(raw) ? '—' : `${raw.toFixed(2)}%`;
    plot = new uPlot({
      width: Math.max(240, host.clientWidth || 760),
      height: 220,
      scales: { x: { time: true }, y: { range: [0, 100] } },
      axes: [
        {
          stroke: muted,
          space: 70,
          grid: { stroke: line, width: 1 },
          ticks: { show: false },
          font: '11px system-ui, -apple-system, Segoe UI, sans-serif',
          values: (_u, values) => values.map((value) => formatClock(Number(value)))
        },
        {
          stroke: muted,
          grid: { stroke: line, width: 1 },
          ticks: { show: false },
          font: '11px system-ui, -apple-system, Segoe UI, sans-serif',
          values: (_u, values) => values.map((value) => `${value}%`)
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
          label: 'CPU',
          stroke: teal,
          width: 2,
          fill: gradientFill(teal),
          points: { show: false },
          value
        },
        {
          label: 'Memory',
          stroke: purple,
          width: 2,
          fill: gradientFill(purple),
          points: { show: false },
          value
        }
      ]
    }, chartData, host);
    const resize = new ResizeObserver(([entry]) => plot?.setSize({ width: Math.max(240, entry.contentRect.width), height: 220 }));
    resize.observe(host);
    return () => { resize.disconnect(); plot?.destroy(); };
  });
</script>

<div bind:this={host} class="chart-host" role="img" aria-label={label}></div>

<style>
  .chart-host { min-height: 220px; width: 100%; }
  .chart-host :global(.uplot) { background: transparent; font-family: inherit; }
  .chart-host :global(.u-axis) { color: var(--muted); }
  .chart-host :global(.u-cursor-x), .chart-host :global(.u-cursor-y) { border-color: var(--muted); opacity: 0.45; }
  .chart-host :global(.u-legend) { margin-top: 6px; color: var(--muted); font-size: 12px; }
  .chart-host :global(.u-legend .u-marker) { width: 9px; height: 9px; border-radius: 50%; }
  .chart-host :global(.u-legend th) { font-weight: 500; }
  .chart-host :global(.u-legend .u-value) { color: var(--ink); font-variant-numeric: tabular-nums; }
</style>
