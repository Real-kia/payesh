<script lang="ts">
  import { onMount, untrack } from 'svelte';
  import Icon from '../../Icon.svelte';
  import StatusPill from '../../components/StatusPill.svelte';
  import { ApiError, apiClient, type Module, type ModuleInstallation } from '../../api';
  import { formatDateTimeInTz } from '../../timezone';
  import { PREVIEW_MODE } from '../../lib/env';
  import { operationKey } from '../../lib/ops';
  import type { ConfirmOptions } from '../../lib/dialogs';
  import type { PreviewServer } from '../../preview/fixtures';

  let { server, catalog, timezone, readOnly, refreshKey = 0, onManage, onBrowse, onConfirm, onNotice, onAuthExpired }: {
    server: PreviewServer;
    catalog: Module[];
    timezone: string;
    readOnly: boolean;
    refreshKey?: number;
    onManage: (module: Module) => void;
    onBrowse: () => void;
    onConfirm: (options: ConfirmOptions) => void;
    onNotice: (message: string) => void;
    onAuthExpired: () => void;
  } = $props();

  let items = $state<ModuleInstallation[]>([]);
  let loading = $state(true);
  let error = $state('');
  let busy = $state('');
  const busyStates = ['downloading', 'verifying', 'installing', 'updating', 'removing'];

  async function load(): Promise<void> {
    loading = true;
    error = '';
    if (PREVIEW_MODE) {
      items = [
        { server_id: server.id, module_id: 'cpu-controls', version: '0.1.0', state: 'enabled', revision: '1', updated_at: new Date(Date.now() - 3600000).toISOString() },
        { server_id: server.id, module_id: 'port-traffic', version: '0.0.9', state: 'installed-disabled', revision: '2', updated_at: new Date(Date.now() - 86400000).toISOString() }
      ];
      loading = false;
      return;
    }
    try {
      items = (await apiClient.listServerModules(server.id)).items.filter((item) => item.state !== 'available' && item.state !== 'unavailable');
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Unable to load installed packages.';
      if (cause instanceof ApiError && cause.authExpired) onAuthExpired();
    } finally { loading = false; }
  }

  function moduleFor(item: ModuleInstallation): Module {
    return catalog.find((entry) => entry.id === item.module_id) ?? { id: item.module_id, name: item.module_id, latest_version: item.version || '', resource_estimate_source: 'unknown' };
  }

  async function act(item: ModuleInstallation, action: 'enable' | 'disable' | 'remove'): Promise<void> {
    if (busy) return;
    busy = `${item.module_id}:${action}`;
    try {
      if (PREVIEW_MODE) {
        items = action === 'remove' ? items.filter((entry) => entry.module_id !== item.module_id) : items.map((entry) => entry.module_id === item.module_id ? { ...entry, state: action === 'enable' ? 'enabled' : 'installed-disabled' } : entry);
        onNotice(`${moduleFor(item).name} is now ${action === 'remove' ? 'removed' : `${action}d`} (preview).`);
        return;
      }
      const result = await apiClient.moduleAction(server.id, item.module_id, action, item.revision, operationKey(`package-${action}`));
      items = action === 'remove' ? items.filter((entry) => entry.module_id !== item.module_id) : items.map((entry) => entry.module_id === item.module_id ? result : entry);
      onNotice(`${moduleFor(item).name} is now ${action === 'remove' ? 'removed' : result.state === 'installed-disabled' ? 'disabled' : result.state}.`);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Package operation failed.';
      if (cause instanceof ApiError && cause.authExpired) onAuthExpired();
    } finally { busy = ''; }
  }

  function confirmRemove(item: ModuleInstallation) {
    const name = moduleFor(item).name;
    onConfirm({ title: `Remove ${name}?`, description: `${name} is removed from ${server.name}. Its stored configuration is deleted.`, tone: 'danger', icon: 'trash', confirmText: 'Remove package', action: () => act(item, 'remove') });
  }

  // Reload after a dialog elsewhere installed or updated a package.
  let seenKey = untrack(() => refreshKey);
  $effect(() => { if (refreshKey !== seenKey) { seenKey = refreshKey; void load(); } });
  onMount(() => { void load(); });
</script>

<article class="panel">
  <div class="panel-heading">
    <div class="title"><span class="section-icon"><Icon name="packages" size={16} /></span><div><h2>Installed packages</h2><span class="muted">Optional signed extensions on {server.name}.</span></div></div>
    <div class="heading-actions">
      <button class="button ghost small" type="button" disabled={loading} onclick={() => void load()}><Icon name="refresh" size={13} />{loading ? 'Refreshing…' : 'Refresh'}</button>
      {#if !readOnly}<button class="button primary small" type="button" onclick={onBrowse}><Icon name="plus" size={13} />Add package</button>{/if}
    </div>
  </div>

  {#if loading && items.length === 0}
    <div class="skeleton-list" aria-label="Loading packages">{#each [0, 1] as index (index)}<span class="skeleton row"></span>{/each}</div>
  {:else if error && items.length === 0}
    <div class="unavailable-panel"><strong>Could not load packages</strong><span>{error}</span><button class="button ghost small" type="button" onclick={() => void load()}>Retry</button></div>
  {:else if items.length === 0}
    <div class="unavailable-panel"><strong>No packages installed</strong><span>This server has no optional packages yet.</span>{#if !readOnly}<button class="button primary small" type="button" onclick={onBrowse}>Browse packages</button>{/if}</div>
  {:else}
    <div class="list stagger">
      {#each items as item, index (item.module_id)}
        {@const entry = moduleFor(item)}
        {@const outdated = !!entry.latest_version && !!item.version && entry.latest_version !== item.version}
        <div class="package" style:--i={index}>
          <div class="head">
            <div class="identity">
              <div class="name"><h3>{entry.name}</h3><span class="mono id">{item.module_id}</span>{#if outdated}<span class="update">v{entry.latest_version} available</span>{/if}</div>
              {#if entry.description}<p>{entry.description}</p>{/if}
            </div>
            <StatusPill state={item.state === 'enabled' ? 'healthy' : item.state === 'failed' ? 'failed' : busyStates.includes(item.state) ? 'installing' : 'disabled'} label={item.state === 'installed-disabled' ? 'Disabled' : item.state === 'enabled' ? 'Enabled' : item.state} />
          </div>
          <div class="facts">
            <div><span>Installed</span><strong class="mono">{item.version ? `v${item.version}` : '—'}</strong></div>
            <div><span>Catalog</span><strong class="mono">{entry.latest_version ? `v${entry.latest_version}` : '—'}</strong></div>
            {#if item.updated_at}<div><span>Last change</span><strong>{formatDateTimeInTz(item.updated_at, timezone)}</strong></div>{/if}
          </div>
          {#if item.error?.message}<p class="form-error">{item.error.message}</p>{/if}
          {#if !readOnly}
            <div class="actions">
              <button class={`button small ${outdated ? 'primary' : 'ghost'}`} type="button" disabled={!!busy || busyStates.includes(item.state)} onclick={() => onManage(entry)}><Icon name="refresh" size={13} />{outdated ? 'Update' : 'Reinstall or update'}</button>
              {#if item.state === 'enabled' || item.state === 'installed-disabled'}
                <button class="button ghost small" type="button" disabled={!!busy} onclick={() => void act(item, item.state === 'enabled' ? 'disable' : 'enable')}>{busy === `${item.module_id}:enable` || busy === `${item.module_id}:disable` ? 'Working…' : item.state === 'enabled' ? 'Disable' : 'Enable'}</button>
              {/if}
              {#if item.state === 'installed-disabled'}
                <button class="button ghost small danger-text" type="button" disabled={!!busy} onclick={() => confirmRemove(item)}><Icon name="trash" size={13} />Remove</button>
              {/if}
            </div>
          {/if}
        </div>
      {/each}
    </div>
    {#if error}<p class="form-error" role="alert">{error}</p>{/if}
  {/if}
</article>

<style>
  .heading-actions { display: flex; flex-wrap: wrap; gap: 8px; }
  .skeleton-list { display: grid; gap: 10px; }
  .skeleton.row { height: 118px; border-radius: var(--radius-md); }
  .list { display: grid; gap: 12px; }
  .package { display: grid; gap: 12px; padding: 16px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface); transition: border-color var(--dur), box-shadow var(--dur); }
  .package:hover { border-color: var(--line-strong); box-shadow: var(--shadow-sm); }
  .head { display: flex; align-items: flex-start; justify-content: space-between; gap: 14px; }
  .identity { display: grid; gap: 4px; min-width: 0; }
  .name { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
  .name h3 { margin: 0; font-size: 14.5px; }
  .id { padding: 1px 6px; border: 1px solid var(--line); border-radius: var(--radius-xs); color: var(--muted); font-size: 11px; }
  .update { padding: 1px 8px; border-radius: var(--radius-pill); background: var(--accent-bg); color: var(--accent); font-size: 11px; font-weight: 650; }
  .identity p { margin: 0; color: var(--muted); font-size: 12.5px; line-height: 1.5; }
  .facts { display: flex; flex-wrap: wrap; gap: 8px 26px; padding: 10px 12px; border-radius: var(--radius-sm); background: var(--surface-muted); }
  .facts div { display: grid; gap: 2px; }
  .facts span { color: var(--muted); font-size: 10.5px; font-weight: 650; letter-spacing: 0.05em; text-transform: uppercase; }
  .facts strong { font-size: 12.5px; font-weight: 600; }
  .actions { display: flex; flex-wrap: wrap; gap: 8px; }
  .danger-text { color: var(--danger); }
</style>
