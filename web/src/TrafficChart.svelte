<script lang="ts">
  import { onMount } from 'svelte';
  import { formatBytes } from './lib/format';
  import { formatNetworkRate } from './network';
  import uPlot from 'uplot';
  import 'uplot/dist/uPlot.min.css';
  import { chartColors, areaFill, hoverValues } from './lib/chart';

  let { data, livePoint, timezone = 'UTC', fleetMode, daily = true, bucketSeconds = 3600, coverage }: {
    data: { timestamps: number[]; networkRx?: (number | null)[]; networkTx?: (number | null)[] };
    livePoint?: { timestamp: number; download: number | null; upload: number | null };
    timezone?: string;
    daily?: boolean;
    bucketSeconds?: number;
    fleetMode?: 'bandwidth' | 'traffic';
    coverage?: (timestamp: number) => string | undefined;
  } = $props();
  let host: HTMLDivElement;
  let plot: uPlot | undefined;

  const chartData = $derived.by((): uPlot.AlignedData => {
    const { timestamps } = data;
    const blanks = timestamps.map(() => null);
    const rx = data.networkRx ?? blanks;
    const tx = data.networkTx ?? blanks;
    if (fleetMode !== 'bandwidth') return [timestamps, rx, tx];

    const lastTimestamp = timestamps.at(-1);
    if (!livePoint || (lastTimestamp !== undefined && livePoint.timestamp <= lastTimestamp)) {
      return [timestamps, rx, tx, blanks, blanks];
    }
    return [
      [...timestamps, livePoint.timestamp],
      [...rx, null],
      [...tx, null],
      [...blanks, livePoint.download],
      [...blanks, livePoint.upload]
    ];
  });

  $effect(() => {
    const updated = chartData;
    plot?.setData(updated);
  });

  onMount(() => {
    if (!chartData[0].length) return;
    const showBytes = fleetMode === 'traffic';
    const dailyBuckets = showBytes && daily;
    const chartTimezone = dailyBuckets ? 'UTC' : timezone;
    const bandwidthPoints = bucketSeconds < 3600 ? { points: { show: true, size: 3 } } : {};
    const padding = (dailyBuckets ? 86400 : bucketSeconds) / 2;
    const livePadding = fleetMode === 'bandwidth' ? 12 : 0;
    const { muted, line, surface, read } = chartColors(host);
    const accent = read('--accent', '#2f6bd8');
    const secondary = read('--muted', '#66666e');
    const bars = (align: -1 | 1) => uPlot.paths.bars?.({ align, size: [0.4, 14], gap: 1 });
    const value = (_u: uPlot, raw: number | null) => raw == null || !Number.isFinite(raw) ? '—' : showBytes ? formatBytes(raw) : formatNetworkRate(raw);
    const liveSeries = (label: string, stroke: string): uPlot.Series => ({
      label,
      paths: () => null,
      stroke,
      points: { show: true, size: 9, width: 2, stroke, fill: surface },
      value
    });
    plot = new uPlot({
      width: Math.max(240, host.clientWidth),
      height: 240,
      plugins: [hoverValues(chartTimezone, dailyBuckets, (timestamp) => coverage?.(timestamp))],
      tzDate: (timestamp) => uPlot.tzDate(new Date(timestamp * 1000), chartTimezone),
      scales: {
        x: fleetMode
          ? { time: true, range: (_u, min, max) => [min - padding, max + Math.max(padding, (max - min) * livePadding / _u.width)] }
          : { time: true },
        ...(fleetMode ? { y: { range: (_u: uPlot, _min: number, max: number) => [0, max > 0 ? max * 1.1 : showBytes ? 1 : 125000] as [number, number] } } : {})
      },
      axes: [
        {
          stroke: muted,
          space: 70,
          ...(fleetMode ? {
            incrs: dailyBuckets ? [86400, 172800, 345600, 604800, 1209600, 2419200] : [300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400, 172800, 345600, 604800],
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
          values: (_u, values) => values.map((v) => new Date(Number(v) * 1000).toLocaleString([], dailyBuckets || (fleetMode === 'bandwidth' && daily)
            ? { month: 'short', day: 'numeric', timeZone: chartTimezone }
            : { hour: '2-digit', minute: '2-digit', timeZone: chartTimezone }))
        },
        {
          stroke: muted,
          size: 65,
          grid: { stroke: line, width: 1 },
          ticks: { show: false },
          font: '11px system-ui, -apple-system, Segoe UI, sans-serif',
          values: (_u, values) => values.map((v) => showBytes ? formatBytes(v) : (Number(v) * 8 / 1_000_000).toLocaleString([], { maximumFractionDigits: 2 }))
        }
      ],
      cursor: {
        focus: { prox: 30 },
        points: {
          show: !showBytes,
          size: 6,
          width: 1.5,
          fill: surface
        }
      },
      series: [
        {},
        {
          label: fleetMode === 'bandwidth' ? 'Avg. download' : 'Download',
          stroke: accent,
          width: showBytes ? 0 : 1.5,
          fill: showBytes ? accent : areaFill(accent),
          ...(showBytes ? { paths: bars(-1), points: { show: false } } : bandwidthPoints),
          value
        },
        {
          label: fleetMode === 'bandwidth' ? 'Avg. upload' : 'Upload',
          stroke: secondary,
          width: showBytes ? 0 : 1.5,
          ...(showBytes ? { paths: bars(1), fill: secondary, points: { show: false } } : bandwidthPoints),
          value
        },
        ...(fleetMode === 'bandwidth' ? [liveSeries('Live download', accent), liveSeries('Live upload', secondary)] : [])
      ]
    }, chartData, host);
    const resize = new ResizeObserver(([entry]) => plot?.setSize({ width: Math.max(240, entry.contentRect.width), height: 240 }));
    resize.observe(host);
    return () => { resize.disconnect(); plot?.destroy(); };
  });
</script>

<div bind:this={host} class="chart-host" role="img" aria-label={fleetMode === 'traffic' ? `Fleet download and upload bytes per ${daily ? 'day' : bucketSeconds < 3600 ? 'recorded interval' : 'hour'}` : 'Network download and upload rate history'}></div>

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
