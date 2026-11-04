<script lang="ts">
  import { followLink } from '../lib/navigation';
  import Icon from '../Icon.svelte';
  import FleetSummary from '../components/FleetSummary.svelte';
  import { filterServers, defaultFilter } from '../lib/serverFilter';
  import { serverLabel } from '../lib/flags';
  import LoadingSkeleton from '../components/LoadingSkeleton.svelte';
  import StatusPill from '../components/StatusPill.svelte';
  import LiveIndicator from '../components/LiveIndicator.svelte';
  import { formatBytes, percentage } from '../lib/format';
  import { PREVIEW_MODE } from '../lib/env';
  import type { Job } from '../api';
  import type { PreviewServer } from '../preview/fixtures';

  let {
    servers,
    loadState,
    apiError,
    readOnly,
    timezone,
    download,
    upload,
    trafficBytes,
    allowanceBytes,
    latestJob,
    jobError,
    jobBusy,
    onSelect,
    onAddServer,
    onNavigate,
    onRetry,
    onCancelJob,
    onReloadJob,
    refreshing = false,
    updatedAt = null,
    firingAlerts = 0
  }: {
    servers: PreviewServer[];
    loadState: 'ready' | 'loading' | 'empty' | 'error';
    apiError: string;
    readOnly: boolean;
    timezone: string;
    download: number | null;
    upload: number | null;
    trafficBytes: string;
    allowanceBytes: string;
    latestJob: Job | null;
    jobError: string;
    jobBusy: boolean;
    onSelect: (server: PreviewServer) => void;
    onAddServer: () => void;
    onNavigate: (page: 'monitoring' | 'alerts' | 'servers') => void;
    onRetry: () => void;
    onCancelJob: () => void;
    onReloadJob: () => void;
    refreshing?: boolean;
    updatedAt?: number | null;
    firingAlerts?: number;
  } = $props();

  const issues = $derived(filterServers(servers, { ...defaultFilter, status: 'attention' }));
  const healthy = $derived(servers.filter((server) => server.displayState === 'healthy').length);
  const attention = $derived(servers.length - healthy);
  const connected = $derived(servers.filter((server) => server.connectionState === 'connected').length);
  const usedShare = $derived(allowanceBytes === '0' ? 0 : percentage(trafficBytes, allowanceBytes));
</script>

<section class="page overview-page" aria-labelledby="overview-title">
  <div class="page-heading">
    <div>
      <h1 id="overview-title" tabindex="-1">Fleet overview</h1>
      <p class="lede">Fleet health, active issues, and billing at a glance.</p>
    </div>
    {#if PREVIEW_MODE}<span class="live-badge">Fixture preview</span>{:else}<LiveIndicator {refreshing} {updatedAt} />{/if}
  </div>

  {#if loadState === 'loading'}
    <LoadingSkeleton label="Connecting to fleet" />
  {:else if loadState === 'empty'}
    <div class="empty-onboarding">
      <div class="empty-main">
        <h2>Bring your first server online</h2>
        <p>Connect a Linux server over SSH. Payesh checks node transport reachability before installing the agent, then shows its live metrics here.</p>
        {#if readOnly}
          <p class="muted">Ask a user with edit access to add the first server.</p>
        {:else}
          <button class="button primary" type="button" onclick={onAddServer}><Icon name="plus" size={15} /><span>Add first server</span></button>
        {/if}
      </div>
      <ol class="empty-steps" aria-label="Server setup steps">
        <li><span>1</span><div><strong>Enter server details</strong><small>Hostname, SSH user, and one sign-in method.</small></div></li>
        <li><span>2</span><div><strong>Verify the connection</strong><small>Payesh checks SSH and the TLS node endpoint.</small></div></li>
        <li><span>3</span><div><strong>Monitor live metrics</strong><small>See health and resource data after enrollment.</small></div></li>
      </ol>
    </div>
  {:else if loadState === 'error'}
    <div class="state-panel error-state">
      <div class="state-icon"><Icon name="alert-triangle" size={22} /></div>
      <h2>Could not connect to fleet</h2>
      <p>{apiError || 'The API endpoint returned an error.'}</p>
      <button class="button primary" type="button" onclick={onRetry}><Icon name="refresh" size={14} />Retry connection</button>
    </div>
  {:else}
    <FleetSummary {servers} {download} {upload} />

    <article class="panel attention-panel">
      <div class="panel-heading">
        <div><h2>Needs attention <span class="count">{issues.length}</span></h2><span class="muted">Servers with connection, collection, or setup issues</span></div>
        <a class="button ghost small" href="/monitoring" onclick={(event) => followLink(event, () => onNavigate('monitoring'))}>Live monitoring <Icon name="chevron-right" size={14} /></a>
      </div>
      {#if issues.length}
        <div class="attention-list">
          {#each issues.slice(0, 5) as server (server.id)}
            <a class="attention-row" href={`/servers/${encodeURIComponent(server.id)}`} onclick={(event) => followLink(event, () => onSelect(server))}>
              <span class="issue-identity"><strong>{serverLabel(server)}</strong><small>{server.freshnessReason || (server.connectionState !== 'connected' ? 'Not connected to the hub' : 'Review server status')}</small></span>
              <StatusPill state={server.displayState} label={server.displayState.replace('-', ' ')} />
              <Icon name="chevron-right" size={14} />
            </a>
          {/each}
        </div>
        {#if issues.length > 5}<p class="hint">Showing 5 of {issues.length} servers needing attention.</p>{/if}
      {:else}
        <div class="all-healthy"><Icon name="check" size={18} /><div><strong>All servers healthy</strong><span>Every server is connected and reporting fresh data.</span></div></div>
      {/if}
      <div class="attention-footer"><span class="muted">{servers.length} servers in your fleet</span><a class="text-button" href="/servers" onclick={(event) => followLink(event, () => onNavigate('servers'))}>Manage servers <Icon name="chevron-right" size={14} /></a></div>
    </article>

    <div class="lower-grid">
      <article class="panel">
        <div class="panel-heading">
          <h2>Allowance overview</h2>
          {#if servers[0]}<a class="text-button" href={`/servers/${encodeURIComponent(servers[0].id)}`} onclick={(event) => followLink(event, () => onSelect(servers[0]))}>View metrics <Icon name="chevron-right" size={14} /></a>{/if}
        </div>
        {#if allowanceBytes === '0'}
          <div class="unavailable-panel">
            <strong>No allowance quota configured</strong>
            <span>Open a server's Network tab to review live interface telemetry.</span>
          </div>
        {:else}
          <div class="allowance">
            <div>
              <strong class="tabular big">{formatBytes(trafficBytes)}</strong>
              <span class="muted">used this billing cycle</span>
            </div>
            <span class="share tabular" class:warn={usedShare >= 75} class:crit={usedShare >= 90}>{usedShare}%</span>
          </div>
          <div class="progress" class:warn={usedShare >= 75} class:crit={usedShare >= 90}><span style:width={`${usedShare}%`}></span></div>
          <p class="hint tabular">{formatBytes(allowanceBytes)} fleet quota · {timezone}</p>
        {/if}
      </article>

      <article class="panel">
        <div class="panel-heading">
          <h2>Collection status</h2>
          <StatusPill state={attention ? 'stale' : 'healthy'} label={attention ? 'Review needed' : 'Receiving data'} />
        </div>
        <div class="feed">
          <div class="feed-item">
            <div>
              <strong>{connected} of {servers.length} servers connected</strong>
              <small>{healthy} healthy · {attention} need attention</small>
            </div>
          </div>
          <div class="feed-item">
            <div>
              <strong>{firingAlerts} active {firingAlerts === 1 ? 'alert' : 'alerts'}</strong>
              <a class="text-button" href="/alerts" onclick={(event) => followLink(event, () => onNavigate('alerts'))}>Review alert history <Icon name="chevron-right" size={14} /></a>
            </div>
          </div>
        </div>
      </article>
    </div>

    {#if latestJob}
      <article class="panel job-panel" aria-live="polite">
        <div class="panel-heading">
          <div><h2 class="capitalize">{latestJob.kind}</h2><span class="muted mono">{latestJob.id}</span></div>
          <StatusPill state={latestJob.state} />
        </div>
        <div class="job-progress" class:running={['queued', 'running'].includes(latestJob.state)}><span style:width={`${Math.max(0, Math.min(100, latestJob.progress))}%`}></span></div>
        <p class="hint tabular">{latestJob.progress}% · rev {latestJob.revision}</p>
        {#if latestJob.error}<p class="form-error">{latestJob.error.message || 'The job failed.'}</p>{/if}
        {#if jobError}<p class="form-error" role="alert">{jobError}</p>{/if}
        <div class="job-actions">
          {#if ['queued', 'running'].includes(latestJob.state)}<button class="button ghost small" type="button" onclick={onCancelJob}>Cancel job</button>{/if}
          <button class="button ghost small" type="button" onclick={onReloadJob} disabled={jobBusy}><Icon name="refresh" size={13} />Reload status</button>
        </div>
      </article>
    {/if}
  {/if}
</section>

<style>
  .live-badge {
    display: inline-flex;
    align-items: center;
    gap: 9px;
    padding: 6px 12px;
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
    background: var(--surface);
    color: var(--muted);
    font-size: 12px;
    white-space: nowrap;
  }
  .count { color: var(--muted); font-size: 13px; font-weight: 400; }
  .attention-list { display: grid; }
  .attention-row { display: flex; align-items: center; gap: 14px; padding: 14px 0; color: var(--ink); text-decoration: none; border-bottom: 1px solid var(--line-light); }
  .attention-row:hover .issue-identity strong { color: var(--accent); }
  .issue-identity { flex: 1; min-width: 0; display: grid; gap: 4px; font-size: 13px; }
  .issue-identity small { color: var(--muted); }
  .attention-footer { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding-top: 14px; font-size: 12px; }
  .all-healthy { display: flex; align-items: center; gap: 12px; padding: 20px 0; color: var(--success); }
  .all-healthy div { display: grid; gap: 5px; font-size: 13px; }
  .all-healthy span { color: var(--muted); }
  .lower-grid { display: grid; grid-template-columns: 1fr 1fr; gap: var(--gap); margin-top: 24px; }
  .lower-grid .panel + .panel { margin-top: 0; }

  .allowance { display: flex; align-items: flex-end; justify-content: space-between; gap: 12px; margin-bottom: 14px; }
  .allowance > div { display: grid; gap: 2px; }
  .big { font-size: 24px; font-weight: 600; letter-spacing: -0.02em; line-height: 1.1; }
  .share { color: var(--ink-secondary); font-size: 14px; font-weight: 600; }
  .share.warn { color: var(--warning); }
  .share.crit { color: var(--danger); }
  .progress.warn span { background: var(--warning); }
  .progress.crit span { background: var(--danger); }
  .hint { margin: 12px 0 0; color: var(--muted); font-size: 12px; }

  .feed { display: flex; flex-direction: column; gap: 12px; }
  .feed-item { display: flex; align-items: center; gap: 12px; padding-bottom: 12px; border-bottom: 1px solid var(--line-light); }
  .feed-item:last-child { padding-bottom: 0; border-bottom: 0; }
  .feed-item strong { display: block; font-size: 13px; }
  .feed-item small { color: var(--muted); font-size: 12px; }

  .job-panel { margin-top: var(--gap); }

  .empty-onboarding {
    display: grid;
    grid-template-columns: minmax(0, 1.15fr) minmax(260px, 0.85fr);
    overflow: hidden;
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
  }
  .empty-main { padding: 36px; }
  .empty-main h2 { margin: 0 0 10px; font-size: 20px; letter-spacing: -0.015em; line-height: 1.25; }
  .empty-main p { max-width: 52ch; margin: 0 0 22px; color: var(--ink-secondary); line-height: 1.6; }
  .empty-steps { display: grid; align-content: center; margin: 0; padding: 20px 30px; list-style: none; border-left: 1px solid var(--line-light); }
  .empty-steps li { display: flex; gap: 12px; padding: 16px 0; border-bottom: 1px solid var(--line-light); }
  .empty-steps li:last-child { border-bottom: 0; }
  .empty-steps li > span { flex: 0 0 16px; color: var(--muted); font-size: 13px; font-variant-numeric: tabular-nums; }
  .empty-steps li div { display: grid; gap: 5px; }
  .empty-steps strong { font-size: 14px; }
  .empty-steps small { color: var(--muted); line-height: 1.5; }

  @media (max-width: 800px) {
    .lower-grid, .empty-onboarding { grid-template-columns: 1fr; }
    .empty-main { padding: 28px; }
    .empty-steps { padding: 0 28px 8px; border-left: 0; }
  }
</style>
