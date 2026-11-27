<script lang="ts">
  import type { FleetTrafficHistory } from '../api';
  import { PREVIEW_MODE } from '../lib/env';
  import type { PreviewServer } from '../preview/fixtures';
  import FleetTrafficSeries from './FleetTrafficSeries.svelte';

  let { servers, timezone, theme, onAuthExpired, onOpenStorage }: { servers: PreviewServer[]; timezone: string; theme: string; onAuthExpired: () => void; onOpenStorage?: () => void } = $props();

  const modes = ['Bandwidth', 'Traffic'] as const;
  let mode = $state<(typeof modes)[number]>('Bandwidth');
  const ranges = [[24, '24 hours'], [168, '7 days'], [720, '30 days']] as const;
  let range = $state(24);
  let selectedTags = $state<string[]>([]);
  let savedTags = $state<string[]>([]);
  let fleetHasTraffic = $state(false);

  function receiveHistory(history: FleetTrafficHistory) {
    savedTags = history.tags ?? [];
    fleetHasTraffic = history.enabled || history.hours.some((entry) => entry.download_bytes !== null || entry.upload_bytes !== null);
  }

  const tagOptions = $derived.by(() => {
    const labels = new Map([...selectedTags, ...savedTags].map((label) => [label, label]));
    for (const server of servers) {
      for (const label of server.tags ?? []) labels.set(label.toLowerCase(), label);
    }
    return [...labels].sort(([, a], [, b]) => a.localeCompare(b));
  });
</script>

<article class="panel fleet-traffic">
  <div class="panel-heading">
    <div><h2>Fleet traffic history</h2><span class="muted">{mode === 'Bandwidth' ? 'Hourly average of recorded traffic · Mbit/s' : range === 24 ? 'Recorded hourly totals' : 'Recorded daily totals'} · {timezone}</span></div>
    <div class="filters">
      {#if !PREVIEW_MODE && tagOptions.length}
        <details class="tag-picker">
          <summary>Server tags · {selectedTags.length ? `${selectedTags.length} selected` : 'All servers'}</summary>
          <div class="tag-options">
            <button type="button" class="button ghost small" onclick={() => (selectedTags = [])}>All servers</button>
            {#each tagOptions as [key, label] (key)}
              <label><input type="checkbox" value={key} bind:group={selectedTags} /> {label}</label>
            {/each}
          </div>
        </details>
      {/if}
      {#if fleetHasTraffic || selectedTags.length}
        <div class="segmented" role="group" aria-label="Fleet traffic mode">
          {#each modes as option}
            <button type="button" aria-pressed={mode === option} onclick={() => (mode = option)}>{option}</button>
          {/each}
        </div>
        <div class="segmented" role="group" aria-label="Fleet traffic range">
          {#each ranges as [hours, label]}
            <button type="button" aria-pressed={range === hours} onclick={() => (range = hours)}>{label}</button>
          {/each}
        </div>
      {/if}
    </div>
  </div>
  <FleetTrafficSeries {servers} {timezone} {theme} {onAuthExpired} {onOpenStorage} {mode} {range}
    tag="" visible={!selectedTags.length} onHistory={receiveHistory} />
  {#each tagOptions.filter(([key]) => selectedTags.includes(key)) as [key, label] (key)}
    <FleetTrafficSeries {servers} {timezone} {theme} {onAuthExpired} {onOpenStorage} {mode} {range}
      tag={key} {label} knownTag={savedTags.includes(key)} />
  {/each}
</article>

<style>
  .fleet-traffic { margin-bottom: 24px; min-width: 0; }
  .panel-heading { flex-wrap: wrap; gap: 12px; }
  .filters { display: flex; align-items: center; flex-wrap: wrap; gap: 8px 12px; }
  .tag-picker { position: relative; font-size: 13px; }
  summary { cursor: pointer; border: 1px solid var(--line); border-radius: var(--radius-sm); padding: 7px 10px; }
  .tag-options { position: absolute; z-index: 2; inset-inline-start: 0; min-width: 100%; max-height: 240px; overflow: auto; padding: 6px; border: 1px solid var(--line); border-radius: var(--radius-sm); background: var(--surface); }
  .tag-options label { display: flex; align-items: center; gap: 8px; padding: 4px 6px; cursor: pointer; }
  .tag-options input { accent-color: var(--accent); }
</style>
