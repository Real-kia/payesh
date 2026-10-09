<script lang="ts">
  import Icon from '../Icon.svelte';
  import FleetSummary from '../components/FleetSummary.svelte';
  import ServerTable from '../components/ServerTable.svelte';
  import LoadingSkeleton from '../components/LoadingSkeleton.svelte';
  import StatusPill from '../components/StatusPill.svelte';
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
    onReloadJob
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
    onNavigate: (page: 'monitoring' | 'alerts') => void;
    onRetry: () => void;
    onCancelJob: () => void;
    onReloadJob: () => void;
  } = $props();

  const healthy = $derived(servers.filter((server) => server.displayState === 'healthy').length);
  const attention = $derived(servers.length - healthy);
  const connected = $derived(servers.filter((server) => server.connectionState === 'connected').length);
  const usedShare = $derived(allowanceBytes === '0' ? 0 : percentage(trafficBytes, allowanceBytes));
</script>

<section class="page overview-page" aria-labelledby="overview-title">
  <div class="page-heading">
    <div>
      <h1 id="overview-title" tabindex="-1">Fleet overview</h1>
      <p class="lede">Health, resources and bandwidth across every server.</p>
    </div>
    <!-- The dot pulses each time fresh fleet data arrives. -->
    <span class="live-badge">{#key servers}<span class="live-ping"></span>{/key}{PREVIEW_MODE ? 'Fixture preview' : 'Live · refreshes every 15s'}</span>
  </div>

  {#if loadState === 'loading'}
    <LoadingSkeleton label="Connecting to fleet" />
  {:else if loadState === 'empty'}
    <div class="empty-onboarding">
      <div class="empty-main">
        <span class="eyebrow">Get started · 1 of 3</span>
        <div class="state-icon"><Icon name="servers" size={26} /></div>
        <h2>Bring your first server online</h2>
        <p>Connect a Linux server over SSH. Payesh checks node transport reachability before installing the agent, then shows its live metrics here.</p>
        {#if readOnly}
          <p class="muted">Ask a user with edit access to add the first server.</p>
        {:else}
          <button class="button primary" type="button" onclick={onAddServer}><Icon name="plus" size={15} /><span>Add first server</span></button>
        {/if}
      </div>
      <ol class="empty-steps stagger" aria-label="Server setup steps">
        <li style:--i={1}><span>01</span><div><strong>Enter server details</strong><small>Hostname, SSH user, and one sign-in method.</small></div></li>
        <li style:--i={2}><span>02</span><div><strong>Verify the connection</strong><small>Payesh checks SSH and the TLS node endpoint.</small></div></li>
        <li style:--i={3}><span>03</span><div><strong>Monitor live metrics</strong><small>See health and resource data after enrollment.</small></div></li>
      </ol>
    </div>
  {:else if loadState === 'error'}
    <div class="state-panel error-state">
      <div class="state-icon"><Icon name="alert-triangle" size={26} /></div>
      <h2>Could not connect to fleet</h2>
      <p>{apiError || 'The API endpoint returned an error.'}</p>
      <button class="button primary" type="button" onclick={onRetry}><Icon name="refresh" size={14} />Retry connection</button>
    </div>
  {:else}
    <FleetSummary {servers} {download} {upload} />

    <div class="section-heading">
      <h2>Servers <span class="count">{servers.length}</span></h2>
      <button class="button ghost small" type="button" onclick={() => onNavigate('monitoring')}>Server monitoring <Icon name="chevron-right" size={14} /></button>
    </div>
    <ServerTable {servers} {timezone} {onSelect} nameHeading="Hostname / address" />

    <div class="lower-grid stagger">
      <article class="panel" style:--i={1}>
        <div class="panel-heading">
          <h2>Allowance overview</h2>
          {#if servers[0]}<button class="text-button" type="button" onclick={() => onSelect(servers[0])}>View metrics <Icon name="chevron-right" size={14} /></button>{/if}
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

      <article class="panel" style:--i={2}>
        <div class="panel-heading">
          <h2>Collection status</h2>
          <StatusPill state={attention ? 'stale' : 'healthy'} label={attention ? 'Review needed' : 'Receiving data'} />
        </div>
        <div class="feed">
          <div class="feed-item">
            <span class="feed-icon success"><Icon name="radio" size={14} /></span>
            <div>
              <strong>{connected} of {servers.length} servers connected</strong>
              <small>{healthy} healthy · {attention} need attention</small>
            </div>
          </div>
          <div class="feed-item">
            <span class="feed-icon warning"><Icon name="alerts" size={14} /></span>
            <div>
              <strong>Alerts and incidents</strong>
              <button class="text-button" type="button" onclick={() => onNavigate('alerts')}>Review alert history <Icon name="chevron-right" size={14} /></button>
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
    border-radius: var(--radius-pill);
    background: var(--surface);
    color: var(--muted);
    font-size: 12px;
    white-space: nowrap;
  }
  .section-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin: 4px 0 12px; }
  .section-heading h2 { display: flex; align-items: center; gap: 8px; margin: 0; }
  .count { padding: 1px 8px; border-radius: var(--radius-pill); background: var(--surface-muted); color: var(--muted); font-size: 12px; font-weight: 600; }
  .lower-grid { display: grid; grid-template-columns: 1fr 1fr; gap: var(--gap); margin-top: 24px; }
  .lower-grid .panel + .panel { margin-top: 0; }

  .allowance { display: flex; align-items: flex-end; justify-content: space-between; gap: 12px; margin-bottom: 14px; }
  .allowance > div { display: grid; gap: 2px; }
  .big { font-size: 30px; font-weight: 700; letter-spacing: -0.035em; line-height: 1.1; }
  .share { color: var(--accent); font-size: 15px; font-weight: 700; }
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
  .feed-icon { display: grid; flex-shrink: 0; place-items: center; width: 30px; height: 30px; border-radius: var(--radius-md); }
  .feed-icon.success { background: var(--success-bg); color: var(--success); }
  .feed-icon.warning { background: var(--warning-bg); color: var(--warning); }

  .job-panel { margin-top: var(--gap); }
  .job-progress.running span { background-image: linear-gradient(90deg, var(--accent), var(--blue), var(--accent)); background-size: 200% 100%; animation: shimmer 1.6s linear infinite; }

  .empty-onboarding {
    display: grid;
    grid-template-columns: minmax(0, 1.15fr) minmax(260px, 0.85fr);
    overflow: hidden;
    border: 1px solid var(--line);
    border-radius: var(--radius-xl);
    background: var(--surface);
    box-shadow: var(--shadow-md);
    animation: pop-in var(--dur-slow) var(--ease-out);
  }
  .empty-main { padding: 40px; background: radial-gradient(120% 120% at 0% 0%, var(--accent-bg), transparent 60%), var(--surface); }
  .empty-main .state-icon { margin: 26px 0 18px; background: var(--accent-bg); color: var(--accent); }
  .empty-main h2 { margin: 0 0 12px; font-size: clamp(22px, 3vw, 30px); letter-spacing: -0.03em; line-height: 1.15; }
  .empty-main p { max-width: 52ch; margin: 0 0 22px; color: var(--ink-secondary); line-height: 1.6; }
  .empty-steps { display: grid; align-content: center; margin: 0; padding: 20px 30px; list-style: none; border-left: 1px solid var(--line-light); }
  .empty-steps li { display: flex; gap: 16px; padding: 20px 0; border-bottom: 1px solid var(--line-light); }
  .empty-steps li:last-child { border-bottom: 0; }
  .empty-steps li > span { flex: 0 0 30px; color: var(--accent); font-size: 12px; font-weight: 700; font-variant-numeric: tabular-nums; }
  .empty-steps li div { display: grid; gap: 5px; }
  .empty-steps strong { font-size: 14px; }
  .empty-steps small { color: var(--muted); line-height: 1.5; }

  @media (max-width: 800px) {
    .lower-grid, .empty-onboarding { grid-template-columns: 1fr; }
    .empty-main { padding: 28px; }
    .empty-steps { padding: 0 28px 8px; border-left: 0; }
  }
</style>
