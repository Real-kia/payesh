<script lang="ts">
  import { onMount } from 'svelte';
  import { formatBytes } from './lib/format';
  import { formatNetworkRate } from './network';
  import uPlot from 'uplot';
  import 'uplot/dist/uPlot.min.css';
  import { chartColors, areaFill, hoverValues } from './lib/chart';

  let { data, timezone = 'UTC', hourlyBytes = false }: {
    data: { timestamps: number[]; networkRx?: (number | null)[]; networkTx?: (number | null)[] };
    timezone?: string;
    hourlyBytes?: boolean;
  } = $props();
  let host: HTMLDivElement;
  let plot: uPlot | undefined;

  $effect(() => {
    const updated: uPlot.AlignedData = [data.timestamps, data.networkRx ?? data.timestamps.map(() => null), data.networkTx ?? data.timestamps.map(() => null)];
    plot?.setData(updated);
  });

  onMount(() => {
    if (!data.networkRx || !data.networkTx || data.timestamps.length === 0) return;
    const { muted, line, surface, read } = chartColors(host);
    const accent = read('--accent', '#2f6bd8');
    const secondary = read('--muted', '#66666e');
    const value = (_u: uPlot, raw: number | null) => raw == null || !Number.isFinite(raw) ? '—' : hourlyBytes ? formatBytes(raw) : formatNetworkRate(raw);
    plot = new uPlot({
      width: Math.max(240, host.clientWidth),
      height: 240,
      plugins: [hoverValues(timezone)],
      scales: { x: { time: true } },
      axes: [
        {
          stroke: muted,
          space: 70,
          grid: { stroke: line, width: 1 },
          ticks: { show: false },
          font: '11px system-ui, -apple-system, Segoe UI, sans-serif',
          values: (_u, values) => values.map((v) => new Date(Number(v) * 1000).toLocaleString([], hourlyBytes && data.timestamps.length > 24
            ? { month: 'short', day: 'numeric', timeZone: timezone }
            : { hour: '2-digit', minute: '2-digit', timeZone: timezone }))
        },
        {
          stroke: muted,
          size: 65,
          grid: { stroke: line, width: 1 },
          ticks: { show: false },
          font: '11px system-ui, -apple-system, Segoe UI, sans-serif',
          values: (_u, values) => values.map((v) => hourlyBytes ? formatBytes(v) : (Number(v) * 8 / 1_000_000).toLocaleString([], { maximumFractionDigits: 2 }))
        }
      ],
      cursor: {
        focus: { prox: 30 },
        points: {
          size: 6,
          width: 1.5,
          fill: surface
        }
      },
      series: [
        {},
        {
          label: 'Download',
          stroke: accent,
          width: 1.5,
          fill: areaFill(accent),
          points: { show: hourlyBytes, size: 4 },
          value
        },
        {
          label: 'Upload',
          stroke: secondary,
          width: 1.5,
          points: { show: hourlyBytes, size: 4 },
          value
        }
      ]
    }, [data.timestamps, data.networkRx, data.networkTx] as uPlot.AlignedData, host);
    const resize = new ResizeObserver(([entry]) => plot?.setSize({ width: Math.max(240, entry.contentRect.width), height: 240 }));
    resize.observe(host);
    return () => { resize.disconnect(); plot?.destroy(); };
  });
</script>

<div bind:this={host} class="chart-host" role="img" aria-label={hourlyBytes ? 'Fleet download and upload bytes per hour' : 'Network download and upload rate history'}></div>

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
