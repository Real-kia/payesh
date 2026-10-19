<script lang="ts">
  import Icon from '../../Icon.svelte';
  import { ACCENTS, type Accent, type Appearance, type Density, type Motion, type ThemeMode } from '../../lib/appearance';
  import { formatDateTimeInTz, formatTimeInTz, getAvailableTimezones } from '../../timezone';

  let { appearance, timezone, onAppearance, onTimezone }: {
    appearance: Appearance;
    timezone: string;
    onAppearance: (next: Appearance) => void;
    onTimezone: (tz: string) => void;
  } = $props();

  const zones = getAvailableTimezones();
  let zoneQuery = $state('');
  let zoneOpen = $state(false);
  let zoneIndex = $state(0);
  let zonePicker: HTMLDivElement;
  const filteredZones = $derived(zones.filter((zone) => !zoneQuery.trim() || zone.toLowerCase().includes(zoneQuery.trim().toLowerCase())));
  function selectZone(tz: string) { onTimezone(tz); zoneOpen = false; zoneQuery = ''; }
  function zoneKey(event: KeyboardEvent) {
    if (event.key === 'Escape') zoneOpen = false;
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault(); zoneOpen = true;
      zoneIndex = Math.max(0, Math.min(filteredZones.length - 1, zoneIndex + (event.key === 'ArrowDown' ? 1 : -1)));
      document.getElementById(`timezone-option-${zoneIndex}`)?.scrollIntoView({ block: 'nearest' });
    }
    if (event.key === 'Enter' && filteredZones[zoneIndex]) { event.preventDefault(); selectZone(filteredZones[zoneIndex]); }
  }
  let now = $state(new Date());
  $effect(() => {
    const timer = window.setInterval(() => (now = new Date()), 30000);
    return () => window.clearInterval(timer);
  });

  const modes: { id: ThemeMode; label: string; icon: string }[] = [
    { id: 'light', label: 'Light', icon: 'sun' },
    { id: 'dark', label: 'Dark', icon: 'moon' },
    { id: 'system', label: 'System', icon: 'monitor' }
  ];

  const set = (patch: Partial<Appearance>) => onAppearance({ ...appearance, ...patch });
</script>

<svelte:window onpointerdown={(event) => { if (zonePicker && !zonePicker.contains(event.target as Node)) zoneOpen = false; }} />

<article class="panel">
  <div class="panel-heading">
    <div class="title"><span class="section-icon"><Icon name="palette" size={16} /></span><div><h2>Appearance</h2><span class="muted">Saved in this browser only.</span></div></div>
  </div>

  <div class="option">
    <div><strong>Theme</strong><small>System follows your operating system setting.</small></div>
    <div class="theme-cards" role="radiogroup" aria-label="Theme">
      {#each modes as mode (mode.id)}
        <button type="button" role="radio" aria-checked={appearance.mode === mode.id} class={`theme-card ${mode.id}`} onclick={() => set({ mode: mode.id })}>
          <span class="preview" aria-hidden="true"><span class="bar"></span><span class="block"></span><span class="block short"></span></span>
          <span class="label"><Icon name={mode.icon} size={13} />{mode.label}</span>
        </button>
      {/each}
    </div>
  </div>

  <div class="option">
    <div><strong>Accent colour</strong><small>Used for highlights, charts and primary buttons.</small></div>
    <div class="swatches" role="radiogroup" aria-label="Accent colour">
      {#each ACCENTS as accent (accent.id)}
        <button type="button" role="radio" aria-checked={appearance.accent === accent.id} class="swatch" style:--swatch={accent.swatch} title={accent.label} onclick={() => set({ accent: accent.id as Accent })}>
          <span class="chip" aria-hidden="true">{#if appearance.accent === accent.id}<Icon name="check" size={13} />{/if}</span>
          <span>{accent.label}</span>
        </button>
      {/each}
    </div>
  </div>

  <div class="option">
    <div><strong>Animations</strong><small>Reduced turns off motion; useful on slow machines or remote desktops.</small></div>
    <div class="segmented" role="group" aria-label="Animations">
      {#each [['full', 'Full'], ['reduced', 'Reduced']] as [id, label] (id)}
        <button type="button" aria-pressed={appearance.motion === id} onclick={() => set({ motion: id as Motion })}>{label}</button>
      {/each}
    </div>
  </div>

  <div class="option">
    <div><strong>Density</strong><small>Compact fits more servers on screen.</small></div>
    <div class="segmented" role="group" aria-label="Density">
      {#each [['comfortable', 'Comfortable'], ['compact', 'Compact']] as [id, label] (id)}
        <button type="button" aria-pressed={appearance.density === id} onclick={() => set({ density: id as Density })}>{label}</button>
      {/each}
    </div>
  </div>
</article>

<article class="panel">
  <div class="panel-heading">
    <div class="title"><span class="section-icon"><Icon name="clock" size={16} /></span><div><h2>Time zone</h2><span class="muted">Charts, logs and alerts use this time zone.</span></div></div>
  </div>
  <div class="settings-meta-box">
    <div class="meta-row"><span>Active time zone</span><strong>{timezone}</strong></div>
    <div class="meta-row"><span>Local time</span><strong class="tabular">{formatTimeInTz(now, timezone, true)} · {formatDateTimeInTz(now, timezone)}</strong></div>
  </div>
  <div class="tz">
    <label for="timezone-search">Time zone</label>
    <div class="tz-picker" bind:this={zonePicker} onfocusout={(event) => { if (!event.currentTarget.contains(event.relatedTarget as Node)) zoneOpen = false; }}>
      <input id="timezone-search" type="search" role="combobox" placeholder="Search time zones" value={zoneOpen ? zoneQuery : timezone} aria-expanded={zoneOpen} aria-controls="timezone-options" onfocus={() => { zoneOpen = true; zoneQuery = ''; }} oninput={(event) => { zoneOpen = true; zoneQuery = event.currentTarget.value; zoneIndex = 0; }} onkeydown={zoneKey} />
      {#if zoneOpen}
        <div id="timezone-options" class="tz-options" role="listbox" aria-label="Time zones">
          {#each filteredZones as tz, index (tz)}
            <!-- Keep the input focused until click selects the option. Safari
                 can blur it without focusing the button, closing the list first. -->
            <button type="button" id={`timezone-option-${index}`} role="option" aria-selected={tz === timezone} onpointerdown={(event) => event.preventDefault()} onclick={() => selectZone(tz)}>{tz}</button>
          {:else}<span class="muted tz-empty">No matching time zones</span>{/each}
        </div>
      {/if}
    </div>
  </div>
</article>

<style>
  .option { display: grid; grid-template-columns: minmax(180px, 0.8fr) minmax(0, 1.4fr); gap: 16px; align-items: center; padding: 16px 0; border-top: 1px solid var(--line-light); }
  .option:first-of-type { border-top: 0; padding-top: 4px; }
  .option > div:first-child { display: grid; gap: 3px; }
  .option strong { font-size: 13.5px; }
  .option small { color: var(--muted); font-size: 12px; line-height: 1.45; }

  .theme-cards { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 10px; }
  .theme-card { display: grid; gap: 8px; padding: 8px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface); color: var(--ink-secondary); text-align: left; transition: border-color var(--transition-fast), box-shadow var(--transition-fast), transform var(--transition-fast); }
  .theme-card:hover { border-color: var(--line-strong); transform: translateY(-1px); }
  .theme-card[aria-checked='true'] { border-color: var(--accent); box-shadow: var(--ring); color: var(--ink); }
  .preview { display: grid; grid-template-columns: 18px 1fr; grid-template-rows: 1fr 1fr; gap: 4px; height: 48px; padding: 6px; border-radius: var(--radius-sm); }
  .preview .bar { grid-row: 1 / -1; border-radius: 3px; }
  .preview .block { border-radius: 3px; }
  .preview .block.short { width: 60%; }
  .theme-card.light .preview { background: #eef1f5; }
  .theme-card.light .bar, .theme-card.light .block { background: #ffffff; }
  .theme-card.dark .preview { background: #0d1218; }
  .theme-card.dark .bar, .theme-card.dark .block { background: #1d2631; }
  .theme-card.system .preview { background: linear-gradient(135deg, #eef1f5 50%, #0d1218 50%); }
  .theme-card.system .bar, .theme-card.system .block { background: color-mix(in srgb, #ffffff 55%, #1d2631); }
  .theme-card .block:first-of-type { background: var(--accent); opacity: 0.85; }
  .label { display: inline-flex; align-items: center; gap: 6px; font-size: 12.5px; font-weight: 600; }

  .swatches { display: flex; flex-wrap: wrap; gap: 8px; }
  .swatch { display: inline-flex; align-items: center; gap: 8px; padding: 6px 12px 6px 6px; border: 1px solid var(--line); border-radius: var(--radius-pill); background: var(--surface); color: var(--ink-secondary); font-size: 12.5px; font-weight: 550; transition: border-color var(--transition-fast), box-shadow var(--transition-fast); }
  .swatch:hover { border-color: var(--line-strong); }
  .swatch[aria-checked='true'] { border-color: var(--swatch); box-shadow: 0 0 0 3px color-mix(in srgb, var(--swatch) 22%, transparent); color: var(--ink); }
  .chip { display: grid; place-items: center; width: 22px; height: 22px; border-radius: 50%; background: var(--swatch); color: #fff; }

  .tz { display: grid; gap: 7px; max-width: 360px; font-size: 13px; font-weight: 550; }
  .tz-picker { position: relative; }
  .tz-options { position: absolute; z-index: 4; top: calc(100% + 4px); right: 0; left: 0; max-height: 240px; overflow: auto; padding: 5px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface); box-shadow: var(--shadow-md); }
  .tz-options button { display: block; width: 100%; padding: 8px 10px; border: 0; border-radius: var(--radius-sm); background: transparent; color: var(--ink-secondary); text-align: left; font: inherit; font-size: 12.5px; cursor: pointer; }
  .tz-options button:hover, .tz-options button[aria-selected='true'] { background: var(--accent-bg); color: var(--accent); }
  .tz-empty { display: block; padding: 10px; font-size: 12px; }
  @media (max-width: 760px) {
    .option { grid-template-columns: 1fr; gap: 10px; }
  }
</style>
