<script lang="ts">
  import { onMount } from 'svelte';
  import uPlot from 'uplot';
  import 'uplot/dist/uPlot.min.css';
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
    const styles = getComputedStyle(host);
    const muted = styles.getPropertyValue('--muted').trim() || '#94a3b8';
    const line = styles.getPropertyValue('--line').trim() || 'rgba(255, 255, 255, 0.08)';
    const teal = styles.getPropertyValue('--teal').trim() || '#10b981';
    const purple = styles.getPropertyValue('--purple').trim() || '#8b5cf6';
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
          ticks: { stroke: line, width: 1 },
          font: '12px Segoe UI, Arial, sans-serif',
          values: (_u, values) => values.map((value) => formatClock(Number(value)))
        },
        {
          stroke: muted,
          grid: { stroke: line, width: 1 },
          ticks: { stroke: line, width: 1 },
          font: '12px Segoe UI, Arial, sans-serif',
          values: (_u, values) => values.map((value) => `${value}%`)
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
          label: 'CPU',
          stroke: teal,
          width: 2,
          fill: 'rgba(16, 185, 129, 0.08)',
          points: { show: false },
          value
        },
        {
          label: 'Memory',
          stroke: purple,
          width: 2,
          fill: 'rgba(139, 92, 246, 0.08)',
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
  :global(.uplot) { background: transparent; font-family: inherit; }
  :global(.uplot .u-axis) { color: var(--muted); font-size: 11px; }
  :global(.uplot .u-cursor-x), :global(.uplot .u-cursor-y) { border-color: var(--muted); opacity: 0.4; }
</style>
