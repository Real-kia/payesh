<script lang="ts">
  import Icon from '../Icon.svelte';
  import LoadingSkeleton from '../components/LoadingSkeleton.svelte';
  import type { Module } from '../api';

  let { modules, loadState, error, packageError, readOnly, busy, hasServers, onRefresh, onInstall }: {
    modules: Module[];
    loadState: 'ready' | 'loading' | 'empty' | 'error';
    error: string;
    packageError: string;
    readOnly: boolean;
    busy: boolean;
    hasServers: boolean;
    onRefresh: () => void;
    onInstall: (module: Module) => void;
  } = $props();

  const icons: Record<string, string> = { 'cpu-controls': 'cpu', 'bandwidth-controls': 'activity', 'port-traffic': 'radio', 'process-monitoring': 'terminal' };
</script>

<section class="page" aria-labelledby="packages-title">
  <div class="page-heading">
    <div>
      <h1 id="packages-title" tabindex="-1">Packages</h1>
      <p class="lede">Optional, signed extensions you can install on the master or any node.</p>
    </div>
    <button class="button ghost" type="button" onclick={onRefresh}><Icon name="refresh" size={14} /><span>Refresh catalog</span></button>
  </div>

  {#if error && loadState === 'ready'}<p class="partial-warning inline" role="status"><Icon name="alert-triangle" size={14} /><span>{error}</span></p>{/if}
  {#if loadState === 'loading'}
    <LoadingSkeleton cards={3} rows={0} label="Loading packages" />
  {:else if loadState === 'error'}
    <div class="state-panel error-state">
      <div class="state-icon"><Icon name="alert-triangle" size={24} /></div>
      <h2>Could not load packages</h2>
      <p>{error}</p>
      <button class="button primary" type="button" onclick={onRefresh}>Retry</button>
    </div>
  {:else if loadState === 'empty'}
    <div class="state-panel">
      <div class="state-icon"><Icon name="packages" size={24} /></div>
      <h2>No packages available</h2>
      <p>The verified package catalog is currently empty.</p>
    </div>
  {:else}
    <div class="module-grid stagger">
      {#each modules as module, index (module.id)}
        <article class="module-card" style:--i={index}>
          <div class="module-top">
            <span class="module-icon"><Icon name={icons[module.id] ?? 'packages'} size={18} /></span>
            <span class="release-tag mono">v{module.latest_version}</span>
          </div>
          <h2>{module.name}</h2>
          <p class="module-desc">{module.description || 'Optional Payesh extension capability.'}</p>
          <div class="module-meta"><span class="mono">{module.id}</span><span>Master or node</span></div>
          {#if !readOnly}
            <button class="button primary small full-width" type="button" disabled={module.install_supported === false || busy || !hasServers} onclick={() => onInstall(module)}>
              {module.install_supported === false ? 'Requires Payesh update' : 'Install or manage'}
            </button>
          {/if}
        </article>
      {/each}
    </div>
  {/if}
  {#if packageError}<p class="form-error" role="alert">{packageError}</p>{/if}
</section>

<style>
  .inline { margin: 0 0 16px; }
  .module-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(100%, 290px), 1fr)); gap: var(--gap); }
  .module-card {
    display: flex;
    flex-direction: column;
    gap: 10px;
    padding: var(--panel-pad);
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
    box-shadow: var(--shadow);
    transition: transform var(--dur) var(--ease-out), box-shadow var(--dur) var(--ease-out), border-color var(--dur) var(--ease-out);
  }
  .module-card:hover { transform: translateY(-2px); border-color: var(--line-strong); box-shadow: var(--shadow-md); }
  .module-top { display: flex; align-items: center; justify-content: space-between; margin-bottom: 4px; }
  .module-icon { display: grid; place-items: center; width: 38px; height: 38px; border-radius: var(--radius-md); background: var(--accent-bg); color: var(--accent); transition: transform var(--dur) var(--ease-spring); }
  .module-card:hover .module-icon { transform: rotate(-6deg) scale(1.06); }
  .release-tag { padding: 2px 8px; border: 1px solid var(--line); border-radius: var(--radius-pill); color: var(--muted); font-size: 11px; }
  .module-card h2 { margin: 0; font-size: 15.5px; }
  .module-desc { flex: 1; margin: 0; color: var(--muted); font-size: 13px; line-height: 1.55; }
  .module-meta { display: flex; justify-content: space-between; gap: 8px; padding-top: 10px; border-top: 1px solid var(--line-light); color: var(--muted); font-size: 11.5px; }
</style>
