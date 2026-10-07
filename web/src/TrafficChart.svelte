<script lang="ts">
  import { onMount } from 'svelte';
  import { formatNetworkRate } from './network';
  import uPlot from 'uplot';
  import 'uplot/dist/uPlot.min.css';
  import type { PreviewChartData } from './preview/fixtures';

  let { data }: { data: PreviewChartData } = $props();
  let host: HTMLDivElement;
  let plot: uPlot | undefined;
  const rate = formatNetworkRate;

  $effect(() => {
    const updated: uPlot.AlignedData = [data.timestamps, data.networkRx ?? data.timestamps.map(() => null), data.networkTx ?? data.timestamps.map(() => null)];
    plot?.setData(updated);
  });

  onMount(() => {
    if (!data.networkRx || !data.networkTx || data.timestamps.length < 2) return;
    const styles = getComputedStyle(host);
    const muted = styles.getPropertyValue('--muted').trim() || '#94a3b8';
    const line = styles.getPropertyValue('--line').trim() || 'rgba(255, 255, 255, 0.08)';
    const teal = styles.getPropertyValue('--teal').trim() || '#10b981';
    const blue = styles.getPropertyValue('--blue').trim() || '#06b6d4';
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
          ticks: { stroke: line, width: 1 },
          font: '12px Segoe UI, Arial, sans-serif',
          values: (_u, values) => values.map((v) => new Date(Number(v) * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', timeZone: 'UTC' }))
        },
        {
          stroke: muted,
          size: 65,
          grid: { stroke: line, width: 1 },
          ticks: { stroke: line, width: 1 },
          font: '12px Segoe UI, Arial, sans-serif',
          values: (_u, values) => values.map((v) => (Number(v) * 8 / 1_000_000).toLocaleString([], { maximumFractionDigits: 2 }))
        }
      ],
      cursor: {
        focus: { prox: 30 },
        points: {
          size: 7,
          width: 2,
          fill: '#fff'
        }
      },
      series: [
        {},
        {
          label: 'Download',
          stroke: teal,
          width: 2,
          fill: 'rgba(16, 185, 129, 0.08)',
          points: { show: false },
          value
        },
        {
          label: 'Upload',
          stroke: blue,
          width: 2,
          fill: 'rgba(6, 182, 212, 0.08)',
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
  :global(.uplot) { background: transparent; font-family: inherit; }
  :global(.uplot .u-axis) { color: var(--muted); font-size: 11px; }
  :global(.uplot .u-cursor-x), :global(.uplot .u-cursor-y) { border-color: var(--muted); opacity: 0.4; }
</style>
