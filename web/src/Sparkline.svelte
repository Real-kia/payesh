<script lang="ts">
  let { tone, values = [] }: { tone: 'teal' | 'purple' | 'blue'; values?: Array<number | null> } = $props();

  function segments(series: Array<number | null>): string[] {
    const numeric = series.filter((value): value is number => value !== null && Number.isFinite(value));
    if (numeric.length < 2) return [];
    const min = Math.min(...numeric);
    const max = Math.max(...numeric);
    const span = max - min || 1;
    const output: string[] = [];
    let current: string[] = [];
    series.forEach((value, index) => {
      if (value === null || !Number.isFinite(value)) {
        if (current.length > 1) output.push(current.join(' '));
        current = [];
        return;
      }
      const x = (index / Math.max(1, series.length - 1)) * 160;
      const y = 38 - ((value - min) / span) * 31;
      current.push(`${x.toFixed(1)},${y.toFixed(1)}`);
    });
    if (current.length > 1) output.push(current.join(' '));
    return output;
  }
</script>

<svg viewBox="0 0 160 42" preserveAspectRatio="none" aria-hidden="true">
  {#each segments(values) as points}
    <polyline points={points} class={`spark-line ${tone}`} />
  {/each}
</svg>

<style>
  svg { height: 100%; width: 100%; }
  .spark-line { fill: none; stroke-width: 2.5; vector-effect: non-scaling-stroke; }
  .teal { stroke: var(--teal); }
  .purple { stroke: var(--purple); }
  .blue { stroke: var(--blue); }
</style>
