<script lang="ts">
  import { onMount } from 'svelte';
  import { formatBytes } from './lib/format';
  import { formatNetworkRate } from './network';
  import uPlot from 'uplot';
  import 'uplot/dist/uPlot.min.css';
  import { chartColors, areaFill, hoverValues } from './lib/chart';

  let { data, timezone = 'UTC', fleetMode }: {
    data: { timestamps: number[]; networkRx?: (number | null)[]; networkTx?: (number | null)[] };
    timezone?: string;
    fleetMode?: 'bandwidth' | 'traffic';
  } = $props();
  let host: HTMLDivElement;
  let plot: uPlot | undefined;

  $effect(() => {
    const updated: uPlot.AlignedData = [data.timestamps, data.networkRx ?? data.timestamps.map(() => null), data.networkTx ?? data.timestamps.map(() => null)];
    plot?.setData(updated);
  });

  onMount(() => {
    if (!data.networkRx || !data.networkTx || data.timestamps.length === 0) return;
    const dailyBytes = fleetMode === 'traffic';
    const chartTimezone = dailyBytes ? 'UTC' : timezone;
    const bucketSeconds = dailyBytes ? 86400 : 3600;
    const { muted, line, surface, read } = chartColors(host);
    const accent = read('--accent', '#2f6bd8');
    const secondary = read('--muted', '#66666e');
    const bars = (align: -1 | 1) => uPlot.paths.bars?.({ align, size: [0.4, 14], gap: 1 });
    const value = (_u: uPlot, raw: number | null) => raw == null || !Number.isFinite(raw) ? '—' : dailyBytes ? formatBytes(raw) : formatNetworkRate(raw);
    plot = new uPlot({
      width: Math.max(240, host.clientWidth),
      height: 240,
      plugins: [hoverValues(chartTimezone, dailyBytes)],
      scales: {
        x: fleetMode
          ? { time: true, range: (_u, min, max) => [min - bucketSeconds / 2, max + bucketSeconds / 2] }
          : { time: true },
        ...(fleetMode ? { y: { range: (_u: uPlot, _min: number, max: number) => [0, max > 0 ? max * 1.1 : dailyBytes ? 1 : 125000] as [number, number] } } : {})
      },
      axes: [
        {
          stroke: muted,
          space: 70,
          ...(fleetMode ? {
            incrs: dailyBytes ? [86400, 172800, 345600, 604800, 1209600, 2419200] : [3600, 7200, 10800, 21600, 43200, 86400, 172800, 345600, 604800],
            splits: (_u: uPlot, _axis: number, min: number, max: number, increment: number) => {
              const ticks: number[] = [];
              for (let tick = Math.ceil(min / increment) * increment; tick <= max; tick += increment) {
                ticks.push(tick);
              }
              return ticks;
            }
          } : {}),
          grid: { stroke: line, width: 1 },
          ticks: { show: false },
          font: '11px system-ui, -apple-system, Segoe UI, sans-serif',
          values: (_u, values) => values.map((v) => new Date(Number(v) * 1000).toLocaleString([], dailyBytes || (fleetMode === 'bandwidth' && data.timestamps.length > 24)
            ? { month: 'short', day: 'numeric', timeZone: chartTimezone }
            : { hour: '2-digit', minute: '2-digit', timeZone: chartTimezone }))
        },
        {
          stroke: muted,
          size: 65,
          grid: { stroke: line, width: 1 },
          ticks: { show: false },
          font: '11px system-ui, -apple-system, Segoe UI, sans-serif',
          values: (_u, values) => values.map((v) => dailyBytes ? formatBytes(v) : (Number(v) * 8 / 1_000_000).toLocaleString([], { maximumFractionDigits: 2 }))
        }
      ],
      cursor: {
        focus: { prox: 30 },
        points: {
          show: !dailyBytes,
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
          width: dailyBytes ? 0 : 1.5,
          fill: dailyBytes ? accent : areaFill(accent),
          ...(dailyBytes ? { paths: bars(-1), points: { show: false } } : {}),
          value
        },
        {
          label: 'Upload',
          stroke: secondary,
          width: dailyBytes ? 0 : 1.5,
          ...(dailyBytes ? { paths: bars(1), fill: secondary, points: { show: false } } : {}),
          value
        }
      ]
    }, [data.timestamps, data.networkRx, data.networkTx] as uPlot.AlignedData, host);
    const resize = new ResizeObserver(([entry]) => plot?.setSize({ width: Math.max(240, entry.contentRect.width), height: 240 }));
    resize.observe(host);
    return () => { resize.disconnect(); plot?.destroy(); };
  });
</script>

<div bind:this={host} class="chart-host" role="img" aria-label={fleetMode === 'traffic' ? 'Fleet download and upload bytes per day' : 'Network download and upload rate history'}></div>

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
