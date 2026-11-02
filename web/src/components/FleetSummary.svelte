<script lang="ts">
  import { formatNetworkRate } from '../network';
  import type { DisplayState, PreviewServer } from '../preview/fixtures';

  let { servers, download, upload, detailed = false }: {
    servers: PreviewServer[];
    download: number | null;
    upload: number | null;
    detailed?: boolean;
  } = $props();

  const order: DisplayState[] = ['healthy', 'installing', 'pending', 'stale', 'failed', 'unreachable', 'disabled', 'unsupported'];
  // The bar groups states by severity so it reads at a glance instead of as a rainbow.
  const tone: Record<DisplayState, 'ok' | 'busy' | 'warn' | 'bad' | 'off'> = {
    healthy: 'ok', installing: 'busy', pending: 'warn', stale: 'warn', failed: 'bad', unreachable: 'bad', disabled: 'off', unsupported: 'off'
  };
  const healthy = $derived(servers.filter((server) => server.displayState === 'healthy').length);
  const attention = $derived(servers.length - healthy);
  const breakdown = $derived(order.map((state) => ({ state, count: servers.filter((server) => server.displayState === state).length })).filter((entry) => entry.count > 0));
  const segments = $derived((['ok', 'busy', 'warn', 'bad', 'off'] as const).map((key) => {
    const entries = breakdown.filter((entry) => tone[entry.state] === key);
    return { tone: key, count: entries.reduce((sum, entry) => sum + entry.count, 0), title: entries.map((entry) => `${entry.count} ${entry.state}`).join(', ') };
  }).filter((segment) => segment.count > 0));
  const problems = $derived(breakdown.filter((entry) => entry.state !== 'healthy'));
  const healthyShare = $derived(servers.length ? Math.round((healthy / servers.length) * 100) : 0);
</script>

<section class="summary" aria-label="Fleet summary">
  <div class="cell">
    <span class="label">Healthy</span>
    <strong class="value tabular">{healthy}<small> / {servers.length}</small></strong>
    {#if servers.length}
      <div class="health-bar" role="img" aria-label={breakdown.map((entry) => `${entry.count} ${entry.state}`).join(', ')}>
        {#each segments as segment (segment.tone)}<span class={segment.tone} style:flex-grow={segment.count} title={segment.title}></span>{/each}
      </div>
    {/if}
    <span class="note">{servers.length ? `${healthyShare}% responding normally` : 'No servers yet'}</span>
  </div>

  <div class="cell">
    <span class="label">Needs attention</span>
    <strong class="value tabular" class:warn={attention > 0}>{attention}</strong>
    {#if problems.length}
      <p class="breakdown">
        {#each problems as entry, index (entry.state)}{#if index > 0}<span class="sep" aria-hidden="true">·</span>{/if}<span class={tone[entry.state]}><b class="tabular">{entry.count}</b> {entry.state}</span>{/each}
      </p>
    {:else}
      <span class="note">Every server is reporting</span>
    {/if}
  </div>

  <div class="cell">
    <span class="label">Fleet bandwidth</span>
    <dl class="rates">
      <div><dt>In</dt><dd class="tabular">{formatNetworkRate(download)}</dd></div>
      <div><dt>Out</dt><dd class="tabular">{formatNetworkRate(upload)}</dd></div>
    </dl>
    <span class="note">Current rate across connected servers</span>
  </div>
</section>

<style>
  .summary {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    margin-bottom: 28px;
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
  }
  .cell { display: flex; flex-direction: column; gap: 8px; min-width: 0; padding: var(--card-pad) 20px; }
  .cell + .cell { border-left: 1px solid var(--line); }
  .label { color: var(--muted); font-size: 12.5px; font-weight: 500; }
  .value { font-size: 26px; font-weight: 600; letter-spacing: -0.02em; line-height: 1.1; }
  .value small { color: var(--muted); font-size: 15px; font-weight: 400; letter-spacing: 0; }
  .value.warn { color: var(--warning); }
  .note { color: var(--muted); font-size: 12px; }

  .health-bar { display: flex; gap: 2px; height: 4px; overflow: hidden; border-radius: 2px; }
  .health-bar span { min-width: 3px; background: var(--line-strong); }
  .health-bar .ok { background: var(--success); }
  .health-bar .busy { background: var(--info); }
  .health-bar .warn { background: var(--warning); }
  .health-bar .bad { background: var(--danger); }

  .breakdown { display: flex; flex-wrap: wrap; gap: 2px 6px; margin: 0; color: var(--muted); font-size: 12.5px; }
  .breakdown b { color: var(--ink); font-weight: 600; }
  .breakdown .warn b { color: var(--warning); }
  .breakdown .bad b { color: var(--danger); }
  .sep { opacity: 0.6; }

  .rates { display: grid; grid-template-columns: auto 1fr; gap: 2px 12px; margin: 0; }
  .rates > div { display: contents; }
  .rates dt { color: var(--muted); font-size: 12.5px; line-height: 26px; }
  .rates dd { overflow: hidden; margin: 0; font-size: 17px; font-weight: 600; line-height: 26px; text-overflow: ellipsis; white-space: nowrap; }

  @media (max-width: 960px) {
    .summary { grid-template-columns: 1fr 1fr; }
    .cell:nth-child(3) { grid-column: 1 / -1; border-left: 0; border-top: 1px solid var(--line); }
  }
  @media (max-width: 480px) {
    .summary { grid-template-columns: 1fr; }
    .cell + .cell { border-left: 0; border-top: 1px solid var(--line); }
    .value { font-size: 22px; }
  }
</style>
