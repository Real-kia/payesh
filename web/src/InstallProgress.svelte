<script context="module" lang="ts">
  export type InstallStage = 'connecting' | 'connected' | 'preflight' | 'installing' | 'enrolling' | 'verifying' | 'succeeded' | 'failed' | 'cancelled';
  export type InstallLog = { time: string; text: string; level: 'info' | 'success' | 'warn' | 'error' };

  export type InstallProgressData = {
    serverId: string;
    serverName: string;
    host: string;
    port: number;
    user: string;
    jobId: string;
    startedAt: number;
    currentStage: InstallStage;
    simulatedProgress: number;
    logs: InstallLog[];
  };
</script>

<script lang="ts">
  import { followLink } from './lib/navigation';
  import Icon from './Icon.svelte';
  import { onMount, onDestroy } from 'svelte';

  export let install: InstallProgressData;
  export let compact: boolean = false;
  export let onCancel: () => void = () => {};
  export let onViewServer: (serverId: string) => void = () => {};
  export let onRetry: () => void = () => {};
  export let onAddAnother: () => void = () => {};

  let logContainer: HTMLDivElement | null = null;
  let elapsedSeconds = 0;
  let timerInterval: number | null = null;

  $: isRunning = ['connecting', 'connected', 'preflight', 'installing', 'enrolling', 'verifying'].includes(install.currentStage);
  $: isSucceeded = install.currentStage === 'succeeded';
  $: isFailed = install.currentStage === 'failed';
  $: progress = Math.max(0, Math.min(100, install.simulatedProgress));

  $: lastErrorLog = install.logs.slice().reverse().find(l => l.level === 'error')?.text || install.logs.slice(-1)[0]?.text || '';

  onMount(() => {
    elapsedSeconds = Math.max(0, Math.floor((Date.now() - install.startedAt) / 1000));
    timerInterval = window.setInterval(() => {
      if (isRunning) {
        elapsedSeconds = Math.max(0, Math.floor((Date.now() - install.startedAt) / 1000));
      }
    }, 1000);
  });

  onDestroy(() => {
    if (timerInterval) window.clearInterval(timerInterval);
  });

  $: if (install.logs && logContainer) {
    // Keep the newest job update in view.
    setTimeout(() => {
      if (logContainer) logContainer.scrollTop = logContainer.scrollHeight;
    }, 50);
  }

  function formatElapsed(sec: number): string {
    const m = Math.floor(sec / 60).toString().padStart(2, '0');
    const s = (sec % 60).toString().padStart(2, '0');
    return `${m}:${s}`;
  }

  const glyph: Record<InstallLog['level'], string> = { info: '›', success: '✓', warn: '!', error: '✕' };
</script>

<article class="panel install" class:compact aria-live="polite">
  <header class="head">
    <div class="title">
      <h2>
        {#if isRunning}Installing the agent{:else if isSucceeded}Server installed{:else if isFailed}Installation failed{:else}Installation cancelled{/if}
      </h2>
      <p><strong>{install.serverName}</strong> <span class="mono">{install.user}@{install.host}:{install.port}</span></p>
    </div>
    {#if isRunning}
      <span class="status-pill installing"><i class="status-dot"></i><span>In progress</span></span>
    {:else if isSucceeded}
      <span class="status-pill healthy"><i class="status-dot"></i><span>Online</span></span>
    {:else if isFailed}
      <span class="status-pill failed"><i class="status-dot"></i><span>Failed</span></span>
    {:else}
      <span class="status-pill cancelled"><i class="status-dot"></i><span>Cancelled</span></span>
    {/if}
  </header>

  <div class="bar-wrap">
    <div class="bar" role="progressbar" aria-label="Installation progress" aria-valuemin="0" aria-valuemax="100" aria-valuenow={progress}>
      <span class:success={isSucceeded} class:error={isFailed} style:width={`${Math.max(2, progress)}%`}></span>
    </div>
    <div class="bar-meta tabular">
      <span>Elapsed {formatElapsed(elapsedSeconds)}</span>
      <span>{progress}%</span>
    </div>
  </div>

  <section class="log" aria-label="Installation job log">
    <div class="log-head"><span>Job log</span><span class="mono">{install.host}</span></div>
    <div class="log-body" bind:this={logContainer}>
      {#each install.logs as log}
        <div class={`line ${log.level}`}>
          <span class="time">{log.time}</span>
          <span class="glyph" aria-hidden="true">{glyph[log.level]}</span>
          <span class="text">{log.text}</span>
        </div>
      {/each}
      {#if isRunning}
        <div class="line waiting"><span class="time"></span><span class="glyph" aria-hidden="true">·</span><span class="text">Waiting for the next job update</span></div>
      {/if}
    </div>
  </section>

  {#if isFailed}
    <p class="failure" role="alert">{lastErrorLog || 'Installation failed.'}</p>
  {/if}

  <footer class="actions">
    {#if isRunning}
      <p class="hint">You can leave this page. The hub keeps running the job.</p>
      <button class="button danger small" type="button" on:click={onCancel}>Cancel installation</button>
    {:else if isSucceeded}
      <p class="hint">The agent is installed and sending heartbeats.</p>
      <div class="buttons">
        <button class="button ghost small" type="button" on:click={onAddAnother}>Add another server</button>
        <a class="button primary small" href={`/servers/${encodeURIComponent(install.serverId)}`} on:click={(event) => followLink(event, () => onViewServer(install.serverId))}>Open server<Icon name="chevron-right" size={14} /></a>
      </div>
    {:else if isFailed}
      <div class="buttons">
        <a class="button ghost small" href={`/servers/${encodeURIComponent(install.serverId)}`} on:click={(event) => followLink(event, () => onViewServer(install.serverId))}>Server details</a>
        <button class="button ghost small" type="button" on:click={onAddAnother}>Add another server</button>
        <button class="button primary small" type="button" on:click={onRetry}>Edit settings and retry</button>
      </div>
    {:else}
      <div class="buttons">
        <button class="button primary small" type="button" on:click={onRetry}>Restart installation</button>
      </div>
    {/if}
  </footer>
</article>

<style>
  .install { display: grid; gap: 18px; }
  .install.compact { gap: 14px; }

  .head { display: flex; flex-wrap: wrap; align-items: flex-start; justify-content: space-between; gap: 8px 16px; }
  .title { display: grid; gap: 4px; min-width: 0; }
  .title h2 { margin: 0; }
  .title p { margin: 0; overflow: hidden; color: var(--muted); font-size: 13px; text-overflow: ellipsis; white-space: nowrap; }
  .title strong { color: var(--ink-secondary); font-weight: 500; }
  .title .mono { font-size: 12px; }

  .bar-wrap { display: grid; gap: 6px; }
  .bar { height: 4px; overflow: hidden; border-radius: 2px; background: var(--surface-muted); }
  .bar span { display: block; height: 100%; background: var(--accent); transition: width var(--dur-slow) var(--ease-out); }
  .bar span.success { background: var(--success); }
  .bar span.error { background: var(--danger); }
  .bar-meta { display: flex; justify-content: space-between; color: var(--muted); font-size: 12px; }

  /* The log is always dark, like the Logs page viewer. */
  .log { overflow: hidden; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--term-bg); color: var(--term-ink); }
  .log-head { display: flex; justify-content: space-between; gap: 12px; padding: 7px 12px; border-bottom: 1px solid var(--term-line); color: var(--term-muted); font-size: 12px; }
  .log-head .mono { overflow: hidden; font-size: 12px; text-overflow: ellipsis; white-space: nowrap; }
  .log-body { display: grid; gap: 2px; max-height: 200px; padding: 10px 12px; overflow-y: auto; font-family: var(--font-mono); font-size: 12px; line-height: 1.55; }
  .compact .log-body { max-height: 160px; }
  .line { display: grid; grid-template-columns: 8ch 12px minmax(0, 1fr); gap: 8px; overflow-wrap: anywhere; }
  .time { color: var(--term-muted); }
  .glyph { color: var(--term-muted); text-align: center; }
  .line.success .glyph { color: #4fc98f; }
  .line.warn .glyph, .line.warn .text { color: #e8b15e; }
  .line.error .glyph, .line.error .text { color: #f2777e; }
  .line.waiting .text { color: var(--term-muted); }
  .line.waiting .glyph { animation: breathe 2s ease-in-out infinite; }

  .failure { margin: 0; padding: 10px 12px; border: 1px solid var(--line); border-radius: var(--radius-md); box-shadow: inset 2px 0 0 var(--danger); color: var(--ink); font-size: 13px; overflow-wrap: anywhere; }

  .actions { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 10px 16px; padding-top: 14px; border-top: 1px solid var(--line); }
  .hint { flex: 1 1 240px; margin: 0; color: var(--muted); font-size: 13px; }
  .buttons { display: flex; flex-wrap: wrap; gap: 8px; margin-left: auto; }

  @media (max-width: 480px) {
    .buttons { width: 100%; margin-left: 0; }
    .buttons .button { flex: 1 1 auto; }
  }
</style>
