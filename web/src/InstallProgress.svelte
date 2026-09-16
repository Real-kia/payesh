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
  $: isCancelled = install.currentStage === 'cancelled';

  $: lastErrorLog = install.logs.slice().reverse().find(l => l.level === 'error')?.text || install.logs.slice(-1)[0]?.text || '';
  $: isHostKeyOrNetworkError = /scan host key|host key|reset by peer|connection reset|timed out|timeout|unreachable/i.test(lastErrorLog);
  $: isAuthError = /permission denied|authentication|password|private key/i.test(lastErrorLog);
  $: isPortError = /connection refused|port/i.test(lastErrorLog);

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
    // Auto-scroll to bottom of log
    setTimeout(() => {
      if (logContainer) logContainer.scrollTop = logContainer.scrollHeight;
    }, 50);
  }

  function formatElapsed(sec: number): string {
    const m = Math.floor(sec / 60).toString().padStart(2, '0');
    const s = (sec % 60).toString().padStart(2, '0');
    return `${m}:${s}s`;
  }

  type StepConfig = {
    id: InstallStage;
    label: string;
    desc: string;
    isDone: (stage: InstallStage) => boolean;
    isActive: (stage: InstallStage) => boolean;
  };

  const steps: StepConfig[] = [
    {
      id: 'connecting',
      label: 'Connecting',
      desc: 'SSH handshake',
      isDone: (s) => ['connected', 'preflight', 'installing', 'enrolling', 'verifying', 'succeeded'].includes(s),
      isActive: (s) => s === 'connecting'
    },
    {
      id: 'connected',
      label: 'Host Verified',
      desc: 'Host key trusted',
      isDone: (s) => ['preflight', 'installing', 'enrolling', 'verifying', 'succeeded'].includes(s),
      isActive: (s) => s === 'connected'
    },
    {
      id: 'preflight',
      label: 'Preflight',
      desc: 'Detecting OS & arch',
      isDone: (s) => ['installing', 'enrolling', 'verifying', 'succeeded'].includes(s),
      isActive: (s) => s === 'preflight'
    },
    {
      id: 'installing',
      label: 'Installing Agent',
      desc: 'systemd service setup',
      isDone: (s) => ['enrolling', 'verifying', 'succeeded'].includes(s),
      isActive: (s) => s === 'installing'
    },
    {
      id: 'verifying',
      label: 'Live Telemetry',
      desc: 'Heartbeat stream',
      isDone: (s) => s === 'succeeded',
      isActive: (s) => ['enrolling', 'verifying'].includes(s)
    }
  ];
</script>

<article class={`panel install-progress-panel ${compact ? 'compact' : ''}`} aria-live="polite">
  <!-- HEADER -->
  <div class="install-header">
    <div class="install-title-group">
      <div class="install-target-chip">
        <Icon name="server" size={14} />
        <strong>{install.serverName}</strong>
        <span class="mono faint">{install.user}@{install.host}:{install.port}</span>
      </div>
      <h2 class="install-heading">
        {#if isRunning}
          Installing Payesh Agent…
        {:else if isSucceeded}
          Server Installed & Active
        {:else if isFailed}
          Installation Failed
        {:else}
          Installation Cancelled
        {/if}
      </h2>
    </div>

    <div class="install-status-group">
      {#if isRunning}
        <span class="status-pill installing">
          <i class="status-dot"></i>
          <span>{install.simulatedProgress}% · In progress</span>
        </span>
      {:else if isSucceeded}
        <span class="status-pill healthy">
          <Icon name="check" size={12} />
          <span>Server Online</span>
        </span>
      {:else if isFailed}
        <span class="status-pill failed">
          <Icon name="alert-triangle" size={12} />
          <span>Failed</span>
        </span>
      {:else}
        <span class="status-pill disabled">
          <span>Cancelled</span>
        </span>
      {/if}
    </div>
  </div>

  <!-- PROGRESS BAR -->
  <div class="install-bar-wrapper">
    <div class="install-bar">
      <span
        class={`install-bar-fill ${isSucceeded ? 'success' : isFailed ? 'error' : ''}`}
        style={`width: ${Math.max(4, Math.min(100, install.simulatedProgress))}%`}
      ></span>
    </div>
    <div class="install-bar-meta">
      <span class="faint">Elapsed: <strong>{formatElapsed(elapsedSeconds)}</strong></span>
      <span class="tabular mono">{install.simulatedProgress}%</span>
    </div>
  </div>

  <!-- STEPPER -->
  <div class="stepper-grid">
    {#each steps as step, i}
      {@const done = step.isDone(install.currentStage)}
      {@const active = step.isActive(install.currentStage)}
      {@const failed = isFailed && active}

      <div class={`step-node ${done ? 'done' : ''} ${active ? 'active' : ''} ${failed ? 'failed' : ''}`}>
        <div class="step-indicator">
          {#if done}
            <span class="step-icon done"><Icon name="check" size={13} /></span>
          {:else if failed}
            <span class="step-icon failed"><Icon name="alert-triangle" size={13} /></span>
          {:else if active}
            <span class="step-icon active"><Icon name="loader" size={13} class="spin" /></span>
          {:else}
            <span class="step-icon pending">{i + 1}</span>
          {/if}
          {#if i < steps.length - 1}
            <div class={`step-connector ${done ? 'connector-done' : ''}`}></div>
          {/if}
        </div>
        <div class="step-content">
          <strong class="step-label">{step.label}</strong>
          <small class="step-desc">{step.desc}</small>
        </div>
      </div>
    {/each}
  </div>

  <!-- TERMINAL LOGS -->
  <div class="terminal-window">
    <div class="terminal-bar">
      <div class="terminal-dots">
        <span class="dot red"></span>
        <span class="dot yellow"></span>
        <span class="dot green"></span>
      </div>
      <div class="terminal-title">
        <Icon name="terminal" size={13} />
        <span>Payesh Deployment Stream · {install.host}</span>
      </div>
      <span class="terminal-timer mono faint">{formatElapsed(elapsedSeconds)}</span>
    </div>
    <div class="terminal-body" bind:this={logContainer}>
      {#each install.logs as log}
        <div class={`log-line ${log.level}`}>
          <span class="log-time mono">{log.time}</span>
          <span class="log-glyph">
            {#if log.level === 'success'}✓
            {:else if log.level === 'error'}✕
            {:else if log.level === 'warn'}!
            {:else}→{/if}
          </span>
          <span class="log-text">{log.text}</span>
        </div>
      {/each}
      {#if isRunning}
        <div class="log-line running-cursor">
          <span class="cursor-dot"></span>
          <span class="faint">Waiting for remote host…</span>
        </div>
      {/if}
    </div>
  </div>

  <!-- ACTIONS -->
  <div class="install-actions">
    {#if isRunning}
      <div class="running-footer-left">
        <span class="active-badge">
          <span class="pulse-ring"></span>
          <span class="pulse-core"></span>
          <span>Deploying agent</span>
        </span>
        <span class="install-hint faint">
          Safe to navigate away — Payesh is managing this job in the background.
        </span>
      </div>
      <button class="cancel-install-btn" type="button" on:click={onCancel}>
        <Icon name="x-circle" size={14} />
        <span>Cancel installation</span>
      </button>
    {:else if isSucceeded}
      <div class="success-banner">
        <Icon name="check" size={16} />
        <span>Installation complete! Telemetry heartbeats are now streaming live.</span>
      </div>
      <div class="action-buttons">
        <button class="button ghost small" type="button" on:click={onAddAnother}>
          Add another server
        </button>
        <button class="button primary small" type="button" on:click={() => onViewServer(install.serverId)}>
          <span>View Server Dashboard</span>
          <Icon name="chevron-right" size={14} />
        </button>
      </div>
    {:else if isFailed}
      <div class="diagnostic-card">
        <div class="diagnostic-header">
          <div class="diagnostic-badge">
            <Icon name="alert-triangle" size={15} />
            <span>Deployment Halted</span>
          </div>
          <span class="diagnostic-tag">Automated Root Cause Diagnosis</span>
        </div>

        <div class="diagnostic-error-summary">
          <p class="error-primary-text">{lastErrorLog || 'Installation failed. Verify SSH credentials and network accessibility.'}</p>
        </div>

        <div class="diagnostic-grid">
          <div class="diagnostic-box analysis">
            <h4><Icon name="info" size={14} /> Diagnostic Analysis</h4>
            <p>
              {#if isHostKeyOrNetworkError}
                SSH key exchange handshake could not complete on port <strong>{install.port}</strong>. This commonly occurs when network middleboxes or cloud ISP firewalls detect and reset or drop SSH handshake packets, or when the remote SSH daemon is unresponsive.
              {:else if isAuthError}
                Remote SSH authentication failed for <strong>{install.user}@{install.host}</strong>. The credentials provided were rejected by the remote server.
              {:else if isPortError}
                Port <strong>{install.port}</strong> refused the connection. The target host is reachable, but sshd is not listening on this port.
              {:else}
                The agent installation process encountered an error while configuring system services on the remote machine.
              {/if}
            </p>
          </div>

          <div class="diagnostic-box solution">
            <h4><Icon name="check" size={14} /> Recommended Fix</h4>
            <ul class="solution-list">
              {#if isHostKeyOrNetworkError}
                <li><strong>Try an alternate port:</strong> If using port 22, try port <code>2222</code> or another high port to bypass middlebox SSH DPI inspection.</li>
                <li><strong>Firewall check:</strong> Ensure port <code>{install.port}</code> is allowed through <code>ufw</code> or <code>iptables</code> on the server.</li>
                <li><strong>sshd MaxStartups:</strong> If the server receives background brute-force scans, raise <code>MaxStartups 100:30:200</code> in <code>/etc/ssh/sshd_config</code>.</li>
              {:else if isAuthError}
                <li>Verify the SSH password or ensure the public key is in <code>~/.ssh/authorized_keys</code>.</li>
                <li>Ensure user has root privileges or sudo access.</li>
              {:else}
                <li>Inspect the deployment stream logs above for the specific exit code.</li>
                <li>Verify disk space and permissions on the remote host.</li>
              {/if}
            </ul>
          </div>
        </div>

        <div class="diagnostic-footer">
          <button class="button ghost small" type="button" on:click={() => onViewServer(install.serverId)}>
            <Icon name="server" size={13} />
            <span>Server details</span>
          </button>
          <button class="button ghost small" type="button" on:click={onAddAnother}>
            <Icon name="plus" size={13} />
            <span>Add another server</span>
          </button>
          <button class="button primary small auto-retry-btn" type="button" on:click={onRetry}>
            <Icon name="refresh" size={13} />
            <span>Adjust Settings & Retry</span>
          </button>
        </div>
      </div>
    {:else}
      <div class="action-buttons">
        <button class="button primary small" type="button" on:click={onRetry}>
          Restart installation
        </button>
      </div>
    {/if}
  </div>
</article>

<style>
  .install-progress-panel {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 1.5rem;
    box-shadow: 0 4px 20px -2px rgba(0, 0, 0, 0.12);
    display: flex;
    flex-direction: column;
    gap: 1.25rem;
    position: relative;
    overflow: hidden;
  }

  .install-progress-panel::before {
    content: '';
    position: absolute;
    top: 0;
    left: 0;
    right: 0;
    height: 2px;
    background: linear-gradient(90deg, #06b6d4, #3b82f6, #10b981);
    opacity: 0.8;
  }

  .install-progress-panel.compact {
    padding: 1.25rem;
    gap: 1rem;
  }

  /* HEADER */
  .install-header {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 1rem;
    flex-wrap: wrap;
  }

  .install-title-group {
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }

  .install-target-chip {
    display: inline-flex;
    align-items: center;
    gap: 0.5rem;
    background: var(--surface-muted);
    border: 1px solid var(--border);
    padding: 0.25rem 0.65rem;
    border-radius: 6px;
    font-size: 0.8125rem;
    color: var(--ink-secondary);
    width: fit-content;
  }

  .install-heading {
    margin: 0;
    font-size: 1.25rem;
    font-weight: 600;
    letter-spacing: -0.015em;
    color: var(--ink);
  }

  /* PROGRESS BAR */
  .install-bar-wrapper {
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
  }

  .install-bar {
    width: 100%;
    height: 6px;
    background: var(--surface-muted);
    border-radius: 9999px;
    overflow: hidden;
    position: relative;
  }

  .install-bar-fill {
    display: block;
    height: 100%;
    background: linear-gradient(90deg, #06b6d4, #3b82f6);
    border-radius: 9999px;
    transition: width 0.4s cubic-bezier(0.4, 0, 0.2, 1);
    box-shadow: 0 0 10px rgba(6, 182, 212, 0.4);
  }

  .install-bar-fill.success {
    background: #10b981;
    box-shadow: 0 0 10px rgba(16, 185, 129, 0.4);
  }

  .install-bar-fill.error {
    background: #ef4444;
    box-shadow: 0 0 10px rgba(239, 68, 68, 0.4);
  }

  .install-bar-meta {
    display: flex;
    justify-content: space-between;
    font-size: 0.75rem;
    color: var(--ink-faint);
  }

  /* STEPPER */
  .stepper-grid {
    display: grid;
    grid-template-columns: repeat(5, 1fr);
    gap: 0.5rem;
    margin: 0.5rem 0;
  }

  @media (max-width: 768px) {
    .stepper-grid {
      grid-template-columns: 1fr;
      gap: 0.75rem;
    }
  }

  .step-node {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    position: relative;
  }

  .step-indicator {
    display: flex;
    align-items: center;
    position: relative;
  }

  .step-icon {
    width: 28px;
    height: 28px;
    border-radius: 50%;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    font-size: 0.75rem;
    font-weight: 600;
    flex-shrink: 0;
    transition: all 0.2s ease;
    z-index: 1;
  }

  .step-icon.pending {
    background: var(--surface-muted);
    color: var(--ink-faint);
    border: 1px solid var(--border);
  }

  .step-icon.active {
    background: rgba(6, 182, 212, 0.15);
    color: #06b6d4;
    border: 1px solid #06b6d4;
    box-shadow: 0 0 12px rgba(6, 182, 212, 0.35);
  }

  .step-icon.done {
    background: rgba(16, 185, 129, 0.15);
    color: #10b981;
    border: 1px solid #10b981;
  }

  .step-icon.failed {
    background: rgba(239, 68, 68, 0.15);
    color: #ef4444;
    border: 1px solid #ef4444;
  }

  .step-connector {
    flex: 1;
    height: 2px;
    background: var(--border);
    margin-left: 0.5rem;
    transition: background 0.3s ease;
  }

  .step-connector.connector-done {
    background: #10b981;
  }

  @media (max-width: 768px) {
    .step-connector {
      display: none;
    }
  }

  .step-content {
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
  }

  .step-label {
    font-size: 0.8125rem;
    font-weight: 600;
    color: var(--ink);
  }

  .step-node.pending .step-label {
    color: var(--ink-faint);
  }

  .step-node.active .step-label {
    color: #06b6d4;
  }

  .step-desc {
    font-size: 0.6875rem;
    color: var(--ink-faint);
  }

  /* TERMINAL */
  .terminal-window {
    background: #080c14;
    border: 1px solid #1e293b;
    border-radius: 8px;
    overflow: hidden;
    display: flex;
    flex-direction: column;
    box-shadow: inset 0 2px 6px rgba(0, 0, 0, 0.4);
  }

  .terminal-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 0.45rem 0.75rem;
    background: #0d1320;
    border-bottom: 1px solid #1e293b;
    font-size: 0.75rem;
    color: #94a3b8;
  }

  .terminal-dots {
    display: flex;
    gap: 5px;
  }

  .terminal-dots .dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
  }

  .dot.red { background: #ef4444; }
  .dot.yellow { background: #eab308; }
  .dot.green { background: #10b981; }

  .terminal-title {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    font-weight: 500;
  }

  .terminal-body {
    padding: 0.75rem;
    max-height: 180px;
    overflow-y: auto;
    font-family: ui-monospace, 'SF Mono', Menlo, Consolas, monospace;
    font-size: 0.75rem;
    line-height: 1.5;
    color: #cbd5e1;
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }

  .log-line {
    display: flex;
    align-items: flex-start;
    gap: 0.5rem;
    word-break: break-word;
  }

  .log-time {
    color: #64748b;
    font-size: 0.6875rem;
    flex-shrink: 0;
  }

  .log-glyph {
    font-weight: 700;
    flex-shrink: 0;
  }

  .log-line.info .log-glyph { color: #06b6d4; }
  .log-line.success .log-glyph { color: #10b981; }
  .log-line.warn .log-glyph { color: #eab308; }
  .log-line.error .log-glyph { color: #ef4444; }
  .log-line.error .log-text { color: #fca5a5; }
  .log-line.success .log-text { color: #86efac; }

  .running-cursor {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding-top: 0.25rem;
  }

  .cursor-dot {
    width: 6px;
    height: 6px;
    background: #06b6d4;
    border-radius: 50%;
    animation: blink 1s ease infinite;
  }

  @keyframes blink {
    0%, 100% { opacity: 0.2; }
    50% { opacity: 1; }
  }

  :global(.spin) {
    animation: rotate 1.2s linear infinite;
  }

  @keyframes rotate {
    from { transform: rotate(0deg); }
    to { transform: rotate(360deg); }
  }

  /* ACTIONS */
  .install-actions {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    flex-wrap: wrap;
    padding-top: 0.75rem;
    border-top: 1px solid var(--border);
  }

  .running-footer-left {
    display: flex;
    align-items: center;
    gap: 0.85rem;
    flex: 1;
    min-width: 0;
    flex-wrap: wrap;
  }

  .active-badge {
    position: relative;
    display: inline-flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.25rem 0.65rem 0.25rem 1.6rem;
    background: rgba(6, 182, 212, 0.08);
    border: 1px solid rgba(6, 182, 212, 0.25);
    border-radius: 9999px;
    font-size: 0.75rem;
    font-weight: 600;
    color: #06b6d4;
    letter-spacing: 0.02em;
    flex-shrink: 0;
  }

  .pulse-core {
    position: absolute;
    left: 8px;
    width: 6px;
    height: 6px;
    background: #06b6d4;
    border-radius: 50%;
  }

  .pulse-ring {
    position: absolute;
    left: 7px;
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: #06b6d4;
    animation: pulse 2s cubic-bezier(0, 0, 0.2, 1) infinite;
    opacity: 0.6;
  }

  @keyframes pulse {
    0% { transform: scale(0.9); opacity: 0.8; }
    70% { transform: scale(2.4); opacity: 0; }
    100% { transform: scale(2.4); opacity: 0; }
  }

  .cancel-install-btn {
    display: inline-flex;
    align-items: center;
    gap: 0.45rem;
    padding: 0.4rem 0.85rem;
    font-size: 0.8125rem;
    font-weight: 500;
    color: #ef4444;
    background: rgba(239, 68, 68, 0.08);
    border: 1px solid rgba(239, 68, 68, 0.25);
    border-radius: 8px;
    cursor: pointer;
    transition: all 0.15s ease;
    white-space: nowrap;
  }

  .cancel-install-btn:hover {
    background: rgba(239, 68, 68, 0.16);
    border-color: rgba(239, 68, 68, 0.45);
    color: #f87171;
    transform: translateY(-1px);
    box-shadow: 0 2px 8px rgba(239, 68, 68, 0.2);
  }

  .cancel-install-btn:active {
    transform: translateY(0);
  }

  .install-hint {
    font-size: 0.8125rem;
    flex: 1;
  }

  .success-banner {
    display: inline-flex;
    align-items: center;
    gap: 0.5rem;
    color: #10b981;
    font-size: 0.875rem;
    font-weight: 500;
  }

  .error-banner {
    display: inline-flex;
    align-items: center;
    gap: 0.5rem;
    color: #ef4444;
    font-size: 0.875rem;
    font-weight: 500;
  }

  .action-buttons {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    margin-left: auto;
  }

  /* DIAGNOSTIC CARD */
  .diagnostic-card {
    width: 100%;
    display: flex;
    flex-direction: column;
    gap: 1rem;
    background: rgba(239, 68, 68, 0.03);
    border: 1px solid rgba(239, 68, 68, 0.22);
    border-radius: 10px;
    padding: 1.25rem;
  }

  .diagnostic-header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 0.75rem;
    flex-wrap: wrap;
  }

  .diagnostic-badge {
    display: inline-flex;
    align-items: center;
    gap: 0.45rem;
    color: #ef4444;
    font-weight: 600;
    font-size: 0.875rem;
  }

  .diagnostic-tag {
    display: inline-flex;
    align-items: center;
    padding: 0.2rem 0.6rem;
    background: var(--surface-muted);
    border: 1px solid var(--border);
    border-radius: 9999px;
    font-size: 0.6875rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--ink-secondary);
  }

  .diagnostic-error-summary {
    background: var(--surface);
    border: 1px solid rgba(239, 68, 68, 0.2);
    border-left: 3px solid #ef4444;
    border-radius: 6px;
    padding: 0.75rem 1rem;
  }

  .error-primary-text {
    margin: 0;
    font-size: 0.875rem;
    line-height: 1.5;
    color: #ef4444;
    font-weight: 500;
    word-break: break-word;
  }

  .diagnostic-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 1rem;
  }

  @media (max-width: 840px) {
    .diagnostic-grid {
      grid-template-columns: 1fr;
    }
  }

  .diagnostic-box {
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 1rem;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }

  .diagnostic-box h4 {
    margin: 0;
    font-size: 0.8125rem;
    font-weight: 600;
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    color: var(--ink);
  }

  .diagnostic-box p {
    margin: 0;
    font-size: 0.8125rem;
    line-height: 1.5;
    color: var(--ink-secondary);
  }

  .diagnostic-box code {
    background: var(--surface-muted);
    border: 1px solid var(--border);
    padding: 0.1rem 0.35rem;
    border-radius: 4px;
    font-size: 0.8em;
    color: #06b6d4;
  }

  .solution-list {
    margin: 0;
    padding-left: 1.25rem;
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
    font-size: 0.8125rem;
    line-height: 1.45;
    color: var(--ink-secondary);
  }

  .solution-list li strong {
    color: var(--ink);
  }

  .diagnostic-footer {
    display: flex;
    justify-content: flex-end;
    align-items: center;
    gap: 0.75rem;
    padding-top: 0.5rem;
    border-top: 1px solid rgba(239, 68, 68, 0.15);
  }

  .auto-retry-btn {
    background: linear-gradient(135deg, #06b6d4, #3b82f6) !important;
    border: none !important;
    color: #fff !important;
    font-weight: 600 !important;
    box-shadow: 0 2px 10px rgba(6, 182, 212, 0.35) !important;
  }

  .auto-retry-btn:hover {
    filter: brightness(1.1);
    transform: translateY(-1px);
    box-shadow: 0 4px 14px rgba(6, 182, 212, 0.45) !important;
  }
</style>
