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
  <label class="tz">Time zone
    <select value={timezone} onchange={(event) => onTimezone(event.currentTarget.value)}>
      {#each zones as tz (tz)}<option value={tz}>{tz}</option>{/each}
    </select>
  </label>
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
  @media (max-width: 760px) {
    .option { grid-template-columns: 1fr; gap: 10px; }
  }
</style>
