<script lang="ts">
  import { untrack } from 'svelte';
  import Icon from '../Icon.svelte';
  import Modal from '../Modal.svelte';
  import StatusPill from './StatusPill.svelte';
  import { ApiError, apiClient, type Module, type ModuleInstallation, type PackageSource } from '../api';
  import { PREVIEW_MODE } from '../lib/env';
  import { operationKey } from '../lib/ops';
  import { roleLabel } from '../lib/format';
  import type { PreviewServer } from '../preview/fixtures';

  let { module, servers, initialServerId = '', onClose, onChanged, onNotice, onAuthExpired }: {
    module: Module;
    servers: PreviewServer[];
    initialServerId?: string;
    onClose: () => void;
    onChanged: () => void;
    onNotice: (message: string) => void;
    onAuthExpired: () => void;
  } = $props();

  const ALL = 'all';
  const busyStates = ['downloading', 'verifying', 'installing', 'updating', 'removing'];
  type TargetResult = { server: PreviewServer; status: 'pending' | 'running' | 'done' | 'failed'; message: string };

  let step = $state<'target' | 'source' | 'progress'>('target');
  // The dialog opens on the given server, or on the master by default.
  let serverId = $state(untrack(() => initialServerId || (servers.find((s) => s.role === 'standalone' || s.role === 'hub') ?? servers[0])?.id || ''));
  let installations = $state<ModuleInstallation[]>([]);
  let statusBusy = $state(false);
  let statusFailed = $state(false);
  let busy = $state('');
  let error = $state('');

  let sourceKind = $state<PackageSource['kind']>('github');
  let location = $state('');
  let version = $state('');
  let manifest = $state('');
  let signature = $state('');

  let results = $state<TargetResult[]>([]);
  let running = $state(false);

  const connected = $derived(servers.filter((server) => server.connectionState === 'connected'));
  const target = $derived(servers.find((server) => server.id === serverId));
  const current = $derived(installations.find((item) => item.module_id === module.id));
  const installed = $derived(serverId !== ALL && (current?.state === 'enabled' || current?.state === 'installed-disabled'));
  const finished = $derived(results.filter((item) => item.status === 'done' || item.status === 'failed').length);
  const failed = $derived(results.filter((item) => item.status === 'failed').length);
  const releaseLabel = $derived(version.trim() || module.release || module.latest_version || 'latest');

  async function loadStatus(): Promise<void> {
    installations = [];
    error = '';
    statusFailed = false;
    if (!serverId || serverId === ALL) return;
    if (PREVIEW_MODE) {
      installations = [
        { server_id: serverId, module_id: 'cpu-controls', version: '0.1.0', state: 'enabled', revision: '1', updated_at: '2026-09-09T14:40:00Z' },
        { server_id: serverId, module_id: 'port-traffic', version: '0.0.9', state: 'installed-disabled', revision: '2', updated_at: '2026-09-09T14:40:00Z' }
      ];
      return;
    }
    statusBusy = true;
    const requested = serverId;
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 10000);
    try {
      const page = await apiClient.listServerModules(requested, { signal: controller.signal });
      if (requested === serverId) installations = page.items;
    } catch (cause) {
      if (requested !== serverId) return;
      statusFailed = true;
      error = cause instanceof DOMException && cause.name === 'AbortError' ? 'Package status took too long to load.' : cause instanceof Error ? cause.message : 'Unable to load package state.';
      if (cause instanceof ApiError && cause.authExpired) onAuthExpired();
    } finally {
      window.clearTimeout(timeout);
      if (requested === serverId) statusBusy = false;
    }
  }

  function packageSource(): PackageSource {
    const source: PackageSource = { kind: sourceKind, location: location.trim() || (sourceKind === 'github' ? module.repository || 'Real-kia/payesh' : '') };
    if (sourceKind === 'github') source.version = version.trim() || module.release || 'latest';
    if (sourceKind !== 'github' && manifest.trim()) source.manifest_location = manifest.trim();
    if (sourceKind !== 'github' && signature.trim()) source.signature_location = signature.trim();
    return source;
  }

  function githubURL(): string {
    const arch = target?.architecture;
    const repo = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(location.trim()) ? location.trim() : module.repository || 'Real-kia/payesh';
    const release = version.trim() || module.release || 'latest';
    const base = release === 'latest' ? `https://github.com/${repo}/releases/latest/download` : `https://github.com/${repo}/releases/download/v${release.replace(/^v/, '')}`;
    return arch === 'amd64' || arch === 'arm64' ? `${base}/${module.id}-linux-${arch}.tar.gz` : `https://github.com/${repo}/releases`;
  }

  // Installs (or updates, when already installed) on each target in turn.
  // The hub downloads, verifies the signed manifest and activates the package
  // inside one request, so every row reflects that server's real outcome.
  async function install(): Promise<void> {
    if (running) return;
    const targets = serverId === ALL ? connected : servers.filter((server) => server.id === serverId);
    if (targets.length === 0) { error = 'No connected server to install on.'; return; }
    const source = packageSource();
    step = 'progress';
    running = true;
    error = '';
    results = targets.map((server) => ({ server, status: 'pending', message: 'Waiting' }));
    for (let index = 0; index < results.length; index++) {
      const row = results[index];
      row.status = 'running';
      row.message = 'Checking current state…';
      try {
        if (PREVIEW_MODE) {
          await new Promise((resolve) => setTimeout(resolve, 500));
          row.status = 'done';
          row.message = `Preview only · would install v${releaseLabel}`;
          continue;
        }
        const existing = (await apiClient.listServerModules(row.server.id)).items.find((item) => item.module_id === module.id);
        if (existing && busyStates.includes(existing.state)) throw new Error(`Another package operation is in progress (${existing.state}).`);
        const updating = existing?.state === 'enabled' || existing?.state === 'installed-disabled';
        row.message = updating ? 'Downloading, verifying and updating…' : 'Downloading, verifying and installing…';
        const result = await apiClient.installModuleSource(row.server.id, module.id, source, existing?.revision ?? '0', operationKey(updating ? 'package-update' : 'package-install'));
        row.status = result.state === 'failed' ? 'failed' : 'done';
        row.message = result.state === 'failed' ? (result.error?.message || 'The hub reported a failure.') : `${updating ? 'Updated' : 'Installed'}${result.version ? ` v${result.version}` : ''} · ${result.state === 'enabled' ? 'enabled' : 'installed, disabled'}`;
      } catch (cause) {
        row.status = 'failed';
        row.message = cause instanceof Error ? cause.message : 'Installation failed.';
        if (cause instanceof ApiError && cause.authExpired) { onAuthExpired(); break; }
      }
    }
    running = false;
    if (failed === 0) onNotice(`${module.name} ${results.length > 1 ? `installed on ${results.length} servers` : 'installed'}.`);
    else error = failed === results.length ? 'Installation failed. See the details above.' : `${failed} of ${results.length} servers failed. See the details above.`;
    onChanged();
    if (serverId !== ALL) void loadStatus();
  }

  async function act(action: 'enable' | 'disable' | 'remove'): Promise<void> {
    if (!serverId || serverId === ALL || busy || !current) return;
    busy = action;
    error = '';
    try {
      const result = await apiClient.moduleAction(serverId, module.id, action, current.revision, operationKey(`package-${action}`));
      installations = [...installations.filter((item) => item.module_id !== module.id), result];
      onNotice(`${module.name} is now ${result.state === 'installed-disabled' ? 'disabled' : result.state}.`);
      onChanged();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : 'Package operation failed.';
      if (cause instanceof ApiError && cause.authExpired) onAuthExpired();
    } finally { busy = ''; }
  }

  function confirm() {
    if (step === 'target') step = 'source';
    else if (step === 'source') void install();
    else onClose();
  }

  $effect(() => { void serverId; void loadStatus(); });
</script>

<Modal
  open={true}
  title={step === 'progress' ? `${installed ? 'Updating' : 'Installing'} ${module.name}` : installed ? `Update ${module.name}` : `Install ${module.name}`}
  description={step === 'target' ? 'Choose where to install or manage this package.' : step === 'source' ? (serverId === ALL ? `Install or update on all ${connected.length} connected servers.` : `Deploy to ${target?.name ?? 'the selected server'}.`) : running ? 'Each server downloads, verifies and activates the signed package.' : failed ? 'Finished with errors.' : 'Finished.'}
  icon="packages"
  confirmText={step === 'target' ? 'Continue' : step === 'source' ? (serverId === ALL ? `Install on ${connected.length} servers` : installed ? 'Update package' : 'Install package') : running ? 'Working…' : 'Done'}
  busy={running}
  hideCancel={step === 'progress'}
  confirmDisabled={step === 'progress' ? running : !serverId || statusBusy || (serverId !== ALL && statusFailed) || !!busy || (step === 'source' && ((serverId !== ALL && !!current && busyStates.includes(current.state)) || (sourceKind !== 'github' && !location.trim())))}
  onCancel={() => { if (!running) onClose(); }}
  onConfirm={confirm}
>
  {#if step === 'target'}
    <label class="field">Install on
      <select bind:value={serverId}>
        <option value="" disabled>Choose a server</option>
        {#if connected.length > 1}<option value={ALL}>All connected servers ({connected.length})</option>{/if}
        {#each servers as server (server.id)}
          <option value={server.id} disabled={server.connectionState !== 'connected'}>{server.name} · {roleLabel(server.role)} ({server.architecture}){server.connectionState !== 'connected' ? ' · offline' : ''}</option>
        {/each}
      </select>
    </label>
    {#if statusBusy}<p class="muted status"><span class="version-spinner" aria-hidden="true"></span>Loading package status…</p>{/if}
    {#if current && !statusBusy}
      <div class="current">
        <div><span class="muted">On {target?.name}</span><strong>{current.version ? `v${current.version}` : 'Installed'}</strong></div>
        <StatusPill state={current.state === 'enabled' ? 'healthy' : current.state === 'failed' ? 'failed' : busyStates.includes(current.state) ? 'installing' : 'disabled'} label={current.state === 'installed-disabled' ? 'Disabled' : current.state} />
      </div>
      {#if current.state === 'enabled' || current.state === 'installed-disabled'}
        <div class="actions">
          <button class="button ghost small" type="button" disabled={!!busy} onclick={() => void act(current?.state === 'enabled' ? 'disable' : 'enable')}>{busy === 'enable' || busy === 'disable' ? 'Working…' : current.state === 'enabled' ? 'Disable' : 'Enable'}</button>
          {#if current.state === 'installed-disabled'}<button class="button ghost small" type="button" disabled={!!busy} onclick={() => void act('remove')}><Icon name="trash" size={13} />{busy === 'remove' ? 'Removing…' : 'Remove'}</button>{/if}
        </div>
      {/if}
    {/if}
  {:else if step === 'source'}
    <button class="button ghost small back" type="button" onclick={() => (step = 'target')}><Icon name="arrow-left" size={13} />{serverId === ALL ? 'All connected servers' : target?.name ?? 'Change server'}</button>
    {#if installed}
      <p class="note"><Icon name="refresh" size={14} /><span>Installed on this server{current?.version ? ` (v${current.version})` : ''}. Continuing updates it; an enabled package stays enabled.</span></p>
    {:else if serverId === ALL}
      <p class="note"><Icon name="servers" size={14} /><span>Installs on each connected server in turn. Servers that already have it are updated.</span></p>
    {/if}
    <div class="fields">
      <label class="field">Source
        <select bind:value={sourceKind} onchange={() => { location = ''; manifest = ''; signature = ''; error = ''; }}>
          <option value="github">GitHub release</option>
          <option value="local">Local path on the server</option>
          <option value="url">Archive URL</option>
        </select>
      </label>
      <label class="field">{sourceKind === 'github' ? 'Repository' : sourceKind === 'local' ? 'Archive path on the server' : 'Archive URL'}
        <input bind:value={location} placeholder={sourceKind === 'github' ? module.repository || 'Real-kia/payesh' : sourceKind === 'local' ? '/opt/packages/cpu-controls-linux-amd64.tar.gz' : 'https://packages.example.com/cpu-controls-linux-amd64.tar.gz'} />
      </label>
      {#if sourceKind === 'github'}
        <label class="field">Release<input bind:value={version} placeholder={`Catalog default (v${module.latest_version})`} /></label>
        {#if serverId !== ALL}<a class="link" href={githubURL()} target="_blank" rel="noopener noreferrer">View package archive ↗</a>{/if}
      {:else}
        <details>
          <summary>Package metadata</summary>
          <label class="field">Manifest {sourceKind === 'local' ? 'path' : 'URL'}<input bind:value={manifest} placeholder="Automatic (.manifest.json)" /></label>
          <label class="field">Signature {sourceKind === 'local' ? 'path' : 'URL'}<input bind:value={signature} placeholder="Automatic (.manifest.sig)" /></label>
        </details>
      {/if}
    </div>
  {:else}
    <div class="progress-head">
      <span class="muted">{finished} of {results.length} {results.length === 1 ? 'server' : 'servers'} · v{releaseLabel}</span>
      {#if !running}<StatusPill state={failed ? 'failed' : 'healthy'} label={failed ? `${failed} failed` : 'Complete'} />{/if}
    </div>
    <div class="job-progress" class:running><span style:width={`${results.length ? Math.round((finished / results.length) * 100) : 0}%`}></span></div>
    <ol class="targets">
      {#each results as row (row.server.id)}
        <li class={row.status}>
          <span class="mark" aria-hidden="true">
            {#if row.status === 'done'}<Icon name="check" size={13} />{:else if row.status === 'failed'}<Icon name="x" size={13} />{:else if row.status === 'running'}<span class="version-spinner"></span>{:else}<span class="dot"></span>{/if}
          </span>
          <div><strong>{row.server.name}</strong><small>{row.message}</small></div>
        </li>
      {/each}
    </ol>
  {/if}
  {#if error}<p class="form-error" role="alert">{error}</p>{#if statusFailed && step === 'target'}<button class="button ghost small" type="button" onclick={() => void loadStatus()}>Retry status</button>{/if}{/if}
</Modal>

<style>
  .field { display: grid; gap: 7px; min-width: 0; font-size: 13px; font-weight: 550; }
  .field select, .field input { width: 100%; }
  .status { display: flex; align-items: center; gap: 8px; margin: 0; font-size: 12.5px; }
  .current { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 12px 14px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface-muted); animation: rise-in var(--dur) var(--ease-out); }
  .current > div { display: grid; gap: 2px; font-size: 12.5px; }
  .actions { display: flex; flex-wrap: wrap; gap: 8px; }
  .back { justify-self: start; }
  .note { display: flex; align-items: flex-start; gap: 8px; margin: 0; padding: 10px 12px; border: 1px solid color-mix(in srgb, var(--accent) 30%, transparent); border-radius: var(--radius-md); background: var(--accent-bg); color: var(--ink-secondary); font-size: 12.5px; }
  .note :global(svg) { flex-shrink: 0; margin-top: 1px; color: var(--accent); }
  .fields { display: grid; gap: 14px; }
  .link { font-size: 12.5px; }
  details summary { margin-bottom: 10px; color: var(--muted); cursor: pointer; font-size: 13px; }
  details .field { margin-bottom: 10px; }
  .progress-head { display: flex; align-items: center; justify-content: space-between; gap: 10px; font-size: 12.5px; }
  .job-progress.running span { background-image: linear-gradient(90deg, var(--accent), var(--blue), var(--accent)); background-size: 200% 100%; animation: shimmer 1.6s linear infinite; }
  .targets { display: grid; gap: 6px; max-height: 260px; margin: 0; padding: 0; overflow-y: auto; list-style: none; }
  .targets li { display: flex; align-items: flex-start; gap: 10px; padding: 9px 10px; border: 1px solid var(--line-light); border-radius: var(--radius-md); font-size: 13px; transition: border-color var(--dur), background-color var(--dur); }
  .targets li > div { display: grid; gap: 2px; min-width: 0; }
  .targets small { color: var(--muted); font-size: 12px; overflow-wrap: anywhere; }
  .mark { display: grid; flex-shrink: 0; place-items: center; width: 22px; height: 22px; border-radius: 50%; background: var(--surface-muted); color: var(--muted); }
  .mark .dot { width: 6px; height: 6px; border-radius: 50%; background: currentColor; opacity: 0.5; }
  .mark .version-spinner { width: 12px; height: 12px; }
  li.running { border-color: color-mix(in srgb, var(--accent) 35%, var(--line)); }
  li.done .mark { background: var(--success-bg); color: var(--success); animation: pop-in var(--dur) var(--ease-spring); }
  li.failed { border-color: color-mix(in srgb, var(--danger) 35%, var(--line)); background: var(--danger-bg); }
  li.failed .mark { background: var(--danger); color: #fff; }
  li.failed small { color: var(--danger); }
</style>
