<script lang="ts">
  import { onMount } from 'svelte';
  import uPlot from 'uplot';
  import 'uplot/dist/uPlot.min.css';
  import type { PreviewChartData } from './preview/fixtures';

  let { data }: { data: PreviewChartData } = $props();
  let host: HTMLDivElement;
  let plot: uPlot | undefined;
  const rate = (value: number) => value >= 1_000_000 ? `${(value / 1_000_000).toFixed(1)} MB/s` : value >= 1_000 ? `${(value / 1_000).toFixed(1)} KB/s` : `${value.toFixed(0)} B/s`;

  onMount(() => {
    if (!data.networkRx || !data.networkTx || data.timestamps.length < 2) return;
    const styles = getComputedStyle(host);
    const muted = styles.getPropertyValue('--muted').trim();
    const line = styles.getPropertyValue('--line').trim();
    plot = new uPlot({ width: Math.max(240, host.clientWidth), height: 220, scales: { x: { time: false } }, axes: [{ stroke: muted, grid: { stroke: line }, values: (_u, values) => values.map((v) => `${Math.round(Number(v) / 60)}m`) }, { stroke: muted, grid: { stroke: line }, values: (_u, values) => values.map((v) => rate(Number(v))) }], series: [{}, { label: 'Download', stroke: '#65d9c2', width: 2 }, { label: 'Upload', stroke: '#81b7ff', width: 2 }] }, [data.timestamps, data.networkRx, data.networkTx] as uPlot.AlignedData, host);
    const resize = new ResizeObserver(([entry]) => plot?.setSize({ width: Math.max(240, entry.contentRect.width), height: 220 })); resize.observe(host);
    return () => { resize.disconnect(); plot?.destroy(); };
  });
</script>
<div bind:this={host} class="chart-host" role="img" aria-label="Network download and upload rate history"></div>
<style>.chart-host { min-height: 220px; width: 100%; }</style>
