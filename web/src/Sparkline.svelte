<script lang="ts">
  let { values = [] }: { values?: Array<number | null> } = $props();

  type Point = { x: number; y: number };

  function segments(series: Array<number | null>): string[] {
    const numeric = series.filter((value): value is number => value !== null && Number.isFinite(value));
    if (numeric.length < 2) return [];
    const min = Math.min(...numeric);
    const max = Math.max(...numeric);
    const span = max - min || 1;
    const output: string[] = [];
    let current: Point[] = [];

    const flush = () => {
      if (current.length > 1) output.push(current.map((p) => `${p.x.toFixed(1)},${p.y.toFixed(1)}`).join(' '));
      current = [];
    };

    series.forEach((value, index) => {
      if (value === null || !Number.isFinite(value)) {
        flush();
        return;
      }
      const x = (index / Math.max(1, series.length - 1)) * 160;
      const y = 36 - ((value - min) / span) * 28;
      current.push({ x, y });
    });
    flush();
    return output;
  }

  const drawn = $derived(segments(values));
</script>

<svg viewBox="0 0 160 42" preserveAspectRatio="none" aria-hidden="true">
  {#each drawn as line}<polyline points={line} />{/each}
</svg>

<style>
  svg { height: 100%; width: 100%; overflow: visible; }
  polyline { fill: none; stroke: var(--muted); stroke-width: 1.25; stroke-linecap: round; stroke-linejoin: round; vector-effect: non-scaling-stroke; }
</style>
