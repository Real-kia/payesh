<script lang="ts" module>
  let instance = 0;
</script>

<script lang="ts">
  let { tone, values = [] }: { tone: 'teal' | 'purple' | 'blue'; values?: Array<number | null> } = $props();

  // Each sparkline needs its own gradient id; a shared id breaks when the
  // first matching <svg> in the document is hidden or removed.
  const gradientId = `spark-grad-${++instance}`;

  type Point = { x: number; y: number };

  function segments(series: Array<number | null>): { line: string; area: string }[] {
    const numeric = series.filter((value): value is number => value !== null && Number.isFinite(value));
    if (numeric.length < 2) return [];
    const min = Math.min(...numeric);
    const max = Math.max(...numeric);
    const span = max - min || 1;
    const output: { line: string; area: string }[] = [];
    let current: Point[] = [];

    const flush = () => {
      if (current.length > 1) {
        const line = current.map((p) => `${p.x.toFixed(1)},${p.y.toFixed(1)}`).join(' ');
        const firstX = current[0].x.toFixed(1);
        const lastX = current[current.length - 1].x.toFixed(1);
        output.push({ line, area: `${firstX},42 ${line} ${lastX},42` });
      }
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

<svg viewBox="0 0 160 42" preserveAspectRatio="none" aria-hidden="true" class={tone}>
  <defs>
    <linearGradient id={gradientId} x1="0%" y1="0%" x2="0%" y2="100%">
      <stop offset="0%" class="stop-top" />
      <stop offset="100%" class="stop-bottom" />
    </linearGradient>
  </defs>
  {#each drawn as seg}
    <polygon points={seg.area} fill={`url(#${gradientId})`} />
    <polyline points={seg.line} class="spark-line" />
  {/each}
</svg>

<style>
  svg { height: 100%; width: 100%; overflow: visible; --spark: var(--accent); }
  svg.purple { --spark: var(--purple); }
  svg.blue { --spark: var(--blue); }
  .spark-line { fill: none; stroke: var(--spark); stroke-width: 2; stroke-linecap: round; stroke-linejoin: round; vector-effect: non-scaling-stroke; }
  .stop-top { stop-color: var(--spark); stop-opacity: 0.28; }
  .stop-bottom { stop-color: var(--spark); stop-opacity: 0; }
</style>
