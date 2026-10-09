<script lang="ts">
  import Icon from '../../Icon.svelte';
  import StatusPill from '../../components/StatusPill.svelte';
  import { PREVIEW_MODE } from '../../lib/env';
  import type { UpdateStatus, WebUpdateState } from '../../api';

  let { status, installedVersion, checkBusy, checkError, applyBusy, applyError, webUpdate, isOwner, isNewerVersion, onCheck, onUpdate }: {
    status: UpdateStatus | null;
    installedVersion: string;
    checkBusy: boolean;
    checkError: string;
    applyBusy: boolean;
    applyError: string;
    webUpdate: WebUpdateState | null;
    isOwner: boolean;
    isNewerVersion: (candidate: string, installed: string) => boolean;
    onCheck: () => void;
    onUpdate: (target: string) => void;
  } = $props();

  const PER_PAGE = 5;
  let page = $state(0);
  const releases = $derived(status?.releases ?? []);
  const pageCount = $derived(Math.max(1, Math.ceil(releases.length / PER_PAGE)));
  const visible = $derived(releases.slice(page * PER_PAGE, (page + 1) * PER_PAGE));
  const webUpdateActive = $derived(webUpdate?.state === 'queued' || webUpdate?.state === 'running');
  const installedAhead = $derived(!!status && (status.installed_ahead ?? isNewerVersion(status.current, status.latest)));
  $effect(() => { if (page > pageCount - 1) page = pageCount - 1; });

  let copied = $state(false);
  async function copyCommand() {
    try { await navigator.clipboard.writeText('sudo payesh update'); copied = true; setTimeout(() => (copied = false), 1600); } catch { copied = false; }
  }
</script>

<article class="panel">
  <div class="panel-heading">
    <div class="title"><span class="section-icon"><Icon name="download" size={16} /></span><div><h2>Versions & updates</h2><span class="muted">Payesh checks GitHub releases for new versions.</span></div></div>
    <button class="button ghost small" type="button" disabled={checkBusy || PREVIEW_MODE} onclick={onCheck}>{#if checkBusy}<span class="version-spinner" aria-hidden="true"></span>Checking…{:else}<Icon name="refresh" size={13} />Check now{/if}</button>
  </div>

  <div class="versions">
    <div class="version-card">
      <span class="muted">Installed</span>
      <strong class="mono">{installedVersion}</strong>
    </div>
    <div class="version-card" class:highlight={!!status?.update_available && !installedAhead}>
      <span class="muted">Latest release</span>
      {#if checkBusy}<span class="skeleton bar"></span>
      {:else if status}<a class="mono" href={status.url} target="_blank" rel="noopener noreferrer">v{status.latest}</a>
      {:else}<strong class="muted">—</strong>{/if}
    </div>
    <div class="version-card">
      <span class="muted">Status</span>
      {#if checkBusy}<span class="skeleton bar"></span>
      {:else if status && !checkError}
        <StatusPill state={installedAhead || status.update_available ? 'pending' : 'healthy'} label={installedAhead ? 'Ahead of latest release' : status.update_available ? 'Update available' : 'Up to date'} />
      {:else}<StatusPill state="disabled" label="Unknown" />{/if}
    </div>
  </div>

  {#if status && !checkBusy && !checkError}
    {#if webUpdateActive}
      <div class="update-progress" role="status">
        <span class="version-spinner" aria-hidden="true"></span>
        <div><strong>Updating to v{webUpdate?.target}…</strong><span class="muted">{webUpdate?.state === 'queued' ? 'Waiting for the update service.' : 'Installing. The dashboard will restart and reload automatically.'}</span></div>
        <div class="indeterminate" aria-hidden="true"><span></span></div>
      </div>
    {:else if status.update_available && isOwner}
      {#if status.web_update_supported}
        <button class="button primary" type="button" disabled={applyBusy} onclick={() => onUpdate(status?.latest ?? '')}><Icon name="download" size={14} />{applyBusy ? 'Starting…' : `Update to v${status.latest}`}</button>
      {:else}
        <p class="muted">Updating from the browser is not set up on this server yet. Run the command below once; later updates can be done here.</p>
      {/if}
    {/if}
  {/if}
  {#if checkError}<p class="form-error" role="alert">{checkError}</p>{/if}
  {#if applyError}<p class="form-error" role="alert">{applyError}</p>{/if}

  <h3>Release history</h3>
  {#if releases.length}
    <ol class="timeline">
      {#each visible as release (release.version)}
        {@const installed = release.version === status?.current}
        <li class:installed>
          <span class="node" aria-hidden="true"></span>
          <div class="release">
            <strong class="mono">v{release.version}</strong>
            {#if installed}<span class="tag">Installed</span>{/if}
            <span class="muted date">{new Date(release.published_at).toLocaleDateString()}</span>
          </div>
          <span class="release-actions">
            <a href={release.url} target="_blank" rel="noopener noreferrer">Release notes ↗</a>
            {#if isOwner && status?.web_update_supported && !webUpdateActive && isNewerVersion(release.version, status.current)}<button class="button ghost small" type="button" disabled={applyBusy} onclick={() => onUpdate(release.version)}>Install</button>{/if}
          </span>
        </li>
      {/each}
    </ol>
    {#if pageCount > 1}
      <div class="pager">
        <button class="button ghost small" type="button" disabled={page === 0} onclick={() => (page -= 1)}>Newer</button>
        <span class="muted">Page {page + 1} of {pageCount} · {releases.length} releases</span>
        <button class="button ghost small" type="button" disabled={page >= pageCount - 1} onclick={() => (page += 1)}>Older</button>
      </div>
    {/if}
  {:else}<p class="muted">{checkBusy ? 'Loading releases…' : 'Release history unavailable.'}</p>{/if}

  <h3>Update from the command line</h3>
  <div class="command">
    <code>sudo payesh update</code>
    <button class="icon-button" type="button" aria-label="Copy update command" title={copied ? 'Copied' : 'Copy'} onclick={() => void copyCommand()}><Icon name={copied ? 'check' : 'copy'} size={14} /></button>
  </div>
</article>

<style>
  .versions { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 10px; margin-bottom: 16px; }
  .version-card { display: grid; align-content: start; gap: 8px; min-width: 0; padding: 14px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface-muted); font-size: 12px; }
  .version-card strong, .version-card a { font-size: 15px; font-weight: 650; }
  .version-card :global(.status-pill) { justify-self: start; }
  .version-card.highlight { border-color: color-mix(in srgb, var(--accent) 45%, var(--line)); background: var(--accent-bg); }
  .bar { width: 70%; height: 18px; }
  .update-progress { position: relative; display: flex; align-items: center; gap: 12px; overflow: hidden; margin: 6px 0 12px; padding: 14px 16px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface-muted); }
  .update-progress > div:not(.indeterminate) { display: grid; gap: 2px; font-size: 13px; }
  .indeterminate { position: absolute; left: 0; right: 0; bottom: 0; height: 2px; overflow: hidden; }
  .indeterminate span { display: block; width: 40%; height: 100%; background: var(--accent); animation: bar-indeterminate 1.4s var(--ease-in-out) infinite; }

  .timeline { position: relative; margin: 0 0 12px; padding: 0; list-style: none; }
  .timeline::before { content: ''; position: absolute; left: 5px; top: 14px; bottom: 14px; width: 2px; background: var(--line); }
  .timeline li { position: relative; display: grid; grid-template-columns: 12px minmax(0, 1fr) auto; gap: 14px; align-items: center; padding: 10px 0; font-size: 13px; }
  .node { width: 12px; height: 12px; border: 2px solid var(--line-strong); border-radius: 50%; background: var(--surface); z-index: 1; }
  li.installed .node { border-color: var(--accent); background: var(--accent); box-shadow: 0 0 0 4px var(--accent-glow); }
  .release { display: flex; flex-wrap: wrap; align-items: center; gap: 4px 10px; }
  .tag { padding: 1px 7px; border-radius: var(--radius-pill); background: var(--accent-bg); color: var(--accent); font-size: 11px; font-weight: 650; }
  .date { font-size: 12px; }
  .release-actions { display: flex; align-items: center; gap: 12px; font-size: 12.5px; }
  .pager { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 8px; font-size: 12.5px; }
  .command { display: flex; align-items: center; justify-content: space-between; gap: 10px; max-width: 360px; padding: 6px 6px 6px 14px; border-radius: var(--radius-md); background: var(--term-bg); color: var(--term-ink); }
  .command code { font-family: var(--font-mono); font-size: 13px; }
  .command .icon-button { width: 30px; height: 30px; border-color: transparent; background: rgba(255, 255, 255, 0.06); color: var(--term-ink); }
  @media (max-width: 640px) {
    .versions { grid-template-columns: 1fr; }
    .timeline li { grid-template-columns: 12px minmax(0, 1fr); }
    .release-actions { grid-column: 2; }
  }
</style>
