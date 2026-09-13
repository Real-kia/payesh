<script lang="ts">
  import { onMount } from 'svelte';
  import uPlot from 'uplot';
  import 'uplot/dist/uPlot.min.css';
  import type { PreviewChartData, PreviewChartRange } from './preview/fixtures';

  let { label = 'Resource chart', range = '15m', data }: { label?: string; range?: PreviewChartRange; data: PreviewChartData } = $props();
  let host: HTMLDivElement;
  let plot: uPlot | undefined;

  function formatElapsed(value: number, selectedRange: PreviewChartRange): string {
    const seconds = Math.max(0, Math.round(value));
    if (selectedRange === '24h') {
      const hours = Math.floor(seconds / 3600);
      return `${hours}h`;
    }
    const minutes = Math.floor(seconds / 60);
    return `${minutes}m`;
  }

  onMount(() => {
    if (data.coverage === 'unavailable' || data.timestamps.length < 2) return;
    const styles = getComputedStyle(host);
    const muted = styles.getPropertyValue('--muted').trim() || '#5c6a63';
    const line = styles.getPropertyValue('--line').trim() || '#dfe6e1';
    const teal = styles.getPropertyValue('--teal').trim() || '#086b5c';
    const purple = styles.getPropertyValue('--purple').trim() || '#6e4bb2';
    const chartData: uPlot.AlignedData = [data.timestamps, data.cpu, data.memory];
    const value = (_u: uPlot, raw: number | null) => raw == null || !Number.isFinite(raw) ? '—' : raw.toFixed(2);
    plot = new uPlot({
      width: Math.max(240, host.clientWidth || 760),
      height: 220,
      scales: { x: { time: false }, y: { range: [0, 100] } },
      axes: [
        { stroke: muted, grid: { stroke: line }, values: (_u, values) => values.map((value) => formatElapsed(Number(value), range)) },
        { stroke: muted, grid: { stroke: line }, values: (_u, values) => values.map((value) => `${value}%`) }
      ],
      cursor: { focus: { prox: 30 } },
      series: [{}, { label: 'CPU', stroke: teal, width: 2, value }, { label: 'Memory', stroke: purple, width: 2, value }]
    }, chartData, host);
    const resize = new ResizeObserver(([entry]) => plot?.setSize({ width: Math.max(240, entry.contentRect.width), height: 220 }));
    resize.observe(host);
    return () => { resize.disconnect(); plot?.destroy(); };
  });
</script>

<div bind:this={host} class="chart-host" role="img" aria-label={label}></div>

<style>
  .chart-host { min-height: 250px; width: 100%; }
  :global(.uplot) { background: transparent; font-family: inherit; }
  :global(.uplot .u-axis) { color: var(--muted); }
</style>
