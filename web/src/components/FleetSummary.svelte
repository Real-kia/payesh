<script lang="ts">
  import Icon from '../Icon.svelte';
  import { formatNetworkRate } from '../network';
  import type { DisplayState, PreviewServer } from '../preview/fixtures';

  let { servers, download, upload, detailed = false }: {
    servers: PreviewServer[];
    download: number | null;
    upload: number | null;
    detailed?: boolean;
  } = $props();

  const order: DisplayState[] = ['healthy', 'installing', 'pending', 'stale', 'failed', 'unreachable', 'disabled', 'unsupported'];
  const healthy = $derived(servers.filter((server) => server.displayState === 'healthy').length);
  const attention = $derived(servers.length - healthy);
  const breakdown = $derived(order.map((state) => ({ state, count: servers.filter((server) => server.displayState === state).length })).filter((entry) => entry.count > 0));
  const healthyShare = $derived(servers.length ? Math.round((healthy / servers.length) * 100) : 0);
</script>

<div class="summary-grid stagger">
  <article class="summary-card" style:--i={0}>
    <div class="card-header">
      <span class="stat-label">Healthy servers</span>
      <span class="stat-icon success"><Icon name="check" size={15} /></span>
    </div>
    <strong class="stat-value tabular">{healthy}<small> / {servers.length}</small></strong>
    {#if servers.length}
      <div class="health-bar" role="img" aria-label={breakdown.map((entry) => `${entry.count} ${entry.state}`).join(', ')}>
        {#each breakdown as entry (entry.state)}<span class={entry.state} style:flex-grow={entry.count} title={`${entry.count} ${entry.state}`}></span>{/each}
      </div>
    {/if}
    <span class="stat-note">{servers.length ? `${healthyShare}% healthy and responding` : 'No servers yet'}</span>
  </article>

  <article class="summary-card" class:attention={attention > 0} style:--i={1}>
    <div class="card-header">
      <span class="stat-label">Needs attention</span>
      <span class="stat-icon warning"><Icon name="alert-triangle" size={15} /></span>
    </div>
    <strong class="stat-value tabular">{attention}</strong>
    {#if detailed && breakdown.some((entry) => entry.state !== 'healthy')}
      <div class="chips">
        {#each breakdown.filter((entry) => entry.state !== 'healthy') as entry (entry.state)}<span class={`chip ${entry.state}`}>{entry.count} {entry.state}</span>{/each}
      </div>
    {:else}
      <span class="stat-note">{attention > 0 ? 'Stale, pending, or offline servers' : 'Every server is reporting'}</span>
    {/if}
  </article>

  <article class="summary-card network" style:--i={2}>
    <div class="card-header">
      <span class="stat-label">Fleet bandwidth</span>
      <span class="stat-icon info"><Icon name="activity" size={15} /></span>
    </div>
    <div class="network-split">
      <div>
        <span class="rate-title"><span class="rate-badge down"><Icon name="download" size={11} /></span>Download</span>
        <strong class="tabular">{formatNetworkRate(download)}</strong>
      </div>
      <div>
        <span class="rate-title"><span class="rate-badge up"><Icon name="upload" size={11} /></span>Upload</span>
        <strong class="tabular">{formatNetworkRate(upload)}</strong>
      </div>
    </div>
    <span class="stat-note">Live rate across connected servers</span>
  </article>
</div>

<style>
  .summary-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: var(--gap); margin-bottom: 24px; }
  .summary-card {
    position: relative;
    display: flex;
    flex-direction: column;
    gap: 10px;
    min-width: 0;
    padding: var(--card-pad) 20px;
    overflow: hidden;
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
    box-shadow: var(--shadow);
  }
  /* A faint corner wash gives each card a colour identity at no runtime cost. */
  .summary-card::before {
    content: '';
    position: absolute;
    inset: 0;
    pointer-events: none;
    background: radial-gradient(120% 90% at 100% 0%, var(--card-wash, transparent), transparent 55%);
  }
  .summary-card:nth-child(1) { --card-wash: var(--success-bg); }
  .summary-card.attention { --card-wash: var(--warning-bg); }
  .summary-card.network { --card-wash: var(--info-bg); }
  .card-header { display: flex; align-items: center; justify-content: space-between; }
  .stat-label { color: var(--muted); font-size: 12.5px; font-weight: 550; }
  .stat-icon { display: grid; place-items: center; width: 28px; height: 28px; border-radius: var(--radius-md); }
  .stat-icon.success { background: var(--success-bg); color: var(--success); }
  .stat-icon.warning { background: var(--warning-bg); color: var(--warning); }
  .stat-icon.info { background: var(--info-bg); color: var(--info); }
  .stat-value { font-size: 30px; font-weight: 700; letter-spacing: -0.035em; line-height: 1; }
  .stat-value small { color: var(--muted); font-size: 15px; font-weight: 500; letter-spacing: 0; }
  .stat-note { color: var(--muted); font-size: 12px; }

  .health-bar { display: flex; gap: 2px; height: 6px; overflow: hidden; border-radius: var(--radius-pill); }
  .health-bar span { min-width: 4px; border-radius: 2px; background: var(--line-strong); transition: flex-grow var(--dur-slow) var(--ease-out); }
  .health-bar .healthy { background: var(--success); }
  .health-bar .installing { background: var(--info); }
  .health-bar .pending, .health-bar .stale { background: var(--warning); }
  .health-bar .failed, .health-bar .unreachable { background: var(--danger); }

  .chips { display: flex; flex-wrap: wrap; gap: 4px; }
  .chip { padding: 2px 7px; border-radius: var(--radius-pill); background: var(--surface-muted); color: var(--muted); font-size: 11px; font-weight: 600; text-transform: capitalize; }
  .chip.stale, .chip.pending { background: var(--warning-bg); color: var(--warning); }
  .chip.failed, .chip.unreachable { background: var(--danger-bg); color: var(--danger); }
  .chip.installing { background: var(--info-bg); color: var(--info); }

  .network-split { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
  .network-split > div { display: grid; gap: 6px; min-width: 0; }
  .network-split > div + div { padding-left: 14px; border-left: 1px solid var(--line); }
  .network-split strong { overflow: hidden; font-size: 17px; font-weight: 650; letter-spacing: -0.01em; text-overflow: ellipsis; white-space: nowrap; }
  .rate-title { display: inline-flex; align-items: center; gap: 6px; color: var(--muted); font-size: 11px; font-weight: 600; letter-spacing: 0.04em; text-transform: uppercase; }
  .rate-badge { display: inline-grid; place-items: center; width: 18px; height: 18px; border-radius: var(--radius-xs); }
  .rate-badge.down { background: var(--accent-bg); color: var(--accent); }
  .rate-badge.up { background: var(--blue-bg); color: var(--blue); }

  @media (max-width: 960px) {
    .summary-grid { grid-template-columns: 1fr 1fr; }
    .summary-card.network { grid-column: 1 / -1; }
  }
  @media (max-width: 480px) {
    .summary-card { padding: 14px 16px; }
    .stat-value { font-size: 25px; }
  }
</style>
