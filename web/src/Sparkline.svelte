<script lang="ts">
  let { tone, values = [] }: { tone: 'teal' | 'purple' | 'blue'; values?: Array<number | null> } = $props();

  function segments(series: Array<number | null>): { line: string; area: string }[] {
    const numeric = series.filter((value): value is number => value !== null && Number.isFinite(value));
    if (numeric.length < 2) return [];
    const min = Math.min(...numeric);
    const max = Math.max(...numeric);
    const span = max - min || 1;
    const output: { line: string; area: string }[] = [];
    let current: { x: number; y: number }[] = [];

    const flush = () => {
      if (current.length > 1) {
        const line = current.map((p) => `${p.x.toFixed(1)},${p.y.toFixed(1)}`).join(' ');
        const firstX = current[0].x.toFixed(1);
        const lastX = current[current.length - 1].x.toFixed(1);
        const area = `${firstX},42 ${line} ${lastX},42`;
        output.push({ line, area });
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
</script>

<svg viewBox="0 0 160 42" preserveAspectRatio="none" aria-hidden="true">
  <defs>
    <linearGradient id={`grad-${tone}`} x1="0%" y1="0%" x2="0%" y2="100%">
      <stop offset="0%" class={`grad-stop-0 ${tone}`} />
      <stop offset="100%" class={`grad-stop-100 ${tone}`} />
    </linearGradient>
  </defs>
  {#each segments(values) as seg}
    <polygon points={seg.area} fill={`url(#grad-${tone})`} />
    <polyline points={seg.line} class={`spark-line ${tone}`} />
  {/each}
</svg>

<style>
  svg { height: 100%; width: 100%; overflow: visible; }
  .spark-line { fill: none; stroke-width: 2; stroke-linecap: round; stroke-linejoin: round; vector-effect: non-scaling-stroke; }
  .teal { stroke: var(--teal); }
  .purple { stroke: var(--purple); }
  .blue { stroke: var(--blue); }

  .grad-stop-0.teal { stop-color: var(--teal); stop-opacity: 0.25; }
  .grad-stop-100.teal { stop-color: var(--teal); stop-opacity: 0.0; }
  .grad-stop-0.purple { stop-color: var(--purple); stop-opacity: 0.25; }
  .grad-stop-100.purple { stop-color: var(--purple); stop-opacity: 0.0; }
  .grad-stop-0.blue { stop-color: var(--blue); stop-opacity: 0.25; }
  .grad-stop-100.blue { stop-color: var(--blue); stop-opacity: 0.0; }
</style>
