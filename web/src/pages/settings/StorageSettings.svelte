<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import Icon from '../../Icon.svelte';
  import { ApiError, apiClient, type StorageStatus } from '../../api';
  import { PREVIEW_MODE } from '../../lib/env';
  import { formatBytes } from '../../lib/format';
  import { previewStorageStatus } from '../../preview/fixtures';

  let { isOwner, onSaved, onAuthExpired, onNotificationsChanged }: {
    isOwner: boolean;
    onSaved: (title: string, description: string) => void;
    onAuthExpired: () => void;
    onNotificationsChanged: () => void;
  } = $props();

  let storageStatus = $state<StorageStatus | null>(null);
  let databaseLimitGB = $state(1);
  let samplingSeconds = $state(15);
  let pressureSamplingSeconds = $state(60);
  let adaptiveSampling = $state(true);
  let notificationsEnabled = $state(true);
  let busy = $state(false);
  let refreshBusy = false;
  let error = $state('');
  let saved = $state('');
  let refreshTimer: number | undefined;

  const snapshotBytes = $derived(storageStatus?.recovery_snapshot_bytes ?? 0);
  const usageBytes = $derived((storageStatus?.database_bytes ?? 0) + snapshotBytes);
  const usageShare = $derived(storageStatus ? Math.min(100, (usageBytes / storageStatus.settings.max_database_bytes) * 100) : 0);

  function applyStatus(status: StorageStatus) {
    storageStatus = status;
    databaseLimitGB = status.settings.max_database_bytes / 1000000000;
    samplingSeconds = status.settings.sample_seconds;
    pressureSamplingSeconds = status.settings.pressure_sample_seconds;
    adaptiveSampling = status.settings.adaptive_sampling;
    notificationsEnabled = status.settings.notifications_enabled;
  }

  async function load(): Promise<void> {
    if (PREVIEW_MODE) { applyStatus(structuredClone(previewStorageStatus)); return; }
    busy = true; error = ''; saved = '';
    try {
      applyStatus(await apiClient.getStorageSettings());
    } catch (err) {
      if (err instanceof ApiError && err.authExpired) onAuthExpired();
      error = err instanceof ApiError ? err.message : 'Storage settings could not be loaded.';
    } finally { busy = false; }
  }

  async function save(): Promise<void> {
    if (!storageStatus) return;
    busy = true; error = ''; saved = '';
    try {
      storageStatus = await apiClient.saveStorageSettings({ ...storageStatus.settings, max_database_bytes: Math.round(databaseLimitGB * 1000000000), sample_seconds: samplingSeconds, pressure_sample_seconds: pressureSamplingSeconds, adaptive_sampling: adaptiveSampling, notifications_enabled: notificationsEnabled });
      saved = 'Settings saved. Collection changes take effect on the next sampling cycle; cleanup runs automatically.';
      onSaved('Storage & sampling updated', `Storage settings applied successfully:\n\n• Maximum database size: ${databaseLimitGB} GB\n• Normal interval: ${samplingSeconds} seconds\n• Storage-saving interval: ${pressureSamplingSeconds} seconds\n• Adaptive sampling: ${adaptiveSampling ? 'Enabled' : 'Disabled'}\n• Storage notifications: ${notificationsEnabled ? 'Enabled' : 'Disabled'}\n\nCollection settings take effect on the next sampling cycle.`);
      onNotificationsChanged();
    } catch (err) {
      if (err instanceof ApiError && err.authExpired) onAuthExpired();
      error = err instanceof ApiError ? err.message : 'Storage settings could not be saved.';
    } finally { busy = false; }
  }

  // Keeps the usage figures current without touching the form fields.
  async function refreshUsage(): Promise<void> {
    if (busy || refreshBusy || !storageStatus || document.hidden) return;
    refreshBusy = true;
    try {
      const result = await apiClient.getStorageSettings();
      if (storageStatus && result.settings.revision === storageStatus.settings.revision) storageStatus = { ...storageStatus, database_bytes: result.database_bytes, recovery_snapshot_bytes: result.recovery_snapshot_bytes ?? 0, effective_sample_seconds: result.effective_sample_seconds, settings: { ...storageStatus.settings, pressure_state: result.settings.pressure_state } };
    } catch (err) {
      if (err instanceof ApiError && err.authExpired) onAuthExpired();
      // Keep the last measurement for other failures; Refresh reports errors.
    } finally { refreshBusy = false; }
  }

  onMount(() => {
    void load();
    if (!PREVIEW_MODE) refreshTimer = window.setInterval(() => void refreshUsage(), 15000);
  });
  onDestroy(() => window.clearInterval(refreshTimer));
</script>

<article class="panel">
  <div class="panel-heading">
    <div class="title"><span class="section-icon"><Icon name="database" size={16} /></span><div><h2>Storage & sampling</h2><span class="muted">How much history is kept and how often servers report.</span></div></div>
    <button class="button ghost small" type="button" disabled={busy || PREVIEW_MODE} onclick={() => void load()}><Icon name="refresh" size={13} />Refresh</button>
  </div>
  {#if storageStatus}
    <div class="settings-meta-box">
      <div class="usage">
        <div><span class="muted">Total storage usage</span><strong class="tabular">{formatBytes(String(usageBytes))} <small>of {formatBytes(String(storageStatus.settings.max_database_bytes))}</small></strong></div>
        <span class="share tabular" class:warn={usageShare >= 75} class:crit={usageShare >= 90}>{usageShare.toFixed(0)}%</span>
      </div>
      <div class="progress" class:warn={usageShare >= 75} class:crit={usageShare >= 90}><span style:width={`${usageShare}%`}></span></div>
      {#if snapshotBytes > 0}
        <div class="meta-row"><span>Database and journals</span><strong>{formatBytes(String(storageStatus.database_bytes))}</strong></div>
        <div class="meta-row"><span>Recovery snapshots</span><strong>{formatBytes(String(snapshotBytes))}</strong></div>
      {/if}
      <div class="meta-row"><span>Current sampling interval</span><strong>{storageStatus.effective_sample_seconds} seconds{storageStatus.settings.pressure_state === 'saving' && storageStatus.settings.adaptive_sampling ? ' · storage-saving mode' : ''}</strong></div>
    </div>
    <form class="form-grid" onsubmit={(event) => { event.preventDefault(); void save(); }}>
      <label>Maximum storage size (GB)<input type="number" min="0.128" max="64" step="0.001" required bind:value={databaseLimitGB} disabled={busy || !isOwner} /></label>
      <label>Normal sample interval (seconds)<input type="number" min="5" max="3600" step="1" required bind:value={samplingSeconds} disabled={busy || !isOwner} /></label>
      <label>Storage-saving interval (seconds)<input type="number" min={samplingSeconds} max="3600" step="1" required bind:value={pressureSamplingSeconds} disabled={busy || !isOwner} /></label>
      <label>Automatic sampling reduction<select bind:value={adaptiveSampling} disabled={busy || !isOwner}><option value={true}>Enabled</option><option value={false}>Disabled</option></select></label>
      <label>Storage notifications<select bind:value={notificationsEnabled} disabled={busy || !isOwner}><option value={true}>Enabled</option><option value={false}>Disabled</option></select></label>
      {#if isOwner}<div class="setup-actions"><button class="button primary" type="submit" disabled={busy || PREVIEW_MODE}>{busy ? 'Saving…' : 'Save settings'}</button></div>{/if}
    </form>
  {:else if busy}
    <p class="muted" role="status"><span class="version-spinner" aria-hidden="true"></span> Loading storage settings…</p>
  {/if}
  {#if error}<p class="form-error" role="alert">{error}</p>{/if}
  {#if saved}<p class="success-text" role="status">{saved}</p>{/if}
</article>

<style>
  .usage { display: flex; align-items: flex-end; justify-content: space-between; gap: 12px; }
  .usage > div { display: grid; gap: 2px; font-size: 13px; }
  .usage strong { font-size: 20px; letter-spacing: -0.02em; }
  .usage small { color: var(--muted); font-size: 13px; font-weight: 500; }
  .share { color: var(--accent); font-weight: 700; }
  .share.warn { color: var(--warning); }
  .share.crit { color: var(--danger); }
  .progress { background: var(--surface); }
  .progress.warn span { background: var(--warning); }
  .progress.crit span { background: var(--danger); }
  p.muted { display: flex; align-items: center; gap: 8px; }
</style>
